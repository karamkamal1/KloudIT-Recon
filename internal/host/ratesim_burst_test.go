package host

import (
	"strings"
	"testing"
	"time"
)

// TestRelaySimBurstPaths: the frame bursts (cc.Media) on paths only 1.1-1.7
// times the pacing rate, with ACKs held up by 1-3 ms (in order) and deep or
// shallow bottleneck queues, judged against the same runs without bursts
// (rsCfg.noBursts): no decrease more, none in a session's first 5 s where
// the run without bursts has none. Before the review of the second hardware
// run's changes the bursts' delivery estimate took the highest per-ACK
// sample of 2 s and ratcheted up on such ACKs (131 Mbit/s on the 20 Mbit/s
// path), the frames burst at several times the bottleneck: 21 delay
// decreases in 6 seeds x 60 s on the 20 and 25 Mbit/s paths where the runs
// without bursts had none; and every frame of a path not measured yet went
// at twice the pacing rate: a loss decrease at 0.06 s and an overflow
// decrease at 0.17 s on the shallow 30 Mbit/s path, a loss decrease at
// 0.06 s on the 50 Mbit/s one, in every seed.
func TestRelaySimBurstPaths(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		name         string
		mbps         float64
		queue        time.Duration
		ceiling, sat int
		owd, jitter  time.Duration
	}{
		{"20 Mbit/s at 15, 100 ms queue, 3 ms jitter", 20, 100 * ms, 15000, 0, 15 * ms, 3 * ms},
		{"25 Mbit/s at 20, 50 ms queue, 3 ms jitter", 25, 50 * ms, 20000, 0, 8 * ms, 3 * ms},
		{"30 Mbit/s at 25, 5 ms queue, 2 ms jitter", 30, 5 * ms, 25000, 0, 10 * ms, 2 * ms},
		{"30 Mbit/s at 20, 10 ms queue, 2 ms jitter", 30, 10 * ms, 20000, 0, 10 * ms, 2 * ms},
		{"50 Mbit/s at 40, 4 ms queue, 2 ms jitter", 50, 4 * ms, 40000, 0, 8 * ms, 2 * ms},
		{"100 Mbit/s LAN at 80, 3 ms queue, 1 ms jitter", 100, 3 * ms, 80000, 60000, 1500 * time.Microsecond, ms},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 3; seed++ {
				var dec, early [2]int
				var drops [2]int
				for i, off := range []bool{false, true} {
					cfg := relaySim(c.name, seed)
					cfg.dur = 40 * time.Second
					cfg.ceiling, cfg.satKbps = c.ceiling, c.sat
					cfg.owd, cfg.jitter, cfg.capQueue = c.owd, c.jitter, c.queue
					capb := c.mbps * 1e6
					cfg.capacity = func(time.Duration) float64 { return capb }
					cfg.noBursts = off
					tr := runRelaySim(cfg)
					for _, at := range tr.downAt {
						dec[i]++
						if at < 5*time.Second {
							early[i]++
						}
					}
					drops[i] = tr.overflowed
					if !off {
						for _, l := range tr.lines {
							if strings.Contains(l, "RATE down") {
								t.Logf("seed %d: %s", seed, l)
							}
						}
						t.Logf("seed %d: burst estimate %.1f Mbit/s, burst %.1f Mbit/s at the end, %d backoffs", seed,
							float64(tr.media.BurstEstimate)/1e6, float64(tr.media.BurstRate)/1e6, tr.media.BurstBackoffs)
					}
				}
				t.Logf("seed %d: decreases %d (first 5 s %d), packets the queue dropped %d; without bursts %d (%d), %d",
					seed, dec[0], early[0], drops[0], dec[1], early[1], drops[1])
				if dec[0] > dec[1] || early[0] > early[1] {
					t.Errorf("seed %d: %d decreases, %d in the first 5 s; without bursts %d, %d", seed, dec[0], early[0], dec[1], early[1])
				}
			}
		})
	}
}
