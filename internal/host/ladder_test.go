package host

import (
	"bytes"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/fec"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestLadder checks every decision of the loss-recovery ladder (GUIDE 2.3).
func TestLadder(t *testing.T) {
	const late, deadline = 40 * time.Millisecond, 33 * time.Millisecond
	ref := func(mode string) ladderIn { return ladderIn{live: 3, mode: mode, gen: 3, seq: 10} }
	out := func(mode string, mod func(*ladderIn)) ladderIn {
		in := ref(mode)
		in.event, in.age, in.deadline, in.newer = lossOutgoing, late, deadline, true
		if mod != nil {
			mod(&in)
		}
		return in
	}
	ev := func(e lossEvent, mode string, mod func(*ladderIn)) ladderIn {
		in := ref(mode)
		in.event = e
		if mod != nil {
			mod(&in)
		}
		return in
	}
	helper := func(in *ladderIn) { in.forceIDR = true }
	wait := func(w lossWait) func(*ladderIn) { return func(in *ladderIn) { in.wait = w } }
	refWait := lossWait{active: true, gen: 3, from: 8, rung: 2}
	for _, c := range []struct {
		name    string
		in      ladderIn
		rung    int
		act     ladderAction
		restart bool
		ends    bool
	}{
		// Rung 1: only where the loss costs no key frame and no damage.
		{"late, invalidate", out(proto.RecoveryInvalidate, nil), 1, actCancel, false, false},
		{"late, ltr", out(proto.RecoveryLTR, nil), 1, actCancel, false, false},
		{"in time", out(proto.RecoveryLTR, func(in *ladderIn) { in.age = deadline - time.Millisecond }), 0, actNone, false, false},
		{"at the deadline", out(proto.RecoveryLTR, func(in *ladderIn) { in.age = deadline }), 1, actCancel, false, false},
		{"nothing newer", out(proto.RecoveryLTR, func(in *ladderIn) { in.newer = false }), 0, actNone, false, false},
		{"late key frame", out(proto.RecoveryLTR, func(in *ladderIn) { in.key = true }), 0, actNone, false, false},
		{"late recovery frame", out(proto.RecoveryLTR, func(in *ladderIn) { in.recovery, in.refFloor = true, 7 }), 0, actNone, false, false},
		// A discardable frame's loss would cost a recovery round (the
		// client cannot tell it apart); thinning leaves out the next ones.
		{"late discardable frame", out(proto.RecoveryLTR, func(in *ladderIn) { in.discardable = true }), 0, actNone, false, false},
		{"discardable, waiting", out(proto.RecoveryLTR, func(in *ladderIn) { in.discardable, in.wait = true, refWait }), 2, actDiscard, false, false},
		{"late, keyframe", out(proto.RecoveryKeyframe, nil), 0, actNone, false, false},
		{"late, skip", out(proto.RecoverySkip, nil), 0, actNone, false, false},
		{"late, nothing live", out("", func(in *ladderIn) { in.live = 0 }), 0, actNone, false, false},
		{"late, a generation the client left", out(proto.RecoveryLTR, func(in *ladderIn) { in.gen = 2 }), 0, actNone, false, false},
		// Rung 2's wait: the frames after the loss up to its answer.
		{"waiting", out(proto.RecoveryLTR, wait(refWait)), 2, actDiscard, false, false},
		{"waiting, in time", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.age, in.newer = refWait, 0, false }), 2, actDiscard, false, false},
		{"waiting, the lost frame itself", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.seq = refWait, 8 }), 2, actDiscard, false, false},
		{"before the loss", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.seq, in.newer = refWait, 7, false }), 0, actNone, false, false},
		{"the recovery frame", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.recovery, in.refFloor = refWait, true, 7 }), 2, actNone, false, true},
		{"a recovery frame for a later loss", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.recovery, in.refFloor = refWait, true, 8 }), 2, actDiscard, false, false},
		{"a key frame", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.key = refWait, true }), 2, actNone, false, true},
		{"after the answer", out(proto.RecoveryLTR, func(in *ladderIn) {
			in.wait, in.newer = refWait, false
			in.wait.ended, in.wait.end = true, 10
		}), 0, actNone, false, false},
		{"before the answer", out(proto.RecoveryLTR, func(in *ladderIn) {
			in.wait = refWait
			in.wait.ended, in.wait.end = true, 11
		}), 2, actDiscard, false, false},
		{"another generation", out(proto.RecoveryLTR, func(in *ladderIn) { in.wait, in.gen, in.newer = refWait, 4, false }), 0, actNone, false, false},
		{"keyframe wait: an in-stream key frame", out(proto.RecoveryKeyframe, func(in *ladderIn) {
			in.wait, in.key = lossWait{active: true, gen: 3, from: 8, rung: 4, wholeGen: true}, true
		}), 4, actDiscard, false, false},

		// Confirmed losses: rung 2, else 3, else 4.
		{"loss, ltr", ev(lossConfirmed, proto.RecoveryLTR, helper), 2, actRecover, false, false},
		{"loss, invalidate", ev(lossConfirmed, proto.RecoveryInvalidate, helper), 2, actRecover, false, false},
		{"loss, Recover failed", ev(lossConfirmed, proto.RecoveryLTR, func(in *ladderIn) { in.forceIDR, in.recoverFailed = true, true }), 4, actKeyframe, false, false},
		{"key frame lost, ltr", ev(lossConfirmed, proto.RecoveryLTR, func(in *ladderIn) { in.forceIDR, in.seq = true, 0 }), 4, actKeyframe, false, false},
		{"loss, skip", ev(lossConfirmed, proto.RecoverySkip, nil), 3, actHeal, false, false},
		{"loss, keyframe, helper", ev(lossConfirmed, proto.RecoveryKeyframe, helper), 4, actKeyframe, false, false},
		{"loss, keyframe, FFmpeg", ev(lossConfirmed, proto.RecoveryKeyframe, nil), 4, actKeyframe, true, false},
		{"loss, a generation the client left", ev(lossConfirmed, proto.RecoveryLTR, func(in *ladderIn) { in.gen = 2 }), 0, actNone, false, false},
		{"loss, nothing live", ev(lossConfirmed, "", func(in *ladderIn) { in.live = 0 }), 0, actNone, false, false},
		{"overflow, ltr", ev(lossOverflow, proto.RecoveryLTR, helper), 2, actRecover, false, false},
		{"overflow, skip", ev(lossOverflow, proto.RecoverySkip, nil), 4, actKeyframe, true, false},
		{"overflow, keyframe", ev(lossOverflow, proto.RecoveryKeyframe, helper), 4, actKeyframe, false, false},

		// Rung 4: never a restart where the pipeline forces IDRs.
		{"key request, helper", ev(lossKeyRequest, "", helper), 4, actKeyframe, false, false},
		{"key request, FFmpeg", ev(lossKeyRequest, "", nil), 4, actKeyframe, true, false},
		{"unhealed, helper", ev(lossUnhealed, proto.RecoverySkip, helper), 4, actRefresh, false, false},
		{"unhealed, FFmpeg", ev(lossUnhealed, proto.RecoverySkip, nil), 4, actRefresh, true, false},
	} {
		st := ladder(c.in)
		if st.rung != c.rung || st.act != c.act || st.restart != c.restart || st.endsWait != c.ends || st.why == "" {
			t.Errorf("%s: rung %d %s restart %v ends %v (%q), want rung %d %s restart %v ends %v",
				c.name, st.rung, st.act, st.restart, st.endsWait, st.why, c.rung, c.act, c.restart, c.ends)
		}
	}
}

