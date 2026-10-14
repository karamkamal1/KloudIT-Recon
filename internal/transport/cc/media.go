// Package cc holds congestion controllers for quic-go's pluggable congestion
// control (quic.Config.Congestion; see third_party/README.md).
package cc

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/congestion"
)

const (
	// DefaultTargetBitrate is the target until the application sets one.
	DefaultTargetBitrate = 20_000_000 // bit/s
	// DefaultFrameInterval is the frame interval until the application sets one.
	DefaultFrameInterval = time.Second / 60
	// PacingGain is the pacing rate relative to the target bitrate: headroom for
	// frame-size variance, retransmissions, audio and input. It bounds what
	// the controller sends over time; a frame goes out faster, at the burst
	// rate (below), as far as the burst credit reaches.
	PacingGain = 1.2

	minTargetBitrate = 100_000 // bit/s

	// Windows in packets of the current maximum datagram size.
	minWindowPackets   = 2     // after persistent congestion (RFC 9002 kMinimumWindow)
	floorWindowPackets = 32    // normal operation: never below quic-go's initial window
	maxWindowPackets   = 10000 // quic-go's MaxCongestionWindowPackets

	// Pacer, as quic-go's: bursts of up to 10 packets or 2 ms of data, at
	// least 1 ms between bursts.
	maxBurstPackets = 10
	burstDuration   = 2 * time.Millisecond
	minPacingDelay  = time.Millisecond

	// Frame bursts. Paced at PacingGain x the target, a frame of the target's
	// average size takes 1/1.2 of a frame interval to send (11.8 ms of
	// "transfer" p50 in game in the second hardware run, on a LAN that
	// carries it in about one): latency a path that is not the bottleneck
	// does not need. The pacer therefore sends at the burst rate while the
	// burst credit lasts: the bytes the pacing rate earned while the sender
	// had nothing to send, at most burstFrames frame intervals' worth, so
	// over any span the controller still sends no more than the pacing rate
	// plus that credit. The burst rate follows the path: burstProbe x the
	// highest delivery rate its acknowledgements measured over burstWindow
	// (trainSample: the spacing of a train's ACKs against its sending),
	// burstStartGain x the pacing rate before the first measure (WebRTC's
	// pacer runs at 2.5 x the target), never under the pacing rate nor over
	// maxBurstRate. A path slower than the bursts shows it in the ACKs'
	// spacing (the burst rate comes down to it), and one that is not
	// burstMinGain times faster than the pacing rate is paced at the
	// pacing rate but for a probe train a second; a path with a queue that
	// stands (burstQueueMax) is paced at the pacing rate; a train that lost
	// burstLossShare of its packets (at least burstLossMin), a queue it
	// overflowed, turns bursts off for burstHold, twice as long each time
	// it happens again soon after (up to burstHoldMax), as does the brake
	// after persistent congestion: the pacer then runs at the pacing rate,
	// as before bursts.
	burstStartGain = 2.0
	burstProbe     = 1.25
	maxBurstRate   = 1_000_000_000 / 8 // bytes/s
	burstFrames    = 1.5
	burstWindow    = 2 * time.Second
	burstLossShare = 0.05
	burstLossMin   = 3
	burstHold      = 5 * time.Second
	burstHoldMax   = 80 * time.Second
	// burstMinGain: bursts only where the path delivered at least this
	// many times the pacing rate in the last burstWindow. A path not much
	// faster than the pacing rate saves a millisecond or two per frame
	// with them but queues a share of each frame at its bottleneck (a
	// 20-25 Mbit/s path at a 16-20 Mbit/s target: one-way delay p95 +5-8
	// ms in the relay simulation). There a train every burstProbeEvery
	// still goes at burstStartGain x the pacing rate, to find a path that
	// got faster.
	burstMinGain    = 2.0
	burstProbeEvery = time.Second
	// burstQueueMax: a queue on the path that every packet of the last
	// burstQueueSlot to 2 x burstQueueSlot met (the smallest round trip of
	// that time over the recent min RTT: standingQueue) longer than this
	// holds bursts, while it lasts: the path is the bottleneck now (a
	// capacity drop, a link that stalls), and the frames are paced at the
	// pacing rate, as before bursts. The first packets of each burst meet
	// no queue of the bursts' own making.
	burstQueueMax  = 5 * time.Millisecond
	burstQueueSlot = 50 * time.Millisecond
	// trainGap: ack-eliciting packets sent less than this apart are one
	// train (a frame's burst, or its pacing); burstSamplePackets: a train's
	// delivery rate needs this many packets' worth acknowledged after its
	// first ACK (receivers acknowledge every 2nd to 10th packet).
	trainGap           = 2 * time.Millisecond
	burstSamplePackets = 8
	trainRing          = 16 // trains whose losses are counted

	// RFC 9002 kPersistentCongestionThreshold.
	persistentCongestionThreshold = 3
	// Send times kept for persistent-congestion detection, by packet number.
	// With a window of more packets the oldest lost packets of an outage are
	// not found, which only makes the brake later.
	sentRingSize = 4096

	// RecentMinRTT is the smallest RTT sample of the last recentRTTWindow,
	// kept in rttBuckets parts (so it reaches back 1.5 to 2 s).
	recentRTTWindow = 2 * time.Second
	// baseRTTWindow: the queue the path's packets meet (QueueDelay,
	// standingQueue) is measured over the smallest RTT sample of the last
	// 7.5 to 10 s: a queue that stands for a few seconds (a capacity drop
	// the rate controller is answering) is not the path's base, as it is
	// RecentMinRTT's after 2 s; a round trip that grows for good (a route
	// change) is after 10 s.
	baseRTTWindow = 10 * time.Second
	rttBuckets    = 4
	// DeliveredAt's record of when done grew: ackLogSize entries, those
	// less than ackLogResolution apart merged (at least 256 ms back).
	ackLogSize       = 1024
	ackLogResolution = 250 * time.Microsecond
)

