package host

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestThinState: the mask of the 32 frames before a frame, the first frame
// sent from a thinned seq on, episodes, and a new generation starting over.
func TestThinState(t *testing.T) {
	var th thinState
	t0 := time.Unix(1000, 0)
	for _, seq := range []uint32{3, 5, 7, 40} {
		th.left(2, seq, t0, "delay")
	}
	for _, c := range []struct {
		seq  uint32
		want uint32
	}{
		{4, 1}, {6, 1<<2 | 1}, {8, 1<<4 | 1<<2 | 1}, {35, 1<<29 | 1<<27 | 1<<31}, {36, 1<<30 | 1<<28}, {38, 1 << 30}, {39, 1 << 31},
		{40, 0}, {41, 1}, {3, 0},
	} {
		if got := th.mask(2, c.seq); got != c.want {
			t.Errorf("mask of seq %d: %b, want %b", c.seq, got, c.want)
		}
	}
	if th.mask(3, 8) != 0 || th.firstSent(3, 5) != 5 {
		t.Fatal("another generation's frames reported")
	}
	if th.firstSent(2, 5) != 6 || th.firstSent(2, 6) != 6 || th.firstSent(2, 40) != 41 {
		t.Fatal("firstSent")
	}
	// Episodes: frames at most thinEpisodeGap apart are one.
	if _, _, _, ok := th.ended(t0.Add(thinEpisodeGap)); ok {
		t.Fatal("episode ended within the gap")
	}
	if n, d, why, ok := th.ended(t0.Add(thinEpisodeGap + time.Millisecond)); !ok || n != 4 || d != 0 || why != "delay" {
		t.Fatalf("episode end: %d frames %v %q %v", n, d, why, ok)
	}
	if _, _, _, ok := th.ended(t0.Add(time.Hour)); ok {
		t.Fatal("episode reported twice")
	}
	if !th.left(2, 42, t0.Add(time.Second), "queue") || th.left(2, 44, t0.Add(time.Second+100*time.Millisecond), "queue") {
		t.Fatal("episode starts")
	}
	// A new generation starts over.
	th.left(3, 1, t0.Add(2*time.Second), "queue")
	if th.mask(2, 43) != 0 || th.mask(3, 2) != 1 {
		t.Fatal("generation change")
	}
	// At most thinKeep seqs are kept.
	for seq := uint32(10); seq < 10+2*thinKeep; seq += 2 {
		th.left(3, seq, t0, "queue")
	}
	if th.firstSent(3, 1) != 1 || len(th.seqs) != thinKeep {
		t.Fatalf("kept %d seqs", len(th.seqs))
	}
}

// sentFrames parses the frames written to the fake connection: seq and the
// thinned mask (-1: none) per stream, in order. The callers wait for a
// frame's stream to open, so it waits (up to 5 s) for every stream to be
// closed or cancelled: on a busy CPU the sender may still be writing the last
// one.
func sentFrames(t *testing.T, c *fakeConn) (seqs []uint32, masks []int64) {
	t.Helper()
	sts := c.snapshot()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); sts = c.snapshot() {
		done := true
		for _, st := range sts {
			done = done && (st.closed || st.cancelled)
		}
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, st := range sts {
		h, ext, _, err := proto.ParseFrame(st.data)
		if err != nil || !st.closed {
			t.Fatalf("stream: %v closed %v", err, st.closed)
		}
		seqs = append(seqs, h.Seq)
		m := int64(-1)
		if v, ok := ext.Get(proto.ExtThinned); ok {
			m = int64(v)
		}
		masks = append(masks, m)
	}
	return seqs, masks
}

