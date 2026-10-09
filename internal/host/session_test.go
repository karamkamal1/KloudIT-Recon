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
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
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

	h, ext := videoHeader(f, 1, now, 0)
	if h.SendUs != f.EncodeDoneUs || h.Flags != proto.FrameFlagKey || !ext.Empty() {
		t.Fatalf("v1: send %d flags %#x ext %v, want send = encodeDone %d, key only", h.SendUs, h.Flags, !ext.Empty(), f.EncodeDoneUs)
	}

	h, ext = videoHeader(f, proto.HelloVersionFrameExt, now, 0)
	done, _ := ext.Get(proto.ExtEncodeDoneUs)
	capture, _ := ext.Get(proto.ExtCaptureUs)
	if h.SendUs != now || h.Flags != proto.FrameFlagKey|proto.FrameFlagExt || done != f.EncodeDoneUs || capture != f.CaptureUs {
		t.Fatalf("v2: send %d flags %#x encodeDone %d capture %d", h.SendUs, h.Flags, done, capture)
	}

	f.CaptureUs = 0
	_, ext = videoHeader(f, proto.HelloVersionFrameExt, now, 0)
	if _, ok := ext.Get(proto.ExtCaptureUs); ok {
		t.Fatal("capture tag sent without a capture stamp")
	}
	for _, tag := range []byte{proto.ExtPresentUs, proto.ExtEncodeSubmitUs, proto.ExtRefFloor, proto.ExtLTRSlot, proto.ExtTemporalLayer} {
		if _, ok := ext.Get(tag); ok {
			t.Fatalf("tag %d sent for an FFmpeg frame", tag)
		}
	}

	// A native helper's frame: its stages and recovery metadata (tags 1, 3,
	// 5-7; refFloor only on recovery frames, 0 included), round trip through
	// the wire format; v1 clients still get the plain header.
	f = &media.Frame{Gen: 4, Seq: 9, PresentUs: 800, CaptureUs: 900, SubmitUs: 1200, EncodeDoneUs: 5000,
		Recovery: true, RefFloor: 0, MarkedLTR: true, LTRSlot: 1, TemporalLayer: 1, Data: []byte{1, 2, 3}}
	h, ext = videoHeader(f, proto.HelloVersionFrameExt, now, 0)
	b := make([]byte, proto.FrameHeaderLen)
	h.Marshal(b)
	b = append(ext.Append(b), f.Data...)
	_, got, payload, err := proto.ParseFrame(b)
	if err != nil || !bytes.Equal(payload, f.Data) {
		t.Fatalf("parse: %v %v", err, payload)
	}
	for tag, want := range map[byte]uint64{proto.ExtPresentUs: 800, proto.ExtCaptureUs: 900, proto.ExtEncodeSubmitUs: 1200,
		proto.ExtEncodeDoneUs: 5000, proto.ExtRefFloor: 0, proto.ExtLTRSlot: 1, proto.ExtTemporalLayer: 1} {
		if v, ok := got.Get(tag); !ok || v != want {
			t.Errorf("tag %d = %d %v, want %d", tag, v, ok, want)
		}
	}
	f.Recovery, f.MarkedLTR, f.TemporalLayer = false, false, 0
	if _, ext = videoHeader(f, proto.HelloVersionFrameExt, now, 0); !func() bool {
		_, r := ext.Get(proto.ExtRefFloor)
		_, l := ext.Get(proto.ExtLTRSlot)
		_, tl := ext.Get(proto.ExtTemporalLayer)
		return !r && !l && !tl
	}() {
		t.Fatal("recovery tags on a frame without recovery metadata")
	}
	if h, ext = videoHeader(f, 1, now, 0); h.Flags&proto.FrameFlagExt != 0 || !ext.Empty() {
		t.Fatal("v1 client got the extension")
	}
	// The thinned mask (Phase 5): only to clients that read it, only when
	// frames were left out.
	if _, ext = videoHeader(f, proto.HelloVersionThinned, now, 0b101); !func() bool {
		v, ok := ext.Get(proto.ExtThinned)
		return ok && v == 0b101
	}() {
		t.Fatal("thinned mask missing for a v4 client")
	}
	for _, v := range []int{proto.HelloVersionRecovery, proto.HelloVersionFrameExt} {
		if _, ext = videoHeader(f, v, now, 0b101); func() bool { _, ok := ext.Get(proto.ExtThinned); return ok }() {
			t.Fatalf("thinned mask sent to a v%d client", v)
		}
	}
	if _, ext = videoHeader(f, proto.HelloVersionThinned, now, 0); func() bool { _, ok := ext.Get(proto.ExtThinned); return ok }() {
		t.Fatal("empty thinned mask sent")
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
	p = expect(auto, "av1_amf", "", "two failures since the last live generation")

	// A generation that captured with AMD Direct Capture takes the session
	// off it; its failures, encoder faults too, do not count against the
	// encoder, which the restarts on ddagrab test.
	p.Source.Backend = "amf"
	fail(p, false, true)
	fail(p, false, true)
	expect(auto, "av1_amf", "", "two AMD Direct Capture failures")
	if !s.amfFailed.Load() {
		t.Fatal("a failed AMD Direct Capture generation left the session on it")
	}

	off := false
	if p, err := s.buildParams(proto.Prefs{Adaptive: &off}); err != nil || p.Adaptive {
		t.Fatalf("adaptive bitrate off in the client, on in the encoder parameters (%v)", err)
	}
	if p, err := s.buildParams(auto); err != nil || !p.Adaptive {
		t.Fatalf("adaptive bitrate on by default, off in the encoder parameters (%v)", err)
	}
}

