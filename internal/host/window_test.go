package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// fakeMeter is a deliveryMeter the test moves by hand: send adds to sent,
// deliver (deliverAt) moves done and records when.
type fakeMeter struct {
	sent, done atomic.Uint64
	progress   chan struct{}
	minRTT     time.Duration
	pacing     int64 // bit/s
	mu         sync.Mutex
	log        []fakeDelivery
}

type fakeDelivery struct {
	done uint64
	at   time.Time
}

func newFakeMeter(minRTT time.Duration) *fakeMeter {
	return &fakeMeter{progress: make(chan struct{}, 1), minRTT: minRTT, pacing: 24_000_000}
}

func (m *fakeMeter) Delivery() (uint64, uint64)  { return m.sent.Load(), m.done.Load() }
func (m *fakeMeter) Progress() <-chan struct{}   { return m.progress }
func (m *fakeMeter) RecentMinRTT() time.Duration { return m.minRTT }
func (m *fakeMeter) PacingRate() int64           { return m.pacing }
func (m *fakeMeter) send(n uint64)               { m.sent.Add(n) }
func (m *fakeMeter) deliver(upTo uint64)         { m.deliverAt(upTo, time.Now()) }

func (m *fakeMeter) deliverAt(upTo uint64, at time.Time) {
	m.mu.Lock()
	m.log = append(m.log, fakeDelivery{upTo, at})
	m.mu.Unlock()
	m.done.Store(upTo)
	select {
	case m.progress <- struct{}{}:
	default:
	}
}

func (m *fakeMeter) DeliveredAt(pos uint64) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.log {
		if d.done >= pos {
			return d.at, true
		}
	}
	return time.Time{}, false
}

// shortOf gives w the evidence of a path that falls short of m's pacing
// rate: the last frames took twice as long to deliver as to send.
func shortOf(w *videoWindow, m deliveryMeter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.meter != m {
		w.meter, w.marks, w.rtt = m, w.marks[:0], 0
	}
	w.npaced = 0
	for range shortfallFrames {
		w.paced[w.npaced%shortfallFrames] = paceSample{took: 20 * time.Millisecond, paced: 10 * time.Millisecond}
		w.npaced++
	}
}

func TestWindowLimit(t *testing.T) {
	fi := time.Second / 60
	for _, c := range []struct {
		rtt, interval time.Duration
		recent, want  int
	}{
		{0, fi, 0, 1},                           // no packet acknowledged yet (RecentMinRTT 0): the next frame waits for the last one
		{time.Millisecond, fi, 0, 1},            // LAN, the last frame sent a while ago: one frame in flight
		{time.Millisecond, fi, 1, 2},            // LAN, the last frame just sent: it is in transit, the next one may follow
		{10 * time.Millisecond, fi, 1, 2},       // 12.5 ms: up to one frame in transit
		{14 * time.Millisecond, fi, 2, 3},       // 17.5 ms: up to two
		{40 * time.Millisecond, fi, 3, 4},       // WAN, 50 ms: 3 frames sent within it at 60 fps
		{40 * time.Millisecond, fi, 1, 2},       // ... a capacity drop spaced them out: 1 sent within it
		{40 * time.Millisecond, fi, 6, 5},       // ... never more than the frame rate sends in a round trip, rounded up
		{40 * time.Millisecond, 0, 3, 4},        // unknown frame rate: 60 fps
		{40 * time.Millisecond, fi / 4, 20, 14}, // 240 fps
	} {
		if got := windowLimit(c.rtt, c.interval, c.recent); got != c.want {
			t.Errorf("windowLimit(%v, %v, %d) = %d, want %d", c.rtt, c.interval, c.recent, got, c.want)
		}
	}
}

