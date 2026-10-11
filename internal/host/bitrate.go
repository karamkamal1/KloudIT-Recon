package host

import (
	"math"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// Video bitrate control (GUIDE 2.2): a delay-based rate controller in the
// style of GCC and SCReAM v2, replacing the interim of step 1.5. It owns the
// session's video bitrate target and decides from the client's receive
// reports (proto.RateReport every 25 ms; older clients: their per-frame 0x40
// acks) and the media congestion controller's packet counters:
//
//   - Queueing delay: the one-way delay of the frames a report covers (p50),
//     less what the host's own pacer explains (sendTrack: a key frame paced
//     out at 1.2 x the target delays itself and the frames behind it; that of
//     the p50's own frame where the report names it), on the scale of the
//     client's first clock sync (fromReport). The
//     target is the minimum of the last owdWindow plus queueMargin, wider on a
//     path whose own jitter is wider (jitterGain x the mean change between
//     reports, at most queueMarginMax): Wi-Fi's bursts must not read as a
//     queue. Frames that stop arriving altogether (a capacity drop: nothing
//     completes while the frames wait for retransmissions) show in the age of
//     the oldest frame the client lacks; far over the target (pendingStrong)
//     that decides on the second report in a row (one report could follow
//     a stall of the client itself: what it lacks may wait in its own
//     socket; a report more than pendingStallGap after the previous one is
//     not judged by it at all). The first owdWarmup of samples decides
//     nothing. A delay that stays where a delay decrease found it, on a path
//     that held the sender back neither before nor since, is a step of the
//     path's delay, not a queue: the base restarts there (levelShift).
//   - Decrease to rateDecreaseFactor (x0.85) when the delay stays over the
//     target for overReports consecutive reports and is not falling (a queue
//     that drains needs no second decrease: the gradient), or more than
//     lossThreshold of the packets were lost in the last lossWindow. The
//     factor applies to the rate the path delivered, where that is lower
//     than the target (carried): on the direct path what the connection's
//     acknowledgements carried in the last ackWindow where the path held the
//     sender back (else nothing: the sender's own rate is no measure of the
//     path's), else the client's receive rate (the lower of decreaseWindow
//     and rateWindow), both divided by the fill (what the session sent
//     against the target, by time); after a capacity drop that is the new
//     capacity.
//     A delay decrease also starts from the capacity the queue's growth
//     implies (queueCapacity), which sees a capacity drop before the
//     delivered rates do. The next decrease waits until this one is in the
//     encoder (live) and the policy's hold more (a restart or a flush also
//     sends a key frame first).
//   - Increase continuously, +5 %/s (incSlow) near the last known-good rate
//     (what the path delivered at the last decrease), up to +25 %/s (incFast)
//     far below it, and accelerating by incAccel per second once above it
//     (the path's capacity grew); never above the user's setting (ceiling), the
//     decoder's cap, or recvHeadroom (1.2) x the delivered rate (the receive
//     rate divided by the encoder's fill: encoders do not hit their target).
//   - Emergencies as in 1.5: a host frame-queue overflow (queueOverflows:
//     only where the network does not keep up, or a backlog that does not
//     drain) or a client whose decoder fell behind cuts at once
//     (rateEmergencyFactor) with an urgent restart, but not within
//     rateEmergencyGap of any other decrease; an overflow behind a key frame
//     past its deadline (signalKeyOverflow) cuts even then, to what lets the
//     next key frame fit. No increase within rateEmergencyGap of any
//     overflow, cut or not. The decoder's cap lifts after rateDecoderHold,
//     twice as long with each further flush.
//   - At the floor, the frame rate goes down a rung (120 -> 90 -> 60) with
//     each decrease before anything else, and back up once the bitrate is
//     well above the floor (1.5 x, or at the limit where that is lower), a
//     rung per fpsHold. An encoder that changes its frame rate in place
//     without a key frame (the native helper's liveFps "seamless":
//     PipelineCaps.LiveFPS) steps through encoder.FPSSteps instead (LowerFPS
//     / RaiseFPS: 120 -> 100 -> 90 -> 75 -> 60, down to host config
//     "fpsFloor", 60 by default as GUIDE 2.2's rungs), each step down and up
//     fpsHoldLive (2 s) apart (GUIDE 9 "FPS before resolution"): at the
//     floor the bitrate stays where it is, so a step does not relieve the
//     path, and a step with every decrease (one per policy hold, 150 ms)
//     would take 120 fps to the floor within a second.
//   - Thinning (Phase 5 temporal SVC, thin.go): the session leaves out
//     discardable frames under congestion at once, a short spike's answer;
//     while it does for the path's pressure nothing increases (thinQuiet),
//     and thinning that goes on for thinSustain decreases like the delay
//     ("thinning"): a lasting shortage is the bitrate's to answer, not the
//     frame rate's. Thinning for the host's own queue alone changes nothing.
//
// The continuous target reaches the encoder at most as often as its pipeline
// can take changes (ratePolicy): a qualified seamless encoder every 250 ms, a
// flushing one (a key frame per change) or an FFmpeg restart (a new encoder
// generation) less often, and those slower ones in small steps near the last
// known-good rate; an FFmpeg cut to cutUrgent or less is an urgent restart
// (the old generation stops at once). The media congestion controller then
// paces at 1.2 x the encoder's bitrate (setCongestionTarget).
const (
	rateFloorKbps       = 2000 // decreases stop here (at the ceiling if that is lower)
	rateDecreaseFactor  = 0.85 // a congestion decrease
	rateEmergencyFactor = 0.75 // a frame-queue overflow or a decoder flush
	// rateDecoderPct: after a client flushed its decoder (signalDecoder) at
	// bitrate B, increases stop at this share of B for rateDecoderHold (or
	// until the settings change): a decoder that fell behind once (the
	// renderer bake-off at the start, a tab switch, a burst of key frames)
	// may keep up at the setting, and a cap for good would leave the stream
	// backed off. One that cannot flushes again and caps again, each time
	// for twice as long (up to 8 x rateDecoderHold): a flush, a frozen
	// picture and a cut every half minute would be worse than the cap.
	rateDecoderPct  = 85
	rateDecoderHold = 30 * time.Second
	// rateEmergencyGap is the minimum time between two emergency cuts.
	rateEmergencyGap = 2 * time.Second
	// rateTick is how often the session advances the controller (increases,
	// changes the encoder could not take yet).
	rateTick = 100 * time.Millisecond

	owdWindow      = 2 * time.Second       // the base delay is the minimum over this window
	queueMargin    = 8 * time.Millisecond  // target queueing delay over the base on a steady path
	queueMarginMax = 50 * time.Millisecond // the jitter-widened target's cap
	jitterGain     = 4                     // margin = max(queueMargin, jitterGain x jitter)
	overReports    = 3                     // consecutive reports over the target that decrease
	// pendingStrong: frames that have not arrived this long over the base
	// decrease at once (also at least 4 x the margin, and 2.5 x the raw
	// base: more than a frame that waits for one retransmission, about 1.125
	// round trips, is late).
	pendingStrong = 40 * time.Millisecond
	// queueGrowthMin, queueGrowthOver: a decrease on the delay also starts
	// from the capacity the queue's growth implies (queueCapacity) when the
	// queue grew by this much (seconds per second, over the last
	// overReports+1 reports spanning at least 2 x rateReportMin) and stands
	// this far over the base.
	queueGrowthMin  = 0.25
	queueGrowthMax  = 1.0
	queueGrowthOver = 20 * time.Millisecond
	rateReportMin   = 20 * time.Millisecond
	// pendingStallGap: a report that comes this long after the previous one
	// (the client's clock) followed a stall of the client: its pending frame
	// says nothing about the path.
	pendingStallGap = 100 * time.Millisecond

	lossWindow    = time.Second
	lossThreshold = 0.02
	// fecLossThreshold replaces lossThreshold while video goes as datagram
	// shards with parity (fec.go): random loss the parity rebuilds is not
	// congestion, and 2 % would cut the bitrate to the floor on a path that
	// loses 3 % (GCC's upper loss threshold; the delay still decreases).
	fecLossThreshold = 0.10
	lossMinPackets   = 100 // fewer packets in the window: no loss decision
	// lossSettle: losses count from this long after a decrease is in the
	// encoder: those detected before were of packets sent at the old rate.
	lossSettle = 300 * time.Millisecond

	rateWindow     = time.Second            // receive and encoder output rates (the increase cap)
	decreaseWindow = 250 * time.Millisecond // ... for a decrease: what the path carries now
	ackWindow      = 100 * time.Millisecond // acknowledged bytes: the connection's delivery rate now
	// ackLimited, ackShort: the acknowledged rate is the path's only when
	// the congestion window held the sender back for at least ackLimited of
	// ackWindow, packets were lost in it, or the path acknowledged less than
	// ackShort of the video the session sent over decreaseWindow
	// (ackDelivered).
	ackLimited = 0.25
	ackShort   = 0.9
	// decreaseMinShare: a decrease starts from at least this share of the
	// target, whatever the delivered rate or the queue's growth say.
	decreaseMinShare = 0.5
	recvHeadroom     = 1.2 // increases stop at this multiple of the delivered rate
	// drainingBy: the delay is falling (no decrease) when it is this much
	// below the delay overReports reports earlier.
	drainingBy = 2 * time.Millisecond

	incSlow   = 0.05 // per second, near the last known-good rate
	incFast   = 0.25 // per second, far below it, or long above it
	incAccel  = 0.05 // per second per second above it
	nearBelow = 0.15 // from this share below the last known-good rate...
	nearAbove = 0.05 // ... to this share above it, slow policies step by...
	nearStep  = 0.05 // ... at most this much
	fastBelow = 0.10 // incSlow down to this share below the last known-good rate...
	fastSpan  = 0.30 // ... incFast from fastBelow + fastSpan below it

	// owdWarmup: the delay decides nothing before its base window has this
	// much behind it (the first frames of a session, its key frame and the
	// client's start-up, would make any later delay look low).
	owdWarmup = time.Second
	// liveTimeout: a decrease the encoder never confirms (live) counts as in
	// effect after this.
	liveTimeout = 3 * time.Second
	// feedbackFresh: increases need a report that covered new frames this
	// recently (a still desktop sends nothing: nothing to judge).
	feedbackFresh = 500 * time.Millisecond
	// ackTimeout: a frame sent this long ago that no feedback covered, from a
	// client that sends feedback, means the path to the client stalls (on the
	// splice relay paths the gateway buffers what its client leg cannot carry,
	// so the host's frame queue does not overflow).
	ackTimeout = time.Second
	// noFeedbackQuiet: a client that never sends feedback (no reports, no
	// acks) gets increases (incSlow) only this long after the last decrease.
	noFeedbackQuiet = 10 * time.Second
	// decodeQueueMin: a backlog in the client's decoder above max(this,
	// fps/10) frames holds increases: the client flushes its decoder at that
	// backlog (stream-worker.js checkDecoderBacklog); software decoders keep a
	// few frames in flight normally (frame threading).
	decodeQueueMin = 4
	// fpsHold is the minimum time from a frame-rate change or a decrease to
	// a frame-rate increase (the rungs go down with each decrease at the
	// floor); fpsHoldLive the same for an encoder that changes its frame
	// rate in place (no key frame, no restart: finer and faster steps), and
	// also between its steps down.
	fpsHold     = 5 * time.Second
	fpsHoldLive = 2 * time.Second
	// fpsFloorLive is the lowest frame rate the fine steps go to by default:
	// GUIDE 2.2's 60 (host config "fpsFloor" overrides it).
	fpsFloorLive = 60

	// Thinning (thin.go). thinQuiet: no increase this long after a frame
	// was thinned (while an episode lasts: frames thinned at most
	// thinEpisodeGap apart; not longer, or the frequent short episodes of a
	// loaded client would hold the bitrate down). thinSustain: thinning
	// that lasts this long decreases the bitrate.
	thinEpisodeGap = 250 * time.Millisecond
	thinQuiet      = thinEpisodeGap
	thinSustain    = time.Second
	// thinOverReports: consecutive reports over the delay target that count
	// as congestion for thinning (one report alone can be a burst).
	thinOverReports = 2
)

// fpsRungs are the frame rates the controller steps down through at the
// bitrate floor (GUIDE 2.2, AMD Streaming SDK QoS), before resolution, where
// every change costs a key frame or a restart; encoders that change their
// frame rate in place use encoder.FPSSteps (applyPolicy.fineFPS).
var fpsRungs = []int{120, 90, 60}

// rateSignal is the kind of a congestion signal from outside the reports.
type rateSignal int

const (
	// signalDelay: an older client's own delay report ({"t":"congestion"}),
	// from clients that do not send rate reports: a decrease like the
	// controller's own.
	signalDelay rateSignal = iota
	// signalOverflow: the host's frame queue overflowed. An emergency: it
	// cuts at most once every rateEmergencyGap.
	signalOverflow
	// signalDecoder: the client flushed its decoder, which fell behind. An
	// emergency like signalOverflow; a cut also caps later increases at
	// rateDecoderPct of the bitrate the decoder fell behind at (for
	// rateDecoderHold), since the delay does not show the client's decode
	// capacity.
	signalDecoder
	// signalKeyOverflow: the frame queue overflowed behind a key frame that
	// is past its deadline: the path did not carry the key frame at the
	// pacing rate, and the key frame that answers the loss comes at the
	// bitrate the encoder has then (at a low bitrate many frame budgets): at
	// an unchanged bitrate the next overflow and the next key frame, a
	// storm. It cuts like signalOverflow, to at most what the path's
	// acknowledgements carried over PacingGain (the key frame then goes out
	// at the rate the path carries), and also within rateEmergencyGap of
	// another decrease (the session sends it once per key frame).
	signalKeyOverflow
)

// applyPolicy is how often the encoder of a pipeline takes bitrate changes.
type applyPolicy struct {
	name    string
	decGap  time.Duration // minimum time since the last change before a decrease
	incGap  time.Duration // ... before an increase or frame-rate change up
	minStep float64       // smaller relative increases wait (except to the limit)
	// hold: after a decrease is in the encoder, this long before the delay
	// may decrease again (the new rate must show in the queue's trend; a
	// restart or flush also sends a key frame first) or increase.
	hold time.Duration
	// cutUrgent: a delay or loss decrease to this share of the encoder's
	// bitrate or less is urgent (0: never): an FFmpeg generation streams on
	// at the old rate until the overlapped restart takes over, which after a
	// capacity drop (the path carries far less than the encoder makes)
	// overflows the host's frame queue; an urgent restart stops it at once,
	// and the decrease does not wait for decGap.
	cutUrgent float64
	// fineFPS: the encoder changes its frame rate in place without a key
	// frame (PipelineCaps.LiveFPS): the frame-rate ladder at the floor
	// steps through encoder.FPSSteps, fpsHoldLive apart.
	fineFPS bool
	// recvCarried: where the acknowledgements say nothing of the path (not
	// path-limited: ackDelivered), a decrease starts from the client's
	// receive rate (carried). A policy whose changes are seconds apart sits
	// at a cut's rate until the next change: starting it from what the path
	// delivered over the last 250 ms and second, not from the target, keeps
	// the cycle in a capacity dip (a cut, a few steps up, a cut) under the
	// capacity (docs/VENDOR_NOTES.md, "First hardware run: rate control on
	// the UDP relay", review).
	recvCarried bool
}

// ratePolicy picks the apply policy for the pipeline that streams: a live
// change qualified seamless (recon-host qualify, GUIDE 3.6: no key frame)
// every 250 ms; one only assumed seamless (the helper's caps defaults) every
// second; a flushing encoder (a key frame per change: GUIDE 3.6 "change less
// often") and a restart (an FFmpeg generation: a new process, a key frame and
// an overlapped switch; or a helper that cannot change live) far less often.
func ratePolicy(c media.PipelineCaps) applyPolicy {
	fine := c.LiveFPS && c.LiveBitrate && !c.LiveBitrateFlush
	switch {
	case c.LiveBitrate && !c.LiveBitrateFlush && c.LiveBitrateMeasured:
		return applyPolicy{"seamless", 0, 250 * time.Millisecond, 0.02, 150 * time.Millisecond, 0, fine, false}
	case c.LiveBitrate && !c.LiveBitrateFlush:
		return applyPolicy{"seamless (assumed)", 0, time.Second, 0.03, 300 * time.Millisecond, 0, fine, false}
	case c.LiveBitrate:
		return applyPolicy{"flush", 250 * time.Millisecond, 2 * time.Second, 0.05, 500 * time.Millisecond, 0, false, true}
	}
	return applyPolicy{"restart", 500 * time.Millisecond, time.Second, 0.05, time.Second, 0.75, false, false}
}

// feedback is one receive report as the controller reads it.
type feedback struct {
	at time.Time
	// frames received since the previous report (0: nothing new), their
	// bytes and the client time they were received over (0: unknown).
	frames   int
	bytes    int64
	interval time.Duration
	// owdValid: qd, owd and owdMax hold the frames' one-way delays: qd the
	// p50 less the host pacer's share (the queueing signal), owd the raw p50.
	owdValid bool
	qd, owd  time.Duration
	owdMax   time.Duration
	// pendingValid: the oldest frame sent after the newest one the client
	// had received was encoded pending ago (less its pacer share) when the
	// report arrived. While frames stop arriving (a capacity drop: the
	// queue fills, losses wait for retransmission) no delay is reported at
	// all; pending less the base (for the report's way back) bounds it.
	pendingValid bool
	pending      time.Duration
	lost, total  int64 // packets lost and sent (lost + acknowledged) since the previous report
	decodeQ      int   // the client's decoder backlog (frames)
	// acked: bytes the media congestion controller saw acknowledged since
	// the previous report (ackedValid), of which nonVideoKbps worth are
	// audio and overhead; limited: how long its congestion window held the
	// sender back meanwhile (cc.Media.WindowLimited).
	ackedValid   bool
	acked        int64
	nonVideoKbps int
	limited      time.Duration
}

// rateChange is a bitrate (and frame-rate) change for the encoder.
type rateChange struct {
	fromKbps, toKbps int
	fromFPS, toFPS   int
	down             bool   // a decrease (also: the frame rate goes down)
	urgent           bool   // at once: an emergency (the client needs a key frame), or a cut of cutUrgent
	why              string // delay | loss | client | overflow | decoder | recovery
}

// qdSample is a delay sample the over-target test judged: d is the report's
// queueing delay, or the pending frame's bound where that is higher; own is
// the report's own delay (ownOK false: it had none, or the pending frame's
// bound replaced it: a frame that does not arrive ages at 1 s per second
// whatever the capacity).
type qdSample struct {
	at    time.Time
	d     time.Duration
	own   time.Duration
	ownOK bool
}

type timedDelay struct {
	at     time.Time
	d, raw time.Duration // queueing delay (less the pacer's share) and raw one-way delay
}

type timedCount struct {
	at       time.Time
	n, total int64
	span     time.Duration
	lim      time.Duration // acknowledged bytes (n; total: packets lost): of span, the time the congestion window held the sender back
	key      bool          // encoder output: a key frame
}

// rateController decides the session's video bitrate. Methods are safe for
// concurrent use; the zero value is ready (clock time.Now, policy "restart",
// adaptive).
type rateController struct {
	now func() time.Time // nil: time.Now (tests use a fake clock)

	mu        sync.Mutex
	fixed     bool        // the client turned adaptive bitrate off: no delay or loss decisions
	policy    applyPolicy // zero: ratePolicy of FFmpeg (setPolicy)
	ceiling   int         // kbps: the settings' bitrate, never exceeded
	fpsMax    int         // the settings' frame rate
	fpsFloor  int         // host config "fpsFloor": the frame-rate ladder's lowest step (0: default)
	est       float64     // kbps: the continuous target (0 until the first generation)
	fps       int         // the frame rate the ladder allows (<= fpsMax)
	applied   int         // kbps the encoder was told (0 before the first generation)
	appliedFP int         // its frame rate
	lastApply time.Time   // when the encoder was last told a new rate
	liveKbps  int         // kbps of the encoder that streams (live; 0: unknown)
	pending   string      // why of a change decided but not yet applied

	// Delay.
	base     []timedDelay // qd samples of the last owdWindow
	jitter   float64      // seconds: mean |qd change| between reports (RFC 3550 style, 1/16)
	lastQD   time.Duration
	haveQD   bool
	firstQD  time.Time  // the first delay sample (owdWarmup)
	strongN  int        // consecutive reports with a pending frame far over the target
	qdOver   bool       // the last report's delay was over the target
	over     int        // consecutive reports over the target (reset by a decrease)
	overRun  int        // ... not reset by a decrease (thinning: overTarget)
	recentQD []qdSample // the last overReports+1 samples, oldest first
	// shiftAt, shiftQD: the last delay decrease and the queueing delay it
	// was decided on (levelShift); zero: none in the last owdWindow, or the
	// path held the sender back (pathAt) within owdWindow before it.
	shiftAt   time.Time
	shiftQD   time.Duration
	pathAt    time.Time // the last report with packets lost or the congestion window holding the sender back
	recv      []timedCount
	out       []timedCount // encoder output: n bytes, total the bytes its target asked for since the previous frame
	sends     []timedCount // bytes handed to the transport, total as out
	lastOut   time.Time
	lastSend  time.Time
	loss      []timedCount
	acked     []timedCount // acknowledged bytes per report (span: host time since the previous)
	ackDirect bool         // the acknowledgements come from the client (setPath)
	fec       bool         // video goes as datagram shards with parity (setFEC)
	nonVideo  int          // kbps of audio and overhead in the acknowledged bytes
	liveFPS   int          // frame rate of the encoder that streams
	lastAcked time.Time
	lossFrom  time.Time // losses before are not counted (lossSettle)
	decodeQ   int

	// Decisions.
	lastDecrease  time.Time     // last decrease (any): emergencies are rateEmergencyGap apart from it
	waitLive      bool          // a decrease is decided; the encoder has not confirmed it
	waitKbps      int           // ... at this rate
	decidedAt     time.Time     // when
	holdUntil     time.Time     // no delay or loss decrease and no increase before
	lastGood      float64       // kbps the path delivered at the last decrease (0: none yet)
	passedGood    time.Time     // when est went above lastGood (zero: below it)
	decoderCap    int           // kbps: increases stop here after a decoder flush; 0: none
	decoderCapAt  time.Time     // the flush that set it (rateDecoderHold)
	decoderHold   time.Duration // how long it holds: rateDecoderHold, doubling with each flush
	overflowAt    time.Time     // the last frame-queue overflow, cut or not: no increase for rateEmergencyGap
	lastFPSChange time.Time
	lastTick      time.Time
	// Thinning (thinned): the current run of thinned frames started at
	// thinSince, the newest was thinned at thinLast.
	thinSince, thinLast time.Time

	// Feedback.
	everFeedback bool      // the client has sent feedback
	lastReport   time.Time // last feedback that covered new frames
}

func (r *rateController) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// setPolicy sets the apply policy of the pipeline that streams (ratePolicy).
func (r *rateController) setPolicy(p applyPolicy) {
	r.mu.Lock()
	r.policy = p
	r.mu.Unlock()
}

// setFPSFloor sets the lowest frame rate the ladder at the bitrate floor
// steps down to (host config "fpsFloor"; 0: 60, fpsFloorLive; the rungs
// never go below their 60).
func (r *rateController) setFPSFloor(fps int) {
	r.mu.Lock()
	r.fpsFloor = fps
	r.mu.Unlock()
}

// setAdaptive follows the client's "adaptive bitrate" setting: off, the
// delay and loss decide nothing (emergencies and their recovery still do).
func (r *rateController) setAdaptive(on bool) {
	r.mu.Lock()
	r.fixed = !on
	r.mu.Unlock()
}

func (r *rateController) pol() applyPolicy {
	if r.policy.name == "" {
		return ratePolicy(media.PipelineCaps{})
	}
	return r.policy
}

// floor is the lowest bitrate a decrease goes to. Called with r.mu held.
func (r *rateController) floor() float64 {
	return float64(min(rateFloorKbps, r.ceiling))
}

// limit is the highest bitrate an increase goes to. Called with r.mu held.
func (r *rateController) limit() float64 {
	l := r.ceiling
	if r.decoderCap > 0 {
		l = min(l, r.decoderCap)
	}
	return float64(l)
}

// target returns the bitrate and frame rate of a new encoder generation for
// the settings' ceiling (kbps) and frame rate: the controller's current
// target, never above them. The first generation, and the first after reset,
// runs at the settings. A back-off stays in effect for every restart (key
// frames, encoder failures, resume) until the controller raises it again or
// the settings change.
func (r *rateController) target(ceiling, fps int) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ceiling, r.fpsMax = ceiling, fps
	if r.est <= 0 || r.est > r.limit() {
		r.est = r.limit()
	}
	if r.fps <= 0 || r.fps > fps {
		r.fps = fps
	}
	kbps := int(math.Round(r.est))
	if kbps != r.applied || r.fps != r.appliedFP {
		r.applied, r.appliedFP, r.lastApply = kbps, r.fps, r.clock()
	}
	r.pending = ""
	return r.applied, r.appliedFP
}

