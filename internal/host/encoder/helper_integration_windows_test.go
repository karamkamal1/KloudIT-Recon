//go:build windows

package encoder

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Integration tests against the real recon-encoder.exe with its mock backend
// (no GPU needed). They run when RECON_HELPER_EXE points at the helper:
//
//	RECON_HELPER_EXE=C:\path\recon-encoder.exe go test -run HelperIntegration ./internal/host/encoder
//
// RECON_HELPER_DUMP=file.h264 also writes the bitstream received before the
// ring-full check, which must decode cleanly (ffmpeg -i file.h264 -f null -).

func helperExe(t *testing.T) string {
	t.Helper()
	exe := os.Getenv("RECON_HELPER_EXE")
	if exe == "" {
		t.Skip("set RECON_HELPER_EXE to recon-encoder.exe to run the helper integration tests")
	}
	return exe
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

func launchMock(t *testing.T, extra ...string) *Helper {
	t.Helper()
	h, err := Launch(Options{
		Exe:      helperExe(t),
		Backend:  "mock",
		LogLevel: "debug",
		Args:     extra,
		Log:      slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// nalTypes lists the H.264 NAL unit types in an Annex-B access unit.
func nalTypes(au []byte) []byte {
	var types []byte
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			types = append(types, au[i+3]&0x1f)
			i += 2
		}
	}
	return types
}

func waitHelperError(t *testing.T, h *Helper, code string) *HelperError {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-h.Errors():
			var he *HelperError
			if errors.As(err, &he) && he.Code == code {
				return he
			}
			t.Logf("ignoring error %v", err)
		case <-deadline:
			t.Fatalf("no %q error", code)
		}
	}
}

func TestHelperIntegrationMock(t *testing.T) {
	h := launchMock(t)
	var dump *os.File
	if p := os.Getenv("RECON_HELPER_DUMP"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		dump = f
	}

	// Caps: the mock, plus the real backends probed and reported unavailable.
	c := h.Caps()
	h264 := c.Codecs["h264"]
	if c.V != ProtocolVersion || c.Backend != "mock" || c.Vendor != "mock" || !c.Usable() || len(c.Codecs) != 1 ||
		!h264.ForceIDR || h264.Recovery != "none" || h264.LiveBitrate != "seamless" || h264.MaxW != 320 ||
		len(c.Capture) < 1 || c.Capture[0] != "synthetic" || c.QPCFrequency <= 0 || c.CursorInVideo {
		t.Fatalf("caps %+v", c)
	}
	for _, n := range []string{"amf", "nvenc"} {
		if c.Unavailable[n] == "" {
			t.Errorf("caps do not say why %s is unavailable: %v", n, c.Unavailable)
		}
	}
	// Every real capture method is either usable (listed after "synthetic") or
	// unavailable with a reason, never both.
	for _, n := range []string{"dda", "amd-direct", "wgc"} {
		if slices.Contains(c.Capture, n) == (c.Unavailable[n] != "") {
			t.Errorf("capture %s: listed %v, unavailable %q", n, slices.Contains(c.Capture, n), c.Unavailable[n])
		}
	}
	t.Logf("capture: %v, unavailable: %v, outputs: %+v", c.Capture, c.Unavailable, c.Outputs)

	// Before start, and bad requests: non-fatal errors, the helper keeps running.
	if err := h.ForceIDR(); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "not_started"); e.Fatal || e.Re != "forceIdr" {
		t.Fatalf("forceIdr before start: %v", e)
	}
	if err := h.send(simpleMsg{T: "bogus"}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "bad_message"); e.Fatal {
		t.Fatalf("unknown message: %v", e)
	}
	_, err := h.Start(StartParams{Codec: "hevc", FPS: 60, Kbps: 4000})
	var he *HelperError
	if !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
		t.Fatalf("hevc start on the mock: %v", err)
	}
	_, err = h.Start(StartParams{Codec: "h264", FPS: 9999, Kbps: 4000})
	if !errors.As(err, &he) || he.Code != "bad_message" {
		t.Fatalf("start with fps 9999: %v", err)
	}
	// A start that fails after the encoder was initialized (a barcode needs the
	// GPU conversion): the helper must release the encoder (Backend::release,
	// which the mock checks on its next init), so the start below works.
	_, err = h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000, Barcode: &Barcode{Cell: proto.BarcodeCell}})
	if !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
		t.Fatalf("barcode start on the synthetic source: %v", err)
	}

	st, err := h.Start(StartParams{Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 4000})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Backend != "mock" || st.Capture != "synthetic" || st.Codec != "h264" || st.Width != 320 || st.Height != 180 ||
		st.CodedWidth != 320 || st.CodedHeight != 180 || st.CropRight != 0 || st.CropBottom != 0 || st.LiveBitrate != "seamless" {
		t.Fatalf("started %+v", st)
	}
	// ACKs are accepted silently (the mock has no LTR to use them for).
	if err := h.Ack(1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); !errors.As(err, &he) || he.Code != "already_started" {
		t.Fatalf("second start: %v", err)
	}

	var last *Frame
	sinceKey := 0
	// check validates every frame read and keeps the position in the clip.
	check := func(f *Frame) {
		t.Helper()
		if last != nil && f.FrameID != last.FrameID+1+uint64(f.DroppedBefore) {
			t.Fatalf("frame %d after %d (DroppedBefore %d)", f.FrameID, last.FrameID, f.DroppedBefore)
		}
		if !bytes.HasPrefix(f.Data, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(f.Data, []byte{0, 0, 1}) {
			t.Fatalf("frame %d is not Annex-B: % x", f.FrameID, f.Data[:min(8, len(f.Data))])
		}
		types := nalTypes(f.Data)
		hasIDR := bytes.IndexByte(types, 5) >= 0
		if f.Key != hasIDR || (f.Key && (bytes.IndexByte(types, 7) < 0 || bytes.IndexByte(types, 8) < 0)) ||
			(!f.Key && bytes.IndexByte(types, 1) < 0) {
			t.Fatalf("frame %d key=%v has NAL types %v", f.FrameID, f.Key, types)
		}
		if f.CaptureQPC <= 0 || f.SubmitQPC < f.CaptureQPC || f.OutputQPC < f.SubmitQPC || f.Width != 320 || f.Height != 180 {
			t.Fatalf("frame %d: %+v", f.FrameID, *f)
		}
		if dump != nil {
			dump.Write(f.Data)
		}
		if f.Key {
			sinceKey = 0
		} else {
			sinceKey++
		}
		last = f
	}
	next := func() *Frame {
		t.Helper()
		select {
		case f, ok := <-h.Frames():
			if !ok {
				t.Fatalf("frames closed: %v", h.Err())
			}
			check(f)
			return f
		case <-time.After(5 * time.Second):
			t.Fatalf("no frame: %v", h.Err())
		}
		return nil
	}

	// The stream starts with an IDR (frame 1), then P frames.
	if f := next(); !f.Key || f.FrameID != 1 {
		t.Fatalf("first frame %+v", *f)
	}
	for i := 0; i < 10; i++ {
		if f := next(); f.Key {
			t.Fatalf("unexpected key frame %d", f.FrameID)
		}
	}

	// forceIdr / recover (no LTR on the mock: IDR) give a key frame within a
	// few frames. The canned clip has its own IDR every 60 frames, so only
	// ask when that one is far away.
	for _, ask := range []func() error{
		h.ForceIDR,
		func() error { return h.Recover(last.FrameID, nil) },
	} {
		for sinceKey < 5 || sinceKey > 40 {
			next()
		}
		asked := last.FrameID
		if err := ask(); err != nil {
			t.Fatal(err)
		}
		for !next().Key {
			if last.FrameID-asked > 6 {
				t.Fatalf("no key frame within 6 frames of the request at %d", asked)
			}
		}
		t.Logf("key frame %d, %d frames after the request", last.FrameID, last.FrameID-asked)
	}

	// setRate is recorded and reported in the stats.
	if err := h.SetRate(2500, 1.5, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.SetROI([]ROIRect{{X: 0, Y: 0, W: 64, H: 64, Weight: 5}}); err != nil {
		t.Fatal(err)
	}
	statsDeadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case s := <-h.Stats():
			got = s.Kbps == 2500 && s.VBVFrames == 1.5 && s.FPS == 60
		case f := <-h.Frames(): // keep the ring moving
			check(f)
		case err := <-h.Errors():
			if he, ok := err.(*HelperError); !ok || he.Fatal || he.Re == "setRate" || he.Re == "setRoi" {
				t.Fatalf("error after setRate/setRoi: %v", err)
			}
		case <-statsDeadline:
			t.Fatal("stats never reported the new rate")
		}
	}

	// The dump stops here: what follows loses frames on purpose.
	if dump != nil {
		dump.Close()
		dump = nil
	}

	// Ring full: stop reading frames for a while (stats keep flowing). The
	// helper must drop the newest frames and say so, never stall.
	pause := time.After(1500 * time.Millisecond)
	droppedStats := 0
	for paused := true; paused; {
		select {
		case s := <-h.Stats():
			if s.Dropped {
				if s.Reason != "ringFull" || s.RingDropped == 0 {
					t.Fatalf("drop stats %+v", s)
				}
				droppedStats++
			}
		case <-pause:
			paused = false
		}
	}
	if droppedStats == 0 {
		t.Fatal("no dropped=true stats while frames were not read")
	}
	t.Logf("%d frames reported dropped while paused", droppedStats)
	// Reading again: the frames that were buffered, then the first frame
	// after the drops carries DroppedBefore (next() checks the id arithmetic).
	gapSeen := false
	for i := 0; i < 40 && !gapSeen; i++ {
		gapSeen = next().DroppedBefore > 0
	}
	if !gapSeen {
		t.Fatal("no frame reported the dropped frames")
	}

	// Clean shutdown.
	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close hung")
	}
	if err := h.Err(); err != nil {
		t.Fatalf("clean shutdown reported %v", err)
	}
}