func TestFrameDeadline(t *testing.T) {
	const ms = time.Millisecond
	for _, c := range []struct {
		fps       int
		bytes     int
		pacingBps float64
		want      time.Duration
	}{
		{60, 40_000, 24e6, 33333333},              // 2 frame intervals
		{120, 50_000, 60e6, 25 * ms},              // the 25 ms floor
		{30, 50_000, 12e6, 66666666},              // 2 frame intervals
		{60, 200_000, 24e6, 66666666 + 16666666},  // a large frame: 66.7 ms to send + an interval
		{60, 1_000_000, 0, 33333333},              // pacing unknown
		{0, 1000, 24e6, 33333333},                 // frame rate unknown: 60 fps
		{240, 500_000, 120e6, 33333333 + 4166666}, // 33.3 ms to send + an interval
		{144, 100_000, 120e6, max(25*ms, 6666666+6944444)},
	} {
		interval := time.Duration(0)
		if c.fps > 0 {
			interval = time.Second / time.Duration(c.fps)
		}
		if got := frameDeadline(interval, c.bytes, c.pacingBps); (got - c.want).Abs() > 2*time.Microsecond {
			t.Errorf("%d fps, %d bytes at %.0f bit/s: %v, want %v", c.fps, c.bytes, c.pacingBps, got, c.want)
		}
	}
}

// TestQueueOverflows: the frame queue overflows at frameQueueFrames only
// where the network does not keep up: the frame being sent is past its
// deadline (its own sending time at the pacing rate plus an interval), or
// the path's delay is over the target; or where a backlog does not drain:
// the frame being sent reached the queue more than frameQueueAge ago, or more
// than an interval past the longest recent deadline. Behind a frame paced
// out on schedule (a key frame at a low bitrate) frames wait, up to
// frameQueueMax.
func TestQueueOverflows(t *testing.T) {
	const ms = time.Millisecond
	interval := time.Second / 60
	key := frameDeadline(interval, 200_000, 2.4e6) // 667 ms at 2.4 Mbit/s, + an interval
	small := frameDeadline(interval, 5000, 2.4e6)  // 33 ms
	for _, c := range []struct {
		name string
		q    queueLoad
		want string
	}{
		{"few frames, congested", queueLoad{queued: frameQueueFrames - 1, congested: true, sending: true, busy: time.Second, deadline: key, lag: time.Second, burst: key}, ""},
		{"behind a key frame on schedule", queueLoad{queued: frameQueueFrames, sending: true, busy: 300 * ms, deadline: key, lag: 300 * ms, burst: key}, ""},
		{"behind a key frame on schedule, many", queueLoad{queued: frameQueueMax - 1, sending: true, busy: 600 * ms, deadline: key, lag: 600 * ms, burst: key}, ""},
		{"behind a key frame past its deadline", queueLoad{queued: frameQueueFrames, sending: true, busy: 700 * ms, deadline: key, lag: 700 * ms, burst: key}, "deadline"},
		{"behind a small frame past its deadline", queueLoad{queued: frameQueueFrames, sending: true, busy: 40 * ms, deadline: small, lag: 40 * ms, burst: small}, "deadline"},
		{"behind a small frame in time", queueLoad{queued: frameQueueFrames, sending: true, busy: 20 * ms, deadline: small, lag: 20 * ms, burst: small}, ""},
		{"the delay over the target", queueLoad{queued: frameQueueFrames, congested: true, sending: true, busy: 10 * ms, deadline: key, lag: 10 * ms, burst: key}, "delay"},
		{"the delay over the target, nothing sending", queueLoad{queued: frameQueueFrames, congested: true}, "delay"},
		{"the sender between two frames (it takes the next at once)", queueLoad{queued: frameQueueFrames}, ""},
		{"a stream that does not open", queueLoad{queued: frameQueueFrames, sending: true, busy: 200 * ms, deadline: small, lag: 200 * ms, burst: small}, "deadline"},
		{"at the bound", queueLoad{queued: frameQueueMax, sending: true, busy: 10 * ms, deadline: key, lag: 10 * ms, burst: key}, "full"},
		// The frames after a key frame, each on schedule: they waited
		// about the key frame's sending time, less as the pacer catches up.
		{"after a key frame on schedule", queueLoad{queued: 30, sending: true, busy: 10 * ms, deadline: small, lag: 650 * ms, burst: key}, ""},
		// Frames on schedule that wait ever longer (an encoder that makes
		// more than the pacer sends): no deadline passes, the wait grows.
		{"a backlog that does not drain", queueLoad{queued: frameQueueFrames, sending: true, busy: 10 * ms, deadline: small, lag: 300 * ms, burst: small}, "backlog"},
		{"a backlog under the age bound", queueLoad{queued: frameQueueFrames, sending: true, busy: 10 * ms, deadline: small, lag: 200 * ms, burst: small}, ""},
		{"a backlog past a key frame's allowance", queueLoad{queued: 40, sending: true, busy: 10 * ms, deadline: small, lag: key + interval + 10*ms, burst: key}, "backlog"},
		{"a backlog of few frames", queueLoad{queued: frameQueueFrames - 1, sending: true, busy: 10 * ms, deadline: small, lag: time.Second}, ""},
	} {
		c.q.interval = interval
		if got := queueOverflows(c.q); got != c.want {
			t.Errorf("%s: overflows %q, want %q", c.name, got, c.want)
		}
	}
}

// TestQueueBurst: the backlog bound's allowance is the longest deadline of
// a frame taken in the last frameQueueBurst.
func TestQueueBurst(t *testing.T) {
	var b queueBurst
	t0 := time.Unix(1000, 0)
	if got := b.within(t0); got != 0 {
		t.Fatalf("empty: %v", got)
	}
	b.note(400*time.Millisecond, t0)
	b.note(30*time.Millisecond, t0.Add(time.Second))
	if got := b.within(t0.Add(frameQueueBurst)); got != 400*time.Millisecond {
		t.Errorf("within the window: %v, want the key frame's 400 ms", got)
	}
	if got := b.within(t0.Add(frameQueueBurst + time.Millisecond)); got != 0 {
		t.Errorf("after it: %v, want 0", got)
	}
	b.note(30*time.Millisecond, t0.Add(frameQueueBurst+time.Millisecond))
	if got := b.within(t0.Add(frameQueueBurst + 2*time.Millisecond)); got != 30*time.Millisecond {
		t.Errorf("a new frame after the window: %v, want 30 ms", got)
	}
}

