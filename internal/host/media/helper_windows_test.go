//go:build windows

package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Tests against the real recon-encoder.exe (RECON_HELPER_EXE; make helper-test
// runs them under Wine): HelperVideo with the mock backend, and the GPU
// priority decision table shared with the helper.

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

// TestGPUPriorityAgreesWithHelper: recon-host applies the GPU priority to the
// FFmpeg encoder process (gpuPriorityClass), the native helper to itself
// (gpuPriorityFor in native/recon-encoder/src/platform/gpu_sched.cpp), both
// from host config "gpuPriority": one decision table, which both must apply
// alike for every mode, vendor (the helper's capture and encoder share one
// adapter: encoder and adapter vendor are the same) and HAGS state.
func TestGPUPriorityAgreesWithHelper(t *testing.T) {
	out, err := exec.Command(helperExe(t), "--gpu-priority-table").Output()
	if err != nil {
		t.Fatalf("--gpu-priority-table: %v", err)
	}
	hags := map[string]string{"on": hagsOn, "off": hagsOff, "unknown": hagsUnknown}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var row struct{ Mode, Vendor, Hags, Priority string }
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &row); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		want := "off"
		if class, ok := gpuPriorityClass(row.Vendor, row.Mode, gpuHost{adapter: row.Vendor, hags: hags[row.Hags]}); ok {
			want = map[uint32]string{gpuClassHigh: "high", gpuClassRealtime: "realtime"}[class]
		}
		if row.Priority != want || hags[row.Hags] == "" {
			t.Errorf("mode %s, %s, HAGS %s: the helper decides %s, recon-host %s", row.Mode, row.Vendor, row.Hags, row.Priority, want)
		}
		n++
	}
	if n != 4*4*3 {
		t.Fatalf("%d rows, want every mode x vendor x HAGS state (48)", n)
	}
}

// TestHelperVideoIntegration drives HelperVideo with the real helper's mock
// backend on its synthetic GPU source (Wine needs an X display for D3D11):
// the stream, host-clock stamps, a forced key frame without a new helper, a
// live bitrate change, a restart after a fatal error (timed), and giving up.
func TestHelperVideoIntegration(t *testing.T) {
	exe := helperExe(t)
	log := slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var mu sync.Mutex
	launches := 0
	fatalAt := "--mock-fatal-at=45" // the first helper fails at frame 45
	setFatal := func(arg string) {
		mu.Lock()
		fatalAt = arg
		mu.Unlock()
	}
	launched := func() int {
		mu.Lock()
		defer mu.Unlock()
		return launches
	}
	launch := func() (*encoder.Helper, error) {
		mu.Lock()
		launches++
		args := []string{}
		if fatalAt != "" {
			args = append(args, fatalAt)
		}
		mu.Unlock()
		return encoder.Launch(encoder.Options{Exe: exe, Backend: "mock", Args: args, Log: log})
	}
	clock := NewHostClock()
	v := NewHelperVideo(HelperOptions{Launch: launch, Log: log, Clock: clock})
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: 320, NativeH: 180},
		Encoder: EncoderInfo{Name: "h264_mock_helper", Family: "h264", Vendor: "mock", HW: true, Helper: true},
		FPS:     30, BitrateKbps: 4000, Barcode: true, GPUPriority: GPUPriorityAuto}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	next := func() VideoEvent {
		t.Helper()
		select {
		case ev := <-v.Events():
			return ev
		case <-time.After(10 * time.Second):
			t.Fatal("no video event")
		}
		return VideoEvent{}
	}
	ev := next()
	if ev.Err != nil {
		var he *encoder.HelperError
		if errors.As(ev.Err, &he) && he.Code == "init_failed" {
			t.Skipf("no D3D11 device for the synthetic GPU source (Wine needs an X display): %v", ev.Err)
		}
		t.Fatalf("start: %v", ev.Err)
	}
	if c := ev.Config; c == nil || c.Gen != 1 || c.Family != "h264" || !strings.HasPrefix(c.Codec, "avc1.") || c.Width != 320 ||
		c.Encoder != "h264_mock_helper" || c.Capture != "synthetic-gpu" {
		t.Fatalf("config %+v", ev.Config)
	}
	if c := v.Capabilities(); !c.ForceIDR || !c.LiveBitrate || c.Recovery != RecoveryKeyframe {
		t.Fatalf("capabilities %+v", c)
	}
	frame := func() *Frame {
		t.Helper()
		for {
			ev := next()
			if ev.Frame != nil {
				return ev.Frame
			}
			if ev.Err != nil || ev.Lost != nil {
				t.Fatalf("event %+v", ev)
			}
		}
	}
	// Stamps: the helper's QPC readings are the host clock's (same counter).
	seq := uint32(0)
	for i := 0; i < 10; i++ {
		f := frame()
		now := clock.Now()
		if f.Gen != 1 || f.Seq != seq || f.Key != (seq == 0) || f.CaptureUs == 0 || f.SubmitUs < f.CaptureUs ||
			f.EncodeDoneUs < f.SubmitUs || f.EncodeDoneUs > now || now-f.EncodeDoneUs > 200_000 || f.PresentUs > f.CaptureUs {
			t.Fatalf("frame %d (now %d): %+v", i, now, *f)
		}
		seq++
	}
	// A forced key frame: a new generation from the running helper.
	t0 := time.Now()
	if err := v.ForceKeyframe(); err != nil {
		t.Fatal(err)
	}
	for {
		ev := next()
		if ev.Config != nil {
			if ev.Config.Gen != 2 {
				t.Fatalf("config after the forced key frame %+v", ev.Config)
			}
			break
		}
	}
	if f := frame(); f.Gen != 2 || f.Seq != 0 || !f.Key {
		t.Fatalf("forced key frame %+v", f)
	}
	t.Logf("forced key frame %v after the request", time.Since(t0).Round(time.Millisecond))
	if err := v.SetRate(2500, 0); err != nil {
		t.Fatal(err)
	}
	if p, _ := v.Current(); p.BitrateKbps != 2500 || launched() != 1 {
		t.Fatalf("after SetRate: %d kbps, %d helpers launched", p.BitrateKbps, launched())
	}
	// The fatal error at frame 45: a new helper, a new generation with a key frame.
	setFatal("")
	var failedAt time.Time
	for {
		ev := next()
		if ev.Err != nil {
			if !ev.Restarted || ev.Fallback {
				t.Fatalf("failure event %+v", ev)
			}
			failedAt = time.Now()
			continue
		}
		if ev.Config != nil && !failedAt.IsZero() {
			if ev.Config.Gen != 3 {
				t.Fatalf("config after the restart %+v", ev.Config)
			}
			break
		}
	}
	if f := frame(); f.Gen != 3 || f.Seq != 0 || !f.Key {
		t.Fatalf("first frame after the restart %+v", f)
	}
	t.Logf("restart without a spare helper: first frame of the new helper %v after the failure was seen",
		time.Since(failedAt).Round(time.Millisecond))
	if p, _ := v.Current(); p.BitrateKbps != 2500 || launched() != 2 {
		t.Fatalf("after the restart: %d kbps, %d helpers launched", p.BitrateKbps, launched())
	}
	// Every helper from now on fails at once: two more failures give up.
	setFatal("--mock-fatal-at=1")
	if err := v.Start(Params{Source: Source{Backend: "test", NativeW: 640, NativeH: 360}, Encoder: p.Encoder, FPS: 30,
		BitrateKbps: 2500, GPUPriority: GPUPriorityAuto}, true); err != nil {
		t.Fatal(err)
	}
	for {
		ev := next()
		if ev.Fallback {
			if ev.Err == nil {
				t.Fatalf("fallback without an error %+v", ev)
			}
			break
		}
	}
	if err := v.Start(p, true); !errors.Is(err, ErrHelperGaveUp) {
		t.Fatalf("Start after giving up: %v", err)
	}
}