// Media is a rate-following congestion controller for real-time video. Unlike
// NewReno it does not probe for bandwidth: it paces at PacingGain × the
// application's target bitrate (each frame's burst faster, as far as the
// burst credit reaches: see burstStartGain) and allows a window of
// pacing rate × (min RTT + 2 frame intervals), so the queue it can build is
// bounded by two frames. A loss does NOT shrink the window; losses are counted
// (Stats) for the application's rate controller, which lowers the target.
// The only brake of its own is for persistent congestion (RFC 9002 section
// 7.6.2: packets lost whose send times span more than 3 × PTO, with no packet
// sent in between acknowledged) or a retransmission timeout: the window
// collapses to two packets and grows back by the acknowledged bytes (slow
// start) to the normal window.
//
// The quic-go connection calls the congestion.CongestionControl methods from its
// run loop. SetTarget, SetTargetBitrate, SetFrameInterval, TargetBitrate and
// Stats may be called from any goroutine.
type Media struct {
	rtt congestion.RTTStats

	targetBitrate   atomic.Int64 // bit/s
	frameInterval   atomic.Int64 // ns
	maxDatagramSize atomic.Int64 // bytes
	brakeWindow     atomic.Int64 // bytes; > 0 while recovering from persistent congestion

	// run loop only
	budget   congestion.ByteCount // pacing budget left after the last packet (at the burst rate)
	credit   congestion.ByteCount // burst credit left after the last packet (at the pacing rate)
	lastSent congestion.Time
	// Bursts (run loop only, but burstRate): the current train and when
	// its last ack-eliciting packet went out, the recent trains' packets
	// sent and lost, the first ACK of the newest acknowledged train
	// (anchor), bytes acknowledged in all (delivered), the highest delivery
	// rate of each part of burstWindow (bytes/s), the bursts' hold after an
	// overflow and its length.
	train         uint32
	trainAt       congestion.Time
	trains        [trainRing]trainStat
	anchor        trainAnchor
	delivered     uint64
	bw            [rttBuckets]bwBucket
	holdUntil     congestion.Time
	holdLen       time.Duration
	lastBackoff   congestion.Time
	probing       bool            // the current train probes (burstMinGain)
	probeAt       congestion.Time // the last probe train began
	qSlot         [2]rttBucket    // the smallest RTT sample of the current and the last burstQueueSlot
	standing      atomic.Int64    // ns: standingQueue as of the last ACK (StandingQueue)
	burstRate     atomic.Int64    // bytes/s the pacer bursts at (0: not yet computed)
	burstEst      atomic.Int64    // bytes/s: the delivery rate the burst rate follows (0: none measured)
	burstBackoffs atomic.Uint64
	// persistent congestion
	sent         [sentRingSize]sentRecord // ack-eliciting packets
	firstAck     congestion.Time          // first RTT sample
	largestAcked congestion.PacketNumber
	lossRunPN    congestion.PacketNumber // first packet lost since largestAcked
	lossRunStart congestion.Time         // its send time; zero: none

	ackedPackets, ackedBytes atomic.Uint64
	lostPackets, lostBytes   atomic.Uint64
	ecnMarks, collapses      atomic.Uint64

	// Delivery positions (Delivery): ack-eliciting bytes sent, and of those
	// the bytes acknowledged or declared lost, cumulative; progress is
	// signalled whenever done grows.
	sentPos, donePos atomic.Uint64
	progress         chan struct{}
	// ackLog: when done reached which position (DeliveredAt), oldest
	// overwritten; ackN entries written so far.
	logMu  sync.Mutex
	ackLog [ackLogSize]ackPoint
	ackN   int

	// RecentMinRTT: the minimum RTT sample of each part of the window (run
	// loop only) and of the whole window (ns; 0: no sample yet).
	rttMin       [rttBuckets]rttBucket
	recentMinRTT atomic.Int64
	rttBase      [rttBuckets]rttBucket // the same over baseRTTWindow
	baseMinRTT   atomic.Int64
	// qMin: the smallest RTT sample since the last QueueDelay (ns; 0: none).
	qMin atomic.Int64

	// WindowLimited: the time the window refused the sender before the
	// current refusal (limitedTotal) and when that began (limitedSince; zero:
	// the window allows); limited is the run loop's own copy of the state.
	limMu        sync.Mutex
	limitedTotal time.Duration
	limitedSince congestion.Time
	limited      bool
	now          func() congestion.Time
	// epoch: the first time the run loop handed the controller (part).
	epoch congestion.Time
}

