package host

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Pipeline selection with the helper's libavcodec backend (GUIDE 3.8): the
// helper with a vendor backend first, then its libavcodec backend, then the
// FFmpeg command line; and a session on the libavcodec backend, whose caps
// (recovery none, live bitrate flush, no LTR / SVC / ROI / intra refresh)
// decide the ladder and the rate controller.

// capsMsg returns a helper's caps message: backend, vendor and adapter, its
// codecs (JSON object members) and unavailable entries (JSON members).
func capsMsg(backend, vendor, adapter, codecs, unavailable string) string {
	return `{"t":"caps","v":1,"backend":"` + backend + `","vendor":"` + vendor + `","adapterName":"` + adapter +
		`","adapterLuid":"00000000:0000a1b2","codecs":{` + codecs + `},"capture":["dda"],"cursorInVideo":false,"outputs":[],` +
		`"unavailable":{` + unavailable + `},"qpcFrequency":10000000}`
}

// The libavcodec backend's codecs as the helper reports them.
const (
	lavcH264 = `"h264":{"maxW":4096,"maxH":4096,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"flush",` +
		`"maxTemporalLayers":1,"roi":"none","hwInstances":1,"alignW":1,"alignH":1,"liveFps":"flush","assumed":["maxW","maxH","liveBitrate","liveFps"]}`
	lavcAV1 = `"av1":{"maxW":8192,"maxH":8192,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"flush",` +
		`"maxTemporalLayers":1,"roi":"none","hwInstances":1,"alignW":1,"alignH":1,"liveFps":"flush","assumed":["maxW","maxH","liveBitrate","liveFps"]}`
	nvencH264 = `"h264":{"maxW":4096,"maxH":4096,"forceIdr":true,"recovery":"invalidate","maxLtr":0,"intraRefresh":true,"liveBitrate":"seamless","alignW":1,"alignH":1}`

	noAMF     = `"amf":"AMF runtime (amfrt64.dll) not found in System32"`
	noNVENC   = `"nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32"`
	noIntel   = `"lavc":"no Intel adapter: the libavcodec backend encodes with Intel Quick Sync Video"`
	noLavcDLL = `"lavc":"libavcodec (avcodec-62.dll, avutil-60.dll of FFmpeg 8.x) not found in C:\\KR\\ffmpeg-lgpl"`
)