func TestHelperIntegrationErrors(t *testing.T) {
	// A non-fatal error: reported, frames keep coming.
	h := launchMock(t, "--mock-error-at=5")
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "mock_error"); e.Fatal {
		t.Fatalf("mock_error is fatal: %v", e)
	}
	for f := range h.Frames() {
		if f.FrameID >= 10 {
			break
		}
	}
	select {
	case <-h.Done():
		t.Fatalf("helper exited after a non-fatal error: %v", h.Err())
	default:
	}
	h.Close()

	// A fatal error: reported, the helper exits, Frames closes; a restarted
	// helper starts again with an IDR (what recon-host does).
	h = launchMock(t, "--mock-fatal-at=20")
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "mock_fatal"); !e.Fatal {
		t.Fatalf("mock_fatal not fatal: %v", e)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after a fatal error")
	}
	n := 0
	for range h.Frames() {
		n++
	}
	var he *HelperError
	if err := h.Err(); !errors.As(err, &he) || he.Code != "mock_fatal" {
		t.Fatalf("Err() = %v", err)
	}
	t.Logf("%d frames before the fatal error", n)
	if err := h.ForceIDR(); err == nil {
		t.Fatal("command to an exited helper succeeded")
	}
	h.Close()

	start := time.Now()
	h = launchMock(t)
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-h.Frames():
		if f == nil || !f.Key || f.FrameID != 1 {
			t.Fatalf("first frame after restart: %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame after restart")
	}
	t.Logf("restart to first frame: %v", time.Since(start))

	// Broken framing (a length above the limit) is fatal.
	h2 := launchMock(t)
	h2.ctrlQ <- []byte{0xff, 0xff, 0xff, 0x7f}
	if e := waitHelperError(t, h2, "protocol"); !e.Fatal {
		t.Fatalf("protocol error not fatal: %v", e)
	}
	<-h2.Done()
	if !errors.As(h2.Err(), &he) || he.Code != "protocol" {
		t.Fatalf("Err() after protocol error = %v", h2.Err())
	}

	// Killed from outside: Done, an ExitError, Frames closes.
	h.c.kill()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after kill")
	}
	var ee *ExitError
	if !errors.As(h.Err(), &ee) {
		t.Fatalf("Err() after kill = %v", h.Err())
	}
	for range h.Frames() {
	}
}

func TestHelperIntegrationStuckExit(t *testing.T) {
	// A capture/encoder call stuck in the driver (the mock never returns from
	// submitting frame 5) must not keep the helper alive once it should exit:
	// its watchdog ends it with code 4 about 500 ms later.
	const exitStuck = 4
	stuck := func() *Helper {
		t.Helper()
		h := launchMock(t, "--mock-hang-at=5")
		if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
			t.Fatal(err)
		}
		for f := range h.Frames() {
			if f.FrameID == 4 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond) // frame 5 is stuck in submit by now
		return h
	}
	var ee *ExitError

	// recon-host went away: stdin EOF, and nobody is left to kill the helper.
	h := stuck()
	start := time.Now()
	h.c.ctrlW.Close() // writeLoop is idle
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper with a stuck thread still running after stdin EOF")
	}
	t.Logf("exited %v after stdin EOF", time.Since(start))
	if !errors.As(h.Err(), &ee) || ee.Code != exitStuck {
		t.Fatalf("Err() after stdin EOF = %v", h.Err())
	}

	// Close (shutdown): the helper ends itself well before Close would kill it.
	h = stuck()
	start = time.Now()
	h.Close()
	if d := time.Since(start); d >= closeGrace {
		t.Fatalf("Close took %v: the helper did not end itself", d)
	}
	t.Logf("Close took %v", time.Since(start))
	if !errors.As(h.Err(), &ee) || ee.Code != exitStuck {
		t.Fatalf("Err() after Close = %v", h.Err())
	}
}

func TestHelperIntegrationAuto(t *testing.T) {
	// The real backends: in CI and under Wine there is no GPU runtime, so the
	// caps must say so and start must fail cleanly (recon-host then uses FFmpeg).
	h, err := Launch(Options{Exe: helperExe(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.Caps()
	t.Logf("auto caps: backend=%s vendor=%s adapter=%q unavailable=%v", c.Backend, c.Vendor, c.AdapterName, c.Unavailable)
	if c.Usable() {
		t.Skip("a real encoder backend is available here")
	}
	_, err = h.Start(StartParams{Codec: "hevc", FPS: 60, Kbps: 20000})
	var he *HelperError
	if !errors.As(err, &he) || he.Code != "unavailable" || he.Fatal {
		t.Fatalf("start without a backend: %v", err)
	}

	// A small ring works too.
	h2, err := Launch(Options{Exe: helperExe(t), Backend: "mock", Slots: 2, SlotSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	h2.Close()
}

func TestHelperIntegrationCommandLine(t *testing.T) {
	exe := helperExe(t)
	// --print-caps prints the same caps message, unframed.
	out, err := exec.Command(exe, "--print-caps", "--backend=mock").Output()
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMessage(bytes.TrimSpace(out))
	if c, ok := m.(*Caps); err != nil || !ok || c.Backend != "mock" || c.V != ProtocolVersion {
		t.Fatalf("--print-caps: %v %v (%s)", m, err, out)
	}
	// Handles that were not inherited: the helper reports a fatal ring error
	// on stdout and exits with code 2.
	cmd := exec.Command(exe, "--ring-handle=0x1234", "--ring-size=200704", "--event-handle=0x1238", "--backend=mock")
	out, err = cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("bad handles: exit %v", err)
	}
	b, err := proto.ReadMsg(bytes.NewReader(out), MaxControlMsg)
	if err != nil {
		t.Fatalf("bad handles: no framed message in %q", out)
	}
	m, err = decodeMessage(b)
	if e, ok := m.(*HelperError); err != nil || !ok || e.Code != "ring" || !e.Fatal {
		t.Fatalf("bad handles: %v %v", m, err)
	}
	// Bad arguments: usage on stderr, exit code 2.
	if err := exec.Command(exe, "--frobnicate").Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("unknown argument: %v", err)
	}
	if err := exec.Command(exe, "--print-caps", "--backend=amf", "--mock-fatal-at=3").Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("mock option without the mock backend: %v", err)
	}
}

// underWine reports whether the test runs under Wine (ntdll exports wine_get_version).
func underWine() bool {
	return syscall.NewLazyDLL("ntdll.dll").NewProc("wine_get_version").Find() == nil
}

func TestHelperIntegrationSelfTests(t *testing.T) {
	exe := helperExe(t)
	// Frame pacing policy: pure logic, runs everywhere.
	out, err := exec.Command(exe, "--self-test-pacer").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("--self-test-pacer: %v", err)
	}
	// Encoder logic (LTR recovery policy, parameter sets, ROI maps): runs everywhere.
	out, err = exec.Command(exe, "--self-test-encoder").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("--self-test-encoder: %v", err)
	}
	// GPU colour conversion on WARP (SDR NV12 and HDR10 P010). Wine needs an X
	// display for D3D11 and has no NV12 / P010 render targets (the self-test
	// then checks the same shaders on separate planes); real Windows must pass
	// in NV12 mode (P010 render targets are logged, not required: WARP's are a
	// VERIFY item, docs/VENDOR_NOTES.md 3.9).
	out, err = exec.Command(exe, "--self-test-convert").CombinedOutput()
	t.Logf("%s", out)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 77 && underWine() {
		t.Skip("no D3D11 device under Wine (needs an X display)")
	}
	if err != nil {
		t.Fatalf("--self-test-convert: %v", err)
	}
	if !underWine() && !strings.Contains(string(out), "self-test-convert: ok (mode nv12;") {
		t.Fatal("conversion not tested on NV12 render targets")
	}
}

