package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// helperCaps returns a helper's caps message with the given codecs (JSON
// object members), capture methods and cursorInVideo.
func helperCaps(codecs, capture string, cursor bool) string {
	return `{"t":"caps","v":1,"backend":"amf","vendor":"amd","adapterName":"AMD Radeon RX 7900 XT","hagsEnabled":true,` +
		`"codecs":{` + codecs + `},"capture":[` + capture + `],"cursorInVideo":` + map[bool]string{true: "true", false: "false"}[cursor] +
		`,"outputs":[],"unavailable":{"wgc":"this build has no C++/WinRT headers"},"qpcFrequency":10000000}`
}

const (
	fakeH264 = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":1,"alignH":1}`
	fakeHEVC = `"hevc":{"maxW":8192,"maxH":4352,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":1,"alignH":1}`
)

func TestHelperBlocker(t *testing.T) {
	dda := encoder.Caps{Capture: []string{"dda"}, Unavailable: map[string]string{"wgc": "no WinRT"}}
	all := encoder.Caps{Capture: []string{"dda", "amd-direct", "wgc"}}
	for _, c := range []struct {
		name       string
		cfg        Config
		prefs      proto.Prefs
		drawCursor bool
		caps       *encoder.Caps
		want       string // substring; "" = the helper can serve the session
	}{
		{"auto, dda", Config{Capture: "auto"}, proto.Prefs{}, false, &dda, ""},
		{"before the caps", Config{Capture: "auto"}, proto.Prefs{}, true, nil, ""},
		{"x11grab", Config{Capture: "x11grab"}, proto.Prefs{}, false, nil, "x11grab"},
		{"test pattern, auto", Config{Capture: "test"}, proto.Prefs{}, true, nil, "test pattern"},
		{"test pattern, helper", Config{Capture: "test", Pipeline: "helper"}, proto.Prefs{}, true, &encoder.Caps{}, ""},
		{"FFmpeg encoder forced", Config{Capture: "auto", Encoder: "hevc_amf"}, proto.Prefs{}, false, nil, "hevc_amf"},
		{"helper encoder forced", Config{Capture: "auto", Encoder: "hevc_amf_helper"}, proto.Prefs{}, false, &dda, ""},
		{"cursor in the video", Config{Capture: "auto"}, proto.Prefs{}, true, &dda, "cursor"},
		{"cursor in the video, helper draws it", Config{Capture: "auto"}, proto.Prefs{}, true,
			&encoder.Caps{Capture: []string{"dda"}, CursorInVideo: true}, ""},
		{"window without WGC", Config{Capture: "auto"}, proto.Prefs{Window: "Notepad"}, false, &dda, "wgc (no WinRT)"},
		{"window with WGC", Config{Capture: "auto"}, proto.Prefs{Window: "Notepad"}, false, &all, ""},
		{"gfxcapture without WGC", Config{Capture: "gfxcapture"}, proto.Prefs{}, false, &dda, "wgc"},
		{"AMD Direct Capture missing", Config{Capture: "amf"}, proto.Prefs{}, false, &dda, "amd-direct"},
		{"AMD Direct Capture", Config{Capture: "amf"}, proto.Prefs{}, false, &all, ""},
		{"no DDA", Config{Capture: "ddagrab"}, proto.Prefs{}, false, &encoder.Caps{Capture: []string{"amd-direct"}}, "dda"},
	} {
		cfg := c.cfg
		s := &Session{a: &Agent{cfg: &cfg}}
		got := s.helperBlocker(c.prefs, c.drawCursor, c.caps)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestHelperSource: what the helper captures, and at which size. The helper
// stretches its source to the size it is given, so a client size of another
// aspect ratio than the monitor's is fitted to the monitor's (even, never
// larger than the client asked, never upscaled); a window keeps its own size.
func TestHelperSource(t *testing.T) {
	for _, c := range []struct {
		name    string
		capture string
		mon     platform.Monitor
		prefs   proto.Prefs
		want    string
	}{
		{"native", "auto", platform.Monitor{W: 2560, H: 1440, DXGIOutput: 1, HMonitor: 7}, proto.Prefs{}, "ddagrab out=1 0x0"},
		{"larger than the monitor", "auto", platform.Monitor{W: 1920, H: 1080, DXGIOutput: 0}, proto.Prefs{Width: 2560, Height: 1440},
			"ddagrab out=0 0x0"},
		{"same aspect", "auto", platform.Monitor{W: 2560, H: 1440}, proto.Prefs{Width: 1920, Height: 1080}, "ddagrab out=0 1920x1080"},
		{"ultrawide host, 16:9 client", "auto", platform.Monitor{W: 3440, H: 1440}, proto.Prefs{Width: 1920, Height: 1080},
			"ddagrab out=0 1920x804"},
		{"16:9 host, 16:10 client", "gfxcapture", platform.Monitor{W: 2560, H: 1440}, proto.Prefs{Width: 1920, Height: 1200},
			"gfxcapture out=0 1920x1080"},
		{"portrait host", "amf", platform.Monitor{W: 1080, H: 1920, Rotated: true}, proto.Prefs{Width: 1920, Height: 1080},
			"amf out=0 608x1080"},
		{"taller than the monitor only", "auto", platform.Monitor{W: 2560, H: 1080}, proto.Prefs{Width: 1920, Height: 1200},
			"ddagrab out=0 1920x810"},
		{"window", "auto", platform.Monitor{W: 2560, H: 1440}, proto.Prefs{Width: 1280, Height: 1024, Window: "Notepad"},
			"gfxcapture out=0 0x0 window=Notepad"},
	} {
		s := &Session{a: &Agent{cfg: &Config{Capture: c.capture}}}
		p := media.Params{Source: media.Source{Backend: "ddagrab"}, DrawCursor: true}
		s.helperSource(&p, c.prefs, c.mon)
		got := fmt.Sprintf("%s out=%d %dx%d", p.Source.Backend, p.Source.Output, p.Width, p.Height)
		if p.Source.Window != "" {
			got += " window=" + p.Source.Window
		}
		if got != c.want || p.DrawCursor || p.Source.NativeW != c.mon.W || p.Source.HMonitor != c.mon.HMonitor {
			t.Errorf("%s: %s (%+v), want %s", c.name, got, p.Source, c.want)
		}
	}
}

// fakeLauncher launches fake helpers for sessions. byBackend: the caps of a
// launch with that backend ("" = auto), else caps; errs: the launches with
// that backend fail, as all do with err; backends records them.
type fakeLauncher struct {
	caps      string
	byBackend map[string]string
	handle    encoder.FakeHandler
	err       error
	errs      map[string]error
	mu        sync.Mutex
	fakes     []*encoder.Fake
	backends  []string
	started   chan *encoder.Fake
}

func (l *fakeLauncher) launch(_ *slog.Logger, backend string) (*encoder.Helper, error) {
	l.mu.Lock()
	l.backends = append(l.backends, backend)
	l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	if err := l.errs[backend]; err != nil {
		return nil, err
	}
	caps := l.caps
	if c, ok := l.byBackend[backend]; ok {
		caps = c
	}
	h, f, err := encoder.LaunchFake(caps, func(f *encoder.Fake, m map[string]any) {
		if l.handle != nil {
			l.handle(f, m)
		}
		if m["t"] == "start" && l.started != nil {
			l.started <- f
		}
	})
	if err == nil {
		l.mu.Lock()
		l.fakes = append(l.fakes, f)
		l.mu.Unlock()
	}
	return h, err
}

func (l *fakeLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fakes)
}

// TestOpenPipeline: the session's pipeline decision, logged once with its
// reason.
func TestOpenPipeline(t *testing.T) {
	ffmpegCaps := &media.Caps{Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"},
		{Name: "libsvtav1", Family: "av1", Vendor: "software"}}}
	h264, av1HW := []proto.DecoderInfo{{Family: "h264", HW: true}}, []proto.DecoderInfo{{Family: "av1", HW: true}}
	for _, c := range []struct {
		name     string
		cfg      Config
		missing  string
		launcher *fakeLauncher
		decoders []proto.DecoderInfo
		helper   bool
		reason   string // in the log line
		notice   string
	}{
		{"not installed", Config{Capture: "test", Pipeline: "helper"}, "recon-encoder.exe is not installed", nil, h264, false,
			"not installed", "not installed"},
		{"launch fails", Config{Capture: "test", Pipeline: "helper"}, "", &fakeLauncher{err: errors.New("CreateProcess: access denied")}, h264,
			false, "did not start", "did not start"},
		{"no usable encoder", Config{Capture: "test", Pipeline: "helper"}, "", &fakeLauncher{caps: helperCaps(``, `"dda"`, false)}, h264,
			false, "no usable encoder", "no usable encoder"},
		{"codec not in the helper", Config{Capture: "test", Pipeline: "helper"}, "", &fakeLauncher{caps: helperCaps(fakeH264, `"dda"`, false)},
			av1HW, false, "libsvtav1", "libsvtav1"},
		{"test pattern in auto", Config{Capture: "test"}, "", &fakeLauncher{caps: helperCaps(fakeH264, `"dda"`, false)}, h264, false,
			"test pattern", ""},
		{"helper", Config{Capture: "test", Pipeline: "helper"}, "", &fakeLauncher{caps: helperCaps(fakeH264+","+fakeHEVC, `"dda"`, false)},
			h264, true, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			cfg.Defaults()
			logs := &lockedLog{}
			a := &Agent{cfg: &cfg, caps: ffmpegCaps, hostClock: media.NewHostClock(), helperMissing: c.missing}
			if c.launcher != nil {
				a.launchHelper = c.launcher.launch
			}
			s := &Session{a: a, hello: proto.Hello{Decoders: c.decoders}, tried: map[string]bool{}, usage: map[string]string{},
				ctx: context.Background(), ctrl: &fakeCtrl{}, log: slog.New(slog.NewTextHandler(logs, nil))}
			notice := s.openPipeline()
			defer s.vid().Stop()
			_, isHelper := s.vid().(*media.HelperVideo)
			lines := logs.lines(`msg="video pipeline"`)
			if isHelper != c.helper || len(lines) != 1 || !strings.Contains(lines[0], c.reason) {
				t.Fatalf("helper %v, log %q; want helper %v with %q", isHelper, lines, c.helper, c.reason)
			}
			if (notice == "") != (c.notice == "") || !strings.Contains(notice, c.notice) {
				t.Fatalf("notice %q, want %q", notice, c.notice)
			}
			names := encoderNames(s.encoders())
			if c.helper != strings.HasPrefix(names, "hevc_amf_helper,h264_amf_helper,libx264") {
				t.Fatalf("encoders %s", names)
			}
			if c.launcher != nil && c.launcher.count() > 0 && !c.helper {
				select {
				case <-c.launcher.fakes[0].Exited():
				case <-time.After(5 * time.Second):
					t.Fatal("the unused helper was not closed")
				}
			}
		})
	}
}

