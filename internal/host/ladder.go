package host

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// The loss-recovery ladder (GUIDE 2.3). Every decision about a frame that is
// late or lost, and about key frames, is made by ladder, from the frame, the
// recovery mode the client was told for the live generation
// (VideoConfig.Recovery) and whether the pipeline forces IDRs in its running
// encoder; never from a vendor. The rungs, cheapest first:
//
//  1. Deadline drop. A frame whose stream is still being written past its
//     deadline (frameDeadline: max(2 frame intervals, 25 ms)) while a newer
//     frame is ready is cancelled (CancelWrite) and becomes a loss, where that
//     loss costs no key frame and no damaged picture: under reference recovery
//     (rung 2) of the live generation. Key frames and recovery frames are
//     never cancelled (another one would have to take their place); under
//     "skip" and "keyframe" a late frame goes on (the loss would cost a
//     smeared picture or a key frame, a late frame only time). Once its write
//     returned (the transport took the frame) and its stream is closed, a
//     frame is QUIC's to deliver: lost packets are retransmitted, not
//     cancelled (docs/VENDOR_NOTES.md 2.3, deviation 7).
//  2. Recover without a key frame (recovery "ltr" / "invalidate": the native
//     helper's AMF long-term references or NVENC reference invalidation, GUIDE
//     3.5): the encoder codes its next frame from frames the client holds.
//     The frames from the loss up to that recovery frame are useless to the
//     client (it discards them) and are not sent (lossWait); they are
//     reported in runs (discardRun).
//  3. Intra refresh, as a safety net only. Recovery "skip" (FFmpeg's NVENC
//     H.264 / HEVC, where rung 2 does not exist and rung 4 is an encoder
//     restart): the client decodes on and the refresh heals the picture,
//     bounded in time (watchHeal). On the helper intra refresh runs wherever
//     it does not conflict (no LTR slots, no SVC: NVENC, AMF H.264 without
//     LTR), under rungs 2 and 4, and the client is not told to rely on it.
//  4. Key frame: on a decoder error or any other key-frame request of the
//     client, at session start (every generation begins with one), for a loss
//     rung 2 cannot answer (recovery "keyframe", the generation's key frame
//     lost, a Recover that failed), for a frame-queue overflow without rung 2
//     (with the bitrate cut), and for a "skip" loss intra refresh did not heal
//     in time: an IDR in the running encoder where the pipeline forces one
//     (the helper: never a restart), else a new encoder generation (FFmpeg's
//     command line: its restart).
//
// lossWait, frameDeadline and the session's methods around them
// (Session.loss, Session.checkOut, frameSender) carry the decisions out.

// lossEvent is what the ladder is asked about.
type lossEvent int

const (
	// lossOutgoing: a frame on its way out: frameSender is about to open its
	// stream (age 0), or its stream has been written for age. Send on,
	// cancel it (rung 1) or discard it (rung 2's wait).
	lossOutgoing lossEvent = iota
	// lossConfirmed: generation gen's frames from seq on will never reach
	// the client: the session dropped them (rung 1, a failed stream, the
	// test hook), the pipeline did (the helper's ring was full), or the
	// client saw a gap that outlasted its wait ({"t":"lost"}).
	lossConfirmed
	// lossOverflow: the frame queue overflowed and its frames from seq on
	// (of gen) were dropped; the bitrate is cut at the same time.
	lossOverflow
	// lossKeyRequest: the client needs a key frame (a decoder error, a loss
	// under "keyframe", no recovery frame came, its watchdog), or another
	// path of the session does (an urgent change).
	lossKeyRequest
	// lossUnhealed: a loss under "skip" that intra refresh did not heal
	// within media.MaxHeal (healDue).
	lossUnhealed
)