// TestPipelineSelection: the order the session tries its video pipelines
// in, and the "video pipeline" log line that names the choice and why the
// rungs before it were skipped.
func TestPipelineSelection(t *testing.T) {
	ffmpegCaps := &media.Caps{Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"},
		{Name: "libsvtav1", Family: "av1", Vendor: "software"}}}
	h264, av1 := []proto.DecoderInfo{{Family: "h264", HW: true}}, []proto.DecoderInfo{{Family: "av1", HW: true}}
	amf := capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeH264+","+fakeHEVC, noNVENC+","+noIntel)
	amfLavcUsable := capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeH264, noNVENC)
	nvenc := capsMsg("nvenc", "nvidia", "NVIDIA GeForce RTX 4080", nvencH264, noAMF+","+noIntel)
	lavc := capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF+","+noNVENC)
	lavcNVENCUsable := capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF) // a hybrid laptop
	lavcAV1Caps := capsMsg("lavc", "intel", "Intel(R) Arc A770", lavcAV1+","+lavcH264, noNVENC)
	none := capsMsg("none", "intel", "Intel(R) UHD Graphics 770", "", noAMF+","+noNVENC+","+noLavcDLL)
	for _, c := range []struct {
		name        string
		libavcodec  string // host config helperLibavcodec
		lavcMissing bool
		byBackend   map[string]string
		decoders    []proto.DecoderInfo
		backend     string   // the helper backend the session runs, "" = FFmpeg
		launches    []string // backends launched in order ("" = auto)
		log         []string // in the video pipeline line
		noLog       []string
	}{
		{"vendor backend", "", false, map[string]string{"": amf}, h264, "amf", []string{""},
			[]string{"pipeline=helper", "backend=amf", "encoders=hevc_amf_helper,h264_amf_helper"}, []string{"skipped"}},
		{"second vendor backend", "", false, map[string]string{"": nvenc}, h264, "nvenc", []string{""},
			[]string{"pipeline=helper", "backend=nvenc", `skipped="amf: AMF runtime (amfrt64.dll) not found in System32"`}, nil},
		{"libavcodec without a vendor backend", "", false, map[string]string{"": lavc}, h264, "lavc", []string{""},
			[]string{"pipeline=helper", "backend=lavc", "encoders=h264_lavc_helper",
				`skipped="amf: AMF runtime (amfrt64.dll) not found in System32; nvenc: NVENC runtime (nvEncodeAPI64.dll) not found in System32"`}, nil},
		{"libavcodec libraries missing", "", true, map[string]string{"": none}, h264, "", []string{""},
			[]string{"pipeline=ffmpeg", `reason="it has no usable encoder"`, "amf: AMF runtime", "nvenc: NVENC runtime",
				"lavc: its FFmpeg libraries are not installed: ", "has no avcodec-62.dll (install-host.ps1 -InstallLibavcodec installs"}, nil},
		{"libavcodec off, vendor backend usable", "off", false, map[string]string{"": lavcNVENCUsable, "nvenc": nvenc}, h264, "nvenc",
			[]string{"", "nvenc"},
			[]string{"pipeline=helper", "backend=nvenc", `amf: AMF runtime`, `lavc: its libavcodec backend is off (host config \"helperLibavcodec\")`}, nil},
		{"libavcodec off, nothing else", "off", false, map[string]string{"": lavc}, h264, "", []string{""},
			[]string{"pipeline=ffmpeg", `reason="its libavcodec backend is off (host config \"helperLibavcodec\")"`, "amf: AMF runtime",
				"nvenc: NVENC runtime"}, nil},
		{"codec only in the libavcodec backend", "", false, map[string]string{"": amfLavcUsable, "lavc": lavcAV1Caps}, av1, "lavc",
			[]string{"", "lavc"},
			[]string{"pipeline=helper", "backend=lavc", "encoders=av1_lavc_helper,h264_lavc_helper",
				"amf: the codec negotiated with this browser (libsvtav1) is not one of the helper's (h264_amf_helper)", "nvenc: NVENC runtime"}, nil},
		{"codec in no backend", "", false, map[string]string{"": amf}, av1, "", []string{""},
			[]string{"pipeline=ffmpeg", `reason="the codec negotiated with this browser (libsvtav1) is not one of the helper's`,
				"nvenc: NVENC runtime", "lavc: no Intel adapter"}, nil},
		{"codec in no backend, libavcodec not installed", "", true, map[string]string{"": amfLavcUsable}, av1, "", []string{""},
			[]string{"pipeline=ffmpeg", "lavc: its FFmpeg libraries are not installed"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{Capture: "test", Pipeline: "helper", HelperLibavcodec: c.libavcodec}
			cfg.Defaults()
			logs := &lockedLog{}
			l := &fakeLauncher{byBackend: c.byBackend}
			a := &Agent{cfg: &cfg, caps: ffmpegCaps, hostClock: media.NewHostClock(), launchHelper: l.launch}
			if c.lavcMissing {
				a.lavcMissing = encoder.LavcMissing(t.TempDir())
			}
			s := &Session{a: a, hello: proto.Hello{Decoders: c.decoders}, tried: map[string]bool{}, usage: map[string]string{},
				ctx: context.Background(), ctrl: &fakeCtrl{}, log: slog.New(slog.NewTextHandler(logs, nil))}
			notice := s.openPipeline()
			defer s.vid().Stop()
			lines := logs.lines(`msg="video pipeline"`)
			if len(lines) != 1 {
				t.Fatalf("log %q", lines)
			}
			for _, want := range c.log {
				if !strings.Contains(lines[0], want) {
					t.Errorf("log %s\nwithout %s", lines[0], want)
				}
			}
			for _, bad := range c.noLog {
				if strings.Contains(lines[0], bad) {
					t.Errorf("log %s\nwith %s", lines[0], bad)
				}
			}
			on, _, hc := s.onHelper()
			if _, isHelper := s.vid().(*media.HelperVideo); isHelper != (c.backend != "") || on != isHelper || hc.Backend != c.backend {
				t.Fatalf("helper pipeline %v, backend %q; want %q", isHelper, hc.Backend, c.backend)
			}
			if (c.backend == "") != (notice != "") {
				t.Fatalf("notice %q", notice)
			}
			l.mu.Lock()
			launches := slices.Clone(l.backends)
			l.mu.Unlock()
			if !slices.Equal(launches, c.launches) {
				t.Fatalf("launches %q, want %q", launches, c.launches)
			}
		})
	}
}

