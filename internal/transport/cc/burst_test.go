package cc

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/quic-go/quic-go/congestion"
)

// burstPath drives a controller with frames sent as its pacer allows over a
// bottleneck of capacity bytes/s and a one-way delay each way; the receiver
// acknowledges every 10th packet and each frame's last, its ACKs held up on
// the way back by up to jitter (in order: they bunch, as on a busy return
// path or a Wi-Fi uplink). A bottleneck with a limit drops what would wait
// longer than it in its queue. A lost packet is declared lost with the first
// ACK after it.
type burstPath struct {
	m        *Media
	now      congestion.Time
	pn       congestion.PacketNumber
	owd      time.Duration
	capacity float64
	queue    time.Duration // added to every packet's way: a queue that stands
	limit    time.Duration // the bottleneck's queue (0: unlimited)
	jitter   time.Duration // the ACKs' way back: 0..jitter more, in order
	rng      *rand.Rand
	lastAck  congestion.Time
	free     congestion.Time
	acks     []burstAck
	lost     []congestion.PacketNumber
	dropped  int // packets the bottleneck's queue dropped
}

type burstAck struct {
	at  congestion.Time
	pns []congestion.PacketNumber
}

func newBurstPath(t *testing.T, minRTT time.Duration, bps int64, capacity float64) *burstPath {
	t.Helper()
	m, now := newTestMedia(minRTT)
	m.SetTarget(bps, time.Second/60)
	p := &burstPath{m: m, now: *now, owd: minRTT / 2, capacity: capacity, rng: rand.New(rand.NewPCG(1, 2))}
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

// ack queues an ACK of pns for the packet that left the bottleneck at dep.
func (p *burstPath) ack(dep congestion.Time, pns []congestion.PacketNumber) {
	at := dep.Add(2*p.owd + p.queue)
	if p.jitter > 0 {
		at = at.Add(time.Duration(p.rng.Float64() * float64(p.jitter)))
	}
	at = max(at, p.lastAck)
	p.lastAck = at
	p.acks = append(p.acks, burstAck{at: at, pns: pns})
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
		if lose != nil && lose(i) || p.limit > 0 && p.free.Sub(p.now) > p.limit {
			if p.limit > 0 && p.free.Sub(p.now) > p.limit {
				p.dropped++
			}
			p.lost = append(p.lost, pn)
			continue
		}
		dep := max(p.now, p.free).Add(time.Duration(mds / p.capacity * 1e9))
		p.free = dep
		pend = append(pend, pn)
		if len(pend) == 10 || i == n-1 {
			p.ack(dep, pend)
			pend = nil
		}
	}
	took := p.now.Sub(start)
	p.advance(start.Add(time.Second / 60))
	return took
}

// frames sends k frames of n packets; the highest frame burst rate and
// delivery estimate (bit/s) the controller published meanwhile.
func (p *burstPath) frames(k, n int) (burst, est int64) {
	for range k {
		p.frame(n, nil)
		s := p.m.Stats()
		burst, est = max(burst, s.BurstRate), max(est, s.BurstEstimate)
	}
	return burst, est
}

// TestMediaBurst: nothing is burst on a path not measured yet (a key frame
// that skips the probe goes at the pacing rate); on a measured gigabit path
// a frame goes out at the burst rate while the burst credit lasts, what is
// beyond it at the pacing rate.
func TestMediaBurst(t *testing.T) {
	// 80 Mbit/s: pacing 96 Mbit/s = 12 MB/s, credit 1.5 frame intervals'
	// worth = 300 kB.
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	if pc := p.m.Pacing(); pc.Rate != 96_000_000 || pc.Burst != pc.Rate || pc.Credit < 299_999 || pc.Credit > 300_000 {
		t.Fatalf("pacing before any packet %+v, want no bursts before a measure", pc)
	}
	// The session's first frame, a 300 kB key frame: the pacer's 24 kB
	// clump at once, the rest at the pacing rate (23 ms), not at twice it.
	p.m.SkipProbe()
	if took := p.frame(250, nil); took < 22*time.Millisecond {
		t.Fatalf("the first key frame took %v to send, want about 23 ms at the pacing rate", took)
	}
	// Two probes later (one a second) the path is measured: a 120 kB
	// frame goes in about a millisecond, not 8 ms at the pacing rate.
	p.frames(60*3, 100)
	if took := p.frame(100, nil); took > 2*time.Millisecond {
		t.Fatalf("a 120 kB frame took %v to send on a measured gigabit path, want about 1 ms at the burst rate", took)
	}
	// Over time no more than the pacing rate and the credit: a 900 kB frame
	// (a key frame) takes at least (900 - 300) kB / 12 MB/s = 50 ms.
	p.advance(p.now.Add(100 * time.Millisecond))
	if took := p.frame(750, nil); took < 48*time.Millisecond {
		t.Fatalf("a 900 kB frame took %v, want at least 50 ms: the credit, then the pacing rate", took)
	}
}

