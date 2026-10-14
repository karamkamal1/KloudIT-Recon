package cc

import (
	"math"
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

// TestMediaBurst: a frame goes out at the burst rate while the burst credit
// lasts, what is beyond it at the pacing rate; before anything is measured
// the burst rate is twice the pacing rate.
func TestMediaBurst(t *testing.T) {
	// 80 Mbit/s: pacing 96 Mbit/s = 12 MB/s, credit 1.5 frame intervals'
	// worth = 300 kB, bursts at 24 MB/s.
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 1e12)
	if pc := p.m.Pacing(); pc.Rate != 96_000_000 || pc.Burst != 192_000_000 || pc.Credit < 299_999 || pc.Credit > 300_000 {
		t.Fatalf("pacing before any packet %+v", pc)
	}
	// A 120 kB frame: 5 ms at the burst rate, not 10 ms at the pacing rate.
	if took := p.frame(100, nil); took > 5500*time.Microsecond {
		t.Fatalf("a 120 kB frame took %v to send, want about 5 ms at the burst rate", took)
	}
	// Over time no more than the pacing rate and the credit: a 900 kB frame
	// (a key frame) takes at least (900 - 300) kB / 12 MB/s = 50 ms.
	p2 := newBurstPath(t, 2*time.Millisecond, 80_000_000, 1e12)
	if took := p2.frame(750, nil); took < 48*time.Millisecond {
		t.Fatalf("a 900 kB frame took %v, want at least 50 ms: the credit, then the pacing rate", took)
	}
}

// TestMediaBurstFollowsPath: the burst rate grows to what the path carries
// (a gigabit LAN: maxBurstRate), comes down to burstProbe x a slower path's
// rate measured by the ACKs' spacing, is the pacing rate on a path not
// burstMinGain times faster than it, and while a queue stands on the path.
func TestMediaBurstFollowsPath(t *testing.T) {
	for _, c := range []struct {
		name     string
		capacity float64 // bytes/s
		want     float64 // bytes/s
	}{
		{"gigabit LAN", 125e6, maxBurstRate},
		{"300 Mbit/s", 37.5e6, 37.5e6 * burstProbe},
		{"150 Mbit/s, under twice the pacing rate", 18.75e6, 12e6},
		{"60 Mbit/s, under the pacing rate", 7.5e6, 12e6},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newBurstPath(t, 2*time.Millisecond, 80_000_000, c.capacity)
			for range 60 {
				p.frame(100, nil)
			}
			got := float64(p.m.Pacing().Burst) / 8
			if math.Abs(got/c.want-1) > 0.1 {
				t.Fatalf("burst rate %.1f MB/s after a second of frames, want %.1f MB/s ±10 %%", got/1e6, c.want/1e6)
			}
		})
	}
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	for range 60 {
		p.frame(100, nil)
	}
	p.queue = 10 * time.Millisecond // every packet's way: a queue that stands
	for range 10 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst != pc.Rate {
		t.Fatalf("burst %d bit/s with a 10 ms queue standing, want the pacing rate %d", pc.Burst, pc.Rate)
	}
	p.queue = 0
	for range 10 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst != maxBurstRate*8 {
		t.Fatalf("burst %d bit/s once the queue drained, want %d again", pc.Burst, maxBurstRate*8)
	}
}

// TestMediaBurstProbe: on a path not burstMinGain times faster than the
// pacing rate one frame a second still goes out at burstStartGain x the
// pacing rate (a probe for a path that got faster), the others at the
// pacing rate; a path that did is burst into again from the next probe.
func TestMediaBurstProbe(t *testing.T) {
	// 150 Mbit/s against 96 Mbit/s of pacing: a 120 kB frame takes 10 ms at
	// the pacing rate, 5 ms at twice it.
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 18.75e6)
	for range 60 {
		p.frame(100, nil)
	}
	fast := 0
	for range 120 {
		if took := p.frame(100, nil); took < 7*time.Millisecond {
			fast++
		}
	}
	if fast != 2 {
		t.Fatalf("%d frames in 2 s went faster than the pacing rate, want the 2 probes", fast)
	}
	// The path gets faster (a gigabit): the next probe measures it.
	p.capacity = 125e6
	for range 90 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst != maxBurstRate*8 {
		t.Fatalf("burst %d bit/s 1.5 s after the path got faster, want %d", pc.Burst, int64(maxBurstRate*8))
	}
}

// TestMediaBurstLoss: a train that loses burstLossShare of its packets (a
// queue its burst overflowed) turns bursts off for burstHold, twice as long
// when it happens again soon after; random losses do not.
func TestMediaBurstLoss(t *testing.T) {
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	for range 30 {
		p.frame(100, func(i int) bool { return i == 50 }) // 1 %: random loss
	}
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts off after random losses")
	}
	p.frame(100, func(i int) bool { return i >= 90 }) // the burst's tail dropped
	p.frame(100, nil)
	if pc, s := p.m.Pacing(), p.m.Stats(); pc.Burst != pc.Rate || s.BurstBackoffs != 1 {
		t.Fatalf("after a burst lost its tail: burst %d, pacing %d, backoffs %d; want bursts off, one backoff", pc.Burst, pc.Rate, s.BurstBackoffs)
	}
	for range 60 * 5 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts still off 5 s after the overflow")
	}
	p.frame(100, func(i int) bool { return i >= 90 })
	for range 60 * 6 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst != pc.Rate {
		t.Fatal("a second overflow soon after the first: bursts back after 6 s, want 10 s off")
	}
	for range 60 * 5 {
		p.frame(100, nil)
	}
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts still off 11 s after the second overflow")
	}
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
