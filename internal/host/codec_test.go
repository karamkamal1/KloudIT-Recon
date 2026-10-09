package host

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// timed is a client decoder with a timed decode of the 1920x1080 sample.
func timed(family string, hw bool, ms float64) proto.DecoderInfo {
	accel := "prefer-hardware"
	if !hw {
		accel = "no-preference"
	}
	return proto.DecoderInfo{Family: family, HW: hw, Timing: &proto.DecodeTiming{Ms: ms, W: 1920, H: 1080, N: 7, Accel: accel}}
}

// TestChooseFamily checks the rule inside a tier (step 4.2): the first family
// in the tier's order unless a later one decodes clearly faster at the
// stream's size; AV1 only with the "faster" policy and never an encoder that
// pads the picture; H.264 against HEVC or AV1 needs the larger margin.
func TestChooseFamily(t *testing.T) {
	cand := func(d proto.DecoderInfo, pads bool) codecCandidate {
		return codecCandidate{enc: media.EncoderInfo{Name: d.Family + "_x", Family: d.Family, HW: true}, dec: d, pads: pads}
	}
	untimed := func(f string) proto.DecoderInfo { return proto.DecoderInfo{Family: f, HW: true} }
	for _, c := range []struct {
		name   string
		cands  []codecCandidate
		policy string
		w, h   int
		want   string
	}{
		{"no timings (clients before 4.2): the order", []codecCandidate{cand(untimed("hevc"), false), cand(untimed("av1"), false), cand(untimed("h264"), false)},
			AV1Faster, 1920, 1080, "hevc"},
		{"AV1 faster, policy fallback", []codecCandidate{cand(timed("hevc", true, 3), false), cand(timed("av1", true, 1.5), false)}, AV1Fallback, 1920, 1080, "hevc"},
		{"AV1 faster, policy faster", []codecCandidate{cand(timed("hevc", true, 3), false), cand(timed("av1", true, 1.5), false)}, AV1Faster, 1920, 1080, "av1"},
		{"AV1 faster by noise only (0.2 ms)", []codecCandidate{cand(timed("hevc", true, 3), false), cand(timed("av1", true, 2.8), false)}, AV1Faster, 1920, 1080, "hevc"},
		{"AV1 faster by under 10 %", []codecCandidate{cand(timed("hevc", true, 12), false), cand(timed("av1", true, 11), false)}, AV1Faster, 1920, 1080, "hevc"},
		{"AV1 faster but its encoder pads the size", []codecCandidate{cand(timed("hevc", true, 3), false), cand(timed("av1", true, 1), true)}, AV1Faster, 1920, 1080, "hevc"},
		{"AV1 slower", []codecCandidate{cand(timed("hevc", true, 2), false), cand(timed("av1", true, 4), false)}, AV1Faster, 1920, 1080, "hevc"},
		{"AV1 faster, HEVC not timed", []codecCandidate{cand(untimed("hevc"), false), cand(timed("av1", true, 1), false)}, AV1Faster, 1920, 1080, "hevc"},
		// H.264 compresses worse: 2 ms and 25 % at the stream's size.
		{"H.264 1 ms faster at 1080p", []codecCandidate{cand(timed("hevc", true, 2.5), false), cand(timed("h264", true, 1.5), false)}, AV1Fallback, 1920, 1080, "hevc"},
		{"H.264 3 ms faster at 1080p", []codecCandidate{cand(timed("hevc", true, 5), false), cand(timed("h264", true, 2), false)}, AV1Fallback, 1920, 1080, "h264"},
		{"H.264 2 ms but only 20 % faster", []codecCandidate{cand(timed("hevc", true, 10), false), cand(timed("h264", true, 8), false)}, AV1Fallback, 1920, 1080, "hevc"},
		// The stream's size: a difference may be all fixed cost (it stays
		// 0.6 ms per 4K frame) or all per pixel (1.33 ms of the 3 ms per
		// 720p frame); the margin must hold either way.
		{"H.264 0.6 ms faster at 1080p, stream 1080p", []codecCandidate{cand(timed("hevc", true, 1.6), false), cand(timed("h264", true, 1.0), false)}, AV1Fallback, 1920, 1080, "hevc"},
		{"H.264 0.6 ms faster at 1080p, stream 4K", []codecCandidate{cand(timed("hevc", true, 1.6), false), cand(timed("h264", true, 1.0), false)}, AV1Fallback, 3840, 2160, "hevc"},
		{"H.264 3 ms faster at 1080p, stream 4K", []codecCandidate{cand(timed("hevc", true, 5), false), cand(timed("h264", true, 2), false)}, AV1Fallback, 3840, 2160, "h264"},
		{"H.264 3 ms faster at 1080p, stream 720p", []codecCandidate{cand(timed("hevc", true, 5), false), cand(timed("h264", true, 2), false)}, AV1Fallback, 1280, 720, "hevc"},
		{"AV1 0.6 ms faster at 1080p, stream 4K", []codecCandidate{cand(timed("hevc", true, 2), false), cand(timed("av1", true, 1.4), false)}, AV1Faster, 3840, 2160, "av1"},
		{"AV1 0.6 ms faster at 1080p, stream 720p", []codecCandidate{cand(timed("hevc", true, 2), false), cand(timed("av1", true, 1.4), false)}, AV1Faster, 1280, 720, "hevc"},
		{"size unknown: the sample's times", []codecCandidate{cand(timed("hevc", true, 1.6), false), cand(timed("h264", true, 1.0), false)}, AV1Fallback, 0, 0, "hevc"},
		// Without HEVC AV1 is first; H.264 may still replace it.
		{"AV1 first without HEVC (fallback)", []codecCandidate{cand(timed("av1", true, 2), false), cand(timed("h264", true, 1.5), false)}, AV1Fallback, 1920, 1080, "av1"},
		{"H.264 much faster than AV1", []codecCandidate{cand(timed("av1", true, 9), false), cand(timed("h264", true, 2), false)}, AV1Fallback, 1920, 1080, "h264"},
		// Software decode tier: H.264 first, HEVC or AV1 replace it at the small margin.
		{"software tier: AV1 (dav1d) faster, policy faster", []codecCandidate{cand(timed("h264", false, 6), false), cand(timed("hevc", false, 9), false), cand(timed("av1", false, 4), false)},
			AV1Faster, 1920, 1080, "av1"},
		{"software tier: HEVC faster", []codecCandidate{cand(timed("h264", false, 6), false), cand(timed("hevc", false, 4), false), cand(timed("av1", false, 3), false)},
			AV1Fallback, 1920, 1080, "hevc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, why := chooseFamily(c.cands, c.policy, c.w, c.h)
			if got.enc.Family != c.want {
				t.Fatalf("%s (%s), want %s", got.enc.Family, why, c.want)
			}
			if got.enc.Family != c.cands[0].enc.Family && !strings.Contains(why, "decodes clearly faster") {
				t.Fatalf("reason %q", why)
			}
		})
	}
}