// The NVENC backend (native/recon-encoder/src/nvenc) driven by
// --self-test-nvenc: against the test double of the NVIDIA runtime
// (native/recon-encoder/test/fake_nvenc.cpp, recon-fake-nvenc.dll in the
// helper's build directory; RECON_FAKE_NVENC points at it, make helper-test and
// CI set it), and against the real driver where there is one (an NVIDIA host;
// elsewhere the helper answers 77 and the subtest skips). Both need a D3D11
// device: under Wine an X display.
func TestHelperIntegrationNvenc(t *testing.T) {
	exe := helperExe(t)
	run := func(t *testing.T, args ...string) {
		out, err := exec.Command(exe, args...).CombinedOutput()
		t.Logf("%s", out)
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 77 {
			t.Skip("cannot run here (no D3D11 device, or no NVIDIA runtime / adapter)")
		}
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(string(out), "self-test-nvenc: ok") {
			t.Fatal("no \"self-test-nvenc: ok\" line")
		}
	}
	t.Run("TestDouble", func(t *testing.T) {
		dll := os.Getenv("RECON_FAKE_NVENC")
		if dll == "" {
			t.Skip("set RECON_FAKE_NVENC to recon-fake-nvenc.dll (built next to recon-encoder.exe)")
		}
		run(t, "--self-test-nvenc="+dll)
	})
	t.Run("Driver", func(t *testing.T) { run(t, "--self-test-nvenc") })
}

// The --encode-test mode (the hardware check of an encoder backend without
// recon-host) through the mock backend: scripted forced IDR, loss and rate
// changes; the file it writes is the Annex-B stream that came out of the ring.
// On an AMD host RECON_HELPER_ENCODE_TEST="--backend=amf --codec=hevc ..."
// runs it against the real encoder instead (docs/VENDOR_NOTES.md 3.3).
func TestHelperIntegrationEncodeTest(t *testing.T) {
	exe := helperExe(t)
	out := t.TempDir() + `\encode-test.h264`
	args := []string{"--encode-test=" + out, "--backend=mock", "--codec=h264", "--capture=synthetic", "--frames=150",
		"--at=20:idr", "--at=40:loss", "--at=70:rate=2000", "--at=100:fps=30", "--at=120:roi=0,0,64,64,10"}
	if extra := os.Getenv("RECON_HELPER_ENCODE_TEST"); extra != "" {
		args = append([]string{"--encode-test=" + out}, strings.Fields(extra)...)
	}
	b, err := exec.Command(exe, args...).CombinedOutput()
	t.Logf("%s", b)
	if err != nil {
		t.Fatalf("--encode-test: %v", err)
	}
	for _, want := range []string{"encode-test: ok", "idr requested at 20: key frame", "loss at 40: recovered at"} {
		if os.Getenv("RECON_HELPER_ENCODE_TEST") == "" && !strings.Contains(string(b), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		t.Fatalf("no bitstream written: %v", err)
	}
	if os.Getenv("RECON_HELPER_ENCODE_TEST") != "" {
		return
	}
	// The file starts with the first IDR (SPS, PPS, IDR) and holds the forced
	// and the loss-recovery IDRs too (the mock recovers by IDR).
	types := nalTypes(data)
	idrs := bytes.Count(types, []byte{7}) // every IDR access unit carries one SPS
	if len(types) < 3 || types[0] != 9 || types[1] != 7 || idrs < 3 {
		t.Fatalf("bitstream: NAL types start %v, %d IDRs", types[:min(len(types), 6)], idrs)
	}
	// Bad options: exit code 2 with the reason.
	cmd := exec.Command(exe, "--encode-test="+out, "--backend=mock", "--codec=h264", "--fps=9999")
	b, err = cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(b), "fps out of range") {
		t.Fatalf("bad encode test options: %v %s", err, b)
	}
	// --hdr (step 3.9) on the GPU test source, which plays an HDR output: the
	// started line says HDR10 (the hardware check runs it with --backend=amf /
	// nvenc and --capture=dda on an HDR desktop).
	b, err = exec.Command(exe, "--encode-test="+out, "--backend=mock", "--codec=h264", "--capture=synthetic-gpu", "--hdr=1",
		"--frames=10").CombinedOutput()
	if errors.As(err, &ee) && ee.ExitCode() == 2 && underWine() && strings.Contains(string(b), "D3D11") {
		t.Logf("--hdr skipped: no D3D11 device under Wine (needs an X display): %s", b)
		return
	}
	if err != nil || !strings.Contains(string(b), `"hdr":true,"bitDepth":10,"colorSpace":"bt2020-pq"`) {
		t.Fatalf("--encode-test --hdr=1: %v %s", err, b)
	}
}

// captureCheck starts a real capture method through the mock encoder: capture,
// pacing and the NV12 conversion run for real, the canned clip comes out.
// Where the method cannot work (Wine has no DuplicateOutput, a CI service
// session may have no desktop) start must fail cleanly and the helper must
// still start another capture. On a real host, RECON_HELPER_SECONDS=N reads
// frames for N seconds and logs the frame rate, idle repeats and the
// present -> capture latency; RECON_HELPER_NV12=file writes converted frame 30
// (view with ffplay -f rawvideo -pixel_format nv12 -video_size WxH file);
// RECON_HELPER_FPS, RECON_HELPER_HMONITOR (decimal or 0x hex),
// RECON_HELPER_WINDOW_TITLE (wgc) and RECON_HELPER_GPU_PRIORITY override the
// start parameters.
func captureCheck(t *testing.T, capture string, p StartParams) {
	if v, _ := strconv.Atoi(os.Getenv("RECON_HELPER_FPS")); v > 0 {
		p.FPS = v
	}
	if v, err := strconv.ParseUint(os.Getenv("RECON_HELPER_HMONITOR"), 0, 64); err == nil {
		p.HMonitor = v
	}
	if v := os.Getenv("RECON_HELPER_WINDOW_TITLE"); v != "" && capture == "wgc" {
		p.WindowTitle = v
	}
	p.GPUPriority = os.Getenv("RECON_HELPER_GPU_PRIORITY")
	var extra []string
	if f := os.Getenv("RECON_HELPER_NV12"); f != "" {
		extra = append(extra, "--dump-nv12="+f)
	}
	h := launchMock(t, extra...)
	if !slices.Contains(h.Caps().Capture, capture) {
		t.Skipf("%s unavailable here: %s", capture, h.Caps().Unavailable[capture])
	}
	p.Capture, p.Codec, p.Kbps = capture, "h264", 4000
	if p.FPS == 0 {
		p.FPS = 60
	}
	st, err := h.Start(p)
	if err != nil {
		var he *HelperError
		if !errors.As(err, &he) || he.Fatal || !slices.Contains([]string{"init_failed", "no_output", "unavailable", "unsupported"}, he.Code) {
			t.Fatalf("%s start: %v", capture, err)
		}
		t.Logf("%s start failed cleanly: %v", capture, err)
		if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
			t.Fatalf("synthetic start after a failed %s start: %v", capture, err)
		}
		nextFrame(t, h)
		return
	}
	t.Logf("%s started: %+v", capture, st)
	if st.Capture != capture || st.CaptureWidth <= 0 || st.AdapterLUID == "" || st.GPUPriority == "" || st.IdleRepeatMs != 100 {
		t.Fatalf("started %+v", st)
	}
	if st.CursorInVideo {
		t.Fatalf("%s frames contain the mouse pointer although caps list it (cursorInVideo false)", capture)
	}
	// A static desktop still yields frames: idle repeats every 100 ms.
	var frames []*Frame
	for i := 0; i < 5; i++ {
		frames = append(frames, nextFrame(t, h))
	}
	if secs, _ := strconv.Atoi(os.Getenv("RECON_HELPER_SECONDS")); secs > 0 {
		for end := time.Now().Add(time.Duration(secs) * time.Second); time.Now().Before(end); {
			frames = append(frames, nextFrame(t, h))
		}
	}
	qpc := h.QPCFrequency()
	repeats, maxInSecond := 0, 0
	var lat []float64
	for i, f := range frames {
		if f.FrameID != uint64(i+1) {
			t.Fatalf("frame %d has id %d", i+1, f.FrameID)
		}
		if f.Repeat {
			repeats++
		} else if f.PresentQPC > 0 {
			lat = append(lat, float64(f.CaptureQPC-f.PresentQPC)*1000/float64(qpc))
		}
	}
	maxInSecond = maxPerSecond(frames, qpc)
	slices.Sort(lat)
	pct := func(q float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(q*float64(len(lat)-1))]
	}
	t.Logf("%s: %d frames, %d idle repeats, at most %d in one second (fps %d); present->capture p50 %.2f ms, p95 %.2f ms (%d frames with a present time)",
		capture, len(frames), repeats, maxInSecond, p.FPS, pct(0.5), pct(0.95), len(lat))
	if maxInSecond > p.FPS+2 {
		t.Fatalf("%d frames in one second at %d fps", maxInSecond, p.FPS)
	}
}