// reset drops the back-off, the decoder's cap and the frame-rate ladder: the
// user chose new video settings. The next generation runs at the (new)
// settings.
func (r *rateController) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.est, r.fps, r.decoderCap, r.lastGood, r.decoderHold = 0, 0, 0, 0, 0
	r.waitLive, r.pending = false, ""
	r.holdUntil, r.lastDecrease, r.passedGood, r.shiftAt, r.overflowAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	r.over = 0
}

// hold is called while nothing streams (the session is paused, or no encoder
// generation is live): there is no delay to judge and nothing to increase.
func (r *rateController) hold() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.over, r.overRun, r.qdOver = 0, 0, false
	r.lastTick = now
	if r.holdUntil.Before(now) {
		r.holdUntil = now
	}
}

// live records the bitrate and frame rate the encoder streams at: a new
// generation went live (its VideoConfig) or the live encoder changed its rate
// (VideoEvent.Rate; fps 0: unchanged). A decrease waits for it before the
// next one.
func (r *rateController) live(kbps, fps int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.liveKbps = kbps
	if fps > 0 {
		r.liveFPS = fps
	}
	if r.waitLive && kbps <= r.waitKbps {
		r.settled(now)
	}
}

// settled: a decrease is in the encoder (or taken to be). Called with r.mu
// held.
func (r *rateController) settled(now time.Time) {
	r.waitLive = false
	r.holdUntil = now.Add(r.pol().hold)
	r.lossFrom = now.Add(lossSettle)
	r.loss = r.loss[:0]
}