// TestCodecSelection checks the automatic choice end to end through
// buildParams, host GPU x client GPU (step 4.2): the host side comes from the
// encoders the probe found working (an RDNA2 or RTX 30 host has no AV1
// encoder; RDNA3's pads to 64x16), the client side from the hello's decoders
// and their timings. HEVC on both vendors by default; AV1 only with
// host.json "av1": "faster" and a client that decodes it clearly faster, and
// never at a size RDNA3 pads; a client's codec setting and a host-forced
// encoder still win; clients before step 4.2 (no timings) get the old order.
func TestCodecSelection(t *testing.T) {
	enc := func(name, fam, vendor string, hw bool) media.EncoderInfo {
		return media.EncoderInfo{Name: name, Family: fam, Vendor: vendor, HW: hw}
	}
	sw := []media.EncoderInfo{enc("libx264", "h264", "software", false), enc("libsvtav1", "av1", "software", false)}
	rdna3 := &media.Caps{Encoders: append([]media.EncoderInfo{enc("av1_amf", "av1", "amd", true), enc("hevc_amf", "hevc", "amd", true), enc("h264_amf", "h264", "amd", true)}, sw...)}
	rdna3.SetAlignment("av1_amf", media.Alignment{W: 64, H: 16, ProbeW: 1920, ProbeH: 1080, CodedW: 1920, CodedH: 1082})
	rdna2 := &media.Caps{Encoders: append([]media.EncoderInfo{enc("hevc_amf", "hevc", "amd", true), enc("h264_amf", "h264", "amd", true)}, sw...)}
	rtx40 := &media.Caps{Encoders: append([]media.EncoderInfo{enc("av1_nvenc", "av1", "nvidia", true), enc("hevc_nvenc", "hevc", "nvidia", true), enc("h264_nvenc", "h264", "nvidia", true)}, sw...)}
	rtx30 := &media.Caps{Encoders: append([]media.EncoderInfo{enc("hevc_nvenc", "hevc", "nvidia", true), enc("h264_nvenc", "h264", "nvidia", true)}, sw...)}

	// Clients (times per 1920x1080 frame).
	fastAV1 := []proto.DecoderInfo{timed("h264", true, 1.4), timed("hevc", true, 2.0), timed("av1", true, 1.2)}
	evenAV1 := []proto.DecoderInfo{timed("h264", true, 1.4), timed("hevc", true, 1.5), timed("av1", true, 1.4)}
	slowHEVC := []proto.DecoderInfo{timed("h264", true, 1.5), timed("hevc", true, 6.0), timed("av1", true, 5.5)}
	noHEVC := []proto.DecoderInfo{timed("h264", true, 1.5), timed("av1", true, 1.8)} // Firefox
	swAV1 := []proto.DecoderInfo{timed("h264", true, 1.5), timed("hevc", true, 2.0), timed("av1", false, 0.9)}
	old := []proto.DecoderInfo{{Family: "h264", HW: true}, {Family: "hevc", HW: true}, {Family: "av1", HW: true}}
	onlySWAV1 := []proto.DecoderInfo{timed("av1", false, 5)} // this sandbox's Chromium
	// Chrome with the decoder setting Prefer software: nothing reported as
	// hardware-decoded, HEVC (no software decoder, so decoded in hardware
	// anyway) untimed, the others timed in software.
	preferSW := []proto.DecoderInfo{timed("h264", false, 3.5), {Family: "hevc"}, timed("av1", false, 2.5)}

	for _, c := range []struct {
		name     string
		caps     *media.Caps
		av1      string
		w, h     int
		decoders []proto.DecoderInfo
		prefs    proto.Prefs
		forced   string
		want     string
	}{
		{"RDNA3 host, AV1 faster on the client, default policy", rdna3, "", 2560, 1440, fastAV1, proto.Prefs{}, "", "hevc_amf"},
		{"RDNA3 host, AV1 faster, av1 faster, aligned size", rdna3, AV1Faster, 2560, 1440, fastAV1, proto.Prefs{}, "", "av1_amf"},
		{"RDNA3 host, AV1 faster, av1 faster, 1920x1080 (pads)", rdna3, AV1Faster, 1920, 1080, fastAV1, proto.Prefs{}, "", "hevc_amf"},
		{"RDNA3 host, AV1 as fast as HEVC", rdna3, AV1Faster, 2560, 1440, evenAV1, proto.Prefs{}, "", "hevc_amf"},
		{"RDNA2 host (no AV1 encoder), av1 faster", rdna2, AV1Faster, 2560, 1440, fastAV1, proto.Prefs{}, "", "hevc_amf"},
		{"RTX 40 host, AV1 faster, default policy", rtx40, "", 1920, 1080, fastAV1, proto.Prefs{}, "", "hevc_nvenc"},
		{"RTX 40 host, AV1 faster, av1 faster", rtx40, AV1Faster, 1920, 1080, fastAV1, proto.Prefs{}, "", "av1_nvenc"},
		{"RTX 30 host (no AV1 encoder), av1 faster", rtx30, AV1Faster, 1920, 1080, fastAV1, proto.Prefs{}, "", "hevc_nvenc"},
		{"slow HEVC decoder on the client: H.264", rtx40, "", 1920, 1080, slowHEVC, proto.Prefs{}, "", "h264_nvenc"},
		{"slow HEVC decoder, RDNA2 host: H.264", rdna2, "", 1920, 1080, slowHEVC, proto.Prefs{}, "", "h264_amf"},
		{"no HEVC in the browser: AV1 before H.264", rtx40, "", 1920, 1080, noHEVC, proto.Prefs{}, "", "av1_nvenc"},
		{"no HEVC in the browser, RTX 30: H.264", rtx30, "", 1920, 1080, noHEVC, proto.Prefs{}, "", "h264_nvenc"},
		{"AV1 decodes in software only: hardware HEVC", rtx40, AV1Faster, 1920, 1080, swAV1, proto.Prefs{}, "", "hevc_nvenc"},
		{"client before step 4.2: HEVC", rtx40, AV1Faster, 1920, 1080, old, proto.Prefs{}, "", "hevc_nvenc"},
		{"client asks for AV1, default policy", rtx40, "", 1920, 1080, fastAV1, proto.Prefs{Codec: "av1"}, "", "av1_nvenc"},
		{"client asks for H.264", rtx40, AV1Faster, 1920, 1080, fastAV1, proto.Prefs{Codec: "h264"}, "", "h264_nvenc"},
		{"host forces h264_amf", rdna3, AV1Faster, 2560, 1440, fastAV1, proto.Prefs{}, "h264_amf", "h264_amf"},
		{"software decode of AV1 only", rtx40, "", 1920, 1080, onlySWAV1, proto.Prefs{}, "", "av1_nvenc"},
		{"Prefer software (Chrome), RDNA3 host: H.264", rdna3, "", 2560, 1440, preferSW, proto.Prefs{}, "", "h264_amf"},
		{"Prefer software (Chrome), RTX 40 host: H.264", rtx40, "", 1920, 1080, preferSW, proto.Prefs{}, "", "h264_nvenc"},
		{"Prefer software, client asks for HEVC", rtx40, "", 1920, 1080, preferSW, proto.Prefs{Codec: "hevc"}, "", "hevc_nvenc"},
		{"software decode of AV1 only, software host", &media.Caps{Encoders: sw}, "", 960, 540, onlySWAV1, proto.Prefs{}, "", "libsvtav1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Capture: "test", TestWidth: c.w, TestHeight: c.h, Encoder: c.forced, AV1: c.av1}
			cfg.Defaults()
			var logs bytes.Buffer
			s := &Session{
				a:     &Agent{cfg: cfg, caps: c.caps, inj: input.NewInjector(nil)},
				hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: c.decoders},
				ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
				log: slog.New(slog.NewTextHandler(&logs, nil)),
			}
			p, err := s.buildParams(c.prefs)
			if err != nil {
				t.Fatal(err)
			}
			if p.Encoder.Name != c.want {
				t.Fatalf("encoder %s, want %s; log:\n%s", p.Encoder.Name, c.want, logs.String())
			}
			// One "codec choice" line with the client's decoders, and not
			// again for a restart that chooses the same.
			if n := strings.Count(logs.String(), `msg="codec choice"`); n != 1 || !strings.Contains(logs.String(), "decoders="+strconv.Quote(decoderSummary(c.decoders))) &&
				!strings.Contains(logs.String(), "decoders="+decoderSummary(c.decoders)+"\n") {
				t.Fatalf("%d codec choice lines; log:\n%s", n, logs.String())
			}
			if _, err := s.buildParams(c.prefs); err != nil || strings.Count(logs.String(), `msg="codec choice"`) != 1 {
				t.Fatalf("restart logged the choice again (%v):\n%s", err, logs.String())
			}
		})
	}
}

