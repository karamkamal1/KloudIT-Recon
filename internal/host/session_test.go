package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// TestVideoHeader checks both meanings of send_us: v1 clients keep the
// encoder-out time (their congestion detection, acks and latency readout
// measure from it), v2 clients get the transport hand-off plus the extension.
func TestVideoHeader(t *testing.T) {
	f := &media.Frame{Gen: 3, Seq: 7, Key: true, PtsUs: 1000, CaptureUs: 900, EncodeDoneUs: 5000}
	const now = 7500 // frame waited 2.5 ms in the host queue

	h, ext := videoHeader(f, 1, now)
	if h.SendUs != f.EncodeDoneUs || h.Flags != proto.FrameFlagKey || !ext.Empty() {
		t.Fatalf("v1: send %d flags %#x ext %v, want send = encodeDone %d, key only", h.SendUs, h.Flags, !ext.Empty(), f.EncodeDoneUs)
	}

	h, ext = videoHeader(f, proto.HelloVersionFrameExt, now)
	done, _ := ext.Get(proto.ExtEncodeDoneUs)
	capture, _ := ext.Get(proto.ExtCaptureUs)
	if h.SendUs != now || h.Flags != proto.FrameFlagKey|proto.FrameFlagExt || done != f.EncodeDoneUs || capture != f.CaptureUs {
		t.Fatalf("v2: send %d flags %#x encodeDone %d capture %d", h.SendUs, h.Flags, done, capture)
	}

	f.CaptureUs = 0
	_, ext = videoHeader(f, proto.HelloVersionFrameExt, now)
	if _, ok := ext.Get(proto.ExtCaptureUs); ok {
		t.Fatal("capture tag sent without a capture stamp")
	}
}

// TestHostStages checks the host's own stage window: only acknowledged frames
// (once each), 10 s by ack time, and the client's percentile definition.
func TestHostStages(t *testing.T) {
	var h hostStages
	var now uint64
	for i := uint64(0); i < 100; i++ {
		now = 1_000_000 + i*200_000
		f := &media.Frame{Gen: 1, Seq: uint32(i), EncodeDoneUs: now, CaptureUs: now - (i+1)*100}
		if i%10 == 0 {
			f.CaptureUs = 0
		}
		h.sentFrame(f, now+60)
		if i%2 == 1 {
			continue // not acknowledged (dropped by the client)
		}
		h.acked(1, uint32(i), now+1000)
		h.acked(1, uint32(i), now+1000) // duplicate
	}
	h.acked(2, 98, now+1000) // other generation
	// Window: frames acknowledged in the last 10 s: even i = 50..98 (25 frames),
	// 20 with a capture stamp ((i+1)/10 ms: 5.3, 5.5, ... 9.9 without i%10 == 0).
	c, q := h.summary(now + 1000)
	if c != "7.7/9.9/9.9 n=20" || q != "0.1/0.1/0.1 n=25" {
		t.Fatalf("capture %q queue %q", c, q)
	}
}