// output records an encoded frame of n bytes (key: a key frame): the
// source's frame interval (frameInterval) and the encoder's output rate
// (outputKbps).
func (r *rateController) output(n int, key bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.out = trimCounts(append(r.out, timedCount{at: now, n: int64(n), total: r.wanted(&r.lastOut, now), key: key}), now, rateWindow)
}

// outputKbps is the rate (kbit/s) the encoder makes at its live target:
// its output over the last rateWindow, without its key frames, as a share of
// what the target asked for over the same time (by time, as the fill), times
// the live target (0: unknown, fewer than 5 frames). Encoders miss their
// target: one at its QP limit (a large picture at a low bitrate) makes far
// more, and the pacer must keep up with that or the host's frame queue grows
// without end (pacedVideoKbps), from the first frames on. Key frames are left
// out: a key frame is a burst the pacer spreads at its gain, as before.
func (r *rateController) outputKbps() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	var n, want int64
	frames := 0
	for _, c := range r.out {
		if now.Sub(c.at) > rateWindow {
			continue
		}
		frames++
		want += c.total
		if !c.key {
			n += c.n
		}
	}
	if frames < 5 || want <= 0 || r.liveKbps <= 0 {
		return 0
	}
	return float64(n) / float64(want) * float64(r.liveKbps)
}

