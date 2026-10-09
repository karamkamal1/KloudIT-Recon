package host

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/fec"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// shardConn is a meteredConn that also takes datagrams: SendDatagram records
// them and moves the meter's sent position (as if packed at once).
type shardConn struct {
	*meteredConn
	mu  sync.Mutex
	dgs [][]byte
}

func (c *shardConn) SendDatagram(b []byte) error {
	c.mu.Lock()
	c.dgs = append(c.dgs, append([]byte(nil), b...))
	c.mu.Unlock()
	c.m.send(uint64(len(b)))
	return nil
}

// shardFrames reassembles the frames sent as shards so far (seq -> the bytes
// a frame stream would carry) and returns every seq a shard was sent for.
func (c *shardConn) shardFrames(t *testing.T) (map[uint32][]byte, map[uint32]bool) {
	t.Helper()
	c.mu.Lock()
	dgs := append([][]byte(nil), c.dgs...)
	c.mu.Unlock()
	var a fec.Assembler
	frames, seen := map[uint32][]byte{}, map[uint32]bool{}
	for _, d := range dgs {
		sh, err := proto.ParseVideoShard(d)
		if err != nil {
			t.Fatalf("shard: %v", err)
		}
		seen[sh.Seq] = true
		b, err := a.Add(sh)
		if err != nil {
			t.Fatal(err)
		}
		if b != nil {
			frames[sh.Seq] = b
		}
	}
	return frames, seen
}

// fecWindowSession is a session in the "datagram + FEC" mode (host config
// fec on, a client that takes shards, the direct path) on a fake connection
// with a delivery meter (10 ms min RTT) for the video window, at 10 fps;
// short: the path falls short of the pacing rate; partial: the client
// negotiated RESET_STREAM_AT (frame streams after the mode ends).
func fecWindowSession(t *testing.T, recovery string, short, partial bool, faults testFaults, helloV int) (*Session, *shardConn, *fakeMeter, *ladderPipeline) {
	t.Helper()
	s, c, _ := testSession(t, faults)
	m := newFakeMeter(10 * time.Millisecond)
	dc := &shardConn{meteredConn: &meteredConn{c, m, false}}
	s.c = dc
	s.meter = func() deliveryMeter { return m }
	if short {
		shortOf(&s.win, m)
	}
	c.partial, s.partial = partial, partial
	s.hello = proto.Hello{V: helloV, FEC: proto.HelloFECVersion}
	s.meta = SessionMeta{Path: "direct"}
	s.a.cfg = &Config{FEC: FECOn}
	s.fecNacks = make(chan proto.FECNack, 8)
	s.fecInit()
	p := &ladderPipeline{caps: media.PipelineCaps{ForceIDR: true, Recovery: recovery}, events: make(chan media.VideoEvent)}
	s.video = p
	s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: recovery}, 0)
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 10})
	go s.frameSender()
	return s, dc, m, p
}

func waitCond(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
	}
}

