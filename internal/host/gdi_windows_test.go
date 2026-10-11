//go:build windows

package host

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
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
	<-caps.GPUCaptureDone() // the probe does not wait for it
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
			defer stopGDISession(s)
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

// setResolution changes the primary display's resolution
// (ChangeDisplaySettingsW, not stored), or restores the stored one (w 0).
func setResolution(t *testing.T, w, h int) {
	t.Helper()
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("ChangeDisplaySettingsW")
	if w == 0 {
		proc.Call(0, 0)
		return
	}
	var dm [220]byte                                 // DEVMODEW
	*(*uint16)(unsafe.Pointer(&dm[68])) = 220        // dmSize
	*(*uint32)(unsafe.Pointer(&dm[72])) = 0x00180000 // dmFields: DM_PELSWIDTH | DM_PELSHEIGHT
	*(*uint32)(unsafe.Pointer(&dm[172])) = uint32(w) // dmPelsWidth
	*(*uint32)(unsafe.Pointer(&dm[176])) = uint32(h) // dmPelsHeight
	if r, _, err := proc.Call(uintptr(unsafe.Pointer(&dm[0])), 0); r != 0 {
		t.Fatalf("ChangeDisplaySettingsW %dx%d: %d (%v)", w, h, int32(r), err)
	}
}

// TestGDIFollowsDisplayWindows changes the display's resolution while a
// session captures it with Windows' own FFmpeg's gdigrab (RECON_TEST_FFMPEG,
// RECON_TEST_DISPLAY_CHANGE set: it changes this desktop's resolution, e.g.
// a Wine virtual desktop's): the session restarts on the new size, the new
// generation streams, and input maps to the new rectangle.
func TestGDIFollowsDisplayWindows(t *testing.T) {
	ff := os.Getenv("RECON_TEST_FFMPEG")
	if ff == "" || os.Getenv("RECON_TEST_DISPLAY_CHANGE") == "" {
		t.Skip("RECON_TEST_FFMPEG and RECON_TEST_DISPLAY_CHANGE not set (it changes the display's resolution)")
	}
	platform.EnableDPIAwareness()
	caps, err := media.Probe(context.Background(), ff, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-caps.GPUCaptureDone()
	if !caps.CanGDIGrab() {
		t.Skip("no gdigrab")
	}
	defer func(d time.Duration) { gdiDisplayEvery = d }(gdiDisplayEvery)
	gdiDisplayEvery = 200 * time.Millisecond
	mons, err := platform.Monitors()
	if err != nil || len(mons) == 0 {
		t.Fatalf("monitors %v %v", mons, err)
	}
	w0, h0 := mons[0].W, mons[0].H
	w1, h1 := 1024, 768
	if w0 == w1 {
		w1, h1 = 800, 600
	}
	cfg := Config{Capture: "gdigrab", DefaultFPS: 30, MaxFPS: 60, DefaultKbps: 4000, MaxKbps: 100000}
	cfg.Defaults()
	logs := &lockedLog{}
	rec := &recordingBackend{}
	a := &Agent{cfg: &cfg, caps: caps, inj: input.NewInjector(rec), hostClock: media.NewHostClock()}
	s, _ := gdiSession(a, logs)
	a.log = s.log
	s.video = media.NewVideo(caps, s.log, a.clock)
	defer stopGDISession(s)
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	frames := func(gen uint8, n int) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for got := 0; got < n; {
			select {
			case f := <-s.frameQ:
				if f.Gen == gen {
					got++
				}
			case <-deadline:
				t.Fatalf("generation %d: %d frames; log:\n%s", gen, got, strings.Join(logs.lines(""), "\n"))
			}
		}
	}
	frames(1, 10)
	defer setResolution(t, 0, 0)
	setResolution(t, w1, h1)
	start := time.Now()
	frames(2, 10)
	t.Logf("generation 2 streamed 10 frames %v after the change", time.Since(start).Round(time.Millisecond))
	p, _ := s.vid().Current()
	if p.Source.Backend != "gdigrab" || p.Source.NativeW != w1 || p.Source.NativeH != h1 {
		t.Fatalf("after the change to %dx%d (from %dx%d): %+v", w1, h1, w0, h0, p.Source)
	}
	_ = a.inj.MoveAbs(0, 0)
	if rec.target.W != w1 || rec.target.H != h1 {
		t.Fatalf("pointer input maps to %+v", rec.target)
	}
	for _, l := range logs.lines(`msg="display changed"`) {
		t.Log(l)
	}
	for _, l := range logs.lines(`msg="encoder ready"`) {
		t.Log(l)
	}
}