// ladderIn is one question to the ladder.
type ladderIn struct {
	event lossEvent
	gen   uint8
	seq   uint32

	// lossOutgoing: the frame (key frame; recovery frame and its refFloor),
	// how long its stream has been written (0: not opened yet), its deadline,
	// and whether a newer frame is ready (queued or taken after it).
	key, recovery bool
	refFloor      uint32
	age, deadline time.Duration
	newer         bool

	// The stream: the live generation (the newest the client was sent a
	// config of), the recovery mode its client was told ("" while nothing
	// streams), the loss the client waits on, and whether the pipeline
	// forces IDRs in its running encoder (PipelineCaps.ForceIDR).
	live     uint8
	mode     string
	wait     lossWait
	forceIDR bool

	// recoverFailed: the pipeline refused the Recover of an earlier answer
	// (rung 2 unavailable after all).
	recoverFailed bool
}

// ladderAction is what the session does.
type ladderAction int

const (
	actNone     ladderAction = iota // nothing: send on, keep waiting, or a generation the client has left
	actCancel                       // rung 1: cancel the frame's stream; the frame is lost (lossConfirmed follows)
	actDiscard                      // rung 2 or 4: the client discards the frame anyway: do not send it (or stop its stream)
	actRecover                      // rung 2: Pipeline.Recover; if it fails, ask again with recoverFailed
	actHeal                         // rung 3: the client skips the frame, intra refresh heals it (watchHeal bounds it)
	actKeyframe                     // rung 4: a key frame now (an IDR in the encoder, or an urgent restart)
	actRefresh                      // rung 4 for an unhealed loss: an IDR, or an overlapped restart (the picture stays meanwhile)
)

var actionNames = [...]string{"none", "cancel", "discard", "recover", "heal", "keyframe", "refresh"}

func (a ladderAction) String() string { return actionNames[a] }

// ladderStep is the ladder's answer.
type ladderStep struct {
	rung int // 1-4; 0: no rung (nothing to do)
	act  ladderAction
	// restart: the key frame (actKeyframe, actRefresh) needs a new encoder
	// generation, the pipeline cannot force an IDR (FFmpeg).
	restart bool
	// endsWait (lossOutgoing): the frame ends the client's wait (it is the
	// recovery frame or key frame the wait was for): send it, and the frames
	// after it.
	endsWait bool
	why      string
}

// ladder decides what to do about a late or lost frame, or a key-frame need
// (see the rungs above).
func ladder(in ladderIn) ladderStep {
	ref := proto.RefRecovery(in.mode)
	key := func(why string) ladderStep {
		return ladderStep{rung: 4, act: actKeyframe, restart: !in.forceIDR, why: why}
	}
	switch in.event {
	case lossOutgoing:
		if w := in.wait; w.waiting(in.gen, in.seq) {
			if w.endedBy(in.key, in.recovery, in.refFloor) {
				return ladderStep{rung: w.rung, endsWait: true, why: "ends the wait for the answer to a loss"}
			}
			return ladderStep{rung: w.rung, act: actDiscard, why: "the client waits for the answer to a loss before it"}
		}
		switch {
		case !in.newer || in.age < in.deadline:
			return ladderStep{why: "in time, or nothing newer to send"}
		case in.key:
			return ladderStep{why: "key frame: never cancelled"}
		case in.recovery:
			return ladderStep{why: "recovery frame: never cancelled"}
		case in.gen != in.live:
			return ladderStep{why: "late, of a generation the client has left (it discards its frames anyway)"}
		case !ref:
			return ladderStep{why: "late, but its loss would cost a key frame or a damaged picture"}
		}
		return ladderStep{rung: 1, act: actCancel, why: "past its deadline"}
	case lossConfirmed, lossOverflow:
		switch {
		case in.mode == "" || in.gen != in.live:
			return ladderStep{why: "a generation the client has left"}
		case ref && in.seq > 0 && !in.recoverFailed:
			return ladderStep{rung: 2, act: actRecover, why: "the encoder recovers from frames the client holds"}
		case ref && in.seq == 0:
			return key("the generation's key frame: nothing to recover from")
		case ref:
			return key("the encoder cannot recover it")
		case in.event == lossOverflow:
			return key("frame queue overflow") // with the bitrate cut
		case in.mode == proto.RecoverySkip:
			return ladderStep{rung: 3, act: actHeal, why: "intra refresh heals it"}
		}
		return key("the encoder recovers only with a key frame")
	case lossKeyRequest:
		return key("key frame requested")
	case lossUnhealed:
		st := key("intra refresh did not heal the loss in time")
		st.act = actRefresh
		return st
	}
	return ladderStep{}
}