// TestPacedInterval: the congestion window is sized for the frames the
// encoder produces: the session's interval, or a slower source's (up to
// maxPacedInterval).
func TestPacedInterval(t *testing.T) {
	const ms = time.Millisecond
	for _, c := range []struct{ interval, source, want time.Duration }{
		{16 * ms, 0, 16 * ms},
		{16 * ms, 10 * ms, 16 * ms},
		{16 * ms, 83 * ms, 83 * ms},
		{16 * ms, time.Second, maxPacedInterval},
		{500 * ms, time.Second, 500 * ms},
	} {
		if got := pacedInterval(c.interval, c.source); got != c.want {
			t.Errorf("pacedInterval(%v, %v) = %v, want %v", c.interval, c.source, got, c.want)
		}
	}
}

// TestSendStateWait: the wait for the answer to a loss, as frameSender takes
// frames: the frames from the loss on are discarded until the recovery frame
// (or a key frame); a loss known late may already have been answered by a
// frame taken earlier; a newer generation ends the wait; a wait the kept
// frames cannot judge discards nothing.
func TestSendStateWait(t *testing.T) {
	type fr = media.Frame
	take := func(s *sendState, f *fr) ladderAction {
		_, st := s.take(f)
		return st.act
	}
	var s sendState
	for seq := uint32(0); seq < 5; seq++ {
		if a := take(&s, &fr{Gen: 1, Seq: seq, Key: seq == 0}); a != actNone {
			t.Fatalf("seq %d before any loss: %s", seq, a)
		}
	}
	s.setWait(1, 5, 2, false) // frame 5 lost (rung 1: its stream was cancelled)
	if s.wait.ended {
		t.Fatal("wait ended before its answer")
	}
	for seq := uint32(5); seq < 8; seq++ {
		if a := take(&s, &fr{Gen: 1, Seq: seq}); a != actDiscard {
			t.Fatalf("seq %d after the loss: %s, want discard", seq, a)
		}
	}
	if a := take(&s, &fr{Gen: 1, Seq: 8, Recovery: true, RefFloor: 5}); a != actDiscard {
		t.Fatalf("a recovery frame that references the lost frame: %s", a)
	}
	if a := take(&s, &fr{Gen: 1, Seq: 9, Recovery: true, RefFloor: 4}); a != actNone || !s.wait.ended || s.wait.end != 9 {
		t.Fatalf("the recovery frame: %s, wait %+v", a, s.wait)
	}
	if a := take(&s, &fr{Gen: 1, Seq: 10}); a != actNone {
		t.Fatalf("after the recovery frame: %s", a)
	}

	// The client reports a loss at 7 a round trip late: frame 9 answered it
	// already (refFloor 4 < 7); the wait is widened to the older loss.
	s.setWait(1, 7, 2, false)
	if w := s.wait; !w.ended || w.end != 9 || w.from != 5 {
		t.Fatalf("late loss: wait %+v, want from 5, ended at 9", w)
	}
	// A loss at 10 (after the answer): a new wait, frame 10 already out.
	s.setWait(1, 10, 2, false)
	if w := s.wait; w.ended || w.from != 10 {
		t.Fatalf("new loss: wait %+v", w)
	}
	if a := take(&s, &fr{Gen: 1, Seq: 11, Recovery: true, RefFloor: 10}); a != actDiscard {
		t.Fatalf("a recovery frame that references the lost frame: %s", a)
	}
	if a := take(&s, &fr{Gen: 1, Seq: 12, Key: true}); a != actNone || s.wait.end != 12 {
		t.Fatalf("an in-stream key frame: %s, wait %+v", a, s.wait)
	}
	// A loss older than that wait (the client reports it late): the key
	// frame answers it too.
	s.setWait(1, 3, 2, false)
	if w := s.wait; !w.ended || w.end != 12 || w.from != 3 {
		t.Fatalf("an older loss: wait %+v", w)
	}

	// "keyframe": the client gives the generation up; only a newer one ends it.
	s.setWait(1, 13, 4, true)
	if a := take(&s, &fr{Gen: 1, Seq: 13, Key: true}); a != actDiscard {
		t.Fatalf("a key frame of the generation the client gave up: %s", a)
	}
	if a := take(&s, &fr{Gen: 2, Seq: 0, Key: true}); a != actNone || s.wait.active {
		t.Fatalf("the next generation: %s, wait %+v", a, s.wait)
	}
	// A loss in a generation the sender has left already: no wait.
	s.setWait(1, 14, 2, false)
	if s.wait.active {
		t.Fatalf("wait for a generation already left: %+v", s.wait)
	}

	// The answer itself lost (the client's "lost" for a late recovery frame,
	// a failed stream): it answers nothing, the wait reopens from its loss
	// (the client keeps waiting from there too) until the next answer.
	var lost sendState
	for seq := uint32(0); seq < 5; seq++ {
		take(&lost, &fr{Gen: 1, Seq: seq, Key: seq == 0})
	}
	lost.setWait(1, 2, 2, false)
	take(&lost, &fr{Gen: 1, Seq: 5, Recovery: true, RefFloor: 1}) // answers the loss at 2
	take(&lost, &fr{Gen: 1, Seq: 6})
	if from := lost.recoverFrom(1, 5); from != 2 {
		t.Fatalf("the recovery frame lost: recover from %d, want 2 (the wait's first loss)", from)
	}
	if from := lost.recoverFrom(1, 6); from != 6 {
		t.Fatalf("a loss after the answer: recover from %d, want 6", from)
	}
	lost.setWait(1, 5, 2, false)
	if w := lost.wait; w.ended || w.from != 2 {
		t.Fatalf("the recovery frame lost: wait %+v, want from 2, not ended", w)
	}
	if a := take(&lost, &fr{Gen: 1, Seq: 7}); a != actDiscard {
		t.Fatalf("after the lost recovery frame: %s, want discard", a)
	}
	if a := take(&lost, &fr{Gen: 1, Seq: 8, Recovery: true, RefFloor: 1}); a != actNone || lost.wait.end != 8 {
		t.Fatalf("the next recovery frame: %s, wait %+v", a, lost.wait)
	}
	// Its loss reported a round trip late: the recovery frame taken since
	// answers it.
	lost.setWait(1, 5, 2, false)
	if w := lost.wait; !w.ended || w.end != 8 || w.from != 2 {
		t.Fatalf("the recovery frame's loss reported late: wait %+v, want from 2, ended at 8", w)
	}
	// An in-stream key frame lost: it is no answer to its own loss.
	take(&lost, &fr{Gen: 1, Seq: 9, Key: true})
	take(&lost, &fr{Gen: 1, Seq: 10})
	lost.setWait(1, 9, 2, false)
	if w := lost.wait; w.ended || w.from != 9 {
		t.Fatalf("an in-stream key frame lost: wait %+v, want from 9, not ended", w)
	}
	if a := take(&lost, &fr{Gen: 1, Seq: 11}); a != actDiscard {
		t.Fatalf("after the lost key frame: %s, want discard", a)
	}

	// More frames than kept: a loss before them cannot be judged (nothing is
	// discarded); a recent one can.
	var long sendState
	for seq := uint32(0); seq < recentFrames+10; seq++ {
		take(&long, &fr{Gen: 5, Seq: seq})
	}
	long.setWait(5, 3, 2, false)
	if a := take(&long, &fr{Gen: 5, Seq: recentFrames + 10}); a != actNone {
		t.Fatalf("a loss older than the kept frames: %s, wait %+v", a, long.wait)
	}
	long.setWait(5, recentFrames+5, 2, false)
	if a := take(&long, &fr{Gen: 5, Seq: recentFrames + 11}); a != actDiscard {
		t.Fatalf("a recent loss: %s, wait %+v", a, long.wait)
	}
}