// TestAdapterBlocker: every helper backend encodes only captures of its own
// GPU, so a monitor on another GPU needs another backend (or FFmpeg).
func TestAdapterBlocker(t *testing.T) {
	c := encoder.Caps{Backend: "lavc", AdapterLUID: "00000000:0000a1b2", AdapterName: "Intel(R) UHD Graphics",
		Outputs: []encoder.Output{
			{Name: `\\.\DISPLAY1`, HMonitor: 0x101, AdapterLUID: "00000000:0000A1B2", AdapterName: "Intel(R) UHD Graphics"},
			{Name: `\\.\DISPLAY2`, HMonitor: 0x202, AdapterLUID: "00000000:0000c3d4", AdapterName: "NVIDIA GeForce RTX 4070 Laptop GPU"}}}
	for _, tc := range []struct {
		name    string
		capture string
		prefs   proto.Prefs
		hmon    uint64
		want    string
	}{
		{"same GPU", "auto", proto.Prefs{}, 0x101, ""},
		{"other GPU", "auto", proto.Prefs{}, 0x202, `its lavc encoder runs on Intel(R) UHD Graphics, the monitor (\\.\DISPLAY2) is on NVIDIA`},
		{"other GPU, AMD Direct Capture", "amf", proto.Prefs{}, 0x202, "the monitor"},
		{"window capture", "auto", proto.Prefs{Window: "Notepad"}, 0x202, ""},
		{"other GPU, WGC monitor capture", "gfxcapture", proto.Prefs{}, 0x202, "the monitor"},
		{"test source", "test", proto.Prefs{}, 0x202, ""},
		{"unknown monitor", "auto", proto.Prefs{}, 0x303, ""},
		{"no monitor handle", "auto", proto.Prefs{}, 0, ""},
	} {
		s := &Session{a: &Agent{cfg: &Config{Capture: tc.capture}}}
		got := s.adapterBlocker(tc.prefs, platform.Monitor{HMonitor: tc.hmon}, &c)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionOnLavcHelper drives a session on a (fake) helper whose encoder
// is the libavcodec backend, as it reports itself: the stream starts without
// LTR slots or intra refresh, the client is told recovery "keyframe" (also a
// client that could wait for recovery frames), a loss is answered with an
// IDR in the running encoder (no recover, no restart), the rate controller
// runs the flush policy (a rate change makes a key frame: changes seconds
// apart), restarts and the spare run the same backend; a live-bitrate
// qualification of the backend that passed seamless (cbr) makes changes
// seamless, one where both modes failed makes each change a new helper.
func TestSessionOnLavcHelper(t *testing.T) {
	key := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
	pFrame := []byte{0, 0, 0, 1, 0x41, 0x9a}
	type rig struct {
		s     *Session
		l     *fakeLauncher
		f     *encoder.Fake
		start map[string]any
		ctrl  *fakeCtrl
		in    *io.PipeWriter
		logs  *lockedLog
	}
	setup := func(t *testing.T, results *qualify.Results) rig {
		l := &fakeLauncher{caps: capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF+","+noNVENC),
			started: make(chan *encoder.Fake, 8)}
		l.handle = func(f *encoder.Fake, m map[string]any) {
			if m["t"] == "start" {
				live, _ := m["liveBitrate"].(string)
				if live == "" {
					live = "flush"
				}
				f.Send(encoder.Started{Backend: "lavc", Encoder: "h264_qsv", Usage: "low_power", Preset: "veryfast", Capture: "synthetic-gpu",
					Codec: "h264", Width: 320, Height: 180, FPS: 30, Kbps: int(m["kbps"].(float64)), LiveBitrate: live, LiveFPS: live,
					RateControl: "vbr_capped", ZeroCopy: true, Barcode: true})
			}
		}
		dir := t.TempDir()
		if results != nil {
			if err := results.Save(qualify.PathFor(filepath.Join(dir, "host.json"))); err != nil {
				t.Fatal(err)
			}
		}
		cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
			DefaultKbps: 4000, MaxKbps: 100000, path: filepath.Join(dir, "host.json")}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		logs := &lockedLog{}
		inR, inW := io.Pipe()
		ctrl := &scriptedCtrl{r: inR}
		s := &Session{
			a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
			hello: proto.Hello{V: proto.HelloVersionRecovery, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
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
		m := expectFakeMsg(t, f, "start")
		for _, k := range []string{"ltrSlots", "intraRefreshFrames", "svcLayers"} {
			if _, ok := m[k]; ok {
				t.Fatalf("start with %s: %v", k, m)
			}
		}
		if m["rc"] != "cbr" || m["codec"] != "h264" {
			t.Fatalf("start %v", m)
		}
		return rig{s: s, l: l, f: f, start: m, ctrl: &ctrl.fakeCtrl, in: inW, logs: logs}
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
	resetKick := func(s *Session) {
		s.kickMu.Lock()
		s.lastKick = time.Time{}
		s.kickMu.Unlock()
	}
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

	t.Run("caps defaults", func(t *testing.T) {
		r := setup(t, nil)
		s, f := r.s, r.f
		if _, ok := r.start["liveBitrate"]; ok {
			t.Fatalf("start %v: without a qualification the helper's default applies", r.start)
		}
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: -1, Data: pFrame, CaptureQPC: 3, OutputQPC: 4})
		nextFrame(t, s)
		nextFrame(t, s)
		// No reference recovery: the client waits for key frames.
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`, `"encoder":"h264_lavc_helper"`, `"recovery":"keyframe"`)
		c := s.vid().Capabilities()
		if !c.ForceIDR || !c.LiveBitrate || !c.LiveBitrateFlush || c.LiveBitrateMeasured || c.IntraRefresh || c.Recovery != media.RecoveryKeyframe {
			t.Fatalf("capabilities %+v", c)
		}
		if p := ratePolicy(c); p.name != "flush" {
			t.Fatalf("rate policy %+v", p)
		}
		for _, want := range []string{`msg="encoder helper started" backend=lavc`, "live_bitrate=flush", "recovery=keyframe",
			"encoder=h264_qsv usage=low_power preset=veryfast"} {
			if len(r.logs.lines(want)) != 1 {
				t.Fatalf("no log line with %s:\n%s", want, r.logs.lines("encoder helper started"))
			}
		}
		// The spare helper beside the live stream runs the same backend.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			r.l.mu.Lock()
			b := slices.Clone(r.l.backends)
			r.l.mu.Unlock()
			if len(b) >= 2 {
				if b[0] != "" || b[1] != "lavc" {
					t.Fatalf("launches %q, want auto then lavc", b)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no spare helper")
			}
		}

		// Frames the helper dropped: an IDR in the running encoder, no
		// recover message (the backend has no reference recovery).
		resetKick(s)
		f.Publish(&encoder.Frame{FrameID: 4, LTRSlot: -1, DroppedBefore: 1, Data: pFrame, CaptureQPC: 5, OutputQPC: 6})
		nextFrame(t, s)
		waitMsg(t, r.ctrl, `"t":"dropped"`, `"gen":1`, `"fromSeq":2`, `"count":1`)
		expectFakeMsg(t, f, "forceIdr")
		none(t, f, "recover")
		f.Publish(&encoder.Frame{FrameID: 5, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 7, OutputQPC: 8})
		if fr := nextFrame(t, s); fr.Gen != 2 || fr.Seq != 0 || !fr.Key {
			t.Fatalf("key frame after the loss %+v", fr)
		}
		// A loss only the client saw: an IDR too.
		resetKick(s)
		b, _ := json.Marshal(proto.ClientMsg{T: proto.MsgLost, Gen: 2, FromSeq: 3})
		if err := proto.WriteMsg(r.in, b); err != nil {
			t.Fatal(err)
		}
		expectFakeMsg(t, f, "forceIdr")
		none(t, f, "recover")

		// A congestion cut: a live rate change (flush: the encoder's next
		// frame is a key frame, in the same generation), announced to the
		// client; no new helper.
		resetKick(s)
		s.congestion(120, signalDelay)
		if m := expectFakeMsg(t, f, "setRate"); m["kbps"] != float64(3400) {
			t.Fatalf("setRate %v", m)
		}
		f.Publish(&encoder.Frame{FrameID: 6, Key: true, LTRSlot: -1, Data: key, CaptureQPC: 9, OutputQPC: 10})
		if fr := nextFrame(t, s); fr.Gen != 2 || fr.Seq != 1 || !fr.Key {
			t.Fatalf("frame after the rate change %+v", fr)
		}
		waitMsg(t, r.ctrl, `"t":"rate"`, `"gen":2`, `"bitrate":3400`)
		// The flush made a key frame: a key-frame request right after it is
		// covered by it.
		s.requestKeyframe("keyframe request")
		none(t, f, "forceIdr")
		if l := r.logs.lines(`msg="restarting video"`); len(l) != 0 {
			t.Fatalf("restarts %q", l)
		}
		select {
		case <-r.l.started:
			t.Fatal("a second helper started a stream")
		default:
		}
	})

	cells := func(seamless, flush string) []qualify.Cell {
		return []qualify.Cell{{Codec: "h264", Quality: "speed", RC: "cbr", LiveBitrate: "seamless", Verdict: seamless},
			{Codec: "h264", Quality: "speed", RC: "cbr", LiveBitrate: "flush", Verdict: flush}}
	}
	results := func(seamless, flush string) *qualify.Results {
		return &qualify.Results{Version: qualify.ResultsVersion, Backend: "lavc", AdapterName: "Intel(R) UHD Graphics 770", Cells: cells(seamless, flush)}
	}

	t.Run("qualified seamless", func(t *testing.T) {
		r := setup(t, results("pass", "pass"))
		if r.start["liveBitrate"] != "seamless" || r.start["rc"] != "cbr" {
			t.Fatalf("start %v", r.start)
		}
		r.f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		nextFrame(t, r.s)
		c := r.s.vid().Capabilities()
		if p := ratePolicy(c); p.name != "seamless" || !c.LiveBitrateMeasured {
			t.Fatalf("rate policy %+v, capabilities %+v", p, c)
		}
		if l := r.logs.lines(`live_bitrate=seamless`); len(l) != 1 || !strings.Contains(l[0], "live_bitrate_from=qualification") {
			t.Fatalf("started log %q", l)
		}
	})

	t.Run("qualified restart", func(t *testing.T) {
		r := setup(t, results("fail", "fail"))
		if _, ok := r.start["liveBitrate"]; ok {
			t.Fatalf("start %v: no live mode passed", r.start)
		}
		r.f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		nextFrame(t, r.s)
		c := r.s.vid().Capabilities()
		if p := ratePolicy(c); p.name != "restart" || c.LiveBitrate || !c.ForceIDR {
			t.Fatalf("rate policy %+v, capabilities %+v", p, c)
		}
		// A rate change starts a new helper (of the same backend), once the
		// restart policy's gap since the start has passed.
		time.Sleep(600 * time.Millisecond)
		if !r.s.congestion(120, signalDelay) {
			t.Fatal("congestion: bitrate kept")
		}
		select {
		case f2 := <-r.l.started:
			if m := expectFakeMsg(t, f2, "start"); m["kbps"] != float64(3400) {
				t.Fatalf("start %v", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no new helper for the rate change")
		}
		r.l.mu.Lock()
		defer r.l.mu.Unlock()
		for _, b := range r.l.backends[1:] {
			if b != "lavc" {
				t.Fatalf("launches %q", r.l.backends)
			}
		}
	})
}