// lossWait is the loss the client waits on: it discards generation gen's
// frames from seq from on until the answer arrives (rung 2, and rung 4 under
// reference recovery: a recovery frame with refFloor < from, or a key frame;
// rung 4 under "keyframe": nothing of the generation, wholeGen, the client
// asks for a new one). end is the seq of that answer once frameSender has
// taken it (ended): the frames from end on are decodable again.
type lossWait struct {
	active   bool
	gen      uint8
	from     uint32
	wholeGen bool
	rung     int
	ended    bool
	end      uint32
}

// waiting reports whether frame seq of generation gen falls into the wait:
// at or after the loss, before its answer.
func (w lossWait) waiting(gen uint8, seq uint32) bool {
	return w.active && gen == w.gen && seq >= w.from && (!w.ended || seq < w.end)
}

// endedBy reports whether a frame of the wait's generation answers it (the
// client's rule, protocol.js endsRecovery: a key frame, or a recovery frame
// that references nothing from the lost frame on).
func (w lossWait) endedBy(key, recovery bool, refFloor uint32) bool {
	return !w.wholeGen && (key || recovery && refFloor < w.from)
}

// minFrameDeadline is GUIDE 2.3's floor of a frame stream's deadline.
const minFrameDeadline = 25 * time.Millisecond

// frameDeadline is how long a frame of size bytes may take on its stream
// before rung 1 cancels it for a newer frame: max(2 frame intervals, 25 ms)
// (GUIDE 2.3), and for a frame larger than the pacer sends in a frame
// interval (pacingBps: the video's pacing rate, 0 unknown) its own sending
// time plus one interval: a large frame (a scene change) is slow on any path,
// not late.
func frameDeadline(interval time.Duration, bytes int, pacingBps float64) time.Duration {
	if interval <= 0 {
		interval = time.Second / 60
	}
	d := max(2*interval, minFrameDeadline)
	if pacingBps > 0 {
		send := time.Duration(float64(bytes) * 8 / pacingBps * float64(time.Second))
		d = max(d, send+interval)
	}
	return d
}

// sendState is frameSender's bookkeeping the ladder works on: the frame
// streams being written (rung 1 may cancel them), the frames taken from the
// queue recently (a loss reported late may have been answered already) and
// the loss the client waits on.
type sendState struct {
	mu     sync.Mutex
	taken  uint64      // frames taken from the queue so far
	out    []*outFrame // streams being written, oldest first
	recent [recentFrames]takenFrame
	wait   lossWait
}

// recentFrames is how many taken frames sendState keeps (by taken count):
// over 4 s at 60 fps, 1 s at 240 fps; losses are known within a round trip
// and the client's late-frame wait.
const recentFrames = 256

// takenFrame is what the wait needs of a frame taken from the queue.
type takenFrame struct {
	gen           uint8
	seq           uint32
	key, recovery bool
	refFloor      uint32
}

// outFrame is a frame whose stream frameSender opened and is writing (or,
// test hook, will write late).
type outFrame struct {
	f        *media.Frame
	st       transport.SendStream
	n        uint64 // its number among the frames taken: a frame taken later is newer
	opened   time.Time
	deadline time.Duration // from opened (frameDeadline)
	timer    *time.Timer   // runs checkOut at the deadline; nil: the ladder never cancels it for lateness
	state    atomic.Int32  // outWriting, then outDone or outCancelled
}

const (
	outWriting int32 = iota
	outDone
	outCancelled // cancelled by the ladder, or its stream failed
)

// cancelledFrame is a frame stream the ladder cancelled (due).
type cancelledFrame struct {
	of   *outFrame
	step ladderStep
	age  time.Duration
}

