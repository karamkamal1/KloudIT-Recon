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
)

// Media is a rate-following congestion controller for real-time video. Unlike
// NewReno it does not probe for bandwidth: it paces at PacingGain × the
// application's target bitrate and allows a window of
// pacing rate × (min RTT + 2 frame intervals), so the queue it can build is
// bounded by two frames. A loss does NOT shrink the window; losses are counted
// (Stats) for the application's rate controller, which lowers the target.
// The only brake of its own is for persistent congestion (no ACK for
// 3 × PTO while data was in flight) or a retransmission timeout: the window
// collapses to two packets and grows back by the acknowledged bytes (slow
// start) to the normal window.
//
// The quic-go connection calls the congestion.CongestionControl methods from its
// run loop. SetTarget, SetTargetBitrate, SetFrameInterval, TargetBitrate and
// Stats may be called from any goroutine.
type Media struct {
	rtt   congestion.RTTStats
	clock func() congestion.Time

	targetBitrate   atomic.Int64 // bit/s
	frameInterval   atomic.Int64 // ns
	maxDatagramSize atomic.Int64 // bytes
	brakeWindow     atomic.Int64 // bytes; > 0 while recovering from persistent congestion

	// run loop only
	budget       congestion.ByteCount // pacing budget left after the last packet
	lastSent     congestion.Time
	lastProgress congestion.Time // last ACK, or the first send into an empty pipe

	ackedPackets, ackedBytes atomic.Uint64
	lostPackets, lostBytes   atomic.Uint64
	ecnMarks, collapses      atomic.Uint64
}

var _ congestion.CongestionControl = (*Media)(nil)

// NewMedia returns a media controller for one path of a connection.
func NewMedia(rtt congestion.RTTStats, initialMaxDatagramSize congestion.ByteCount) *Media {
	m := &Media{rtt: rtt, clock: congestion.Now}
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

func (m *Media) OnPacketSent(sentTime congestion.Time, bytesInFlight congestion.ByteCount, _ congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool) {
	budget := m.budgetAt(sentTime)
	m.budget = budget - min(bytes, budget)
	m.lastSent = sentTime
	if isRetransmittable && bytesInFlight <= bytes {
		// The pipe was empty: the persistent-congestion clock starts now.
		m.lastProgress = sentTime
	}
}

func (m *Media) OnPacketAcked(_ congestion.PacketNumber, ackedBytes, _ congestion.ByteCount, eventTime congestion.Time) {
	m.lastProgress = eventTime
	m.ackedPackets.Add(1)
	m.ackedBytes.Add(uint64(ackedBytes))
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
func (m *Media) OnCongestionEvent(_ congestion.PacketNumber, lostBytes, _ congestion.ByteCount) {
	if lostBytes == 0 { // ECN congestion experienced
		m.ecnMarks.Add(1)
		return
	}
	m.lostPackets.Add(1)
	m.lostBytes.Add(uint64(lostBytes))
	now := m.clock()
	if !m.lastProgress.IsZero() && now.Sub(m.lastProgress) > persistentCongestionThreshold*m.rtt.PTO(true) {
		m.collapse(now)
	}
}

func (m *Media) OnRetransmissionTimeout(packetsRetransmitted bool) {
	if packetsRetransmitted {
		m.collapse(m.clock())
	}
}

func (m *Media) collapse(now congestion.Time) {
	m.collapses.Add(1)
	m.brakeWindow.Store(int64(minWindowPackets * m.mds()))
	m.lastProgress = now // one collapse per episode
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