// TestFrameSenderThinning: under (simulated) congestion frameSender leaves
// out only discardable frames (never a key or recovery frame), of clients
// that read the mask and with "svc" on; every frame after one carries the
// mask; nothing is reported dropped or lost.
func TestFrameSenderThinning(t *testing.T) {
	// 12 frames: a key frame, then base / enhancement layers alternating;
	// seq 9 is a recovery frame flagged discardable (the pipelines never do:
	// the session does not rely on it).
	frames := func() []*media.Frame {
		var fs []*media.Frame
		for seq := uint32(0); seq < 12; seq++ {
			f := &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, TemporalLayer: uint8(seq % 2), Discardable: seq%2 == 1,
				Recovery: seq == 9, RefFloor: 8, Data: bytes.Repeat([]byte{byte(seq)}, 50)}
			fs = append(fs, f)
		}
		return fs
	}
	feed := func(t *testing.T, helloV int, svc string, faults testFaults) (*Session, *fakeConn, *fakeCtrl) {
		s, c, ctrl := testSession(t, faults)
		s.a.cfg = &Config{SVC: svc}
		s.hello.V = helloV
		go s.frameSender()
		// One at a time: frames waiting in the queue are pressure of their own.
		for i, f := range frames() {
			s.frameQ <- f
			deadline := time.Now().Add(5 * time.Second)
			for len(c.snapshot())+int(s.stats.thinned.Load()) < i+1 {
				if time.Now().After(deadline) {
					t.Fatalf("frame %d not handled", i)
				}
				time.Sleep(time.Millisecond)
			}
		}
		time.Sleep(20 * time.Millisecond)
		return s, c, ctrl
	}
	run := func(t *testing.T, helloV int, svc string, faults testFaults) ([]uint32, []int64, *Session, *fakeCtrl) {
		s, c, ctrl := feed(t, helloV, svc, faults)
		seqs, masks := sentFrames(t, c)
		return seqs, masks, s, ctrl
	}
	// Frames taken 4..11 (seqs 3..10) under pressure: n % 12 >= 4.
	pressure := testFaults{thinEvery: 12, thinFor: 8}

	t.Run("v4 client", func(t *testing.T) {
		seqs, masks, s, ctrl := run(t, proto.HelloVersionThinned, "", pressure)
		thinned := []uint32{3, 5, 7}
		if fmt.Sprint(seqs) != "[0 1 2 4 6 8 9 10 11]" {
			t.Fatalf("sent %v, want the enhancement frames under pressure (3, 5, 7) left out, the recovery frame 9 sent", seqs)
		}
		for i, seq := range seqs {
			want := int64(-1)
			for _, th := range thinned {
				if th < seq {
					want = max(want, 0) | 1<<(seq-1-th)
				}
			}
			if masks[i] != want {
				t.Errorf("seq %d: mask %b, want %b", seq, masks[i], want)
			}
		}
		if n := s.stats.thinned.Load(); n != 3 {
			t.Fatalf("thinned %d", n)
		}
		if d := ctrl.dropped(t); len(d) != 0 || s.stats.dropped.Load() != 0 {
			t.Fatalf("thinned frames reported dropped: %+v", d)
		}
	})
	t.Run("v3 client", func(t *testing.T) {
		seqs, masks, s, _ := run(t, proto.HelloVersionRecovery, "", pressure)
		if len(seqs) != 12 || s.stats.thinned.Load() != 0 {
			t.Fatalf("an older client was thinned: sent %v", seqs)
		}
		for _, m := range masks {
			if m != -1 {
				t.Fatal("mask sent to an older client")
			}
		}
	})
	t.Run("svc off", func(t *testing.T) {
		if seqs, _, _, _ := run(t, proto.HelloVersionThinned, "off", pressure); len(seqs) != 12 {
			t.Fatalf("thinned with svc off: sent %v", seqs)
		}
	})
	t.Run("test faults", func(t *testing.T) {
		// The hook drops frames 6 and 12 (seqs 5, 11) and delays frame 8
		// (seq 7): enhancement frames under pressure, yet never thinned
		// (the faults stay as configured); seq 3 is.
		f := pressure
		f.dropEvery, f.delayEvery, f.delay = 6, 8, 5*time.Millisecond
		s, c, ctrl := feed(t, proto.HelloVersionThinned, "", f)
		var sent []uint32
		reset := 0
		for _, st := range c.snapshot() {
			if st.cancelled {
				reset++ // half a frame
				continue
			}
			h, _, _, err := proto.ParseFrame(st.data)
			if err != nil || !st.closed {
				t.Fatalf("stream: %v closed %v", err, st.closed)
			}
			sent = append(sent, h.Seq)
		}
		var dropped []uint32
		for _, d := range ctrl.dropped(t) {
			dropped = append(dropped, d.FromSeq)
		}
		if fmt.Sprint(sent) != "[0 1 2 4 6 7 8 9 10]" || reset != 2 || fmt.Sprint(dropped) != "[5 11]" {
			t.Fatalf("sent %v, %d streams reset, dropped %v: want seq 3 thinned, 5 and 11 dropped by the hook, 7 delayed and sent",
				sent, reset, dropped)
		}
		if n := s.stats.thinned.Load(); n != 1 {
			t.Fatalf("thinned %d, want 1 (seq 3)", n)
		}
	})
	t.Run("no pressure", func(t *testing.T) {
		if seqs, _, _, _ := run(t, proto.HelloVersionThinned, "", testFaults{}); len(seqs) != 12 {
			t.Fatalf("thinned without congestion: sent %v", seqs)
		}
	})
}

