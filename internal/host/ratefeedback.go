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
// congestion controller paces at 1.2 x the target, so a frame larger than
// the average (a key frame, the first frames of a new FFmpeg generation)
// takes several frame intervals to go out and delays the frames behind it,
// for up to a second, with no network queue at all. sendTrack therefore
// keeps, per frame sent, the share of its sending time the pacer explains: a
// fluid model of the pacer (a backlog fed with each frame at its encodeDone
// time and drained at the video's share of the pacing rate, less its burst
// allowance), but at most the frame's measured encodeDone -> written time
// (frameSender's Write returns when all but the frame's last packet has been
// paced out). The rate controller then sees each report's delay minus that
// share: a queue anywhere else, including the host's own queue while the
// congestion window blocks sending (which is the network's doing), stays in
// the signal.

// sendTrackLen is how many sent frames sendTrack keeps (8 s at 60 fps).
const sendTrackLen = 512

// paceOverhead is the share of the video pacing rate taken by QUIC packet
// and frame headers.
const paceOverhead = 0.04

// minPaceBurst is the pacer's least burst allowance (cc.Media: 10 packets).
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
	backlog float64
	lastUs  uint64
}

// sent records a frame whose write returned at doneUs. paceBps is the pacing
// rate available to video (bit/s; 0: the connection does not pace by the
// target, nothing is attributed to pacing).
func (t *sendTrack) sent(gen uint8, seq uint32, encodeDoneUs, doneUs uint64, bytes int, paceBps float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := sentFrame{gen: gen, seq: seq, encodeDoneUs: encodeDoneUs, sentUs: doneUs, bytes: bytes}
	if paceBps > 0 && encodeDoneUs > 0 && doneUs >= encodeDoneUs {
		rate := paceBps * (1 - paceOverhead) / 8 // bytes/s
		if t.lastUs != 0 && encodeDoneUs > t.lastUs {
			t.backlog = max(0, t.backlog-rate*float64(encodeDoneUs-t.lastUs)/1e6)
		}
		t.lastUs = max(t.lastUs, encodeDoneUs)
		t.backlog += float64(bytes)
		burst := max(rate*0.002, minPaceBurst)
		model := uint64(max(0, t.backlog-burst) / rate * 1e6)
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
	// lost + acknowledged packets, acknowledged bytes); reset when a path
	// migration replaced it.
	ccLost, ccTotal, ccAcked uint64
	ccHave                   bool
	// Raw one-way delay p50s and maxima over the stats period, for the log.
	owds   []time.Duration
	owdMax time.Duration
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

// ccCounters are the media congestion controller's cumulative counters.
type ccCounters struct {
	ok                 bool // the connection has the media congestion controller
	lost, total, acked uint64
	nonVideoKbps       int // audio and overhead in the acknowledged bytes
}

// fromReport converts a report that arrived at now; comp is the pacer share
// of the frames it covers, cc the media congestion controller's counters
// (without them the client's own loss count is used).
func (f *rateFeedback) fromReport(r proto.RateReport, now time.Time, comp time.Duration, cc ccCounters) feedback {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb := feedback{at: now, decodeQ: int(r.DecodeQueue)}
	if f.have {
		p := f.last
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
		fb.owdValid = true
		fb.owd = time.Duration(r.OWDP50Us) * time.Microsecond
		fb.owdMax = time.Duration(r.OWDMaxUs) * time.Microsecond
		fb.qd = fb.owd - comp
		f.note(fb.owd, fb.owdMax)
	}
	return fb
}

// ccDelta puts the media congestion controller's losses and acknowledged
// bytes since the last call into fb. Called with f.mu held.
func (f *rateFeedback) ccDelta(fb *feedback, cc ccCounters) {
	if !cc.ok {
		return
	}
	if !f.ccHave || cc.lost < f.ccLost || cc.total < f.ccTotal || cc.acked < f.ccAcked {
		f.ccLost, f.ccTotal, f.ccAcked, f.ccHave = cc.lost, cc.total, cc.acked, true
		return
	}
	fb.lost, fb.total = int64(cc.lost-f.ccLost), int64(cc.total-f.ccTotal)
	fb.ackedValid, fb.acked, fb.nonVideoKbps = true, int64(cc.acked-f.ccAcked), cc.nonVideoKbps
	f.ccLost, f.ccTotal, f.ccAcked = cc.lost, cc.total, cc.acked
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
	f.note(fb.owd, fb.owdMax)
	f.ccDelta(&fb, cc)
	f.acks, f.acksFrom = f.acks[:0], now
	return fb, true
}

// note keeps a report's delays for the stats log. Called with f.mu held.
func (f *rateFeedback) note(owd, owdMax time.Duration) {
	if len(f.owds) < 4096 {
		f.owds = append(f.owds, owd)
	}
	f.owdMax = max(f.owdMax, owdMax)
}

// stats returns and clears the stats period's one-way delay: p50 and p95 of
// the reports' p50s and the largest maximum (n = 0: no report had delays).
func (f *rateFeedback) stats() (p50, p95, maxOWD time.Duration, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n = len(f.owds)
	if n > 0 {
		slices.Sort(f.owds)
		p50, p95 = f.owds[min(n-1, n/2)], f.owds[min(n-1, n*95/100)]
	}
	maxOWD = f.owdMax
	f.owds, f.owdMax = f.owds[:0], 0
	return p50, p95, maxOWD, n
}