type sentRecord struct {
	pn    congestion.PacketNumber
	t     congestion.Time
	train uint32
}

// trainStat counts a train's ack-eliciting packets and those lost.
type trainStat struct {
	id         uint32
	sent, lost int
	backedOff  bool
}

// trainAnchor is the first ACK of a train: when it arrived, the newest
// send time among the packets it acknowledged, and delivered after it.
type trainAnchor struct {
	train     uint32
	at, sent  congestion.Time
	delivered uint64
	ok        bool
}

// bwBucket is the highest delivery rate (bytes/s) measured in the n-th part
// of burstWindow/rttBuckets since the controller's first timestamp (part).
type bwBucket struct {
	n   int64
	max float64
}

type ackPoint struct {
	done uint64
	at   congestion.Time
}

// rttBucket is the smallest RTT sample of the n-th part of
// recentRTTWindow/rttBuckets since the controller's first timestamp (part)
// (0: none).
type rttBucket struct {
	n   int64
	min time.Duration
}

var _ congestion.CongestionControl = (*Media)(nil)

// NewMedia returns a media controller for one path of a connection.
func NewMedia(rtt congestion.RTTStats, initialMaxDatagramSize congestion.ByteCount) *Media {
	m := &Media{rtt: rtt, largestAcked: -1, progress: make(chan struct{}, 1), now: congestion.Now}
	m.targetBitrate.Store(DefaultTargetBitrate)
	m.frameInterval.Store(int64(DefaultFrameInterval))
	m.maxDatagramSize.Store(int64(initialMaxDatagramSize))
	return m
}

// MediaFactory is a quic.Config.Congestion factory that creates Media controllers.
func MediaFactory(rtt congestion.RTTStats, initialMaxDatagramSize congestion.ByteCount) congestion.CongestionControl {
	return NewMedia(rtt, initialMaxDatagramSize)
}

// SetClock replaces the controller's clock (congestion.Now) for the
// timestamps it takes itself (WindowLimited, a loss's DeliveredAt): a
// simulation drives it with its own time. Call it before the controller is
// used.
func (m *Media) SetClock(now func() congestion.Time) { m.now = now }

// SetTargetBitrate sets the application's current target bitrate in bit/s.
func (m *Media) SetTargetBitrate(bps int64) {
	m.targetBitrate.Store(max(bps, minTargetBitrate))
}

// SetFrameInterval sets the video frame interval (0: DefaultFrameInterval).
func (m *Media) SetFrameInterval(d time.Duration) {
	if d <= 0 {
		d = DefaultFrameInterval
	}
	m.frameInterval.Store(int64(d))
}

// SetTarget sets the target bitrate (bit/s) and the frame interval.
func (m *Media) SetTarget(bps int64, frameInterval time.Duration) {
	m.SetTargetBitrate(bps)
	m.SetFrameInterval(frameInterval)
}

// TargetBitrate returns the current target bitrate in bit/s.
func (m *Media) TargetBitrate() int64 { return m.targetBitrate.Load() }