// TestCodecSelectionFailover: an encoder that failed in the session is out of
// the choice; the next family in the tier takes over by the same rule.
func TestCodecSelectionFailover(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{
		{Name: "av1_nvenc", Family: "av1", Vendor: "nvidia", HW: true},
		{Name: "hevc_nvenc", Family: "hevc", Vendor: "nvidia", HW: true},
		{Name: "h264_nvenc", Family: "h264", Vendor: "nvidia", HW: true},
	}}
	cfg := &Config{Capture: "test", TestWidth: 1920, TestHeight: 1080}
	cfg.Defaults()
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
		hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{timed("h264", true, 1.5), timed("hevc", true, 2), timed("av1", true, 1.9)}},
		ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, want := range []string{"hevc_nvenc", "av1_nvenc", "h264_nvenc"} {
		p, err := s.buildParams(proto.Prefs{})
		if err != nil || p.Encoder.Name != want {
			t.Fatalf("got %s (%v), want %s", p.Encoder.Name, err, want)
		}
		s.tried[want] = true
	}
	if _, err := s.buildParams(proto.Prefs{}); err == nil {
		t.Fatal("no encoder left, but no error")
	}
}

// TestCodecWarningOnce: a codec setting no encoder of the host serves is
// warned about once, not again at each encoder restart (buildParams runs for
// every one: the FFmpeg path restarts for each bitrate change); again after
// the client asked for a codec that works and then for the missing one.
func TestCodecWarningOnce(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{
		{Name: "libx264", Family: "h264", Vendor: "software"},
		{Name: "libsvtav1", Family: "av1", Vendor: "software"},
	}}
	cfg := &Config{Capture: "test", TestWidth: 960, TestHeight: 540}
	cfg.Defaults()
	ctrl := &ctrlRecorder{}
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
		hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{timed("h264", true, 1.5), timed("hevc", true, 2), timed("av1", true, 1.9)}},
		ctrl:  ctrl, tried: map[string]bool{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	warnings := func() int {
		n := 0
		for _, m := range ctrl.notices(t) {
			if strings.Contains(m, "not available end-to-end") {
				n++
			}
		}
		return n
	}
	for i, c := range []struct {
		codec string
		want  int
	}{
		{"hevc", 1}, {"hevc", 0}, {"hevc", 0}, // the session's start, then two restarts
		{"h264", 0}, {"hevc", 1}, {"auto", 0}, {"hevc", 1},
	} {
		if _, err := s.buildParams(proto.Prefs{Codec: c.codec}); err != nil {
			t.Fatal(err)
		}
		if n := warnings(); n != c.want {
			t.Fatalf("step %d (codec %s): %d warnings, want %d", i, c.codec, n, c.want)
		}
	}
}

