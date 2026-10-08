package host

import (
	"bytes"
	"context"
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
		{"client reason before the pipeline's", proto.HDRAuto, with(func(h *proto.HDRPrefs) { h.Display = false }), "hevc", helperNoHDR, false,
			"the client's display is not in HDR mode"},
	} {
		got, why := decideHDR(c.config, c.client, c.family, c.pipeline)
		if got != c.want || why != c.why {
			t.Errorf("%s: %v %q, want %v %q", c.name, got, why, c.want, c.why)
		}
	}
}

// TestHDRPipeline: which pipelines make HDR10: the helper with a codec whose
// caps have hdr10, FFmpeg only for the test pattern with libsvtav1 that the
// probe ran.
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
		{"test pattern, libsvtav1, probed", probed, media.Params{Source: test, Encoder: svt}, true},
		{"test pattern, libsvtav1, probe failed", notProbed, media.Params{Source: test, Encoder: svt}, false},
		{"test pattern, libaom-av1", probed, media.Params{Source: test, Encoder: media.EncoderInfo{Name: "libaom-av1", Family: "av1"}}, false},
		{"ddagrab hevc_amf", probed, media.Params{Source: media.Source{Backend: "ddagrab"}, Encoder: media.EncoderInfo{Name: "hevc_amf", Family: "hevc", HW: true}}, false},
		{"gfxcapture hevc_nvenc", probed, media.Params{Source: media.Source{Backend: "gfxcapture"}, Encoder: media.EncoderInfo{Name: "hevc_nvenc", Family: "hevc", HW: true}}, false},
	} {
		why := (&Agent{caps: c.caps}).hdrPipeline(c.p, helper)
		if (why == "") != c.ok {
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
// generation) and the new config is SDR with the reason; a settings change
// to HDR Off restarts it without hdr.
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
	ctrl := &fakeCtrl{}
	prefs := proto.Prefs{HDR: hdrClient("hevc")}
	s := &Session{
		a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
		hello: proto.Hello{V: proto.HelloVersionRecovery, Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}}, Prefs: prefs},
		prefs: prefs, tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
		log: slog.New(slog.NewTextHandler(&lockedLog{}, nil)),
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

	// The client's setting to Off (a settings message): a new helper without hdr.
	off := proto.Prefs{HDR: &proto.HDRPrefs{Mode: proto.HDROff, Display: true, Canvas: true, Decoders: []string{"hevc"}}}
	s.prefsMu.Lock()
	s.prefs = off
	s.prefsMu.Unlock()
	if err := s.startVideo(false, "settings"); err != nil {
		t.Fatal(err)
	}
	var f4 *encoder.Fake
	select {
	case f4 = <-l.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no new helper after the HDR setting changed")
	}
	if m := expectFakeMsg(t, f4, "start"); m["hdr"] != nil {
		t.Fatalf("start with HDR off: %v", m)
	}
	f4.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
	waitMsg(t, ctrl, `"t":"video"`, `"gen":4`, `"hdrNote":"HDR is off in the client's settings"`)
}

// TestHDRPrefsChange: a settings message whose HDR prefs differ is a video
// change (the session restarts the video); an equal one is not.
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
}
