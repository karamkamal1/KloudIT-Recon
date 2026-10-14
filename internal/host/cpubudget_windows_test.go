//go:build windows

package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestCPUBudgetGDIWindows runs the CPU budget with Windows' own FFmpeg
// (RECON_TEST_FFMPEG) on a desktop without GPU capture (Wine, as a VM
// without a GPU): the agent measures libx264 and GDI's grab at the
// monitor's size, and a session on a CPU set to encode a quarter of 1280x720
// at 30 fps (the lowest rung: 960x540 at 20 fps, scaled by gdigrab's CPU
// scaling) streams gdigrab + libx264 at it (the overlay's text in the video
// config, one informational notice), with capture stamps that are
// gdigrab's own grab times.
func TestCPUBudgetGDIWindows(t *testing.T) {
	ff := os.Getenv("RECON_TEST_FFMPEG")
	if ff == "" {
		t.Skip("RECON_TEST_FFMPEG not set (a Windows FFmpeg 8.1 on a desktop without GPU capture, e.g. under Wine)")
	}
	caps, err := media.Probe(context.Background(), ff, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-caps.GPUCaptureDone()
	if caps.GPUCaptureBlocked() == "" || !caps.CanGDIGrab() {
		t.Skipf("GPU capture works here (gdigrab %v)", caps.CanGDIGrab())
	}
	cfg := Config{Capture: "auto"}
	cfg.Defaults()
	logs := &lockedLog{}
	a := &Agent{cfg: &cfg, caps: caps, inj: input.NewInjector(nil), hostClock: media.NewHostClock(),
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	start := time.Now()
	a.startCPUBench(context.Background())
	if a.bench.done == nil {
		t.Fatalf("no measurement (encoders %v)", caps.Encoders)
	}
	<-a.bench.done
	b, ok := a.cpuBenchResult()
	if !ok || b.Superfast <= 0 || b.Ultrafast <= 0 {
		t.Fatalf("measurement %+v %v; log:\n%s", b, ok, strings.Join(logs.lines(""), "\n"))
	}
	t.Logf("measured libx264 at %dx%d: superfast %.0f fps, ultrafast %.0f fps; gdigrab %.0f fps %s(%v)", b.W, b.H, b.Superfast, b.Ultrafast,
		b.Grab, b.GrabErr, time.Since(start).Round(time.Millisecond))
	if b.Grab <= 0 {
		t.Fatalf("gdigrab not measured: %s", b.GrabErr)
	}
	// A CPU that encodes 1280x720 at 20 fps (superfast) and 25 (ultrafast),
	// in frames of the measured size: the lowest rung, 960x540 at 20 fps.
	a.bench.res.Superfast, a.bench.res.Ultrafast = 20*float64(1280*720)/float64(b.W*b.H), 25*float64(1280*720)/float64(b.W*b.H)

	s, ctrl := gdiSession(a, logs)
	s.video = media.NewVideo(caps, s.log, a.clock)
	defer stopGDISession(s)
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	p, _ := s.vid().Current()
	w, h := p.OutputSize()
	t.Logf("generation 1: %s %dx%d at %d fps, quality %s, monitor %dx%d", p.Source.Backend, w, h, p.FPS, p.Quality, p.Source.NativeW, p.Source.NativeH)
	if p.Source.Backend != "gdigrab" || p.Quality != "speed" || p.FPS != 20 || h != min(540, p.Source.NativeH) || s.cpu.rung != len(cpuRungs)-1 {
		t.Fatalf("not at the lowest rung (960x540 at 20 fps on a 1920x1080 monitor): %dx%d, rung %d, %+v", w, h, s.cpu.rung, p)
	}
	for n := 0; n < 20; n++ {
		select {
		case f := <-s.frameQ:
			lag := int64(a.clock()) - int64(f.CaptureUs)
			if f.CaptureUs == 0 || lag < 0 || lag > 5_000_000 {
				t.Fatalf("frame %d: capture stamp %d, %d µs before now", n, f.CaptureUs, lag)
			}
			if n == 19 {
				t.Logf("frame 19: grab->delivered %.1f ms", float64(lag)/1000)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("frame %d missing; log:\n%s", n, strings.Join(logs.lines(""), "\n"))
		}
	}
	var limit string
	for _, b := range ctrlMessages(t, ctrl) {
		var c proto.VideoConfig
		if json.Unmarshal(b, &c) == nil && c.T == "video" {
			limit = c.CPULimit
		}
	}
	if !strings.Contains(limit, "fastest preset (asked: ") {
		t.Fatalf("video config's cpuLimit %q", limit)
	}
	t.Logf("overlay: %s", limit)
	var cpuNotices int
	for _, n := range ctrlNotices(t, ctrl) {
		if strings.HasPrefix(n.Msg, "This stream encodes video on the CPU (libx264") && n.Level == "info" {
			cpuNotices++
		}
	}
	if cpuNotices != 1 {
		t.Fatalf("notices %+v", ctrlNotices(t, ctrl))
	}
	if l := logs.lines(`msg="cpu budget: software encoding"`); len(l) != 1 {
		t.Fatalf("budget log %q", l)
	}
}
