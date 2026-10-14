package host

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
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

// TestBenchRung: the first rung from the agent's measurements fits cpuShare
// of what the CPU encoded at the rung's preset and, for GDI capture,
// cpuGrabShare of what GDI grabbed (a session grabs the whole monitor
// whatever its size): a 4-vCPU VM streams the settings; a 2-vCPU one (the
// 57 and 76 fps measured on two Xeon vCPUs, with the 40 fps GDI grabbed a
// 1920x1080 Wine desktop on two CPUs) 1920x1080 at 30 fps with the fastest
// preset, where its encoder alone would allow 45 (in the A/B on two CPUs
// 45 fps delivered 28-29 fps with grab->encoded p50 74-76 ms: behind; 30
// fps 27-28 fps, p50 64-68 ms); without a grab measurement at most 30 fps;
// a slow one 1600x900 at 30.
func TestBenchRung(t *testing.T) {
	for _, c := range []struct {
		name             string
		super, ult, grab float64
		rung             int
		streams          string
	}{
		{"4 vCPUs", 110, 150, 120, 0, "1920×1080 at 60 fps"},
		{"2 vCPUs", 57, 76, 40, 3, "1920×1080 at 30 fps"},
		{"2 vCPUs, encoder alone", 57, 76, 70, 2, "1920×1080 at 45 fps"},
		{"2 vCPUs, grab not measured", 57, 76, 0, 3, "1920×1080 at 30 fps"},
		{"4 vCPUs, grab not measured", 110, 150, 0, 3, "1920×1080 at 30 fps"},
		{"slow", 25, 35, 45, 4, "1600×900 at 30 fps"},
		{"slow grab", 110, 150, 30, len(cpuRungs) - 1, "960×540 at 20 fps"},
		{"very slow", 4, 5, 40, len(cpuRungs) - 1, "960×540 at 20 fps"},
	} {
		b := media.SoftwareBench{Encoder: "libx264", W: 1920, H: 1080, Superfast: c.super, Ultrafast: c.ult, Grab: c.grab}
		r := benchRung(b, gdiAsked())
		if got := describeParams(rungParams(gdiAsked(), r)); r != c.rung || got != c.streams {
			t.Errorf("%s: rung %d (%s), want %d (%s)", c.name, r, got, c.rung, c.streams)
		}
	}
	// The grab rate is scaled to the monitor the session captures: a
	// 2560x1440 one costs GDI 1.8 times the measured 1920x1080.
	big := gdiAsked()
	big.Source.NativeW, big.Source.NativeH = 2560, 1440
	if f := grabFPS(media.SoftwareBench{W: 1920, H: 1080, Grab: 100}, big); f < 44.9 || f > 45.1 {
		t.Errorf("grab rate at 2560x1440 from 100 fps at 1920x1080: %.1f, want 45", f)
	}
	// Desktop Duplication and the test pattern are not limited by GDI.
	dda := gdiAsked()
	dda.Source.Backend = "ddagrab"
	if r := benchRung(media.SoftwareBench{W: 1920, H: 1080, Superfast: 110, Ultrafast: 150}, dda); r != 0 {
		t.Errorf("ddagrab without a grab measurement: rung %d, want 0", r)
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
	a.bench.res, a.bench.ok = media.SoftwareBench{Encoder: "libx264", W: 1920, H: 1080, Superfast: 57, Ultrafast: 76, Grab: 70}, true
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
	want := []string{"info: This stream encodes video on the CPU (libx264: no GPU encoder for it), which keeps up with 1920×1080 at 45 fps: " +
		"streaming at that instead of 1920×1080 at 60 fps. The overlay shows the limit; it rises again when the CPU has room."}
	if got := noticeLines(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices %q", got)
	}
	cfg := proto.VideoConfig{Gen: 1, FPS: 45}
	s.cpuConfig(&cfg)
	if cfg.CPULimit != "1920×1080 at 45 fps, fastest preset (asked: 1920×1080 at 60 fps): what the CPU keeps up with" {
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
	if why := look(30, 150*time.Millisecond, p); why != cpuWhyDown || s.cpu.rung != 3 {
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
	if why := look(30, 20*time.Millisecond, p); why != cpuWhyUp || s.cpu.rung != 2 {
		t.Fatalf("at ease past the wait: %q, rung %d", why, s.cpu.rung)
	}
	// The rung above falls behind at once: back down, and the next try
	// waits twice as long.
	p = asked
	s.cpuBudget(&p)
	cfg = proto.VideoConfig{Gen: 3, FPS: 45}
	s.cpuConfig(&cfg)
	look(35, 120*time.Millisecond, p)
	if why := look(35, 120*time.Millisecond, p); why != cpuWhyDown || s.cpu.rung != 3 || s.cpu.upWait != 2*cpuUpWait {
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

// TestCPUBench: the agent measures software encoding and GDI's grab at its
// start only where sessions encode on the CPU and capture the desktop (no
// GPU encoder, gdigrab), at the main monitor's size, in the background. It
// measures again while no session streams, up to cpuBenchRuns in all, and
// the best of each rate counts (a measurement at logon, the CPU busy, was a
// fifth of one minutes later here); one that a session's start interrupts
// is given up and does not count.
func TestCPUBench(t *testing.T) {
	defer func(f func(ctx context.Context, ff string, e media.EncoderInfo, w, h int) (media.SoftwareBench, error),
		g func(ctx context.Context, ff string, x, y, w, h int) (float64, error), again time.Duration) {
		benchSoftware, benchGrab, cpuBenchAgain = f, g, again
	}(benchSoftware, benchGrab, cpuBenchAgain)
	cpuBenchAgain = 20 * time.Millisecond
	var mu sync.Mutex
	var calls []string
	ncalls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	busy := []float64{12, 50, 40} // the CPU's rate at each measurement: busy at logon
	grabs := []float64{20, 0, 45}
	block := make(chan struct{}) // the third measurement waits here until a session starts
	benchSoftware = func(ctx context.Context, _ string, e media.EncoderInfo, w, h int) (media.SoftwareBench, error) {
		mu.Lock()
		n := len(calls)
		calls = append(calls, e.Name)
		mu.Unlock()
		if n == 2 {
			select {
			case <-block:
			case <-ctx.Done():
				return media.SoftwareBench{}, ctx.Err()
			}
		}
		return media.SoftwareBench{Encoder: e.Name, W: w, H: h, Superfast: busy[min(n, 2)], Ultrafast: busy[min(n, 2)] * 1.4}, nil
	}
	var gmu sync.Mutex
	gn := 0
	benchGrab = func(_ context.Context, _ string, x, y, w, h int) (float64, error) {
		gmu.Lock()
		defer gmu.Unlock()
		g := grabs[min(gn, 2)]
		gn++
		if g == 0 {
			return 0, errors.New("gdigrab: Failed to capture image (error 5)")
		}
		return g, nil
	}
	caps := gdiTestCaps(noDevice, true)
	caps.FFmpeg = "ffmpeg"
	logs := &lockedLog{}
	a := gdiAgent(Config{Capture: "auto"}, caps)
	a.log = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startCPUBench(ctx)
	<-a.bench.done
	if b, ok := a.cpuBenchResult(); !ok || b.W != gdiMonitors[0].W || b.H != gdiMonitors[0].H || b.Superfast != 12 || b.Grab != 20 {
		t.Fatalf("first measurement %+v %v", b, ok)
	}
	// The second, a minute later here: better encoding, the desktop locked
	// (no grab): the best of each.
	waitUntil(t, logs, "no second measurement", func() bool { return len(logs.lines(`msg="software encoding measured again`)) == 1 })
	if b, _ := a.cpuBenchResult(); b.Superfast != 50 || b.Ultrafast != 70 || b.Grab != 20 || b.GrabErr != "" {
		t.Fatalf("after the second measurement %+v, want the best of each", b)
	}
	// The third: a session starts while it runs: given up, not counted, and
	// the one after it (the session gone) completes it.
	waitUntil(t, logs, "no third measurement", func() bool { return ncalls() == 3 })
	s := &Session{}
	a.mu.Lock()
	a.active = s
	a.mu.Unlock()
	waitUntil(t, logs, "the third measurement not given up", func() bool {
		return len(logs.lines(`msg="software encoding measurement given up: a session started"`)) == 1
	})
	close(block)
	time.Sleep(10 * cpuBenchAgain)
	if n := ncalls(); n != 3 {
		t.Fatalf("%d measurements while a session streams", n)
	}
	a.mu.Lock()
	a.active = nil
	a.mu.Unlock()
	waitUntil(t, logs, "no third measurement after the session", func() bool { return len(logs.lines(`msg="software encoding measured again`)) == 2 })
	if b, _ := a.cpuBenchResult(); b.Superfast != 50 || b.Grab != 45 {
		t.Fatalf("after the third measurement %+v", b)
	}
	time.Sleep(10 * cpuBenchAgain)
	if n := ncalls(); n != cpuBenchRuns+1 {
		t.Fatalf("%d measurements, want %d (and one given up)", n, cpuBenchRuns+1)
	}

	gpu := gdiTestCaps("", true)
	gpu.FFmpeg = "ffmpeg"
	gpu.Encoders = append([]media.EncoderInfo{{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true}}, gpu.Encoders...)
	for _, a := range []*Agent{gdiAgent(Config{Capture: "auto"}, gpu), gdiAgent(Config{Capture: "auto", CPUBudget: "off"}, caps),
		gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, false))} {
		a.startCPUBench(ctx)
		if a.bench.done != nil {
			t.Fatalf("measured with caps %+v, config %+v", a.caps.Encoders, a.cfg)
		}
	}
}

// TestCPUBudgetDesktopDuplication: a session that encodes Desktop
// Duplication on the CPU (a GPU PC whose GPU has no encoder for the codec:
// AV1 on an RTX 30 or RX 6000) gets frames only when the screen changes. A
// 30 fps video in a 60 fps stream, 8 ms from capture to encoded, is not a
// CPU that falls behind (it used to step down three rungs in 12 s, to 30
// fps at the fastest preset); frames that wait is.
func TestCPUBudgetDesktopDuplication(t *testing.T) {
	a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
	s, _ := gdiSession(a, nil)
	s.cancel()
	s.monitor = platform.Monitor{W: 1920, H: 1080, Hz: 60, Primary: true, DXGIOutput: 0}
	asked := media.Params{Source: media.Source{Backend: "ddagrab", NativeW: 1920, NativeH: 1080}, FPS: 60, BitrateKbps: 30000,
		Quality: "balanced", Encoder: media.EncoderInfo{Name: "libsvtav1", Family: "av1", Vendor: "software"}}
	p := asked
	s.cpuBudget(&p)
	if !s.cpu.applies || s.cpu.rung != 0 || describeParams(p) != "1920×1080 at 60 fps" {
		t.Fatalf("first generation: covered %v, rung %d, %s", s.cpu.applies, s.cpu.rung, describeParams(p))
	}
	cfg := proto.VideoConfig{Gen: 1, FPS: 60}
	s.cpuConfig(&cfg)
	now := time.Now()
	look := func(fps float64, lag time.Duration) string {
		for i := 0; i < int(fps*cpuWindow.Seconds()); i++ {
			now = now.Add(time.Duration(float64(time.Second) / fps))
			done := uint64(now.UnixMicro())
			s.cpuFrame(&media.Frame{Gen: s.cpu.gen, CaptureUs: done - uint64(lag.Microseconds()), EncodeDoneUs: done}, now)
		}
		return s.cpuLook(now, p, true)
	}
	for i := 0; i < 6; i++ {
		if why := look(30, 8*time.Millisecond); why != "" || s.cpu.rung != 0 {
			t.Fatalf("look %d at 30 of 60 fps, 8 ms: %q, rung %d", i, why, s.cpu.rung)
		}
	}
	look(60, 90*time.Millisecond)
	if why := look(60, 90*time.Millisecond); why != cpuWhyDown || s.cpu.rung != 1 {
		t.Fatalf("frames 90 ms from capture to encoded: %q, rung %d", why, s.cpu.rung)
	}
}

// TestCPURestart: a step down stops the generation that fell behind first
// (urgent), a step up starts the next one beside it (overlapped).
func TestCPURestart(t *testing.T) {
	s, p, _ := fakePipelineSession(t, slog.New(slog.NewTextHandler(io.Discard, nil)), media.PipelineCaps{})
	s.cpuRestart(cpuWhyDown)
	s.cpuRestart(cpuWhyUp)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.starts) != 2 || !p.starts[0] || p.starts[1] {
		t.Fatalf("urgent starts %v, want [true false]", p.starts)
	}
}