// sent records a frame of n bytes the session handed to the transport (its
// stream's write returned, or its shards went out): the fill. Frames the
// session dropped (a queue overflow), thinned or discarded never reach the
// path, so they are no part of what the path is measured against.
func (r *rateController) sent(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.sends = trimCounts(append(r.sends, timedCount{at: now, n: int64(n), total: r.wanted(&r.lastSend, now)}), now, rateWindow)
}

// wanted is what the encoder's target asks for (bytes) in the time since
// *last (at most rateWindow; a frame interval for the first), and moves *last
// to now: a source that sends fewer frames than the session's frame rate (a
// still picture, a game menu, idle repeats) is measured by its time, not by
// its frame count. Called with r.mu held.
func (r *rateController) wanted(last *time.Time, now time.Time) int64 {
	if r.liveKbps <= 0 || r.liveFPS <= 0 {
		return 0
	}
	dt := time.Second / time.Duration(r.liveFPS)
	if !last.IsZero() {
		dt = min(max(0, now.Sub(*last)), rateWindow)
	}
	*last = now
	return int64(float64(r.liveKbps) * 1000 / 8 * dt.Seconds())
}

// frameInterval is the mean time between the frames the encoder produced
// over the last rateWindow (0: fewer than two): longer than the session's
// frame interval for a source that sends fewer frames (a still picture, a
// game menu), whose frames are then larger. The encoder's, not the
// sender's: congestion delays the sending, not the source.
func (r *rateController) frameInterval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	n := 0
	var first, last time.Time
	for _, c := range r.out {
		if now.Sub(c.at) <= rateWindow {
			if n == 0 {
				first = c.at
			}
			last = c.at
			n++
		}
	}
	if n < 2 {
		return 0
	}
	return last.Sub(first) / time.Duration(n-1)
}