// TestVideoWindow: the frames in flight and in transit on a path that falls
// short of the pacing rate; without that evidence the window never holds a
// frame back.
func TestVideoWindow(t *testing.T) {
	var w videoWindow
	m := newFakeMeter(time.Millisecond)
	fi := time.Second / 60
	now := time.Now()
	if !w.room(m, fi, now) {
		t.Fatal("an empty window has no room")
	}
	shortOf(&w, m)
	m.send(50_000)
	w.sent(m, 0, now) // frame 1: in flight up to 50 000
	if !w.room(m, fi, now) {
		t.Fatal("frame 1 just sent is in transit: the next one may follow it")
	}
	now = now.Add(fi)
	if w.room(m, fi, now) {
		t.Fatal("frame 1 sent a frame interval ago on a 1 ms path and not acknowledged: queued, the window is full")
	}
	m.deliverAt(49_999, now)
	if w.room(m, fi, now) {
		t.Fatal("frame 1 not wholly acknowledged: still full")
	}
	m.deliverAt(50_000, now)
	if !w.room(m, fi, now) {
		t.Fatal("frame 1 acknowledged: room")
	}
	m.send(50_000)
	w.sent(m, 50_000, now)
	m.send(50_000)
	w.sent(m, 100_000, now) // frames 2 and 3 in flight, both just sent
	if w.room(m, fi, now) {
		t.Fatal("two frames in flight on a 1 ms path: full")
	}
	m.minRTT = 20 * time.Millisecond
	if w.room(m, fi, now) {
		t.Fatal("while the path falls short its round trips hold the window's own backlog: the one from before counts")
	}
	w.mu.Lock()
	w.npaced = 0
	w.mu.Unlock()
	w.room(m, fi, now) // no shortfall: the round trip follows the path's
	shortOf(&w, m)
	if !w.room(m, fi, now) {
		t.Fatal("a 20 ms round trip (25 ms: up to two frames in transit) keeps both in transit: room for a third")
	}
	if w.room(m, fi, now.Add(26*time.Millisecond)) {
		t.Fatal("frames older than the round trip are queued, not in transit: full")
	}
	// Without the evidence of a shortfall the same frames in flight hold
	// nothing back: a round trip that grew, or jitter.
	w.mu.Lock()
	w.npaced = 0
	w.mu.Unlock()
	if !w.room(m, fi, now.Add(26*time.Millisecond)) {
		t.Fatal("the path keeps up with the pacer: room whatever is in flight")
	}
	// A path migration (a new controller) starts over.
	shortOf(&w, m)
	m2 := newFakeMeter(time.Millisecond)
	if !w.room(m2, fi, now) {
		t.Fatal("a new controller's window is empty")
	}
	m2.send(10)
	w.sent(m2, 0, now)
	w.mu.Lock()
	marks, npaced := slices.Clone(w.marks), w.npaced
	w.mu.Unlock()
	if len(marks) != 1 || marks[0].pos != 10 || npaced != 0 {
		t.Fatalf("marks after the migration %v (%d samples), want one at 10 and no evidence", marks, npaced)
	}
	w.noteHold(3 * time.Millisecond)
	w.noteHold(time.Millisecond)
	if held, mx := w.stats(); held != 2 || mx != 3*time.Millisecond {
		t.Fatalf("stats %d %v", held, mx)
	}
	if held, mx := w.stats(); held != 0 || mx != 0 {
		t.Fatalf("stats not reset: %d %v", held, mx)
	}
}

// TestWindowShortfall: the evidence of a path slower than the pacer. Each
// frame is 30 000 bytes, 10 ms at the fake's 24 Mbit/s pacing; the path
// takes took from the acknowledgement of its first byte to that of its mark.
func TestWindowShortfall(t *testing.T) {
	w := &videoWindow{}
	m := newFakeMeter(10 * time.Millisecond)
	fi := time.Second / 60
	now := time.Now()
	w.room(m, fi, now)
	// frame sends a frame of n bytes at now whose first byte the peer
	// acknowledges rtt later and its mark took after that.
	frame := func(n uint64, rtt, took time.Duration) {
		start, _ := m.Delivery()
		m.send(n)
		w.sent(m, start, now)
		m.deliverAt(start+1, now.Add(rtt))
		m.deliverAt(start+n, now.Add(rtt+took))
		now = now.Add(fi)
		w.room(m, fi, now) // observes the acknowledgement
	}
	short := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.shortfall()
	}
	frame(30_000, 10*time.Millisecond, 40*time.Millisecond)
	if short() {
		t.Fatal("one frame (10 ms of pacing) decides nothing")
	}
	frame(30_000, 10*time.Millisecond, 40*time.Millisecond)
	if !short() {
		t.Fatal("two frames delivered at a quarter of the pacing rate: a shortfall")
	}
	// The path carries the pacing rate again, whatever its round trip
	// does: a step from 10 to 80 ms, then jitter of +-8 ms on each end.
	for i := range 8 {
		frame(30_000, 80*time.Millisecond, 10*time.Millisecond)
		if i < 3 && !short() {
			t.Fatalf("frame %d: the evidence went after %d good frames", i, i+1)
		}
	}
	if short() {
		t.Fatal("8 frames delivered at the pacing rate over a longer round trip: no shortfall")
	}
	for i := range 40 {
		took := 10*time.Millisecond + time.Duration(i%5-2)*4*time.Millisecond
		frame(30_000, 20*time.Millisecond+time.Duration(i%3)*8*time.Millisecond, took)
		if short() {
			t.Fatalf("jitter frame %d: shortfall", i)
		}
	}
	// A capacity drop to 40 % of the pacing rate shows within three frames.
	for i := range 3 {
		if short() {
			t.Fatalf("shortfall after %d slow frames", i)
		}
		frame(30_000, 10*time.Millisecond, 25*time.Millisecond)
	}
	if !short() {
		t.Fatal("three frames at 40 % of the pacing rate: a shortfall")
	}
	// Small frames (a still desktop) decide nothing: their
	// acknowledgements' timing would.
	w, m, now = &videoWindow{}, newFakeMeter(10*time.Millisecond), time.Now()
	w.room(m, fi, now)
	for range 8 {
		frame(1_500, 10*time.Millisecond, 25*time.Millisecond)
	}
	if short() {
		t.Fatal("8 frames of 0.5 ms pacing: no decision")
	}
}

