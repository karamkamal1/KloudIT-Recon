package host

import (
	"github.com/karamkamal1/kloudit-recon/internal/transport"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// videoPace is the media congestion controller's pacing as video frames see
// it (cc.Pace less audio and the overhead allowance): rate is what it sends
// over time and burst what a frame starts at (bit/s), for as long as credit
// (bytes) lasts. burst equals rate where the pacer does not burst; rate 0:
// the connection does not pace by the target.
type videoPace struct {
	rate, burst, credit float64
}

// flatPace is a pace without bursts at bps (bit/s).
func flatPace(bps float64) videoPace { return videoPace{rate: bps, burst: bps} }

// videoPaceOf is p (cc.Media.Pacing) less nonVideo bit/s of audio and
// overhead.
func videoPaceOf(p cc.Pace, nonVideo float64) videoPace {
	return videoPace{rate: max(0, float64(p.Rate)-nonVideo), burst: max(0, float64(p.Burst)-nonVideo), credit: float64(p.Credit)}
}

// videoPace is the connection's pace for video (zero with another
// controller).
func (s *Session) videoPace() videoPace {
	m := transport.MediaControl(s.c)
	if m == nil {
		return videoPace{}
	}
	return videoPaceOf(m.Pacing(), float64((s.audioKbps.Load()+ccOverheadKbps)*1000))
}

// pacerModel is a fluid model of the media congestion controller's pacer
// (cc.Media: bursts): what it has queued (backlog) drains at the burst rate
// while the burst credit lasts, the credit sinking by the difference to the
// pacing rate, then at the pacing rate; with nothing queued the credit
// refills at the pacing rate up to its most. Rates in bytes/s.
type pacerModel struct {
	rate, burst, depth float64
	backlog, credit    float64
	set                bool
}

// pace sets the pacing (bit/s, the credit's most in bytes) for what follows,
// less overhead (a share of the rates taken by packet and frame headers). An
// idle pacer starts with its credit full.
func (p *pacerModel) pace(v videoPace, overhead float64) {
	p.rate, p.burst, p.depth = v.rate*(1-overhead)/8, max(v.burst, v.rate)*(1-overhead)/8, v.credit
	if !p.set {
		p.credit, p.set = p.depth, true
	}
	p.credit = min(p.credit, p.depth)
}

// advance lets dt seconds pass.
func (p *pacerModel) advance(dt float64) {
	for dt > 0 && p.rate > 0 {
		if p.backlog <= 0 {
			p.backlog, p.credit = 0, min(p.depth, p.credit+p.rate*dt)
			return
		}
		if p.credit > 0 && p.burst > p.rate {
			tc, tq := p.credit/(p.burst-p.rate), p.backlog/p.burst
			t := min(dt, tc, tq)
			p.backlog, p.credit, dt = p.backlog-p.burst*t, p.credit-(p.burst-p.rate)*t, dt-t
			if t == tc {
				p.credit = 0
			}
			if t == tq {
				p.backlog = 0
			}
			continue
		}
		t := min(dt, p.backlog/p.rate)
		p.backlog, dt = p.backlog-p.rate*t, dt-t
		if p.backlog < 1 {
			p.backlog = 0
		}
	}
}

// drain is how long (seconds) the first n bytes of the backlog take to go
// out from now.
func (p *pacerModel) drain(n float64) float64 {
	if n <= 0 || p.rate <= 0 {
		return 0
	}
	if p.credit > 0 && p.burst > p.rate {
		tc := p.credit / (p.burst - p.rate)
		if n <= p.burst*tc {
			return n / p.burst
		}
		return tc + (n-p.burst*tc)/p.rate
	}
	return n / p.rate
}
