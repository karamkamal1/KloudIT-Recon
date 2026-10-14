package host

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The second hardware run (docs/VENDOR_NOTES.md "Second hardware run:
// pacing, rate control and smoothness"): an AMF helper streaming a game on
// the gateway's UDP relay inside the owner's LAN. Its numbers, which the
// scenarios below are built from: HEVC 1080p60, setting 250 Mbit/s, the
// encoder making 40-65 Mbit/s whatever the target (frames of 125-170 KB),
// encode p50 2.8 / p95 9.5 ms (the game keeps the GPU busy: frames leave the
// encoder unevenly), network (send -> first byte) p50 6-9 ms, transfer
// (first -> last byte) 11.8 / 24.5 ms in game but 0.1 ms p50 / ~20 ms p95 for
// a still desktop's small frames (the client's worker, not the path: the
// browser's network stack acknowledged them on time), 0.1-0.4 % loss, and
// delay decreases at targets 3-15 times what the encoder made.

// clientStalls is a client worker's busy schedule (rsCfg.clientStall): short
// stalls of 8-25 ms about every 250 ms, and one in twenty 40-100 ms.
func clientStalls(seed uint64, dur time.Duration) func(time.Duration) time.Duration {
	rng := rand.New(rand.NewPCG(seed, 11))
	var iv [][2]time.Duration
	for t := time.Duration(0); t < dur; {
		t += time.Duration(rng.ExpFloat64() * float64(250*time.Millisecond))
		d := 8*time.Millisecond + time.Duration(rng.Float64()*float64(17*time.Millisecond))
		if rng.Float64() < 0.05 {
			d = 40*time.Millisecond + time.Duration(rng.Float64()*float64(60*time.Millisecond))
		}
		iv = append(iv, [2]time.Duration{t, t + d})
		t += d
	}
	return func(t time.Duration) time.Duration {
		i := sort.Search(len(iv), func(i int) bool { return iv[i][1] > t })
		if i < len(iv) && iv[i][0] <= t {
			return iv[i][1]
		}
		return 0
	}
}

// lanRelay is the second hardware run's session: the relay within a LAN
// (gigabit, ~3 ms round trip), a 250 Mbit/s setting, a game the encoder
// makes 60 Mbit/s of at most, frames leaving the encoder unevenly.
func lanRelay(name string, seed uint64) rsCfg {
	c := relaySim(name, seed)
	c.ceiling, c.satKbps = 250000, 60000
	c.owd, c.jitter = 1500*time.Microsecond, time.Millisecond
	c.capacity = func(time.Duration) float64 { return 940e6 }
	c.encJitter = 8 * time.Millisecond
	return c
}

// lanStats is what a LAN run is judged by, over a time span: the overlay's
// stages (p50, p95) of the frames captured in it that were decoded, the
// video the client decoded, and what the rate control did.
type lanStats struct {
	e2e, queue, hold, network, transfer, owd [2]time.Duration
	mbps, fps                                float64 // decoded video
	target, tmin, tmax                       float64 // the encoder's target: mean, lowest, highest (kbps)
	dec                                      int
	thinStarts, thinned, holds               int
}

func (s lanStats) String() string {
	ms := func(d [2]time.Duration) string {
		return fmt.Sprintf("%.1f/%.1f", float64(d[0])/1e6, float64(d[1])/1e6)
	}
	return fmt.Sprintf("e2e %s, host queue %s, window hold %s, network %s, transfer %s, one-way %s ms (p50/p95); decoded %.1f Mbit/s at %.1f fps; "+
		"target mean %.0f, range %.0f-%.0f kbps; %d decreases, %d thinning episodes (%d frames), %d frames held",
		ms(s.e2e), ms(s.queue), ms(s.hold), ms(s.network), ms(s.transfer), ms(s.owd), s.mbps, s.fps, s.target, s.tmin, s.tmax, s.dec,
		s.thinStarts, s.thinned, s.holds)
}

func (tr *rsTrace) lan(from, to time.Duration) lanStats {
	var s lanStats
	var e2e, q, h, nw, tf, owd []time.Duration
	bytes := 0
	for _, f := range tr.frames {
		if f.capture < from || f.capture >= to {
			continue
		}
		if f.status == "thinned" {
			s.thinned++
		}
		if f.decoded == 0 {
			continue
		}
		e2e = append(e2e, f.decoded+time.Millisecond-f.capture)
		q = append(q, f.taken-f.encDone)
		h = append(h, f.sendAt-f.taken)
		nw = append(nw, f.first-f.sendAt)
		tf = append(tf, f.recv-f.first)
		owd = append(owd, f.recv-f.encDone)
		if f.sendAt-f.taken > time.Millisecond {
			s.holds++
		}
		bytes += f.bytes
	}
	pct := func(v []time.Duration) [2]time.Duration {
		if len(v) == 0 {
			return [2]time.Duration{}
		}
		slices.Sort(v)
		return [2]time.Duration{v[len(v)/2], v[min(len(v)-1, len(v)*95/100)]}
	}
	s.e2e, s.queue, s.hold, s.network, s.transfer, s.owd = pct(e2e), pct(q), pct(h), pct(nw), pct(tf), pct(owd)
	secs := (to - from).Seconds()
	s.mbps, s.fps = float64(bytes)*8/secs/1e6, float64(len(e2e))/secs
	s.tmin = -1
	n := 0
	for i, a := range tr.applied {
		if at := time.Duration(i+1) * rateTick; at >= from && at < to {
			s.target += float64(a)
			n++
			if s.tmin < 0 || float64(a) < s.tmin {
				s.tmin = float64(a)
			}
			s.tmax = max(s.tmax, float64(a))
		}
	}
	s.target /= float64(max(1, n))
	for _, at := range tr.downAt {
		if at >= from && at < to {
			s.dec++
		}
	}
	for _, l := range tr.lines {
		if f := strings.Fields(l); len(f) > 2 && f[1] == "thinning" && f[2] == "starts" {
			if t, err := strconv.ParseFloat(f[0], 64); err == nil && time.Duration(t*float64(time.Second)) >= from && time.Duration(t*float64(time.Second)) < to {
				s.thinStarts++
			}
		}
	}
	return s
}

// TestRelaySimLAN runs the second hardware run's session (lanRelay) and
// judges its stages, rate and smoothness over the last 30 s (and a capacity
// drop's dip). The numbers before this test's changes (bursts, the
// confirmed delay, the window's backlog test, thinning a queue only behind a
// late frame, the decrease from what the encoder makes) are in
// docs/VENDOR_NOTES.md "Second hardware run: pacing, rate control and
// smoothness"; each bound names what it held then.
func TestRelaySimLAN(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		name  string
		set   func(*rsCfg)
		check func(tr *rsTrace, cfg rsCfg, st lanStats) string
	}{
		// The in-game state of the log's sessions: a target near what the
		// encoder makes (60 of 80 Mbit/s): paced at 1.2 x the target a
		// frame took 8 ms p50, 19 ms p95 to send, end to end 20/31 ms.
		{"wired LAN, 80 Mbit/s", func(c *rsCfg) { c.ceiling = 80000 }, func(tr *rsTrace, cfg rsCfg, st lanStats) string {
			if st.transfer[0] > 3*ms || st.e2e[1] > 20*ms || st.dec > 0 || st.thinStarts > 0 {
				return "want transfer p50 under 3 ms, end-to-end p95 under 20 ms (8.2 and 31 ms paced at 1.2 x), no decrease, no thinning"
			}
			return ""
		}},
		// The log's sessions: the 250 Mbit/s setting, a client whose worker
		// stalls now and then.
		{"wired LAN, client stalls", func(c *rsCfg) { c.clientStall = clientStalls(c.seed, c.dur) }, steadyLAN(15*ms, 25*ms, 0)},
		// The same over Wi-Fi: less capacity, more jitter, and the link's
		// own stalls (wifiPauses: the log's one-way delay maxima of
		// 70-300 ms): 227-262 frames held by the window, 7-11 thinning
		// episodes before.
		{"Wi-Fi LAN, client stalls", func(c *rsCfg) {
			c.clientStall, c.pause = clientStalls(c.seed, c.dur), wifiPauses
			c.capacity = func(time.Duration) float64 { return 300e6 }
			c.owd, c.jitter, c.capQueue = 2*time.Millisecond, 4*time.Millisecond, 200*time.Millisecond
		}, steadyLAN(20*ms, 40*ms, 5)},
		// The same from the log's in-game target (backed off to 80 Mbit/s
		// from the 250 setting).
		{"Wi-Fi LAN, client stalls, from 80 Mbit/s", func(c *rsCfg) {
			c.clientStall, c.pause = clientStalls(c.seed, c.dur), wifiPauses
			c.capacity = func(time.Duration) float64 { return 300e6 }
			c.owd, c.jitter, c.capQueue = 2*time.Millisecond, 4*time.Millisecond, 200*time.Millisecond
			c.startKbps = 80000
		}, steadyLAN(20*ms, 40*ms, 5)},
		{"wired LAN, client stalls, from 80 Mbit/s", func(c *rsCfg) {
			c.clientStall, c.startKbps = clientStalls(c.seed, c.dur), 80000
		}, steadyLAN(15*ms, 25*ms, 0)},
		// The second hardware run's game sessions as the log has them: the
		// frames took 11-12 ms from first to last byte whatever the pacing
		// (NMLlzRSB at the 250 Mbit/s setting, 104-143 KB frames, nothing
		// lowered: 300 Mbit/s of pacing), ~75-95 Mbit/s carried: a
		// 100 Mbit/s link somewhere on the way (an inference; HARDWARE_TEST_PLAN
		// 8.1c measures it), a 100 ms queue at it. Each frame paced at 1.2 x
		// 250 Mbit/s queues ~8-10 ms there. Before: 5-7 delay decreases a
		// run, the target 176-234 Mbit/s on average over the last 30 s,
		// 776-859 frames held by the window and 10-16 thinning episodes in
		// that time, end to end 24.9-25.5 / 36.7-37.8 ms (25.1-25.8 /
		// 40.0-40.2 with the client's stalls): the pumping and the judder of
		// the log. Its first second (the 2.5 MB key frame of a 250 Mbit/s
		// start into the link's 1.2 MB queue: a loss and an overflow
		// decrease) is the same before and after.
		{"100 Mbit/s link", func(c *rsCfg) {
			c.capacity, c.capQueue = func(time.Duration) float64 { return 95e6 }, 100*time.Millisecond
		}, linkLAN(1, 24*ms, 37*ms)},
		{"100 Mbit/s link, client stalls", func(c *rsCfg) {
			c.capacity, c.capQueue = func(time.Duration) float64 { return 95e6 }, 100*time.Millisecond
			c.clientStall = clientStalls(c.seed, c.dur)
		}, linkLAN(1, 24*ms, 40*ms)},
		// The same at 150 Mbit/s: before, one delay decrease (seed 1), 202-238
		// frames held, end to end 18.7-18.9 / 28.7-29.6 ms.
		{"150 Mbit/s link, client stalls", func(c *rsCfg) {
			c.capacity, c.capQueue = func(time.Duration) float64 { return 150e6 }, 100*time.Millisecond
			c.clientStall = clientStalls(c.seed, c.dur)
		}, linkLAN(0, 19*ms, 31*ms)},
		// Real congestion still backs off: the LAN's capacity drops to
		// 40 Mbit/s for 10 s (Wi-Fi with a neighbour's download), a 100 ms
		// queue at the access point. Before: the first cut 85-130 ms after
		// the drop, in the dip end to end 27-44 ms p50, 51-129 ms p95, the
		// target 35-47 Mbit/s on average (the encoder made 60 at 250: the
		// decreases from the target took 5-6 steps to bite).
		{"LAN capacity drop to 40 Mbit/s", func(c *rsCfg) {
			c.clientStall, c.capQueue = clientStalls(c.seed, c.dur), 100*time.Millisecond
			c.capacity = func(t time.Duration) float64 {
				if t >= 15*time.Second && t < 25*time.Second {
					return 40e6
				}
				return 940e6
			}
		}, func(tr *rsTrace, cfg rsCfg, st lanStats) string {
			first := time.Duration(-1)
			for _, at := range tr.downAt {
				if at >= 15*time.Second {
					first = at - 15*time.Second
					break
				}
			}
			dip, after := tr.lan(16*time.Second, 25*time.Second), tr.lan(35*time.Second, cfg.dur)
			tr.lines = append(tr.lines, fmt.Sprintf("first cut %v after the drop; in the dip (16-25 s): %v; 35-40 s: %v", first, dip, after))
			// Back: the increases' climb from the dip's last decrease (the
			// path's queue still decides those, a second or so before the
			// capacity returns or right at it) reaches 80 % of the setting
			// by the end.
			if first < 0 || first > 500*ms || dip.target > 40000 || dip.e2e[1] > 100*ms || dip.fps < 50 || after.tmax < 0.8*float64(cfg.ceiling) {
				return fmt.Sprintf("first cut %v after the drop, in the dip a target of %.0f, end-to-end p95 %v at %.1f fps, at the end a target up to %.0f: "+
					"want a cut within 500 ms, the target under the 40 Mbit/s, the p95 under 100 ms at 50 fps or more, back over 80 %% of the setting",
					first, dip.target, dip.e2e[1], dip.fps, after.tmax)
			}
			return ""
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 2; seed++ {
				cfg := lanRelay(c.name, seed)
				c.set(&cfg)
				tr := runRelaySim(cfg)
				st := tr.lan(10*time.Second, cfg.dur)
				fail := c.check(tr, cfg, st)
				t.Logf("%s seed %d, last 30 s: %v; decreases %v, overflows %d", c.name, seed, st, tr.decreases, tr.overflows)
				if n := len(tr.lines); strings.HasPrefix(tr.lines[n-1], "first cut") {
					t.Logf("%s seed %d: %s", c.name, seed, tr.lines[n-1])
				}
				if fail != "" {
					t.Errorf("seed %d: %s\n%s", seed, fail, tr.report())
				}
			}
		})
	}
}