// maxPerSecond is the most frames submitted to the encoder within any one
// second. The pacer allows one new image above the fps in a window (a frame
// may use its slot a quarter interval early), and an idle repeat takes no slot,
// so the first new image after a pause can follow it at once
// (--self-test-pacer checks the exact bounds); submit times also carry the
// conversion time, so callers allow fps+2.
func maxPerSecond(frames []*Frame, qpc int64) int {
	best := 0
	for i, f := range frames {
		n := 0
		for _, g := range frames[i:] {
			if g.SubmitQPC-f.SubmitQPC >= qpc {
				break
			}
			n++
		}
		best = max(best, n)
	}
	return best
}

func TestHelperIntegrationDDA(t *testing.T) {
	captureCheck(t, "dda", StartParams{Width: 640, Height: 360, Barcode: &Barcode{Cell: proto.BarcodeCell}})
}

func TestHelperIntegrationAMDDirect(t *testing.T) {
	captureCheck(t, "amd-direct", StartParams{Width: 640, Height: 360})
}

// On an AMD host: a start that fails after the AMF encoder was initialized (a
// barcode that does not fit the picture, found by the colour conversion) must
// release the encoder before the capture is destroyed: with amd-direct the
// encoder lives on the capture's AMFContext, with dda it holds a VCN session.
// The next start in the same helper must then encode. Skips without the AMF
// backend (Wine, CI, NVIDIA hosts).
func TestHelperIntegrationAMFFailedStart(t *testing.T) {
	exe := helperExe(t)
	ran := 0
	for _, capture := range []string{"amd-direct", "dda"} {
		h, err := Launch(Options{Exe: exe, Backend: "amf", LogLevel: "debug",
			Log: slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		if c := h.Caps(); !c.Usable() || !slices.Contains(c.Capture, capture) {
			h.Close()
			t.Logf("AMF backend or %s unavailable here: %v", capture, c.Unavailable)
			continue
		}
		ran++
		p := StartParams{Capture: capture, Codec: "hevc", Width: 640, Height: 360, FPS: 60, Kbps: 4000}
		bad := p
		bad.Barcode = &Barcode{X: 600, Y: 0, Cell: proto.BarcodeCell}
		_, err = h.Start(bad)
		var he *HelperError
		if !errors.As(err, &he) || he.Code != "bad_message" || he.Fatal {
			h.Close()
			t.Fatalf("%s: start with a barcode outside the picture: %v", capture, err)
		}
		st, err := h.Start(p)
		if err != nil {
			h.Close()
			t.Fatalf("%s: start after a start that failed after the encoder init: %v", capture, err)
		}
		if f := nextFrame(t, h); !f.Key || f.FrameID != 1 {
			h.Close()
			t.Fatalf("%s: first frame %+v", capture, f)
		}
		t.Logf("%s: started after the failed start: %+v", capture, st)
		h.Close()
	}
	if ran == 0 {
		t.Skip("no AMF backend with amd-direct or dda here")
	}
}

// Windows.Graphics.Capture of monitor 0 (MSVC builds; the mingw build has no
// C++/WinRT and reports wgc unavailable).
func TestHelperIntegrationWGC(t *testing.T) {
	captureCheck(t, "wgc", StartParams{Width: 640, Height: 360})
}

// Phase 5 through the mock: the engine choice (EncoderInstanceFor
// "dedicated": engine 1 of the mock's two), the refusals of what the mock
// cannot do (three temporal layers, re-encode, sub-frame output, a third
// engine: unsupported, and the helper keeps running), SetFPS (the capture
// re-paces at once, the stats report it, no key frame follows), and the
// synthetic source's unknown dirty share. Its two temporal layers:
// TestHelperIntegrationSVC.
func TestHelperIntegrationPhase5(t *testing.T) {
	h := launchMock(t)
	cc := h.Caps().Codecs["h264"]
	if cc.LiveFPS != "seamless" || !cc.InstanceSelect || cc.HWInstances != 2 || cc.Reencode || cc.MaxTemporalLayers != 2 ||
		cc.ROI != "importance" || !cc.SliceOutput {
		t.Fatalf("mock caps %+v", cc)
	}
	two := 2
	for _, p := range []StartParams{
		{Codec: "h264", FPS: 60, Kbps: 4000, SVCLayers: 3},
		{Codec: "h264", FPS: 60, Kbps: 4000, ReencodeOversized: 3},
		{Codec: "h264", FPS: 60, Kbps: 4000, EncoderInstance: &two},
	} {
		var he *HelperError
		if _, err := h.Start(p); !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
			t.Fatalf("start %+v: %v, want unsupported", p, err)
		}
	}
	var he *HelperError
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000, ReencodeOversized: 1.2}); !errors.As(err, &he) || he.Code != "bad_message" {
		t.Fatalf("reencodeOversized 1.2: %v, want bad_message", err)
	}
	inst, err := EncoderInstanceFor("dedicated", cc)
	if err != nil || inst == nil || *inst != 1 {
		t.Fatalf("EncoderInstanceFor: %v %v", inst, err)
	}
	st, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000, EncoderInstance: inst})
	if err != nil || st.EncoderInstance != 1 || st.HWInstances != 2 || st.LiveFPS != "seamless" || st.SVCLayers != 1 || st.SliceOutput != 0 {
		t.Fatalf("start: %+v %v", st, err)
	}
	var before []*Frame
	for t0 := time.Now(); time.Since(t0) < time.Second; {
		before = append(before, nextFrame(t, h))
	}
	if err := h.SetFPS(20); err != nil {
		t.Fatal(err)
	}
	var after []*Frame
	for t0 := time.Now(); time.Since(t0) < 2*time.Second; {
		after = append(after, nextFrame(t, h))
	}
	qpc := h.QPCFrequency()
	late := after[len(after)/4:] // well after the change
	rateBefore, rateAfter := maxPerSecond(before, qpc), maxPerSecond(late, qpc)
	t.Logf("%d frames/s at 60 fps, %d at 20 fps", rateBefore, rateAfter)
	if rateBefore < 48 || rateAfter > 22 || rateAfter < 16 {
		t.Fatalf("SetFPS(20): %d frames per second before, %d after", rateBefore, rateAfter)
	}
	for _, f := range append(before, after...) {
		// Key frames only where the mock's 60-frame clip starts over: the
		// frame-rate change forced none.
		if f.Key != ((f.FrameID-1)%60 == 0) || f.Dirty != -1 || f.Discardable {
			t.Fatalf("frame %d: key %v, dirty %v, discardable %v", f.FrameID, f.Key, f.Dirty, f.Discardable)
		}
	}
	sawFPS := false
	for deadline := time.After(2 * time.Second); !sawFPS; {
		select {
		case s := <-h.Stats():
			sawFPS = s.FPS == 20 && s.Kbps == 4000 && s.Dirty == -1
		case <-deadline:
			t.Fatal("no stats with fps 20 (and the bitrate unchanged)")
		}
	}
}