// meteredConn is fakeConn whose frame stream writes move a fakeMeter's sent
// position by the bytes written; with paced, a write takes as long as the
// meter's pacing rate needs for its bytes (quic-go's Write returns once
// nearly all of the frame is sent).
type meteredConn struct {
	*fakeConn
	m     *fakeMeter
	paced bool
}

func (c *meteredConn) OpenUniStreamSync(ctx context.Context) (transport.SendStream, error) {
	st, err := c.fakeConn.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return meteredStream{st.(*fakeStream), c.m, c.paced}, nil
}

type meteredStream struct {
	*fakeStream
	m     *fakeMeter
	paced bool
}

func (s meteredStream) Write(b []byte) (int, error) {
	if s.paced {
		time.Sleep(time.Duration(float64(len(b)) * 8 / float64(s.m.PacingRate()) * float64(time.Second)))
	}
	n, err := s.fakeStream.Write(b)
	s.m.send(uint64(n))
	return n, err
}

// TestFrameSenderWindow: frameSender at 10 or 30 fps on a path with a 10 ms min
// RTT (a round trip under a frame interval) that falls short of the pacing
// rate: two frames go out back to back (the first is in transit), the third
// waits with its stream open and nothing written until both are
// acknowledged (past the round trip neither is in transit any more); its
// send time is the moment it goes out. Without acknowledgements a held
// frame goes out whole when three quarters of its deadline have passed, and
// its deadline starts then: the window never makes rung 1 cancel it, also
// when its write takes the pacer's time. A frame the client would discard
// (it waits for the answer to a loss before it) is released at once and its
// stream reset, nothing sent.
func TestFrameSenderWindow(t *testing.T) {
	// fps 30: deadline 67 ms, holds up to 50 ms; 10: 200 ms, up to 150 ms
	// (room for a loaded machine where only the hold itself is checked).
	setup := func(t *testing.T, mode string, fps int, paced bool) (*Session, *fakeConn, *fakeCtrl, *fakeMeter, *ladderPipeline) {
		s, c, ctrl := testSession(t, testFaults{})
		m := newFakeMeter(10 * time.Millisecond)
		s.c = &meteredConn{c, m, paced}
		s.meter = func() deliveryMeter { return m }
		shortOf(&s.win, m)
		s.hello.V = proto.HelloVersionRecovery
		p := &ladderPipeline{caps: media.PipelineCaps{ForceIDR: true, Recovery: mode}, events: make(chan media.VideoEvent)}
		s.video = p
		s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: mode}, 0)
		s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: fps})
		go s.frameSender()
		return s, c, ctrl, m, p
	}
	sized := func(seq uint32, n int) *media.Frame {
		return &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Data: bytes.Repeat([]byte{byte(seq)}, n)}
	}
	frame := func(seq uint32) *media.Frame { return sized(seq, 1000) }
	waitStreams := func(t *testing.T, c *fakeConn, n int, what string) []streamState {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
			st := c.snapshot()
			done := 0
			for _, x := range st {
				if x.closed || x.cancelled {
					done++
				}
			}
			if done >= n {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d of %d frame streams done: %+v", what, done, n, st)
			}
		}
	}
	held := func(t *testing.T, c *fakeConn) {
		t.Helper()
		if st := c.snapshot(); len(st) != 3 || len(st[2].data) != 0 || st[2].closed || st[2].cancelled {
			t.Fatalf("the third frame must wait with its stream open and nothing written: %+v", st)
		}
	}

	t.Run("held until acknowledged", func(t *testing.T) {
		s, c, _, m, _ := setup(t, proto.RecoveryKeyframe, 10, false)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- frame(seq)
		}
		waitStreams(t, c, 2, "two frames go out")
		time.Sleep(15 * time.Millisecond) // past the round trip, before the deadline
		held(t, c)
		if s.windowSince.Load() == 0 {
			t.Fatal("frameSender is not holding the frame for the window")
		}
		s.win.mu.Lock()
		first, second := s.win.marks[0].pos, s.win.marks[1].pos
		s.win.mu.Unlock()
		m.deliver(first) // the first frame acknowledged: the second is still in flight, no longer in transit
		time.Sleep(3 * time.Millisecond)
		held(t, c)
		release := s.a.clock()
		m.deliver(second)
		st := waitStreams(t, c, 3, "the third frame after the second was acknowledged")
		h, _, payload, err := proto.ParseFrame(st[2].data)
		if err != nil || h.Seq != 2 || len(payload) != 1000 {
			t.Fatalf("third frame: %+v %v", h, err)
		}
		if h.SendUs < release {
			t.Errorf("send time %d is before the window opened (%d): the hold must count as host queue", h.SendUs, release)
		}
		if n, mx := s.win.stats(); n != 1 || mx < 18*time.Millisecond || mx >= 150*time.Millisecond {
			t.Errorf("window stats: %d held, longest %v", n, mx)
		}
		if s.windowSince.Load() != 0 {
			t.Error("windowSince not cleared")
		}
	})

	t.Run("goes out before its deadline", func(t *testing.T) {
		s, c, ctrl, _, p := setup(t, proto.RecoveryInvalidate, 30, false)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- frame(seq)
			s.checkOut()
		}
		waitStreams(t, c, 2, "two frames go out")
		opened := time.Now()
		time.Sleep(5 * time.Millisecond)
		held(t, c)
		s.frameQ <- frame(3) // a newer frame is ready
		s.checkOut()
		st := waitStreams(t, c, 3, "the held frame at its deadline")
		if d := time.Since(opened); d < 45*time.Millisecond {
			t.Errorf("the held frame went out after %v, before three quarters of its deadline (67 ms)", d)
		}
		if !st[2].closed || st[2].cancelled || len(st[2].data) < 1000 {
			t.Fatalf("held frame: closed %v cancelled %v, %d bytes: want it sent whole", st[2].closed, st[2].cancelled, len(st[2].data))
		}
		time.Sleep(60 * time.Millisecond) // rung 1's timer has passed
		if d := ctrl.dropped(t); len(d) != 0 {
			t.Fatalf("dropped reports %+v: the window must not make rung 1 cancel a frame", d)
		}
		if rec, _, _ := p.state(); len(rec) != 0 || s.stats.cancelled.Load() != 0 {
			t.Fatalf("recover calls %v, cancelled %d", rec, s.stats.cancelled.Load())
		}
	})

	// 60 kB frames take 20 ms at the pacing rate: released 50 ms after its
	// stream opened, the third frame's write ends 3 ms past a deadline
	// counted from the opening (67 ms) while a newer frame waits. Its
	// deadline counts from its release.
	t.Run("a paced write after the hold", func(t *testing.T) {
		s, c, ctrl, _, p := setup(t, proto.RecoveryInvalidate, 30, true)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- sized(seq, 60_000)
			s.checkOut()
		}
		waitStreams(t, c, 2, "two frames go out")
		time.Sleep(5 * time.Millisecond)
		held(t, c)
		s.frameQ <- sized(3, 60_000) // a newer frame is ready
		s.checkOut()
		st := waitStreams(t, c, 3, "the held frame")
		if !st[2].closed || st[2].cancelled || len(st[2].data) < 60_000 {
			t.Fatalf("held frame: closed %v cancelled %v, %d bytes: want it sent whole", st[2].closed, st[2].cancelled, len(st[2].data))
		}
		time.Sleep(120 * time.Millisecond) // rung 1's timers of frames 2 and 3 have passed
		if d := ctrl.dropped(t); len(d) != 0 {
			t.Fatalf("dropped reports %+v: the window's hold must not count toward rung 1's deadline", d)
		}
		if rec, _, _ := p.state(); len(rec) != 0 || s.stats.cancelled.Load() != 0 {
			t.Fatalf("recover calls %v, cancelled %d", rec, s.stats.cancelled.Load())
		}
		if n, mx := s.win.stats(); n < 1 || mx < 45*time.Millisecond {
			t.Errorf("window stats: %d held, longest %v: the third frame waited to its bound", n, mx)
		}
	})

	t.Run("discarded while held", func(t *testing.T) {
		s, c, _, _, p := setup(t, proto.RecoveryInvalidate, 10, false)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- frame(seq)
			s.checkOut()
		}
		waitStreams(t, c, 2, "two frames go out")
		time.Sleep(2 * time.Millisecond)
		held(t, c)
		start := time.Now()
		s.lostFrame(&media.Frame{Gen: 1, Seq: 1}, "test") // the client now waits for the answer to seq 1
		s.checkOut()
		st := waitStreams(t, c, 3, "the held frame discarded")
		if d := time.Since(start); d > 20*time.Millisecond {
			t.Errorf("the discarded frame was released after %v", d)
		}
		if !st[2].cancelled || len(st[2].data) != 0 {
			t.Fatalf("discarded frame: cancelled %v with %d bytes written", st[2].cancelled, len(st[2].data))
		}
		if rec, _, _ := p.state(); len(rec) != 1 || rec[0] != "1/1" {
			t.Fatalf("recover calls %v, want [1/1]", rec)
		}
	})
}