// TestCodecSelectionSoftwareEncode: software encoding keeps its order
// (libx264 first, the cheapest to encode) whatever the client decodes faster,
// on a host without hardware encoders and on one whose hardware encoders all
// failed in the session.
func TestCodecSelectionSoftwareEncode(t *testing.T) {
	sw := []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"}, {Name: "libsvtav1", Family: "av1", Vendor: "software"}}
	hw := []media.EncoderInfo{
		{Name: "av1_nvenc", Family: "av1", Vendor: "nvidia", HW: true},
		{Name: "hevc_nvenc", Family: "hevc", Vendor: "nvidia", HW: true},
		{Name: "h264_nvenc", Family: "h264", Vendor: "nvidia", HW: true},
	}
	for _, c := range []struct {
		name  string
		encs  []media.EncoderInfo
		tried []string
	}{
		{"software host", sw, nil},
		{"RTX 40 host, every hardware encoder failed", append(hw, sw...), []string{"av1_nvenc", "hevc_nvenc", "h264_nvenc"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Capture: "test", TestWidth: 2560, TestHeight: 1440, AV1: AV1Faster}
			cfg.Defaults()
			s := &Session{
				a:     &Agent{cfg: cfg, caps: &media.Caps{Encoders: c.encs}, inj: input.NewInjector(nil)},
				hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{timed("h264", true, 2.0), timed("av1", true, 1.4)}},
				ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			for _, e := range c.tried {
				s.tried[e] = true
			}
			p, err := s.buildParams(proto.Prefs{})
			if err != nil || p.Encoder.Name != "libx264" {
				t.Fatalf("got %s (%v), want libx264", p.Encoder.Name, err)
			}
		})
	}
}

