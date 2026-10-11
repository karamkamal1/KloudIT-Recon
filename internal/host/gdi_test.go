package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The first hardware report: a Proxmox VM without a GPU, FFmpeg's ddagrab
// failing every start with "Failed to create D3D11VA device." (gdi.go).

const noDevice = "no Direct3D 11 video device: a PC or VM without a GPU"

// wineStderr is FFmpeg 8.1's real stderr for the user's ddagrab + libx264
// generation, recorded under Wine (no Direct3D 11 video device either).
func wineStderr(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("media", "testdata", "ffmpeg81-stderr-ddagrab-libx264.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Split(strings.TrimSpace(string(b)), "\n"), " | ") // as the stderr ring holds it
}

// gdiTestCaps: an FFmpeg 8.1 Windows build's capture sources and the
// software encoders; blocked: the probe found no GPU capture.
func gdiTestCaps(blocked string, gdigrab bool) *media.Caps {
	c := &media.Caps{
		Filters: map[string]bool{"ddagrab": true, "gfxcapture": true, "settb": true, "setpts": true},
		Devices: map[string]bool{"gdigrab": gdigrab},
		Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"},
			{Name: "libsvtav1", Family: "av1", Vendor: "software"}},
	}
	c.SetGPUCaptureBlocked(blocked)
	return c
}

// ctrlNotices returns the notices the session sent on its control stream.
func ctrlNotices(t *testing.T, c *fakeCtrl) []proto.Notice {
	t.Helper()
	c.mu.Lock()
	r := bytes.NewReader(bytes.Clone(c.buf.Bytes()))
	c.mu.Unlock()
	var out []proto.Notice
	for {
		b, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		var n proto.Notice
		if json.Unmarshal(b, &n) == nil && n.T == "notice" {
			out = append(out, n)
		}
	}
}

// gdiSession is a session of agent a for a browser that decodes H.264,
// whose control stream ctrl records what it sends. Its context is live:
// cancel it where nothing may start a generation (the failure handler's
// delayed restarts).
func gdiSession(a *Agent, logs io.Writer) (*Session, *fakeCtrl) {
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := &fakeCtrl{}
	if logs == nil {
		logs = io.Discard
	}
	s := &Session{
		a: a, ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 6),
		hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return s, ctrl
}

// gdiMonitors: the VM's monitor, and a second one left of it at a negative
// x, both with odd sizes (gdigrab captures an even area).
var gdiMonitors = []platform.Monitor{
	{Index: 0, Name: `\\.\DISPLAY1`, X: 0, Y: 0, W: 1281, H: 721, Primary: true, Hz: 60, HMonitor: 0x10001, DXGIOutput: 0},
	{Index: 1, Name: `\\.\DISPLAY2`, X: -1920, Y: 120, W: 1920, H: 1080, Hz: 60, HMonitor: 0x20001, DXGIOutput: 1},
}