// MediaStats is a snapshot of a Media controller.
type MediaStats struct {
	TargetBitrate int64 // bit/s
	PacingRate    int64 // bit/s
	BurstRate     int64 // bit/s: a frame's burst (Pacing)
	BurstEstimate int64 // bit/s: the delivery rate the bursts measured (0: none in burstWindow)
	BurstBackoffs uint64
	Window        int64 // bytes
	AckedPackets  uint64
	AckedBytes    uint64
	LostPackets   uint64
	LostBytes     uint64
	ECNMarks      uint64 // ECN congestion-experienced reports
	Collapses     uint64 // persistent congestion / retransmission timeouts
}

// Stats returns counters for the application's rate controller.
func (m *Media) Stats() MediaStats {
	p := m.Pacing()
	return MediaStats{
		TargetBitrate: m.targetBitrate.Load(),
		PacingRate:    p.Rate,
		BurstRate:     p.Burst,
		BurstEstimate: m.burstEst.Load() * 8,
		BurstBackoffs: m.burstBackoffs.Load(),
		Window:        int64(m.GetCongestionWindow()),
		AckedPackets:  m.ackedPackets.Load(),
		AckedBytes:    m.ackedBytes.Load(),
		LostPackets:   m.lostPackets.Load(),
		LostBytes:     m.lostBytes.Load(),
		ECNMarks:      m.ecnMarks.Load(),
		Collapses:     m.collapses.Load(),
	}
}

// Delivery returns the connection's delivery positions: sent, the bytes of
// the ack-eliciting packets sent so far, and done, how many of them have left
// the network (acknowledged, or declared lost and so queued for a
// retransmission that counts as sent again). Packets leave in about the order
// they were sent, so data that was handed to the transport when sent read s
// has left once done >= s. Both are cumulative over this controller (a path
// migration starts a new one).
func (m *Media) Delivery() (sent, done uint64) {
	done = m.donePos.Load()
	return m.sentPos.Load(), done
}

// Progress is signalled (capacity 1, never blocks the connection) whenever
// done grows: an ACK or a loss.
func (m *Media) Progress() <-chan struct{} { return m.progress }

// DeliveredAt returns when done (Delivery) first reached pos: the ACK (or
// loss) that took the byte at sent position pos out of the network, within
// ackLogResolution. false: done has not reached pos yet, or did so before
// the record reaches back.
func (m *Media) DeliveredAt(pos uint64) (time.Time, bool) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if m.ackN == 0 || m.ackLog[(m.ackN-1)%ackLogSize].done < pos {
		return time.Time{}, false
	}
	lo := max(0, m.ackN-ackLogSize)
	i := lo + sort.Search(m.ackN-lo, func(i int) bool { return m.ackLog[(lo+i)%ackLogSize].done >= pos })
	if i == lo && lo > 0 {
		return time.Time{}, false // reached before the oldest entry kept
	}
	return m.ackLog[i%ackLogSize].at.ToTime(), true
}

// RecentMinRTT is the smallest round trip a packet of this path took in the
// last 1.5 to 2 s (its ACK's arrival less its sending; 0: none acknowledged
// yet). Unlike quic-go's min RTT, which is the connection's lifetime
// minimum, it follows a path whose round trip grows (a route change, a
// relay fallback) within that time.
func (m *Media) RecentMinRTT() time.Duration { return time.Duration(m.recentMinRTT.Load()) }

// PacingRate is the rate the controller paces at over time, in bit/s
// (bursts aside: Pacing).
func (m *Media) PacingRate() int64 { return int64(m.pacingRate() * 8) }

// pacingRate is in bytes/s.
func (m *Media) pacingRate() float64 {
	return float64(m.targetBitrate.Load()) * PacingGain / 8
}

// Pace is how the controller paces: Rate over time and Burst while the burst
// credit lasts (bit/s), and the credit's most (bytes): an idle sender's
// next Credit bytes go out at Burst, what follows at Rate.
type Pace struct {
	Rate, Burst int64
	Credit      int64
}

// Pacing returns the controller's pacing (any goroutine).
func (m *Media) Pacing() Pace {
	b := m.pacingRate()
	burst := float64(m.burstRate.Load())
	if burst <= 0 {
		burst = m.burstFor(b, 0, false, false) // nothing sent yet
	}
	return Pace{Rate: int64(b * 8), Burst: int64(max(burst, b) * 8), Credit: int64(m.creditMax(b))}
}