// TestHelperIntegrationSlicesAndROI: the mock's emulated sub-frame output and
// its ROI (Phase 5 wiring B plumbing): started sliceOutput 2, every frame
// with 2 slices and a first-slice time between its submit and its output
// through the ring (the stats say the same); setRoi accepted (the mock logs
// the rects) and cleared, the stream unharmed.
func TestHelperIntegrationSlicesAndROI(t *testing.T) {
	logs := &lockedBuf{}
	h, err := Launch(Options{Exe: helperExe(t), Backend: "mock", LogLevel: "debug",
		Log: slog.New(slog.NewTextHandler(io.MultiWriter(logs, testLogWriter{t}), &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	st, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000, SliceOutput: 2})
	if err != nil || st.SliceOutput != 2 {
		t.Fatalf("start: %+v %v", st, err)
	}
	rects := FocusROI(320, 180, 320, 180, &image.Point{X: 160, Y: 90}, FocusOptions{CenterSize: -1})
	if err := h.SetROI(rects); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		f := nextFrame(t, h)
		if f.Slices != 2 || f.FirstSliceQPC < f.SubmitQPC || f.FirstSliceQPC > f.OutputQPC || f.FirstSliceQPC == 0 {
			t.Fatalf("frame %d: slices %d, submit %d first slice %d output %d", f.FrameID, f.Slices, f.SubmitQPC, f.FirstSliceQPC, f.OutputQPC)
		}
	}
	if err := h.SetROI(nil); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, h)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		set, cleared := logs.has(fmt.Sprintf("mock: setRoi 1 rect(s): %d,%d %dx%d weight 6;", rects[0].X, rects[0].Y, rects[0].W, rects[0].H)),
			logs.has("mock: setRoi 0 rect(s):")
		if set && cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mock did not log the regions set (%v) and cleared (%v)", set, cleared)
		}
	}
	for deadline := time.After(2 * time.Second); ; {
		select {
		case s := <-h.Stats():
			if s.Slices == 2 && s.FirstSliceQPC != 0 {
				return
			}
		case err := <-h.Errors():
			t.Fatalf("helper error: %v", err)
		case <-deadline:
			t.Fatal("no stats with slices")
		}
	}
}

// lockedBuf collects log output.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) has(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), s)
}

// TestHelperIntegrationSVC: the mock's two temporal layers (Phase 5 SVC):
// started svcLayers 2; a key frame, then pairs of an enhancement-layer frame
// (layer 1, discardable: Droppable, the non-reference copy of the canned P
// frame after it) and a base-layer frame (layer 0, referenced), through the
// ring's flags; the stats say the same; a forced IDR starts the pattern
// again. With RECON_FFMPEG (a Windows ffmpeg.exe) the stream and the stream
// without the discardable frames both decode cleanly, the base frames to the
// same pictures.
func TestHelperIntegrationSVC(t *testing.T) {
	h := launchMock(t)
	st, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000, SVCLayers: 2})
	if err != nil || st.SVCLayers != 2 {
		t.Fatalf("start: %+v %v", st, err)
	}
	var frames []*Frame
	for len(frames) < 50 {
		frames = append(frames, nextFrame(t, h))
		if len(frames) == 20 {
			if err := h.ForceIDR(); err != nil {
				t.Fatal(err)
			}
		}
	}
	sinceKey, keys, drop := -1, 0, 0
	for _, f := range frames {
		if f.Key {
			sinceKey, keys = 0, keys+1
		} else if sinceKey >= 0 {
			sinceKey++
		}
		enh := sinceKey > 0 && sinceKey%2 == 1 // key, then copy (layer 1) / P (layer 0) pairs
		if sinceKey < 0 || f.TemporalLayer != map[bool]uint32{true: 1, false: 0}[enh] || f.Discardable != enh || f.Droppable() != enh ||
			(f.Key && f.Discardable) {
			t.Fatalf("frame %d (%d after the key frame): layer %d, discardable %v, key %v", f.FrameID, sinceKey, f.TemporalLayer, f.Discardable, f.Key)
		}
		if enh {
			drop++
		}
	}
	if keys != 2 || drop < 20 {
		t.Fatalf("%d key frames (want 2: the first, the forced IDR), %d droppable of %d", keys, drop, len(frames))
	}
stats:
	for deadline := time.After(2 * time.Second); ; {
		select {
		case s := <-h.Stats():
			if s.TemporalLayer == 1 {
				if !s.Discardable {
					t.Fatalf("stats of layer-1 frame %d not discardable", s.FrameID)
				}
				break stats
			}
		case <-deadline:
			t.Fatal("no stats of a layer-1 frame")
		}
	}
	ffmpeg := os.Getenv("RECON_FFMPEG")
	if ffmpeg == "" {
		t.Log("set RECON_FFMPEG to a Windows ffmpeg.exe to decode the layers")
		return
	}
	var all, base []byte
	for _, f := range frames {
		all = append(all, f.Data...)
		if !f.Droppable() {
			base = append(base, f.Data...)
		}
	}
	md5s := func(name string, data []byte) []string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-v", "error", "-f", "h264", "-i", path, "-f", "framemd5", "-")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(stderr.String()) != "" {
			t.Fatalf("%s: %v %s", name, err, stderr.String())
		}
		var sums []string
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				sums = append(sums, l[strings.LastIndex(l, ",")+1:])
			}
		}
		return sums
	}
	full, thin := md5s("all.h264", all), md5s("base.h264", base)
	var want []string
	for i, f := range frames {
		if !f.Droppable() && i < len(full) {
			want = append(want, strings.TrimSpace(full[i]))
		}
	}
	for i := range thin {
		thin[i] = strings.TrimSpace(thin[i])
	}
	if len(full) != len(frames) || strings.Join(thin, ",") != strings.Join(want, ",") {
		t.Fatalf("decoded %d of %d frames; the base layer alone decodes to other pictures (%d / %d)", len(full), len(frames), len(thin), len(want))
	}
	t.Logf("%d frames decoded clean, the %d base-layer frames alone to the same pictures", len(full), len(thin))
}

// The present-driven capture path end to end on the GPU test source: a
// simulated game presents at twice the stream's fps for 1 s, then pauses
// 0.6 s. Frames must be capped at the fps, repeats must fill the pauses
// every 100 ms, and the frame dumped after conversion must carry its frame
// id in the barcode.
func TestHelperIntegrationGPUPipeline(t *testing.T) {
	dump := t.TempDir() + `\frame30.nv12`
	h := launchMock(t, "--dump-nv12="+dump)
	const w, hgt, fps = 320, 180, 30
	st, err := h.Start(StartParams{Capture: "synthetic-gpu", Codec: "h264", Width: w, Height: hgt, FPS: fps, Kbps: 4000,
		Barcode: &Barcode{Cell: proto.BarcodeCell}})
	var he *HelperError
	if errors.As(err, &he) && he.Code == "init_failed" && underWine() {
		t.Skipf("no D3D11 device under Wine (needs an X display): %v", err)
	}
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.HDR || st.BitDepth != 8 || st.ColorSpace != "bt709" || st.HDRMetadata != nil {
		t.Fatalf("an SDR stream reports hdr %v, bitDepth %d, colorSpace %q", st.HDR, st.BitDepth, st.ColorSpace)
	}
	// A forced IDR after frame 10 starts a new sequence: the barcodes from it
	// on count from 0 again.
	var frames []*Frame
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		frames = append(frames, nextFrame(t, h))
		if len(frames) == 10 {
			if err := h.ForceIDR(); err != nil {
				t.Fatal(err)
			}
		}
	}
	seqBase := uint64(0)
	for _, f := range frames {
		if f.SeqStart != (f.FrameID == 1 || f.FrameID > 10 && seqBase == 0 && f.Key) || f.SeqStart && !f.Key {
			t.Fatalf("frame %d: key %v seqStart %v (sequence starts: frame 1 and the first key frame after 10)", f.FrameID, f.Key, f.SeqStart)
		}
		if f.SeqStart && f.FrameID > 1 {
			seqBase = f.FrameID
		}
	}
	if seqBase == 0 || seqBase > 30 {
		t.Fatalf("forced IDR after frame 10 started a sequence at frame %d", seqBase)
	}
	qpc := h.QPCFrequency()
	repeats, maxInSecond := 0, 0
	for i, f := range frames {
		if f.FrameID != uint64(i+1) {
			t.Fatalf("frame %d has id %d", i+1, f.FrameID)
		}
		if f.Repeat != (f.PresentQPC == 0) {
			t.Fatalf("frame %d: repeat %v with presentQpc %d", f.FrameID, f.Repeat, f.PresentQPC)
		}
		if f.Repeat {
			repeats++
		}
		// The test source changes the whole image with every present (Phase 5
		// dirty share 1); an idle repeat changes nothing.
		if want := map[bool]float64{false: 1, true: 0}[f.Repeat]; f.Dirty != want {
			t.Fatalf("frame %d (repeat %v): dirty %v, want %v", f.FrameID, f.Repeat, f.Dirty, want)
		}
	}
	maxInSecond = maxPerSecond(frames, qpc)
	t.Logf("%d frames in 3.5 s, %d idle repeats, at most %d in one second", len(frames), repeats, maxInSecond)
	if maxInSecond > fps+2 || maxInSecond < fps*8/10 {
		t.Fatalf("%d frames in one second at %d fps", maxInSecond, fps)
	}
	if repeats < 6 {
		t.Fatalf("only %d idle repeats in two 0.6 s pauses", repeats)
	}

	// The converted frame 30: the barcode in its top-left corner (proto's
	// frame barcode, read as the browser reads it) is its sequence number.
	b, err := os.ReadFile(dump)
	if err != nil || len(b) != w*hgt*3/2 {
		t.Fatalf("dump: %d bytes, %v", len(b), err)
	}
	v, ok := proto.BarcodeReadLuma(b[:w*hgt], w, proto.BarcodeCell)
	if !ok || uint64(v) != 30-seqBase {
		t.Fatalf("barcode of the dumped frame 30 reads %d (valid %v), want %d (sequence from frame %d)", v, ok, 30-seqBase, seqBase)
	}
}

