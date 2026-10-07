package quic

import (
	"github.com/quic-go/quic-go/congestion"
	"github.com/quic-go/quic-go/internal/ackhandler"
	internalcongestion "github.com/quic-go/quic-go/internal/congestion"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
)

// CongestionControl returns the congestion controller that Config.Congestion
// created for the connection's current path, or nil if the connection uses the
// default controller. A path migration replaces the controller, so call this
// again instead of keeping the result.
func (c *Conn) CongestionControl() congestion.CongestionControl {
	if cc := c.congestionControl.Load(); cc != nil {
		return *cc
	}
	return nil
}

// congestionFactory adapts Config.Congestion for the sent packet handler.
func (c *Conn) congestionFactory() ackhandler.CongestionFactory {
	newCC := c.config.Congestion
	if newCC == nil {
		return nil
	}
	return func(rttStats *utils.RTTStats, initialMaxDatagramSize protocol.ByteCount) internalcongestion.SendAlgorithmWithDebugInfos {
		cc := newCC(rttStats, initialMaxDatagramSize)
		c.congestionControl.Store(&cc)
		return lossCounter{CongestionControl: cc, stats: &c.connStats}
	}
}

// lossCounter maintains the loss counters of ConnectionStats, as the default
// controller does.
type lossCounter struct {
	congestion.CongestionControl
	stats *utils.ConnectionStats
}

func (l lossCounter) OnCongestionEvent(pn congestion.PacketNumber, lostBytes, priorInFlight congestion.ByteCount) {
	l.stats.PacketsLost.Add(1)
	l.stats.BytesLost.Add(uint64(lostBytes))
	l.CongestionControl.OnCongestionEvent(pn, lostBytes, priorInFlight)
}
