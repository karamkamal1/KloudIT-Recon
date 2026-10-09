package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// hdrClient is a client that can present HDR10 with 10-bit decoders of fams.
func hdrClient(fams ...string) *proto.HDRPrefs {
	return &proto.HDRPrefs{Mode: proto.HDRAuto, Display: true, Canvas: true, Decoders: fams}
}

// TestDecideHDR is the negotiation matrix (GUIDE 3.9 / 4.5): host config x
// client (setting, display, canvas, 10-bit decoder) x codec x pipeline. HDR10
// only when every one says yes; otherwise SDR, with the first reason (none
// for clients before HDR, which never hear of it).
func TestDecideHDR(t *testing.T) {
	with := func(f func(h *proto.HDRPrefs)) *proto.HDRPrefs { h := hdrClient("hevc", "av1"); f(h); return h }
	const helperNoHDR = "the native encoder cannot make HEVC HDR10 on this GPU (caps hdr10 false)"
	for _, c := range []struct {
		name     string
		config   string
		client   *proto.HDRPrefs
		family   string
		pipeline string
		want     bool
		why      string
	}{
		{"everything says yes (HEVC)", proto.HDRAuto, hdrClient("hevc", "av1"), "hevc", "", true, ""},
		{"everything says yes (AV1)", proto.HDRAuto, hdrClient("av1"), "av1", "", true, ""},
		{"host config off", proto.HDROff, hdrClient("hevc"), "hevc", "", false, `HDR is off in the host config ("hdr")`},
		{"host config default", "", hdrClient("hevc"), "hevc", "", false, `HDR is off in the host config ("hdr")`},
		{"client before HDR", proto.HDRAuto, nil, "hevc", "", false, ""},
		{"client before HDR, config off", proto.HDROff, nil, "hevc", "", false, ""},
		{"client setting off", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Mode = proto.HDROff }), "hevc", "", false, "HDR is off in the client's settings"},
		{"client setting unknown", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Mode = "on" }), "hevc", "", false, "HDR is off in the client's settings"},
		{"SDR display", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Display = false }), "hevc", "", false, "the client's display is not in HDR mode"},
		{"no extended-range canvas (2D renderer)", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Canvas, h.Why = false, "the renderer is the 2D canvas" }),
			"hevc", "", false, "the renderer is the 2D canvas"},
		{"no extended-range canvas, no reason", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Canvas = false }), "hevc", "", false,
			"the client's canvas cannot show extended range"},
		{"no 10-bit HEVC decoder", proto.HDRAuto, hdrClient("av1"), "hevc", "", false, "the browser has no 10-bit hevc decoder"},
		{"no 10-bit decoder at all", proto.HDRAuto, hdrClient(), "av1", "", false, "the browser has no 10-bit av1 decoder"},
		{"H.264", proto.HDRAuto, hdrClient("hevc", "av1", "h264"), "h264", "", false, "H.264 streams are SDR (HDR10 needs HEVC Main 10 or AV1 10-bit)"},
		{"H.264, config off", proto.HDROff, hdrClient("hevc"), "h264", "", false, `HDR is off in the host config ("hdr")`},
		{"pipeline cannot", proto.HDRAuto, hdrClient("hevc"), "hevc", helperNoHDR, false, helperNoHDR},
		// What stays the same for the session is the reason before what can
		// change (the client's setting and display): no restart for a note.
		{"the pipeline's reason before the client's", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Display = false }), "hevc", helperNoHDR, false,
			helperNoHDR},
		{"the canvas before the display", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Canvas, h.Display, h.Why = false, false, "the renderer is the 2D canvas" }),
			"hevc", "", false, "the renderer is the 2D canvas"},
		{"the decoder before the setting", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Mode, h.Decoders = proto.HDROff, []string{"av1"} }),
			"hevc", "", false, "the browser has no 10-bit hevc decoder"},
		{"the setting before the display", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Mode, h.Display = proto.HDROff, false }),
			"hevc", "", false, "HDR is off in the client's settings"},
	} {
		got, why := decideHDR(c.config, c.client, c.family, c.pipeline)
		if got != c.want || why != c.why {
			t.Errorf("%s: %v %q, want %v %q", c.name, got, why, c.want, c.why)
		}
	}
}