// linkLAN judges a LAN behind a link the frames' bursts queue at but the
// stream fits in: at most delays decreases for a delay (the whole run: the
// pumping), the target over 80 % of the setting in the last 30 s, end to
// end under e2e50 and e2e95, at most 3 thinning episodes and 40 frames held
// by the window.
func linkLAN(delays int, e2e50, e2e95 time.Duration) func(*rsTrace, rsCfg, lanStats) string {
	return func(tr *rsTrace, cfg rsCfg, st lanStats) string {
		n := 0
		for _, l := range tr.lines {
			if strings.Contains(l, "RATE down") && strings.Contains(l, "why=delay") {
				n++
			}
		}
		if n > delays || st.target < 0.8*float64(cfg.ceiling) || st.e2e[0] > e2e50 || st.e2e[1] > e2e95 || st.thinStarts > 3 || st.holds > 40 {
			return fmt.Sprintf("%d delay decreases, want at most %d; want the target over 80 %% of the setting, end to end under %v/%v, at most 3 thinning episodes and 40 frames held",
				n, delays, e2e50, e2e95)
		}
		return ""
	}
}

// steadyLAN judges a LAN whose path carries the stream (the client's own
// stalls aside): no decrease, the setting kept, end to end under e2e50 and
// e2e95 (p50, p95), at most thin thinning episodes and a few frames held by
// the window.
func steadyLAN(e2e50, e2e95 time.Duration, thin int) func(*rsTrace, rsCfg, lanStats) string {
	return func(tr *rsTrace, cfg rsCfg, st lanStats) string {
		if st.dec > 0 || st.target < 0.95*float64(cfg.ceiling) || st.e2e[0] > e2e50 || st.e2e[1] > e2e95 || st.thinStarts > thin || st.holds > 20 {
			return fmt.Sprintf("want no decrease, the target at the setting, end to end under %v/%v, at most %d thinning episodes and 20 frames held",
				e2e50, e2e95, thin)
		}
		return ""
	}
}