// datagramConn is a transport.Conn whose SendDatagram blocks (the datagram
// queue is full) and whose ReceiveDatagram hands out the queued datagrams.
type datagramConn struct {
	transport.Conn
	in    chan []byte
	reads atomic.Int32
	block chan struct{}
}

func (c *datagramConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-c.in:
		c.reads.Add(1)
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *datagramConn) SendDatagram([]byte) error {
	<-c.block
	return nil
}

// TestPongDoesNotBlockInput: while SendDatagram blocks (quic-go's queue is
// full), the datagram loop keeps reading the client's datagrams (input)
// instead of waiting to send a pong.
func TestPongDoesNotBlockInput(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	c := &datagramConn{in: make(chan []byte, 32), block: make(chan struct{})}
	defer close(c.block)
	s.c = c
	s.pongs = make(chan []byte, 4)
	go s.pongSender()
	go s.datagrams()
	for i := range 20 {
		c.in <- proto.PingDatagram(uint32(i), float64(i), 0)
	}
	for deadline := time.Now().Add(2 * time.Second); c.reads.Load() < 20; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the datagram loop read %d of 20 datagrams: it waits for a pong to be sent", c.reads.Load())
		}
	}
}

// --- Datagram latency behind a video backlog (GUIDE 2.7) ---