func gdiAgent(cfg Config, caps *media.Caps) *Agent {
	cfg.Defaults()
	return &Agent{cfg: &cfg, caps: caps, inj: input.NewInjector(nil), hostClock: media.NewHostClock(),
		listMonitors: func() []platform.Monitor { return gdiMonitors }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestCaptureChoiceGDI: the capture source by FFmpeg's capture sources and
// the probe's finding (caps), the host config and the client's preferences
// (native, scaled, a window), on a monitor and a virtual display, and after
// the session's own Desktop Duplication refusals. GPU capture gives way to
// GDI wherever it cannot work and the build has gdigrab, configured GPU
// captures too; it stays where the PC has GPU capture or nothing better.
func TestCaptureChoiceGDI(t *testing.T) {
	native, scaled, window := proto.Prefs{}, proto.Prefs{Width: 1280, Height: 720}, proto.Prefs{Window: "Notepad"}
	gpu := gdiTestCaps("", true)
	noGPU := gdiTestCaps(noDevice, true)
	noGPUNoGDI := gdiTestCaps(noDevice, false)
	gdiOnly := gdiTestCaps("", true)
	gdiOnly.Filters = map[string]bool{}
	for _, c := range []struct {
		name    string
		caps    *media.Caps
		capture string
		want    [3]string // native, scaled, window
	}{
		{"GPU, auto", gpu, "auto", [3]string{"ddagrab", "gfxcapture", "gfxcapture"}},
		{"GPU, ddagrab", gpu, "ddagrab", [3]string{"ddagrab", "ddagrab", "ddagrab"}},
		{"GPU, gfxcapture", gpu, "gfxcapture", [3]string{"gfxcapture", "gfxcapture", "gfxcapture"}},
		{"GPU, amf", gpu, "amf", [3]string{"amf", "amf", "amf"}},
		{"GPU, gdigrab", gpu, "gdigrab", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
		{"no GPU, auto", noGPU, "auto", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
		{"no GPU, ddagrab", noGPU, "ddagrab", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
		{"no GPU, gfxcapture", noGPU, "gfxcapture", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
		{"no GPU, amf", noGPU, "amf", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
		{"no GPU, test", noGPU, "test", [3]string{"test", "test", "test"}},
		{"no GPU, no gdigrab: nothing better", noGPUNoGDI, "auto", [3]string{"ddagrab", "gfxcapture", "gfxcapture"}},
		{"a build without GPU capture", gdiOnly, "auto", [3]string{"gdigrab", "gdigrab", "gdigrab"}},
	} {
		a := gdiAgent(Config{Capture: c.capture}, c.caps)
		s, _ := gdiSession(a, nil)
		for i, prefs := range []proto.Prefs{native, scaled, window} {
			if got := a.backendFor(prefs); got != c.want[i] {
				t.Errorf("%s, prefs %+v: backendFor %s, want %s", c.name, prefs, got, c.want[i])
			}
			if got := s.captureBackend(prefs, gdiMonitors[0], false); got != c.want[i] {
				t.Errorf("%s, prefs %+v: captureBackend %s, want %s", c.name, prefs, got, c.want[i])
			}
		}
		// A virtual display (not on DXGI adapter 0) is captured whole: by
		// gfxcapture on the GPU, by GDI without.
		virt := gdiMonitors[0]
		virt.DXGIOutput = -1
		want := "gfxcapture"
		if c.want[0] == "gdigrab" {
			want = "gdigrab"
		}
		if got := s.captureBackend(native, virt, true); c.capture != "test" && got != want {
			t.Errorf("%s: virtual display captured with %s, want %s", c.name, got, want)
		}
	}

	// The session's own refusals (Desktop Duplication twice: a remote
	// desktop session) move only that session's GPU captures to GDI.
	a := gdiAgent(Config{Capture: "auto"}, gpu)
	s, _ := gdiSession(a, nil)
	s.gdi = "Desktop Duplication cannot capture this display: a remote desktop session, or a display mode it does not support"
	for _, prefs := range []proto.Prefs{native, scaled, window} {
		if got := s.captureBackend(prefs, gdiMonitors[0], false); got != "gdigrab" {
			t.Errorf("session refused, prefs %+v: %s", prefs, got)
		}
	}
	if got := s.captureBackend(native, gdiMonitors[0], true); got != "gdigrab" {
		t.Errorf("session refused, virtual display: %s", got)
	}
	if s2, _ := gdiSession(a, nil); s2.captureBackend(native, gdiMonitors[0], false) != "ddagrab" {
		t.Error("another session of the agent captures with GDI after one session's refusals")
	}
}

// recordingBackend keeps the target of absolute pointer moves.
type recordingBackend struct {
	input.Backend
	target input.Rect
}

func (b *recordingBackend) MoveAbs(x, y uint16, t input.Rect) error { b.target = t; return nil }

// TestGDIParams: a GDI generation captures exactly the session's monitor
// (its virtual-desktop rectangle, an even size), at the stream's frame rate
// (at most the monitor's refresh), with the cursor as the client wants it,
// scaled on the CPU to the client's size and never up; absolute pointer
// input maps to that monitor. The command line (media.BuildArgs) carries it.
func TestGDIParams(t *testing.T) {
	defer func(v bool) { cursorOnPlatform = v }(cursorOnPlatform)
	cursorOnPlatform = true // the host draws the cursor unless the client wants it in the video
	a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, true))
	rec := &recordingBackend{}
	a.inj = input.NewInjector(rec)
	for _, c := range []struct {
		prefs      proto.Prefs
		mon        int
		w, h, fps  int
		drawCursor bool
	}{
		{proto.Prefs{Monitor: 0, FPS: 120}, 0, 0, 0, 60, false},
		{proto.Prefs{Monitor: 1, FPS: 30, Width: 1280, Height: 720, Cursor: "video"}, 1, 1280, 720, 30, true},
		{proto.Prefs{Monitor: 1, Width: 3840, Height: 2160}, 1, 0, 0, 60, false}, // never upscaled
	} {
		s, _ := gdiSession(a, nil)
		s.cancel()
		s.prefs = c.prefs
		p, err := s.buildParams(c.prefs)
		if err != nil {
			t.Fatal(err)
		}
		mon := gdiMonitors[c.mon]
		want := media.Source{Backend: "gdigrab", X: mon.X, Y: mon.Y, NativeW: mon.W &^ 1, NativeH: mon.H &^ 1}
		if p.Source != want || p.Width != c.w || p.Height != c.h || p.FPS != c.fps || p.DrawCursor != c.drawCursor ||
			p.Encoder.Name != "libx264" {
			t.Errorf("prefs %+v: %+v, want source %+v, %dx%d at %d fps, cursor %v", c.prefs, p, want, c.w, c.h, c.fps, c.drawCursor)
		}
		_ = a.inj.MoveAbs(0, 0)
		if rec.target != (input.Rect{X: mon.X, Y: mon.Y, W: mon.W, H: mon.H}) {
			t.Errorf("prefs %+v: pointer input maps to %+v, want monitor %d", c.prefs, rec.target, c.mon)
		}
	}
}

// TestCaptureFailedGDI drives the failure handler with the video manager's
// failure events of a session whose PC has GPU capture as far as the probe
// knew. No Direct3D 11 video device (the user's case) moves the session and
// the agent to GDI after the first failure, with one notice; Desktop
// Duplication refusing the display twice in a row moves the session only;
// the secure desktop and a lost display keep Desktop Duplication, and their
// repeated notices are not repeated.
func TestCaptureFailedGDI(t *testing.T) {
	fail := func(s *Session, p media.Params, stderr string, live bool) {
		s.handleEncoderFailure(media.VideoEvent{Err: fmt.Errorf("encoder %s exited: %s", p.Encoder.Name, stderr), Failed: &p,
			Live: live, CaptureFailure: media.ClassifyCapture(stderr)})
	}
	next := func(t *testing.T, s *Session) media.Params {
		t.Helper()
		p, err := s.buildParams(s.prefs)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	msgs := func(t *testing.T, c *fakeCtrl) []string {
		var out []string
		for _, n := range ctrlNotices(t, c) {
			out = append(out, n.Level+": "+n.Msg)
		}
		return out
	}
	const refused = "[Parsed_ddagrab_0 @ 0000020a] Selected output not supported | [Parsed_ddagrab_0 @ 0000020a] Failed to configure output pad on Parsed_ddagrab_0"
	const denied = "[Parsed_ddagrab_0 @ 0000020a] Desktop duplication access denied | [Parsed_ddagrab_0 @ 0000020a] Failed to configure output pad on Parsed_ddagrab_0"
	const lost = "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0026"

	t.Run("no Direct3D 11 video device", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
		logs := &lockedLog{}
		s, ctrl := gdiSession(a, logs)
		s.cancel()
		p := next(t, s)
		if p.Source.Backend != "ddagrab" {
			t.Fatalf("first generation captures with %s", p.Source.Backend)
		}
		fail(s, p, wineStderr(t), false)
		if s.failures != 0 {
			t.Fatalf("the switch to GDI counted as a failure (%d)", s.failures)
		}
		p = next(t, s)
		if p.Source.Backend != "gdigrab" || a.gpuCaptureBlocked() != noDevice {
			t.Fatalf("after the failure: %s, agent finding %q", p.Source.Backend, a.gpuCaptureBlocked())
		}
		next(t, s) // a later restart: no second notice
		want := []string{"warn: No GPU screen capture on this PC (" + noDevice + "): capturing with GDI, which uses more CPU and adds latency."}
		if got := msgs(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("notices %q, want %q", got, want)
		}
		if l := logs.lines(`msg="GPU screen capture failed: capturing with GDI (gdigrab)"`); len(l) != 1 ||
			!strings.Contains(l[0], `ffmpeg="Failed to create D3D11VA device."`) || !strings.Contains(l[0], `for="this PC"`) {
			t.Fatalf("switch log %q", l)
		}
		if l := logs.lines(`msg="encoder failed"`); len(l) != 1 || !strings.Contains(l[0], "Failed to configure output pad on Parsed_ddagrab_0") {
			t.Fatalf("raw FFmpeg error not in host.log: %q", l)
		}
		// The next session starts with GDI, and says so once.
		s2, ctrl2 := gdiSession(a, nil)
		s2.cancel()
		if p := next(t, s2); p.Source.Backend != "gdigrab" {
			t.Fatalf("next session captures with %s", p.Source.Backend)
		}
		next(t, s2)
		if got := msgs(t, ctrl2); len(got) != 1 || !strings.HasPrefix(got[0], "warn: No GPU screen capture on this PC") {
			t.Fatalf("next session's notices %q", got)
		}
	})

	t.Run("gfxcapture without a device", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "gfxcapture"}, gdiTestCaps("", true))
		s, _ := gdiSession(a, nil)
		s.cancel()
		p := next(t, s)
		fail(s, p, "[Parsed_gfxcapture_0 @ 000001c4] Failed to create D3D11VA device. | [Parsed_gfxcapture_0 @ 000001c4] Failed to configure output pad on Parsed_gfxcapture_0", false)
		if p := next(t, s); p.Source.Backend != "gdigrab" {
			t.Fatalf("after gfxcapture's failure: %s", p.Source.Backend)
		}
	})

	t.Run("Desktop Duplication refuses the display", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
		s, ctrl := gdiSession(a, nil)
		s.cancel()
		p := next(t, s)
		fail(s, p, refused, false) // a mode change can cause one: retried
		if p = next(t, s); p.Source.Backend != "ddagrab" || s.failures != 1 {
			t.Fatalf("after one refusal: %s, %d failures", p.Source.Backend, s.failures)
		}
		fail(s, p, refused, false)
		if p = next(t, s); p.Source.Backend != "gdigrab" || s.failures != 1 {
			t.Fatalf("after two refusals: %s, %d failures", p.Source.Backend, s.failures)
		}
		if a.gpuCaptureBlocked() != "" {
			t.Fatal("one session's refusals taken for the PC's")
		}
		got := msgs(t, ctrl)
		if len(got) != 2 || got[0] != "warn: Screen capture failed, retrying: Desktop Duplication cannot capture this display: a remote desktop session, or a display mode it does not support." ||
			!strings.HasPrefix(got[1], "warn: Screen capture on the GPU failed (Desktop Duplication cannot capture this display") ||
			!strings.HasSuffix(got[1], "capturing with GDI for this session, which uses more CPU and adds latency.") {
			t.Fatalf("notices %q", got)
		}
	})

	t.Run("refusals not in a row", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
		s, _ := gdiSession(a, nil)
		s.cancel()
		p := next(t, s)
		fail(s, p, refused, false)
		fail(s, next(t, s), denied, false) // the secure desktop in between
		fail(s, next(t, s), refused, false)
		if p = next(t, s); p.Source.Backend != "ddagrab" {
			t.Fatalf("refused, denied, refused: %s", p.Source.Backend)
		}
		s.encoderLive() // a generation went live
		fail(s, next(t, s), refused, false)
		if p = next(t, s); p.Source.Backend != "ddagrab" {
			t.Fatalf("refused, live, refused: %s", p.Source.Backend)
		}
	})

	t.Run("secure desktop and lost display keep Desktop Duplication", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
		s, ctrl := gdiSession(a, nil)
		s.cancel()
		p := next(t, s)
		s.encoderLive()
		fail(s, p, lost, true) // a mode change ends the live generation
		for range 4 {          // then a UAC prompt for a while
			fail(s, next(t, s), denied, false)
		}
		if p = next(t, s); p.Source.Backend != "ddagrab" || s.failures != 5 || a.gpuCaptureBlocked() != "" {
			t.Fatalf("after the outage: %s, %d failures, finding %q", p.Source.Backend, s.failures, a.gpuCaptureBlocked())
		}
		want := []string{
			"warn: Screen capture failed, retrying: Desktop Duplication lost the display: a display mode change, or a full-screen program took it over.",
			"warn: Screen capture failed, retrying: Windows does not let programs capture the secure desktop: a UAC prompt or the lock screen.",
		}
		if got := msgs(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("notices %q, want %q (one per text)", got, want)
		}
	})

	t.Run("no gdigrab in this FFmpeg", func(t *testing.T) {
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", false))
		s, ctrl := gdiSession(a, nil)
		s.cancel()
		for range 3 {
			p := next(t, s)
			if p.Source.Backend != "ddagrab" {
				t.Fatalf("captures with %s", p.Source.Backend)
			}
			fail(s, p, wineStderr(t), false)
		}
		if got := msgs(t, ctrl); len(got) != 1 || got[0] != "warn: Screen capture failed, retrying: No Direct3D 11 video device: a PC or VM without a GPU." {
			t.Fatalf("notices %q", got)
		}
	})
}

