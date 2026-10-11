//go:build windows

package host

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// TestSessionGDIWindows is the first hardware report end to end on Windows'
// own FFmpeg (RECON_TEST_FFMPEG: the FFmpeg 8.1 build the installer
// downloads) on a desktop without a Direct3D 11 video device (a GPU-less VM;
// Wine has none either), with the real monitor list and the real desktop:
//   - the probe finds no GPU capture, and a session captures with gdigrab
//     from its first generation and streams libx264 frames with capture
//     stamps, with one notice;
//   - a session of an agent whose probe did not know (an agent from before
//     this check) fails its first generation on ddagrab with FFmpeg's real
//     error, switches to gdigrab after that one failure and streams, with
//     one notice.
func TestSessionGDIWindows(t *testing.T) {
	ff := os.Getenv("RECON_TEST_FFMPEG")
	if ff == "" {
		t.Skip("RECON_TEST_FFMPEG not set (a Windows FFmpeg 8.1 on a desktop without GPU capture, e.g. under Wine)")
	}
	caps, err := media.Probe(context.Background(), ff, nil)
	if err != nil {
		t.Fatal(err)
	}
	if caps.GPUCaptureBlocked() == "" || !caps.CanGDIGrab() {
		blocked, detail := media.CheckGPUCapture(context.Background(), ff)
		t.Skipf("the probe found no finding (gdigrab %v); checked again: %q, %q", caps.CanGDIGrab(), blocked, detail)
	}
	if _, ok := caps.Best("h264"); !ok {
		t.Skip("no H.264 encoder")
	}
	for _, probeKnew := range []bool{true, false} {
		name := map[bool]string{true: "the probe found it", false: "found by the session"}[probeKnew]
		t.Run(name, func(t *testing.T) {
			c := *caps
			if !probeKnew {
				c.SetGPUCaptureBlocked("")
			}
			cfg := Config{Capture: "auto", DefaultFPS: 30, MaxFPS: 60, DefaultKbps: 4000, MaxKbps: 100000}
			cfg.Defaults()
			logs := &lockedLog{}
			a := &Agent{cfg: &cfg, caps: &c, inj: input.NewInjector(nil), hostClock: media.NewHostClock()}
			s, ctrl := gdiSession(a, logs)
			a.log = s.log
			s.video = media.NewVideo(&c, s.log, a.clock)
			defer func() { s.cancel(); s.video.Stop() }()
			go s.videoEvents()
			start := time.Now()
			if err := s.startVideo(false, ""); err != nil {
				t.Fatal(err)
			}
			wantGen := uint8(1)
			if !probeKnew {
				wantGen = 2
			}
			for n := 0; n < 30; n++ {
				select {
				case f := <-s.frameQ:
					if f.Gen != wantGen || f.Seq != uint32(n) || (n == 0) != f.Key || f.CaptureUs == 0 {
						t.Fatalf("frame %d: gen %d seq %d key %v capture %d", n, f.Gen, f.Seq, f.Key, f.CaptureUs)
					}
					if n == 0 {
						t.Logf("first frame %v after the start", time.Since(start).Round(time.Millisecond))
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("frame %d missing; log:\n%s", n, strings.Join(logs.lines(""), "\n"))
				}
			}
			t.Logf("30 frames %v after the start", time.Since(start).Round(time.Millisecond))
			got := ctrlNotices(t, ctrl)
			if len(got) != 1 || !strings.HasPrefix(got[0].Msg, "No GPU screen capture on this PC (no Direct3D 11 video device") {
				t.Fatalf("notices %+v", got)
			}
			starts := logs.lines(`msg="starting encoder"`)
			if len(starts) != int(wantGen) || !strings.Contains(starts[len(starts)-1], "capture=gdigrab") {
				t.Fatalf("encoder starts %q", starts)
			}
			if !probeKnew {
				if l := logs.lines(`msg="GPU screen capture failed: capturing with GDI (gdigrab)"`); len(l) != 1 ||
					!strings.Contains(l[0], `ffmpeg="Failed to create D3D11VA device."`) {
					t.Fatalf("switch log %q", l)
				}
				if a.gpuCaptureBlocked() == "" {
					t.Fatal("the agent did not keep the finding")
				}
			}
			for _, l := range logs.lines("") {
				if strings.Contains(l, "level=WARN") || strings.Contains(l, "level=ERROR") {
					t.Log(l)
				}
			}
		})
	}
}