// TestSessionLiveBitrateQualified: a session on the helper reads the
// live-bitrate qualification next to host.json (recon-host qualify) and
// starts the stream with the live-bitrate mode it chose for the codec and rate
// control: flush where seamless failed (the rate controller's flush policy:
// changes seconds apart), seamless where it passed (every 250 ms); results of
// another GPU, or measured with another quality preset than the session's,
// are not used; without a file the helper's defaults apply (seamless, only
// assumed: a change per second).
func TestSessionLiveBitrateQualified(t *testing.T) {
	// Run as the session starts H.264 here: no preset (the helper's
	// default, speed), two LTR slots (recovery ltr).
	cells := func(seamless string) []qualify.Cell {
		return []qualify.Cell{{Codec: "h264", Quality: "speed", LTRSlots: 2, RC: "cbr", LiveBitrate: "seamless", Verdict: seamless},
			{Codec: "h264", Quality: "speed", LTRSlots: 2, RC: "cbr", LiveBitrate: "flush", Verdict: "pass"}}
	}
	balanced := cells("pass")
	for i := range balanced {
		balanced[i].Quality = "balanced"
	}
	for _, c := range []struct {
		name     string
		results  *qualify.Results
		wantLive any // the start's liveBitrate
		wantLog  string
		policy   string // ratePolicy
	}{
		{"flush", &qualify.Results{Version: qualify.ResultsVersion, Backend: "amf", AdapterName: "AMD Radeon RX 7900 XT",
			Cells: cells("fail")}, "flush", "choice=\"h264 speed: adaptive cbr/flush, fixed vbr/-\"", "flush"},
		{"seamless", &qualify.Results{Version: qualify.ResultsVersion, Backend: "amf", AdapterName: "AMD Radeon RX 7900 XT",
			Cells: cells("pass")}, "seamless", "h264 speed: adaptive cbr/seamless", "seamless"},
		{"other preset", &qualify.Results{Version: qualify.ResultsVersion, Backend: "amf", AdapterName: "AMD Radeon RX 7900 XT",
			Cells: balanced}, nil, "h264 balanced: adaptive cbr/seamless", "seamless (assumed)"},
		{"other GPU", &qualify.Results{Version: qualify.ResultsVersion, Backend: "amf", AdapterName: "AMD Radeon RX 6800",
			Cells: cells("fail")}, nil, "live-bitrate qualification not used", "seamless (assumed)"},
		{"none", nil, nil, "no live-bitrate qualification", "seamless (assumed)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.results != nil {
				r := *c.results // its choice is left out: recon-host computes it from the cells
				if err := r.Save(qualify.PathFor(filepath.Join(dir, "host.json"))); err != nil {
					t.Fatal(err)
				}
			}
			l := &fakeLauncher{caps: helperCaps(fakeH264, `"dda"`, false), started: make(chan *encoder.Fake, 4)}
			l.handle = func(f *encoder.Fake, m map[string]any) {
				if m["t"] == "start" {
					live, _ := m["liveBitrate"].(string)
					if live == "" {
						live = "seamless"
					}
					f.Send(encoder.Started{Backend: "amf", Capture: "synthetic-gpu", Codec: "h264", Width: 320, Height: 180, FPS: 30,
						Kbps: int(m["kbps"].(float64)), LiveBitrate: live, Barcode: true})
				}
			}
			cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
				DefaultKbps: 4000, MaxKbps: 100000, path: filepath.Join(dir, "host.json")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &lockedLog{}
			s := &Session{
				a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
				hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
				tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
				ctx: ctx, cancel: cancel, ctrl: &fakeCtrl{}, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
				log: slog.New(slog.NewTextHandler(logs, nil)),
			}
			if n := s.openPipeline(); n != "" {
				t.Fatalf("notice %q", n)
			}
			defer func() { s.vid().Stop() }()
			go s.videoEvents()
			if err := s.startVideo(false, ""); err != nil {
				t.Fatal(err)
			}
			f := <-l.started
			if m := expectFakeMsg(t, f, "start"); m["liveBitrate"] != c.wantLive || m["rc"] != "cbr" {
				t.Fatalf("start liveBitrate %v rc %v, want %v cbr", m["liveBitrate"], m["rc"], c.wantLive)
			}
			if lines := logs.lines(c.wantLog); len(lines) != 1 {
				t.Fatalf("log lines with %q: %q\n%s", c.wantLog, lines, logs.lines("level="))
			}
			key := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
			f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
			select {
			case <-s.frameQ:
			case <-time.After(5 * time.Second):
				t.Fatal("no frame")
			}
			if p := ratePolicy(s.vid().Capabilities()); p.name != c.policy {
				t.Fatalf("rate policy %+v, want %s (capabilities %+v)", p, c.policy, s.vid().Capabilities())
			}
		})
	}
}