// TestRestartNotice: an identical restart notice is held back until a
// generation went live since it was sent and a minute has passed; another
// text goes out at once. FFmpeg's log contexts are dropped from an error
// nothing recognises.
func TestRestartNotice(t *testing.T) {
	a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps("", true))
	s, ctrl := gdiSession(a, nil)
	s.cancel()
	ev := media.VideoEvent{Err: errors.New("encoder hevc_amf exited: [hevc_amf @ 000001f0] AMF failed to initialise | [vost#0:0/hevc_amf @ 0000020a] [enc:hevc_amf @ 0000020b] Error while opening encoder")}
	for range 3 {
		s.restartNotice(failureNotice(ev))
	}
	s.encoderLive()
	s.restartNotice(failureNotice(ev)) // live since, but within the minute
	s.prefsMu.Lock()
	s.restartNoteAt = time.Now().Add(-2 * time.Minute)
	s.prefsMu.Unlock()
	s.restartNotice(failureNotice(ev)) // live since and a minute later
	s.restartNotice(failureNotice(ev))
	s.restartNotice("Video encoder restarted (something else)")
	var got []string
	for _, n := range ctrlNotices(t, ctrl) {
		got = append(got, n.Msg)
	}
	plain := "Video encoder restarted (encoder hevc_amf exited: AMF failed to initialise | Error while opening encoder)"
	if want := []string{plain, plain, "Video encoder restarted (something else)"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices %q, want %q", got, want)
	}
}