// TestHostStages checks the host's own stage window: only acknowledged frames
// (once each), 10 s by ack time, and the client's percentile definition; with
// sub-frame output (Phase 5 wiring B) the encoder submit -> first slice and
// first slice -> whole frame rows, from frames with consistent stamps only.
func TestHostStages(t *testing.T) {
	var h hostStages
	var now uint64
	for i := uint64(0); i < 100; i++ {
		now = 1_000_000 + i*200_000
		f := &media.Frame{Gen: 1, Seq: uint32(i), EncodeDoneUs: now, CaptureUs: now - (i+1)*100}
		if i%10 == 0 {
			f.CaptureUs = 0
		}
		if i%4 == 0 {
			// Submitted 2 ms + i * 10 µs before the first slice, which was
			// ready 1 ms before the whole frame; frame 96's first slice
			// stamp is after the frame's (not counted).
			f.SubmitUs, f.FirstSliceUs = now-3000-i*10, now-1000
			if i == 96 {
				f.FirstSliceUs = now + 5
			}
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
	sum := h.summary(now + 1000)
	if sum.capture != "7.7/9.9/9.9 n=20" || sum.queue != "0.1/0.1/0.1 n=25" {
		t.Fatalf("capture %q queue %q", sum.capture, sum.queue)
	}
	// First slices: i = 52, 56, ... 92 (11 frames): 2.52 .. 2.92 ms.
	if sum.firstSlice != "2.7/2.9/2.9 n=11" || sum.sliceRest != "1.0/1.0/1.0 n=11" {
		t.Fatalf("first slice %q rest %q", sum.firstSlice, sum.sliceRest)
	}
	var none hostStages
	none.sentFrame(&media.Frame{Gen: 1, Seq: 1, SubmitUs: 10, EncodeDoneUs: 50}, 60)
	none.acked(1, 1, 100)
	if sum := none.summary(200); sum.firstSlice != "" || sum.sliceRest != "" || sum.queue == "" {
		t.Fatalf("whole frames: %+v", sum)
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
		a: &Agent{hostClock: media.NewHostClock()}, c: transport.FromQUIC(server), ctx: ctx, frameQ: make(chan *media.Frame, 6),
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
			a:     &Agent{cfg: cfg, caps: &c, inj: input.NewInjector(nil), hostClock: media.NewHostClock()},
			hello: proto.Hello{Decoders: []proto.DecoderInfo{{Family: enc.Family}}},
			tried: map[string]bool{}, usage: map[string]string{},
			ctx: ctx, cancel: cancel, ctrl: &fakeCtrl{}, frameQ: make(chan *media.Frame, 6),
			// Debug: restarts for the rate controller log there (startVideoLog).
			log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
		s.video = media.NewVideo(&c, s.log, s.a.clock)
		t.Cleanup(func() { cancel(); s.video.Stop() })
		go s.videoEvents()
		return s, logs
	}
	// keepCutWindowOpen moves the last decrease ahead so that a slow
	// machine cannot leave the 2 s window before the overflow, and holds
	// further delay decreases.
	keepCutWindowOpen := func(s *Session) {
		s.rate.mu.Lock()
		s.rate.lastDecrease = time.Now().Add(time.Hour)
		s.rate.holdUntil = s.rate.lastDecrease
		s.rate.mu.Unlock()
	}
	// backOff sets the controller's target as a decrease would.
	backOff := func(s *Session, kbps int) {
		s.rate.mu.Lock()
		s.rate.est = float64(kbps)
		s.rate.mu.Unlock()
	}
	target := func(s *Session) int {
		cur, _ := s.rate.kbps()
		return cur
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
		// The generation has streamed for longer than the FFmpeg policy's
		// gap between changes (else the next rate-loop tick applies it).
		s.rate.mu.Lock()
		s.rate.lastApply = time.Now().Add(-time.Second)
		s.rate.mu.Unlock()
		s.congestion(120, signalDelay)
		keepCutWindowOpen(s)
		if _, ok := s.video.Active(); !ok {
			t.Fatal("overlapped back-off stopped the active generation")
		}
		if p, _ := s.video.Current(); p.BitrateKbps != 3400 || target(s) != 3400 {
			t.Fatalf("back-off to %d kbps (starting %d), want 3400 (x0.85)", target(s), p.BitrateKbps)
		}
		// The client stops taking frames: generation 1 overflows the queue.
		line := waitFor(t, logs, `msg="restarting video" reason="queue overflow" urgent=true`)
		if !strings.Contains(line, "takeover=true") {
			t.Fatalf("overflow did not hand over to the starting generation: %s", line)
		}
		if _, ok := s.video.Active(); ok {
			t.Fatal("the old generation still streams after the overflow")
		}
		if p, ok := s.video.Current(); !ok || p.BitrateKbps != 3400 || target(s) != 3400 {
			t.Fatalf("after the overflow: starting %v at %d kbps, target %d, want 3400 (no second cut)", ok, p.BitrateKbps, target(s))
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
		backOff(s, 3000) // a cut moments ago
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
		if n := len(logs.lines(`msg="congestion: lowering bitrate"`)); n != 0 || target(s) != 3000 {
			t.Fatalf("%d bitrate cuts, target %d kbps, want none and 3000", n, target(s))
		}
	})

	// A key-frame request (a confirmed loss) keeps the back-off.
	t.Run("keyframe request", func(t *testing.T) {
		s, logs := session(t)
		backOff(s, 3000)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		s.requestKeyframe("keyframe request")
		if l := waitFor(t, logs, `msg="starting encoder" gen=2`); !strings.Contains(l, " kbps=3000 ") {
			t.Fatalf("key-frame restart undid the back-off: %s", l)
		}
	})

	// A decoder flush ({"t":"congestion","reason":"decoder"}) within 2 s of
	// a cut cuts nothing but restarts at once for the key frame the client
	// waits for; a delay report then restarts nothing. Outside the window
	// the flush cuts with an urgent restart and caps the raises.
	t.Run("decoder report", func(t *testing.T) {
		s, logs := session(t)
		go func() { // the client takes every frame: no queue overflow
			for {
				select {
				case <-s.frameQ:
				case <-s.ctx.Done():
					return
				}
			}
		}()
		backOff(s, 3000)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		keepCutWindowOpen(s)
		control := func(msgs ...proto.ClientMsg) {
			t.Helper()
			var in bytes.Buffer
			for _, m := range msgs {
				b, _ := json.Marshal(m)
				if err := proto.WriteMsg(&in, b); err != nil {
					t.Fatal(err)
				}
			}
			s.ctrl = &scriptedCtrl{r: &in}
			if err := s.controlLoop(); !errors.Is(err, io.EOF) {
				t.Fatalf("control loop: %v", err)
			}
		}
		control(proto.ClientMsg{T: "congestion", Reason: proto.CongestionDecoder}, proto.ClientMsg{T: "congestion", DelayMs: 120})
		if l := logs.lines(`msg="restarting video"`); len(l) != 1 || !strings.Contains(l[0], `reason="keyframe request" urgent=true`) {
			t.Fatalf("restarts %q, want one key-frame restart", l)
		}
		if n := len(logs.lines(`msg="congestion: lowering bitrate"`)); n != 0 || target(s) != 3000 || s.rate.decoderLimit() != 0 {
			t.Fatalf("%d bitrate cuts, target %d kbps, decoder limit %d; want none, 3000, none", n, target(s), s.rate.decoderLimit())
		}

		s.rate.mu.Lock()
		s.rate.lastDecrease, s.rate.holdUntil = time.Time{}, time.Time{}
		s.rate.mu.Unlock()
		control(proto.ClientMsg{T: "congestion", Reason: proto.CongestionDecoder})
		if l := logs.lines(`msg="congestion: lowering bitrate"`); len(l) != 1 || !strings.Contains(l[0], "from=3000 to=2250") || !strings.Contains(l[0], "urgent=true") {
			t.Fatalf("cuts %q, want 3000 -> 2250 urgent", l)
		}
		if l := logs.lines(`msg="bitrate recovery limited by the client's decoder"`); len(l) != 1 || !strings.Contains(l[0], "max=2550") {
			t.Fatalf("decoder limit lines %q, want max=2550", l)
		}
		if l := logs.lines(`msg="restarting video"`); len(l) != 2 || !strings.Contains(l[1], `reason=congestion urgent=true`) {
			t.Fatalf("restarts %q, want an urgent congestion restart", l)
		}
	})

	// A settings message that changes only the audio keeps it too.
	t.Run("audio settings", func(t *testing.T) {
		s, logs := session(t)
		backOff(s, 3000)
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		off := false
		msg, _ := json.Marshal(proto.ClientMsg{T: "settings", Prefs: &proto.Prefs{Audio: &off}})
		var in bytes.Buffer
		if err := proto.WriteMsg(&in, msg); err != nil {
			t.Fatal(err)
		}
		s.ctrl = &scriptedCtrl{r: &in}
		if err := s.controlLoop(); !errors.Is(err, io.EOF) {
			t.Fatalf("control loop: %v", err)
		}
		if kb := target(s); kb != 3000 {
			t.Fatalf("audio-only settings change reset the back-off: target %d kbps, want 3000", kb)
		}
		s.requestKeyframe("keyframe request")
		if l := waitFor(t, logs, `msg="starting encoder" gen=2`); !strings.Contains(l, " kbps=3000 ") {
			t.Fatalf("key-frame restart after an audio-only settings change undid the back-off: %s", l)
		}
	})
}

// TestLogStagesRenderer: a client's stage summary ({"t":"stages"}) is logged
// next to the encoder with the presentation path the client names (step
// 4.3: the draw and display rows depend on it), its frame pacing mode (step
// 4.4: hold and display depend on it) and its upscaling (Phase 5: draw and
// display depend on it); a value that is not a plain path name or a known
// mode, or none (older clients), adds nothing. The hold
// row is logged, also in a report with every stage (ten rows). With the test
// hook pre-stage-hold the host takes reports as hosts before step 4.4 did:
// the nine rows a client sends them (hold and draw as one draw row), not ten.
func TestLogStagesRenderer(t *testing.T) {
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
	cfg := &Config{Capture: "test", Encoder: enc.Name, TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
		DefaultKbps: 4000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &lockedLog{}
	s := &Session{
		a:     &Agent{cfg: cfg, caps: caps, inj: input.NewInjector(nil), hostClock: media.NewHostClock()},
		hello: proto.Hello{Decoders: []proto.DecoderInfo{{Family: enc.Family}}},
		tried: map[string]bool{}, usage: map[string]string{},
		ctx: ctx, cancel: cancel, ctrl: &fakeCtrl{}, frameQ: make(chan *media.Frame, 6),
		log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	s.video = media.NewVideo(caps, s.log, s.a.clock)
	t.Cleanup(func() { cancel(); s.video.Stop() })
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	for deadline := time.After(20 * time.Second); ; {
		if _, ok := s.video.Active(); ok {
			break
		}
		select {
		case <-s.frameQ:
		case <-deadline:
			t.Fatal("the encoder sends no frames")
		}
	}
	rows := []proto.StageStat{{Name: "hold", N: 40, P50: 8.1, P95: 15.9, P99: 16.4}, {Name: "draw", N: 40, P50: 0.4, P95: 0.9, P99: 1.2},
		{Name: "e2e", From: "capture", N: 40, P50: 20, P95: 30, P99: 40}}
	var all []proto.StageStat
	for _, n := range []string{"capture", "queue", "network", "transfer", "wait", "decode", "hold", "draw", "display", "e2e"} {
		all = append(all, proto.StageStat{Name: n, N: 30, P50: 1, P95: 2, P99: 3})
	}
	var merged []proto.StageStat // what a client sends a host without stage-hold
	for _, r := range all {
		if r.Name == "draw" {
			r.P50 = 9
		}
		if r.Name != "hold" {
			merged = append(merged, r)
		}
	}
	report := func(msgs ...proto.ClientMsg) {
		t.Helper()
		var in bytes.Buffer
		for _, m := range msgs {
			m.T = "stages"
			b, _ := json.Marshal(m)
			if err := proto.WriteMsg(&in, b); err != nil {
				t.Fatal(err)
			}
		}
		s.ctrl = &scriptedCtrl{r: &in}
		if err := s.controlLoop(); !errors.Is(err, io.EOF) {
			t.Fatalf("control loop: %v", err)
		}
	}
	report(proto.ClientMsg{Stages: rows, Renderer: "webgpu", Pacing: "smooth", Upscale: "fsr"},
		proto.ClientMsg{Stages: rows, Renderer: `x" injected="1`, Pacing: `smooth" injected="1`, Upscale: `fsr" injected="1`},
		proto.ClientMsg{Stages: rows},
		proto.ClientMsg{Stages: all, Renderer: "canvas2d", Pacing: "mixed"})
	s.a.faults.preStageHold = true
	report(proto.ClientMsg{Stages: all, Pacing: "smooth"}, proto.ClientMsg{Stages: merged, Pacing: "smooth"})
	l := logs.lines(`msg="latency stages`)
	if len(l) != 5 || !strings.Contains(l[0], " renderer=webgpu pacing=smooth upscale=fsr ") || !strings.Contains(l[0], `draw="0.4/0.9/1.2 n=40"`) ||
		!strings.Contains(l[0], `hold="8.1/15.9/16.4 n=40"`) ||
		strings.Contains(l[1], "renderer=") || strings.Contains(l[1], "pacing=") || strings.Contains(l[1], "upscale=") || strings.Contains(l[1], "injected") ||
		strings.Contains(l[2], "renderer=") || strings.Contains(l[2], "pacing=") || strings.Contains(l[2], "upscale=") ||
		!strings.Contains(l[3], " pacing=mixed ") || !strings.Contains(l[3], `hold="1.0/2.0/3.0 n=30"`) || !strings.Contains(l[3], `display="1.0/2.0/3.0 n=30"`) ||
		strings.Contains(l[4], "hold=") || !strings.Contains(l[4], `draw="9.0/2.0/3.0 n=30"`) || !strings.Contains(l[4], `display="1.0/2.0/3.0 n=30"`) {
		t.Fatalf("stage lines:\n%s", strings.Join(l, "\n"))
	}
}

// scriptedCtrl hands controlLoop the client messages in r, then io.EOF.
type scriptedCtrl struct {
	fakeCtrl
	r io.Reader
}

func (c *scriptedCtrl) Read(b []byte) (int, error) { return c.r.Read(b) }

// TestHealWatch checks which reported losses the session bounds in time
// (recovery "skip" from an encoder that heals, HealFrames > 0) and when a
// watch ends: once the encoder has produced the frame HealFrames after the
// latest loss, or with a new generation (its key frame). The restart when a
// watch outlasts media.MaxHeal: internal/e2e TestStreamingIntraRefreshStill.
func TestHealWatch(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	until := func() (uint32, bool) {
		s.healMu.Lock()
		defer s.healMu.Unlock()
		if s.heal == nil {
			return 0, false
		}
		return s.heal.seq, true
	}
	drop := func(gen uint8, seqs ...uint32) {
		var fs []*media.Frame
		for _, seq := range seqs {
			fs = append(fs, &media.Frame{Gen: gen, Seq: seq})
		}
		s.reportDropped(fs, "test")
	}
	expect := func(after string, seq uint32, watching bool) {
		t.Helper()
		if got, ok := until(); ok != watching || got != seq {
			t.Fatalf("after %s: watching %v until seq %d, want %v until %d", after, ok, got, watching, seq)
		}
	}
	// Not watched: a skip forced by the test hook on an encoder without intra
	// refresh (nothing heals it), and recovery keyframe (the client asks).
	s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: proto.RecoverySkip}, 0)
	drop(1, 5)
	expect("a forced skip", 0, false)
	s.healConfig(&proto.VideoConfig{Gen: 2, Recovery: proto.RecoveryKeyframe}, 30)
	drop(2, 5)
	expect("recovery keyframe", 0, false)

	s.healConfig(&proto.VideoConfig{Gen: 3, Recovery: proto.RecoverySkip}, 30)
	drop(2, 9) // an earlier generation's frame left in the queue
	expect("a superseded generation's loss", 0, false)
	drop(3, 10)
	expect("a loss", 40, true)
	s.healFrame(&media.Frame{Gen: 3, Seq: 39})
	expect("seq 39", 40, true)
	drop(3, 20, 21) // later losses move the frame on, not the deadline
	expect("two more losses", 51, true)
	s.healFrame(&media.Frame{Gen: 3, Seq: 51})
	expect("seq 51", 0, false)
	drop(3, 60)
	expect("another loss", 90, true)
	s.healConfig(&proto.VideoConfig{Gen: 4, Recovery: proto.RecoverySkip}, 30)
	expect("a new generation", 0, false)
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
		{"rotated monitor", hevc, false, platform.Monitor{Index: 1, W: 1440, H: 2560, DXGIOutput: 2, Rotated: true}, true, "monitor 1 is rotated"},
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
	s.useAMFCapture(&p, mon, false)
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
		s.useAMFCapture(&p, mon, false)
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
		Failed: &media.Params{Source: media.Source{Backend: "ddagrab"}}})
	p = dda
	if s.useAMFCapture(&p, mon, false); p.Source.Backend != "amf" {
		t.Fatal("a ddagrab failure turned AMD Direct Capture off")
	}
	s.noteCaptureFailure(media.VideoEvent{Err: errors.New("encoder hevc_amf exited: Failed to initialize capture component: 3"),
		Failed: &media.Params{Source: media.Source{Backend: "amf"}}})
	p = dda
	if s.useAMFCapture(&p, mon, false); p.Source.Backend != "ddagrab" || !strings.Contains(logs.String(), "AMD Direct Capture failed") ||
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

// TestCursorLoopAfterVideoCursor: a session that starts with the client's
// cursor setting "video" sends no pointer (the video shows it); when the
// client switches to its local cursor mid-session (a live settings change,
// whose video restart leaves the pointer out of the video), the host's
// pointer shape and position follow at once. Before, the loop ended at the
// start of such a session, and the client drew a plain arrow from then on.
func TestCursorLoopAfterVideoCursor(t *testing.T) {
	onPlatform, gc, ci := cursorOnPlatform, getCursor, cursorImage
	t.Cleanup(func() { cursorOnPlatform, getCursor, cursorImage = onPlatform, gc, ci })
	cursorOnPlatform = true
	getCursor = func() (platform.CursorState, error) {
		return platform.CursorState{Visible: true, X: 960, Y: 540, Handle: 7}, nil
	}
	cursorImage = func(h uint64) (*platform.CursorShape, error) {
		return &platform.CursorShape{ID: h, W: 1, H: 1, RGBA: []byte{1, 2, 3, 255}}, nil
	}
	s, _, ctrl := testSession(t, testFaults{})
	dc := &dgConn{Conn: s.c}
	s.c = dc
	s.a.cfg = &Config{}
	s.prefs = proto.Prefs{Cursor: "video"}
	s.monitor = platform.Monitor{W: 1920, H: 1080}
	go s.cursorLoop()
	time.Sleep(60 * time.Millisecond)
	dc.mu.Lock()
	n := len(dc.sent)
	dc.mu.Unlock()
	if hasMsg(ctrl.messages(t), `"t":"cursor"`) || n != 0 {
		t.Fatalf("pointer sent while the video shows it: %q, %d datagrams", ctrl.messages(t), n)
	}
	s.prefsMu.Lock()
	s.prefs.Cursor = "local"
	s.prefsMu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		dc.mu.Lock()
		n = len(dc.sent)
		dc.mu.Unlock()
		if hasMsg(ctrl.messages(t), `"t":"cursor"`, `"id":7`, `"png":`) && n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pointer after switching to the local cursor: %q, %d datagrams", ctrl.messages(t), n)
		}
	}
}

