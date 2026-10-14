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
// without a GPU): the agent measures libx264 at the monitor's size, and a
// session on a CPU measured slow streams gdigrab + libx264 at the rung it
// allows (the overlay's text in the video config, one informational
// notice), with capture stamps that are gdigrab's own grab times.
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
	a.startCPUBench()
	if a.bench.done == nil {
		t.Fatalf("no measurement (encoders %v)", caps.Encoders)
	}
	<-a.bench.done
	b, ok := a.cpuBenchResult()
	if !ok || b.Superfast <= 0 || b.Ultrafast <= 0 {
		t.Fatalf("measurement %+v %v; log:\n%s", b, ok, strings.Join(logs.lines(""), "\n"))
	}
	t.Logf("measured libx264 at %dx%d: superfast %.0f fps, ultrafast %.0f fps (%v)", b.W, b.H, b.Superfast, b.Ultrafast, time.Since(start).Round(time.Millisecond))
	// A CPU a quarter as fast as the one measured here: the lowest rungs.
	a.bench.res.Superfast, a.bench.res.Ultrafast = 20*float64(b.W*b.H)/(1280*720), 25*float64(b.W*b.H)/(1280*720)

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
	if p.Source.Backend != "gdigrab" || p.Quality != "speed" || p.FPS >= 60 {
		t.Fatalf("not limited: %+v", p)
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
		if strings.HasPrefix(n.Msg, "This PC encodes video on its CPU") && n.Level == "info" {
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