// TestSessionOnHelper drives a session on the (fake) native helper: the
// stream starts in the helper with the test pattern's barcode, key frame
// requests and bitrate changes act in the running encoder (no restart, no
// new helper), losses in the helper are reported to the client, a capture
// size change restarts the stream with a new helper once it settled, a key
// frame request while a replacement helper starts keeps that one, and
// repeated helper failures move the session to FFmpeg.
func TestSessionOnHelper(t *testing.T) {
	var mu sync.Mutex
	failing, hold := false, false
	l := &fakeLauncher{caps: helperCaps(fakeH264, `"dda"`, false), started: make(chan *encoder.Fake, 8)}
	l.handle = func(f *encoder.Fake, m map[string]any) {
		if m["t"] != "start" {
			return
		}
		mu.Lock()
		fail, h := failing, hold
		mu.Unlock()
		switch {
		case fail:
			f.Send(encoder.HelperError{Code: "encode_failed", Text: "test", Fatal: true})
			f.Exit(3)
		case !h:
			f.Send(encoder.Started{Backend: "amf", Capture: "synthetic-gpu", Codec: "h264", Width: 320, Height: 180, FPS: 30,
				Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless", Barcode: true})
		}
	}
	ff, _ := exec.LookPath("ffmpeg")
	caps := &media.Caps{}
	if ff != "" {
		if c, err := media.Probe(context.Background(), ff, nil); err == nil {
			caps = c
		}
	}
	cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
		DefaultKbps: 4000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedLog{}
	ctrl := &fakeCtrl{}
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
		hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
		log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	defer func() { s.vid().Stop() }()
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	f := <-l.started
	m := expectFakeMsg(t, f, "start")
	if bc, _ := m["barcode"].(map[string]any); m["capture"] != "synthetic-gpu" || m["width"] != float64(320) || bc["cell"] != float64(16) {
		t.Fatalf("start %v", m)
	}
	key := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
	nextFrame := func() *media.Frame {
		t.Helper()
		select {
		case fr := <-s.frameQ:
			return fr
		case <-time.After(5 * time.Second):
			t.Fatal("no frame reached the session")
		}
		return nil
	}
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	if fr := nextFrame(); fr.Gen != 1 || fr.Seq != 0 || !fr.Key {
		t.Fatalf("first frame %+v", fr)
	}
	if c := s.vid().Capabilities(); !c.ForceIDR || !c.LiveBitrate {
		t.Fatalf("capabilities %+v", c)
	}

	// A key frame request: an IDR in the running encoder, a new generation.
	s.requestKeyframe("keyframe request")
	expectFakeMsg(t, f, "forceIdr")
	f.Publish(&encoder.Frame{FrameID: 2, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 3, OutputQPC: 4})
	if fr := nextFrame(); fr.Gen != 2 || fr.Seq != 0 {
		t.Fatalf("forced key frame %+v", fr)
	}

	// A delay report (from a client without rate reports): the bitrate
	// changes in the encoder (x0.85), the client hears of it. A seamless
	// change makes no key frame: the client's key-frame request right after
	// it still gets one.
	s.kickMu.Lock()
	s.lastKick = time.Time{}
	s.kickMu.Unlock()
	s.congestion(120, signalDelay)
	if m := expectFakeMsg(t, f, "setRate"); m["kbps"] != float64(3400) {
		t.Fatalf("setRate %v", m)
	}
	s.requestKeyframe("keyframe request")
	expectFakeMsg(t, f, "forceIdr")

	// Frames the helper dropped: reported to the client, and a key frame.
	s.kickMu.Lock()
	s.lastKick = time.Time{}
	s.kickMu.Unlock()
	f.Publish(&encoder.Frame{FrameID: 5, LTRSlot: -1, DroppedBefore: 2, Data: []byte{0, 0, 0, 1, 0x41}, CaptureQPC: 5, OutputQPC: 6})
	if fr := nextFrame(); fr.Gen != 2 || fr.Seq != 3 {
		t.Fatalf("frame after the loss %+v", fr)
	}
	expectFakeMsg(t, f, "forceIdr")
	if l := logs.lines(`msg="restarting video"`); len(l) != 0 {
		t.Fatalf("restarts on the helper: %q", l)
	}
	select {
	case <-l.started: // (the spare helper kept beside the stream is launched, not started)
		t.Fatal("a second helper started a stream")
	default:
	}
	waitMsg(t, ctrl, `"t":"rate"`, `"gen":2`, `"bitrate":3400`, `"fps":30`)
	waitMsg(t, ctrl, `"t":"dropped"`, `"gen":2`, `"fromSeq":1`, `"count":2`) // sent asynchronously
	waitMsg(t, ctrl, `"t":"video"`, `"gen":2`, `"encoder":"h264_amf_helper"`)

	// The capture source changed size (a display mode change or rotation;
	// a window being resized reports every frame): once it settled, one new
	// helper, although the session's parameters are the same.
	for i := 0; i < 3; i++ {
		f.Send(encoder.CaptureChanged{Reason: "resized", Width: 180, Height: 320, Rotation: 90})
		time.Sleep(50 * time.Millisecond)
	}
	var f2 *encoder.Fake
	select {
	case f2 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after the capture was resized")
	}
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 7, OutputQPC: 8})
	if fr := nextFrame(); fr.Gen != 3 || fr.Seq != 0 {
		t.Fatalf("first frame after the resize %+v", fr)
	}
	select {
	case <-f.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the resized helper was not shut down")
	}
	select {
	case <-l.started:
		t.Fatal("more than one new helper for the resize")
	case <-time.After(500 * time.Millisecond):
	}
	if l := logs.lines(`msg="restarting video" reason="capture resized"`); len(l) != 1 {
		t.Fatalf("resize restarts %q", l)
	}
	f = f2

	// The helper fails; while its replacement starts, the client asks for a
	// key frame (its watchdog): the starting helper is kept, no other one.
	mu.Lock()
	hold = true
	mu.Unlock()
	f.Send(encoder.HelperError{Code: "encode_failed", Text: "test", Fatal: true})
	var f3 *encoder.Fake
	select {
	case f3 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no replacement helper")
	}
	s.kickMu.Lock()
	s.lastKick = time.Time{}
	s.kickMu.Unlock()
	s.requestKeyframe("keyframe request")
	select {
	case <-l.started:
		t.Fatal("the key frame request started another helper")
	case <-f3.Exited():
		t.Fatal("the key frame request killed the starting helper")
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	hold = false
	mu.Unlock()
	f3.Send(encoder.Started{Backend: "amf", Capture: "synthetic-gpu", Codec: "h264", Width: 320, Height: 180, FPS: 30, Kbps: 3000,
		LiveBitrate: "seamless", Barcode: true})
	f3.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 9, OutputQPC: 10})
	if fr := nextFrame(); fr.Gen != 4 || fr.Seq != 0 {
		t.Fatalf("replacement's first frame %+v", fr)
	}
	f = f3

	// Two more failures (three within a minute): the session continues on
	// FFmpeg, its generations after the helper's.
	mu.Lock()
	failing = true
	mu.Unlock()
	f.Send(encoder.HelperError{Code: "encode_failed", Text: "test", Fatal: true})
	f.Exit(3)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s.vid().(*media.Video); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not fall back to FFmpeg")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l := logs.lines(`msg="video pipeline" pipeline=ffmpeg was=helper`); len(l) != 1 || !strings.Contains(l[0], "failed too often") {
		t.Fatalf("fallback log %q", l)
	}
	waitMsg(t, ctrl, `"t":"notice"`, "streaming with FFmpeg")
	if len(caps.Encoders) == 0 {
		t.Skip("no ffmpeg: the FFmpeg generation cannot start here")
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		fr := nextFrame()
		if fr.Gen > 4 {
			break // gen 5 and later: an FFmpeg generation (helper restarts never went live)
		}
		if time.Now().After(deadline) {
			t.Fatal("no FFmpeg frames")
		}
	}
	if l := logs.lines(`msg="starting encoder"`); len(l) == 0 || strings.Contains(l[0], "_helper") {
		t.Fatalf("FFmpeg start %q", l)
	}
}