// burstFor is the burst rate (bytes/s) at pacing rate b with delivery-rate
// estimate est (0: none), or b while bursts are held or the path is not
// burstMinGain times faster than b (but a probe train: probe).
func (m *Media) burstFor(b, est float64, held, probe bool) float64 {
	if held {
		return b
	}
	p := b * burstStartGain // before the first measure, and a probe
	if est > 0 {
		switch {
		case est >= burstMinGain*b:
			p = est * burstProbe
		case !probe:
			return b
		}
	}
	return min(max(p, b), max(b, maxBurstRate))
}

// burstAt computes the burst rate at now (run loop only) and publishes it.
func (m *Media) burstAt(now congestion.Time) float64 {
	b := m.pacingRate()
	est := m.bwEstimate(now)
	m.burstEst.Store(int64(est))
	p := m.burstFor(b, est, m.brakeWindow.Load() > 0 || now.Before(m.holdUntil) || m.standingQueue(now) > burstQueueMax, m.probing)
	m.burstRate.Store(int64(p))
	return p
}

// StandingQueue is the queue every packet acknowledged in the last 50 to
// 100 ms met on the path (standingQueue, as of the last ACK; any
// goroutine).
func (m *Media) StandingQueue() time.Duration { return time.Duration(m.standing.Load()) }

// standingQueue is how much longer than the path's base round trip
// (baseRTTWindow) the quickest round trip of the packets acknowledged in the
// current and the last burstQueueSlot took (0: none acknowledged). Run loop
// only.
func (m *Media) standingQueue(now congestion.Time) time.Duration {
	n := m.part(now, rttBuckets*burstQueueSlot)
	var low time.Duration
	for _, b := range m.qSlot {
		if b.min > 0 && b.n >= n-1 && (low == 0 || b.min < low) {
			low = b.min
		}
	}
	if low == 0 {
		return 0
	}
	return max(0, low-time.Duration(m.baseMinRTT.Load()))
}

// creditMax is the most burst credit at pacing rate b (bytes/s): burstFrames
// frame intervals' worth, at least the pacer's own burst.
func (m *Media) creditMax(b float64) congestion.ByteCount {
	d := congestion.ByteCount(b * burstFrames * time.Duration(m.frameInterval.Load()).Seconds())
	return max(d, m.maxBurst(b))
}

// part is the number of the part of window/rttBuckets now is in, counted
// from the first time the run loop handed the controller: the parts' edges
// do not depend on the monotonic clock's origin (a simulation's runs on
// their own clocks repeat). Run loop only.
func (m *Media) part(now congestion.Time, window time.Duration) int64 {
	if m.epoch.IsZero() {
		m.epoch = now
	}
	return int64(now.Sub(m.epoch) / (window / rttBuckets))
}

// bwEstimate is the highest delivery rate measured in the last 1.5 to 2 s
// (bytes/s; 0: none). Run loop only.
func (m *Media) bwEstimate(now congestion.Time) float64 {
	n := m.part(now, burstWindow)
	var hi float64
	for _, b := range m.bw {
		if b.n > n-rttBuckets {
			hi = max(hi, b.max)
		}
	}
	return hi
}

// trainSample measures a train's delivery rate from the ACK of a packet of
// it (r, sent at r.t), delivered already counting it: the bytes
// acknowledged since the train's first ACK over the time since, or over the
// time between sending the newest packets of the two where that is longer
// (a path that carries the bursts acknowledges at the rate they were sent,
// and ACKs that bunch on the way back say nothing faster). Run loop only.
func (m *Media) trainSample(r sentRecord, now congestion.Time) {
	a := &m.anchor
	switch {
	case !a.ok || int32(r.train-a.train) > 0:
		*a = trainAnchor{train: r.train, at: now, sent: r.t, delivered: m.delivered, ok: true}
		return
	case r.train != a.train:
		return // an older train's late acknowledgement
	case now == a.at: // the first ACK's other packets
		a.sent, a.delivered = max(a.sent, r.t), m.delivered
		return
	}
	n := m.delivered - a.delivered
	span := max(now.Sub(a.at), r.t.Sub(a.sent))
	if n < burstSamplePackets*uint64(m.mds()) || span <= 0 {
		return
	}
	v := float64(n) / span.Seconds()
	k := m.part(now, burstWindow)
	if b := &m.bw[k%rttBuckets]; b.n != k {
		*b = bwBucket{k, v}
	} else {
		b.max = max(b.max, v)
	}
}