// bottleneck relays UDP between one client and a server over a link of a
// fixed capacity in each direction, with a drop-tail queue and a one-way
// delay: the downlink a capacity drop leaves the video, before the rate
// controller has lowered the bitrate. Each packet may also wait a random
// extra delay up to jitter (a busy Wi-Fi channel; first in, first out: a
// packet never overtakes the one before it), and the delay may change
// mid-run (setDelay: a path that moves, a relay fallback).
type bottleneck struct {
	pc, up *net.UDPConn
	client atomic.Pointer[net.UDPAddr]
	bps    float64
	delay  atomic.Int64 // ns
	jitter time.Duration
	maxQ   time.Duration
}

type linkPacket struct {
	due  time.Time
	data []byte
}

type link struct {
	mu   sync.Mutex
	free time.Time // when the link has sent everything queued
	last time.Time // the last packet's due time (FIFO)
	out  chan linkPacket
}

func newBottleneck(t *testing.T, server net.Addr, bps float64, delay, jitter, maxQ time.Duration) *bottleneck {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	up, err := net.DialUDP("udp", nil, server.(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*net.UDPConn{pc, up} {
		_ = c.SetReadBuffer(8 << 20)
		_ = c.SetWriteBuffer(8 << 20)
	}
	b := &bottleneck{pc: pc, up: up, bps: bps, jitter: jitter, maxQ: maxQ}
	b.setDelay(delay)
	toServer := b.link(func(p []byte) { _, _ = up.Write(p) })
	toClient := b.link(func(p []byte) {
		if a := b.client.Load(); a != nil {
			_, _ = pc.WriteToUDP(p, a)
		}
	})
	go b.pump(func(p []byte) (int, error) {
		n, a, err := pc.ReadFromUDP(p)
		if err == nil {
			b.client.Store(a)
		}
		return n, err
	}, toServer)
	go b.pump(up.Read, toClient)
	t.Cleanup(func() { pc.Close(); up.Close() })
	return b
}

// setDelay sets the one-way delay of both directions.
func (b *bottleneck) setDelay(d time.Duration) { b.delay.Store(int64(d)) }

// link delivers the packets in order, each at its due time.
func (b *bottleneck) link(write func([]byte)) *link {
	l := &link{out: make(chan linkPacket, 1<<15)}
	go func() {
		for p := range l.out {
			if d := time.Until(p.due); d > 0 {
				time.Sleep(d)
			}
			write(p.data)
		}
	}()
	return l
}

func (b *bottleneck) pump(read func([]byte) (int, error), l *link) {
	defer close(l.out)
	for {
		p := make([]byte, 2048)
		n, err := read(p)
		if err != nil {
			return
		}
		now := time.Now()
		l.mu.Lock()
		start := now
		if l.free.After(now) {
			start = l.free
		}
		if start.Sub(now) > b.maxQ {
			l.mu.Unlock()
			continue // the queue is full: drop
		}
		l.free = start.Add(time.Duration(float64(n*8) / b.bps * float64(time.Second)))
		due := l.free.Add(time.Duration(b.delay.Load()))
		if b.jitter > 0 {
			due = due.Add(rand.N(b.jitter))
		}
		if due.Before(l.last) {
			due = l.last
		}
		l.last = due
		l.mu.Unlock()
		l.out <- linkPacket{due, p[:n]}
	}
}

func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	cert, err := tlsutil.SelfSigned([]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	const alpn = "recon-test"
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}},
		&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{alpn}}
}