// A path migration (the client switching networks, or its NAT rebinding)
// gives the connection a new media controller at the defaults; the session
// target must be back on it by the next frame.
func TestMediaTargetSurvivesMigration(t *testing.T) {
	cert, err := tlsutil.SelfSigned([]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	const alpn = "kloudit-test"
	ln, err := quic.ListenAddr("127.0.0.1:0",
		&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}},
		transport.QUICConfig(transport.WithCongestion(transport.CongestionMedia)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	udp := func() *quic.Transport {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		tr := &quic.Transport{Conn: pc}
		t.Cleanup(func() { tr.Close() })
		return tr
	}
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{alpn}}
	client, err := udp().Dial(ctx, ln.Addr(), clientTLS, transport.QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "")
	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	s := &Session{
		a: &Agent{hostClock: media.NewClock()}, c: transport.FromQUIC(server), ctx: ctx, frameQ: make(chan *media.Frame, 6),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	go s.frameSender()
	s.setCongestionTarget(media.Params{BitrateKbps: 50_000, FPS: 120})
	before := transport.MediaControl(s.c)
	if before == nil {
		t.Fatal("no media controller")
	}
	want := before.TargetBitrate()
	if want < 50_000_000 {
		t.Fatalf("target %d bit/s is below the video bitrate", want)
	}

	path, err := client.AddPath(udp())
	if err != nil {
		t.Fatal(err)
	}
	if err := path.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := path.Switch(); err != nil {
		t.Fatal(err)
	}
	// The server migrates on the first non-probing packet from the new address.
	for transport.MediaControl(s.c) == before {
		if ctx.Err() != nil {
			t.Fatal("the server never migrated")
		}
		_ = client.SendDatagram([]byte{0})
		time.Sleep(10 * time.Millisecond)
	}

	s.frameQ <- &media.Frame{Data: []byte("frame")}
	st, err := client.AcceptUniStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(st); err != nil {
		t.Fatal(err)
	}
	if got := transport.MediaControl(s.c).TargetBitrate(); got != want {
		t.Fatalf("target after migration = %d bit/s, want %d", got, want)
	}
}

// ctrlRecorder is a control stream that keeps what the host writes.
type ctrlRecorder struct{ bytes.Buffer }

func (*ctrlRecorder) Read([]byte) (int, error)         { return 0, io.EOF }
func (*ctrlRecorder) Close() error                     { return nil }
func (*ctrlRecorder) CancelRead()                      {}
func (*ctrlRecorder) CancelWrite()                     {}
func (*ctrlRecorder) SetReadDeadline(time.Time) error  { return nil }
func (*ctrlRecorder) SetWriteDeadline(time.Time) error { return nil }

// notices returns the texts of the notices written so far.
func (r *ctrlRecorder) notices(t *testing.T) []string {
	t.Helper()
	var out []string
	for r.Len() > 0 {
		b, err := proto.ReadMsg(&r.Buffer, proto.MaxControlMsg)
		if err != nil {
			t.Fatal(err)
		}
		var n proto.Notice
		json.Unmarshal(b, &n)
		if n.T == "notice" {
			out = append(out, n.Msg)
		}
	}
	return out
}

// TestAlignmentGuard checks the encoder choice for an encoder that pads the
// coded picture (step 1.7): AV1 on RDNA3 (probed alignment 64x16) gives way
// to HEVC at 1920x1080, asked for by the client or chosen automatically,
// with one notice; at aligned sizes AV1 stays. Without HEVC end-to-end H.264
// takes over; with nothing else, or forced in the host config, AV1 stays
// (the client crops).
func TestAlignmentGuard(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{
		{Name: "av1_amf", Family: "av1", Vendor: "amd", HW: true},
		{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
		{Name: "h264_amf", Family: "h264", Vendor: "amd", HW: true},
		{Name: "libx264", Family: "h264", Vendor: "software"},
	}}
	caps.SetAlignment("av1_amf", media.Alignment{W: 64, H: 16, ProbeW: 1920, ProbeH: 1080, CodedW: 1920, CodedH: 1082})
	all := []proto.DecoderInfo{{Family: "av1", HW: true}, {Family: "hevc", HW: true}, {Family: "h264", HW: true}}
	newSession := func(w, h int, decoders []proto.DecoderInfo, forced string) (*Session, *ctrlRecorder) {
		cfg := &Config{Capture: "test", TestWidth: w, TestHeight: h, Encoder: forced}
		cfg.Defaults()
		rec := &ctrlRecorder{}
		return &Session{
			a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
			hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: decoders},
			ctrl:  rec, tried: map[string]bool{},
			log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, rec
	}
	av1 := proto.Prefs{Codec: "av1"}
	const notice = "AV1 on this GPU needs 64×16-aligned sizes; using HEVC"
	for _, c := range []struct {
		name     string
		w, h     int
		decoders []proto.DecoderInfo
		prefs    proto.Prefs
		forced   string
		want     string
		notice   string
	}{
		{"forced AV1 at 1920x1080", 1920, 1080, all, av1, "", "hevc_amf", notice},
		// An encoder forced in host.json is kept (VideoConfig announces the crop).
		{"host forces av1_amf at 1920x1080", 1920, 1080, all, proto.Prefs{}, "av1_amf", "av1_amf", ""},
		{"forced AV1 at 2560x1440", 2560, 1440, all, av1, "", "av1_amf", ""},
		{"forced AV1 at 3840x2160", 3840, 2160, all, av1, "", "av1_amf", ""},
		{"forced AV1 at 1280x720", 1280, 720, all, av1, "", "av1_amf", ""},
		{"forced AV1 at 3440x1440", 3440, 1440, all, av1, "", "hevc_amf", notice},
		{"auto, AV1 the only hardware decoder", 1920, 1080, []proto.DecoderInfo{{Family: "av1", HW: true}, {Family: "hevc"}, {Family: "h264"}},
			proto.Prefs{}, "", "hevc_amf", notice},
		{"no HEVC in the browser", 1920, 1080, []proto.DecoderInfo{{Family: "av1", HW: true}, {Family: "h264", HW: true}}, av1, "", "h264_amf",
			"AV1 on this GPU needs 64×16-aligned sizes; using H.264"},
		{"only AV1 in the browser", 1920, 1080, []proto.DecoderInfo{{Family: "av1", HW: true}}, av1, "", "av1_amf", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, rec := newSession(c.w, c.h, c.decoders, c.forced)
			p, err := s.buildParams(c.prefs)
			if err != nil {
				t.Fatal(err)
			}
			got := rec.notices(t)
			if p.Encoder.Name != c.want || (c.notice == "") != (len(got) == 0) || (c.notice != "" && (len(got) != 1 || got[0] != c.notice)) {
				t.Fatalf("encoder %s, notices %q; want %s, %q", p.Encoder.Name, got, c.want, c.notice)
			}
			// Restarts at the same size do not repeat the notice.
			if p, _ = s.buildParams(c.prefs); p.Encoder.Name != c.want || len(rec.notices(t)) != 0 {
				t.Fatalf("restart: encoder %s or a repeated notice", p.Encoder.Name)
			}
		})
	}

	// A session that switches to an aligned size gets AV1 back, and the
	// notice again when it returns to 1920x1080.
	s, rec := newSession(2560, 1440, all, "")
	for i, sz := range [][2]int{{1920, 1080}, {1280, 720}, {1920, 1080}} {
		p, err := s.buildParams(proto.Prefs{Codec: "av1", Width: sz[0], Height: sz[1]})
		if err != nil {
			t.Fatal(err)
		}
		want, notices := "av1_amf", 0
		if sz[0] == 1920 {
			want, notices = "hevc_amf", 1
		}
		if p.Encoder.Name != want || len(rec.notices(t)) != notices {
			t.Fatalf("step %d %dx%d: encoder %s", i, sz[0], sz[1], p.Encoder.Name)
		}
	}
}