// fakeDisplayRequest records what a session does with its display request.
type fakeDisplayRequest struct {
	mu  *sync.Mutex
	log *[]string
}

func (f fakeDisplayRequest) Set(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.log = append(*f.log, map[bool]string{true: "set", false: "clear"}[on])
	return nil
}

func (f fakeDisplayRequest) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.log = append(*f.log, "close")
}

// fakeDisplayRequests replaces newDisplayRequest for a test and returns
// what was done with the requests made, in order.
func fakeDisplayRequests(t *testing.T) (events func() string) {
	var mu sync.Mutex
	var log []string
	nd := newDisplayRequest
	t.Cleanup(func() { newDisplayRequest = nd })
	newDisplayRequest = func(string) (displayRequest, error) { return fakeDisplayRequest{&mu, &log}, nil }
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(log, ",")
	}
}

// TestSessionKeepsDisplayOn: a session keeps the PC's display on from its
// welcome until it ends, whatever the pipeline (here FFmpeg's, whose video
// cannot start: the session ends at once and lets the display go). Before,
// only the native helper's capture did, so on the FFmpeg path the display
// turned off after the power plan's timeout in a session with only a
// controller's input.
func TestSessionKeepsDisplayOn(t *testing.T) {
	events := fakeDisplayRequests(t)
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{Pipeline: "ffmpeg"})
	msgs, _, done := runClient(t, r.a, proto.Hello{V: proto.HelloVersionFrameExt, Client: proto.ClientInfo{Width: 1920, Height: 1080}})
	waitFor(t, msgs, `"t":"notice"`, "Could not start video")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	if e := events(); e != "set,clear,close" {
		t.Fatalf("display request: %v, want set, then clear and close when the session ends", e)
	}
}