// ladderPipeline is a media.Pipeline that records what the ladder asks of it.
type ladderPipeline struct {
	mu        sync.Mutex
	caps      media.PipelineCaps
	recovers  []string // "gen/seq"
	keyframes int
	starts    []bool // urgent
	params    []media.Params
	events    chan media.VideoEvent
	starting  bool // Hurry finds a generation starting (it takes over)
	hurries   int
}

func (p *ladderPipeline) Start(params media.Params, urgent bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, urgent)
	p.params = append(p.params, params)
	return nil
}
func (p *ladderPipeline) Events() <-chan media.VideoEvent { return p.events }
func (p *ladderPipeline) Hurry() (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hurries++
	return p.starting, p.starting
}
func (p *ladderPipeline) Suspend()                      {}
func (p *ladderPipeline) Stop()                         {}
func (p *ladderPipeline) Active() (media.Params, bool)  { return media.Params{FPS: 60}, true }
func (p *ladderPipeline) Current() (media.Params, bool) { return media.Params{FPS: 60}, true }
func (p *ladderPipeline) Gen() uint8                    { return 1 }
func (p *ladderPipeline) ForceKeyframe() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keyframes++
	return nil
}
func (p *ladderPipeline) SetRate(int, int, float64) error { return nil }
func (p *ladderPipeline) Recover(gen uint8, seq uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !proto.RefRecovery(p.caps.Recovery) {
		return media.ErrNoRecovery
	}
	p.recovers = append(p.recovers, fmt.Sprintf("%d/%d", gen, seq))
	return nil
}
func (p *ladderPipeline) Ack(uint8, uint32)                {}
func (p *ladderPipeline) SetFocus(media.Focus) error       { return media.ErrNoROI }
func (p *ladderPipeline) Capabilities() media.PipelineCaps { return p.caps }
func (p *ladderPipeline) state() ([]string, int, []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.recovers...), p.keyframes, append([]bool(nil), p.starts...)
}