// The synthetic GPU source's high-motion mode (start "motion", the live-bitrate
// qualification's source, step 3.6): presents without pauses (no idle
// repeats), every image noisy, the barcode readable over it; and "motion"
// with another capture is refused.
func TestHelperIntegrationMotionSource(t *testing.T) {
	dump := t.TempDir() + `\frame30.nv12`
	h := launchMock(t, "--dump-nv12="+dump)
	const w, hgt, fps = 320, 180, 30
	_, err := h.Start(StartParams{Capture: "synthetic", Codec: "h264", FPS: fps, Kbps: 4000, Motion: true})
	if he := (*HelperError)(nil); !errors.As(err, &he) || he.Code != "bad_message" || !strings.Contains(he.Text, "motion") {
		t.Fatalf("motion with capture synthetic: %v", err)
	}
	_, err = h.Start(StartParams{Capture: "synthetic-gpu", Codec: "h264", Width: w, Height: hgt, FPS: fps, Kbps: 4000, Motion: true,
		Barcode: &Barcode{X: 16, Y: 16, Cell: proto.BarcodeCell}})
	var he *HelperError
	if errors.As(err, &he) && he.Code == "init_failed" && underWine() {
		t.Skipf("no D3D11 device under Wine (needs an X display): %v", err)
	}
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var frames []*Frame
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		frames = append(frames, nextFrame(t, h))
	}
	// The motion source presents all the time (the plain one pauses 0.6 s in
	// every 1.6 s). A starved capture thread may still miss a present now and
	// then (an idle repeat 100 ms later), so a few repeats are fine, a pause
	// is not.
	repeats, last, longest := 0, int64(0), int64(0)
	for _, f := range frames {
		if f.Repeat {
			repeats++
		}
		if f.PresentQPC > last {
			if last > 0 {
				longest = max(longest, f.PresentQPC-last)
			}
			last = f.PresentQPC
		}
	}
	if repeats*20 > len(frames) || longest > h.QPCFrequency()*4/10 {
		t.Fatalf("%d idle repeats in %d frames, %d ms at most between presents: the motion source must present all the time",
			repeats, len(frames), longest*1000/h.QPCFrequency())
	}
	if n := maxPerSecond(frames, h.QPCFrequency()); n > fps+2 || n < fps*8/10 {
		t.Fatalf("%d frames in one second at %d fps", n, fps)
	}
	b, err := os.ReadFile(dump)
	if err != nil || len(b) != w*hgt*3/2 {
		t.Fatalf("dump: %d bytes, %v", len(b), err)
	}
	// The barcode at (16, 16) reads 29 (frame 30 of the sequence from frame 1).
	if v, ok := proto.BarcodeReadLuma(b[16*w+16:w*hgt], w, proto.BarcodeCell); !ok || v != 29 {
		t.Fatalf("barcode reads %d (valid %v), want 29", v, ok)
	}
	// Noise: neighbouring luma samples below the barcode differ a lot.
	diff, n := 0, 0
	for y := 80; y < hgt; y++ {
		for x := 1; x < w; x++ {
			d := int(b[y*w+x]) - int(b[y*w+x-1])
			diff += max(d, -d)
			n++
		}
	}
	if avg := float64(diff) / float64(n); avg < 8 {
		t.Fatalf("mean neighbour difference %.1f: not the high-motion picture", avg)
	}
}

// HDR10 end to end on the GPU test source (step 3.9): with hdr it plays an
// output in Windows HDR mode (FP16 scRGB frames, a 1000 cd/m2 panel), the
// mock asks for P010, and the dumped frame must be P010 with BT.2020 PQ codes:
// the 10-bit code in the high bits of every 16-bit sample, the barcode cells
// at 64 / 940 (reading as the frame's sequence number), the source's 1000 cd/m2 patch (top right) at PQ code 723 with
// neutral chroma.
func TestHelperIntegrationHDRPipeline(t *testing.T) {
	dump := t.TempDir() + `\frame30.p010`
	h := launchMock(t, "--dump-nv12="+dump)
	const w, hgt, fps = 320, 180, 30
	st, err := h.Start(StartParams{Capture: "synthetic-gpu", Codec: "h264", Width: w, Height: hgt, FPS: fps, Kbps: 4000, HDR: true,
		Barcode: &Barcode{Cell: proto.BarcodeCell}})
	var he *HelperError
	if errors.As(err, &he) && he.Code == "init_failed" && underWine() {
		t.Skipf("no D3D11 device under Wine (needs an X display): %v", err)
	}
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	m := st.HDRMetadata
	if !st.HDR || st.BitDepth != 10 || st.ColorSpace != "bt2020-pq" || m == nil || m.MaxLuminance != 1000 || m.MinLuminance != 0.005 ||
		m.MaxCLL != 1000 || m.MaxFALL != 400 || m.DisplayPrimaries[0] != [2]float64{0.708, 0.292} {
		t.Fatalf("started: hdr %v, bitDepth %d, colorSpace %q, metadata %+v", st.HDR, st.BitDepth, st.ColorSpace, m)
	}
	for i := 0; i < 32; i++ {
		nextFrame(t, h)
	}
	b, err := os.ReadFile(dump)
	if err != nil || len(b) != w*hgt*3 {
		t.Fatalf("dump: %d bytes (want %d: P010), %v", len(b), w*hgt*3, err)
	}
	sample := func(i int) int { return int(b[2*i]) | int(b[2*i+1])<<8 }
	for i := 0; i < len(b)/2; i++ {
		if sample(i)&63 != 0 {
			t.Fatalf("P010 sample %d = %#04x: the low 6 bits are not zero", i, sample(i))
		}
	}
	code := func(i int) int { return sample(i) >> 6 }
	// The barcode (top left): every cell 64 or 940 at its centre, and read
	// as the browser reads it (the codes / 4 as 8-bit luma) frame 30's
	// sequence number, 29 (the sequence from frame 1).
	for k := 0; k < proto.BarcodeBits; k++ {
		x, y := (k%proto.BarcodeCols)*proto.BarcodeCell+proto.BarcodeCell/2, (k/proto.BarcodeCols)*proto.BarcodeCell+proto.BarcodeCell/2
		if v := code(y*w + x); v != 64 && v != 940 {
			t.Fatalf("barcode cell %d luma %d, want 64 or 940", k, v)
		}
	}
	luma := make([]byte, w*hgt)
	for i := range luma {
		luma[i] = byte(code(i) >> 2)
	}
	if v, ok := proto.BarcodeReadLuma(luma, w, proto.BarcodeCell); !ok || v != 29 {
		t.Fatalf("barcode of the dumped frame reads %d (valid %v), want 29", v, ok)
	}
	// The 1000 cd/m2 patch: Y 723 (64 + 876 x PQ(1000 cd/m2) = 722.6), Cb Cr 512.
	x, y := 312, 8
	cb, cr := code(w*hgt+(y/2)*w+x), code(w*hgt+(y/2)*w+x+1)
	if yy := code(y*w + x); yy < 722 || yy > 724 || cb < 511 || cb > 513 || cr < 511 || cr > 513 {
		t.Fatalf("1000 cd/m2 patch: YCbCr %d,%d,%d, want 723,512,512", yy, cb, cr)
	}
}