// TestRecheckGPUCapture: a finding of no GPU capture is checked again in the
// background by a session start once it is a minute old, one check at a
// time; only a capture that opens lifts it.
func TestRecheckGPUCapture(t *testing.T) {
	var calls atomic.Int32
	result := make(chan [2]string, 4)
	release := make(chan struct{})
	checkGPUCapture = func(ctx context.Context, ffmpeg string) (string, string) {
		calls.Add(1)
		<-release
		return func(r [2]string) (string, string) { return r[0], r[1] }(<-result)
	}
	t.Cleanup(func() { checkGPUCapture = media.CheckGPUCapture })
	caps := gdiTestCaps(noDevice, true)
	caps.FFmpeg = "ffmpeg.exe"
	a := gdiAgent(Config{Capture: "auto"}, caps)
	setAt := func(at time.Time) {
		a.gpuCap.mu.Lock()
		a.gpuCap.at = at
		a.gpuCap.mu.Unlock()
	}
	setAt(time.Now())
	wait := func(n int32, finding string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			a.gpuCap.mu.Lock()
			checking := a.gpuCap.checking
			a.gpuCap.mu.Unlock()
			if calls.Load() == n && !checking && a.gpuCaptureBlocked() == finding {
				return
			}
		}
		t.Fatalf("%d checks, finding %q; want %d, %q", calls.Load(), a.gpuCaptureBlocked(), n, finding)
	}
	a.recheckGPUCapture() // the probe's finding is fresh
	wait(0, noDevice)
	setAt(time.Now().Add(-2 * gpuCaptureRecheck))
	a.recheckGPUCapture()
	a.recheckGPUCapture() // one check at a time
	result <- [2]string{"", "Desktop duplication access denied"}
	close(release)
	wait(1, noDevice) // inconclusive: the finding stays, checked again a minute later
	a.recheckGPUCapture()
	wait(1, noDevice)
	setAt(time.Now().Add(-2 * gpuCaptureRecheck))
	result <- [2]string{"", ""}
	a.recheckGPUCapture()
	wait(2, "") // the capture opened: GPU capture again
	if b := a.backendFor(proto.Prefs{}); b != "ddagrab" {
		t.Fatalf("after the check: %s", b)
	}
	a.recheckGPUCapture() // nothing to check
	time.Sleep(20 * time.Millisecond)
	wait(2, "")
}

