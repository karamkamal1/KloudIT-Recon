package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// manager's failure events. Only an encoder failing to start counts against
// it: a capture outage (UAC prompt, lock screen: the first restart fails
// after a live generation, then every restart until it ends) and a live
// generation that fails keep the encoder and its usage. An encoder fault
// retries h264_amf once with the low-latency usage (AMF issue #410) and
// otherwise excludes an encoder at its second one, counted per encoder and
// reset when a generation goes live. The client's adaptive bitrate setting
// reaches the encoder.
func TestEncoderFailureFallback(t *testing.T) {
	caps := &media.Caps{Encoders: []media.EncoderInfo{
		{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
		{Name: "av1_amf", Family: "av1", Vendor: "amd", HW: true},
		{Name: "h264_amf", Family: "h264", Vendor: "amd", HW: true},
		{Name: "libx264", Family: "h264", Vendor: "software"},
	}}
	cfg := &Config{Capture: "test", TestWidth: 1280, TestHeight: 720, DefaultFPS: 60, MaxFPS: 240, DefaultKbps: 20000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the handler's delayed restarts do nothing
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil)},
		hello: proto.Hello{Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}, {Family: "hevc", HW: true}, {Family: "av1", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// expect builds the next generation's parameters and checks its encoder and usage.
	expect := func(prefs proto.Prefs, enc, usage, after string) media.Params {
		t.Helper()
		p, err := s.buildParams(prefs)
		if err != nil {
			t.Fatal(err)
		}
		if p.Encoder.Name != enc || p.Usage != usage {
			t.Fatalf("after %s: %s usage %q, want %s usage %q", after, p.Encoder.Name, p.Usage, enc, usage)
		}
		return p
	}
	// fail feeds the failure of p's generation to the handler. live: it had
	// gone live; fault: the encoder failed (else e.g. its capture source).
	fail := func(p media.Params, live, fault bool) {
		if live {
			s.encoderLive() // videoEvents on the generation's config
		}
		s.handleEncoderFailure(media.VideoEvent{Err: errors.New("encoder " + p.Encoder.Name + " exited"), Failed: &p, Live: live, EncoderFault: fault})
	}
	auto, h264 := proto.Prefs{}, proto.Prefs{Codec: "h264"}

	// Capture outage of about 4.5 s: the live generation loses the desktop,
	// then five restarts fail to capture (backing off 300 ms more each time).
	p := expect(auto, "hevc_amf", "", "start")
	fail(p, true, false)
	for i := range 5 {
		p = expect(auto, "hevc_amf", "", "capture failures")
		fail(p, false, false)
		if i == 4 && s.failures != 6 {
			t.Fatalf("%d failures in a row, want 6", s.failures)
		}
	}
	expect(auto, "hevc_amf", "", "a capture outage")
	s.encoderLive()

	// h264_amf: neither a capture failure nor a failure while live is the
	// init failure the low-latency usage works around.
	p = expect(h264, "h264_amf", "", "start")
	fail(p, false, false)
	p = expect(h264, "h264_amf", "", "a capture failure")
	fail(p, true, true)
	p = expect(h264, "h264_amf", "", "an encoder failure while live")
	fail(p, false, true)
	p = expect(h264, "h264_amf", "lowlatency", "a start failure")
	fail(p, false, true)
	expect(h264, "libx264", "", "the retry failed")

	// Counted per encoder: av1_amf gets two tries after hevc_amf used up its
	// own, although every start in between failed.
	p = expect(auto, "hevc_amf", "", "start")
	fail(p, false, true)
	p = expect(auto, "hevc_amf", "", "one encoder failure")
	fail(p, false, true)
	p = expect(auto, "av1_amf", "", "two hevc_amf failures")
	fail(p, false, true)
	p = expect(auto, "av1_amf", "", "one av1_amf failure")
	fail(p, false, true)
	expect(auto, "libx264", "", "two av1_amf failures (h264_amf excluded above)")

	// A generation going live resets the count (as a settings change does).
	s.tried, s.usage = map[string]bool{}, map[string]string{}
	s.encoderLive()
	p = expect(auto, "hevc_amf", "", "a reset")
	fail(p, false, true)
	s.encoderLive()
	p = expect(auto, "hevc_amf", "", "a failure, then a live generation")
	fail(p, false, true)
	p = expect(auto, "hevc_amf", "", "a failure since the last live generation")
	fail(p, false, true)
	expect(auto, "av1_amf", "", "two failures since the last live generation")

	off := false
	if p, err := s.buildParams(proto.Prefs{Adaptive: &off}); err != nil || p.Adaptive {
		t.Fatalf("adaptive bitrate off in the client, on in the encoder parameters (%v)", err)
	}
	if p, err := s.buildParams(auto); err != nil || !p.Adaptive {
		t.Fatalf("adaptive bitrate on by default, off in the encoder parameters (%v)", err)
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

// lockedLog collects a session's log lines for TestQueueOverflowEscalates.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *lockedLog) lines(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// TestQueueOverflowEscalates: a frame-queue overflow within 2 s of a bitrate
// cut is not swallowed by the cut's rate limit. After a client congestion
// report (overlapped restart: the old generation streams on at the old
// bitrate while the new one starts) the old generation stops at once and the
// starting one takes over; with nothing starting (the overflow dropped the
// newest generation's key frame) a new generation starts at once. Neither
// cuts the bitrate again, and both restarts keep the lowered bitrate.
func TestQueueOverflowEscalates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in encoder is a shell script")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	caps, err := media.Probe(context.Background(), ff, nil)
	if err != nil {
		t.Skipf("probe: %v", err)
	}
	enc, ok := caps.Best("h264")
	if !ok {
		t.Skip("no h264 encoder")
	}
	// An encoder that starts and never delivers a frame: a generation that
	// stays "starting" however fast the test runs.
	stall := filepath.Join(t.TempDir(), "stalled-ffmpeg")
	if err := os.WriteFile(stall, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := func(t *testing.T) (*Session, *lockedLog) {
		c := *caps
		cfg := &Config{Capture: "test", Encoder: enc.Name, TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
			DefaultKbps: 4000, MaxKbps: 100000}
		ctx, cancel := context.WithCancel(context.Background())
		logs := &lockedLog{}
		s := &Session{
			a:     &Agent{cfg: cfg, caps: &c, inj: input.NewInjector(nil), hostClock: media.NewClock()},
			hello: proto.Hello{Decoders: []proto.DecoderInfo{{Family: enc.Family}}},
			tried: map[string]bool{}, usage: map[string]string{},
			ctx: ctx, cancel: cancel, ctrl: &fakeCtrl{}, frameQ: make(chan *media.Frame, 6),
			log: slog.New(slog.NewTextHandler(logs, nil)),
		}
		s.video = media.NewVideo(&c, s.log, s.a.clock)
		t.Cleanup(func() { cancel(); s.video.Stop() })
		go s.videoEvents()
		return s, logs
	}
	// keepCutWindowOpen moves the last cut ahead so that a slow machine
	// cannot leave the 2 s window before the overflow.
	keepCutWindowOpen := func(s *Session) {
		s.kickMu.Lock()
		s.lastCong = time.Now().Add(time.Hour)
		s.kickMu.Unlock()
	}
	waitFor := func(t *testing.T, logs *lockedLog, substr string) string {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if l := logs.lines(substr); len(l) > 0 {
				return l[0]
			}
		}
		t.Fatalf("no log line with %s", substr)
		return ""
	}

	t.Run("overlapped back-off", func(t *testing.T) {
		s, logs := session(t)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		// The client takes frames until generation 1 streams.
		for n := 0; n < 10; n++ {
			select {
			case <-s.frameQ:
			case <-time.After(20 * time.Second):
				t.Fatal("generation 1 sends no frames")
			}
		}
		s.a.caps.FFmpeg = stall // the next generation stays starting
		s.congestion(120, false)
		keepCutWindowOpen(s)
		if _, ok := s.video.Active(); !ok {
			t.Fatal("overlapped back-off stopped the active generation")
		}
		if p, _ := s.video.Current(); p.BitrateKbps != 3000 || s.curKbps.Load() != 3000 {
			t.Fatalf("back-off to %d kbps (starting %d), want 3000", s.curKbps.Load(), p.BitrateKbps)
		}
		// The client stops taking frames: generation 1 overflows the queue.
		line := waitFor(t, logs, `msg="restarting video" reason="queue overflow" urgent=true`)
		if !strings.Contains(line, "takeover=true") {
			t.Fatalf("overflow did not hand over to the starting generation: %s", line)
		}
		if _, ok := s.video.Active(); ok {
			t.Fatal("the old generation still streams after the overflow")
		}
		if p, ok := s.video.Current(); !ok || p.BitrateKbps != 3000 || s.curKbps.Load() != 3000 {
			t.Fatalf("after the overflow: starting %v at %d kbps, target %d, want 3000 (no second cut)", ok, p.BitrateKbps, s.curKbps.Load())
		}
		if n := len(logs.lines(`msg="starting encoder"`)); n != 2 {
			t.Fatalf("%d encoder starts, want 2 (the starting generation is kept)", n)
		}
		if n := len(logs.lines(`msg="congestion: lowering bitrate"`)); n != 1 {
			t.Fatalf("%d bitrate cuts, want 1", n)
		}
	})

	t.Run("key frame dropped", func(t *testing.T) {
		s, logs := session(t)
		s.curKbps.Store(3000) // a cut moments ago
		keepCutWindowOpen(s)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		// Nobody takes frames: generation 1's key frame is among the
		// dropped ones and the client cannot decode anything of it.
		waitFor(t, logs, `msg="frames dropped" why="queue overflow" gen=1 from_seq=0`)
		waitFor(t, logs, `msg="restarting video" reason="queue overflow" urgent=true`)
		if l := waitFor(t, logs, `msg="starting encoder" gen=2`); !strings.Contains(l, " kbps=3000 ") {
			t.Fatalf("restart not at the lowered bitrate: %s", l)
		}
		if n := len(logs.lines(`msg="congestion: lowering bitrate"`)); n != 0 || s.curKbps.Load() != 3000 {
			t.Fatalf("%d bitrate cuts, target %d kbps, want none and 3000", n, s.curKbps.Load())
		}
	})

	// A key-frame request (a confirmed loss) keeps the back-off.
	t.Run("keyframe request", func(t *testing.T) {
		s, logs := session(t)
		s.curKbps.Store(3000)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		s.requestKeyframe()
		if l := waitFor(t, logs, `msg="starting encoder" gen=2`); !strings.Contains(l, " kbps=3000 ") {
			t.Fatalf("key-frame restart undid the back-off: %s", l)
		}
	})
}
