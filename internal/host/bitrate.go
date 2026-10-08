package host

import (
	"math"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// Video bitrate control (GUIDE 2.2): a delay-based rate controller in the
// style of GCC and SCReAM v2, replacing the interim of step 1.5. It owns the
// session's video bitrate target and decides from the client's receive
// reports (proto.RateReport every 25 ms; older clients: their per-frame 0x40
// acks) and the media congestion controller's packet counters:
//
//   - Queueing delay: the one-way delay of the frames a report covers (p50),
//     less what the host's own pacer explains (sendTrack: a key frame paced
//     out at 1.2 x the target delays itself and the frames behind it). The
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
//     nothing.
//   - Decrease to rateDecreaseFactor (x0.85) when the delay stays over the
//     target for overReports consecutive reports and is not falling (a queue
//     that drains needs no second decrease: the gradient), or more than
//     lossThreshold of the packets were lost in the last lossWindow. The
//     factor applies to the rate the path delivered, where that is lower
//     than the target (carried): on the direct path what the connection's
//     acknowledgements carried in the last ackWindow, else the client's
//     receive rate (the lower of decreaseWindow and rateWindow), both divided
//     by the encoder's fill; after a capacity drop that is the new capacity.
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
//   - Emergencies as in 1.5: a host frame-queue overflow or a client whose
//     decoder fell behind cuts at once (rateEmergencyFactor) with an urgent
//     restart, but not within rateEmergencyGap of any other decrease.
//   - At the floor, the frame rate goes down a rung (120 -> 90 -> 60) before
//     anything else, and back up once the bitrate is well above the floor.
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
	// bitrate B, increases stop at this share of B until the settings change.
	rateDecoderPct = 85
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

	lossWindow     = time.Second
	lossThreshold  = 0.02
	lossMinPackets = 100 // fewer packets in the window: no loss decision
	// lossSettle: losses count from this long after a decrease is in the
	// encoder: those detected before were of packets sent at the old rate.
	lossSettle = 300 * time.Millisecond

	rateWindow     = time.Second            // receive and encoder output rates (the increase cap)
	decreaseWindow = 250 * time.Millisecond // ... for a decrease: what the path carries now
	ackWindow      = 100 * time.Millisecond // acknowledged bytes: the connection's delivery rate now
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
	// relay paths the gateway buffers what its client leg cannot carry, so
	// the host's frame queue does not overflow).
	ackTimeout = time.Second
	// noFeedbackQuiet: a client that never sends feedback (no reports, no
	// acks) gets increases (incSlow) only this long after the last decrease.
	noFeedbackQuiet = 10 * time.Second
	// decodeQueueMin: a backlog in the client's decoder above max(this,
	// fps/10) frames holds increases: the client flushes its decoder at that
	// backlog (stream-worker.js checkDecoderBacklog); software decoders keep a
	// few frames in flight normally (frame threading).
	decodeQueueMin = 4
	// fpsHold is the minimum time between frame-rate changes, and from a
	// decrease to a frame-rate increase.
	fpsHold = 5 * time.Second
)

// fpsRungs are the frame rates the controller steps down through at the
// bitrate floor (GUIDE 2.2, AMD Streaming SDK QoS), before resolution.
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
	// rateDecoderPct of the bitrate the decoder fell behind at, since the
	// delay does not show the client's decode capacity.
	signalDecoder
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
}