// TestFECVideoWindow (GUIDE 2.5 with 2.7): a frame sent as datagram shards
// goes through the video window as a frame stream does. On a path that
// falls short of the pacing rate the third frame waits, none of its shards
// sent, until the two before it are acknowledged; it then goes out with its
// send time stamped at the release (in the data shards' frame header), and
// every frame sent as shards is in flight in the window from its shards'
// start to at least their end. A frame the client would discard (it waits
// for the answer to a loss before it) is not sent once the hold ends.
func TestFECVideoWindow(t *testing.T) {
	frame := func(seq uint32) *media.Frame {
		return &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Data: bytes.Repeat([]byte{byte(seq)}, 3000)}
	}
	t.Run("held until acknowledged", func(t *testing.T) {
		s, dc, m, _ := fecWindowSession(t, proto.RecoveryKeyframe, true, false, testFaults{}, proto.HelloVersionRecovery)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- frame(seq)
		}
		waitCond(t, "two frames as shards", func() bool { f, _ := dc.shardFrames(t); return len(f) == 2 })
		time.Sleep(15 * time.Millisecond) // past the round trip
		if _, seen := dc.shardFrames(t); seen[2] || s.windowSince.Load() == 0 {
			t.Fatalf("the third frame is not held: shards sent %v, holding %v", seen, s.windowSince.Load() != 0)
		}
		s.win.mu.Lock()
		marks := append([]windowMark(nil), s.win.marks...)
		s.win.mu.Unlock()
		if len(marks) != 2 || marks[0].pos <= marks[0].start || marks[1].start < marks[0].pos {
			t.Fatalf("window marks %+v: want the two shard frames in flight, in order", marks)
		}
		m.deliver(marks[0].pos)
		time.Sleep(3 * time.Millisecond)
		if _, seen := dc.shardFrames(t); seen[2] {
			t.Fatal("the third frame went out with the second still in flight")
		}
		release := s.a.clock()
		m.deliver(marks[1].pos)
		waitCond(t, "the third frame after the second was acknowledged", func() bool { f, _ := dc.shardFrames(t); return f[2] != nil })
		frames, _ := dc.shardFrames(t)
		h, _, payload, err := proto.ParseFrame(frames[2])
		if err != nil || h.Seq != 2 || !bytes.Equal(payload, frame(2).Data) {
			t.Fatalf("third frame: %+v %v", h, err)
		}
		if h.SendUs < release {
			t.Errorf("send time %d before the window opened (%d): the hold must count as host queue", h.SendUs, release)
		}
		if n, mx := s.win.stats(); n != 1 || mx < 15*time.Millisecond {
			t.Errorf("window stats: %d held, longest %v", n, mx)
		}
		s.win.mu.Lock()
		last := s.win.marks[len(s.win.marks)-1]
		s.win.mu.Unlock()
		if sent, _ := m.Delivery(); last.pos != sent {
			t.Errorf("third frame's mark at %d, its last shard ends at %d", last.pos, sent)
		}
	})

	t.Run("discarded while held", func(t *testing.T) {
		s, dc, _, p := fecWindowSession(t, proto.RecoveryInvalidate, true, false, testFaults{}, proto.HelloVersionRecovery)
		for seq := uint32(0); seq < 3; seq++ {
			s.frameQ <- frame(seq)
		}
		waitCond(t, "two frames as shards", func() bool { f, _ := dc.shardFrames(t); return len(f) == 2 })
		time.Sleep(2 * time.Millisecond)
		s.lostFrame(&media.Frame{Gen: 1, Seq: 1}, "test") // the client now waits for the answer to seq 1
		// The hold ends at three quarters of the frame's deadline (10 fps:
		// at most 150 ms); the frame is then discarded, not sent.
		waitCond(t, "the hold to end", func() bool { return s.windowSince.Load() == 0 })
		time.Sleep(20 * time.Millisecond)
		if _, seen := dc.shardFrames(t); seen[2] {
			t.Fatal("a frame the client would discard was sent as shards after its hold")
		}
		if rec, _, _ := p.state(); len(rec) != 1 || rec[0] != "1/1" {
			t.Fatalf("recover calls %v, want [1/1]", rec)
		}
	})
}

// TestFECStreamsSwitch: one session sends frames as shards, then (the mode
// ended) on frame streams, with temporal SVC thinning and partial delivery
// (GUIDE 2.4) on: thinned frames go out neither way and the next frame's
// header (shards or stream) carries the thinned mask; frame streams mark
// their header reliable, shard frames have no stream and no boundary; the
// video window counts both.
func TestFECStreamsSwitch(t *testing.T) {
	// thin=every:3:for:1: the 2nd, 5th, ... frame taken is under pressure.
	s, dc, _, _ := fecWindowSession(t, proto.RecoveryKeyframe, false, true, testFaults{thinEvery: 3, thinFor: 1}, proto.HelloVersionThinned)
	c := dc.fakeConn
	frame := func(seq uint32) *media.Frame {
		return &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Discardable: seq == 1 || seq == 4, Data: bytes.Repeat([]byte{byte(seq + 1)}, 2000)}
	}
	for seq := uint32(0); seq < 3; seq++ {
		s.frameQ <- frame(seq)
	}
	waitCond(t, "frames 0 and 2 as shards", func() bool { f, _ := dc.shardFrames(t); return f[0] != nil && f[2] != nil })
	s.fecEnd("test: datagrams failed")
	for seq := uint32(3); seq < 6; seq++ {
		s.frameQ <- frame(seq)
	}
	waitCond(t, "frames 3 and 5 on streams", func() bool {
		st := c.snapshot()
		return len(st) == 2 && st[0].closed && st[1].closed
	})
	time.Sleep(10 * time.Millisecond)
	frames, seen := dc.shardFrames(t)
	if len(frames) != 2 || seen[1] || seen[3] || seen[4] || seen[5] {
		t.Fatalf("shard frames %v: want 0 and 2 only", seen)
	}
	mask := func(b []byte) (uint32, uint64) {
		h, ext, _, err := proto.ParseFrame(b)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := ext.Get(proto.ExtThinned)
		return h.Seq, v
	}
	if seq, m := mask(frames[2]); seq != 2 || m != 1 {
		t.Errorf("shard frame %d: thinned mask %b, want 1 (seq 1)", seq, m)
	}
	st := c.snapshot()
	for i, want := range []struct {
		seq  uint32
		mask uint64
	}{{3, 0b10}, {5, 0b1001}} {
		x := st[i]
		seq, m := mask(x.data)
		_, _, payload, _ := proto.ParseFrame(x.data)
		if seq != want.seq || m != want.mask || x.boundaries != 1 || x.boundary != len(x.data)-len(payload) {
			t.Errorf("stream %d: seq %d mask %b, boundary at %d (%d calls); want seq %d mask %b, the header (%d bytes) marked once",
				i, seq, m, x.boundary, x.boundaries, want.seq, want.mask, len(x.data)-len(payload))
		}
	}
	if n := s.stats.thinned.Load(); n != 2 {
		t.Errorf("%d frames thinned, want 2", n)
	}
	s.win.mu.Lock()
	marks := len(s.win.marks)
	s.win.mu.Unlock()
	if marks != 4 {
		t.Errorf("%d frames in flight in the window, want 4 (2 as shards, 2 on streams)", marks)
	}
}