// TestHiddenClientLetsDisplaySleep: while the client is hidden the session
// lets the display follow the power plan (as the native helper does, whose
// capture stops), and keeps it on again from resume.
func TestHiddenClientLetsDisplaySleep(t *testing.T) {
	events := fakeDisplayRequests(t)
	s, _, ctrl := fakePipelineSession(t, slog.New(slog.NewTextHandler(io.Discard, nil)), media.PipelineCaps{})
	control := func(msgs ...proto.ClientMsg) {
		t.Helper()
		var in bytes.Buffer
		for _, m := range msgs {
			b, _ := json.Marshal(m)
			if err := proto.WriteMsg(&in, b); err != nil {
				t.Fatal(err)
			}
		}
		ctrl.r = &in
		if err := s.controlLoop(); !errors.Is(err, io.EOF) {
			t.Fatalf("control loop: %v", err)
		}
	}
	release := s.keepDisplayOn()
	control(proto.ClientMsg{T: "pause"}, proto.ClientMsg{T: "pause"})
	if e := events(); e != "set,clear" {
		t.Fatalf("display request after pause: %v, want set, then clear", e)
	}
	control(proto.ClientMsg{T: "resume"})
	release()
	if e := events(); e != "set,clear,set,clear,close" {
		t.Fatalf("display request: %v, want set again on resume, then clear and close", e)
	}
}
