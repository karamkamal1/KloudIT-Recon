package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// gdiAsked is a session's generation on a VM without a GPU at the default
// settings: GDI capture of a 1920x1080 monitor, libx264, 60 fps.
func gdiAsked() media.Params {
	return media.Params{Source: media.Source{Backend: "gdigrab", NativeW: 1920, NativeH: 1080}, FPS: 60, BitrateKbps: 30000,
		Quality: "balanced", Encoder: media.EncoderInfo{Name: "libx264", Family: "h264", Vendor: "software"}}
}

// TestBenchRung: the first rung from the agent's measurement fits cpuShare
// of what the CPU encoded at the rung's preset: a 4-vCPU VM streams the
// settings, a 2-vCPU one (the 57 fps measured here on two Xeon vCPUs)
// 1920x1080 at 45 fps with the fastest preset, a slow one 1600x900 at 30.
func TestBenchRung(t *testing.T) {
	for _, c := range []struct {
		name       string
		super, ult float64
		rung       int
		streams    string
	}{
		{"4 vCPUs", 110, 150, 0, "1920×1080 at 60 fps"},
		{"2 vCPUs", 57, 76, 2, "1920×1080 at 45 fps"},
		{"slow", 25, 35, 4, "1600×900 at 30 fps"},
		{"very slow", 4, 5, len(cpuRungs) - 1, "960×540 at 20 fps"},
	} {
		b := media.SoftwareBench{Encoder: "libx264", W: 1920, H: 1080, Superfast: c.super, Ultrafast: c.ult}
		r := benchRung(b, gdiAsked())
		if got := describeParams(rungParams(gdiAsked(), r)); r != c.rung || got != c.streams {
			t.Errorf("%s: rung %d (%s), want %d (%s)", c.name, r, got, c.rung, c.streams)
		}
	}
}

// TestCPURungs: a rung lowers the frame rate, the height (aspect kept, on
// GDI's CPU scaling only) and the preset; rungs that change nothing for a
// session (it asks for 1280x720 at 30 already) are skipped both ways.
func TestCPURungs(t *testing.T) {
	p := rungParams(gdiAsked(), 5)
	if w, h := p.OutputSize(); w != 1280 || h != 720 || p.FPS != 30 || p.Quality != "speed" {
		t.Fatalf("rung 5: %dx%d at %d, quality %s", w, h, p.FPS, p.Quality)
	}
	dda := gdiAsked()
	dda.Source.Backend = "ddagrab"
	if w, h := rungParams(dda, 6).OutputSize(); w != 1920 || h != 1080 {
		t.Fatalf("ddagrab (no CPU scaling) at rung 6: %dx%d", w, h)
	}
	small := gdiAsked()
	small.Width, small.Height, small.FPS = 1280, 720, 30
	if r := nextRung(small, 0, true); r != 1 {
		t.Fatalf("down from 0: %d", r)
	}
	if r := nextRung(small, 1, true); r != 6 {
		t.Fatalf("down from 1: %d, want 6 (540 lines: the first rung that changes 1280x720 at 30)", r)
	}
	if r := nextRung(small, 6, false); r != 1 {
		t.Fatalf("up from 6: %d", r)
	}
	if r := nextRung(small, len(cpuRungs)-1, true); r != len(cpuRungs)-1 {
		t.Fatalf("down from the last: %d", r)
	}
}

