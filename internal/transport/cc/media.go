// Package cc holds congestion controllers for quic-go's pluggable congestion
// control (quic.Config.Congestion; see third_party/README.md).
package cc

import (
	"math"
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
	// frame-size variance, retransmissions, audio and input.
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

	// RFC 9002 kPersistentCongestionThreshold.
	persistentCongestionThreshold = 3
	// Send times kept for persistent-congestion detection, by packet number.
	// With a window of more packets the oldest lost packets of an outage are
	// not found, which only makes the brake later.
	sentRingSize = 4096
)

// Media is a rate-following congestion controller for real-time video. Unlike
// NewReno it does not probe for bandwidth: it paces at PacingGain × the
// application's target bitrate and allows a window of
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
	budget   congestion.ByteCount // pacing budget left after the last packet
	lastSent congestion.Time
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
}

type sentRecord struct {
	pn congestion.PacketNumber
	t  congestion.Time
}

var _ congestion.CongestionControl = (*Media)(nil)

// NewMedia returns a media controller for one path of a connection.
func NewMedia(rtt congestion.RTTStats, initialMaxDatagramSize congestion.ByteCount) *Media {
	m := &Media{rtt: rtt, largestAcked: -1, progress: make(chan struct{}, 1)}
	m.targetBitrate.Store(DefaultTargetBitrate)
	m.frameInterval.Store(int64(DefaultFrameInterval))
	m.maxDatagramSize.Store(int64(initialMaxDatagramSize))
	return m
}

// MediaFactory is a quic.Config.Congestion factory that creates Media controllers.
func MediaFactory(rtt congestion.RTTStats, initialMaxDatagramSize congestion.ByteCount) congestion.CongestionControl {
	return NewMedia(rtt, initialMaxDatagramSize)
}

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
	return MediaStats{
		TargetBitrate: m.targetBitrate.Load(),
		PacingRate:    int64(m.pacingRate() * 8),
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

// MinRTT is the path's minimum round-trip time (0: no sample yet).
func (m *Media) MinRTT() time.Duration { return m.rtt.MinRTT() }

// pacingRate is in bytes/s.
func (m *Media) pacingRate() float64 {
	return float64(m.targetBitrate.Load()) * PacingGain / 8
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
	return bytesInFlight < m.GetCongestionWindow()
}

func (m *Media) maxBurst() congestion.ByteCount {
	return max(congestion.ByteCount(m.pacingRate()*burstDuration.Seconds()), maxBurstPackets*m.mds())
}

// budgetAt is the pacing budget (token bucket) at time now.
func (m *Media) budgetAt(now congestion.Time) congestion.ByteCount {
	if m.lastSent.IsZero() {
		return m.maxBurst()
	}
	budget := m.budget
	if d := now.Sub(m.lastSent); d > 0 {
		budget += congestion.ByteCount(m.pacingRate() * d.Seconds())
	}
	return min(budget, m.maxBurst())
}

func (m *Media) HasPacingBudget(now congestion.Time) bool {
	return m.budgetAt(now) >= m.mds()
}

// TimeUntilSend returns when the pacer has budget for a full-size packet
// again (the zero Time: now).
func (m *Media) TimeUntilSend(congestion.ByteCount) congestion.Time {
	if m.lastSent.IsZero() || m.budget >= m.mds() {
		return 0
	}
	d := time.Duration(math.Ceil(float64(m.mds()-m.budget) / m.pacingRate() * 1e9))
	return m.lastSent.Add(max(d, minPacingDelay))
}

func (m *Media) OnPacketSent(sentTime congestion.Time, bytesInFlight congestion.ByteCount, pn congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool) {
	budget := m.budgetAt(sentTime)
	m.budget = budget - min(bytes, budget)
	m.lastSent = sentTime
	if isRetransmittable {
		m.sent[pn%sentRingSize] = sentRecord{pn: pn, t: sentTime}
		m.sentPos.Add(uint64(bytes))
	}
	// bytesInFlight is quic-go's own count after this packet: it also drops
	// packets the controller never hears of (lost MTU probes), so done
	// catches up with them here.
	if sent := m.sentPos.Load(); sent >= uint64(bytesInFlight) {
		m.advance(sent - uint64(bytesInFlight))
	}
}

// advance moves the delivery position forward to done (run loop only: the
// single writer).
func (m *Media) advance(done uint64) {
	if done = min(done, m.sentPos.Load()); done > m.donePos.Load() {
		m.donePos.Store(done)
		select {
		case m.progress <- struct{}{}:
		default:
		}
	}
}

func (m *Media) OnPacketAcked(pn congestion.PacketNumber, ackedBytes, _ congestion.ByteCount, eventTime congestion.Time) {
	if m.firstAck.IsZero() {
		m.firstAck = eventTime
	}
	m.largestAcked = max(m.largestAcked, pn)
	m.ackedPackets.Add(1)
	m.ackedBytes.Add(uint64(ackedBytes))
	m.advance(m.donePos.Load() + uint64(ackedBytes))
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
	m.advance(m.donePos.Load() + uint64(lostBytes))
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