// The capture texture formats AMD Direct Capture can hand out besides 8-bit
// UNORM and FP16 (final review), played by the GPU test source (start
// testFormat): a fully typed sRGB texture (a game's sRGB swap chain) and a
// 10-bit sRGB-coded one convert into NV12 frames (the barcode of the dumped
// frame 30 reads 29, the picture keeps its top-to-bottom gradient); a 10-bit
// one holding BT.2020 PQ (hdr: the output in HDR mode, an HDR10 swap chain)
// goes into P010 as it is (the 1000 cd/m2 patch, PQ code 769, at Y 722-723);
// and a format the conversion cannot read ends the helper with the fatal
// capture_failed within seconds instead of streaming nothing.
func TestHelperIntegrationCaptureFormats(t *testing.T) {
	const w, hgt, fps = 320, 180, 30
	start := func(t *testing.T, h *Helper, p StartParams) Started {
		t.Helper()
		p.Capture, p.Codec, p.Width, p.Height, p.FPS, p.Kbps = "synthetic-gpu", "h264", w, hgt, fps, 4000
		st, err := h.Start(p)
		var he *HelperError
		if errors.As(err, &he) && he.Code == "init_failed" && underWine() {
			t.Skipf("no D3D11 device under Wine (needs an X display): %v", err)
		}
		if err != nil {
			t.Fatalf("start %+v: %v", p, err)
		}
		return st
	}
	for _, f := range []string{"bgra-srgb", "rgb10a2"} {
		t.Run(f, func(t *testing.T) {
			dump := t.TempDir() + `\frame30.nv12`
			h := launchMock(t, "--dump-nv12="+dump)
			if st := start(t, h, StartParams{TestFormat: f, Barcode: &Barcode{Cell: proto.BarcodeCell}}); st.HDR || st.BitDepth != 8 {
				t.Fatalf("started hdr %v, bitDepth %d", st.HDR, st.BitDepth)
			}
			for i := 0; i < 32; i++ {
				nextFrame(t, h)
			}
			b, err := os.ReadFile(dump)
			if err != nil || len(b) != w*hgt*3/2 {
				t.Fatalf("dump: %d bytes, %v", len(b), err)
			}
			if v, ok := proto.BarcodeReadLuma(b[:w*hgt], w, proto.BarcodeCell); !ok || v != 29 {
				t.Fatalf("barcode of the dumped frame 30 reads %d (valid %v), want 29", v, ok)
			}
			// Green rises from 0 at the top to 255 at the bottom (the test
			// pattern), about 157 luma codes; columns 200-239 keep clear of
			// the barcode and of most of the moving bar.
			mean := func(y0 int) float64 {
				sum := 0
				for y := y0; y < y0+8; y++ {
					for x := 200; x < 240; x++ {
						sum += int(b[y*w+x])
					}
				}
				return float64(sum) / (8 * 40)
			}
			if top, bottom := mean(4), mean(hgt-12); bottom-top < 100 {
				t.Fatalf("mean luma %.1f at the top, %.1f at the bottom: not the test pattern's gradient", top, bottom)
			}
		})
	}
	t.Run("rgb10a2 PQ", func(t *testing.T) {
		dump := t.TempDir() + `\frame30.p010`
		h := launchMock(t, "--dump-nv12="+dump)
		if st := start(t, h, StartParams{TestFormat: "rgb10a2", HDR: true}); !st.HDR || st.BitDepth != 10 || st.ColorSpace != "bt2020-pq" {
			t.Fatalf("started hdr %v, bitDepth %d, colorSpace %q", st.HDR, st.BitDepth, st.ColorSpace)
		}
		for i := 0; i < 32; i++ {
			nextFrame(t, h)
		}
		b, err := os.ReadFile(dump)
		if err != nil || len(b) != w*hgt*3 {
			t.Fatalf("dump: %d bytes (want %d: P010), %v", len(b), w*hgt*3, err)
		}
		code := func(i int) int { return (int(b[2*i]) | int(b[2*i+1])<<8) >> 6 }
		// 64 + 876 x 769 / 1023 = 722.5; read as sRGB it would be far brighter.
		x, y := 312, 8
		cb, cr := code(w*hgt+(y/2)*w+x), code(w*hgt+(y/2)*w+x+1)
		if yy := code(y*w + x); yy < 721 || yy > 724 || cb < 511 || cb > 513 || cr < 511 || cr > 513 {
			t.Fatalf("1000 cd/m2 PQ patch: YCbCr %d,%d,%d, want 722,512,512", yy, cb, cr)
		}
	})
	t.Run("rgba16", func(t *testing.T) {
		h := launchMock(t)
		start(t, h, StartParams{TestFormat: "rgba16"})
		began := time.Now()
		he := waitHelperError(t, h, "capture_failed")
		if !he.Fatal || !strings.Contains(he.Text, "cannot convert capture format") {
			t.Fatalf("capture_failed %+v: want fatal, naming the conversion failure", he)
		}
		t.Logf("fatal after %v: %s", time.Since(began).Round(time.Millisecond), he.Text)
	})
}

