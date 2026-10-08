package host

import (
	"slices"
	"sync"
	"time"
)

// Video bitrate control on the FFmpeg path (guide step 1.5, B3), the interim
// until the delay-gradient rate controller (2.2). A congestion signal cuts the
// bitrate by 25 %; after a quiet period (no congestion signal, the client
// acknowledging the frames it is sent, with a one-way delay close to its
// recent minimum) the bitrate goes back up 15 % at a time to the user's
// setting, or below it after the client's decoder fell behind.
// Every change is a new encoder generation (the FFmpeg command line cannot
// change the bitrate of a running encoder), so changes are rate-limited. On
// the native helper an encoder qualified to change seamlessly (recon-host
// qualify, GUIDE 3.6) changes in place without a key frame, so changes there
// may follow each other after rateSeamlessGap; one that flushes (a key frame
// per change) keeps the full period.
const (
	rateCutPct    = 75   // a congestion signal cuts the bitrate to 75 %
	rateRaisePct  = 115  // a quiet period raises it by 15 %
	rateFloorKbps = 2000 // cuts stop here (at the ceiling if that is lower)
	// rateDecoderPct: after a client flushed its decoder (signalDecoder) at
	// bitrate B, raises stop at this share of B until the settings change.
	rateDecoderPct = 85
	// ratePeriod is both the quiet period before a raise and the minimum time
	// between two changes (cuts or raises), except emergency cuts.
	ratePeriod = 10 * time.Second
	// rateEmergencyGap is the minimum time between two emergency cuts (host
	// frame-queue overflow, a client that flushed its decoder).
	rateEmergencyGap = 2 * time.Second
	// rateSeamlessGap is the minimum time between two changes (cuts or
	// raises) on an encoder qualified to change its bitrate seamlessly: the
	// interval the qualification steps at. The quiet period before a raise
	// stays ratePeriod.
	rateSeamlessGap = 2 * time.Second
	// rateTick is how often the session evaluates the delay and checks for a
	// raise; each evaluation judges the acknowledgements since the last.
	rateTick = 500 * time.Millisecond
	// owdWindow is the window of the one-way delay minimum; the delay counts
	// as low while the median since the last evaluation is within owdSlack
	// of that minimum.
	owdWindow     = 2 * time.Second
	owdSlack      = 10 * time.Millisecond
	owdMaxSamples = 4096 // bound on the kept acknowledgements (2 s at 240 fps is 480)
	// ackTimeout: a frame handed to the transport this long ago with no
	// acknowledgement since (500 ms plus a generous round trip and decode)
	// means the path to the client stalls, from a client that acknowledges.
	ackTimeout = time.Second
)

// rateSignal is the kind of a congestion signal.
type rateSignal int

const (
	// signalDelay: the client saw the one-way delay grow. It cuts at most
	// once per period after any change.
	signalDelay rateSignal = iota
	// signalOverflow: the host's frame queue overflowed. An emergency: it
	// cuts at most once every rateEmergencyGap.
	signalOverflow
	// signalDecoder: the client flushed its decoder, which fell behind. An
	// emergency like signalOverflow; a cut also caps later raises at
	// rateDecoderPct of the bitrate the decoder fell behind at, since the
	// delay the raises look at does not show the client's decode capacity.
	signalDecoder
)

// rateController decides the session's video bitrate. Methods are safe for
// concurrent use; the zero value is ready (clock time.Now, period ratePeriod).
type rateController struct {
	now    func() time.Time // nil: time.Now (tests use a fake clock)
	period time.Duration    // 0: ratePeriod (test hook rate-period shortens it)
	gap    time.Duration    // minimum time between two changes, 0: the period (setGap; guarded by mu)

	mu         sync.Mutex
	ceiling    int       // kbps: the bitrate the settings ask for, never exceeded
	cur        int       // kbps: the current target; 0 until the next generation starts
	decoderCap int       // kbps: raises stop here after a decoder flush (signalDecoder); 0: none
	lastChange time.Time // last cut or raise; zero: none since the settings
	lastCut    time.Time // last cut, for the emergency gap
	quietSince time.Time // start of the quiet period: last congestion signal, high delay, stall, pause or reset
	lastEval   time.Time // last delay evaluation (tick)
	owd        []owdSample
	scratch    []time.Duration
	acked      bool      // the client has acknowledged a frame (it does: clock synced, v2 or later)
	unacked    time.Time // first frame handed to the transport since the last acknowledgement; zero: none
}

type owdSample struct {
	at  time.Time
	owd time.Duration
}

func (r *rateController) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *rateController) interval() time.Duration {
	if r.period > 0 {
		return r.period
	}
	return ratePeriod
}

// changeGap is the minimum time between two changes (cuts on a delay
// report, raises): the period, or the shorter gap set for the encoder.
// Called with r.mu held.
func (r *rateController) changeGap() time.Duration {
	if r.gap > 0 && r.gap < r.interval() {
		return r.gap
	}
	return r.interval()
}

// setGap sets the minimum time between two changes for the encoder that
// streams now (rateSeamlessGap for a qualified seamless one; 0: the period).
func (r *rateController) setGap(d time.Duration) {
	r.mu.Lock()
	r.gap = d
	r.mu.Unlock()
}