// TestMediaBurstFollowsPath: the burst rate grows to what the path carries
// (a gigabit LAN: maxBurstRate), is burstProbe x a slower path's rate
// measured by the ACKs' spacing, is the pacing rate on a path not
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
			p.frames(60*4, 100)
			got := float64(p.m.Pacing().Burst) / 8
			if math.Abs(got/c.want-1) > 0.1 {
				t.Fatalf("burst rate %.1f MB/s after 4 s of frames, want %.1f MB/s ±10 %% (estimate %.1f MB/s)",
					got/1e6, c.want/1e6, float64(p.m.Stats().BurstEstimate)/8e6)
			}
		})
	}
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	p.frames(60*4, 100)
	p.queue = 10 * time.Millisecond // every packet's way: a queue that stands
	p.frames(10, 100)
	if pc := p.m.Pacing(); pc.Burst != pc.Rate {
		t.Fatalf("burst %d bit/s with a 10 ms queue standing, want the pacing rate %d", pc.Burst, pc.Rate)
	}
	p.queue = 0
	p.frames(10, 100)
	if pc := p.m.Pacing(); pc.Burst != maxBurstRate*8 {
		t.Fatalf("burst %d bit/s once the queue drained, want %d again", pc.Burst, maxBurstRate*8)
	}
}

// TestMediaBurstAckJitter: ACKs that bunch on the way back (in order, by up
// to 1-3 ms) do not ratchet the estimate up. On paths 1.1-1.7 times the
// pacing rate (the review's 20 Mbit/s path at a 15 Mbit/s target, 105 and
// 163 Mbit/s at 80 Mbit/s), deep or shallow queue: before the whole-train
// measure and the median, the highest per-ACK sample of 2 s took the
// estimate to 131 Mbit/s on the 20 Mbit/s path at 3 ms and every frame
// burst at several times the bottleneck. Now a probe's few ACKs bunched
// by chance may still read high twice in a row: the first frame that
// bursts on it queues at the bottleneck (burstGrowthMax) and takes the
// estimate back down at once, so at most one frame in a hundred bursts;
// a shallow bottleneck drops no more than without bursts but for that
// frame's or a probe's tail, and each such loss holds bursts and probes
// off for twice as long as the last (burstHold). A gigabit path with 1 ms
// of it is burst into.
func TestMediaBurstAckJitter(t *testing.T) {
	for _, c := range []struct {
		name     string
		bps      int64
		capacity float64 // bytes/s
		pkts     int     // a frame
		limit    time.Duration
	}{
		{"20 Mbit/s at 15, deep queue", 15_000_000, 2.5e6, 26, 100 * time.Millisecond},
		{"20 Mbit/s at 15, 5 ms queue", 15_000_000, 2.5e6, 26, 5 * time.Millisecond},
		{"105 Mbit/s at 80, 50 ms queue", 80_000_000, 13.2e6, 100, 50 * time.Millisecond},
		{"163 Mbit/s at 80, 3 ms queue", 80_000_000, 20.4e6, 100, 3 * time.Millisecond},
	} {
		for _, j := range []time.Duration{time.Millisecond, 3 * time.Millisecond} {
			run := func(on bool) (burstFrames int, est int64, p *burstPath) {
				p = newBurstPath(t, 10*time.Millisecond, c.bps, c.capacity)
				p.jitter, p.limit = j, c.limit
				p.m.SetBursts(on)
				for range 60 * 60 {
					if s := p.m.Stats(); s.BurstRate > s.PacingRate {
						burstFrames++
					}
					p.frame(c.pkts, nil)
					est = max(est, p.m.Stats().BurstEstimate)
				}
				return burstFrames, est, p
			}
			n, est, p := run(true)
			_, _, off := run(false)
			bo := p.m.Stats().BurstBackoffs
			t.Logf("%s, ACK jitter %v, 60 s: %d frames burst, highest estimate %.1f Mbit/s, %d packets dropped (%d without bursts), %d backoffs",
				c.name, j, n, float64(est)/1e6, p.dropped, off.dropped, bo)
			if n > 36 || bo > 4 || p.dropped > off.dropped+25*int(bo) {
				t.Errorf("%s, ACK jitter %v: %d of 3600 frames burst, %d packets dropped against %d without bursts, %d backoffs; want at most 36, at most 25 more a backoff, at most 4 (holds of 5, 10, 20, 40 s)",
					c.name, j, n, p.dropped, off.dropped, bo)
			}
		}
	}
	p := newBurstPath(t, 10*time.Millisecond, 80_000_000, 125e6)
	p.jitter = time.Millisecond
	if burst, _ := p.frames(60*5, 100); burst < 4*p.m.PacingRate() {
		t.Errorf("gigabit path, ACK jitter 1 ms: highest burst %.1f Mbit/s, want bursts", float64(burst)/1e6)
	}
}

