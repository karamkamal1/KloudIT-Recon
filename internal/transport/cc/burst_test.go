package cc

import (
	"slices"
	"testing"
	"time"

	"github.com/quic-go/quic-go/congestion"
)

// burstPath drives a controller with frames sent as its pacer allows over a
// bottleneck of capacity bytes/s and a one-way delay each way; the receiver
// acknowledges every 10th packet and each frame's last. A lost packet is
// declared lost with the first ACK after it.
type burstPath struct {
	m        *Media
	now      congestion.Time
	pn       congestion.PacketNumber
	owd      time.Duration
	capacity float64
	queue    time.Duration // added to every packet's way: a queue that stands
	free     congestion.Time
	acks     []burstAck
	lost     []congestion.PacketNumber
}

type burstAck struct {
	at  congestion.Time
	pns []congestion.PacketNumber
}

func newBurstPath(t *testing.T, minRTT time.Duration, bps int64, capacity float64) *burstPath {
	t.Helper()
	m, now := newTestMedia(minRTT)
	m.SetClock(func() congestion.Time { return *now })
	m.SetTarget(bps, time.Second/60)
	p := &burstPath{m: m, now: *now, owd: minRTT / 2, capacity: capacity}
	m.SetClock(func() congestion.Time { return p.now })
	return p
}

// advance moves the clock to t, delivering the ACKs due by then.
func (p *burstPath) advance(t congestion.Time) {
	slices.SortStableFunc(p.acks, func(a, b burstAck) int { return int(a.at.Sub(b.at)) })
	for len(p.acks) > 0 && !p.acks[0].at.After(t) {
		a := p.acks[0]
		p.acks = p.acks[1:]
		p.now = max(p.now, a.at)
		for _, pn := range p.lost {
			p.m.OnCongestionEvent(pn, mds, mds)
		}
		p.lost = p.lost[:0]
		for _, pn := range a.pns {
			p.m.OnPacketAcked(pn, mds, mds, a.at)
		}
	}
	p.now = max(p.now, t)
}

// frame sends n packets from now as the pacer allows (lose: the path drops
// packet i of the frame) and returns how long the sending took; then the
// clock moves on to the next frame, a frame interval after the first.
func (p *burstPath) frame(n int, lose func(int) bool) time.Duration {
	start := p.now
	var pend []congestion.PacketNumber
	for i := range n {
		for !p.m.HasPacingBudget(p.now) {
			next := p.m.TimeUntilSend(0)
			if !next.After(p.now) {
				next = p.now.Add(time.Microsecond)
			}
			p.advance(next)
		}
		pn := p.pn
		p.pn++
		p.m.OnPacketSent(p.now, mds, pn, mds, true)
		if lose != nil && lose(i) {
			p.lost = append(p.lost, pn)
			continue
		}
		dep := max(p.now, p.free).Add(time.Duration(mds / p.capacity * 1e9))
		p.free = dep
		pend = append(pend, pn)
		if len(pend) == 10 || i == n-1 {
			p.acks = append(p.acks, burstAck{at: dep.Add(2*p.owd + p.queue), pns: pend})
			pend = nil
		}
	}
	took := p.now.Sub(start)
	p.advance(start.Add(time.Second / 60))
	return took
}

// TestMediaQueueDelay: the quickest round trip since the last call over the
// path's base: none without an ACK, 0 on an empty path, the queue's length
// where one stands, whatever ACKs come late on top. StandingQueue: the queue
// every packet of the last 50-100 ms met.
func TestMediaQueueDelay(t *testing.T) {
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	if _, ok := p.m.QueueDelay(); ok {
		t.Fatal("a queue delay before any ACK")
	}
	for range 30 {
		p.frame(100, nil)
	}
	if q, ok := p.m.QueueDelay(); !ok || q > 200*time.Microsecond {
		t.Fatalf("queue delay %v %v on an empty path, want about 0", q, ok)
	}
	p.queue = 10 * time.Millisecond
	p.frame(100, nil)
	if q, ok := p.m.QueueDelay(); !ok || q < 9*time.Millisecond || q > 11*time.Millisecond {
		t.Fatalf("queue delay %v %v with a 10 ms queue, want about 10 ms", q, ok)
	}
	if _, ok := p.m.QueueDelay(); ok {
		t.Fatal("a queue delay again without a new ACK")
	}
	for range 10 {
		p.frame(100, nil)
	}
	if q := p.m.StandingQueue(); q < 9*time.Millisecond || q > 11*time.Millisecond {
		t.Fatalf("standing queue %v after 10 frames over a 10 ms queue, want about 10 ms", q)
	}
	p.queue = 0
	for range 10 {
		p.frame(100, nil)
	}
	if q := p.m.StandingQueue(); q > time.Millisecond {
		t.Fatalf("standing queue %v once the queue drained, want about 0", q)
	}
}