// target returns the bitrate of a new encoder generation: the current
// target, never above ceiling (the bitrate of the settings, kbps). The first
// generation, and the first after reset, runs at the ceiling. A back-off
// stays in effect for every restart (key frames, encoder failures, resume)
// until the controller raises the bitrate again or the settings change.
func (r *rateController) target(ceiling int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quietSince.IsZero() {
		r.quietSince = r.clock()
	}
	r.ceiling = ceiling
	if r.cur <= 0 || r.cur > r.ceiling {
		r.cur = r.ceiling
	}
	return r.cur
}

// reset drops the back-off and the decoder's cap: the user chose new video
// settings. The next generation runs at the (new) ceiling; the quiet period
// starts again.
func (r *rateController) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur, r.decoderCap = 0, 0
	r.lastChange, r.lastCut = time.Time{}, time.Time{}
	r.quietSince = r.clock()
}

// hold restarts the quiet period without a congestion signal: nothing
// streams (the session is paused, or no encoder generation is live), so there
// is no delay to judge.
func (r *rateController) hold() {
	r.mu.Lock()
	r.quietSince = r.clock()
	r.mu.Unlock()
}

// congestion handles a congestion signal and reports whether it cut the
// bitrate (from -> to kbps). Every signal restarts the quiet period. A
// client's delay report (signalDelay) cuts at most once per period (or gap,
// setGap) after any change; an emergency (signalOverflow, signalDecoder) cuts at most once
// every rateEmergencyGap. Nothing is cut at the floor. A decoder flush that
// cuts also caps later raises (rateDecoderPct of from) until reset.
func (r *rateController) congestion(sig rateSignal) (from, to int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.quietSince = now
	if r.cur <= 0 {
		return 0, 0, false // no generation yet
	}
	if sig != signalDelay {
		if !r.lastCut.IsZero() && now.Sub(r.lastCut) < rateEmergencyGap {
			return r.cur, r.cur, false
		}
	} else if !r.lastChange.IsZero() && now.Sub(r.lastChange) < r.changeGap() {
		return r.cur, r.cur, false
	}
	next := max(r.cur*rateCutPct/100, min(rateFloorKbps, r.ceiling))
	if next >= r.cur {
		return r.cur, r.cur, false
	}
	from = r.cur
	r.cur = next
	r.lastChange, r.lastCut = now, now
	if c := from * rateDecoderPct / 100; sig == signalDecoder && (r.decoderCap == 0 || c < r.decoderCap) {
		r.decoderCap = c
	}
	return from, next, true
}

// sent records a frame handed to the transport.
func (r *rateController) sent() {
	r.mu.Lock()
	if r.unacked.IsZero() {
		r.unacked = r.clock()
	}
	r.mu.Unlock()
}

// ack records the one-way delay of a frame the client acknowledged.
func (r *rateController) ack(owd time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acked, r.unacked = true, time.Time{}
	if len(r.owd) >= owdMaxSamples {
		r.owd = slices.Delete(r.owd, 0, len(r.owd)-owdMaxSamples+1)
	}
	r.owd = append(r.owd, owdSample{at: r.clock(), owd: owd})
}

// tick judges the one-way delay of the frames acknowledged since the last
// tick and raises the bitrate when it may (from -> to kbps): the bitrate is
// below the ceiling (and the decoder's cap), the quiet period has lasted a
// whole period, and the last change is a period (or gap, setGap) ago. The delay is high, and
// the quiet period starts again, when the median since the last tick is more
// than owdSlack above the minimum of the last owdWindow, and also when frames
// went out ackTimeout ago or longer with no acknowledgement since, from a
// client that acknowledges frames: the path to the client stalls. On the
// relay paths the gateway buffers what its client leg cannot carry, so the
// host's frame queue does not overflow then. A tick with nothing sent (a
// still desktop) or for a client that never acknowledges (no clock sync yet,
// a v1 client) has no delay to judge: the quiet period runs on time alone.
func (r *rateController) tick() (from, to int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	since := r.lastEval
	r.lastEval = now
	i := 0
	for i < len(r.owd) && now.Sub(r.owd[i].at) > owdWindow {
		i++
	}
	r.owd = r.owd[i:]
	if len(r.owd) > 0 {
		lo := r.owd[0].owd
		recent := r.scratch[:0]
		for _, s := range r.owd {
			lo = min(lo, s.owd)
			if s.at.After(since) {
				recent = append(recent, s.owd)
			}
		}
		if len(recent) > 0 {
			slices.Sort(recent)
			if recent[len(recent)/2] > lo+owdSlack {
				r.quietSince = now
			}
		}
		r.scratch = recent[:0]
	}
	if r.acked && !r.unacked.IsZero() && now.Sub(r.unacked) >= ackTimeout {
		r.quietSince = now
	}
	if r.quietSince.IsZero() {
		r.quietSince = now
	}
	limit := r.ceiling
	if r.decoderCap > 0 {
		limit = min(limit, r.decoderCap)
	}
	if r.cur <= 0 || r.cur >= limit || now.Sub(r.quietSince) < r.interval() ||
		(!r.lastChange.IsZero() && now.Sub(r.lastChange) < r.changeGap()) {
		return r.cur, r.cur, false
	}
	from = r.cur
	r.cur = min(max(r.cur*rateRaisePct/100, r.cur+1), limit)
	r.lastChange = now
	return from, r.cur, true
}

// kbps returns the current target and the ceiling (0, 0 before the first
// generation).
func (r *rateController) kbps() (cur, ceiling int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur, r.ceiling
}

// decoderLimit returns the cap on raises after a decoder flush (0: none).
func (r *rateController) decoderLimit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.decoderCap
}
