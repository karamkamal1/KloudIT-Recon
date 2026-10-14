package host

import (
	"slices"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The rate controller's view of the client's receive reports (bitrate.go).
//
// A frame's one-way delay as the client measures it (last byte received minus
// the frame's encodeDoneUs) includes the host's own sending: the media
// congestion controller paces at 1.2 x the target (a frame's burst faster,
// as far as the burst credit reaches), so a frame larger than the credit (a
// key frame, the first frames of a new FFmpeg generation) takes several
// frame intervals to go out and delays the frames behind it, for up to a
// second, with no network queue at all. sendTrack therefore keeps, per frame
// sent, the share of its sending time the pacer explains: a fluid model of
// the pacer (pacerModel: a backlog fed with each frame at its encodeDone
// time, drained at the video's share of the burst rate while the credit
// lasts and of the pacing rate after, less its burst allowance), but at most
// the frame's measured encodeDone -> written time (frameSender's Write
// returns when all but the frame's last packet has been paced out). The rate
// controller then sees each report's delay minus that share: a queue
// anywhere else, including the host's own queue while the congestion window
// blocks sending (which is the network's doing), stays in the signal; the
// controller also asks the host's own measure of the path's queue
// (feedback.ackQD) whether it is the path's.

// sendTrackLen is how many sent frames sendTrack keeps (8 s at 60 fps).
const sendTrackLen = 512

// paceOverhead is the share of the video pacing rate taken by QUIC packet
// and frame headers.
const paceOverhead = 0.04

// minPaceBurst is the pacer's least burst allowance (cc.Media: 10 packets);
// its burst allowance is 2 ms at the burst rate, at least that.
const minPaceBurst = 12000 // bytes

type sentFrame struct {
	gen          uint8
	seq          uint32
	encodeDoneUs uint64
	sentUs       uint64 // host clock when its write returned
	bytes        int
	comp         uint64 // µs of its encodeDone -> written time the pacer explains
}

// sendTrack keeps the frames the session sent, in send order, and which of
// them feedback has covered (a report naming a later frame, an ack).
type sendTrack struct {
	mu      sync.Mutex
	ring    [sendTrackLen]sentFrame
	n       uint64 // frames recorded
	covered uint64 // frames [0, covered) are covered
	// The pacer model at lastUs.
	model  pacerModel
	lastUs uint64
}

// sent records a frame whose write returned at doneUs. pace is the pacing
// available to video (Session.videoPace; rate 0: the connection does not
// pace by the target, nothing is attributed to pacing).
func (t *sendTrack) sent(gen uint8, seq uint32, encodeDoneUs, doneUs uint64, bytes int, pace videoPace) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := sentFrame{gen: gen, seq: seq, encodeDoneUs: encodeDoneUs, sentUs: doneUs, bytes: bytes}
	if pace.rate > 0 && encodeDoneUs > 0 && doneUs >= encodeDoneUs {
		if t.lastUs != 0 && encodeDoneUs > t.lastUs {
			// At the pace the pacer had until now: a target raised since
			// (this frame's write returns at the new rate) did not drain
			// what came before it any faster.
			t.model.advance(float64(encodeDoneUs-t.lastUs) / 1e6)
		}
		t.lastUs = max(t.lastUs, encodeDoneUs)
		t.model.pace(pace, paceOverhead)
		t.model.backlog += float64(bytes)
		burst := max(t.model.burst*0.002, minPaceBurst)
		model := uint64(t.model.drain(t.model.backlog-burst) * 1e6)
		f.comp = min(model, doneUs-encodeDoneUs)
	}
	t.ring[t.n%sendTrackLen] = f
	t.n++
	if t.n-t.covered > sendTrackLen {
		t.covered = t.n - sendTrackLen
	}
}

// find returns the index of frame (gen, seq) among those not yet covered
// (from start), searching from the newest.
func (t *sendTrack) find(gen uint8, seq uint32, from uint64) (uint64, bool) {
	for i := t.n; i > from; i-- {
		if f := t.ring[(i-1)%sendTrackLen]; f.gen == gen && f.seq == seq {
			return i - 1, true
		}
	}
	return 0, false
}

// cover marks the frames sent up to and including (gen, seq) as covered and
// returns the median pacer share of those newly covered and how many there
// were (0 when the frame is unknown or covered already).
func (t *sendTrack) cover(gen uint8, seq uint32) (time.Duration, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.find(gen, seq, t.covered)
	if !ok {
		return 0, 0
	}
	comps := make([]uint64, 0, k+1-t.covered)
	for i := t.covered; i <= k; i++ {
		comps = append(comps, t.ring[i%sendTrackLen].comp)
	}
	t.covered = k + 1
	slices.Sort(comps)
	return time.Duration(comps[(len(comps)-1)/2]) * time.Microsecond, len(comps)
}