// TestLateDiscardableFrame: under reference recovery a discardable frame
// (temporal SVC's enhancement layer) whose stream stalls past its deadline
// while newer frames wait is not cancelled: it is sent to the end, nothing is
// reported dropped, the encoder is not asked to recover and no frame waits
// for a recovery frame; thinning leaves out the next discardable frame under
// the backlog the stall left. Before, rung 1 cancelled it as a loss: reported
// dropped, Recover, and the frames up to the recovery frame discarded.
func TestLateDiscardableFrame(t *testing.T) {
	s, c, ctrl := testSession(t, testFaults{})
	s.a.cfg = &Config{} // svc auto
	s.hello.V = proto.HelloVersionThinned
	c.stall = map[int]bool{1: true} // seq 1's stream
	logs := &lockedLog{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	p := &ladderPipeline{caps: media.PipelineCaps{Recovery: proto.RecoveryLTR, ForceIDR: true}, events: make(chan media.VideoEvent)}
	s.video = p
	s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: proto.RecoveryLTR}, 0)
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60}) // deadline 33 ms
	go s.frameSender()
	queue := func(seq uint32) {
		s.frameQ <- &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, TemporalLayer: uint8(seq % 2), Discardable: seq%2 == 1,
			Data: bytes.Repeat([]byte{byte(seq)}, 100)}
		s.checkOut() // as videoEvents does for every frame it queues
	}
	queue(0)
	queue(1) // stalls
	for deadline := time.Now().Add(5 * time.Second); len(c.snapshot()) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("seq 1's stream not opened")
		}
	}
	for seq := uint32(2); seq < 6; seq++ {
		queue(seq) // newer frames wait
	}
	time.Sleep(100 * time.Millisecond) // three times its deadline
	s.checkOut()
	if st := c.snapshot()[1]; st.cancelled {
		t.Fatal("the late discardable frame was cancelled")
	}
	c.release(1)
	deadline := time.Now().Add(5 * time.Second)
	for len(c.snapshot())+int(s.stats.thinned.Load()) < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("frames handled: %d streams, %d thinned", len(c.snapshot()), s.stats.thinned.Load())
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	seqs, masks := sentFrames(t, c)
	if fmt.Sprint(seqs) != "[0 1 2 4 5]" || masks[3] != 1 {
		t.Fatalf("sent %v (masks %v), want every frame but seq 3, thinned under the backlog (seq 4 names it)", seqs, masks)
	}
	if d := ctrl.dropped(t); len(d) != 0 {
		t.Fatalf("dropped reports %+v", d)
	}
	if rec, keys, _ := p.state(); len(rec) != 0 || keys != 0 {
		t.Fatalf("pipeline: recover %v, key frames %d; want neither", rec, keys)
	}
	if n, m := s.stats.cancelled.Load(), s.stats.discarded.Load(); n != 0 || m != 0 {
		t.Fatalf("%d cancelled, %d discarded", n, m)
	}
}