// burstLoss counts a lost packet against its train, and turns bursts off
// when the train lost burstLossShare of its packets (at least burstLossMin):
// the burst overflowed a queue. Run loop only.
func (m *Media) burstLoss(r sentRecord, now congestion.Time) {
	t := &m.trains[r.train%trainRing]
	if t.id != r.train || t.backedOff {
		return
	}
	t.lost++
	if t.lost < max(burstLossMin, int(math.Ceil(burstLossShare*float64(t.sent)))) {
		return
	}
	t.backedOff = true
	if now.Before(m.holdUntil) {
		return
	}
	d := burstHold
	if !m.lastBackoff.IsZero() && now.Sub(m.lastBackoff) < 2*m.holdLen+burstHold {
		d = min(2*m.holdLen, burstHoldMax)
	}
	m.holdLen, m.holdUntil, m.lastBackoff = d, now.Add(d), now
	m.bw = [rttBuckets]bwBucket{} // measured with the bursts that overflowed
	m.burstBackoffs.Add(1)
}

func (m *Media) mds() congestion.ByteCount {
	return congestion.ByteCount(m.maxDatagramSize.Load())
}

// window is the normal congestion window: pacing rate × (min RTT + 2 frame
// intervals), within [floorWindowPackets, maxWindowPackets].
func (m *Media) window() congestion.ByteCount {
	d := m.rtt.MinRTT() + 2*time.Duration(m.frameInterval.Load())
	w := congestion.ByteCount(m.pacingRate() * d.Seconds())
	return min(max(w, floorWindowPackets*m.mds()), maxWindowPackets*m.mds())
}

// GetCongestionWindow returns the current congestion window in bytes.
func (m *Media) GetCongestionWindow() congestion.ByteCount {
	w := m.window()
	if b := congestion.ByteCount(m.brakeWindow.Load()); b > 0 {
		return min(b, w)
	}
	return w
}

func (m *Media) CanSend(bytesInFlight congestion.ByteCount) bool {
	ok := bytesInFlight < m.GetCongestionWindow()
	if limited := !ok; limited != m.limited {
		// quic-go asks before each packet it would send (and after each ACK
		// it processes), so the refusals' span is when the sender waited.
		m.limited = limited
		now := m.now()
		m.limMu.Lock()
		if limited {
			m.limitedSince = now
		} else {
			m.limitedTotal += now.Sub(m.limitedSince)
			m.limitedSince = 0
		}
		m.limMu.Unlock()
	}
	return ok
}

// WindowLimited is how long in all the congestion window has kept the sender
// from sending (from a packet it refused to the next one it allowed):
// time spent waiting for the path's acknowledgements, as opposed to the
// pacer's or the sender's own (a run loop that runs late on a busy CPU).
func (m *Media) WindowLimited() time.Duration {
	m.limMu.Lock()
	defer m.limMu.Unlock()
	d := m.limitedTotal
	if !m.limitedSince.IsZero() {
		d += m.now().Sub(m.limitedSince)
	}
	return d
}

// maxBurst is the pacer's burst at rate r (bytes/s): 2 ms of it, at least
// 10 packets.
func (m *Media) maxBurst(r float64) congestion.ByteCount {
	return max(congestion.ByteCount(r*burstDuration.Seconds()), maxBurstPackets*m.mds())
}

// burst is the burst rate the pacer runs at (bytes/s; run loop only).
func (m *Media) burst() float64 {
	if p := float64(m.burstRate.Load()); p > 0 {
		return max(p, m.pacingRate())
	}
	return m.burstFor(m.pacingRate(), 0, false, false)
}

// budgetAt is the pacing budget (token bucket at the burst rate) at time now.
func (m *Media) budgetAt(now congestion.Time) congestion.ByteCount {
	p := m.burst()
	if m.lastSent.IsZero() {
		return m.maxBurst(p)
	}
	budget := m.budget
	if d := now.Sub(m.lastSent); d > 0 {
		budget += congestion.ByteCount(p * d.Seconds())
	}
	return min(budget, m.maxBurst(p))
}

// creditAt is the burst credit (token bucket at the pacing rate) at time now.
func (m *Media) creditAt(now congestion.Time) congestion.ByteCount {
	b := m.pacingRate()
	if m.lastSent.IsZero() {
		return m.creditMax(b)
	}
	credit := m.credit
	if d := now.Sub(m.lastSent); d > 0 {
		credit += congestion.ByteCount(b * d.Seconds())
	}
	return min(credit, m.creditMax(b))
}

func (m *Media) HasPacingBudget(now congestion.Time) bool {
	return m.budgetAt(now) >= m.mds() && m.creditAt(now) >= m.mds()
}