// The libavcodec backend (native/recon-encoder/src/lavc, step 3.8). Without
// FFmpeg's DLLs it is unavailable with the reason. With RECON_FFMPEG_DIR set to
// the bin directory of an FFmpeg 8.x shared build that has libx264 (BtbN's
// ffmpeg-n8.1-latest-win64-gpl-shared-8.1; make helper-test FFMPEG_DIR=...),
// --lavc-test-encoder=libx264 drives that software encoder through the
// backend's code path in place of Quick Sync Video: the DLLs loaded at run
// time, frames submitted (the synthetic source's test pattern, and the GPU
// test source's converted frames read back, barcode included), forced IDRs,
// rate and frame-rate changes, the packets through the ring. The bitstream
// must decode cleanly (the build's ffmpeg.exe, when it is there).
func TestHelperIntegrationLavc(t *testing.T) {
	exe := helperExe(t)
	h, err := Launch(Options{Exe: exe, Backend: "lavc", FFmpegDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := h.Caps()
	if c.Backend != "none" || !strings.Contains(c.Unavailable["lavc"], "avcodec-62.dll") {
		t.Fatalf("lavc without DLLs: backend %q, unavailable %v", c.Backend, c.Unavailable)
	}
	var he *HelperError
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); !errors.As(err, &he) || he.Code != "unavailable" {
		t.Fatalf("start without DLLs: %v", err)
	}
	h.Close()

	dir := os.Getenv("RECON_FFMPEG_DIR")
	if dir == "" {
		t.Skip("set RECON_FFMPEG_DIR to the bin directory of an FFmpeg 8.x shared build with libx264 for the stream checks")
	}
	launch := func(t *testing.T) *Helper {
		h, err := Launch(Options{Exe: exe, Backend: "lavc", FFmpegDir: dir, LogLevel: "debug", Args: []string{"--lavc-test-encoder=libx264"},
			Log: slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	// decode runs the build's ffmpeg.exe on a bitstream (args: its output
	// options) and returns stdout; nil when the build has no ffmpeg.exe.
	decode := func(t *testing.T, stream []byte, args ...string) []byte {
		ff := dir + `\ffmpeg.exe`
		if _, err := os.Stat(ff); err != nil {
			t.Logf("no %s: bitstream not decoded", ff)
			return nil
		}
		file := t.TempDir() + `\stream.h264`
		if err := os.WriteFile(file, stream, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(ff, append([]string{"-v", "error", "-i", file}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil || stderr.Len() > 0 {
			t.Fatalf("ffmpeg %v: %v %s", args, err, stderr.Bytes())
		}
		return out
	}
	hasNAL := func(au []byte, want ...byte) bool {
		types := nalTypes(au)
		for _, w := range want {
			if bytes.IndexByte(types, w) < 0 {
				return false
			}
		}
		return true
	}

	// No encoder opens (libopenh264 takes no NV12): unavailable.lavc carries
	// every encoder's error. Two encoders of one codec: the first that opens.
	t.Run("Reasons", func(t *testing.T) {
		h, err := Launch(Options{Exe: exe, Backend: "lavc", FFmpegDir: dir, Args: []string{"--lavc-test-encoder=libopenh264,nosuch"}})
		if err != nil {
			t.Fatal(err)
		}
		c := h.Caps()
		h.Close()
		why := c.Unavailable["lavc"]
		if c.Backend != "none" || !strings.Contains(why, "libopenh264: avcodec_open2") || !strings.Contains(why, "nosuch is not in") {
			t.Fatalf("no encoder opens: backend %q, unavailable %v", c.Backend, c.Unavailable)
		}
		t.Logf("unavailable.lavc: %s", why)
		h, err = Launch(Options{Exe: exe, Backend: "lavc", FFmpegDir: dir, Args: []string{"--lavc-test-encoder=libx264,libopenh264"}})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		st, err := h.Start(StartParams{Capture: "synthetic", Codec: "h264", Width: 320, Height: 180, FPS: 60, Kbps: 1000})
		if err != nil || st.Encoder != "libx264" {
			t.Fatalf("libx264 and libopenh264: start %+v %v, caps unavailable %v", st, err, h.Caps().Unavailable)
		}
	})

	// FFmpegDir relative to the working directory, with characters the
	// ANSI command line cannot carry as UTF-8 (or at all).
	t.Run("Paths", func(t *testing.T) {
		base := t.TempDir()
		const name = "ffmpeg-ü-ж"
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			if underWine() {
				t.Skipf("%v (Wine stores such names only under a UTF-8 Unix locale: LANG=C.UTF-8)", err)
			}
			t.Fatal(err)
		}
		for _, dll := range []string{"avcodec-62.dll", "avutil-60.dll", "swresample-6.dll"} {
			from, to := filepath.Join(dir, dll), filepath.Join(base, name, dll)
			if os.Link(from, to) != nil {
				b, err := os.ReadFile(from)
				if err == nil {
					err = os.WriteFile(to, b, 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Chdir(base)
		h, err := Launch(Options{Exe: exe, Backend: "lavc", FFmpegDir: name, Args: []string{"--lavc-test-encoder=libx264"}})
		if err != nil {
			t.Fatal(err)
		}
		c := h.Caps()
		h.Close()
		if c.Backend != "lavc" || c.Codecs["h264"].MaxW == 0 {
			t.Fatalf("FFmpegDir %q in %s: backend %q, unavailable %v", name, base, c.Backend, c.Unavailable)
		}
	})

	t.Run("Flush", func(t *testing.T) {
		h := launch(t)
		c := h.Caps()
		cc := c.Codecs["h264"]
		if c.Backend != "lavc" || !c.Usable() || !cc.ForceIDR || cc.Recovery != "none" || cc.MaxLTR != 0 || cc.LiveBitrate != "flush" ||
			cc.LiveFPS != "flush" || !cc.IsAssumed("liveBitrate") || cc.ROI != "none" || cc.HDR10 || cc.InstanceSelect {
			t.Fatalf("caps %+v", c)
		}
		// What the backend cannot do is refused (a failed start may be followed by another).
		for _, p := range []StartParams{
			{Capture: "synthetic", Codec: "hevc", FPS: 60, Kbps: 4000},
			{Capture: "synthetic", Codec: "h264", FPS: 60, Kbps: 4000, LTRSlots: 2},
			{Capture: "synthetic", Codec: "h264", FPS: 60, Kbps: 4000, HDR: true},
			{Capture: "synthetic", Codec: "h264", FPS: 60, Kbps: 4000, IntraRefreshFrames: 30},
		} {
			var he *HelperError
			if _, err := h.Start(p); !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
				t.Fatalf("start %+v: %v, want unsupported", p, err)
			}
		}
		st, err := h.Start(StartParams{Capture: "synthetic", Codec: "h264", Width: 320, Height: 180, FPS: 60, Kbps: 1000})
		if err != nil || st.Backend != "lavc" || st.Encoder != "libx264" || st.LiveBitrate != "flush" || st.LiveFPS != "flush" ||
			st.ZeroCopy || st.Width != 320 || st.BitDepth != 8 || st.ColorSpace != "bt709" {
			t.Fatalf("start: %+v %v", st, err)
		}
		var stream []byte
		next := func() *Frame {
			f := nextFrame(t, h)
			if f.DroppedBefore > 0 {
				t.Fatalf("frame %d: %d frames dropped before it", f.FrameID, f.DroppedBefore)
			}
			stream = append(stream, f.Data...)
			return f
		}
		waitKey := func(what string) *Frame {
			for i := 0; i < 10; i++ {
				if f := next(); f.Key {
					if !hasNAL(f.Data, 7, 8, 5) {
						t.Fatalf("%s: key frame %d without SPS / PPS / IDR (NAL types %v)", what, f.FrameID, nalTypes(f.Data))
					}
					t.Logf("%s: key frame %d (gen %d)", what, f.FrameID, f.Gen)
					return f
				}
			}
			t.Fatalf("%s: no key frame within 10 frames", what)
			return nil
		}
		first := waitKey("start")
		if first.FrameID != 1 || first.Width != 320 || first.Height != 180 {
			t.Fatalf("first frame %+v", first)
		}
		for i := 0; i < 10; i++ {
			if f := next(); f.Key {
				t.Fatalf("frame %d: a key frame nobody asked for", f.FrameID)
			}
		}
		if err := h.ForceIDR(); err != nil {
			t.Fatal(err)
		}
		k := waitKey("forceIdr")
		if err := h.SetRate(400, 0, 0); err != nil {
			t.Fatal(err)
		}
		// liveBitrate flush: the frame with the new rate is an IDR of a new generation.
		if r := waitKey("setRate (flush)"); r.Gen != k.Gen+1 {
			t.Fatalf("setRate: gen %d after %d, want a new generation", r.Gen, k.Gen)
		}
		if err := h.Recover(k.FrameID+20, nil); err != nil {
			t.Fatal(err)
		}
		waitKey("recover (recovery none: IDR)")
		if err := h.SetFPS(30); err != nil {
			t.Fatal(err)
		}
		waitKey("setRate fps (flush)")
		if err := h.SetROI([]ROIRect{{X: 0, Y: 0, W: 64, H: 64, Weight: 5}}); err != nil {
			t.Fatal(err)
		}
		if e := waitHelperError(t, h, "unsupported"); e.Re != "setRoi" || e.Fatal {
			t.Fatalf("setRoi: %+v", e)
		}
		for i := 0; i < 10; i++ {
			next()
		}
		decode(t, stream, "-f", "null", "-")
	})

	t.Run("Seamless", func(t *testing.T) {
		h := launch(t)
		st, err := h.Start(StartParams{Capture: "synthetic", Codec: "h264", Width: 320, Height: 180, FPS: 60, Kbps: 1000,
			LiveBitrate: "seamless"})
		if err != nil || st.LiveBitrate != "seamless" || st.LiveFPS != "seamless" {
			t.Fatalf("start: %+v %v", st, err)
		}
		if f := nextFrame(t, h); !f.Key {
			t.Fatal("the first frame is not a key frame")
		}
		for i := 0; i < 5; i++ {
			nextFrame(t, h)
		}
		if err := h.SetRate(400, 0, 0); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			if f := nextFrame(t, h); f.Key || f.Gen != 0 {
				t.Fatalf("frame %d after a seamless rate change: key %v, gen %d", f.FrameID, f.Key, f.Gen)
			}
		}
	})

	// The GPU test source through the colour conversion: the converted
	// frames are read back (Wine: as separate Y / CbCr textures), encoded,
	// and every decoded frame's barcode reads its sequence number (no forced
	// IDR: its frame id - 1).
	t.Run("GPU", func(t *testing.T) {
		h := launch(t)
		const w, hgt = 320, 180
		st, err := h.Start(StartParams{Capture: "synthetic-gpu", Codec: "h264", Width: w, Height: hgt, FPS: 30, Kbps: 2000,
			Barcode: &Barcode{Cell: proto.BarcodeCell}})
		var he *HelperError
		if errors.As(err, &he) && he.Code == "init_failed" && underWine() {
			t.Skipf("no D3D11 device under Wine (needs an X display): %v", err)
		}
		if err != nil || !st.Barcode || st.ZeroCopy || st.Encoder != "libx264" {
			t.Fatalf("start: %+v %v", st, err)
		}
		var frames []*Frame
		var stream []byte
		for len(frames) < 45 {
			f := nextFrame(t, h)
			if f.DroppedBefore > 0 || f.FrameID != uint64(len(frames)+1) {
				t.Fatalf("frame %d after %d frames (%d dropped before it)", f.FrameID, len(frames), f.DroppedBefore)
			}
			frames = append(frames, f)
			stream = append(stream, f.Data...)
		}
		raw := decode(t, stream, "-f", "rawvideo", "-pix_fmt", "gray", "-")
		if raw == nil {
			return
		}
		if len(raw) != len(frames)*w*hgt {
			t.Fatalf("decoded %d bytes, want %d frames of %dx%d", len(raw), len(frames), w, hgt)
		}
		for i, f := range frames {
			y := raw[i*w*hgt : (i+1)*w*hgt]
			if v, ok := proto.BarcodeReadLuma(y, w, proto.BarcodeCell); !ok || uint64(v) != f.FrameID-1 {
				t.Fatalf("decoded frame %d: barcode reads %d (valid %v), want %d (frame id %d)", i, v, ok, f.FrameID-1, f.FrameID)
			}
		}
		t.Logf("%d frames decoded, every barcode reads its sequence number", len(frames))
	})
}