// TestHDRPipeline: which pipelines make HDR10: the helper with a codec whose
// caps have hdr10 and a capture with an HDR path (not WGC: a window or host
// capture "gfxcapture"), FFmpeg only for the test pattern with libsvtav1 that
// the probe ran.
func TestHDRPipeline(t *testing.T) {
	probed, notProbed := &media.Caps{}, &media.Caps{}
	probed.SetHDRTest(true)
	helper := encoder.Caps{Codecs: map[string]encoder.CodecCaps{"hevc": {HDR10: true}, "av1": {HDR10: true}, "h264": {}}}
	svt := media.EncoderInfo{Name: "libsvtav1", Family: "av1", Vendor: "software"}
	test := media.Source{Backend: "test"}
	for _, c := range []struct {
		name string
		caps *media.Caps
		p    media.Params
		ok   bool
	}{
		{"helper HEVC hdr10", notProbed, media.Params{Encoder: media.EncoderInfo{Name: "hevc_amf_helper", Family: "hevc", Helper: true}}, true},
		{"helper AV1 hdr10", notProbed, media.Params{Encoder: media.EncoderInfo{Name: "av1_nvenc_helper", Family: "av1", Helper: true}}, true},
		{"helper H.264", notProbed, media.Params{Encoder: media.EncoderInfo{Name: "h264_amf_helper", Family: "h264", Helper: true}}, false},
		{"helper DDA", notProbed, media.Params{Source: media.Source{Backend: "ddagrab"}, Encoder: media.EncoderInfo{Name: "hevc_amf_helper", Family: "hevc", Helper: true}}, true},
		{"helper AMD Direct Capture", notProbed, media.Params{Source: media.Source{Backend: "amf"}, Encoder: media.EncoderInfo{Name: "hevc_amf_helper", Family: "hevc", Helper: true}}, true},
		{"helper WGC (a window)", notProbed, media.Params{Source: media.Source{Backend: "gfxcapture", Window: "Game"},
			Encoder: media.EncoderInfo{Name: "hevc_amf_helper", Family: "hevc", Helper: true}}, false},
		{"helper WGC (host capture gfxcapture)", notProbed, media.Params{Source: media.Source{Backend: "gfxcapture"},
			Encoder: media.EncoderInfo{Name: "av1_nvenc_helper", Family: "av1", Helper: true}}, false},
		{"test pattern, libsvtav1, probed", probed, media.Params{Source: test, Encoder: svt}, true},
		{"test pattern, libsvtav1, probe failed", notProbed, media.Params{Source: test, Encoder: svt}, false},
		{"test pattern, libaom-av1", probed, media.Params{Source: test, Encoder: media.EncoderInfo{Name: "libaom-av1", Family: "av1"}}, false},
		{"ddagrab hevc_amf", probed, media.Params{Source: media.Source{Backend: "ddagrab"}, Encoder: media.EncoderInfo{Name: "hevc_amf", Family: "hevc", HW: true}}, false},
		{"gfxcapture hevc_nvenc", probed, media.Params{Source: media.Source{Backend: "gfxcapture"}, Encoder: media.EncoderInfo{Name: "hevc_nvenc", Family: "hevc", HW: true}}, false},
	} {
		why := (&Agent{caps: c.caps}).hdrPipeline(c.p, helper)
		if (why == "") != c.ok || (c.p.Source.Backend == "gfxcapture" && c.p.Encoder.Helper && why != media.HelperWGCNoHDR) {
			t.Errorf("%s: %q", c.name, why)
		}
	}
}

