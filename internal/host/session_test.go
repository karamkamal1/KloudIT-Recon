package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
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
	const notice = "AV1 on this GPU needs 64x16-aligned sizes; using HEVC"
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
			"AV1 on this GPU needs 64x16-aligned sizes; using H.264"},
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