// TestCPUBudgetSession: on a VM without a GPU a session starts at the rung
// the agent's measurement allows (one informational notice, the overlay's
// text in the video config); a stream that falls behind (frames slower than
// the rate, frames waiting from capture to encoded) steps down after two
// looks; one at ease tries the rung above once the wait is over, and a step
// up that falls behind soon doubles the next wait. A hardware encoder, or
// host config "cpuBudget" "off", is never limited.
func TestCPUBudgetSession(t *testing.T) {
	a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, true))
	a.bench.res, a.bench.ok = media.SoftwareBench{Encoder: "libx264", W: 1920, H: 1080, Superfast: 57, Ultrafast: 76}, true
	s, ctrl := gdiSession(a, nil)
	s.cancel()
	s.monitor = platform.Monitor{W: 1920, H: 1080, Hz: 60, Primary: true, DXGIOutput: 0}
	asked := gdiAsked()
	p := asked
	s.cpuBudget(&p)
	if describeParams(p) != "1920×1080 at 45 fps" || p.Quality != "speed" || s.cpu.rung != 2 {
		t.Fatalf("first generation: %s, quality %s, rung %d", describeParams(p), p.Quality, s.cpu.rung)
	}
	q := asked
	s.cpuBudget(&q) // a restart: no second notice
	want := []string{"info: This PC encodes video on its CPU (no GPU encoder), which keeps up with 1920×1080 at 45 fps: streaming at that " +
		"instead of 1920×1080 at 60 fps. The overlay shows the limit; it rises again when the CPU has room."}
	if got := noticeLines(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices %q", got)
	}
	cfg := proto.VideoConfig{Gen: 1, FPS: 45}
	s.cpuConfig(&cfg)
	if cfg.CPULimit != "1920×1080 at 45 fps, fastest preset (asked: 1920×1080 at 60 fps): what this PC's CPU encodes" {
		t.Fatalf("overlay text %q", cfg.CPULimit)
	}

	// The stream: 30 frames a second of a 45 fps generation, each 150 ms
	// from capture to encoded.
	now := time.Now()
	look := func(fps float64, lag time.Duration, live media.Params) string {
		t.Helper()
		iv := time.Duration(float64(time.Second) / fps)
		for i := 0; i < int(fps*cpuWindow.Seconds()); i++ {
			now = now.Add(iv)
			done := uint64(now.UnixMicro())
			s.cpuFrame(&media.Frame{Gen: s.cpu.gen, CaptureUs: done - uint64(lag.Microseconds()), EncodeDoneUs: done}, now)
		}
		return s.cpuLook(now, live, true)
	}
	if why := look(30, 150*time.Millisecond, p); why != "" {
		t.Fatalf("first look behind: %q", why)
	}
	if why := look(30, 150*time.Millisecond, p); why != "the CPU does not keep up" || s.cpu.rung != 3 {
		t.Fatalf("second look behind: %q, rung %d", why, s.cpu.rung)
	}
	p = asked
	s.cpuBudget(&p)
	if describeParams(p) != "1920×1080 at 30 fps" {
		t.Fatalf("after the step down: %s", describeParams(p))
	}
	cfg = proto.VideoConfig{Gen: 2, FPS: 30}
	s.cpuConfig(&cfg)
	// At ease: no step up before the wait is over.
	for i := 0; i < cpuEaseWindows; i++ {
		if why := look(30, 20*time.Millisecond, p); why != "" {
			t.Fatalf("look %d at ease: %q", i, why)
		}
	}
	now = now.Add(cpuUpWait) // a gap in the frames (left out), and the wait is over
	if why := look(30, 20*time.Millisecond, p); why != "the CPU has room" || s.cpu.rung != 2 {
		t.Fatalf("at ease past the wait: %q, rung %d", why, s.cpu.rung)
	}
	// The rung above falls behind at once: back down, and the next try
	// waits twice as long.
	p = asked
	s.cpuBudget(&p)
	cfg = proto.VideoConfig{Gen: 3, FPS: 45}
	s.cpuConfig(&cfg)
	look(35, 120*time.Millisecond, p)
	if why := look(35, 120*time.Millisecond, p); why != "the CPU does not keep up" || s.cpu.rung != 3 || s.cpu.upWait != 2*cpuUpWait {
		t.Fatalf("failed step up: %q, rung %d, wait %v", why, s.cpu.rung, s.cpu.upWait)
	}

	// Not covered: a hardware encoder, the native helper, "cpuBudget" off.
	hw := asked
	hw.Encoder = media.EncoderInfo{Name: "h264_amf", Family: "h264", Vendor: "amd", HW: true}
	s.cpuBudget(&hw)
	if describeParams(hw) != "1920×1080 at 60 fps" || hw.Quality != "balanced" {
		t.Fatalf("hardware encoder limited: %s", describeParams(hw))
	}
	cfg = proto.VideoConfig{Gen: 4, FPS: 60}
	if s.cpuConfig(&cfg); cfg.CPULimit != "" {
		t.Fatalf("hardware encoder's overlay text %q", cfg.CPULimit)
	}
	a.cfg.CPUBudget = "off"
	off := asked
	s.cpuBudget(&off)
	if describeParams(off) != "1920×1080 at 60 fps" {
		t.Fatalf("cpuBudget off: %s", describeParams(off))
	}
}

// TestCPUBench: the agent measures software encoding at its start only
// where sessions encode on the CPU and capture the desktop (no GPU encoder,
// gdigrab), at the main monitor's size, in the background.
func TestCPUBench(t *testing.T) {
	defer func(f func(ctx context.Context, ff string, e media.EncoderInfo, w, h int) (media.SoftwareBench, error)) {
		benchSoftware = f
	}(benchSoftware)
	var calls []string
	benchSoftware = func(_ context.Context, _ string, e media.EncoderInfo, w, h int) (media.SoftwareBench, error) {
		calls = append(calls, e.Name)
		return media.SoftwareBench{Encoder: e.Name, W: w, H: h, Superfast: 50, Ultrafast: 70}, nil
	}
	caps := gdiTestCaps(noDevice, true)
	caps.FFmpeg = "ffmpeg"
	a := gdiAgent(Config{Capture: "auto"}, caps)
	a.startCPUBench()
	<-a.bench.done
	if b, ok := a.cpuBenchResult(); !ok || b.W != gdiMonitors[0].W || b.H != gdiMonitors[0].H || len(calls) != 1 || calls[0] != "libx264" {
		t.Fatalf("bench %+v %v, calls %q", b, ok, calls)
	}
	gpu := gdiTestCaps("", true)
	gpu.FFmpeg = "ffmpeg"
	gpu.Encoders = append([]media.EncoderInfo{{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true}}, gpu.Encoders...)
	for _, a := range []*Agent{gdiAgent(Config{Capture: "auto"}, gpu), gdiAgent(Config{Capture: "auto", CPUBudget: "off"}, caps),
		gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, false))} {
		a.startCPUBench()
		if a.bench.done != nil {
			t.Fatalf("measured with caps %+v, config %+v", a.caps.Encoders, a.cfg)
		}
	}
}