// take records the frame frameSender takes from the queue, and returns its
// number and what the ladder says about it before its stream opens: send it,
// or actDiscard (the client waits for the answer to a loss before it). The
// frame that answers the loss ends the wait; so does a newer generation.
func (s *sendState) take(f *media.Frame) (uint64, ladderStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken++
	s.recent[s.taken%recentFrames] = takenFrame{gen: f.Gen, seq: f.Seq, key: f.Key, recovery: f.Recovery, refFloor: f.RefFloor}
	if s.wait.active && int8(f.Gen-s.wait.gen) > 0 {
		s.wait = lossWait{} // the client has left the generation it waited in
	}
	st := ladder(ladderIn{event: lossOutgoing, gen: f.Gen, seq: f.Seq, key: f.Key, recovery: f.Recovery, refFloor: f.RefFloor,
		wait: s.wait})
	if st.endsWait {
		s.wait.ended, s.wait.end = true, f.Seq
	}
	return s.taken, st
}

// outgoing asks the ladder about a frame being sent (in: lossOutgoing, the
// frame, its age and deadline, whether a newer one is ready) with the
// client's current wait: for frames that go as shards (fec.go), which have
// no stream for due to find.
func (s *sendState) outgoing(in ladderIn) ladderStep {
	s.mu.Lock()
	in.wait = s.wait
	s.mu.Unlock()
	return ladder(in)
}

// register adds a frame stream being written.
func (s *sendState) register(of *outFrame) {
	s.mu.Lock()
	s.out = append(s.out, of)
	s.mu.Unlock()
}

// finish ends a frame stream's write as done or failed (outCancelled); false
// if the ladder cancelled it first.
func (s *sendState) finish(of *outFrame, state int32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !of.state.CompareAndSwap(outWriting, state) {
		return false
	}
	s.remove(of)
	return true
}

// remove takes a frame stream out of the list and stops its timer. Called
// with s.mu held.
func (s *sendState) remove(of *outFrame) {
	if of.timer != nil {
		of.timer.Stop()
	}
	for i, o := range s.out {
		if o == of {
			n := copy(s.out[i:], s.out[i+1:])
			s.out[i+n] = nil
			s.out = s.out[:i+n]
			return
		}
	}
}

// due asks the ladder (in: the live stream) about every frame stream being
// written and takes those it cancels (rung 1, or the wait: actDiscard) out
// of the list; the caller resets their streams. queued: frames wait in the
// queue (a newer frame is ready).
func (s *sendState) due(in ladderIn, queued bool, now time.Time) []cancelledFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cancelledFrame
	for i := 0; i < len(s.out); {
		of := s.out[i]
		f := of.f
		age := now.Sub(of.opened)
		in.event, in.gen, in.seq, in.key, in.recovery, in.refFloor = lossOutgoing, f.Gen, f.Seq, f.Key, f.Recovery, f.RefFloor
		in.age, in.deadline, in.newer, in.wait = age, of.deadline, queued || s.taken > of.n, s.wait
		st := ladder(in)
		if (st.act == actCancel || st.act == actDiscard) && of.state.CompareAndSwap(outWriting, outCancelled) {
			s.remove(of)
			out = append(out, cancelledFrame{of: of, step: st, age: age})
			continue
		}
		i++
	}
	return out
}