// TestMediaBurstProbe: on a path not burstMinGain times faster than the
// pacing rate nothing goes faster than the pacing rate allows: a probe a
// second is the pacer's own clump at its start, measured; a path that got
// faster is burst into after the next two probes.
func TestMediaBurstProbe(t *testing.T) {
	// 150 Mbit/s against 96 Mbit/s of pacing: a 120 kB frame takes 8 ms
	// at the pacing rate after its 24 kB clump.
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 18.75e6)
	p.frames(60, 100)
	for range 120 {
		if took := p.frame(100, nil); took < 7*time.Millisecond {
			t.Fatalf("a frame took %v on a path not twice the pacing rate, want the pacing rate's 8 ms", took)
		}
	}
	if est := float64(p.m.Stats().BurstEstimate); math.Abs(est/150e6-1) > 0.2 {
		t.Fatalf("estimate %.1f Mbit/s on a 150 Mbit/s path", est/1e6)
	}
	// The path gets faster (a gigabit): the next probes measure it.
	p.capacity = 125e6
	p.frames(60*3, 100)
	if pc := p.m.Pacing(); pc.Burst != maxBurstRate*8 {
		t.Fatalf("burst %d bit/s 3 s after the path got faster, want %d", pc.Burst, int64(maxBurstRate*8))
	}
}

// TestMediaBurstStart: at a session's start, on a shallow bottleneck only
// 1.5 times the pacing rate, nothing overflows: the key frame (SkipProbe)
// goes at the pacing rate, the probes are the pacer's own clump. Before:
// every frame burst at twice the pacing rate until the first measure, and
// the first key frame overflowed such a queue at once.
func TestMediaBurstStart(t *testing.T) {
	// 80 Mbit/s: pacing 96 Mbit/s; the path 144 Mbit/s with a 3 ms queue
	// (54 kB); a 300 kB key frame, then 100 kB frames.
	p := newBurstPath(t, 10*time.Millisecond, 80_000_000, 18e6)
	p.limit = 3 * time.Millisecond
	p.m.SkipProbe()
	p.frame(250, nil)
	p.frames(60*5, 83)
	if s := p.m.Stats(); p.dropped > 0 || s.BurstBackoffs > 0 || s.BurstRate != s.PacingRate {
		t.Fatalf("first 5 s: %d packets dropped, %d backoffs, burst %d bit/s; want none, none, the pacing rate %d",
			p.dropped, s.BurstBackoffs, s.BurstRate, s.PacingRate)
	}
}

// TestMediaBurstGrowth: a train whose ACKs came burstGrowthMax later than
// its sending allows outran the path: the estimate is its delivery rate at
// once, not the median of trains measured before.
func TestMediaBurstGrowth(t *testing.T) {
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	p.frames(60*4, 100)
	if pc := p.m.Pacing(); pc.Burst != maxBurstRate*8 {
		t.Fatalf("burst %d bit/s on a gigabit path", pc.Burst)
	}
	p.capacity = 31.25e6 // 250 Mbit/s: a 120 kB frame queues 2.9 ms at 1 Gbit/s
	p.frames(3, 100)
	if pc := p.m.Pacing(); float64(pc.Burst) > 1.3*250e6 || pc.Burst < 2*pc.Rate {
		t.Fatalf("burst %d bit/s three frames after the path became 250 Mbit/s, want about 1.25 x 250 Mbit/s", pc.Burst)
	}
}

// TestMediaBurstLoss: a train that loses burstLossShare of its packets (a
// queue its burst overflowed) turns bursts off for burstHold, twice as long
// when it happens again soon after, and the path is measured again from
// probes after it; random losses do not.
func TestMediaBurstLoss(t *testing.T) {
	p := newBurstPath(t, 2*time.Millisecond, 80_000_000, 125e6)
	for range 60 * 4 {
		p.frame(100, func(i int) bool { return i == 50 }) // 1 %: random loss
	}
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts off after random losses")
	}
	p.frame(100, func(i int) bool { return i >= 90 }) // the burst's tail dropped
	p.frame(100, nil)
	if pc, s := p.m.Pacing(), p.m.Stats(); pc.Burst != pc.Rate || s.BurstBackoffs != 1 || s.BurstEstimate != 0 {
		t.Fatalf("after a burst lost its tail: burst %d, pacing %d, backoffs %d, estimate %d; want bursts off, one backoff, no estimate",
			pc.Burst, pc.Rate, s.BurstBackoffs, s.BurstEstimate)
	}
	p.frames(60*4+50, 100)
	if pc := p.m.Pacing(); pc.Burst != pc.Rate {
		t.Fatal("bursts back within the 5 s hold")
	}
	p.frames(60*3, 100)
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts still off 3 s after the hold")
	}
	p.frame(100, func(i int) bool { return i >= 90 })
	p.frames(60*9+50, 100)
	if pc := p.m.Pacing(); pc.Burst != pc.Rate {
		t.Fatal("a second overflow soon after the first: bursts back within 10 s")
	}
	p.frames(60*3, 100)
	if pc := p.m.Pacing(); pc.Burst == pc.Rate {
		t.Fatal("bursts still off 3 s after the second overflow's 10 s hold")
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