// TimeUntilSend returns when the pacer has budget and credit for a full-size
// packet again (the zero Time: now).
func (m *Media) TimeUntilSend(congestion.ByteCount) congestion.Time {
	if m.lastSent.IsZero() {
		return 0
	}
	var d time.Duration
	if m.budget < m.mds() {
		d = time.Duration(math.Ceil(float64(m.mds()-m.budget) / m.burst() * 1e9))
	}
	if m.credit < m.mds() {
		d = max(d, time.Duration(math.Ceil(float64(m.mds()-m.credit)/m.pacingRate()*1e9)))
	}
	if d <= 0 {
		return 0
	}
	return m.lastSent.Add(max(d, minPacingDelay))
}

func (m *Media) OnPacketSent(sentTime congestion.Time, bytesInFlight congestion.ByteCount, pn congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool) {
	budget, credit := m.budgetAt(sentTime), m.creditAt(sentTime)
	m.budget = budget - min(bytes, budget)
	m.credit = credit - min(bytes, credit)
	m.lastSent = sentTime
	if isRetransmittable {
		if m.trainAt.IsZero() || sentTime.Sub(m.trainAt) > trainGap {
			m.train++
			m.trains[m.train%trainRing] = trainStat{id: m.train}
			if m.probing = m.probeAt.IsZero() || sentTime.Sub(m.probeAt) >= burstProbeEvery; m.probing {
				m.probeAt = sentTime
			}
		}
		m.trainAt = sentTime
		m.trains[m.train%trainRing].sent++
		m.sent[pn%sentRingSize] = sentRecord{pn: pn, t: sentTime, train: m.train}
		m.sentPos.Add(uint64(bytes))
	}
	m.burstAt(sentTime) // the rate the next packets go out at
	// bytesInFlight is quic-go's own count after this packet: it also drops
	// packets the controller never hears of (lost MTU probes), so done
	// catches up with them here.
	if sent := m.sentPos.Load(); sent >= uint64(bytesInFlight) {
		m.advance(sent-uint64(bytesInFlight), sentTime)
	}
}

// advance moves the delivery position forward to done at now (run loop
// only: the single writer).
func (m *Media) advance(done uint64, now congestion.Time) {
	if done = min(done, m.sentPos.Load()); done > m.donePos.Load() {
		m.donePos.Store(done)
		m.logMu.Lock()
		if last := &m.ackLog[(m.ackN+ackLogSize-1)%ackLogSize]; m.ackN > 0 && now.Sub(last.at) < ackLogResolution {
			last.done = done
		} else {
			m.ackLog[m.ackN%ackLogSize] = ackPoint{done, now}
			m.ackN++
		}
		m.logMu.Unlock()
		select {
		case m.progress <- struct{}{}:
		default:
		}
	}
}

// rttSample records a packet's round trip at now for RecentMinRTT (run loop
// only).
func (m *Media) rttSample(now congestion.Time, rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	n := m.part(now, recentRTTWindow)
	if b := &m.rttMin[n%rttBuckets]; b.n != n || b.min == 0 {
		*b = rttBucket{n, rtt}
	} else {
		b.min = min(b.min, rtt)
	}
	m.recentMinRTT.Store(int64(bucketMin(&m.rttMin, n)))
	n = m.part(now, baseRTTWindow)
	if b := &m.rttBase[n%rttBuckets]; b.n != n || b.min == 0 {
		*b = rttBucket{n, rtt}
	} else {
		b.min = min(b.min, rtt)
	}
	m.baseMinRTT.Store(int64(bucketMin(&m.rttBase, n)))
	k := m.part(now, rttBuckets*burstQueueSlot)
	if b := &m.qSlot[k%2]; b.n != k || b.min == 0 {
		*b = rttBucket{k, rtt}
	} else {
		b.min = min(b.min, rtt)
	}
	m.standing.Store(int64(m.standingQueue(now)))
	for {
		q := m.qMin.Load()
		if q != 0 && q <= int64(rtt) || m.qMin.CompareAndSwap(q, int64(rtt)) {
			break
		}
	}
}

// bucketMin is the smallest sample of the parts of a window up to part n (0:
// none).
func bucketMin(bs *[rttBuckets]rttBucket, n int64) time.Duration {
	var low time.Duration
	for _, b := range bs {
		if b.min > 0 && b.n > n-rttBuckets && (low == 0 || b.min < low) {
			low = b.min
		}
	}
	return low
}