// latencyCase is a measurement's path and video: d of streaming through a
// bottleneck of capacity bit/s with oneWay delay (plus up to jitter) each
// way, the delay stepping to stepTo at stepAt (0: no step); frame sizes vary
// uniformly by ± sizeVar of the average.
type latencyCase struct {
	d              time.Duration
	capacity       float64
	oneWay, jitter time.Duration
	stepAt, stepTo time.Duration
	sizeVar        float64
}

type latencyRun struct {
	audio, pong []time.Duration // audio one-way delay, ping round trip
	frame       []time.Duration // frame queued at the host -> its last byte at the client
	frames      int             // frame streams the client read completely
	held        int64           // frames the video window held
}

func pctl(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[int(math.Round(float64(len(s)-1)*p))]
}

// measureDatagramLatency streams c from a host session (frameSender, the
// datagram loop answering pings, the media congestion controller) whose
// encoder produces 20 Mbit/s at 60 fps (a 6-deep frame queue that drops what
// does not fit, as the session's) over real quic-go through a bottleneck
// (300 ms drop-tail queue). Below 20 Mbit/s the frame queue stays full, a
// large video backlog. Audio-sized datagrams go out every 10 ms beside the
// video (as the session's audio does), the client pings every 20 ms. Clock:
// one process, so the audio's one-way delay and the frames' latency are
// exact. window false runs the session without its video window (before
// GUIDE 2.7).
func measureDatagramLatency(t *testing.T, window bool, c latencyCase) latencyRun {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, transport.QUICConfig(transport.WithCongestion(transport.CongestionMedia)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const (
		kbps = 20000
		fps  = 60
	)
	bn := newBottleneck(t, ln.Addr(), c.capacity, c.oneWay, c.jitter, 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), c.d+15*time.Second)
	defer cancel()
	t0 := time.Now()
	since := func() time.Duration { return time.Since(t0) }
	if c.stepAt > 0 {
		step := time.AfterFunc(c.stepAt, func() { bn.setDelay(c.stepTo) })
		defer step.Stop()
	}
	queued := make([]atomic.Int64, int(c.d/(time.Second/fps))+fps) // by seq: when the frame was queued
	warm := 500 * time.Millisecond                                 // the backlog builds

	var run latencyRun
	hostDone := make(chan *Session, 1)
	go func() {
		qc, err := ln.Accept(ctx)
		if err != nil {
			hostDone <- nil
			return
		}
		sctx, scancel := context.WithCancel(ctx)
		s := &Session{
			a: &Agent{hostClock: media.NewHostClock()}, c: transport.FromQUIC(qc), ctx: sctx, cancel: scancel,
			frameQ: make(chan *media.Frame, 6), pongs: make(chan []byte, 4),
			log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		s.hello.V = proto.HelloVersionRecovery
		if !window {
			s.meter = func() deliveryMeter { return nil }
		}
		s.setCongestionTarget(media.Params{BitrateKbps: kbps, FPS: fps})
		go s.frameSender()
		go s.pongSender()
		go s.datagrams()
		stop := time.After(c.d)
		video := time.NewTicker(time.Second / fps)
		audio := time.NewTicker(10 * time.Millisecond)
		defer video.Stop()
		defer audio.Stop()
		avg := kbps * 1000 / 8 / fps
		payload := make([]byte, avg+int(float64(avg)*c.sizeVar))
		for seq := uint32(0); ; {
			select {
			case <-stop:
				time.Sleep(500 * time.Millisecond) // what is in flight arrives
				scancel()
				qc.CloseWithError(0, "")
				hostDone <- s
				return
			case <-video.C:
				n := avg
				if c.sizeVar > 0 {
					n = int(float64(avg) * (1 + c.sizeVar*(2*rand.Float64()-1)))
				}
				if int(seq) < len(queued) {
					queued[seq].Store(int64(since()))
				}
				select {
				case s.frameQ <- &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Data: payload[:n]}:
				default: // the queue is full: the encoder's frame is dropped
				}
				seq++
			case <-audio.C:
				b := make([]byte, 200) // a 10 ms Opus packet and its header
				b[0] = proto.DgAudio
				binary.LittleEndian.PutUint64(b[8:], uint64(since()))
				_ = s.c.SendDatagram(b)
			}
		}
	}()

	qc, err := quic.DialAddr(ctx, bn.pc.LocalAddr().String(), clientTLS, transport.QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer qc.CloseWithError(0, "")
	var mu sync.Mutex
	go func() {
		for {
			st, err := qc.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				b, err := io.ReadAll(st)
				now := since()
				h, _, _, perr := proto.ParseFrame(b)
				if err != nil || perr != nil || len(b) <= proto.FrameHeaderLen {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				run.frames++
				if int(h.Seq) < len(queued) {
					if q := time.Duration(queued[h.Seq].Load()); q > warm {
						run.frame = append(run.frame, now-q)
					}
				}
			}()
		}
	}()
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for id := uint32(0); ; id++ {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			if qc.SendDatagram(proto.PingDatagram(id, float64(since())/1e6, 0)) != nil {
				return
			}
		}
	}()
	for {
		b, err := qc.ReceiveDatagram(ctx)
		if err != nil {
			break
		}
		now := since()
		switch {
		case b[0] == proto.DgAudio && len(b) >= 16:
			if sent := time.Duration(binary.LittleEndian.Uint64(b[8:])); sent > warm {
				run.audio = append(run.audio, now-sent)
			}
		case b[0] == proto.DgPong && len(b) >= 24:
			sent := time.Duration(math.Float64frombits(binary.LittleEndian.Uint64(b[8:])) * 1e6)
			if sent > warm {
				run.pong = append(run.pong, now-sent)
			}
		}
	}
	if s := <-hostDone; s != nil {
		run.held, _ = s.win.stats()
	}
	mu.Lock()
	defer mu.Unlock()
	out := run
	out.frame = slices.Clone(run.frame)
	return out
}