// TestThinPressure: the signals that count as congestion for thinning: the
// rate controller over its delay target, two frames waiting, a frame stream
// past its deadline (or the last one written that slowly).
func TestThinPressure(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	now := time.Now()
	if why := s.thinPressure(1, now); why != "" {
		t.Fatalf("idle session under pressure: %s", why)
	}
	s.rate.mu.Lock()
	s.rate.overRun = thinOverReports
	s.rate.mu.Unlock()
	if why := s.thinPressure(1, now); why != "delay" {
		t.Fatalf("over the delay target: %q", why)
	}
	s.rate.mu.Lock()
	s.rate.overRun = 0
	s.rate.mu.Unlock()
	s.frameQ <- &media.Frame{}
	if why := s.thinPressure(1, now); why != "" {
		t.Fatalf("one frame waiting: %q", why)
	}
	s.frameQ <- &media.Frame{}
	if why := s.thinPressure(1, now); why != "queue" {
		t.Fatalf("two frames waiting: %q", why)
	}
	s.drainQueue()
	of := &outFrame{f: &media.Frame{}, st: &fakeStream{}, opened: now.Add(-30 * time.Millisecond), deadline: 50 * time.Millisecond}
	s.send.register(of)
	if why := s.thinPressure(1, now); why != "" {
		t.Fatalf("a stream within its deadline: %q", why)
	}
	of.opened = now.Add(-60 * time.Millisecond)
	if why := s.thinPressure(1, now); why != "deadline" {
		t.Fatalf("a stream past its deadline: %q", why)
	}
	s.send.finish(of, outDone)
	if why := s.thinPressure(1, now); why != "deadline" {
		t.Fatalf("the last stream written slowly: %q", why)
	}
	quick := &outFrame{f: &media.Frame{}, st: &fakeStream{}, opened: time.Now(), deadline: time.Second}
	s.send.register(quick)
	s.send.finish(quick, outDone)
	if why := s.thinPressure(1, time.Now()); why != "" {
		t.Fatalf("after a quick write: %q", why)
	}
}

// TestThinShardsAfterSlowStream: the deadline pressure follows the frames
// sent. A frame stream written past its deadline just before the session
// switches to datagram + FEC is pressure for the next frame only: the
// frames sent as shards after it, all in time, leave nothing to thin.
func TestThinShardsAfterSlowStream(t *testing.T) {
	s, c, _ := testSession(t, testFaults{})
	c.stall = map[int]bool{0: true}
	s.c = &dgConn{Conn: c}
	s.a.cfg = &Config{} // fec and svc auto
	s.hello = proto.Hello{V: proto.HelloVersionThinned, FEC: proto.HelloFECVersion}
	s.meta = SessionMeta{Path: "direct"}
	s.fecNacks = make(chan proto.FECNack, 64)
	s.fecInit()
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60})
	go s.frameSender()
	// Frame 0 on a stream (no round trip measured yet), written past its
	// deadline.
	s.frameQ <- &media.Frame{Gen: 1, Seq: 0, Key: true, Data: bytes.Repeat([]byte{1}, 5000)}
	time.Sleep(100 * time.Millisecond)
	c.release(0)
	waitCond(t, "frame 0 written", func() bool { st := c.snapshot(); return len(st) == 1 && st[0].closed })
	if !s.send.slow(time.Now()) {
		t.Fatal("the slow frame stream is no deadline pressure")
	}
	// A 40 ms round trip: datagram + FEC. Frame 1 goes as shards; every
	// frame after it is discardable, sent with no congestion at all.
	s.clientRTT.Store(int64(40 * time.Millisecond))
	s.fec.rttReports.Store(fecRTTReports)
	for seq := uint32(1); seq <= 20; seq++ {
		s.frameQ <- &media.Frame{Gen: 1, Seq: seq, Discardable: seq%2 == 0, TemporalLayer: uint8(1 - seq%2), Data: bytes.Repeat([]byte{2}, 3000)}
		waitCond(t, fmt.Sprintf("frame %d sent or thinned", seq), func() bool {
			return s.fec.frames.Load()+s.stats.thinned.Load() == int64(seq)
		})
	}
	if n := s.stats.thinned.Load(); n != 0 || !s.fecActive() || s.fec.frames.Load() != 20 {
		t.Fatalf("thinned %d of 10 discardable frames with no congestion (datagram + FEC %v, %d frames as shards)",
			n, s.fecActive(), s.fec.frames.Load())
	}
}