// TestFrameSenderLadder runs frameSender against stalled frame streams (a
// write that does not progress, as when the congestion window is full)
// with a newer frame queued behind: under reference recovery the stalled
// stream is cancelled at its deadline (rung 1), reported dropped, the encoder
// is asked to recover (rung 2), the frames up to the recovery frame are not
// sent (reported too), the recovery frame and the frames after it are. Key
// frames and recovery frames are never cancelled; under "keyframe" and
// "skip" a late frame goes on. The test hook's delay is a stalled stream
// too. A pipeline that cannot recover gets a key frame, never a restart.
func TestFrameSenderLadder(t *testing.T) {
	type rig struct {
		s    *Session
		c    *fakeConn
		ctrl *fakeCtrl
		p    *ladderPipeline
		logs *lockedLog
	}
	// setup: the client was told mode; the pipeline (caps, Recovery mode
	// if empty) streams generation 1 at 60 fps.
	setup := func(t *testing.T, mode string, stall []int, faults testFaults, caps media.PipelineCaps) rig {
		s, c, ctrl := testSession(t, faults)
		c.stall = map[int]bool{}
		for _, i := range stall {
			c.stall[i] = true
		}
		logs := &lockedLog{}
		s.log = slog.New(slog.NewTextHandler(logs, nil))
		if caps.Recovery == "" {
			caps.Recovery = mode
		}
		p := &ladderPipeline{caps: caps, events: make(chan media.VideoEvent)}
		s.video = p
		s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: mode}, 0)
		s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60}) // deadline 33 ms
		go s.frameSender()
		return rig{s: s, c: c, ctrl: ctrl, p: p, logs: logs}
	}
	// queue sends frames seq from..to-1 of generation 1 (seq 0 a key frame).
	queue := func(r rig, from, to uint32, mod func(*media.Frame)) {
		for seq := from; seq < to; seq++ {
			f := &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Data: bytes.Repeat([]byte{byte(seq)}, 100)}
			if mod != nil {
				mod(f)
			}
			r.s.frameQ <- f
			r.s.checkOut() // as videoEvents does for every frame it queues
		}
	}
	// streams waits for n streams that are done (closed or reset) and
	// returns their header's seq (-1: not parsable) and state.
	streams := func(t *testing.T, r rig, n int) []streamState {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			st := r.c.snapshot()
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
				t.Fatalf("%d of %d frame streams done: %+v", done, n, st)
			}
		}
	}
	seqOf := func(x streamState) int {
		h, _, _, err := proto.ParseFrame(x.data)
		if err != nil {
			return -1
		}
		return int(h.Seq)
	}
	// waitDropped waits for reports of n frames in all.
	waitDropped := func(t *testing.T, r rig, n int) []proto.Dropped {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			d := r.ctrl.dropped(t)
			frames := 0
			for _, m := range d {
				frames += m.Count
			}
			if frames >= n {
				return d
			}
			if time.Now().After(deadline) {
				t.Fatalf("dropped reports %+v, want %d", r.ctrl.dropped(t), n)
			}
		}
	}
	droppedSeqs := func(d []proto.Dropped) string {
		var seqs []int
		for _, m := range d {
			for i := 0; i < m.Count; i++ {
				seqs = append(seqs, int(m.FromSeq)+i)
			}
		}
		sort.Ints(seqs)
		return fmt.Sprint(seqs)
	}

	t.Run("invalidate", func(t *testing.T) {
		r := setup(t, proto.RecoveryInvalidate, []int{2}, testFaults{}, media.PipelineCaps{ForceIDR: true})
		start := time.Now()
		queue(r, 0, 5, nil) // seq 2 stalls; 3 and 4 are newer
		st := streams(t, r, 3)
		if x := st[2]; !x.cancelled || x.closed {
			t.Fatalf("stalled stream: %+v", x)
		}
		if at := st[2].doneAt.Sub(start); at < 30*time.Millisecond || at > 500*time.Millisecond {
			t.Errorf("stalled stream cancelled after %v, want its deadline (33 ms)", at)
		}
		d := waitDropped(t, r, 3)
		if got := droppedSeqs(d); got != "[2 3 4]" || len(d) != 2 {
			t.Fatalf("dropped %s in %d reports, want 2 (cancelled) and 3, 4 (waiting for the recovery frame, one run)", got, len(d))
		}
		if l := r.logs.lines(`msg="frames dropped" why="awaiting recovery frame" gen=1 from_seq=3 count=2`); len(l) != 1 {
			t.Fatalf("discard log %q", r.logs.lines(`msg="frames dropped"`))
		}
		if rec, keys, starts := r.p.state(); len(rec) != 1 || rec[0] != "1/2" || keys != 0 || len(starts) != 0 {
			t.Fatalf("pipeline: recover %v, key frames %d, starts %v; want one recover of 1/2", rec, keys, starts)
		}
		// The recovery frame (refFloor 1 < 2) and the frames after it go out.
		queue(r, 5, 6, func(f *media.Frame) { f.Recovery, f.RefFloor = true, 1 })
		queue(r, 6, 8, nil)
		st = streams(t, r, 6)
		var sent []int
		for _, x := range st {
			if x.closed {
				sent = append(sent, seqOf(x))
			}
		}
		if fmt.Sprint(sent) != "[0 1 5 6 7]" {
			t.Fatalf("frames sent %v, want [0 1 5 6 7]", sent)
		}
		if l := r.logs.lines(`msg="frame stream cancelled" gen=1 seq=2 why="past its deadline"`); len(l) != 1 {
			t.Fatalf("cancel log %q", r.logs.lines(`msg="frame stream cancelled"`))
		}
		if n, m := r.s.stats.cancelled.Load(), r.s.stats.discarded.Load(); n != 1 || m != 2 {
			t.Fatalf("counters: %d cancelled, %d discarded; want 1 and 2", n, m)
		}
	})

	// The recovery frame itself lost (the client's "lost" for it: its drop
	// test, a failed stream): the client waits from the first loss again, so
	// the encoder is asked to recover from there, and its answer (refFloor
	// below the first loss) goes out with the frames after it. Asked to
	// recover from the lost answer, the encoder's frame (refFloor 4) would be
	// discarded with every frame after it.
	t.Run("recovery frame lost", func(t *testing.T) {
		r := setup(t, proto.RecoveryInvalidate, []int{2}, testFaults{}, media.PipelineCaps{ForceIDR: true})
		// recovery queues the encoder's answer to its latest Recover as
		// frame seq: refFloor = the frame before the loss it was asked to
		// recover from (Video's test recovery, reference invalidation).
		recovery := func(seq uint32) {
			rec, _, _ := r.p.state()
			var gen, from uint32
			if len(rec) == 0 {
				t.Fatal("no Recover")
			}
			if _, err := fmt.Sscanf(rec[len(rec)-1], "%d/%d", &gen, &from); err != nil {
				t.Fatal(err)
			}
			queue(r, seq, seq+1, func(f *media.Frame) { f.Recovery, f.RefFloor = true, from-1 })
		}
		queue(r, 0, 5, nil) // seq 2 stalls: cancelled, the encoder recovers from 2; 3, 4 not sent
		streams(t, r, 3)
		waitDropped(t, r, 3)
		recovery(5)
		queue(r, 6, 7, nil)
		streams(t, r, 5)
		r.s.loss(lossConfirmed, 1, 5, "client") // the client lost the recovery frame
		if rec, _, _ := r.p.state(); fmt.Sprint(rec) != "[1/2 1/2]" {
			t.Fatalf("recover %v, want [1/2 1/2] (the lost answer reopens the wait from 2)", rec)
		}
		if l := r.logs.lines(`msg="recovering from a loss" gen=1 from_seq=5 why=client wait_from=2`); len(l) != 1 {
			t.Fatalf("log %q", r.logs.lines(`msg="recovering from a loss"`))
		}
		queue(r, 7, 8, nil) // not sent: the client waits
		recovery(8)
		queue(r, 9, 10, nil)
		st := streams(t, r, 7)
		var sent []int
		for _, x := range st {
			if x.closed {
				sent = append(sent, seqOf(x))
			}
		}
		if fmt.Sprint(sent) != "[0 1 5 6 8 9]" {
			t.Fatalf("frames sent %v, want [0 1 5 6 8 9]", sent)
		}
	})

	t.Run("key and recovery frames go on", func(t *testing.T) {
		r := setup(t, proto.RecoveryLTR, []int{0, 2}, testFaults{}, media.PipelineCaps{ForceIDR: true})
		queue(r, 0, 1, nil)                                                       // the key frame stalls
		queue(r, 1, 2, nil)                                                       // a newer frame waits
		time.Sleep(80 * time.Millisecond)                                         // past the deadline
		r.c.release(0)                                                            // the key frame completes
		queue(r, 2, 3, func(f *media.Frame) { f.Recovery, f.RefFloor = true, 0 }) // a recovery frame stalls
		queue(r, 3, 4, nil)
		time.Sleep(80 * time.Millisecond)
		r.c.release(2)
		st := streams(t, r, 4)
		for i, x := range st {
			if !x.closed || x.cancelled {
				t.Errorf("stream %d: %+v, want sent", i, x)
			}
		}
		if d := r.ctrl.dropped(t); len(d) != 0 {
			t.Fatalf("dropped %+v", d)
		}
	})

	for _, mode := range []string{proto.RecoveryKeyframe, proto.RecoverySkip} {
		t.Run("late frames go on, "+mode, func(t *testing.T) {
			r := setup(t, mode, []int{1}, testFaults{}, media.PipelineCaps{})
			queue(r, 0, 3, nil)
			time.Sleep(100 * time.Millisecond)
			r.c.release(1)
			st := streams(t, r, 3)
			for i, x := range st {
				if !x.closed || x.cancelled {
					t.Errorf("stream %d: %+v, want sent", i, x)
				}
			}
			if d := r.ctrl.dropped(t); len(d) != 0 {
				t.Fatalf("dropped %+v", d)
			}
		})
	}

	// The test hook's delay (browser E2E): the frame's stream stands still
	// for 200 ms while the next frames go out; it is cancelled at its
	// deadline and never written afterwards.
	t.Run("test hook delay", func(t *testing.T) {
		r := setup(t, proto.RecoveryInvalidate, nil, testFaults{delayEvery: 2, delay: 200 * time.Millisecond}, media.PipelineCaps{ForceIDR: true})
		queue(r, 0, 3, nil) // frame 2 (seq 1) is delayed
		st := streams(t, r, 3)
		if x := st[1]; !x.cancelled || len(x.data) != 0 {
			t.Fatalf("delayed stream %+v", x)
		}
		time.Sleep(250 * time.Millisecond) // the hook's write would come now
		if x := r.c.snapshot()[1]; len(x.data) != 0 || x.closed {
			t.Fatalf("delayed stream written after its cancel: %+v", x)
		}
		if got := droppedSeqs(waitDropped(t, r, 1)); got != "[1]" {
			t.Fatalf("dropped %s, want 1", got)
		}
		if rec, _, _ := r.p.state(); len(rec) != 1 || rec[0] != "1/1" {
			t.Fatalf("recover %v", rec)
		}
	})

	// The encoder cannot recover (Recover fails): rung 4, an IDR in the
	// encoder (the helper), and the frames up to it are not sent.
	t.Run("no recovery possible", func(t *testing.T) {
		r := setup(t, proto.RecoveryLTR, []int{1}, testFaults{}, media.PipelineCaps{ForceIDR: true, Recovery: proto.RecoveryKeyframe})
		queue(r, 0, 4, nil)
		streams(t, r, 2)
		if got := droppedSeqs(waitDropped(t, r, 3)); got != "[1 2 3]" {
			t.Fatalf("dropped %s", got)
		}
		if _, keys, starts := r.p.state(); keys != 1 || len(starts) != 0 {
			t.Fatalf("key frames %d, starts %v: want one IDR, no restart", keys, starts)
		}
		if l := r.logs.lines(`msg="no recovery frame possible, forcing a key frame" gen=1 from_seq=1`); len(l) != 1 {
			t.Fatalf("log %q", l)
		}
	})

	// "keyframe" on the helper: a frame whose stream failed costs an IDR in
	// the encoder at once (the host does not wait for the client's request;
	// on FFmpeg a new generation: internal/e2e, the browser E2E), and the
	// rest of the generation is not sent (the client gives it up).
	t.Run("keyframe loss", func(t *testing.T) {
		r := setup(t, proto.RecoveryKeyframe, nil, testFaults{dropEvery: 2}, media.PipelineCaps{ForceIDR: true})
		queue(r, 0, 4, func(f *media.Frame) { f.Key = f.Seq == 0 || f.Seq == 3 })
		if got := droppedSeqs(waitDropped(t, r, 3)); got != "[1 2 3]" {
			t.Fatalf("dropped %s (an in-stream key frame of the given-up generation included)", got)
		}
		if _, keys, starts := r.p.state(); keys != 1 || len(starts) != 0 {
			t.Fatalf("key frames %d, starts %v: want one IDR, no restart", keys, starts)
		}
		if n := r.s.stats.keyframes.Load(); n != 1 {
			t.Fatalf("key frame counter %d", n)
		}
	})
}