// ratePolicy picks the apply policy for the pipeline that streams: a live
// change qualified seamless (recon-host qualify, GUIDE 3.6: no key frame)
// every 250 ms; one only assumed seamless (the helper's caps defaults) every
// second; a flushing encoder (a key frame per change: GUIDE 3.6 "change less
// often") and a restart (an FFmpeg generation: a new process, a key frame and
// an overlapped switch; or a helper that cannot change live) far less often.
func ratePolicy(c media.PipelineCaps) applyPolicy {
	switch {
	case c.LiveBitrate && !c.LiveBitrateFlush && c.LiveBitrateMeasured:
		return applyPolicy{"seamless", 0, 250 * time.Millisecond, 0.02, 150 * time.Millisecond, 0}
	case c.LiveBitrate && !c.LiveBitrateFlush:
		return applyPolicy{"seamless (assumed)", 0, time.Second, 0.03, 300 * time.Millisecond, 0}
	case c.LiveBitrate:
		return applyPolicy{"flush", 250 * time.Millisecond, 2 * time.Second, 0.05, 500 * time.Millisecond, 0}
	}
	return applyPolicy{"restart", 500 * time.Millisecond, time.Second, 0.05, time.Second, 0.75}
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
	// audio and overhead.
	ackedValid   bool
	acked        int64
	nonVideoKbps int
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
	est       float64     // kbps: the continuous target (0 until the first generation)
	fps       int         // the frame rate the ladder allows (<= fpsMax)
	applied   int         // kbps the encoder was told (0 before the first generation)
	appliedFP int         // its frame rate
	lastApply time.Time   // when the encoder was last told a new rate
	liveKbps  int         // kbps of the encoder that streams (live; 0: unknown)
	pending   string      // why of a change decided but not yet applied

	// Delay.
	base      []timedDelay // qd samples of the last owdWindow
	jitter    float64      // seconds: mean |qd change| between reports (RFC 3550 style, 1/16)
	lastQD    time.Duration
	haveQD    bool
	firstQD   time.Time  // the first delay sample (owdWarmup)
	strongN   int        // consecutive reports with a pending frame far over the target
	qdOver    bool       // the last report's delay was over the target
	over      int        // consecutive reports over the target
	recentQD  []qdSample // the last overReports+1 samples, oldest first
	recv      []timedCount
	out       []timedCount // encoder output: n bytes, total the bytes its target asked for
	loss      []timedCount
	acked     []timedCount // acknowledged bytes per report (span: host time since the previous)
	ackDirect bool         // the acknowledgements come from the client (setPath)
	nonVideo  int          // kbps of audio and overhead in the acknowledged bytes
	liveFPS   int          // frame rate of the encoder that streams
	lastAcked time.Time
	lossFrom  time.Time // losses before are not counted (lossSettle)
	decodeQ   int

	// Decisions.
	lastDecrease  time.Time // last decrease (any): emergencies are rateEmergencyGap apart from it
	waitLive      bool      // a decrease is decided; the encoder has not confirmed it
	waitKbps      int       // ... at this rate
	decidedAt     time.Time // when
	holdUntil     time.Time // no delay or loss decrease and no increase before
	lastGood      float64   // kbps the path delivered at the last decrease (0: none yet)
	passedGood    time.Time // when est went above lastGood (zero: below it)
	decoderCap    int       // kbps: increases stop here after a decoder flush; 0: none
	lastFPSChange time.Time
	lastTick      time.Time

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
	r.est, r.fps, r.decoderCap, r.lastGood = 0, 0, 0, 0
	r.waitLive, r.pending = false, ""
	r.holdUntil, r.lastDecrease, r.passedGood = time.Time{}, time.Time{}, time.Time{}
	r.over = 0
}

// hold is called while nothing streams (the session is paused, or no encoder
// generation is live): there is no delay to judge and nothing to increase.
func (r *rateController) hold() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.over, r.qdOver = 0, false
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

// output records an encoded frame of n bytes (the encoder's output rate).
func (r *rateController) output(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	want := int64(0)
	if r.liveKbps > 0 && r.liveFPS > 0 {
		want = int64(r.liveKbps) * 1000 / 8 / int64(r.liveFPS)
	}
	r.out = trimCounts(append(r.out, timedCount{at: now, n: int64(n), total: want}), now, rateWindow)
}

