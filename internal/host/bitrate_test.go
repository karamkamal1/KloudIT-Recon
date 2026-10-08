package host

import (
	"testing"
	"time"
)

// rateHarness drives a rateController on a fake clock the way a session does:
// a frame acknowledged every frame interval, an evaluation every rateTick.
type rateHarness struct {
	t     *testing.T
	clock time.Time
	r     *rateController
	start time.Time
}

func newRateHarness(t *testing.T, ceiling int) *rateHarness {
	h := &rateHarness{t: t, clock: time.Unix(1_000_000, 0)}
	h.start = h.clock
	h.r = &rateController{now: func() time.Time { return h.clock }}
	if got := h.r.target(ceiling); got != ceiling {
		t.Fatalf("first generation at %d kbps, want the ceiling %d", got, ceiling)
	}
	return h
}

// raise is a bitrate change by tick, at the time since the harness started.
type raise struct {
	at       time.Duration
	from, to int
}

// run advances the clock by d: a frame sent and acknowledged every 1/60 s
// with the one-way delay owd(t) (nil: a still desktop, nothing sent), t since
// the start of the harness, and a tick every rateTick. It returns the raises.
func (h *rateHarness) run(d time.Duration, owd func(t time.Duration) time.Duration) []raise {
	return h.frames(d, owd != nil, owd)
}

// stalled advances the clock by d with a frame sent every 1/60 s and none
// acknowledged (a stalled path, or a client that never acknowledges).
func (h *rateHarness) stalled(d time.Duration) []raise { return h.frames(d, true, nil) }

func (h *rateHarness) frames(d time.Duration, send bool, owd func(t time.Duration) time.Duration) []raise {
	const frame = time.Second / 60
	var out []raise
	end := h.clock.Add(d)
	nextTick := h.clock.Add(rateTick)
	for h.clock.Before(end) {
		h.clock = h.clock.Add(frame)
		if send {
			h.r.sent()
		}
		if owd != nil {
			h.r.ack(owd(h.clock.Sub(h.start)))
		}
		if !h.clock.Before(nextTick) {
			nextTick = nextTick.Add(rateTick)
			if from, to, ok := h.r.tick(); ok {
				out = append(out, raise{h.clock.Sub(h.start), from, to})
			}
		}
	}
	return out
}

// wait advances the clock by d without frames or ticks.
func (h *rateHarness) wait(d time.Duration) { h.clock = h.clock.Add(d) }

// cut sends a congestion signal: an emergency (a queue overflow) or a delay
// report.
func (h *rateHarness) cut(emergency bool, wantFrom, wantTo int, wantOK bool) {
	h.t.Helper()
	sig := signalDelay
	if emergency {
		sig = signalOverflow
	}
	from, to, ok := h.r.congestion(sig)
	if ok != wantOK || (ok && (from != wantFrom || to != wantTo)) {
		h.t.Fatalf("at %v: congestion(emergency %v) = %d -> %d, %v; want %d -> %d, %v",
			h.clock.Sub(h.start), emergency, from, to, ok, wantFrom, wantTo, wantOK)
	}
}

func (h *rateHarness) cur() int {
	cur, _ := h.r.kbps()
	return cur
}

const steadyOWD = 20 * time.Millisecond

func steady(time.Duration) time.Duration { return steadyOWD }