// setPath tells the controller whether the session's connection ends at the
// client (the direct path, the UDP relay): its acknowledgements are then the
// client's.
func (r *rateController) setPath(direct bool) {
	r.mu.Lock()
	r.ackDirect = direct
	r.mu.Unlock()
}

// setFEC notes whether video goes as datagram shards with parity (fec.go):
// the loss decrease then waits for fecLossThreshold.
func (r *rateController) setFEC(on bool) {
	r.mu.Lock()
	r.fec = on
	r.mu.Unlock()
}

// lossLimit is the packet loss above which the rate decreases. Called with
// r.mu held.
func (r *rateController) lossLimit() float64 {
	if r.fec {
		return fecLossThreshold
	}
	return lossThreshold
}

// report takes a receive report and returns the decrease it calls for.
func (r *rateController) report(fb feedback) (rateChange, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := fb.at
	r.everFeedback = true
	if fb.frames > 0 {
		r.lastReport = now
	}
	if fb.interval > 0 && fb.interval < 5*time.Second {
		r.recv = trimCounts(append(r.recv, timedCount{at: now, n: fb.bytes, span: fb.interval}), now, rateWindow)
	}
	if fb.total > 0 && !now.Before(r.lossFrom) {
		r.loss = trimCounts(append(r.loss, timedCount{at: now, n: fb.lost, total: fb.total}), now, lossWindow)
	}
	if fb.lost > 0 || fb.limited > 0 {
		r.pathAt = now
	}
	if fb.ackedValid {
		if !r.lastAcked.IsZero() && now.After(r.lastAcked) {
			r.acked = trimCounts(append(r.acked, timedCount{at: now, n: fb.acked, total: fb.lost, span: now.Sub(r.lastAcked), lim: fb.limited}), now, decreaseWindow)
		}
		r.lastAcked, r.nonVideo = now, fb.nonVideoKbps
	}
	r.decodeQ = fb.decodeQ
	r.delay(now, fb)
	if r.fixed || r.applied <= 0 || r.waitLive || now.Before(r.holdUntil) {
		return rateChange{}, false
	}
	switch {
	case r.over >= overReports && !r.draining() && !r.levelShift(now):
		return r.decrease(now, "delay")
	case r.lossFraction() > r.lossLimit():
		return r.decrease(now, "loss")
	case r.thinSustained(now):
		return r.decrease(now, "thinning")
	}
	return rateChange{}, false
}

// overTarget reports whether the path is congested now by the delay: the
// queueing delay (or the bound of a frame that does not arrive) was over the
// target in the last thinOverReports reports, or a frame far over it does not
// arrive. Thinning (thin.go) acts on it at once; the bitrate waits for
// overReports in a row (and a decrease does not end it: the queue is still
// there).
func (r *rateController) overTarget() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.overRun >= thinOverReports || r.strongN > 0
}

// delayOver reports whether the last report found the path's delay over
// the target (or a frame far over it that does not arrive): the frame
// queue's congestion test (Session.queueFull), where one report is enough
// (queueOverflows also needs a backlog of frameQueueFrames).
func (r *rateController) delayOver() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.qdOver || r.strongN > 0
}