// sentDatagrams: quic-go queues datagrams, so the meter's sent position can
// be short of a frame's last shard: the mark is at least its shards' end.
// The write's own time is the shards' hand-over less the time the congestion
// window held the sender meanwhile.
func TestWindowSentDatagrams(t *testing.T) {
	m := newFakeMeter(10 * time.Millisecond)
	m.send(100)
	var w videoWindow
	now := time.Now()
	ws := writeStart{pos: 100, at: now, limited: m.WindowLimited()}
	m.limited.Add(int64(5 * time.Millisecond))                // the window held the sender for 5 ms of it
	w.sentDatagrams(m, ws, 600, now.Add(20*time.Millisecond)) // nothing packed yet
	ws = writeStart{pos: 600, at: now, limited: m.WindowLimited()}
	m.send(800)
	w.sentDatagrams(m, ws, 700, now.Add(2*time.Millisecond)) // packed (with other datagrams) past its end
	if len(w.marks) != 2 || w.marks[0].pos != 600 || w.marks[1].pos != 900 {
		t.Fatalf("marks %+v, want positions 600 and 900", w.marks)
	}
	if w.marks[0].own != 15*time.Millisecond || w.marks[1].own != 2*time.Millisecond {
		t.Fatalf("own times %v and %v, want 15 ms and 2 ms", w.marks[0].own, w.marks[1].own)
	}
}

// The shortfall gate on frames sent as shards (GUIDE 2.5 with 2.7's window,
// as TestWindowShortfall for streams): frames delivered at a quarter of the
// pacing rate are a shortfall when their shards were handed over within the
// pacer's time, or when the congestion window held the hand-over; not when
// the sender itself took that long (a CPU-starved send loop: the shards
// leave as late as they are handed over, no queue builds).
func TestWindowShortfallShards(t *testing.T) {
	for _, c := range []struct {
		name         string
		wrote, held  time.Duration
		wantShortage bool
	}{
		{"handed over at the pacer's time", 0, 0, true},
		{"a starved sender's own time", 40 * time.Millisecond, 0, false},
		{"held by the congestion window", 40 * time.Millisecond, 38 * time.Millisecond, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &videoWindow{}
			m := newFakeMeter(10 * time.Millisecond)
			fi := time.Second / 60
			now := time.Now()
			w.room(m, fi, now)
			for range 3 { // 30 000 bytes: 10 ms at the fake's 24 Mbit/s
				start, _ := m.Delivery()
				ws := writeStart{pos: start, at: now, limited: m.WindowLimited()}
				m.send(30_000)
				m.limited.Add(int64(c.held))
				w.sentDatagrams(m, ws, start+30_000, now.Add(c.wrote))
				m.deliverAt(start+1, now.Add(10*time.Millisecond))
				m.deliverAt(start+30_000, now.Add(50*time.Millisecond))
				now = now.Add(max(fi, c.wrote))
				w.room(m, fi, now)
			}
			w.mu.Lock()
			short := w.shortfall()
			w.mu.Unlock()
			if short != c.wantShortage {
				t.Fatalf("shortfall %v, want %v", short, c.wantShortage)
			}
		})
	}
}