// QueueDelay returns how much longer than the path's base round trip (the
// smallest of the last 7.5 to 10 s: baseRTTWindow) the quickest round trip
// of the packets acknowledged since the last call took (false: none was
// acknowledged). That excess is a queue
// every one of those packets met on the path, either way, measured by the
// host's own clock from each packet's sending to its ACK: unlike the
// receiver's one-way delays it holds nothing of the host's wait before
// sending (its frame queue, the pacer, a send loop running late) nor of the
// receiving application's (a browser's worker busy elsewhere: the network
// stack acknowledges on its own), and no clock offset. A burst's own queue
// at a slower bottleneck is not in it either: the first packets of each
// burst meet none. One caller: the rate controller, once per receive report.
func (m *Media) QueueDelay() (time.Duration, bool) {
	q := m.qMin.Swap(0)
	if q == 0 {
		return 0, false
	}
	return max(0, time.Duration(q)-time.Duration(m.baseMinRTT.Load())), true
}

func (m *Media) OnPacketAcked(pn congestion.PacketNumber, ackedBytes, _ congestion.ByteCount, eventTime congestion.Time) {
	if m.firstAck.IsZero() {
		m.firstAck = eventTime
	}
	m.largestAcked = max(m.largestAcked, pn)
	m.ackedPackets.Add(1)
	m.ackedBytes.Add(uint64(ackedBytes))
	m.delivered += uint64(ackedBytes)
	if r := m.sent[pn%sentRingSize]; r.pn == pn && !r.t.IsZero() {
		m.rttSample(eventTime, eventTime.Sub(r.t))
		m.trainSample(r, eventTime)
	}
	m.advance(m.donePos.Load()+uint64(ackedBytes), eventTime)
	if b := m.brakeWindow.Load(); b > 0 {
		b += int64(ackedBytes)
		if congestion.ByteCount(b) >= m.window() {
			b = 0 // recovered
		}
		m.brakeWindow.Store(b)
	}
}

// OnCongestionEvent counts the loss for the application; it does not shrink
// the window unless the loss proves persistent congestion.
func (m *Media) OnCongestionEvent(pn congestion.PacketNumber, lostBytes, _ congestion.ByteCount) {
	if lostBytes == 0 { // ECN congestion experienced
		m.ecnMarks.Add(1)
		return
	}
	m.lostPackets.Add(1)
	m.lostBytes.Add(uint64(lostBytes))
	now := m.now()
	m.advance(m.donePos.Load()+uint64(lostBytes), now)
	if r := m.sent[pn%sentRingSize]; r.pn == pn && !r.t.IsZero() {
		m.burstLoss(r, now)
	}
	if m.persistentCongestion(pn) {
		m.collapse()
	}
}

// persistentCongestion reports whether the loss of pn establishes persistent
// congestion (RFC 9002 section 7.6.2): it was sent more than 3 × PTO after the
// first packet lost since the largest acknowledged one. Send times count, not
// the time since the last ACK: losses are only detected after the outage, one
// PTO backoff and an RTT later, so that time would brake on short outages.
func (m *Media) persistentCongestion(pn congestion.PacketNumber) bool {
	r := m.sent[pn%sentRingSize]
	if r.pn != pn || r.t.IsZero() || m.firstAck.IsZero() || !r.t.After(m.firstAck) {
		return false // send time unknown, or sent before the first RTT sample
	}
	if m.lossRunStart.IsZero() || m.largestAcked > m.lossRunPN {
		m.lossRunPN, m.lossRunStart = pn, r.t
		return false
	}
	if r.t.Sub(m.lossRunStart) <= persistentCongestionThreshold*m.rtt.PTO(true) {
		return false
	}
	m.lossRunStart = 0 // one collapse per episode
	return true
}

func (m *Media) OnRetransmissionTimeout(packetsRetransmitted bool) {
	if packetsRetransmitted {
		m.collapse()
	}
}

func (m *Media) collapse() {
	m.collapses.Add(1)
	m.brakeWindow.Store(int64(minWindowPackets * m.mds()))
}

func (m *Media) SetMaxDatagramSize(s congestion.ByteCount) {
	m.maxDatagramSize.Store(int64(s))
}

// MaybeExitSlowStart is a no-op: Media has no slow start (except after a collapse).
func (m *Media) MaybeExitSlowStart() {}

// InSlowStart reports whether the window is growing back after a collapse.
func (m *Media) InSlowStart() bool { return m.brakeWindow.Load() > 0 }

// InRecovery is always false: losses don't change the window.
func (m *Media) InRecovery() bool { return false }