// TestHelperVideoSpareRestart times the restart after a fatal error with a
// spare helper kept launched (KeepSpare, as sessions run it): the restart
// skips the process start and the caps probe.
func TestHelperVideoSpareRestart(t *testing.T) {
	exe := helperExe(t)
	log := slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var mu sync.Mutex
	launches := 0
	launch := func() (*encoder.Helper, error) {
		mu.Lock()
		launches++
		var args []string
		if launches == 1 {
			args = []string{"--mock-fatal-at=60"}
		}
		mu.Unlock()
		return encoder.Launch(encoder.Options{Exe: exe, Backend: "mock", Args: args, Log: log})
	}
	v := NewHelperVideo(HelperOptions{Launch: launch, Log: log, Clock: NewHostClock(), KeepSpare: true})
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: 320, NativeH: 180},
		Encoder: EncoderInfo{Name: "h264_mock_helper", Family: "h264", Vendor: "mock", HW: true, Helper: true},
		FPS:     30, BitrateKbps: 4000, GPUPriority: GPUPriorityAuto}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	var failedAt time.Time
	deadline := time.After(20 * time.Second)
	for {
		var ev VideoEvent
		select {
		case ev = <-v.Events():
		case <-deadline:
			t.Fatal("no restart")
		}
		if ev.Err != nil {
			var he *encoder.HelperError
			if errors.As(ev.Err, &he) && he.Code == "init_failed" {
				t.Skipf("no D3D11 device for the synthetic GPU source (Wine needs an X display): %v", ev.Err)
			}
			if !ev.Restarted {
				t.Fatalf("failure event %+v", ev)
			}
			failedAt = time.Now()
			continue
		}
		if ev.Frame != nil && !failedAt.IsZero() && ev.Frame.Key && ev.Frame.Seq == 0 {
			break
		}
	}
	mu.Lock()
	n := launches
	mu.Unlock()
	t.Logf("restart with a spare helper: first frame of the new helper %v after the failure was seen (%d launches)",
		time.Since(failedAt).Round(time.Millisecond), n)
}