// setPath tells the controller whether the session's connection ends at the
// client (the direct path): its acknowledgements are then the client's.
func (r *rateController) setPath(direct bool) {
	r.mu.Lock()
	r.ackDirect = direct
	r.mu.Unlock()
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
	if fb.ackedValid {
		if !r.lastAcked.IsZero() && now.After(r.lastAcked) {
			r.acked = trimCounts(append(r.acked, timedCount{at: now, n: fb.acked, span: now.Sub(r.lastAcked)}), now, ackWindow)
		}
		r.lastAcked, r.nonVideo = now, fb.nonVideoKbps
	}
	r.decodeQ = fb.decodeQ
	r.delay(now, fb)
	if r.fixed || r.applied <= 0 || r.waitLive || now.Before(r.holdUntil) {
		return rateChange{}, false
	}
	switch {
	case r.over >= overReports && !r.draining():
		return r.decrease(now, "delay")
	case r.lossFraction() > lossThreshold:
		return r.decrease(now, "loss")
	}
	return rateChange{}, false
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
		r.over, r.qdOver, r.strongN = 0, false, 0
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

// fill is the encoder's output as a share of its target over the last
// rateWindow: encoders do not hit their target exactly (libx264 gives about
// 80 % of 30 Mbit/s on the test pattern, a still desktop far less), so a
// rate the path carries is worth rate / fill of target. ok is false with
// fewer than 5 frames or less than half the window. Called with r.mu held.
func (r *rateController) fill(now time.Time) (float64, bool) {
	var n, want int64
	var first time.Time
	frames := 0
	for _, c := range r.out {
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
// direct path; on the relay paths it ends at the gateway). Called with r.mu
// held.
func (r *rateController) ackDelivered(now time.Time) (float64, bool) {
	var b int64
	var span time.Duration
	for _, c := range r.acked {
		if now.Sub(c.at) <= ackWindow {
			b += c.n
			span += c.span
		}
	}
	f, ok := r.fill(now)
	if !r.ackDirect || span < ackWindow/2 || !ok {
		return 0, false
	}
	video := float64(b)*8/span.Seconds()/1000 - float64(r.nonVideo)
	return max(0, video) / f, true
}

// carried is the rate a decrease starts from: on the direct path what the
// acknowledgements carried, else (or without them) the lower of the
// receive rates over decreaseWindow and rateWindow (ok false: none is
// known). Called with r.mu held.
func (r *rateController) carried(now time.Time) (float64, bool) {
	if d, ok := r.ackDelivered(now); ok {
		return d, true
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
	cur := r.est
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
	r.over, r.loss, r.lastDecrease, r.recentQD = 0, r.loss[:0], now, r.recentQD[:0]
	to := max(r.floor(), cur*rateDecreaseFactor)
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

// fpsDown lowers the frame rate a rung, if the ladder has one below. Called
// with r.mu held.
func (r *rateController) fpsDown(now time.Time) bool {
	for _, f := range fpsRungs {
		if f < r.fps {
			r.fps, r.lastFPSChange = f, now
			return true
		}
	}
	return false
}

// fpsUp raises the frame rate a rung towards the settings' rate. Called with
// r.mu held.
func (r *rateController) fpsUp(now time.Time) {
	next := r.fpsMax
	for _, f := range fpsRungs {
		if f > r.fps && f < next {
			next = f
		}
	}
	r.fps, r.lastFPSChange = next, now
}

// congestion handles a congestion signal from outside the reports and
// returns the cut, if any. A client's delay report (signalDelay) decreases
// like the controller's own delay decision. An emergency (signalOverflow,
// signalDecoder) cuts by rateEmergencyFactor (an overflow at least to
// rateDecreaseFactor x the delivered rate), in the encoder at once, but not
// within rateEmergencyGap of the last decrease of any kind: right after one,
// the old generation (or rate) that still streams overflows the queue, and
// the session only hurries the switch. A decoder flush that cuts also caps
// later increases (rateDecoderPct of the bitrate it cut from) until reset.
// Nothing is cut at the floor, or before the first generation.
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
	if !r.lastDecrease.IsZero() && now.Sub(r.lastDecrease) < rateEmergencyGap {
		return rateChange{}, false
	}
	from := r.est
	to := from * rateEmergencyFactor
	why := "decoder"
	if sig == signalOverflow {
		why = "overflow"
		if d, ok := r.carried(now); ok {
			to = min(to, d*rateDecreaseFactor)
			r.lastGood = d
		} else {
			r.lastGood = to / rateDecreaseFactor
		}
		r.passedGood = time.Time{}
	}
	to = max(to, r.floor())
	if to >= from {
		return rateChange{}, false
	}
	if c := int(from * rateDecoderPct / 100); sig == signalDecoder && (r.decoderCap == 0 || c < r.decoderCap) {
		r.decoderCap = c
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
	if r.mayIncrease(now, stalled) {
		r.increase(now, dt)
	}
	if r.fps < r.fpsMax && r.mayIncrease(now, stalled) && r.est >= 1.5*r.floor() &&
		now.Sub(r.lastDecrease) >= fpsHold && now.Sub(r.lastFPSChange) >= fpsHold {
		r.fpsUp(now)
	}
	return r.decide(now)
}

// mayIncrease reports whether nothing holds an increase: no decrease under
// way or draining, fresh feedback (frames covered in the last
// feedbackFresh) with the delay under the target and the client's decoder
// keeping up, no stalled path; a client that never sent feedback gets
// increases noFeedbackQuiet after the last decrease. Called with r.mu held.
func (r *rateController) mayIncrease(now time.Time, stalled bool) bool {
	if r.waitLive || now.Before(r.holdUntil) {
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