// pending returns, at host clock nowUs, how long ago the oldest frame no
// feedback has covered was encoded, less its pacer share (ok false: none).
func (t *sendTrack) pending(nowUs uint64) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.covered >= t.n {
		return 0, false
	}
	f := t.ring[t.covered%sendTrackLen]
	if f.encodeDoneUs == 0 || nowUs < f.encodeDoneUs+f.comp {
		return 0, false
	}
	return time.Duration(nowUs-f.encodeDoneUs-f.comp) * time.Microsecond, true
}

// compOf returns the pacer share of a recent frame (ok false: unknown),
// covering nothing: the frame whose one-way delay a report names
// (proto.RateReportOWDFrame).
func (t *sendTrack) compOf(gen uint8, seq uint32) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	from := uint64(0)
	if t.n > sendTrackLen {
		from = t.n - sendTrackLen
	}
	k, ok := t.find(gen, seq, from)
	if !ok {
		return 0, false
	}
	return time.Duration(t.ring[k%sendTrackLen].comp) * time.Microsecond, true
}

// frame returns a recent frame's record (for acks), covering it and the
// frames before it.
func (t *sendTrack) frame(gen uint8, seq uint32) (sentFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	from := uint64(0)
	if t.n > sendTrackLen {
		from = t.n - sendTrackLen
	}
	k, ok := t.find(gen, seq, from)
	if !ok {
		return sentFrame{}, false
	}
	t.covered = max(t.covered, k+1)
	return t.ring[k%sendTrackLen], true
}

// uncoveredSince returns when the oldest frame no feedback has covered was
// sent (host clock µs; ok false: every frame is covered).
func (t *sendTrack) uncoveredSince() (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.covered >= t.n {
		return 0, false
	}
	return t.ring[t.covered%sendTrackLen].sentUs, true
}

// rateFeedback turns the client's reports (or, from clients without them,
// its frame acks) into the rate controller's feedback: differences of the
// cumulative counters, the delay less the pacer's share, the packet losses of
// the media congestion controller.
type rateFeedback struct {
	mu       sync.Mutex
	last     proto.RateReport
	have     bool      // a report arrived
	lastAt   time.Time // when the last report arrived
	acks     []ackSample
	acksFrom time.Time
	// The media congestion controller's counters at the last report (lost,
	// lost + acknowledged packets, acknowledged bytes, the time its window
	// held the sender back); reset when a path migration replaced it.
	ccLost, ccTotal, ccAcked uint64
	ccLimited, ccHeld        time.Duration
	ccHave                   bool
	// Raw one-way delay p50s and maxima over the stats period, on the
	// client's current clock sync, and the last report's clock shift (its
	// current sync less its first), for the log.
	owds   []time.Duration
	owdMax time.Duration
	shift  time.Duration
	// The host's own measure of the path's queue each report came with
	// (ccCounters.queue), for the log.
	queues []time.Duration
}

type ackSample struct {
	owd, comp time.Duration
	bytes     int
}

// reportsActive reports whether the client sent a rate report recently: its
// acks are then not the controller's feedback.
func (f *rateFeedback) reportsActive(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have && now.Sub(f.lastAt) < time.Second
}

// ccCounters are the media congestion controller's cumulative counters, and
// its queueing delay since the last report (queueOK false: none measured).
type ccCounters struct {
	ok                 bool // the connection has the media congestion controller
	lost, total, acked uint64
	limited            time.Duration // the time its congestion window held the sender back (WindowLimited)
	nonVideoKbps       int           // audio and overhead in the acknowledged bytes
	queue              time.Duration // cc.Media.QueueDelay
	queueOK            bool
	held               time.Duration // the video window's holds in all (videoWindow.holdTime)
}

// fromReport converts a report that arrived at now; comp is the pacer share
// of the frame whose one-way delay is the report's p50 where the report names
// it (proto.RateReportOWDFrame), else the median of the frames it covers; cc
// the media congestion controller's counters (without them the client's own
// loss count is used). The delays are put on the scale of the client's first
// clock sync (less ClockShiftUs): a better sync sample later moves the
// client's offset, which is no change of the path's delay. That scale keeps
// the first sync's error, though (the second hardware run's
// report_owd_p50_ms of ~110 ms where the client's stages had ~20: a first
// ping answered late at the session's start); fb.shift takes a delay back to
// the client's current, best sync, which the stats log shows. ok is false for a
// report not newer than the last one (its client clock): a reordered or
// duplicated datagram, whose counters would make wrapped differences; the
// next report's differences cover its frames.
func (f *rateFeedback) fromReport(r proto.RateReport, now time.Time, comp time.Duration, cc ccCounters) (feedback, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb := feedback{at: now, decodeQ: int(r.DecodeQueue)}
	if f.have {
		p := f.last
		if int32(r.TimeMs-p.TimeMs) <= 0 {
			return feedback{}, false
		}
		fb.frames = int(r.Frames - p.Frames)
		fb.bytes = int64(r.Bytes - p.Bytes)
		fb.interval = time.Duration(r.TimeMs-p.TimeMs) * time.Millisecond
		if !cc.ok {
			lost := int64(r.Lost - p.Lost)
			fb.lost, fb.total = lost, lost+int64(r.Audio-p.Audio)+int64(fb.frames)
		}
	}
	f.last, f.have, f.lastAt = r, true, now
	f.ccDelta(&fb, cc)
	if r.Flags&proto.RateReportOWD != 0 {
		var shift time.Duration
		if r.Flags&proto.RateReportOWDFrame != 0 {
			shift = time.Duration(r.ClockShiftUs) * time.Microsecond
		}
		fb.owdValid, fb.shift = true, shift
		fb.owd = time.Duration(r.OWDP50Us)*time.Microsecond - shift
		fb.owdMax = time.Duration(r.OWDMaxUs)*time.Microsecond - shift
		fb.qd = fb.owd - comp
		f.note(fb.owd+shift, fb.owdMax+shift, shift)
	}
	return fb, true
}