// TestSessionHDRChoice: the FFmpeg test path's generations (buildParams):
// HDR10 for a client that can present it with AV1 (libsvtav1); the automatic
// codec choice is not changed for HDR (H.264 first: SDR with the reason); a
// client before HDR, or a host with HDR off, streams SDR exactly as before.
// The decision is logged once per change.
func TestSessionHDRChoice(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"}, {Name: "libsvtav1", Family: "av1", Vendor: "software"}}}
	caps.SetHDRTest(true)
	decoders := []proto.DecoderInfo{{Family: "h264", HW: true}, {Family: "av1"}}
	for _, c := range []struct {
		name   string
		hdr    string
		prefs  proto.Prefs
		want   bool
		enc    string
		note   string
		logged bool
	}{
		{"AV1, HDR client, host auto", proto.HDRAuto, proto.Prefs{Codec: "av1", HDR: hdrClient("av1")}, true, "libsvtav1", "", true},
		{"auto codec: H.264 stays SDR", proto.HDRAuto, proto.Prefs{HDR: hdrClient("av1")}, false, "libx264",
			"H.264 streams are SDR (HDR10 needs HEVC Main 10 or AV1 10-bit)", true},
		{"host config off", "", proto.Prefs{Codec: "av1", HDR: hdrClient("av1")}, false, "libsvtav1", `HDR is off in the host config ("hdr")`, true},
		{"client before HDR", proto.HDRAuto, proto.Prefs{Codec: "av1"}, false, "libsvtav1", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Capture: "test", TestWidth: 480, TestHeight: 270, HDR: c.hdr}
			cfg.Defaults()
			var logs bytes.Buffer
			s := &Session{
				a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
				hello: proto.Hello{V: proto.HelloVersionRecovery, Decoders: decoders},
				ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
				log: slog.New(slog.NewTextHandler(&logs, nil)),
			}
			for i := 0; i < 2; i++ {
				p, err := s.buildParams(c.prefs)
				if err != nil {
					t.Fatal(err)
				}
				if p.HDR != c.want || p.HDRNote != c.note || p.Encoder.Name != c.enc {
					t.Fatalf("HDR %v %q with %s, want %v %q with %s", p.HDR, p.HDRNote, p.Encoder.Name, c.want, c.note, c.enc)
				}
				if args, err := caps.BuildArgs(p); err != nil || strings.Contains(strings.Join(args, " "), "zscale") != c.want {
					t.Fatalf("args (%v): %q", err, args)
				}
			}
			if n := strings.Count(logs.String(), `msg="hdr choice"`); n != map[bool]int{true: 1, false: 0}[c.logged] {
				t.Fatalf("%d hdr choice lines:\n%s", n, logs.String())
			}
		})
	}
}