// TestDiscardRun: consecutive discards of a generation are one report; a gap,
// another generation or another reason ends the run; a run reported already
// is not reported again (the timer of a run that a sent frame ended).
func TestDiscardRun(t *testing.T) {
	seqs := func(fs []*media.Frame) string {
		var out []string
		for _, f := range fs {
			out = append(out, fmt.Sprintf("%d/%d", f.Gen, f.Seq))
		}
		return fmt.Sprint(out)
	}
	var r discardRun
	done, _, first := r.add(1, 3, "awaiting recovery frame")
	if done != nil || first == 0 {
		t.Fatalf("first discard: done %v, begun %d", seqs(done), first)
	}
	for seq := uint32(4); seq < 6; seq++ {
		if done, _, begun := r.add(1, seq, "awaiting recovery frame"); done != nil || begun != 0 {
			t.Fatalf("seq %d: done %v, begun %d; want the run continued", seq, seqs(done), begun)
		}
	}
	done, why, second := r.add(1, 8, "awaiting recovery frame") // a gap
	if seqs(done) != "[1/3 1/4 1/5]" || why != "awaiting recovery frame" || second == 0 || second == first {
		t.Fatalf("after a gap: done %v (%s), begun %d", seqs(done), why, second)
	}
	if fs, _ := r.take(first); fs != nil {
		t.Fatalf("the first run reported twice: %v", seqs(fs))
	}
	if done, _, _ := r.add(2, 9, "awaiting recovery frame"); seqs(done) != "[1/8]" {
		t.Fatalf("another generation: done %v", seqs(done))
	}
	if done, _, _ := r.add(2, 10, "awaiting key frame"); seqs(done) != "[2/9]" {
		t.Fatalf("another reason: done %v", seqs(done))
	}
	if fs, why := r.take(0); seqs(fs) != "[2/10]" || why != "awaiting key frame" {
		t.Fatalf("take: %v (%s)", seqs(fs), why)
	}
	if fs, _ := r.take(0); fs != nil {
		t.Fatalf("nothing open, took %v", seqs(fs))
	}
}

// TestOverflowRecovered: a frame-queue overflow within 2 s of the last
// decrease (the bitrate cut is refused) on a pipeline whose bitrate changes
// are new generations (no live bitrate). Rung 2 answers the dropped frames
// with a recovery frame: no IDR; a generation starting at a lower bitrate
// (an overlapped back-off) takes over at once. Without rung 2 ("keyframe")
// the overflow gets its key frame.
func TestOverflowRecovered(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		starting   bool
		recovers   string
		keys       int
	}{
		{"ltr", proto.RecoveryLTR, false, "[1/2]", 0},
		{"ltr, a generation starting", proto.RecoveryLTR, true, "[1/2]", 0},
		{"keyframe", proto.RecoveryKeyframe, false, "[]", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := testSession(t, testFaults{})
			logs := &lockedLog{}
			s.log = slog.New(slog.NewTextHandler(logs, nil))
			p := &ladderPipeline{caps: media.PipelineCaps{Recovery: c.mode, ForceIDR: true}, events: make(chan media.VideoEvent),
				starting: c.starting}
			s.video = p
			s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: c.mode}, 0)
			s.rate.mu.Lock()
			s.rate.applied, s.rate.est, s.rate.lastDecrease = 20000, 20000, time.Now() // a cut moments ago
			s.rate.mu.Unlock()
			go s.videoEvents()
			frame := func(seq uint32) { p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: seq, Key: seq == 0}} }
			for seq := uint32(0); seq < 2; seq++ { // the client got the key frame and frame 1
				frame(seq)
				<-s.frameQ
			}
			for seq := uint32(2); seq < 10; seq++ { // nobody takes 2-7; 8 overflows the queue (6)
				frame(seq) // returns once the previous frame is handled
			}
			if l := logs.lines(`msg="frame queue overflow" frames=7`); len(l) != 1 {
				t.Fatalf("overflow log %q", logs.lines(`msg="frame queue overflow"`))
			}
			if l := logs.lines(`msg="congestion: lowering bitrate"`); len(l) != 0 {
				t.Fatalf("cut within 2 s of the last: %q", l)
			}
			rec, keys, starts := p.state()
			if fmt.Sprint(rec) != c.recovers || keys != c.keys || len(starts) != 0 {
				t.Fatalf("recover %v, key frames %d, starts %v; want %s, %d, none", rec, keys, starts, c.recovers, c.keys)
			}
			if n := s.stats.keyframes.Load(); n != int64(c.keys) {
				t.Fatalf("key frame counter %d, want %d", n, c.keys)
			}
			p.mu.Lock()
			hurries := p.hurries
			p.mu.Unlock()
			if hurries != 1 {
				t.Fatalf("%d Hurry calls, want 1 (a starting generation takes over)", hurries)
			}
			if l := logs.lines(`msg="restarting video" reason="queue overflow" urgent=true takeover=true`); len(l) != btoi(c.starting) {
				t.Fatalf("takeover log %q", l)
			}
		})
	}
}

