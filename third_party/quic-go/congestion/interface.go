// Package congestion lets an application replace the congestion controller of
// a connection (see Config.Congestion and Conn.CongestionControl).
package congestion

import (
	"time"

	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
)

type (
	// A ByteCount is a number of bytes.
	ByteCount = protocol.ByteCount
	// A PacketNumber is a QUIC packet number.
	PacketNumber = protocol.PacketNumber
	// A Time is an instant in monotonic time, as used by the send path.
	Time = monotime.Time
)

// Now returns the current monotonic time.
func Now() Time { return monotime.Now() }

// RTTStats is a read-only view of the connection's RTT estimator.
// Before the first RTT sample, the RTTs report the initial RTT.
type RTTStats interface {
	MinRTT() time.Duration
	SmoothedRTT() time.Duration
	LatestRTT() time.Duration
	MeanDeviation() time.Duration
	// PTO is the probe timeout (RFC 9002, section 6.2.1).
	PTO(includeMaxAckDelay bool) time.Duration
}

// CongestionControl is a congestion controller. It has the same semantics as
// quic-go's built-in NewReno controller (internal/congestion): all methods are
// called from the connection's run loop and must not block.
type CongestionControl interface {
	// TimeUntilSend returns when the next packet may be sent (pacing);
	// the zero Time means now.
	TimeUntilSend(bytesInFlight ByteCount) Time
	HasPacingBudget(now Time) bool
	OnPacketSent(sentTime Time, bytesInFlight ByteCount, packetNumber PacketNumber, bytes ByteCount, isRetransmittable bool)
	CanSend(bytesInFlight ByteCount) bool
	MaybeExitSlowStart()
	OnPacketAcked(number PacketNumber, ackedBytes ByteCount, priorInFlight ByteCount, eventTime Time)
	// OnCongestionEvent is called for every lost packet, and with
	// lostBytes == 0 for an ECN congestion-experienced mark.
	OnCongestionEvent(number PacketNumber, lostBytes ByteCount, priorInFlight ByteCount)
	OnRetransmissionTimeout(packetsRetransmitted bool)
	SetMaxDatagramSize(ByteCount)
	InSlowStart() bool
	InRecovery() bool
	GetCongestionWindow() ByteCount
}