// TestRateRecovers: after a cut, 10 s without a congestion signal and with
// the one-way delay at its minimum raise the bitrate by 15 %, then again
// every 10 s, up to the ceiling and no further.
func TestRateRecovers(t *testing.T) {
	h := newRateHarness(t, 20000)
	h.run(5*time.Second, steady)
	h.cut(false, 20000, 15000, true)
	cutAt := h.clock.Sub(h.start)
	if r := h.run(9500*time.Millisecond, steady); len(r) != 0 {
		t.Fatalf("raised within 10 s of the cut: %+v", r)
	}
	got := h.run(60*time.Second, steady)
	want := []raise{{10 * time.Second, 15000, 17250}, {20 * time.Second, 17250, 19837}, {30 * time.Second, 19837, 20000}}
	if len(got) != len(want) {
		t.Fatalf("raises %+v, want %d", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		// A raise comes with the first tick after it is due: each up to one
		// tick (plus the frame the tick falls on) after the previous one.
		if late := g.at - cutAt - w.at; g.from != w.from || g.to != w.to || late < 0 || late > time.Duration(i+1)*(rateTick+time.Second/60) {
			t.Fatalf("raise %d: %d -> %d at cut + %v, want %d -> %d at cut + %v", i, g.from, g.to, g.at-cutAt, w.from, w.to, w.at)
		}
	}
	if h.cur() != 20000 {
		t.Fatalf("target %d, want the ceiling 20000", h.cur())
	}
	// A restart (key frame, encoder failure) stays at the target.
	if got := h.r.target(20000); got != 20000 {
		t.Fatalf("restart at %d kbps", got)
	}
}

// TestRateHighDelayHolds: while the one-way delay stays more than 10 ms over
// its 2 s minimum (a queue builds), nothing is raised; the quiet period
// starts when the delay is back down. A lone late frame (a key frame) does
// not count: the test is the median of the frames since the last tick.
func TestRateHighDelayHolds(t *testing.T) {
	h := newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	cutAt := h.clock.Sub(h.start)
	// The delay climbs 10 ms per second for 30 s (a queue builds, the 2 s
	// minimum trails 20 ms behind), then falls back to the base.
	ramp := func(t time.Duration) time.Duration {
		if t -= cutAt; t < 30*time.Second {
			return steadyOWD + t/100
		}
		return steadyOWD
	}
	if r := h.run(30*time.Second, ramp); len(r) != 0 {
		t.Fatalf("raised while the delay grew: %+v", r)
	}
	r := h.run(15*time.Second, ramp)
	if len(r) != 1 || r[0].at-cutAt < 40*time.Second || r[0].at-cutAt > 40*time.Second+rateTick || r[0].to != 17250 {
		t.Fatalf("raises %+v, want 15000 -> 17250 10 s after the delay went down (cut + 40 s)", r)
	}

	// A full bottleneck: the queue fills in 3 s (15 ms per second), drops
	// and fills again.
	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	saw := func(t time.Duration) time.Duration { return steadyOWD + (t%(3*time.Second))*15/1000 }
	if r := h.run(30*time.Second, saw); len(r) != 0 {
		t.Fatalf("raised on a full bottleneck: %+v", r)
	}

	// Jitter within 10 ms and one key frame 80 ms late every 2 s.
	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	n := 0
	jitter := func(time.Duration) time.Duration {
		n++
		if n%120 == 0 {
			return steadyOWD + 80*time.Millisecond
		}
		return steadyOWD + time.Duration(n%7)*time.Millisecond
	}
	if r := h.run(10*time.Second+rateTick, jitter); len(r) != 1 || r[0].to != 17250 {
		t.Fatalf("jitter and lone late frames held the bitrate: raises %+v", r)
	}
}

// TestRateCeiling: the bitrate never goes above the ceiling (the settings'
// bitrate); a lower ceiling caps the target at once.
func TestRateCeiling(t *testing.T) {
	h := newRateHarness(t, 3000)
	if r := h.run(30*time.Second, steady); len(r) != 0 {
		t.Fatalf("raised above the ceiling: %+v", r)
	}
	h.cut(false, 3000, 2250, true)
	r := h.run(32*time.Second, steady)
	if len(r) != 3 || r[0].to != 2587 || r[1].to != 2975 || r[2].to != 3000 {
		t.Fatalf("raises %+v, want 2250 -> 2587 -> 2975 -> 3000 (the ceiling, not 3421)", r)
	}
	if r := h.run(30*time.Second, steady); len(r) != 0 || h.cur() != 3000 {
		t.Fatalf("raises %+v at the ceiling, target %d", r, h.cur())
	}
	if got := h.r.target(2500); got != 2500 {
		t.Fatalf("target %d under a 2500 kbps ceiling", got)
	}
}

// TestRateLimit: at most one change per 10 s. A delay report within 10 s of
// a cut or a raise cuts nothing but holds off the next raise; emergencies
// (queue overflow, decoder flush) cut within 10 s, but at most every 2 s.
func TestRateLimit(t *testing.T) {
	h := newRateHarness(t, 40000)
	h.cut(false, 40000, 30000, true)
	h.run(5*time.Second, steady)
	h.cut(false, 0, 0, false) // 5 s after the cut
	if h.cur() != 30000 {
		t.Fatalf("target %d after a refused cut", h.cur())
	}
	// The refused report restarted the quiet period: no raise at cut + 10 s.
	r := h.run(10*time.Second, steady)
	if len(r) != 1 || r[0].at < 15*time.Second || r[0].to != 34500 {
		t.Fatalf("raises %+v, want one 10 s after the refused report", r)
	}
	h.run(3*time.Second, steady)
	h.cut(false, 0, 0, false) // 3 s after the raise
	h.run(7*time.Second, steady)
	h.cut(false, 34500, 25875, true) // 10 s after the raise

	// Emergencies: within 10 s of the delay cut, but 2 s after any cut.
	h.run(time.Second, steady)
	h.cut(true, 0, 0, false)
	h.run(time.Second, steady)
	h.cut(true, 25875, 19406, true)
	h.run(1500*time.Millisecond, steady)
	h.cut(true, 0, 0, false)
	h.run(500*time.Millisecond, steady)
	h.cut(true, 19406, 14554, true)
	// The emergency counts as a change: a delay report 5 s later cuts nothing.
	h.run(5*time.Second, steady)
	h.cut(false, 0, 0, false)

	// Raises at least 10 s apart even with the quiet period long over.
	h = newRateHarness(t, 100000)
	h.cut(false, 100000, 75000, true)
	h.wait(2 * time.Second)
	h.cut(true, 75000, 56250, true)
	h.wait(2 * time.Second)
	h.cut(true, 56250, 42187, true)
	r = h.run(45*time.Second, steady)
	if len(r) != 4 {
		t.Fatalf("raises %+v, want 4 in 45 s", r)
	}
	for i := 1; i < len(r); i++ {
		if d := r[i].at - r[i-1].at; d < 10*time.Second {
			t.Fatalf("raises %d and %d %v apart", i-1, i, d)
		}
	}
}

// TestRateSeamlessGap: on an encoder qualified to change its bitrate
// seamlessly (setGap(rateSeamlessGap)) changes may follow each other after
// 2 s instead of 10 s; the quiet period before the first raise stays 10 s,
// emergencies keep their own 2 s, and a gap above the period (a shortened
// test period) is the period.
func TestRateSeamlessGap(t *testing.T) {
	h := newRateHarness(t, 40000)
	h.r.setGap(rateSeamlessGap)
	h.cut(false, 40000, 30000, true)
	h.wait(1500 * time.Millisecond)
	h.cut(false, 0, 0, false) // 1.5 s after the cut
	h.wait(2 * time.Second)
	h.cut(false, 30000, 22500, true) // 3.5 s: the 2 s gap is over
	cutAt := h.clock.Sub(h.start)
	r := h.run(20*time.Second, steady)
	if len(r) < 3 || r[0].from != 22500 || r[0].at-cutAt < 10*time.Second {
		t.Fatalf("raises %+v: the first 10 s after the last cut", r)
	}
	for i := 1; i < len(r); i++ {
		if d := r[i].at - r[i-1].at; d < rateSeamlessGap || d > rateSeamlessGap+rateTick+time.Second/60 {
			t.Fatalf("raises %d and %d %v apart, want 2 s", i-1, i, d)
		}
	}
	if h.cur() != 40000 {
		t.Fatalf("target %d, want back at the ceiling 40000", h.cur())
	}
	// Back to the full period (a flushing encoder, FFmpeg): 10 s again.
	h.r.setGap(0)
	h.wait(10 * time.Second)
	h.cut(false, 40000, 30000, true)
	h.wait(3 * time.Second)
	h.cut(false, 0, 0, false)
	// A gap longer than a shortened period is the period.
	r2 := &rateController{now: func() time.Time { return h.clock }, period: time.Second}
	r2.setGap(rateSeamlessGap)
	if g := r2.changeGap(); g != time.Second {
		t.Fatalf("gap %v with a 1 s period", g)
	}
}

// TestRateDecrease: cuts are 25 %, stop at 2 Mbit/s (or at a lower ceiling),
// and need a generation; a settings change (reset) drops the back-off.
func TestRateDecrease(t *testing.T) {
	r := &rateController{}
	if _, _, ok := r.congestion(signalOverflow); ok {
		t.Fatal("cut before the first generation")
	}
	h := newRateHarness(t, 4000)
	h.cut(false, 4000, 3000, true)
	h.wait(10 * time.Second)
	h.cut(true, 3000, 2250, true)
	h.wait(10 * time.Second)
	h.cut(false, 2250, 2000, true) // the floor, not 1687
	h.wait(10 * time.Second)
	h.cut(false, 0, 0, false)
	h.cut(true, 0, 0, false)

	h = newRateHarness(t, 1500) // a ceiling below the floor: nothing to cut
	h.cut(true, 0, 0, false)
	h.wait(10 * time.Second)
	h.cut(false, 0, 0, false)

	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	h.r.reset()
	if got := h.r.target(25000); got != 25000 {
		t.Fatalf("after a settings change: %d kbps, want the new setting 25000", got)
	}
	h.cut(false, 25000, 18750, true) // no rate limit left from before the reset
}

// TestRateQuietWithoutAcks: ticks with nothing sent (a still desktop), and
// frames that a client which never acknowledges (no clock sync, a v1 client)
// was sent, neither break nor prove the quiet; a pause (hold) restarts it.
func TestRateQuietWithoutAcks(t *testing.T) {
	h := newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	if r := h.stalled(10*time.Second + rateTick); len(r) != 1 || r[0].to != 17250 {
		t.Fatalf("raises %+v for a client that never acknowledges, want 15000 -> 17250", r)
	}
	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	if r := h.run(10*time.Second+rateTick, nil); len(r) != 1 || r[0].to != 17250 {
		t.Fatalf("raises %+v without acknowledgements, want 15000 -> 17250", r)
	}
	h.run(6*time.Second, nil)
	h.r.hold()
	if r := h.run(9*time.Second, nil); len(r) != 0 {
		t.Fatalf("raised within 10 s of a pause: %+v", r)
	}
	if r := h.run(2*time.Second, nil); len(r) != 1 {
		t.Fatalf("raises %+v, want one 10 s after the pause", r)
	}
	// The kept acknowledgements stay bounded however fast they come.
	for range 3 * owdMaxSamples {
		h.r.ack(steadyOWD)
	}
	if n := len(h.r.owd); n > owdMaxSamples {
		t.Fatalf("%d acknowledgements kept", n)
	}
}

// TestRateStalledPath: frames go out but a client that acknowledged frames
// before acknowledges none (the gateway's leg to the client stalls; the
// gateway buffers the frames, so the host's queue does not overflow): no
// raise however long it lasts, and a delay report when it ends can still cut.
// Once acknowledgements resume, the raise needs a whole quiet period; a gap
// in the acknowledgements shorter than ackTimeout changes nothing.
func TestRateStalledPath(t *testing.T) {
	h := newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	h.run(8*time.Second, steady)
	if r := h.stalled(30 * time.Second); len(r) != 0 {
		t.Fatalf("raised while no frame was acknowledged: %+v", r)
	}
	resumed := h.clock.Sub(h.start)
	r := h.run(11*time.Second, steady)
	if len(r) != 1 || r[0].to != 17250 || r[0].at-resumed < 10*time.Second-ackTimeout-rateTick {
		t.Fatalf("raises %+v, want 15000 -> 17250 about 10 s after the acknowledgements resumed (at %v)", r, resumed)
	}

	// The stall covers the moment the raise is due; the delay report that
	// follows it cuts. (A stall that starts less than ackTimeout before the
	// raise is due does not stop that raise.)
	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	h.run(8*time.Second, steady)
	if r := h.stalled(5 * time.Second); len(r) != 0 {
		t.Fatalf("raised during the stall: %+v", r)
	}
	h.cut(false, 15000, 11250, true)

	// Acknowledgements 900 ms apart (lost datagrams) do not hold the bitrate
	// (each stretch has one tick, 500 ms in, so the raise comes up to one
	// stretch late).
	h = newRateHarness(t, 20000)
	h.cut(false, 20000, 15000, true)
	r = nil
	for range 12 {
		r = append(r, h.stalled(900*time.Millisecond-time.Second/60)...)
		r = append(r, h.run(time.Second/60, steady)...)
	}
	if len(r) != 1 || r[0].to != 17250 || r[0].at < 10*time.Second || r[0].at > 11*time.Second {
		t.Fatalf("raises %+v with acknowledgements 900 ms apart, want 15000 -> 17250 10 s after the cut", r)
	}
}

// TestRateDecoderCap: a cut for a client that flushed its decoder caps the
// raises at 85 % of the bitrate the decoder fell behind at, so a client that
// cannot decode the setting does not go through flush, cut and raise again
// and again; a lower flush lowers the cap, a settings change drops it.
func TestRateDecoderCap(t *testing.T) {
	h := newRateHarness(t, 20000)
	if from, to, ok := h.r.congestion(signalDecoder); !ok || from != 20000 || to != 15000 {
		t.Fatalf("decoder flush: %d -> %d, %v; want 20000 -> 15000", from, to, ok)
	}
	if lim := h.r.decoderLimit(); lim != 17000 {
		t.Fatalf("decoder limit %d, want 17000", lim)
	}
	r := h.run(40*time.Second, steady)
	if len(r) != 1 || r[0].from != 15000 || r[0].to != 17000 || h.cur() != 17000 {
		t.Fatalf("raises %+v, want 15000 -> 17000 (85 %% of 20000) and no more", r)
	}
	// It falls behind again at 17000: the cap goes down with it.
	if _, to, ok := h.r.congestion(signalDecoder); !ok || to != 12750 || h.r.decoderLimit() != 14450 {
		t.Fatalf("second flush: to %d (%v), limit %d; want 12750, 14450", to, ok, h.r.decoderLimit())
	}
	if r := h.run(40*time.Second, steady); len(r) != 1 || r[0].to != 14450 {
		t.Fatalf("raises %+v, want 12750 -> 14450", r)
	}
	// Overflows and delay reports cut without moving the cap.
	h.cut(true, 14450, 10837, true)
	if r := h.run(40*time.Second, steady); len(r) != 3 || r[2].to != 14450 || h.r.decoderLimit() != 14450 {
		t.Fatalf("raises %+v, limit %d; want 10837 -> 12462 -> 14331 -> 14450", r, h.r.decoderLimit())
	}
	h.r.reset()
	if got := h.r.target(20000); got != 20000 || h.r.decoderLimit() != 0 {
		t.Fatalf("after a settings change: %d kbps, limit %d; want 20000, none", got, h.r.decoderLimit())
	}
	// A flush that cuts nothing (2 s after a cut) sets no cap.
	h.cut(true, 20000, 15000, true)
	h.wait(time.Second)
	if _, _, ok := h.r.congestion(signalDecoder); ok || h.r.decoderLimit() != 0 {
		t.Fatalf("flush within 2 s of a cut: cut %v, limit %d", ok, h.r.decoderLimit())
	}
}