func TestDecoderSummary(t *testing.T) {
	got := decoderSummary([]proto.DecoderInfo{timed("hevc", true, 2.104), {Family: "h264"}, timed("av1", false, 4.5)})
	if want := "hevc:hw:2.10ms@1920x1080 h264:sw:- av1:sw:4.50ms@1920x1080"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

// TestHelloTiming: the hello's decoders as the client sends them (step 4.2),
// and as clients before it did.
func TestHelloTiming(t *testing.T) {
	var h proto.Hello
	if err := json.Unmarshal([]byte(`{"t":"hello","v":2,"decoders":[{"family":"av1","hw":false,"timing":{"ms":4.12,"w":1920,"h":1080,"n":7,"accel":"no-preference"}},{"family":"h264","hw":true}]}`), &h); err != nil {
		t.Fatal(err)
	}
	if tm := h.Decoders[0].Timing; tm == nil || tm.Ms != 4.12 || tm.W != 1920 || tm.H != 1080 || tm.N != 7 || tm.Accel != "no-preference" || h.Decoders[1].Timing != nil {
		t.Fatalf("%+v", h.Decoders)
	}
	// Scaled down by pixel count, never up (decodeEstimate).
	if got := decodeEstimate(h.Decoders[0], 960, 540); got != 4.12/4 {
		t.Fatalf("960x540 estimate %v", got)
	}
	if got := decodeEstimate(h.Decoders[0], 3840, 2160); got != 4.12 {
		t.Fatalf("4K estimate %v", got)
	}
}

func TestLoadConfigAV1(t *testing.T) {
	for _, c := range []struct {
		json, want string
		ok         bool
	}{
		{`{}`, AV1Fallback, true},
		{`{"av1":"fallback"}`, AV1Fallback, true},
		{`{"av1":"faster"}`, AV1Faster, true},
		{`{"av1":"always"}`, "", false},
	} {
		p := filepath.Join(t.TempDir(), "host.json")
		if err := os.WriteFile(p, []byte(c.json), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(p)
		if (err == nil) != c.ok || (c.ok && cfg.av1() != c.want) {
			t.Fatalf("%s: %v %+v", c.json, err, cfg)
		}
	}
}
