//go:build windows

package media

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// TestPriorityChildProcess is the child process of TestRaisePriority.
func TestPriorityChildProcess(t *testing.T) {
	if os.Getenv("RECON_TEST_PRIORITY_CHILD") != "1" {
		t.Skip("child process of TestRaisePriority")
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// TestRaisePriority sets CPU and GPU priority on a real child process.
// Without D3DKMTSetProcessSchedulingPriorityClass in gdi32 (Wine) every set is
// refused: REALTIME, then HIGH, and the result is failed.
func TestRaisePriority(t *testing.T) {
	have := procD3DKMTSetProcessSchedulingPriorityClass.Find() == nil
	mode, regErr := readHwSchMode()
	t.Logf("D3DKMTSetProcessSchedulingPriorityClass found: %v; HwSchMode %d (%v); %v; SeIncreaseBasePriorityPrivilege: %v",
		have, mode, regErr, gpuHostInfo(), EnableGPUPriorityPrivilege())
	for _, tc := range []struct{ vendor, mode string }{
		{"amd", ""}, {"amd", "auto"}, {"nvidia", "auto"}, {"nvidia", "realtime"}, {"amd", "high"}, {"software", "auto"}, {"amd", "off"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPriorityChildProcess$")
		cmd.Env = append(os.Environ(), "RECON_TEST_PRIORITY_CHILD=1")
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		got, host, err := raisePriority(cmd, tc.vendor, tc.mode)
		var cpu uint32
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid)); err == nil {
			cpu, _ = windows.GetPriorityClass(h)
			windows.CloseHandle(h)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Logf("%s mode %q: gpu %s (adapter %q, hags %s, err %v), cpu priority class %#x", tc.vendor, tc.mode, got, host.adapter, host.hags, err, cpu)
		if cpu != windows.HIGH_PRIORITY_CLASS {
			t.Errorf("%s mode %q: cpu priority class %#x, want HIGH", tc.vendor, tc.mode, cpu)
		}
		class, on := gpuPriorityClass(tc.vendor, tc.mode, host)
		switch {
		case !on:
			if got != "off" || err != nil {
				t.Errorf("mode off: %s %v", got, err)
			}
		case !have:
			if got != "failed" || err == nil || !strings.Contains(err.Error(), "D3DKMTSetProcessSchedulingPriorityClass") {
				t.Errorf("%s mode %q without the export: %s %v, want failed", tc.vendor, tc.mode, got, err)
			}
		case class == gpuClassHigh && got != "high",
			class == gpuClassRealtime && got != "realtime" && got != "high":
			t.Errorf("%s mode %q: %s %v, want class %d", tc.vendor, tc.mode, got, err, class)
		}
	}

	// A process that cannot be opened (pid 0, the System Idle Process).
	if got, _, err := setGPUPriority(0, "amd", "auto"); got != "failed" || err == nil || !strings.Contains(err.Error(), "OpenProcess") {
		t.Errorf("pid 0: %s %v", got, err)
	}
}

// TestD3DKMTCall runs setGPUPriority against testdata/fake_d3dkmt.c (path in
// RECON_TEST_D3DKMT_DLL) in place of gdi32: the call gets a handle to the
// child that GetProcessId accepts, the 32-bit NTSTATUS is read even with junk
// in the high bits, and the classes asked for and the outcome match
// applyGPUPriority for no refusal, REALTIME refused, and both refused.
func TestD3DKMTCall(t *testing.T) {
	dll := fakeD3DKMT(t, &procD3DKMTSetProcessSchedulingPriorityClass)
	reset, call := dll.NewProc("FakeReset"), dll.NewProc("FakeCall")

	cmd := exec.Command(os.Args[0], "-test.run=^TestPriorityChildProcess$")
	cmd.Env = append(os.Environ(), "RECON_TEST_PRIORITY_CHILD=1")
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := uint32(cmd.Process.Pid)

	for refuse := range 3 {
		for _, vendor := range []string{"amd", "nvidia"} {
			for _, mode := range []string{"auto", "high", "realtime", "off"} {
				reset.Call(uintptr(refuse))
				got, host, err := setGPUPriority(int(pid), vendor, mode)
				var classes []uint32
				for i := 0; ; i++ {
					var p uint32
					c, _, _ := call.Call(uintptr(i), uintptr(unsafe.Pointer(&p)))
					if int32(c) < 0 {
						break
					}
					if p != pid {
						t.Errorf("call %d: pid %d, want %d", i, p, pid)
					}
					classes = append(classes, uint32(c))
				}
				var want []uint32
				wantGot, wantErr := applyGPUPriority(vendor, mode, host, func(c uint32) error {
					want = append(want, c)
					if c == gpuClassRealtime && refuse >= 1 || c == gpuClassHigh && refuse >= 2 {
						return errors.New("refused")
					}
					return nil
				})
				t.Logf("refuse %d %s %s: %s, classes %v, err %v", refuse, vendor, mode, got, classes, err)
				if got != wantGot || !slices.Equal(classes, want) || (err != nil) != (wantErr != nil) {
					t.Errorf("refuse %d %s %s: %s %v %v, want %s %v %v", refuse, vendor, mode, got, classes, err, wantGot, want, wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), "D3DKMTSetProcessSchedulingPriorityClass(") {
					t.Errorf("error without the call: %v", err)
				}
			}
		}
	}
}

func (h gpuHost) String() string {
	return fmt.Sprintf("adapter %s (%q), hags %s from %s, err %v", orUnknown(h.adapter), h.name, h.hags, h.hagsFrom, h.err)
}

// fakeD3DKMT loads testdata/fake_d3dkmt.c (path in RECON_TEST_D3DKMT_DLL)
// and points procs at its exports of the same name until the test ends.
func fakeD3DKMT(t *testing.T, procs ...**windows.LazyProc) *windows.LazyDLL {
	path := os.Getenv("RECON_TEST_D3DKMT_DLL")
	if path == "" {
		t.Skip("RECON_TEST_D3DKMT_DLL not set (a build of testdata/fake_d3dkmt.c)")
	}
	dll := windows.NewLazyDLL(path)
	if err := dll.Load(); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		saved := *p
		*p = dll.NewProc(saved.Name)
		t.Cleanup(func() { *p = saved })
	}
	return dll
}

// TestKernelHAGS runs kernelHAGS against the stand-in DLL: the adapter is
// opened by the LUID given, asked for KMTQAITYPE_WDDM_2_7_CAPS (4 bytes) and
// closed; HwSchEnabled (bit 1) alone decides; a refused open or query is an
// error (closed after a refused query).
func TestKernelHAGS(t *testing.T) {
	dll := fakeD3DKMT(t, &procD3DKMTOpenAdapterFromLuid, &procD3DKMTQueryAdapterInfo, &procD3DKMTCloseAdapter)
	fake, flog := dll.NewProc("FakeAdapter"), dll.NewProc("FakeAdapterLog")
	const luid = 0x00000001_8000_0a2c // HighPart 1, LowPart 0x80000a2c
	for _, c := range []struct {
		caps, fail uint32
		want       bool
		log        [8]uint32 // opens, luid low, high, queries, adapter, type, size, closes
	}{
		{0b011, 0, true, [8]uint32{1, 0x80000a2c, 1, 1, 0x40000240, 70, 4, 1}},  // supported, enabled
		{0b111, 0, true, [8]uint32{1, 0x80000a2c, 1, 1, 0x40000240, 70, 4, 1}},  // and on by default
		{0b101, 0, false, [8]uint32{1, 0x80000a2c, 1, 1, 0x40000240, 70, 4, 1}}, // on by default but turned off
		{0b001, 0, false, [8]uint32{1, 0x80000a2c, 1, 1, 0x40000240, 70, 4, 1}},
		{0b011, 1, false, [8]uint32{1, 0x80000a2c, 1, 0, 0, 0, 0, 0}},           // open refused
		{0b011, 2, false, [8]uint32{1, 0x80000a2c, 1, 1, 0x40000240, 70, 4, 1}}, // query refused
	} {
		fake.Call(uintptr(c.caps), uintptr(c.fail))
		on, err := kernelHAGS(luid)
		var log [8]uint32
		flog.Call(uintptr(unsafe.Pointer(&log)))
		t.Logf("caps %03b fail %d: %v %v, calls %#x", c.caps, c.fail, on, err, log)
		if on != c.want || (err != nil) != (c.fail != 0) || log != c.log {
			t.Errorf("caps %03b fail %d: %v %v, calls %#x, want %v %#x", c.caps, c.fail, on, err, log, c.want, c.log)
		}
		if c.fail != 0 && !strings.Contains(err.Error(), "D3DKMT") {
			t.Errorf("error without the call: %v", err)
		}
	}
}

// TestDetectGPUHost: the kernel's answer wins over HwSchMode; without it
// (refused, or no adapter) HwSchMode decides and the reason is kept. With a
// DXGI adapter (Wine under X), the kernel is asked for adapter 0's LUID.
func TestDetectGPUHost(t *testing.T) {
	dll := fakeD3DKMT(t, &procD3DKMTOpenAdapterFromLuid, &procD3DKMTQueryAdapterInfo, &procD3DKMTCloseAdapter)
	fake, flog := dll.NewProc("FakeAdapter"), dll.NewProc("FakeAdapterLog")
	if a, err := platform.PrimaryAdapter(); err != nil {
		t.Logf("no DXGI adapter: %v", err)
	} else {
		fake.Call(0b011, 0)
		h := detectGPUHost(platform.PrimaryAdapter)
		var log [8]uint32
		flog.Call(uintptr(unsafe.Pointer(&log)))
		t.Logf("adapter 0 %+v: %v, LUID asked for %#x:%#x", a, h, log[2], log[1])
		if h.adapter != a.Vendor || h.name != a.Name || h.hags != hagsOn || h.hagsFrom != "kernel" || uint64(log[2])<<32|uint64(log[1]) != a.LUID {
			t.Errorf("adapter 0 %+v: %v, LUID asked for %#x:%#x", a, h, log[2], log[1])
		}
	}
	nvidia := func() (platform.Adapter, error) {
		return platform.Adapter{Vendor: "nvidia", VendorID: 0x10DE, LUID: 0x1_0000_0042, Name: "NVIDIA GeForce RTX 4080"}, nil
	}
	noDXGI := func() (platform.Adapter, error) { return platform.Adapter{}, errors.New("no DXGI") }
	registry := hwSchModeHAGS(readHwSchMode())
	for _, c := range []struct {
		name               string
		adapter0           func() (platform.Adapter, error)
		caps, fail         uint32
		adapter, hags, src string
		err                bool
	}{
		{"kernel on", nvidia, 0b011, 0, "nvidia", hagsOn, "kernel", false},
		{"kernel off", nvidia, 0b101, 0, "nvidia", hagsOff, "kernel", false},
		{"query refused", nvidia, 0b011, 2, "nvidia", registry, "registry", true},
		{"no adapter", noDXGI, 0b011, 0, "", registry, "registry", true},
	} {
		fake.Call(uintptr(c.caps), uintptr(c.fail))
		h := detectGPUHost(c.adapter0)
		t.Logf("%s: %v", c.name, h)
		if h.adapter != c.adapter || h.hags != c.hags || h.hagsFrom != c.src || (h.err != nil) != c.err {
			t.Errorf("%s: %v, want adapter %q hags %s from %s err %v", c.name, h, c.adapter, c.hags, c.src, c.err)
		}
	}
}

// TestHAGSRegistry reads HwSchMode. RECON_TEST_HWSCHMODE (a number, or
// "none" for a missing value) makes it check the value set up for the test:
// 2 on, 1 off, 0 or missing unknown (the OS/driver default).
func TestHAGSRegistry(t *testing.T) {
	mode, err := readHwSchMode()
	hags := hwSchModeHAGS(mode, err)
	t.Logf("HwSchMode %d, err %v: hags %s; detected %v", mode, err, hags, gpuHostInfo())
	switch want := os.Getenv("RECON_TEST_HWSCHMODE"); want {
	case "":
	case "none":
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || hags != hagsUnknown {
			t.Fatalf("missing HwSchMode: %d %v, hags %s", mode, err, hags)
		}
	default:
		n, _ := strconv.ParseUint(want, 10, 32)
		wantHAGS := map[uint64]string{0: hagsUnknown, 1: hagsOff, 2: hagsOn}[n]
		if err != nil || mode != n || hags != wantHAGS {
			t.Fatalf("HwSchMode %d %v, hags %s, want %d %s", mode, err, hags, n, wantHAGS)
		}
	}
}

func TestEnableGPUPriorityPrivilege(t *testing.T) {
	err := EnableGPUPriorityPrivilege()
	t.Logf("EnableGPUPriorityPrivilege: %v", err)
	if err != nil && !errors.Is(err, windows.ERROR_NOT_ALL_ASSIGNED) {
		t.Fatal(err)
	}
}

// TestVideoGPUPriorityLog checks that an encoder generation logs its GPU
// priority with the encoder's vendor.
func TestVideoGPUPriorityLog(t *testing.T) {
	caps := probeOrSkip(t)
	enc, ok := caps.Best("h264")
	if !ok {
		t.Skip("no h264 encoder")
	}
	var buf syncBuffer
	start := time.Now()
	v := NewVideo(caps, slog.New(slog.NewTextHandler(&buf, nil)), func() uint64 { return uint64(time.Since(start).Microseconds()) })
	defer v.Stop()
	if err := v.Start(Params{Source: Source{Backend: "test", NativeW: 320, NativeH: 180}, Encoder: enc, FPS: 30, BitrateKbps: 1000}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-v.Events():
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no encoder output")
	}
	logs := buf.String()
	t.Log(logs)
	if !strings.Contains(logs, `msg="gpu priority: `) || !strings.Contains(logs, "vendor="+enc.Vendor+" adapter=") || !strings.Contains(logs, "mode=auto") {
		t.Fatal("no gpu priority line with the encoder vendor")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