// TestFrameQueueBehindKeyFrame runs videoEvents and frameSender with a key
// frame whose write stands still (a large key frame paced out at a low
// bitrate). Frames queue behind it without an overflow while it is on
// schedule and the path's delay is in its target, and all go out once it is
// written: no recovery, no key frame. With the delay over the target, or
// with the key frame past its deadline, the queue overflows at
// frameQueueFrames; the dropped frames follow the key frame still being
// sent, so no recovery frame is possible and the key frame they cost is
// forced once that one's write is done, and only one however often it is
// asked for meanwhile.
func TestFrameQueueBehindKeyFrame(t *testing.T) {
	type rig struct {
		s    *Session
		c    *fakeConn
		p    *ladderPipeline
		logs *lockedLog
	}
	setup := func(t *testing.T, keyBytes int) rig {
		s, c, _ := testSession(t, testFaults{})
		s.frameQ = make(chan *media.Frame, frameQueueMax)
		c.stall = map[int]bool{0: true}
		logs := &lockedLog{}
		s.log = slog.New(slog.NewTextHandler(logs, nil))
		p := &ladderPipeline{caps: media.PipelineCaps{Recovery: proto.RecoveryLTR, ForceIDR: true, LiveBitrate: true},
			events: make(chan media.VideoEvent)}
		s.video = p
		s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: proto.RecoveryLTR}, 0)
		s.setCongestionTarget(media.Params{BitrateKbps: 2000, FPS: 60}) // video pacing 2.4 Mbit/s
		s.rate.mu.Lock()
		s.rate.applied, s.rate.est, s.rate.lastDecrease = 2000, 2000, time.Now() // no cut: the key frame alone
		s.rate.mu.Unlock()
		go s.videoEvents()
		go s.frameSender()
		p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: 0, Key: true, Data: make([]byte, keyBytes)}}
		for deadline := time.Now().Add(5 * time.Second); !s.keyBeingSent(1, 1) || len(c.snapshot()) == 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the key frame is not being written")
			}
		}
		return rig{s: s, c: c, p: p, logs: logs}
	}
	frames := func(r rig, from, to uint32) {
		for seq := from; seq < to; seq++ {
			r.p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: seq, Data: bytes.Repeat([]byte{byte(seq)}, 100)}}
		}
	}
	// waitLog waits for n log lines with substr (videoEvents takes a frame
	// before it handles it).
	waitLog := func(t *testing.T, r rig, substr string, n int) []string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			if l := r.logs.lines(substr); len(l) >= n {
				return l
			} else if time.Now().After(deadline) {
				t.Fatalf("log %q, want %d lines with %s", r.logs.lines(""), n, substr)
			}
		}
	}
	waitKeys := func(t *testing.T, r rig, n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			if _, keys, _ := r.p.state(); keys == n {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("%d key frames, want %d", keys, n)
			}
		}
	}

	t.Run("on schedule", func(t *testing.T) {
		r := setup(t, 300_000) // 1 s at 2.4 Mbit/s
		frames(r, 1, 21)
		for deadline := time.Now().Add(5 * time.Second); len(r.s.frameQ) < 20 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		if l := r.logs.lines(`msg="frame queue overflow"`); len(l) != 0 || len(r.s.frameQ) != 20 {
			t.Fatalf("%d frames queued, overflow %q; want all 20 queued", len(r.s.frameQ), l)
		}
		r.c.release(0)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			closed := 0
			for _, x := range r.c.snapshot() {
				if x.closed {
					closed++
				}
			}
			if closed == 21 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of 21 frames sent", closed)
			}
		}
		if rec, keys, starts := r.p.state(); len(rec) != 0 || keys != 0 || len(starts) != 0 {
			t.Fatalf("recover %v, key frames %d, starts %v; want none", rec, keys, starts)
		}
	})

	for _, c := range []struct {
		name     string
		keyBytes int
		set      func(r rig)
	}{
		{"the delay over the target", 300_000, func(r rig) {
			r.s.rate.mu.Lock()
			r.s.rate.qdOver = true
			r.s.rate.mu.Unlock()
		}},
		{"past its deadline", 100, func(rig) { time.Sleep(60 * time.Millisecond) }}, // deadline 33 ms
	} {
		t.Run(c.name, func(t *testing.T) {
			r := setup(t, c.keyBytes)
			c.set(r)
			frames(r, 1, uint32(frameQueueFrames)+2)
			if l := waitLog(t, r, `msg="frame queue overflow"`, 1); len(l) != 1 || !strings.Contains(l[0], "frames=7") {
				t.Fatalf("overflow log %q, want 7 frames dropped", l)
			}
			waitLog(t, r, `msg="key frame deferred: one is being sent"`, 1)
			if l := r.logs.lines(`msg="no recovery frame possible, forcing a key frame" gen=1 from_seq=1`); len(l) != 1 ||
				!strings.Contains(l[0], "a key frame before the loss is still being sent") {
				t.Fatalf("ladder log %q", l)
			}
			if l := r.logs.lines(`msg="key frame deferred: one is being sent"`); len(l) != 1 {
				t.Fatalf("deferral log %q", l)
			}
			r.s.requestKeyframe("frame lost") // the client's request for the same loss
			if err := r.s.keyframe("decoder error"); err != nil {
				t.Fatal(err)
			}
			if rec, keys, _ := r.p.state(); len(rec) != 0 || keys != 0 {
				t.Fatalf("recover %v, %d key frames while the key frame is being written; want none", rec, keys)
			}
			r.c.release(0)
			waitKeys(t, r, 1)
			time.Sleep(50 * time.Millisecond)
			if _, keys, starts := r.p.state(); keys != 1 || len(starts) != 0 {
				t.Fatalf("%d key frames, starts %v after the write; want one IDR", keys, starts)
			}
			if l := r.logs.lines(`msg="key frame deferred: one is being sent"`); len(l) != 1 {
				t.Fatalf("deferral logged %d times, want once", len(l))
			}
		})
	}

	// Past its deadline above the floor, right after another decrease: the
	// overflow cuts all the same (signalKeyOverflow), so that the key frame
	// forced after this one comes at a bitrate the path carried; a second
	// overflow behind the same key frame does not cut again.
	t.Run("past its deadline, a cut once per key frame", func(t *testing.T) {
		r := setup(t, 100)
		r.s.rate.mu.Lock()
		r.s.rate.applied, r.s.rate.est = 8000, 8000
		r.s.rate.mu.Unlock()
		time.Sleep(60 * time.Millisecond) // deadline 33 ms
		frames(r, 1, uint32(frameQueueFrames)+2)
		if l := waitLog(t, r, `msg="frame queue overflow"`, 1); !strings.Contains(l[0], "why=deadline") || !strings.Contains(l[0], "sender_kind=key") ||
			!strings.Contains(l[0], "key_cut=true") {
			t.Fatalf("overflow log %q, want why=deadline behind a key frame, cut", l)
		}
		if cur, _ := r.s.rate.kbps(); cur != 6000 {
			t.Fatalf("target %d after the overflow behind the key frame, want 6000 (0.75 x, within the gap of the last decrease)", cur)
		}
		frames(r, uint32(frameQueueFrames)+2, 2*uint32(frameQueueFrames)+3)
		if l := waitLog(t, r, `msg="frame queue overflow"`, 2); strings.Contains(l[1], "key_cut") {
			t.Fatalf("second overflow log %q, want no second cut", l[1])
		}
		if cur, _ := r.s.rate.kbps(); cur != 6000 {
			t.Fatalf("target %d after a second overflow behind the same key frame, want 6000", cur)
		}
		r.c.release(0)
	})
}