// TestSessionHelperHDR: on the native helper (fake), an HDR10 client with
// HEVC gets a start with hdr; the helper's started HDR fields become the
// video config's (10-bit codec string, BT.2020 PQ, the display's metadata).
// Windows HDR turned off during the stream restarts it (a new helper, a new
// generation) and the new config is SDR with the reason. Settings messages
// that change only the client's HDR prefs (controlLoop) restart the video
// when they change the HDR decision (HDR Off: a new helper without hdr; Auto
// again: with hdr), keeping the congestion back-off, and not otherwise (a
// decoder list that keeps HEVC, the display under HDR Off).
func TestSessionHelperHDR(t *testing.T) {
	const fakeHEVC10 = `"hevc":{"maxW":8192,"maxH":4352,"tenBit":true,"hdr10":true,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":1,"alignH":1}`
	md := &encoder.HDRMetadata{DisplayPrimaries: [3][2]float64{{0.708, 0.292}, {0.17, 0.797}, {0.131, 0.046}}, WhitePoint: [2]float64{0.3127, 0.329},
		MaxLuminance: 1015.5, MinLuminance: 0.005, MaxCLL: 1016, MaxFALL: 400}
	var mu sync.Mutex
	displayHDR := true
	l := &fakeLauncher{caps: helperCaps(fakeHEVC10, `"dda"`, false), started: make(chan *encoder.Fake, 8)}
	l.handle = func(f *encoder.Fake, m map[string]any) {
		if m["t"] != "start" {
			return
		}
		mu.Lock()
		on := displayHDR && m["hdr"] == true
		mu.Unlock()
		st := encoder.Started{Backend: "amf", Capture: "synthetic-gpu", Codec: "hevc", Width: 1920, Height: 1080, FPS: 60, Kbps: int(m["kbps"].(float64)),
			LiveBitrate: "seamless", BitDepth: 8, ColorSpace: "bt709"}
		if on {
			st.HDR, st.BitDepth, st.ColorSpace, st.HDRMetadata = true, 10, "bt2020-pq", md
		}
		f.Send(st)
	}
	// The helper's synthetic GPU source (capture "test"; it plays an HDR
	// output with hdr in the real helper): the fake answers as the display
	// is.
	cfg := &Config{Capture: "test", Pipeline: "helper", HDR: proto.HDRAuto, TestWidth: 1920, TestHeight: 1080, DefaultFPS: 60, MaxFPS: 120,
		DefaultKbps: 20000, MaxKbps: 100000}
	cfg.Defaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, client := io.Pipe() // the client's control messages
	defer client.Close()
	sc := &scriptedCtrl{r: in}
	ctrl := &sc.fakeCtrl
	logs := &lockedLog{}
	prefs := proto.Prefs{HDR: hdrClient("hevc")}
	s := &Session{
		a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
		hello: proto.Hello{V: proto.HelloVersionRecovery, Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}}, Prefs: prefs},
		prefs: prefs, tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: sc, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
		log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	defer func() { s.vid().Stop() }()
	go s.videoEvents()
	go func() { // the frame sender's place: frames are not sent here
		for range s.frameQ {
		}
	}()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	key := []byte{0, 0, 0, 1, 0x26, 0x01, 0xaf} // HEVC IDR slice without parameter sets: the generic codec string
	f := <-l.started
	if m := expectFakeMsg(t, f, "start"); m["hdr"] != true || m["codec"] != "hevc" {
		t.Fatalf("start %v", m)
	}
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":1`, `"codec":"hvc1.2.4.L153.B0"`, `"hdr":true`, `"bitDepth":10`,
		`"colorSpace":{"primaries":"bt2020","transfer":"pq","matrix":"bt2020-ncl","fullRange":false}`, `"maxLuminance":1015.5`, `"maxCll":1016`, `"maxFall":400`)
	if hasMsg(ctrl.messages(t), `"gen":1`, "hdrNote") {
		t.Fatal("an HDR10 config with a note")
	}

	// Windows HDR turned off: the helper says so, the session restarts the
	// stream with a new helper, which streams SDR (the display is SDR now).
	mu.Lock()
	displayHDR = false
	mu.Unlock()
	f.Send(encoder.CaptureChanged{Reason: "hdr", Width: 1920, Height: 1080, HDR: false, Text: "Windows HDR turned off"})
	var f2 *encoder.Fake
	select {
	case f2 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after Windows HDR turned off")
	}
	if m := expectFakeMsg(t, f2, "start"); m["hdr"] != true || f2 == f {
		t.Fatalf("restart start %v", m)
	}
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":2`, `"codec":"hvc1.1.6.L153.B0"`, `"hdrNote":"the host display is not in Windows HDR mode"`)
	if hasMsg(ctrl.messages(t), `"gen":2`, `"hdr":true`) {
		t.Fatal("an SDR stream announced as HDR10")
	}
	// An SDR stream asked for HDR: Windows HDR on again restarts it too.
	mu.Lock()
	displayHDR = true
	mu.Unlock()
	f2.Send(encoder.CaptureChanged{Reason: "hdr", Width: 1920, Height: 1080, HDR: true})
	var f3 *encoder.Fake
	select {
	case f3 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after Windows HDR turned on")
	}
	expectFakeMsg(t, f3, "start")
	f3.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":3`, `"hdr":true`)

	// Settings messages with HDR prefs only (the client's control stream),
	// under a congestion back-off to 7000 kbit/s.
	go func() { _ = s.controlLoop() }()
	s.rate.mu.Lock()
	s.rate.est = 7000
	s.rate.mu.Unlock()
	settings := func(h *proto.HDRPrefs) {
		t.Helper()
		b, _ := json.Marshal(proto.ClientMsg{T: "settings", Prefs: &proto.Prefs{HDR: h}})
		if err := proto.WriteMsg(client, b); err != nil {
			t.Fatal(err)
		}
	}
	both := []string{"hevc", "av1"}
	// A 10-bit AV1 decoder more: the HEVC stream's decision stays (no
	// restart). The setting to Off: a new helper without hdr, at the
	// backed-off bitrate. (Had the first restarted, generation 4 would be
	// that restart's HDR10 one.)
	settings(&proto.HDRPrefs{Mode: proto.HDRAuto, Display: true, Canvas: true, Decoders: both})
	settings(&proto.HDRPrefs{Mode: proto.HDROff, Display: true, Canvas: true, Decoders: both})
	var f4 *encoder.Fake
	select {
	case f4 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after the HDR setting changed")
	}
	if m := expectFakeMsg(t, f4, "start"); m["hdr"] != nil || m["kbps"] != float64(7000) {
		t.Fatalf("start with HDR off: %v (want no hdr, the backed-off 7000 kbps)", m)
	}
	f4.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":4`, `"hdrNote":"HDR is off in the client's settings"`)
	// Under HDR Off the display going SDR changes nothing (no restart);
	// Auto on an HDR display again restarts with hdr: generation 5 is HDR10.
	settings(&proto.HDRPrefs{Mode: proto.HDROff, Display: false, Canvas: true, Decoders: both})
	settings(&proto.HDRPrefs{Mode: proto.HDRAuto, Display: true, Canvas: true, Decoders: both})
	var f5 *encoder.Fake
	select {
	case f5 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after the HDR setting changed back")
	}
	if m := expectFakeMsg(t, f5, "start"); m["hdr"] != true {
		t.Fatalf("start with HDR Auto: %v", m)
	}
	f5.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":5`, `"hdr":true`)
	if n := len(logs.lines(`msg="restarting video" reason="HDR settings"`)); n != 2 {
		t.Fatalf("%d restarts for HDR settings, want 2", n)
	}
}