// thinned records that the session left out a frame under congestion
// (temporal SVC thinning): no increase for thinQuiet, and thinning that goes
// on for thinSustain decreases the bitrate (report). path: the pressure was
// the path's (the delay, a frame stream past its deadline); thinning for the
// host's own frame queue alone (a key frame or a burst paced out at the
// pacing rate) is no congestion: it holds and decreases nothing.
func (r *rateController) thinned(path bool) {
	if !path {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	if r.thinLast.IsZero() || now.Sub(r.thinLast) > thinEpisodeGap {
		r.thinSince = now
	}
	r.thinLast = now
}

// thinSustained reports whether frames have been thinned for thinSustain
// without a pause of thinEpisodeGap, up to now. Called with r.mu held.
func (r *rateController) thinSustained(now time.Time) bool {
	return !r.thinLast.IsZero() && now.Sub(r.thinLast) <= thinEpisodeGap && now.Sub(r.thinSince) >= thinSustain
}

// delay takes a report's queueing delay: the base window and the jitter
// from the frames' delay, the over-target test from it or from the pending
// frame's bound, whichever is higher. Called with r.mu held.
func (r *rateController) delay(now time.Time, fb feedback) {
	i := 0
	for i < len(r.base) && now.Sub(r.base[i].at) > owdWindow {
		i++
	}
	r.base = r.base[i:]
	if fb.owdValid {
		r.base = append(r.base, timedDelay{now, fb.qd, fb.owd})
		if r.haveQD {
			r.jitter += ((fb.qd - r.lastQD).Abs().Seconds() - r.jitter) / 16
		}
		r.lastQD, r.haveQD = fb.qd, true
	}
	if len(r.base) == 0 {
		return
	}
	if r.firstQD.IsZero() {
		r.firstQD = now
	}
	if now.Sub(r.firstQD) < owdWarmup {
		r.over, r.overRun, r.qdOver, r.strongN = 0, 0, false, 0
		return
	}
	base, raw := r.base[0].d, r.base[0].raw
	for _, s := range r.base {
		base, raw = min(base, s.d), min(raw, s.raw)
	}
	// Frames that stopped arriving: the oldest one the client lacks has been
	// on its way pending - raw (the report's way back is at most the raw
	// base, on a symmetric path). Far over the target the controller does
	// not wait for overReports: a capacity drop overflows the host's frame
	// queue within a few frame intervals, and no frame completes meanwhile
	// (they wait for retransmissions). The first such report counts as one
	// over the target, the second in a row decides: after a stall of the
	// client itself one report can find frames missing that wait in its own
	// socket, and the next has them.
	qd, ok := fb.qd, fb.owdValid
	strong := false
	if late := fb.pending - raw; fb.pendingValid && fb.interval <= pendingStallGap &&
		late > base+max(pendingStrong, 4*r.margin(), raw*5/2) {
		r.strongN++
		qd, ok, strong = max(qd, late), true, r.strongN >= 2
	} else {
		r.strongN = 0
	}
	if !ok {
		return
	}
	if len(r.recentQD) > overReports {
		r.recentQD = append(r.recentQD[:0], r.recentQD[1:]...)
	}
	r.recentQD = append(r.recentQD, qdSample{at: now, d: qd, own: fb.qd, ownOK: fb.owdValid && qd == fb.qd})
	r.qdOver = qd > base+r.margin()
	if r.qdOver {
		r.overRun++
	} else {
		r.overRun = 0
	}
	switch {
	case strong:
		r.over = max(r.over+1, overReports)
	case r.qdOver:
		r.over++
	default:
		r.over = 0
	}
}

// draining reports whether the delay is falling: the last sample is
// drainingBy below the one overReports samples earlier. Called with r.mu held.
func (r *rateController) draining() bool {
	n := len(r.recentQD)
	return n > overReports && r.recentQD[n-1].d < r.recentQD[0].d-drainingBy
}

// levelShift reports whether a delay over the target is a step of the
// path's one-way delay rather than a queue, and if so makes it the new base:
// a delay decrease less than owdWindow ago found the delay at the same level
// (within the margin), and the decrease, in effect since, changed nothing.
// A queue behind a rate the path cannot carry keeps growing until a decrease
// is enough, and then drains (draining); a delay that stays put whatever the
// rate is the path's (a relay's route, a client's clock corrected by a
// better sync sample), and the base, the minimum of the last owdWindow, would
// read it as a queue for that long: a decrease on every report after each
// hold (GCC decides on the delay's trend for the same reason). The base
// window then restarts at that decrease. Not where the path held the sender
// back within the base window (pathAt): a queue standing at the capacity
// after a decrease that just reached it looks the same in the delay, but
// comes with losses at a full bottleneck queue or a congestion window
// waiting for acknowledgements. Called with r.mu held.
func (r *rateController) levelShift(now time.Time) bool {
	n := len(r.recentQD)
	if r.shiftAt.IsZero() || now.Sub(r.shiftAt) > owdWindow || n == 0 || !r.pathAt.IsZero() && now.Sub(r.pathAt) <= owdWindow {
		return false
	}
	last := r.recentQD[n-1]
	if !last.ownOK || last.d > r.shiftQD+r.margin() {
		return false // still growing, or frames that do not arrive
	}
	i := 0
	for i < len(r.base)-1 && r.base[i].at.Before(r.shiftAt) {
		i++
	}
	r.base = r.base[i:]
	r.over, r.overRun, r.qdOver = 0, 0, false
	r.shiftAt = time.Time{}
	return true
}

// margin is the target queueing delay over the base. Called with r.mu held.
func (r *rateController) margin() time.Duration {
	j := time.Duration(jitterGain * r.jitter * float64(time.Second))
	return min(max(queueMargin, j), queueMarginMax)
}

// lossFraction is the share of packets lost in the last lossWindow (0 with
// too few packets to tell). Called with r.mu held.
func (r *rateController) lossFraction() float64 {
	var lost, total int64
	for _, c := range r.loss {
		lost += c.n
		total += c.total
	}
	if total < lossMinPackets {
		return 0
	}
	return float64(lost) / float64(total)
}

// fill is what the session sent as a share of the encoder's target over the
// last rateWindow, by time: encoders do not hit their target exactly
// (libx264 gives about 80 % of 30 Mbit/s on the test pattern, a still
// desktop far less), and the session leaves frames out (thinning, a queue
// overflow, frames the client would discard), so a rate the path carries is
// worth rate / fill of target. ok is false with fewer than 5 frames or less
// than half the window. Called with r.mu held.
func (r *rateController) fill(now time.Time) (float64, bool) {
	return windowFill(r.sends, now)
}

// windowFill is the bytes of s over rateWindow as a share of their totals.
func windowFill(s []timedCount, now time.Time) (float64, bool) {
	var n, want int64
	var first time.Time
	frames := 0
	for _, c := range s {
		if now.Sub(c.at) <= rateWindow {
			if frames == 0 {
				first = c.at
			}
			n += c.n
			want += c.total
			frames++
		}
	}
	if frames < 5 || now.Sub(first) < rateWindow/2 || n <= 0 || want <= 0 {
		return 0, false
	}
	return float64(n) / float64(want), true
}

// delivered is the target (kbps) the client's receive rate of the last
// window is worth (rate / fill). ok is false while the reports cover less
// than half the window. Called with r.mu held.
func (r *rateController) delivered(now time.Time, window time.Duration) (float64, bool) {
	var b int64
	var span time.Duration
	for _, c := range r.recv {
		if now.Sub(c.at) <= window {
			b += c.n
			span += c.span
		}
	}
	f, ok := r.fill(now)
	if span < window/2 || !ok {
		return 0, false
	}
	return float64(b) * 8 / span.Seconds() / 1000 / f, true
}

// ackDelivered is delivered from the connection's acknowledgements of the
// last ackWindow, less audio and overhead. It sees the network deliver while
// frames still wait for retransmissions (a frame counts for the client only
// when complete), which after a capacity drop is the new capacity, but only
// on the host's own connection: used where that ends at the client (the
// direct path and the UDP relay; on the splice relay paths it ends at the
// gateway). And only where the path held the sender back: its congestion
// window for at least ackLimited of the window, a queue that lost packets
// in it, or a path that acknowledged markedly less than the session sent
// (ackShort over decreaseWindow: a queue building). Otherwise the sender had
// nothing more to send (a source slower than the session's frame rate,
// frames thinned or paced out) and the acknowledged rate is its own, not the
// path's (as BBR's application-limited samples). Called with r.mu held.
func (r *rateController) ackDelivered(now time.Time) (float64, bool) {
	video, ok := r.ackVideo(now)
	f, fok := r.fill(now)
	if !ok || !fok {
		return 0, false
	}
	return video / f, true
}

// ackVideo is the video rate (kbit/s) the connection's acknowledgements
// carried over the last ackWindow, ok only where the path held the sender
// back (ackDelivered, which divides it by the fill). Called with r.mu held.
func (r *rateController) ackVideo(now time.Time) (float64, bool) {
	var b, lost int64
	var span, lim time.Duration
	var longB int64
	var longSpan time.Duration
	for _, c := range r.acked {
		if now.Sub(c.at) <= ackWindow {
			b += c.n
			lost += c.total
			span += c.span
			lim += c.lim
		}
		longB += c.n
		longSpan += c.span
	}
	if !r.ackDirect || span < ackWindow/2 {
		return 0, false
	}
	if lost == 0 && float64(lim) < ackLimited*float64(span) && !r.ackFallsShort(now, longB, longSpan) {
		return 0, false
	}
	video := float64(b)*8/span.Seconds()/1000 - float64(r.nonVideo)
	return max(0, video), true
}

// ackFallsShort reports whether the video the path acknowledged (b bytes
// over span, the acknowledged reports of the last decreaseWindow, less audio
// and overhead) is less than ackShort of what the session sent over the same
// time. Called with r.mu held.
func (r *rateController) ackFallsShort(now time.Time, b int64, span time.Duration) bool {
	if span < decreaseWindow/2 {
		return false
	}
	var sent int64
	for _, c := range r.sends {
		if now.Sub(c.at) <= span {
			sent += c.n
		}
	}
	acked := float64(b) - float64(r.nonVideo)*1000/8*span.Seconds()
	return sent > 0 && acked < ackShort*float64(sent)
}

// carried is the rate a decrease starts from: on the direct path what the
// acknowledgements carried while the path held the sender back (none
// otherwise: the path carried all it was given; a policy with recvCarried
// then goes on to the receive rates), else (or without them) the lower of
// the receive rates over decreaseWindow and rateWindow (ok false: none is
// known). Called with r.mu held.
func (r *rateController) carried(now time.Time) (float64, bool) {
	if r.ackDirect && len(r.acked) > 0 {
		if d, ok := r.ackDelivered(now); ok || !r.pol().recvCarried {
			return d, ok
		}
	}
	d1, ok1 := r.delivered(now, decreaseWindow)
	d2, ok2 := r.delivered(now, rateWindow)
	switch {
	case ok1 && ok2:
		return min(d1, d2), true
	case ok1:
		return d1, true
	}
	return d2, ok2
}

// decrease lowers the target by rateDecreaseFactor from the delivered rate
// (or the target, if lower), or at the floor the frame rate, and returns the
// change if the encoder may take it now (else tick applies it). Called with
// r.mu held.
func (r *rateController) decrease(now time.Time, why string) (rateChange, bool) {
	// The rate the path saw: an increase the encoder has not taken yet (a
	// policy that takes changes seconds apart) never reached it.
	cur := min(r.est, float64(r.applied))
	if d, ok := r.carried(now); ok && d < cur {
		cur = d
	}
	if d, ok := r.queueCapacity(); ok && why == "delay" && d < cur {
		cur = d
	}
	// At most a halving at once: a stall of a tenth of a second (the
	// host's, the client's or the path's) leaves the last ackWindow with
	// next to nothing acknowledged.
	cur = max(cur, r.est*decreaseMinShare)
	r.lastGood, r.passedGood = cur, time.Time{}
	shiftQD, shift := time.Duration(0), false
	if n := len(r.recentQD); n > 0 {
		shiftQD, shift = r.recentQD[n-1].d, r.recentQD[n-1].ownOK
	}
	r.over, r.loss, r.lastDecrease, r.recentQD = 0, r.loss[:0], now, r.recentQD[:0]
	r.thinSince = now // thinning that goes on decreases again only after another thinSustain
	to := max(r.floor(), cur*rateDecreaseFactor)
	// A delay decrease of the bitrate on the reports' own delays (not on
	// frames that do not arrive, not a frame-rate step at the floor, which
	// sends as much), on a path that has not held the sender back for the
	// base window, is what a level shift is told by (levelShift). Where it
	// did (losses at a full bottleneck queue, the congestion window waiting
	// for acknowledgements) a delay that stays put after the decrease is a
	// queue standing at the capacity, not a step of the path's delay.
	r.shiftAt = time.Time{}
	if why == "delay" && shift && to < r.est && (r.pathAt.IsZero() || now.Sub(r.pathAt) > owdWindow) {
		r.shiftAt, r.shiftQD = now, shiftQD
	}
	if to >= r.est && !r.fpsDown(now) {
		return rateChange{}, false // at the floor, and at the lowest frame rate
	}
	r.est = min(r.est, to)
	r.waitLive, r.waitKbps, r.decidedAt = true, int(math.Round(r.est)), now
	r.holdUntil, r.lossFrom = now.Add(liveTimeout), now.Add(liveTimeout)
	r.pending = why
	return r.decide(now)
}

// queueCapacity is the rate (kbps of target) the path carries by the
// growth of the queue over the recent samples: sending at rate R into a
// bottleneck of capacity C grows the queueing delay by (R - C) / C seconds
// per second, so C = R / (1 + growth). It sees a capacity drop at once,
// where the delivered rates still hold the time before it (a decrease right
// after the drop from those alone stays above the new capacity, and the
// queue overflows before the next). From the reports' own delays (not the
// pending frame's bound, which grows at 1 s per second whatever the
// capacity). Only for a queue that grows by at least
// queueGrowthMin and stands at least queueGrowthOver over the base: a few
// milliseconds of jitter over 75 ms would read as a large growth; and at
// most half the rate (queueGrowthMax): a stall of the client's own (its
// reports' delays jump by the stall) is no capacity. Called with r.mu held.
func (r *rateController) queueCapacity() (float64, bool) {
	n := len(r.recentQD)
	if n <= overReports || r.liveKbps <= 0 || len(r.base) == 0 {
		return 0, false
	}
	first, last := r.recentQD[0], r.recentQD[n-1]
	span := last.at.Sub(first.at)
	if span < 2*rateReportMin || !first.ownOK || !last.ownOK {
		return 0, false
	}
	base := r.base[0].d
	for _, s := range r.base {
		base = min(base, s.d)
	}
	g := (last.own - first.own).Seconds() / span.Seconds()
	if g < queueGrowthMin || last.own-base < queueGrowthOver {
		return 0, false
	}
	return float64(r.liveKbps) / (1 + min(g, queueGrowthMax)), true
}

// fpsDown lowers the frame rate a rung, if the ladder has one below (the
// policy's: encoder.FPSSteps down to fpsFloor for an encoder that changes
// its frame rate in place, fpsHoldLive after the last change, else fpsRungs).
// Called with r.mu held.
func (r *rateController) fpsDown(now time.Time) bool {
	next := r.fps
	if r.pol().fineFPS {
		if !r.lastFPSChange.IsZero() && now.Sub(r.lastFPSChange) < fpsHoldLive {
			return false
		}
		floor := r.fpsFloor
		if floor <= 0 {
			floor = fpsFloorLive
		}
		next = encoder.LowerFPS(r.fps, floor)
	} else {
		for _, f := range fpsRungs {
			if f < r.fps && f >= r.fpsFloor {
				next = f
				break
			}
		}
	}
	if next >= r.fps {
		return false
	}
	r.fps, r.lastFPSChange = next, now
	return true
}

// fpsUp raises the frame rate a rung towards the settings' rate (the
// policy's ladder, as fpsDown). Called with r.mu held.
func (r *rateController) fpsUp(now time.Time) {
	next := r.fpsMax
	if r.pol().fineFPS {
		next = encoder.RaiseFPS(r.fps, r.fpsMax)
	} else {
		for _, f := range fpsRungs {
			if f > r.fps && f < next {
				next = f
			}
		}
	}
	r.fps, r.lastFPSChange = next, now
}

// fpsHold is the time between frame-rate changes of the policy's ladder.
// Called with r.mu held.
func (r *rateController) fpsHold() time.Duration {
	if r.pol().fineFPS {
		return fpsHoldLive
	}
	return fpsHold
}

// congestion handles a congestion signal from outside the reports and
// returns the cut, if any. A client's delay report (signalDelay) decreases
// like the controller's own delay decision. An emergency (signalOverflow,
// signalDecoder) cuts by rateEmergencyFactor (an overflow at least to
// rateDecreaseFactor x the delivered rate, but from at least
// decreaseMinShare of the target), in the encoder at once, but not within
// rateEmergencyGap of the last decrease of any kind: right after one, the old
// generation (or rate) that still streams overflows the queue, and the
// session only hurries the switch. An overflow behind a key frame past its
// deadline (signalKeyOverflow) cuts even then. Any overflow, cut or not,
// holds the increases for rateEmergencyGap. A decoder flush that cuts also
// caps later increases (rateDecoderPct of the bitrate it cut from) for
// rateDecoderHold, twice as long with each further flush. Nothing is cut at
// the floor, or before the first generation.
func (r *rateController) congestion(sig rateSignal) (rateChange, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	if r.applied <= 0 {
		return rateChange{}, false
	}
	if sig == signalDelay {
		if r.waitLive || now.Before(r.holdUntil) {
			return rateChange{}, false // a decrease is under way
		}
		return r.decrease(now, "client")
	}
	overflow := sig == signalOverflow || sig == signalKeyOverflow
	if overflow {
		r.overflowAt = now
	}
	if sig != signalKeyOverflow && !r.lastDecrease.IsZero() && now.Sub(r.lastDecrease) < rateEmergencyGap {
		return rateChange{}, false
	}
	from := r.est
	to := from * rateEmergencyFactor
	why := "decoder"
	if overflow {
		why = "overflow"
		r.passedGood = time.Time{}
		v, ackOK := r.ackVideo(now)
		d, ok := r.carried(now)
		switch {
		case sig == signalKeyOverflow && ackOK:
			// The key frame went out at the path's rate, not the pacer's:
			// the next one, paced at PacingGain x the target, at about that
			// rate (at most a halving).
			to = min(to, max(v/cc.PacingGain, from*decreaseMinShare))
			r.lastGood = v
		case ok:
			// At most a halving, as in decrease: frames that stalled for
			// a tenth of a second leave next to nothing acknowledged.
			d = max(d, from*decreaseMinShare)
			to = min(to, d*rateDecreaseFactor)
			r.lastGood = d
		default:
			r.lastGood = to / rateDecreaseFactor
		}
	}
	to = max(to, r.floor())
	if to >= from {
		return rateChange{}, false
	}
	// Never below the floor: the increases' limit would take the target
	// under it.
	if c := max(int(r.floor()), int(from*rateDecoderPct/100)); sig == signalDecoder && (r.decoderCap == 0 || c < r.decoderCap) {
		r.decoderCap = c
	}
	if sig == signalDecoder {
		r.decoderCapAt = now
		r.decoderHold = min(max(rateDecoderHold, 2*r.decoderHold), 8*rateDecoderHold)
	}
	r.est = to
	r.lastDecrease = now
	r.over, r.loss = 0, r.loss[:0]
	r.waitLive, r.waitKbps, r.decidedAt = true, int(math.Round(to)), now
	r.holdUntil, r.lossFrom = now.Add(liveTimeout), now.Add(liveTimeout)
	c := rateChange{fromKbps: r.applied, toKbps: int(math.Round(to)), fromFPS: r.appliedFP, toFPS: r.fps, down: true, urgent: true, why: why}
	r.applied, r.lastApply, r.pending = c.toKbps, now, ""
	return c, true
}

// tick advances the controller by the time since the last tick: it raises
// the target where it may and returns the change the encoder should take now
// (a pending decrease, an increase or a frame-rate step). stalled: frames
// went out ackTimeout ago and no feedback has covered them.
func (r *rateController) tick(stalled bool) (rateChange, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	dt := min(now.Sub(r.lastTick), 2*rateTick)
	if r.lastTick.IsZero() || dt < 0 {
		dt = 0
	}
	r.lastTick = now
	if r.applied <= 0 {
		return rateChange{}, false
	}
	if r.waitLive && now.Sub(r.decidedAt) >= liveTimeout {
		r.settled(now)
	}
	if r.decoderCap > 0 && now.Sub(r.decoderCapAt) >= max(r.decoderHold, rateDecoderHold) {
		r.decoderCap = 0 // the decoder had its time: increases probe again
	}
	if r.mayIncrease(now, stalled) {
		r.increase(now, dt)
	}
	if hold := r.fpsHold(); r.fps < r.fpsMax && r.mayIncrease(now, stalled) && r.est >= min(1.5*r.floor(), r.limit()) &&
		now.Sub(r.lastDecrease) >= hold && now.Sub(r.lastFPSChange) >= hold {
		r.fpsUp(now)
	}
	return r.decide(now)
}

// mayIncrease reports whether nothing holds an increase: no decrease under
// way or draining, no frame-queue overflow in the last rateEmergencyGap (an
// overflow refused a cut is no less a sign the path or the sender does not
// keep up), fresh feedback (frames covered in the last feedbackFresh) with
// the delay under the target and the client's decoder keeping up, no
// stalled path; a client that never sent feedback gets increases
// noFeedbackQuiet after the last decrease. Called with r.mu held.
func (r *rateController) mayIncrease(now time.Time, stalled bool) bool {
	if r.waitLive || now.Before(r.holdUntil) || !r.thinLast.IsZero() && now.Sub(r.thinLast) < thinQuiet {
		return false
	}
	if !r.overflowAt.IsZero() && now.Sub(r.overflowAt) < rateEmergencyGap {
		return false
	}
	if !r.everFeedback {
		return now.Sub(r.lastDecrease) >= noFeedbackQuiet
	}
	return !stalled && now.Sub(r.lastReport) <= feedbackFresh && r.over == 0 && !r.qdOver && r.decodeQ <= max(decodeQueueMin, r.liveFPS/10)
}

// increase raises the target for dt. Called with r.mu held.
func (r *rateController) increase(now time.Time, dt time.Duration) {
	lim := r.limit()
	if r.est >= lim || dt <= 0 {
		return
	}
	g := incSlow
	switch {
	case r.lastGood > 0 && r.est < r.lastGood:
		x := (r.lastGood - r.est) / r.lastGood
		g = incSlow + (incFast-incSlow)*min(1, max(0, (x-fastBelow)/fastSpan))
		r.passedGood = time.Time{}
	case r.lastGood > 0:
		if r.passedGood.IsZero() {
			r.passedGood = now
		}
		g = min(incFast, incSlow+incAccel*now.Sub(r.passedGood).Seconds())
	}
	next := r.est * (1 + g*dt.Seconds())
	if d, ok := r.delivered(now, rateWindow); ok {
		next = min(next, max(r.est, recvHeadroom*d))
	}
	r.est = min(next, lim)
}

// decide returns the change the encoder should take now, if any: the
// target (rounded) or frame rate differs from what the encoder was told and
// the policy's gap since the last change has passed; increases also wait for
// minStep (unless they reach the limit). Called with r.mu held.
func (r *rateController) decide(now time.Time) (rateChange, bool) {
	kbps := int(math.Round(r.est))
	if r.applied <= 0 || (kbps == r.applied && r.fps == r.appliedFP) {
		r.pending = ""
		return rateChange{}, false
	}
	p := r.pol()
	since := now.Sub(r.lastApply)
	down := kbps < r.applied || (kbps == r.applied && r.fps < r.appliedFP)
	why := r.pending
	if why == "" {
		why = "recovery"
		if down {
			why = "delay"
		}
	}
	// Only the controller's own decisions: an older client's delay report
	// ("client") says nothing about the capacity.
	urgent := down && why != "client" && float64(kbps) <= p.cutUrgent*float64(r.applied)
	if down {
		if since < p.decGap && !urgent {
			return rateChange{}, false
		}
	} else {
		if since < p.incGap || (r.fps == r.appliedFP && float64(kbps) < float64(r.applied)*(1+p.minStep) && float64(kbps) < r.limit()) {
			return rateChange{}, false
		}
		// Near the last known-good rate, an encoder that takes changes
		// seconds apart (or late: a restart) steps by at most nearStep: an
		// overshoot there fills the queue until the next change.
		if p.incGap >= time.Second && r.lastGood > 0 && float64(r.applied) >= (1-nearBelow)*r.lastGood && float64(r.applied) <= (1+nearAbove)*r.lastGood {
			kbps = min(kbps, int(float64(r.applied)*(1+nearStep)))
			r.est = min(r.est, float64(kbps))
		}
	}
	c := rateChange{fromKbps: r.applied, toKbps: kbps, fromFPS: r.appliedFP, toFPS: r.fps, down: down, urgent: urgent, why: why}
	r.applied, r.appliedFP, r.lastApply, r.pending = kbps, r.fps, now, ""
	return c, true
}

// decreasedAt returns when the controller last decided a decrease.
func (r *rateController) decreasedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDecrease
}

// kbps returns the encoder's target and the ceiling (0, 0 before the first
// generation).
func (r *rateController) kbps() (cur, ceiling int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied, r.ceiling
}

// decoderLimit returns the cap on increases after a decoder flush (0: none).
func (r *rateController) decoderLimit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.decoderCap
}

// state is a snapshot for the session's stats log: the continuous target,
// the frame rate, the queueing-delay target (base + margin) and the loss.
func (r *rateController) state() (est float64, fps int, margin time.Duration, loss float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.est, r.fps, r.margin(), r.lossFraction()
}

// trimCounts drops the samples older than window.
func trimCounts(s []timedCount, now time.Time, window time.Duration) []timedCount {
	i := 0
	for i < len(s) && now.Sub(s[i].at) > window {
		i++
	}
	if i > 0 {
		s = append(s[:0], s[i:]...)
	}
	return s
}