// TestDatagramLatencyBehindVideo measures the audio datagrams' one-way delay
// and the pings' round trip while a large video backlog is queued (20 Mbit/s
// of video into a 10 Mbit/s path, before the rate controller lowers the
// bitrate), without and with the video window (GUIDE 2.7). Without, they wait
// behind the media congestion window's worth of video in the bottleneck
// (pacing at 1.2 x 20 Mbit/s x (min RTT + 2 frame intervals), ~100 ms at
// 10 Mbit/s); with it, behind one to two frames (33 ms each at 10 Mbit/s).
// On paths that carry the video the window holds nothing back: the same
// frames, frame latency and datagram delays either way, on a clean
// 50 Mbit/s path, on a jittery one (Wi-Fi) and on one whose round trip
// grows tenfold mid-stream (a relay fallback). The video throughput stays
// the path's.
func TestDatagramLatencyBehindVideo(t *testing.T) {
	if testing.Short() {
		t.Skip("26 s real-time measurement")
	}
	// Segmentation offload would hand the bottleneck 64 kB batches.
	t.Setenv("QUIC_GO_DISABLE_GSO", "true")
	const oneWay = 5 * time.Millisecond
	log := func(name string, r latencyRun) {
		ms := func(d time.Duration) time.Duration { return d.Round(100 * time.Microsecond) }
		t.Logf("%s: audio one-way p50 %v p95 %v max %v (%d); pong RTT p50 %v p95 %v (%d); frame latency p50 %v p95 %v; %d frames received, %d held by the window",
			name, ms(pctl(r.audio, .5)), ms(pctl(r.audio, .95)), ms(pctl(r.audio, 1)), len(r.audio), ms(pctl(r.pong, .5)),
			ms(pctl(r.pong, .95)), len(r.pong), ms(pctl(r.frame, .5)), ms(pctl(r.frame, .95)), r.frames, r.held)
		if len(r.audio) < 100 || len(r.pong) < 50 {
			t.Fatalf("%s: too few samples", name)
		}
	}
	backlog := latencyCase{d: 3 * time.Second, capacity: 10e6, oneWay: oneWay}
	before := measureDatagramLatency(t, false, backlog)
	log("10 Mbit/s, before (no window)", before)
	after := measureDatagramLatency(t, true, backlog)
	log("10 Mbit/s, after (video window)", after)
	if before.held != 0 || after.held == 0 {
		t.Errorf("window held %d frames before, %d after", before.held, after.held)
	}
	if b, a := pctl(before.audio, .5), pctl(after.audio, .5); a > b*6/10 {
		t.Errorf("audio one-way p50 %v with the window, %v without: want at most 60 %%", a, b)
	}
	if b, a := pctl(before.pong, .5), pctl(after.pong, .5); a > b*6/10 {
		t.Errorf("pong RTT p50 %v with the window, %v without: want at most 60 %%", a, b)
	}
	if after.frames < before.frames*85/100 {
		t.Errorf("%d frames received with the window, %d without: the window must not cost throughput", after.frames, before.frames)
	}

	// Paths that carry the video: no cost.
	for _, c := range []struct {
		name string
		c    latencyCase
	}{
		{"50 Mbit/s", latencyCase{d: 2 * time.Second, capacity: 50e6, oneWay: oneWay}},
		{"200 Mbit/s, jitter 2.5 ms + up to 10 ms each way, frames ±50 %",
			latencyCase{d: 3 * time.Second, capacity: 200e6, oneWay: 2500 * time.Microsecond, jitter: 10 * time.Millisecond, sizeVar: .5}},
		{"200 Mbit/s, round trip 4 ms, 40 ms from 1.5 s",
			latencyCase{d: 3 * time.Second, capacity: 200e6, oneWay: 2 * time.Millisecond, stepAt: 1500 * time.Millisecond, stepTo: 20 * time.Millisecond}},
	} {
		cb := measureDatagramLatency(t, false, c.c)
		log(c.name+", before (no window)", cb)
		ca := measureDatagramLatency(t, true, c.c)
		log(c.name+", after (video window)", ca)
		if b, a := pctl(cb.audio, .5), pctl(ca.audio, .5); a > b+5*time.Millisecond {
			t.Errorf("%s: audio one-way p50 %v with the window, %v without", c.name, a, b)
		}
		if b, a := pctl(cb.frame, .5), pctl(ca.frame, .5); a > b+5*time.Millisecond {
			t.Errorf("%s: frame latency p50 %v with the window, %v without", c.name, a, b)
		}
		if b, a := pctl(cb.frame, .95), pctl(ca.frame, .95); a > b+15*time.Millisecond {
			t.Errorf("%s: frame latency p95 %v with the window, %v without", c.name, a, b)
		}
		if ca.frames < cb.frames*95/100 {
			t.Errorf("%s: %d frames received with the window, %d without", c.name, ca.frames, cb.frames)
		}
	}
}