// TestSessionRefRecovery: ACK-based recovery end to end in the session (GUIDE
// 3.5) on a fake AMF helper with LTR slots. A client with hello v >= 3 is
// told recovery "ltr"; its frame ACKs of LTR-marked frames reach the helper;
// frames the helper dropped and losses the client reports ({"t":"lost"}) are
// answered with a recover naming the newest acknowledged LTR frame, never a
// key frame; the recovery frame goes out flagged and the answer is logged; a
// loss nothing can be recovered from (the generation's key frame) gets a key
// frame in the encoder; a stale generation's report nothing. A v2 client is
// told "keyframe" and gets key frames, as before.
func TestSessionRefRecovery(t *testing.T) {
	key := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
	pFrame := []byte{0, 0, 0, 1, 0x41, 0x9a}
	type rig struct {
		s    *Session
		f    *encoder.Fake
		ctrl *fakeCtrl
		in   *io.PipeWriter
		logs *lockedLog
	}
	setup := func(t *testing.T, helloV int) rig {
		l := &fakeLauncher{caps: helperCaps(fakeH264, `"dda"`, false), started: make(chan *encoder.Fake, 8)}
		l.handle = func(f *encoder.Fake, m map[string]any) {
			if m["t"] == "start" {
				f.Send(encoder.Started{Backend: "amf", Capture: "synthetic-gpu", Codec: "h264", Width: 320, Height: 180, FPS: 30,
					Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless", LTRSlots: int(m["ltrSlots"].(float64)), Barcode: true})
			}
		}
		cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
			DefaultKbps: 4000, MaxKbps: 100000}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		logs := &lockedLog{}
		inR, inW := io.Pipe()
		ctrl := &scriptedCtrl{r: inR}
		s := &Session{
			a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
			hello: proto.Hello{V: helloV, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
			tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
			ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
			log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
		if n := s.openPipeline(); n != "" {
			t.Fatalf("notice %q", n)
		}
		t.Cleanup(func() { inW.Close(); s.vid().Stop() })
		go s.videoEvents()
		go func() { _ = s.controlLoop() }()
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		f := <-l.started
		if m := expectFakeMsg(t, f, "start"); m["ltrSlots"] != float64(2) {
			t.Fatalf("start %v", m)
		}
		return rig{s: s, f: f, ctrl: &ctrl.fakeCtrl, in: inW, logs: logs}
	}
	nextFrame := func(t *testing.T, s *Session) *media.Frame {
		t.Helper()
		select {
		case fr := <-s.frameQ:
			return fr
		case <-time.After(5 * time.Second):
			t.Fatal("no frame reached the session")
		}
		return nil
	}
	send := func(t *testing.T, r rig, m proto.ClientMsg) {
		t.Helper()
		b, _ := json.Marshal(m)
		if err := proto.WriteMsg(r.in, b); err != nil {
			t.Fatal(err)
		}
	}
	waitLog := func(t *testing.T, logs *lockedLog, substr string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); len(logs.lines(substr)) == 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no log line with %s", substr)
			}
		}
	}
	// none fails on a message of type typ the helper gets within 200 ms.
	none := func(t *testing.T, f *encoder.Fake, typ string) {
		t.Helper()
		deadline := time.After(200 * time.Millisecond)
		for {
			select {
			case m := <-f.Messages():
				if m["t"] == typ {
					t.Fatalf("unexpected %s: %v", typ, m)
				}
			case <-deadline:
				return
			}
		}
	}

	t.Run("v3 client", func(t *testing.T) {
		r := setup(t, proto.HelloVersionRecovery)
		s, f := r.s, r.f
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: 0, Data: pFrame, CaptureQPC: 3, OutputQPC: 4})
		f.Publish(&encoder.Frame{FrameID: 3, LTRSlot: -1, Data: pFrame, CaptureQPC: 5, OutputQPC: 6})
		for seq := uint32(0); seq < 3; seq++ {
			if fr := nextFrame(t, s); fr.Gen != 1 || fr.Seq != seq {
				t.Fatalf("frame %+v", fr)
			}
		}
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`, `"recovery":"ltr"`)
		// The client decoded frames 1-3 (seq 0-2): the ACK of the LTR frame
		// goes on to the helper.
		for seq := uint32(0); seq < 3; seq++ {
			s.vid().Ack(1, seq)
		}
		if m := expectFakeMsg(t, f, "ack"); m["frameId"] != float64(2) {
			t.Fatalf("ack %v", m)
		}
		// The helper dropped frame 4 (ring full): the client is told, the
		// helper recovers from frame 2; no key frame.
		f.Publish(&encoder.Frame{FrameID: 5, LTRSlot: -1, DroppedBefore: 1, Data: pFrame, CaptureQPC: 7, OutputQPC: 8})
		if fr := nextFrame(t, s); fr.Seq != 4 {
			t.Fatalf("frame after the loss %+v", fr)
		}
		waitMsg(t, r.ctrl, `"t":"dropped"`, `"gen":1`, `"fromSeq":3`, `"count":1`)
		if m := expectFakeMsg(t, f, "recover"); m["lostFromFrameId"] != float64(4) || m["ackedLtrFrameId"] != float64(2) {
			t.Fatalf("recover %v", m)
		}
		none(t, f, "forceIdr")
		f.Publish(&encoder.Frame{FrameID: 6, LTRSlot: -1, Recovery: true, RefFloor: 2, Data: pFrame, CaptureQPC: 9, OutputQPC: 10})
		fr := nextFrame(t, s)
		if fr.Seq != 5 || !fr.Recovery || fr.RefFloor != 1 {
			t.Fatalf("recovery frame %+v", fr)
		}
		h, ext := videoHeader(fr, proto.HelloVersionRecovery, 100)
		if v, ok := ext.Get(proto.ExtRefFloor); !ok || v != 1 || h.Flags&proto.FrameFlagKey != 0 {
			t.Fatalf("recovery frame header %+v ext %v", h, ext)
		}
		waitLog(t, r.logs, `msg="loss recovered" gen=1 from_seq=3 by="recovery frame" at=1/5`)
		s.send.take(fr) // as frameSender does (this rig takes the frames itself): the recovery frame ends the wait

		// A loss only the client saw: {"t":"lost"}. Seq 5 is the recovery
		// frame that answered the loss at seq 3: its loss reopens the wait
		// from seq 3 (the client waits from there again), so the helper is
		// asked to recover from frame 4 (seq 3) again, with the acknowledged
		// LTR; a recovery from frame 6 alone would not end the wait.
		send(t, r, proto.ClientMsg{T: proto.MsgLost, Gen: 1, FromSeq: 5})
		if m := expectFakeMsg(t, f, "recover"); m["lostFromFrameId"] != float64(4) || m["ackedLtrFrameId"] != float64(2) {
			t.Fatalf("recover after the client's report %v", m)
		}
		waitLog(t, r.logs, `msg="recovering from a loss" gen=1 from_seq=5 why=client wait_from=3`)
		// Another generation's report: nothing to do.
		send(t, r, proto.ClientMsg{T: proto.MsgLost, Gen: 9, FromSeq: 5})
		none(t, f, "recover")
		// The generation's key frame lost: nothing to recover from, a key
		// frame in the encoder (no restart).
		send(t, r, proto.ClientMsg{T: proto.MsgLost, Gen: 1, FromSeq: 0})
		expectFakeMsg(t, f, "forceIdr")
		waitLog(t, r.logs, `msg="no recovery frame possible, forcing a key frame" gen=1 from_seq=0`)
		if l := r.logs.lines(`msg="restarting video"`); len(l) != 0 {
			t.Fatalf("restarts %q", l)
		}
	})

	// A frame-queue overflow (GUIDE 2.3): the dropped frames are answered
	// with a recovery frame (rung 2), and the bitrate cut changes the
	// encoder's rate seamlessly, without the emergency's IDR.
	t.Run("queue overflow", func(t *testing.T) {
		r := setup(t, proto.HelloVersionRecovery)
		s, f := r.s, r.f
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: 0, Data: pFrame, CaptureQPC: 3, OutputQPC: 4})
		nextFrame(t, s)
		nextFrame(t, s)
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`, `"recovery":"ltr"`)
		s.vid().Ack(1, 1)
		expectFakeMsg(t, f, "ack")
		// Nobody takes frames: the 65th overflows the queue (64).
		for id := uint64(3); id <= 3+64; id++ {
			for !f.Publish(&encoder.Frame{FrameID: id, LTRSlot: -1, Data: pFrame, CaptureQPC: int64(2 * id), OutputQPC: int64(2*id + 1)}) {
				time.Sleep(time.Millisecond) // the ring is full until HelperVideo reads it
			}
		}
		waitMsg(t, r.ctrl, `"t":"dropped"`, `"gen":1`, `"fromSeq":2`, `"count":65`)
		if m := expectFakeMsg(t, f, "recover"); m["lostFromFrameId"] != float64(3) || m["ackedLtrFrameId"] != float64(2) {
			t.Fatalf("recover %v", m)
		}
		if m := expectFakeMsg(t, f, "setRate"); m["kbps"] == nil {
			t.Fatalf("setRate %v", m)
		}
		none(t, f, "forceIdr")
		waitLog(t, r.logs, `msg="congestion: lowering bitrate"`)
		if l := r.logs.lines(`msg="congestion: lowering bitrate"`); !strings.Contains(l[0], "why=overflow") || !strings.Contains(l[0], "urgent=false") {
			t.Fatalf("cut %q", l)
		}
		if l := r.logs.lines(`msg="restarting video"`); len(l) != 0 {
			t.Fatalf("restarts %q", l)
		}
	})

	t.Run("v2 client", func(t *testing.T) {
		r := setup(t, proto.HelloVersionFrameExt)
		s, f := r.s, r.f
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: 0, Data: pFrame, CaptureQPC: 3, OutputQPC: 4})
		nextFrame(t, s)
		nextFrame(t, s)
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`, `"recovery":"keyframe"`)
		s.vid().Ack(1, 1)
		expectFakeMsg(t, f, "ack") // the helper's LTR policy still runs
		f.Publish(&encoder.Frame{FrameID: 4, LTRSlot: -1, DroppedBefore: 1, Data: pFrame, CaptureQPC: 5, OutputQPC: 6})
		nextFrame(t, s)
		expectFakeMsg(t, f, "forceIdr")
		none(t, f, "recover")
	})
}

func expectFakeMsg(t *testing.T, f *encoder.Fake, typ string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-f.Messages():
			if m["t"] == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("the helper got no %s", typ)
		}
	}
}

// messages returns the control messages written so far.
func (c *fakeCtrl) messages(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	b := bytes.NewReader(c.buf.Bytes())
	var out []string
	for b.Len() > 0 {
		m, err := proto.ReadMsg(b, proto.MaxControlMsg)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if json.Unmarshal(m, &v) == nil {
			out = append(out, string(m))
		}
	}
	return out
}

// waitMsg waits for a control message that contains all parts.
func waitMsg(t *testing.T, c *fakeCtrl, parts ...string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !hasMsg(c.messages(t), parts...); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no control message with %q in %q", parts, c.messages(t))
		}
	}
}

func hasMsg(msgs []string, parts ...string) bool {
	for _, m := range msgs {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(m, p)
		}
		if ok {
			return true
		}
	}
	return false
}
