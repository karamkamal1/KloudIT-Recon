package host

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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

// TestEncoderFailureFallback drives the failure handler with the video
// manager's failure events: the first start failure of h264_amf retries it
// with the low-latency usage (AMF issue #410) and the next one excludes it,
// while a generation that fails after going live keeps its usage; an encoder
// without another usage is excluded when it fails after another failure. The
// client's adaptive bitrate setting reaches the encoder.
func TestEncoderFailureFallback(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{
		{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
		{Name: "h264_amf", Family: "h264", Vendor: "amd", HW: true},
		{Name: "libx264", Family: "h264", Vendor: "software"},
	}}
	cfg := &Config{Capture: "test", TestWidth: 1280, TestHeight: 720, DefaultFPS: 60, MaxFPS: 240, DefaultKbps: 20000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the handler's delayed restarts do nothing
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
		hello: proto.Hello{Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}, {Family: "hevc", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{},
		ctx: ctx, cancel: cancel,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	next := func(prefs proto.Prefs) media.Params {
		t.Helper()
		p, err := s.buildParams(prefs)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	fail := func(p media.Params, live bool) {
		if live {
			s.failures = 0 // the generation went live (videoEvents on its config)
		}
		s.handleEncoderFailure(media.VideoEvent{Err: errors.New("encoder " + p.Encoder.Name + " exited"), Failed: &p, Live: live})
	}
	h264 := proto.Prefs{Codec: "h264"}
	p := next(h264)
	if p.Encoder.Name != "h264_amf" || p.Usage != "" || !p.Adaptive {
		t.Fatalf("start: %s usage %q adaptive %v", p.Encoder.Name, p.Usage, p.Adaptive)
	}
	fail(p, true)
	if p = next(h264); p.Encoder.Name != "h264_amf" || p.Usage != "" {
		t.Fatalf("after a failure while live: %s usage %q, want h264_amf with its usage", p.Encoder.Name, p.Usage)
	}
	fail(p, false)
	if p = next(h264); p.Encoder.Name != "h264_amf" || p.Usage != "lowlatency" {
		t.Fatalf("after a start failure: %s usage %q, want h264_amf lowlatency", p.Encoder.Name, p.Usage)
	}
	fail(p, false)
	if p = next(h264); p.Encoder.Name != "libx264" || p.Usage != "" {
		t.Fatalf("after the retry failed: %s usage %q, want libx264", p.Encoder.Name, p.Usage)
	}

	s.failures = 0
	auto := proto.Prefs{}
	if p = next(auto); p.Encoder.Name != "hevc_amf" {
		t.Fatalf("auto: %s, want hevc_amf", p.Encoder.Name)
	}
	fail(p, false)
	if p = next(auto); p.Encoder.Name != "hevc_amf" || p.Usage != "" {
		t.Fatalf("after one failure: %s usage %q, want hevc_amf again", p.Encoder.Name, p.Usage)
	}
	fail(p, false)
	if p = next(auto); p.Encoder.Name != "libx264" {
		t.Fatalf("after two failures: %s, want libx264", p.Encoder.Name)
	}

	off := false
	if next(proto.Prefs{Adaptive: &off}).Adaptive {
		t.Fatal("adaptive bitrate off in the client, on in the encoder parameters")
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