// TestHDRPrefsChange: a settings message whose HDR prefs differ (SameHDR)
// restarts the video only when it changes the current generation's HDR
// decision or its reason (hdrRestart): an HDR10 stream whose client's
// display left HDR mode, or whose decoder the client withdrew, or an SDR
// stream that can be HDR10 now. Not an SDR stream that cannot be HDR10
// anyway (host config off, H.264, FFmpeg's screen capture, the helper's WGC,
// a 2D canvas client) when the display changes, nor a stream whose family
// keeps its decoder, nor when nothing streams.
func TestHDRPrefsChange(t *testing.T) {
	a, b := hdrClient("hevc"), hdrClient("hevc")
	if !proto.SameHDR(a, b) || !proto.SameHDR(nil, nil) || proto.SameHDR(a, nil) {
		t.Fatal("equal prefs differ")
	}
	for _, f := range []func(h *proto.HDRPrefs){
		func(h *proto.HDRPrefs) { h.Mode = proto.HDROff }, func(h *proto.HDRPrefs) { h.Display = false },
		func(h *proto.HDRPrefs) { h.Canvas = false }, func(h *proto.HDRPrefs) { h.Decoders = []string{"hevc", "av1"} },
	} {
		c := hdrClient("hevc")
		f(c)
		if proto.SameHDR(a, c) {
			t.Errorf("%+v counts as unchanged", c)
		}
	}

	caps := &media.Caps{}
	caps.SetHDRTest(true)
	svt := media.EncoderInfo{Name: "libsvtav1", Family: "av1", Vendor: "software"}
	x264 := media.EncoderInfo{Name: "libx264", Family: "h264", Vendor: "software"}
	amf := media.EncoderInfo{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true}
	helperHEVC := media.EncoderInfo{Name: "hevc_amf_helper", Family: "hevc", Helper: true}
	test := media.Source{Backend: "test"}
	with := func(f func(h *proto.HDRPrefs)) *proto.HDRPrefs { h := hdrClient("hevc", "av1"); f(h); return h }
	sdrDisplay := func(h *proto.HDRPrefs) { h.Display = false }
	canvas2D := func(h *proto.HDRPrefs) { h.Canvas, h.Why = false, "HDR needs the WebGPU renderer" }
	for _, c := range []struct {
		name     string
		config   string
		p        media.Params // the current generation (its HDR decision from old)
		none     bool         // nothing streams
		old, now *proto.HDRPrefs
		restart  bool
	}{
		{"HDR10 stream, the display leaves HDR mode", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			hdrClient("hevc", "av1"), with(sdrDisplay), true},
		{"HDR10 stream, the setting Off", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			hdrClient("av1"), with(func(h *proto.HDRPrefs) { h.Mode = proto.HDROff }), true},
		{"HDR10 stream, its decoder withdrawn", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			hdrClient("hevc", "av1"), hdrClient("hevc"), true},
		{"HDR10 stream, another family's decoder withdrawn", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			hdrClient("hevc", "av1"), hdrClient("av1"), false},
		{"SDR stream that can be HDR10: the display enters HDR mode", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			with(sdrDisplay), hdrClient("hevc", "av1"), true},
		{"host config off, the display changes", proto.HDROff, media.Params{Source: test, Encoder: svt}, false,
			hdrClient("av1"), with(sdrDisplay), false},
		{"H.264, the display changes", proto.HDRAuto, media.Params{Source: test, Encoder: x264}, false,
			hdrClient("hevc", "av1"), with(sdrDisplay), false},
		{"FFmpeg screen capture, the display changes", proto.HDRAuto, media.Params{Source: media.Source{Backend: "ddagrab"}, Encoder: amf}, false,
			with(sdrDisplay), hdrClient("hevc", "av1"), false},
		{"helper WGC, the display changes", proto.HDRAuto, media.Params{Source: media.Source{Backend: "gfxcapture", Window: "Game"}, Encoder: helperHEVC}, false,
			hdrClient("hevc"), with(sdrDisplay), false},
		{"2D canvas client, the display changes", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			with(canvas2D), with(func(h *proto.HDRPrefs) { canvas2D(h); sdrDisplay(h) }), false},
		{"HDR Off, the display changes", proto.HDRAuto, media.Params{Source: test, Encoder: svt}, false,
			with(func(h *proto.HDRPrefs) { h.Mode = proto.HDROff }), with(func(h *proto.HDRPrefs) { h.Mode = proto.HDROff; sdrDisplay(h) }), false},
		{"nothing streams", proto.HDRAuto, media.Params{}, true, hdrClient("av1"), with(sdrDisplay), false},
	} {
		cfg := &Config{HDR: c.config}
		cfg.Defaults()
		s := &Session{a: &Agent{cfg: cfg, caps: caps}, video: currentOnly{p: c.p, ok: !c.none},
			helperCaps: encoder.Caps{Codecs: map[string]encoder.CodecCaps{"hevc": {HDR10: true}}}}
		if c.p.Encoder.Helper {
			s.helperEncs = []media.EncoderInfo{helperHEVC}
		}
		_, _, helper := s.onHelper()
		c.p.HDR, c.p.HDRNote = decideHDR(cfg.hdr(), c.old, c.p.Encoder.Family, s.a.hdrPipeline(c.p, helper))
		s.video = currentOnly{p: c.p, ok: !c.none}
		if got := s.hdrRestart(proto.Prefs{HDR: c.now}); got != c.restart {
			t.Errorf("%s: restart %v, want %v (generation HDR %v %q)", c.name, got, c.restart, c.p.HDR, c.p.HDRNote)
		}
	}
}

// currentOnly is a pipeline whose current generation has the parameters p
// (ok: one streams); nothing else is called.
type currentOnly struct {
	media.Pipeline
	p  media.Params
	ok bool
}

func (c currentOnly) Current() (media.Params, bool) { return c.p, c.ok }