// TestConfigCapture: host config "capture" takes its values, gdigrab too.
func TestConfigCapture(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.json")
	for _, v := range []string{"auto", "ddagrab", "gfxcapture", "gdigrab", "amf", "x11grab", "test"} {
		if err := os.WriteFile(p, []byte(`{"capture":"`+v+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if c, err := LoadConfig(p); err != nil || c.Capture != v {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if err := os.WriteFile(p, []byte(`{"capture":"gdi"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), `capture must be one of "auto"`) {
		t.Fatalf("unknown capture: %v", err)
	}
}

// gpuLessHelperCaps is recon-encoder.exe --print-caps on a PC without a GPU
// encoder (run under Wine: no AMF or NVENC runtime, no FFmpeg libraries).
const gpuLessHelperCaps = `{"t":"caps","v":1,"helperVersion":"0.1.0","backend":"none","vendor":"other","adapterLuid":"00000000:000003f0",` +
	`"adapterName":"Microsoft Basic Render Driver","hagsEnabled":null,"codecs":{},"capture":[],"cursorInVideo":false,"outputs":[],` +
	`"unavailable":{"amf":"AMF runtime (amfrt64.dll) not found in System32: Module not found (error 126)",` +
	`"nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: Module not found (error 126)",` +
	`"lavc":"libavcodec (avcodec-62.dll, avutil-60.dll of FFmpeg 8.x) not found",` +
	`"amd-direct":"no display output on an AMD adapter","wgc":"this build has no C++/WinRT headers (mingw-w64 build)"},"qpcFrequency":10000000}`

// TestOpenPipelineGPULess: on a PC without a GPU the native helper starts,
// reports no usable encoder and is passed over at once, with the reason and
// why each backend was skipped in host.log: one launch, no failures to
// count. The session streams with FFmpeg, capturing with GDI.
func TestOpenPipelineGPULess(t *testing.T) {
	l := &fakeLauncher{caps: gpuLessHelperCaps}
	a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, true))
	a.launchHelper = l.launch
	logs := &lockedLog{}
	s, ctrl := gdiSession(a, logs)
	s.cancel()
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	defer s.vid().Stop()
	if _, ok := s.vid().(*media.Video); !ok || l.count() != 1 {
		t.Fatalf("pipeline %T after %d launches", s.vid(), l.count())
	}
	lines := logs.lines(`msg="video pipeline"`)
	if len(lines) != 1 || !strings.Contains(lines[0], `pipeline=ffmpeg`) || !strings.Contains(lines[0], `reason="it has no usable encoder"`) ||
		!strings.Contains(lines[0], "amf: AMF runtime (amfrt64.dll) not found") || !strings.Contains(lines[0], "nvenc: NVENC runtime") {
		t.Fatalf("log %q", lines)
	}
	p, err := s.buildParams(s.prefs)
	if err != nil || p.Source.Backend != "gdigrab" || p.Encoder.Name != "libx264" {
		t.Fatalf("params %+v %v", p, err)
	}
	if got := ctrlNotices(t, ctrl); len(got) != 1 {
		t.Fatalf("notices %+v", got)
	}
}

// TestSessionSwitchesToGDI runs a session against a stand-in for FFmpeg:
// its ddagrab generation fails with the user's error (FFmpeg 8.1's real
// stderr, recorded under Wine), its gdigrab generation streams (this
// machine's FFmpeg, which has no gdigrab, encodes a test source in its
// place). The session switches after the first failure, starts the gdigrab
// generation at once with the monitor's area, streams, and sends one notice.
func TestSessionSwitchesToGDI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for FFmpeg is a shell script")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	caps, err := media.Probe(context.Background(), ff, nil)
	if err != nil {
		t.Skipf("probe: %v", err)
	}
	if _, ok := caps.Best("h264"); !ok {
		t.Skip("no h264 encoder")
	}
	// The build of the user's PC: ddagrab, gfxcapture, gdigrab; its GPU
	// capture not known to fail (the probe of an agent from before).
	caps.Filters["ddagrab"], caps.Filters["gfxcapture"] = true, true
	caps.Devices = map[string]bool{"gdigrab": true}
	dir := t.TempDir()
	argLog := filepath.Join(dir, "args")
	wine, err := filepath.Abs(filepath.Join("media", "testdata", "ffmpeg81-stderr-ddagrab-libx264.txt"))
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %[1]s
case "$*" in
*ddagrab*) cat %[2]s >&2; exit 171 ;;
*gdigrab*) exec %[3]s -hide_banner -loglevel warning -nostdin -f lavfi -i testsrc2=s=320x180:r=30 -vf settb=AVTB,setpts=time\(0\)*1000000,format=yuv420p -c:v libx264 -preset ultrafast -tune zerolatency -g 9999 -bf 0 -enc_time_base 1:1000000 -fps_mode passthrough -f nut -write_index 0 pipe:1 ;;
esac
exit 1
`, argLog, wine, ff)
	caps.FFmpeg = filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(caps.FFmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(v bool) { cursorOnPlatform = v }(cursorOnPlatform)
	cursorOnPlatform = true
	a := gdiAgent(Config{Capture: "auto", DefaultFPS: 30, MaxFPS: 60, DefaultKbps: 4000, MaxKbps: 100000}, caps)
	logs := &lockedLog{}
	s, ctrl := gdiSession(a, logs)
	s.prefs = proto.Prefs{Monitor: 1, FPS: 30}
	s.video = media.NewVideo(caps, s.log, a.clock)
	defer func() { s.cancel(); s.video.Stop() }()
	go s.videoEvents()
	start := time.Now()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	var f *media.Frame
	select {
	case f = <-s.frameQ:
	case <-time.After(20 * time.Second):
		t.Fatalf("no frame; log:\n%s", strings.Join(logs.lines(""), "\n"))
	}
	if f.Gen != 2 || !f.Key || f.CaptureUs == 0 {
		t.Fatalf("first frame %+v: want generation 2's key frame with a capture stamp", f)
	}
	t.Logf("first frame %v after the session started", time.Since(start).Round(time.Millisecond))
	// The client takes frames: the generation streams on.
	for n := 1; n < 15; n++ {
		select {
		case f = <-s.frameQ:
			if f.Gen != 2 || f.Seq != uint32(n) {
				t.Fatalf("frame %d: %d/%d", n, f.Gen, f.Seq)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("generation 2 stopped after %d frames", n)
		}
	}
	b, _ := os.ReadFile(argLog)
	runs := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(runs) != 2 || !strings.Contains(runs[0], "ddagrab=output_idx=1:") ||
		!strings.Contains(runs[1], "-f gdigrab -framerate 30 -draw_mouse 0 -offset_x -1920 -offset_y 120 -video_size 1920x1080 -i desktop") {
		t.Fatalf("ffmpeg runs:\n%s", b)
	}
	got := ctrlNotices(t, ctrl)
	if len(got) != 1 || got[0].Msg != "No GPU screen capture on this PC ("+noDevice+"): capturing with GDI, which uses more CPU and adds latency." {
		t.Fatalf("notices %+v", got)
	}
	if l := logs.lines(`msg="starting encoder"`); len(l) != 2 || !strings.Contains(l[1], "capture=gdigrab") {
		t.Fatalf("encoder starts %q", l)
	}
	if l := logs.lines(`msg="restarting video" reason="encoder failure"`); len(l) != 1 {
		t.Fatalf("restarts %q", l)
	}
	for _, n := range []string{`level=WARN msg="encoder failed"`, `msg="GPU screen capture failed: capturing with GDI (gdigrab)"`} {
		if l := logs.lines(n); len(l) != 1 {
			t.Fatalf("%s: %q", n, l)
		}
	}
}

// TestProbeSampleGDI: "recon-host probe" prints the command lines of a
// session that captures with GDI where the probe found no GPU capture,
// whatever GPU capture host.json names.
func TestProbeSampleGDI(t *testing.T) {
	for _, capture := range []string{"auto", "ddagrab", "gfxcapture"} {
		cfg := &Config{Capture: capture}
		cfg.Defaults()
		caps := gdiTestCaps(noDevice, true)
		p := ProbeSample(cfg, caps)(caps.Encoders[0])
		if p.Source.Backend != "gdigrab" || p.Source.NativeW == 0 || p.FPS != 60 {
			t.Fatalf("%s: sample %+v", capture, p)
		}
		if _, err := caps.BuildArgs(p); err != nil {
			t.Fatalf("%s: %v", capture, err)
		}
	}
}