// TestAMFCaptureBackend checks the opt-in AMD Direct Capture backend (step
// 1.6): only configured, never automatic; used only with an AMF encoder,
// without a cursor in the video and for a monitor on DXGI adapter 0, else
// ddagrab with one log line per change; after a failure the session stays on
// ddagrab.
func TestAMFCaptureBackend(t *testing.T) {
	caps := &media.Caps{Filters: map[string]bool{"ddagrab": true, "gfxcapture": true, "vsrc_amf": true, "select": true},
		Encoders: []media.EncoderInfo{
			{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
			{Name: "libx264", Family: "h264", Vendor: "software"},
		}}
	var logs bytes.Buffer
	newSession := func(capture string) *Session {
		cfg := &Config{Capture: capture}
		cfg.Defaults()
		logs.Reset()
		return &Session{
			a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
			hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}, {Family: "h264", HW: true}}},
			ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
			log: slog.New(slog.NewTextHandler(&logs, nil)),
		}
	}
	fallbacks := func() int { return strings.Count(logs.String(), "not used, capturing with ddagrab") }

	// Never chosen automatically, only when configured.
	if b := newSession("auto").a.backendFor(proto.Prefs{}); b != "ddagrab" {
		t.Fatalf("auto picks %s", b)
	}
	if b := newSession("amf").a.backendFor(proto.Prefs{}); b != "amf" {
		t.Fatalf("configured amf: %s", b)
	}

	hevc, x264 := caps.Encoders[0], caps.Encoders[1]
	mon := platform.Monitor{Index: 1, W: 2560, H: 1440, DXGIOutput: 2}
	for _, c := range []struct {
		name    string
		enc     media.EncoderInfo
		cursor  bool
		mon     platform.Monitor
		filters bool
		want    string // "" = usable
	}{
		{"AMF encoder", hevc, false, mon, true, ""},
		{"software encoder", x264, false, mon, true, "only feeds AMF encoders, not libx264"},
		{"cursor in the video", hevc, true, mon, true, "cursor"},
		{"monitor on another adapter", hevc, false, platform.Monitor{Index: 1, DXGIOutput: -1}, true, "not output 0-8 of DXGI adapter 0"},
		{"FFmpeg without vsrc_amf", hevc, false, mon, false, "vsrc_amf"},
	} {
		caps.Filters["vsrc_amf"] = c.filters
		got := newSession("amf").a.amfCaptureBlocker(c.enc, c.cursor, c.mon)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	caps.Filters["vsrc_amf"] = true

	// The switch from ddagrab to amf keeps the monitor's output index.
	s := newSession("amf")
	dda := media.Params{Source: media.Source{Backend: "ddagrab", Output: 2, NativeW: 2560, NativeH: 1440}, Encoder: hevc}
	p := dda
	s.useAMFCapture(&p, mon)
	if p.Source != (media.Source{Backend: "amf", Output: 2, NativeW: 2560, NativeH: 1440}) || fallbacks() != 0 {
		t.Fatalf("source %+v, log %s", p.Source, logs.String())
	}
	// A fallback is logged once while its reason stays, again when it changes.
	for i, c := range []struct {
		cursor bool
		logs   int
	}{{true, 1}, {true, 1}, {false, 1}, {true, 2}} {
		p = dda
		p.DrawCursor = c.cursor
		s.useAMFCapture(&p, mon)
		if wantAMF := !c.cursor; (p.Source.Backend == "amf") != wantAMF || fallbacks() != c.logs {
			t.Fatalf("step %d: source %s, %d fallback lines", i, p.Source.Backend, fallbacks())
		}
	}

	// A client that wants the cursor in the video keeps a whole buildParams
	// on ddagrab, with the reason in the log once.
	s = newSession("amf")
	for i := 0; i < 2; i++ {
		p, err := s.buildParams(proto.Prefs{Cursor: "video"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Source.Backend != "ddagrab" || p.Encoder.Name != "hevc_amf" || fallbacks() != 1 {
			t.Fatalf("buildParams: %+v, log %s", p, logs.String())
		}
	}

	// A failed amf generation moves the rest of the session to ddagrab.
	s = newSession("amf")
	s.noteCaptureFailure(media.VideoEvent{Err: errors.New("encoder hevc_amf exited: Failed to initialize capture component: 3"),
		Failed: media.Params{Source: media.Source{Backend: "ddagrab"}}})
	p = dda
	if s.useAMFCapture(&p, mon); p.Source.Backend != "amf" {
		t.Fatal("a ddagrab failure turned AMD Direct Capture off")
	}
	s.noteCaptureFailure(media.VideoEvent{Err: errors.New("encoder hevc_amf exited: Failed to initialize capture component: 3"),
		Failed: media.Params{Source: media.Source{Backend: "amf"}}})
	p = dda
	if s.useAMFCapture(&p, mon); p.Source.Backend != "ddagrab" || !strings.Contains(logs.String(), "AMD Direct Capture failed") ||
		!strings.Contains(logs.String(), "failed earlier in this session") {
		t.Fatalf("after a failure: %s, log %s", p.Source.Backend, logs.String())
	}
}

// TestProbeSample checks that "recon-host probe" prints command lines for the
// session the agent builds (buildParams) when a browser client at its default
// settings (stream.js DEFAULTS) streams with that encoder, whatever the host's
// capture setting: the test pattern at testWidth x testHeight with its
// padding, ddagrab, gfxcapture or x11grab of the first monitor, AMD Direct
// Capture only where the agent would use it.
func TestProbeSample(t *testing.T) {
	caps := &media.Caps{Filters: map[string]bool{"ddagrab": true, "gfxcapture": true, "vsrc_amf": true},
		Encoders: []media.EncoderInfo{
			{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
			{Name: "h264_nvenc", Family: "h264", Vendor: "nvidia", HW: true},
			{Name: "libx264", Family: "h264", Vendor: "software"},
		}}
	browser := proto.Prefs{Codec: "auto", BitrateKbps: 30000, FPS: 60, Cursor: "local", Quality: "balanced"}
	newConfig := func(capture string, drawCursor bool) *Config {
		cfg := &Config{Capture: capture, TestWidth: 1600, TestHeight: 900, TestPad: 8, MaxFPS: 50, DrawCursor: drawCursor}
		cfg.Defaults()
		return cfg
	}
	for _, capture := range []string{"auto", "ddagrab", "gfxcapture", "amf", "x11grab", "test"} {
		for _, drawCursor := range []bool{false, true} {
			cfg := newConfig(capture, drawCursor)
			sample := ProbeSample(cfg, caps)
			for _, enc := range caps.Encoders {
				cfg.Encoder = enc.Name
				s := &Session{
					a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
					hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}, {Family: "h264", HW: true}}},
					ctrl:  &ctrlRecorder{}, tried: map[string]bool{},
					log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				}
				want, err := s.buildParams(browser)
				if err != nil {
					t.Fatal(err)
				}
				if got := sample(enc); got != want {
					t.Errorf("capture %s, drawCursor %v, %s:\nprobe %+v\nagent %+v", capture, drawCursor, enc.Name, got, want)
				}
			}
		}
	}
	// capture "amf" on a monitor of DXGI adapter 0: AMD Direct Capture for the
	// AMF encoder only, on Windows (elsewhere the video carries the cursor).
	sample := (&Agent{cfg: newConfig("amf", false), caps: caps}).probeSample(platform.Monitor{W: 2560, H: 1440, Hz: 144, DXGIOutput: 1})
	if amf, x264 := sample(caps.Encoders[0]).Source, sample(caps.Encoders[2]).Source; (amf.Backend == "amf") != (runtime.GOOS == "windows") ||
		amf.Output != 1 || x264 != (media.Source{Backend: "ddagrab", Output: 1, NativeW: 2560, NativeH: 1440}) {
		t.Fatalf("capture amf: %s %+v, %s %+v", caps.Encoders[0].Name, amf, caps.Encoders[2].Name, x264)
	}
	// The Linux default is the configured test pattern, not a fixed 1920x1080.
	if p := ProbeSample(newConfig("test", false), caps)(caps.Encoders[2]); p.Source != (media.Source{Backend: "test", NativeW: 1600, NativeH: 900}) ||
		p.TestPad != 8 || p.FPS != 50 || p.BitrateKbps != 30000 || p.Quality != "balanced" {
		t.Fatalf("test pattern sample %+v", p)
	}
}