// ccDelta puts the media congestion controller's losses and acknowledged
// bytes since the last call into fb. Called with f.mu held.
func (f *rateFeedback) ccDelta(fb *feedback, cc ccCounters) {
	if !cc.ok {
		return
	}
	if !f.ccHave || cc.lost < f.ccLost || cc.total < f.ccTotal || cc.acked < f.ccAcked || cc.limited < f.ccLimited || cc.held < f.ccHeld {
		f.ccLost, f.ccTotal, f.ccAcked, f.ccLimited, f.ccHeld, f.ccHave = cc.lost, cc.total, cc.acked, cc.limited, cc.held, true
		return
	}
	fb.ackQD, fb.ackQDValid = cc.queue, cc.queueOK
	if cc.queueOK && len(f.queues) < 4096 {
		f.queues = append(f.queues, cc.queue)
	}
	fb.lost, fb.total = int64(cc.lost-f.ccLost), int64(cc.total-f.ccTotal)
	fb.ackedValid, fb.acked, fb.nonVideoKbps = true, int64(cc.acked-f.ccAcked), cc.nonVideoKbps
	fb.limited, fb.held = cc.limited-f.ccLimited, cc.held-f.ccHeld
	f.ccLost, f.ccTotal, f.ccAcked, f.ccLimited, f.ccHeld = cc.lost, cc.total, cc.acked, cc.limited, cc.held
}

// ack records a client's frame ack (clients without rate reports).
func (f *rateFeedback) ack(owd, comp time.Duration, bytes int, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acks) == 0 && f.acksFrom.IsZero() {
		f.acksFrom = now
	}
	if len(f.acks) < 4096 {
		f.acks = append(f.acks, ackSample{owd, comp, bytes})
	}
}

// fromAcks turns the acks since the last call into one report (ok false:
// none). The losses are the media congestion controller's.
func (f *rateFeedback) fromAcks(now time.Time, cc ccCounters) (feedback, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acks) == 0 {
		f.acksFrom = now
		return feedback{}, false
	}
	fb := feedback{at: now, frames: len(f.acks), interval: now.Sub(f.acksFrom), owdValid: true}
	raw := make([]time.Duration, 0, len(f.acks))
	qd := make([]time.Duration, 0, len(f.acks))
	for _, a := range f.acks {
		fb.bytes += int64(a.bytes)
		raw = append(raw, a.owd)
		qd = append(qd, a.owd-a.comp)
	}
	slices.Sort(raw)
	slices.Sort(qd)
	fb.owd, fb.owdMax, fb.qd = raw[(len(raw)-1)/2], raw[len(raw)-1], qd[(len(qd)-1)/2]
	f.note(fb.owd, fb.owdMax, 0)
	f.ccDelta(&fb, cc)
	f.acks, f.acksFrom = f.acks[:0], now
	return fb, true
}

// note keeps a report's delays (on the client's current clock sync) and
// clock shift for the stats log. Called with f.mu held.
func (f *rateFeedback) note(owd, owdMax, shift time.Duration) {
	if len(f.owds) < 4096 {
		f.owds = append(f.owds, owd)
	}
	f.owdMax = max(f.owdMax, owdMax)
	f.shift = shift
}

// feedbackStats is a stats period's feedback: the reports' one-way delays
// (p50 and p95 of their p50s, the largest maximum; n = 0: no report had
// delays) on the client's current clock sync and the last report's clock
// shift, and the host's own measure of the path's queue (p50 and p95 of the
// reports'; queues = 0: none).
type feedbackStats struct {
	n                    int
	p50, p95, max, shift time.Duration
	queues               int
	queueP50, queueP95   time.Duration
}

// stats returns and clears the stats period's feedback.
func (f *rateFeedback) stats() feedbackStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := feedbackStats{n: len(f.owds), max: f.owdMax, shift: f.shift, queues: len(f.queues)}
	if n := st.n; n > 0 {
		slices.Sort(f.owds)
		st.p50, st.p95 = f.owds[min(n-1, n/2)], f.owds[min(n-1, n*95/100)]
	}
	if n := st.queues; n > 0 {
		slices.Sort(f.queues)
		st.queueP50, st.queueP95 = f.queues[min(n-1, n/2)], f.queues[min(n-1, n*95/100)]
	}
	f.owds, f.owdMax, f.queues = f.owds[:0], 0, f.queues[:0]
	return st
}