// setWait notes that the client discards generation gen's frames from seq
// from on until the answer to their loss (rung 2; rung 4 with wholeGen: none
// in the generation). A wait in the same generation that is not over yet, or
// whose answer is at or after the loss, is widened and looks for its answer
// again: the client keeps the oldest loss it waits on, and a lost answer (a
// recovery frame or key frame) answers nothing. The frames taken already
// after the lost one may hold the answer (a loss the client reported a round
// trip late): then the wait ended at it; where the kept frames do not reach
// back that far nothing is known, and nothing is discarded.
func (s *sendState) setWait(gen uint8, from uint32, rung int, wholeGen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taken > 0 && int8(s.recent[s.taken%recentFrames].gen-gen) > 0 {
		return // frames of a newer generation went out: the client is leaving this one
	}
	w := lossWait{active: true, gen: gen, from: from, rung: rung, wholeGen: wholeGen}
	if o := s.wait; o.joins(gen, from) {
		w.from, w.rung, w.wholeGen = min(o.from, from), max(o.rung, rung), o.wholeGen || wholeGen
	}
	lo := uint64(1)
	if s.taken > recentFrames {
		lo = s.taken - recentFrames + 1
	}
	complete := lo == 1 // nothing forgotten
	for n := lo; n <= s.taken; n++ {
		t := s.recent[n%recentFrames]
		if t.gen != w.gen || t.seq < w.from {
			complete = true // the kept frames reach back before the loss (or the generation's start)
			continue
		}
		if !w.ended && t.seq > from && w.endedBy(t.key, t.recovery, t.refFloor) {
			w.ended, w.end = true, t.seq
		}
	}
	if !complete {
		w.ended, w.end = true, w.from
	}
	s.wait = w
}

// joins reports whether a loss of generation gen's frames from seq from on
// is part of the wait w (setWait widens w): w is not answered yet, the loss
// is before its answer (which answers it too unless it is older than the
// wait), or the answer itself was lost.
func (w lossWait) joins(gen uint8, from uint32) bool {
	return w.active && w.gen == gen && (!w.ended || from <= w.end)
}

// recoverFrom returns the frame the encoder must recover from for a loss of
// generation gen's frames from seq from on (Session.loss, rung 2): the
// oldest loss of the wait it joins (joins), else from. A lost answer reopens
// the wait from its first loss, here and at the client, and only a recovery
// frame with refFloor below that ends it: one made for the lost answer alone
// (refFloor from-1) would be discarded like every frame after it, until the
// client gives up and asks for a key frame.
func (s *sendState) recoverFrom(gen uint8, from uint32) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.wait; o.joins(gen, from) {
		return min(o.from, from)
	}
	return from
}

// discardReportAfter bounds how long a run of discarded frames goes
// unreported (discardRun): a "keyframe" wait lasts until the next
// generation, up to the client's 1 s watchdog.
const discardReportAfter = 250 * time.Millisecond

// discardRun is the run of consecutive frames of a generation the session
// discarded (Session.discard) and has not reported yet: the client hears of
// a run in one {"t":"dropped"} (and host.log in one line) when the run
// breaks, when a frame is sent again (the wait ended) or discardReportAfter
// after the run began. The client needs no report to end its wait (the
// answer does that); it counts them.
type discardRun struct {
	mu    sync.Mutex
	id    uint64 // runs begun so far; the current one's
	gen   uint8
	from  uint32
	count int
	why   string
}

// add appends frame seq of generation gen, discarded for why. A frame that
// does not continue the run ends it: done holds the run's frames to report
// (nil: none). begun is the id of the run the frame began (0: it continued
// one); report it with take after discardReportAfter.
func (r *discardRun) add(gen uint8, seq uint32, why string) (done []*media.Frame, doneWhy string, begun uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count > 0 && (gen != r.gen || seq != r.from+uint32(r.count) || why != r.why) {
		done, doneWhy = r.frames(), r.why
		r.count = 0
	}
	if r.count == 0 {
		r.id++
		r.gen, r.from, r.why, begun = gen, seq, why, r.id
	}
	r.count++
	return done, doneWhy, begun
}

// take ends run id (0: whichever is open) and returns its frames to report
// (nil: none, or that run was reported already).
func (r *discardRun) take(id uint64) ([]*media.Frame, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 || id != 0 && id != r.id {
		return nil, ""
	}
	fs := r.frames()
	r.count = 0
	return fs, r.why
}

// frames returns the run as frames for reportDropped. Called with r.mu held.
func (r *discardRun) frames() []*media.Frame {
	fs := make([]*media.Frame, r.count)
	for i := range fs {
		fs[i] = &media.Frame{Gen: r.gen, Seq: r.from + uint32(i)}
	}
	return fs
}