// TestFrameQueueBacklog: a frame sent within its deadline that the encoder
// delivered more than frameQueueAge ago (an encoder that makes more than the
// pacer sends: every frame on schedule, the wait growing) overflows the
// queue at frameQueueFrames ("backlog"); one that waited less does not.
func TestFrameQueueBacklog(t *testing.T) {
	for _, c := range []struct {
		name     string
		waited   time.Duration
		overflow bool
	}{
		{"waited 400 ms", 400 * time.Millisecond, true},
		{"waited 100 ms", 100 * time.Millisecond, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, conn, _ := testSession(t, testFaults{})
			s.frameQ = make(chan *media.Frame, frameQueueMax)
			conn.stall = map[int]bool{0: true}
			logs := &lockedLog{}
			s.log = slog.New(slog.NewTextHandler(logs, nil))
			p := &ladderPipeline{caps: media.PipelineCaps{Recovery: proto.RecoveryLTR, ForceIDR: true, LiveBitrate: true},
				events: make(chan media.VideoEvent)}
			s.video = p
			s.healConfig(&proto.VideoConfig{Gen: 1, Recovery: proto.RecoveryLTR}, 0)
			s.setCongestionTarget(media.Params{BitrateKbps: 2000, FPS: 60}) // 45 KB: 150 ms at 2.4 Mbit/s, deadline 167 ms
			go s.videoEvents()
			go s.frameSender()
			done := s.a.clock()
			time.Sleep(c.waited) // the frame waited this long since the encoder delivered it
			p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: 0, EncodeDoneUs: done, Data: make([]byte, 45000)}}
			for deadline := time.Now().Add(5 * time.Second); len(conn.snapshot()) == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the frame is not being written")
				}
			}
			for seq := uint32(1); seq <= frameQueueFrames+1; seq++ {
				p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: seq, EncodeDoneUs: s.a.clock(), Data: bytes.Repeat([]byte{byte(seq)}, 100)}}
			}
			for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
				l := logs.lines(`msg="frame queue overflow"`)
				switch {
				case c.overflow && len(l) > 0:
					if !strings.Contains(l[0], "why=backlog") || !strings.Contains(l[0], "frames=7") {
						t.Fatalf("overflow log %q, want 7 frames for a backlog", l)
					}
					conn.release(0)
					return
				case !c.overflow && len(l) > 0:
					t.Fatalf("overflow %q, want none", l)
				case !c.overflow && len(s.frameQ) == frameQueueFrames+1:
					conn.release(0)
					return
				case time.Now().After(deadline):
					t.Fatalf("%d frames queued, overflow %q", len(s.frameQ), l)
				}
			}
		})
	}
}

// TestQueueOverflowsShards: a frame sent as datagram shards is judged by its
// bytes on the wire (parity and shard headers: Session.sending in sendFEC).
// A 25-budget key frame at 2 Mbit/s whose shards go out on schedule overflows
// nothing; judged by its stream bytes, its own sending time would be past
// that deadline with 18 frames queued behind it, and the storm of key frames
// the queue rule ends would come back on every path with auto FEC (the UDP
// relay).
func TestQueueOverflowsShards(t *testing.T) {
	const stream = 104254 // bytes: 25 x the budget at 2 Mbit/s, 60 fps
	var enc fec.Encoder
	_, dgs, err := enc.Cut(make([]byte, stream), 1, 1, proto.MaxShardPayload, func(k int) int { return fec.Parity(k, 0.02) })
	if err != nil {
		t.Fatal(err)
	}
	wire := 0
	for _, d := range dgs {
		wire += len(d)
	}
	interval := time.Second / 60
	pace := 2.4e6 * float64(wire) / stream // the congestion target carries the shards' overhead (fecOverheadKbps)
	hand := time.Duration(float64(wire) * 8 / pace * float64(time.Second))
	q := queueLoad{queued: int(hand / interval), sending: true, busy: hand, lag: hand, interval: interval,
		deadline: frameDeadline(interval, wire, pace)}
	q.burst = q.deadline
	if got := queueOverflows(q); got != "" {
		t.Fatalf("shards (%d bytes for %d) handed over in %v, deadline %v, %d queued: overflow %q, want none", wire, stream, hand, q.deadline, q.queued, got)
	}
	q.deadline = frameDeadline(interval, stream, pace) // judged by the stream bytes
	q.burst = q.deadline
	if got := queueOverflows(q); got != "deadline" {
		t.Fatalf("judged by its stream bytes (deadline %v): overflow %q, want deadline", q.deadline, got)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
