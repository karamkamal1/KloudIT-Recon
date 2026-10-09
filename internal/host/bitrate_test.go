package host

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// ctlHarness drives a rateController on a fake clock the way a session does,
// with a perfect seamless encoder (a change is live at once and the encoder
// hits its target) and a client that receives every frame: a frame every
// 1/fps, a report every 25 ms with the frames' bytes and a queueing delay of
// qd(t), a tick every rateTick.
type ctlHarness struct {
	t       *testing.T
	clock   time.Time
	start   time.Time
	r       *rateController
	fps     int
	share   float64      // received share of the encoder's output (1: everything)
	fill    float64      // the encoder's output as a share of its target (1: hits it)
	stalled bool         // tick's stalled
	decodeQ int          // reports' decoder backlog
	changes []rateChange // applied changes, in order
	at      []time.Duration
}

func newCtl(t *testing.T, ceiling, fps int, p applyPolicy) *ctlHarness {
	h := &ctlHarness{t: t, clock: time.Unix(1_000_000, 0), fps: fps, share: 1, fill: 1}
	h.start = h.clock
	h.r = &rateController{now: func() time.Time { return h.clock }}
	h.r.setPolicy(p)
	h.r.setPath(true)
	kbps, f := h.r.target(ceiling, fps)
	if kbps != ceiling || f != fps {
		t.Fatalf("first generation at %d kbps %d fps, want the settings %d %d", kbps, f, ceiling, fps)
	}
	h.r.live(kbps, f)
	return h
}

func (h *ctlHarness) elapsed() time.Duration { return h.clock.Sub(h.start) }

func (h *ctlHarness) apply(c rateChange) {
	h.changes = append(h.changes, c)
	h.at = append(h.at, h.elapsed())
	h.r.live(c.toKbps, c.toFPS)
	h.fps = c.toFPS
}

// run advances the clock by d; qd(t) is the queueing delay the reports carry
// (t since the harness started; nil: no frames, a still desktop).
func (h *ctlHarness) run(d time.Duration, qd func(t time.Duration) time.Duration) {
	end := h.clock.Add(d)
	nextFrame, nextReport, nextTick := h.clock, h.clock.Add(25*time.Millisecond), h.clock.Add(rateTick)
	var bytes int64
	frames := 0
	for h.clock.Before(end) {
		h.clock = h.clock.Add(time.Millisecond)
		if qd != nil && !h.clock.Before(nextFrame) {
			nextFrame = nextFrame.Add(time.Second / time.Duration(h.fps))
			cur, _ := h.r.kbps()
			n := int(float64(cur*1000/8/h.fps) * h.fill)
			h.r.output(n)
			bytes += int64(float64(n) * h.share)
			frames++
		}
		if !h.clock.Before(nextReport) {
			nextReport = nextReport.Add(25 * time.Millisecond)
			fb := feedback{at: h.clock, frames: frames, bytes: bytes, interval: 25 * time.Millisecond, decodeQ: h.decodeQ}
			if frames > 0 {
				fb.owdValid, fb.qd = true, qd(h.elapsed())
				fb.owd = fb.qd + 10*time.Millisecond
			}
			frames, bytes = 0, 0
			if c, ok := h.r.report(fb); ok {
				h.apply(c)
			}
		}
		if !h.clock.Before(nextTick) {
			nextTick = nextTick.Add(rateTick)
			if c, ok := h.r.tick(h.stalled); ok {
				h.apply(c)
			}
		}
	}
}

func (h *ctlHarness) cur() int {
	c, _ := h.r.kbps()
	return c
}

// decreases returns the decreases applied since change i.
func (h *ctlHarness) decreases(i int) []rateChange {
	var out []rateChange
	for _, c := range h.changes[i:] {
		if c.down {
			out = append(out, c)
		}
	}
	return out
}

func flat(d time.Duration) func(time.Duration) time.Duration {
	return func(time.Duration) time.Duration { return d }
}

// after returns qd: base until t0, then over.
func after(t0, base, over time.Duration) func(time.Duration) time.Duration {
	return func(t time.Duration) time.Duration {
		if t >= t0 {
			return over
		}
		return base
	}
}

var seamless = ratePolicy(seamlessCaps)

// TestRateDelayDecrease: a queueing delay over the base (the 2 s minimum)
// plus 8 ms for three reports in a row decreases the bitrate to 0.85 x;
// two reports do not. The next decrease waits until the first is live plus
// the policy's hold (150 ms seamless), and none comes while the delay falls
// (the queue drains).
func TestRateDelayDecrease(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.run(3*time.Second, flat(20*time.Millisecond))
	if len(h.changes) != 0 {
		t.Fatalf("changes on a steady path: %+v", h.changes)
	}
	// 9 ms over the base for two reports: nothing.
	h.run(50*time.Millisecond, flat(29*time.Millisecond))
	h.run(time.Second, flat(20*time.Millisecond))
	if len(h.changes) != 0 {
		t.Fatalf("two reports over the target decreased: %+v", h.changes)
	}
	// Over the target from now on (a standing queue): the third report
	// decreases, to 0.85 x 20000 (the client receives everything: the
	// delivered rate is the target).
	h.run(80*time.Millisecond, flat(29*time.Millisecond))
	if len(h.changes) != 1 || !h.changes[0].down || h.changes[0].toKbps != 17000 || h.changes[0].why != "delay" {
		t.Fatalf("changes %+v, want one delay decrease to 17000", h.changes)
	}
	// It is live at once; the next one comes after the hold and three
	// more reports over the target.
	first := h.at[0]
	h.run(time.Second, flat(29*time.Millisecond))
	if len(h.changes) < 2 || h.changes[1].toKbps != 14450 || h.at[1]-first < seamless.hold {
		t.Fatalf("changes %+v at %v, want a second decrease to 14450 at least %v after the first", h.changes, h.at, seamless.hold)
	}
	// A queue that appears at once decreases once; while it then drains
	// (the delay falls, over the target for most of a second) nothing more.
	h = newCtl(t, 20000, 60, seamless)
	h.run(3*time.Second, flat(20*time.Millisecond))
	t0 := h.elapsed()
	drain := func(t time.Duration) time.Duration { return max(20*time.Millisecond, 70*time.Millisecond-(t-t0)/16) }
	h.run(2*time.Second, drain)
	if d := h.decreases(0); len(d) != 1 {
		t.Fatalf("decreases %+v, want one (the queue drains after it)", d)
	}
}

// TestRateDecreaseFromDelivered: a decrease starts from what the path
// delivered when that is less than the target (the client receives half of
// what the encoder produces: 0.85 x 10000), but from at least half the
// target, and an encoder that undershoots its target (fill 0.8) does not
// count as a path that carries less.
func TestRateDecreaseFromDelivered(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.run(3*time.Second, flat(20*time.Millisecond))
	h.share = 0.5
	h.run(time.Second, flat(20*time.Millisecond))
	h.run(100*time.Millisecond, flat(40*time.Millisecond))
	if len(h.changes) != 1 || h.changes[0].toKbps < 8400 || h.changes[0].toKbps > 8600 {
		t.Fatalf("changes %+v, want a decrease to about 8500 (0.85 x the delivered 10000)", h.changes)
	}
	if h.r.lastGood < 9800 || h.r.lastGood > 10200 {
		t.Fatalf("last known-good %.0f, want about 10000", h.r.lastGood)
	}
	// A path that delivers a tenth (a stall): at most a halving, 0.85 x
	// 10000.
	h = newCtl(t, 20000, 60, seamless)
	h.run(3*time.Second, flat(20*time.Millisecond))
	h.share = 0.1
	h.run(time.Second, flat(20*time.Millisecond))
	h.run(100*time.Millisecond, flat(40*time.Millisecond))
	if len(h.changes) != 1 || h.changes[0].toKbps != 8500 {
		t.Fatalf("changes %+v, want a decrease to 8500 (0.85 x half the target)", h.changes)
	}
	// An encoder at 80 % of its target whose output all arrives: 0.85 x
	// the target, not x the 16000 received.
	h = newCtl(t, 20000, 60, seamless)
	h.fill = 0.8
	h.run(3*time.Second, flat(20*time.Millisecond))
	h.run(100*time.Millisecond, flat(30*time.Millisecond))
	if len(h.changes) != 1 || h.changes[0].toKbps < 16800 || h.changes[0].toKbps > 17200 {
		t.Fatalf("changes %+v, want a decrease to about 17000", h.changes)
	}
}

// TestRatePendingFrame: frames that stop arriving (the oldest frame the
// client lacks is far over the target: 300 ms against a 20 ms base) decrease
// on the second such report in a row, without waiting for overReports; one
// such report followed by a normal one does nothing (a stall of the client
// itself), nor does a report that comes more than pendingStallGap after the
// previous one.
func TestRatePendingFrame(t *testing.T) {
	setup := func() *ctlHarness {
		h := newCtl(t, 20000, 60, seamless)
		h.run(3*time.Second, flat(20*time.Millisecond))
		return h
	}
	report := func(h *ctlHarness, interval, pending time.Duration) (rateChange, bool) {
		h.clock = h.clock.Add(interval)
		return h.r.report(feedback{at: h.clock, frames: 1, bytes: 40000, interval: interval, owdValid: true,
			qd: 20 * time.Millisecond, owd: 30 * time.Millisecond, pendingValid: pending > 0, pending: pending})
	}
	h := setup()
	if c, ok := report(h, 25*time.Millisecond, 300*time.Millisecond); ok {
		t.Fatalf("one report with a pending frame decreased: %+v", c)
	}
	if c, ok := report(h, 25*time.Millisecond, 300*time.Millisecond); !ok || !c.down || c.why != "delay" {
		t.Fatalf("second report in a row: %+v %v, want a delay decrease", c, ok)
	}

	h = setup()
	for i := 0; i < 10; i++ {
		p := time.Duration(0)
		if i%2 == 0 {
			p = 300 * time.Millisecond
		}
		if c, ok := report(h, 25*time.Millisecond, p); ok {
			t.Fatalf("report %d (pending frames every other report) decreased: %+v", i, c)
		}
	}
	for i := 0; i < 5; i++ {
		if c, ok := report(h, 150*time.Millisecond, 300*time.Millisecond); ok {
			t.Fatalf("reports %v apart decreased on their pending frame: %+v", 150*time.Millisecond, c)
		}
	}
}

// TestRateQueueGrowth: a queue that grows fast (a capacity drop: 0.5 s of
// delay per second) decreases from the capacity its growth implies (sending
// 20000 into a path that carries 20000 / 1.5); a slower growth (0.1 s/s),
// or a fast one that stands less than 20 ms over the base, from the target.
func TestRateQueueGrowth(t *testing.T) {
	for _, c := range []struct {
		name string
		per  time.Duration // growth per 25 ms report
		top  time.Duration // over the base at most (0: no limit)
		want int
	}{
		{"0.5 s/s", 12500 * time.Microsecond, 0, 11333},
		{"0.1 s/s", 2500 * time.Microsecond, 0, 17000},
		{"0.5 s/s, under 20 ms", 12500 * time.Microsecond, 15 * time.Millisecond, 17000},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newCtl(t, 20000, 60, seamless)
			h.run(3*time.Second, flat(20*time.Millisecond))
			t0 := h.elapsed()
			grow := func(t time.Duration) time.Duration {
				over := time.Duration((t-t0)/(25*time.Millisecond)) * c.per
				if c.top > 0 {
					over = min(over, c.top)
				}
				return 20*time.Millisecond + over
			}
			h.run(200*time.Millisecond, grow)
			d := h.decreases(0)
			if len(d) != 1 || math.Abs(float64(d[0].toKbps-c.want)) > float64(c.want)/50 {
				t.Fatalf("decreases %+v, want one to about %d", d, c.want)
			}
		})
	}
}

// TestRateLoss: more than 2 % of at least 100 packets lost in the last second
// decreases ("loss"); 1 % does not, nor do fewer packets; the losses detected
// within lossSettle after a decrease is live are not counted.
func TestRateLoss(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.run(2*time.Second, flat(20*time.Millisecond))
	report := func(lost, total int64) (rateChange, bool) {
		h.clock = h.clock.Add(25 * time.Millisecond)
		return h.r.report(feedback{at: h.clock, frames: 1, bytes: 40000, interval: 25 * time.Millisecond, owdValid: true,
			qd: 20 * time.Millisecond, owd: 30 * time.Millisecond, lost: lost, total: total})
	}
	for i := 0; i < 40; i++ { // 1 %: 1 of 100 per report
		if c, ok := report(1, 100); ok {
			t.Fatalf("1 %% loss decreased: %+v", c)
		}
	}
	h.clock = h.clock.Add(2 * time.Second)
	for i := 0; i < 30; i++ { // 5 % of 60 packets: too few in the window at first
		if c, ok := report(3, 60); ok {
			if i < 1 {
				t.Fatalf("decreased on %d packets: %+v", 60*(i+1), c)
			}
			if c.why != "loss" || c.toKbps != 17000 {
				t.Fatalf("change %+v, want a loss decrease to 17000", c)
			}
			h.apply(c)
			break
		}
	}
	if len(h.changes) != 1 {
		t.Fatalf("no loss decrease at 5 %%: %+v", h.changes)
	}
	// Losses right after the decrease are of packets sent before it.
	for i := 0; i < 10; i++ { // 250 ms
		if c, ok := report(10, 100); ok {
			t.Fatalf("decreased again on losses within lossSettle: %+v", c)
		}
	}
}

// TestRateLossFEC: while video goes as datagram shards with parity (GUIDE
// 2.5) random loss the parity rebuilds decreases nothing up to
// fecLossThreshold (5 % does not; 12 % does); back on streams 5 % does again.
func TestRateLossFEC(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.r.setFEC(true)
	h.run(2*time.Second, flat(20*time.Millisecond))
	report := func(lost, total int64) (rateChange, bool) {
		h.clock = h.clock.Add(25 * time.Millisecond)
		return h.r.report(feedback{at: h.clock, frames: 1, bytes: 40000, interval: 25 * time.Millisecond, owdValid: true,
			qd: 20 * time.Millisecond, owd: 30 * time.Millisecond, lost: lost, total: total})
	}
	for i := 0; i < 80; i++ { // 2 s at 5 %
		if c, ok := report(5, 100); ok {
			t.Fatalf("5 %% loss decreased with FEC: %+v", c)
		}
	}
	decreased := func(lost int64) bool {
		for i := 0; i < 80; i++ {
			if c, ok := report(lost, 100); ok {
				if c.why != "loss" {
					t.Fatalf("change %+v, want a loss decrease", c)
				}
				h.apply(c)
				return true
			}
		}
		return false
	}
	if !decreased(12) {
		t.Fatal("12 % loss did not decrease with FEC")
	}
	h.r.setFEC(false)
	h.clock = h.clock.Add(2 * time.Second)
	if !decreased(5) {
		t.Fatal("5 % loss did not decrease on streams")
	}
}

// TestRateIncrease: after a decrease the target climbs +5 %/s just below the
// last known-good rate, faster far below it (up to +25 %/s), accelerating
// above it, and never above the ceiling.
func TestRateIncrease(t *testing.T) {
	growth := func(t *testing.T, lastGood, from float64, d time.Duration) float64 {
		h := newCtl(t, 100000, 60, seamless)
		h.r.mu.Lock()
		h.r.est, h.r.applied, h.r.lastGood = from, int(from), lastGood
		h.r.mu.Unlock()
		h.r.live(int(from), 60)
		h.run(d, flat(20*time.Millisecond))
		return float64(h.cur()) / from
	}
	// One second near (5 % below) the last known-good rate: +5 %.
	if g := growth(t, 20000, 19000, time.Second); math.Abs(g-1.05) > 0.01 {
		t.Fatalf("near the last known-good rate: x%.3f in 1 s, want about x1.05", g)
	}
	// Half of it: +25 %/s (the encoder takes it every 250 ms).
	if g := growth(t, 20000, 10000, time.Second); g < 1.2 || g > 1.3 {
		t.Fatalf("far below the last known-good rate: x%.3f in 1 s, want about x1.25", g)
	}
	// Above it the rate accelerates: the third second grows more than the first.
	h := newCtl(t, 100000, 60, seamless)
	h.r.mu.Lock()
	h.r.est, h.r.applied, h.r.lastGood = 10000, 10000, 10000
	h.r.mu.Unlock()
	h.r.live(10000, 60)
	var at []int
	for i := 0; i < 4; i++ {
		at = append(at, h.cur())
		h.run(time.Second, flat(20*time.Millisecond))
	}
	g1, g3 := float64(at[1])/float64(at[0]), float64(at[3])/float64(at[2])
	if g1 > 1.08 || g3 < g1+0.05 {
		t.Fatalf("above the last known-good rate: x%.3f in the first second, x%.3f in the third, want ~x1.05 then faster", g1, g3)
	}
	// Up to the ceiling, not beyond.
	h = newCtl(t, 3000, 60, seamless)
	h.r.mu.Lock()
	h.r.est, h.r.applied, h.r.lastGood = 2000, 2000, 2000
	h.r.mu.Unlock()
	h.r.live(2000, 60)
	h.run(20*time.Second, flat(20*time.Millisecond))
	if h.cur() != 3000 {
		t.Fatalf("target %d after 20 s, want the ceiling 3000", h.cur())
	}
	for _, c := range h.changes {
		if c.toKbps > 3000 {
			t.Fatalf("raised above the ceiling: %+v", c)
		}
	}
}

// TestRateIncreaseCap: increases stop at 1.2 x the delivered rate (here the
// client receives 60 % of the encoder's output), and at the decoder's cap.
func TestRateIncreaseCap(t *testing.T) {
	// 10000 is the setting while the receive rate is measured, then the
	// setting goes up (target is what every generation start calls).
	h := newCtl(t, 10000, 60, seamless)
	h.share = 0.6
	h.run(2*time.Second, flat(20*time.Millisecond))
	h.r.target(50000, 60)
	h.r.mu.Lock()
	h.r.lastGood = 40000
	h.r.mu.Unlock()
	h.run(10*time.Second, flat(20*time.Millisecond))
	// Each step at most 1.2 x 0.6 of the encoder's rate: it cannot climb.
	if h.cur() > 10000 {
		t.Fatalf("target %d with 60 %% delivered, want no increase above 1.2 x the delivered 6000", h.cur())
	}
	h.share = 1
	h.r.mu.Lock()
	h.r.decoderCap = 15000
	h.r.mu.Unlock()
	h.run(20*time.Second, flat(20*time.Millisecond))
	if h.cur() != 15000 {
		t.Fatalf("target %d, want the decoder's cap 15000", h.cur())
	}
}

// TestRateIncreaseHolds: nothing increases without fresh reports (a still
// desktop), on a stalled path, with the decoder behind, or with the delay
// over the target.
func TestRateIncreaseHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(h *ctlHarness)
		qd   func(time.Duration) time.Duration
	}{
		{"still desktop", func(*ctlHarness) {}, nil},
		{"stalled", func(h *ctlHarness) { h.stalled = true }, flat(20 * time.Millisecond)},
		{"decoder behind", func(h *ctlHarness) { h.decodeQ = 7 }, flat(20 * time.Millisecond)}, // over max(4, 60/10)
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newCtl(t, 20000, 60, seamless)
			h.run(3*time.Second, flat(20*time.Millisecond))
			h.r.mu.Lock()
			h.r.est, h.r.applied, h.r.lastGood = 10000, 10000, 20000
			h.r.mu.Unlock()
			h.r.live(10000, 60)
			c.set(h)
			// The last report with frames counts for feedbackFresh.
			h.run(feedbackFresh+100*time.Millisecond, c.qd)
			held, _, _, _ := h.r.state()
			h.run(5*time.Second, c.qd)
			if est, _, _, _ := h.r.state(); est != held || held > 11400 {
				t.Fatalf("target %.0f, then %.0f: want it held from 10000", held, est)
			}
		})
	}
}

// TestRatePolicyGaps: a restart policy (FFmpeg) takes an increase at most
// once per second and in steps of at least 5 %; near the last known-good rate
// in steps of at most 5 %; a decrease no sooner than 500 ms after the last
// change (tick applies it then), overlapped, unless it cuts to 75 % or less:
// then at once, urgent.
func TestRatePolicyGaps(t *testing.T) {
	h := newCtl(t, 40000, 60, ratePolicy(restartCaps))
	h.r.mu.Lock()
	h.r.est, h.r.applied, h.r.lastGood = 10000, 10000, 30000
	h.r.mu.Unlock()
	h.r.live(10000, 60)
	h.run(20*time.Second, flat(20*time.Millisecond))
	if len(h.changes) < 5 || h.cur() != 40000 {
		t.Fatalf("%d changes, target %d: want the climb to the ceiling 40000", len(h.changes), h.cur())
	}
	for i, c := range h.changes {
		if i > 0 && h.at[i]-h.at[i-1] < time.Second {
			t.Fatalf("increases %v apart: %v", h.at[i]-h.at[i-1], h.at)
		}
		near := float64(c.fromKbps) >= (1-nearBelow)*30000 && float64(c.fromKbps) <= (1+nearAbove)*30000
		if step := float64(c.toKbps) / float64(c.fromKbps); step < 1.05-1e-3 && c.toKbps != 40000 || near && step > 1+nearStep+1e-3 {
			t.Fatalf("change %d %+v: step x%.3f (near the last known-good rate: %v)", i, c, step, near)
		}
	}
	// A decrease 200 ms after an increase waits for the 500 ms gap.
	h.r.mu.Lock()
	h.r.est, h.r.applied, h.r.lastApply = 30000, 30000, h.clock
	h.r.mu.Unlock()
	h.r.live(30000, 60)
	n := len(h.changes)
	h.run(200*time.Millisecond, flat(20*time.Millisecond))
	h.run(time.Second, flat(30*time.Millisecond))
	d := h.decreases(n)
	if len(d) == 0 || h.at[n]-h.at[n-1] < 500*time.Millisecond || d[0].urgent {
		t.Fatalf("decreases %+v at %v, want the first at least 500 ms after the last change, overlapped", d, h.at[n-1:])
	}
	// A cut to 75 % or less (the path carries half of what the encoder
	// makes: a capacity drop) is urgent, and does not wait for the gap.
	h = newCtl(t, 30000, 60, ratePolicy(restartCaps))
	h.run(3*time.Second, flat(20*time.Millisecond))
	h.share = 0.5
	h.run(time.Second, flat(20*time.Millisecond))
	h.r.mu.Lock()
	h.r.lastApply = h.clock
	h.r.mu.Unlock()
	h.run(150*time.Millisecond, flat(40*time.Millisecond))
	if d := h.decreases(0); len(d) != 1 || !d[0].urgent || d[0].toKbps > 30000*3/4 {
		t.Fatalf("decreases %+v, want one urgent cut to about 0.85 x 15000 within 150 ms of the last change", d)
	}
}

// TestRateEmergency: a frame-queue overflow cuts at once by 25 %, urgent; not
// within 2 s of any decrease; a decoder flush also caps increases at 85 % of
// the bitrate it cut from; nothing below the floor or before the first
// generation.
func TestRateEmergency(t *testing.T) {
	var r rateController
	if _, ok := r.congestion(signalOverflow); ok {
		t.Fatal("cut before the first generation")
	}
	h := newCtl(t, 20000, 60, seamless)
	c, ok := h.r.congestion(signalOverflow)
	if !ok || !c.urgent || !c.down || c.toKbps != 15000 || c.why != "overflow" {
		t.Fatalf("overflow: %+v %v, want an urgent cut to 15000", c, ok)
	}
	h.apply(c)
	h.clock = h.clock.Add(time.Second)
	if c, ok := h.r.congestion(signalDecoder); ok {
		t.Fatalf("emergency 1 s after a cut: %+v", c)
	}
	h.clock = h.clock.Add(1500 * time.Millisecond)
	c, ok = h.r.congestion(signalDecoder)
	if !ok || c.toKbps != 11250 || h.r.decoderLimit() != 12750 {
		t.Fatalf("decoder flush: %+v %v cap %d, want 11250 and a cap of 12750", c, ok, h.r.decoderLimit())
	}
	h.apply(c)
	h.run(30*time.Second, flat(20*time.Millisecond))
	if h.cur() != 12750 {
		t.Fatalf("target %d after the decoder flush, want the cap 12750", h.cur())
	}
	// An overflow after the frames stalled: nothing acknowledged in the last
	// 100 ms. At most a halving, as in decrease: 0.85 x half the target, and
	// the last known-good rate half the target.
	h = newCtl(t, 20000, 60, seamless)
	h.run(3*time.Second, flat(20*time.Millisecond))
	for i := 0; i < 5; i++ {
		h.clock = h.clock.Add(25 * time.Millisecond)
		h.r.report(feedback{at: h.clock, ackedValid: true})
	}
	if d, ok := h.r.carried(h.clock); !ok || d != 0 {
		t.Fatalf("carried %.0f %v, want 0 (nothing acknowledged)", d, ok)
	}
	c, ok = h.r.congestion(signalOverflow)
	if !ok || c.toKbps != 8500 || h.r.lastGood != 10000 {
		t.Fatalf("overflow after a stall: %+v %v, last known-good %.0f; want a cut to 8500 from 10000", c, ok, h.r.lastGood)
	}
	// The floor.
	h = newCtl(t, 2500, 60, seamless)
	c, ok = h.r.congestion(signalOverflow)
	if !ok || c.toKbps != 2000 {
		t.Fatalf("cut %+v %v, want the floor 2000", c, ok)
	}
	h.apply(c)
	h.clock = h.clock.Add(3 * time.Second)
	if c, ok := h.r.congestion(signalOverflow); ok {
		t.Fatalf("cut below the floor: %+v", c)
	}
}

// TestRateFPSLadder: at the floor a decrease lowers the frame rate a rung
// (120 -> 90 -> 60) instead, and nothing below 60; once the bitrate is well
// above the floor again (1.5 x, or at a limit below that: a setting of 2500)
// the frame rate goes back up a rung every 5 s.
func TestRateFPSLadder(t *testing.T) {
	for _, ceiling := range []int{10000, 2500} {
		h := newCtl(t, ceiling, 120, seamless)
		h.run(3*time.Second, flat(20*time.Millisecond))
		h.r.mu.Lock()
		h.r.est, h.r.applied = 2000, 2000
		h.r.mu.Unlock()
		h.r.live(2000, 120)
		h.run(1500*time.Millisecond, flat(40*time.Millisecond))
		var fps []int
		for _, c := range h.changes {
			if !c.down || c.toKbps != 2000 {
				t.Fatalf("setting %d: change %+v at the floor, want frame-rate decreases at 2000", ceiling, c)
			}
			fps = append(fps, c.toFPS)
		}
		if len(fps) != 2 || fps[0] != 90 || fps[1] != 60 {
			t.Fatalf("setting %d: frame rates %v, want 90, 60", ceiling, fps)
		}
		h.changes = nil
		h.run(20*time.Second, flat(20*time.Millisecond))
		var up []int
		for _, c := range h.changes {
			if c.toFPS != c.fromFPS {
				up = append(up, c.toFPS)
			}
		}
		if len(up) != 2 || up[0] != 90 || up[1] != 120 || h.fps != 120 {
			t.Fatalf("setting %d: frame rates back %v (now %d), want 90 then 120", ceiling, up, h.fps)
		}
	}
}

// TestRateAdaptiveOff: with adaptive bitrate off the delay and losses decide
// nothing; an emergency still cuts.
func TestRateAdaptiveOff(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.r.setAdaptive(false)
	h.run(2*time.Second, flat(20*time.Millisecond))
	h.run(2*time.Second, flat(80*time.Millisecond))
	if len(h.changes) != 0 {
		t.Fatalf("changes with adaptive bitrate off: %+v", h.changes)
	}
	if _, ok := h.r.congestion(signalOverflow); !ok {
		t.Fatal("no emergency cut with adaptive bitrate off")
	}
}

// TestRateNoFeedback: a client that sends no feedback (no reports, no acks)
// decreases only on its own delay reports ({"t":"congestion"}), and gets
// increases (+5 %/s) 10 s after the last decrease.
func TestRateNoFeedback(t *testing.T) {
	h := newCtl(t, 20000, 60, ratePolicy(restartCaps))
	h.clock = h.clock.Add(time.Second) // past the policy's gap since the generation started
	c, ok := h.r.congestion(signalDelay)
	if !ok || c.urgent || c.toKbps != 17000 || c.why != "client" {
		t.Fatalf("client delay report: %+v %v, want a non-urgent decrease to 17000", c, ok)
	}
	h.apply(c)
	tick := func(d time.Duration) {
		for end := h.clock.Add(d); h.clock.Before(end); {
			h.clock = h.clock.Add(rateTick)
			if c, ok := h.r.tick(false); ok {
				h.apply(c)
			}
		}
	}
	tick(9500 * time.Millisecond)
	if h.cur() != 17000 {
		t.Fatalf("target %d within 10 s, want 17000", h.cur())
	}
	tick(10 * time.Second)
	if h.cur() < 19000 {
		t.Fatalf("target %d 20 s after the decrease, want the climb back", h.cur())
	}
}

// TestRateReset: a settings change drops the back-off, the decoder's cap and
// the frame-rate ladder; a restart keeps them.
func TestRateReset(t *testing.T) {
	h := newCtl(t, 20000, 120, seamless)
	h.r.mu.Lock()
	h.r.est, h.r.fps, h.r.decoderCap = 9000, 90, 12000
	h.r.mu.Unlock()
	if kbps, fps := h.r.target(20000, 120); kbps != 9000 || fps != 90 {
		t.Fatalf("restart at %d kbps %d fps, want 9000 at 90", kbps, fps)
	}
	h.r.reset()
	if kbps, fps := h.r.target(25000, 60); kbps != 25000 || fps != 60 || h.r.decoderLimit() != 0 {
		t.Fatalf("after reset %d kbps %d fps cap %d, want the new settings and no cap", kbps, fps, h.r.decoderLimit())
	}
}

// TestRateJitter: Wi-Fi-like jitter (each report 0-15 ms over the base,
// uniformly) widens the target margin and decreases nothing in a minute;
// the same path with a queue that grows 20 ms/s on top is still caught.
func TestRateJitter(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	jitter := func(time.Duration) time.Duration {
		return 20*time.Millisecond + time.Duration(rng.IntN(15000))*time.Microsecond
	}
	h := newCtl(t, 20000, 60, seamless)
	h.run(60*time.Second, jitter)
	if len(h.changes) != 0 {
		t.Fatalf("decreases on jitter alone: %+v", h.changes)
	}
	if _, _, m, _ := h.r.state(); m <= queueMargin {
		t.Fatalf("margin %v, want wider than %v", m, queueMargin)
	}
	growing := func(t time.Duration) time.Duration {
		return jitter(t) + max(0, t-62*time.Second)/50
	}
	h.run(3*time.Second, growing)
	if d := h.decreases(0); len(d) == 0 {
		t.Fatal("a growing queue under jitter decreased nothing")
	}
}

// TestSendTrack: the pacer's share of a frame's sending time follows the
// fluid model (backlog less the burst, at the video pacing rate), at most the
// measured encodeDone -> written time; a key frame delays itself and the
// frames behind it, decaying; cover returns the median of the frames it
// covers; pending is the age of the oldest frame not covered.
func TestSendTrack(t *testing.T) {
	var tr sendTrack
	const pace = 24e6 // bit/s for video
	rate := pace * (1 - paceOverhead) / 8
	us := func(d time.Duration) uint64 { return uint64(d.Microseconds()) + 1_000_000 }
	frame := time.Second / 60
	// A 41.7 kB frame (20 Mbit/s at 60 fps) every frame interval, written
	// 20 ms after encodeDone (measured), a 200 kB key frame at seq 10.
	var comps []time.Duration
	for i := 0; i < 30; i++ {
		size := 41700
		if i == 10 {
			size = 200000
		}
		enc := time.Duration(i) * frame
		tr.sent(1, uint32(i), us(enc), us(enc+200*time.Millisecond), size, pace)
		c, n := tr.cover(1, uint32(i))
		if n != 1 {
			t.Fatalf("frame %d: covered %d", i, n)
		}
		comps = append(comps, c)
	}
	want0 := time.Duration((41700 - max(rate*0.002, minPaceBurst)) / rate * float64(time.Second))
	if d := comps[0] - want0; d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("first frame's pacer share %v, want about %v", comps[0], want0)
	}
	if comps[10] < 50*time.Millisecond || comps[11] >= comps[10] || comps[11] < comps[5] || comps[29] > comps[11] {
		t.Fatalf("pacer shares around the key frame %v: want it large, then decaying", comps[8:16])
	}
	// The measured time bounds it.
	tr.sent(1, 30, us(30*frame), us(30*frame+time.Millisecond), 200000, pace)
	if c, _ := tr.cover(1, 30); c != time.Millisecond {
		t.Fatalf("share %v, want the measured 1 ms", c)
	}
	// Without the media congestion controller nothing is attributed.
	tr.sent(1, 31, us(31*frame), us(31*frame+50*time.Millisecond), 200000, 0)
	if c, _ := tr.cover(1, 31); c != 0 {
		t.Fatalf("share %v without pacing, want 0", c)
	}
	// cover: the median of the frames it covers; pending: the oldest
	// uncovered one's age less its share.
	for i := 32; i < 37; i++ {
		tr.sent(1, uint32(i), us(time.Duration(i)*frame), us(time.Duration(i)*frame), 1000, 0)
	}
	if p, ok := tr.pending(us(32*frame + 70*time.Millisecond)); !ok || p != 70*time.Millisecond {
		t.Fatalf("pending %v %v, want 70 ms", p, ok)
	}
	if _, n := tr.cover(1, 34); n != 3 {
		t.Fatalf("covered %d, want 3", n)
	}
	if _, n := tr.cover(1, 33); n != 0 {
		t.Fatalf("covered %d of a frame already covered", n)
	}
	if at, ok := tr.uncoveredSince(); !ok || at != us(35*frame) {
		t.Fatalf("uncovered since %d %v", at, ok)
	}
	if f, ok := tr.frame(1, 36); !ok || f.seq != 36 {
		t.Fatalf("frame 36: %+v %v", f, ok)
	}
	if _, ok := tr.uncoveredSince(); ok {
		t.Fatal("an acked frame left frames uncovered")
	}
}

// TestRateFeedback: reports become differences of their counters (also
// across the wrap), a reordered or duplicated report is ignored (its client
// clock is not newer), the client's own losses count only without the media
// congestion controller, whose counters restart with a new path; acks of
// clients without reports become one report per call.
func TestRateFeedback(t *testing.T) {
	var f rateFeedback
	now := time.Unix(1_000_000, 0)
	r := proto.RateReport{Flags: proto.RateReportOWD, TimeMs: 0xffffffff - 10, Frames: 100, Bytes: 0xffffff00, OWDP50Us: 25000,
		OWDMaxUs: 40000, Lost: 2, Audio: 1000}
	if fb, ok := f.fromReport(r, now, 0, ccCounters{}); !ok || fb.frames != 0 || !fb.owdValid || fb.qd != 25*time.Millisecond {
		t.Fatalf("first report: %+v", fb)
	}
	first := r
	r.TimeMs, r.Frames, r.Bytes, r.Lost, r.Audio = 15, 103, 0x100, 3, 1003
	fb, _ := f.fromReport(r, now.Add(25*time.Millisecond), 5*time.Millisecond, ccCounters{})
	if fb.frames != 3 || fb.bytes != 0x200 || fb.interval != 26*time.Millisecond || fb.lost != 1 || fb.total != 7 ||
		fb.qd != 20*time.Millisecond || fb.owd != 25*time.Millisecond {
		t.Fatalf("second report: %+v", fb)
	}
	// The first report again (reordered), and the second (duplicated):
	// ignored, and the next report's differences are from the second.
	for _, old := range []proto.RateReport{first, r} {
		if fb, ok := f.fromReport(old, now.Add(30*time.Millisecond), 0, ccCounters{}); ok {
			t.Fatalf("an old report taken: %+v", fb)
		}
	}
	r.TimeMs, r.Frames, r.Bytes, r.Lost, r.Audio = 40, 105, 0x300, 3, 1004
	if fb, ok := f.fromReport(r, now.Add(50*time.Millisecond), 0, ccCounters{}); !ok || fb.frames != 2 || fb.bytes != 0x200 ||
		fb.interval != 25*time.Millisecond || fb.lost != 0 || fb.total != 3 {
		t.Fatalf("the report after old ones: %+v %v", fb, ok)
	}
	cc := ccCounters{ok: true, lost: 10, total: 1000, acked: 1e6, nonVideoKbps: 360}
	r.TimeMs += 25
	f.fromReport(r, now.Add(75*time.Millisecond), 0, cc)
	cc.lost, cc.total, cc.acked = 12, 1100, 1.1e6
	r.TimeMs += 25
	fb, _ = f.fromReport(r, now.Add(100*time.Millisecond), 0, cc)
	if fb.lost != 2 || fb.total != 100 || !fb.ackedValid || fb.acked != 100000 || fb.nonVideoKbps != 360 {
		t.Fatalf("media congestion controller deltas: %+v", fb)
	}
	cc.lost, cc.total, cc.acked = 1, 10, 1000 // a new path's controller
	r.TimeMs += 25
	if fb, _ := f.fromReport(r, now.Add(125*time.Millisecond), 0, cc); fb.total != 0 || fb.ackedValid {
		t.Fatalf("after a path change: %+v", fb)
	}
	var a rateFeedback
	if _, ok := a.fromAcks(now, ccCounters{}); ok {
		t.Fatal("a report from no acks")
	}
	for i, owd := range []int{30, 10, 20} {
		a.ack(time.Duration(owd)*time.Millisecond, time.Duration(i)*time.Millisecond, 1000, now)
	}
	fb, ok := a.fromAcks(now.Add(100*time.Millisecond), ccCounters{})
	if !ok || fb.frames != 3 || fb.bytes != 3000 || fb.interval != 100*time.Millisecond || fb.owd != 20*time.Millisecond ||
		fb.owdMax != 30*time.Millisecond || fb.qd != 18*time.Millisecond {
		t.Fatalf("acks: %+v %v", fb, ok)
	}
	if p50, p95, mx, n := a.stats(); n != 1 || p50 != 20*time.Millisecond || p95 != 20*time.Millisecond || mx != 30*time.Millisecond {
		t.Fatalf("stats %v %v %v %d", p50, p95, mx, n)
	}
}

// The simulations (ratesim_test.go): the GUIDE 2.2 acceptance on netem.sh's
// capdrop profile (50 -> 15 -> 50 Mbit/s, 20 s steps, 50 ms queue) at a
// 30 Mbit/s setting: no host frame-queue overflow, one-way delay p95 during
// the dip under the baseline's + 30 ms, the bitrate back within 15 % of the
// setting within 10 s of capacity returning, and staying there; and the
// other profiles.

func TestRateSimCapdrop(t *testing.T) {
	for _, c := range []struct {
		name      string
		policy    applyPolicy
		fill      float64
		relay     bool
		overflows int           // allowed: at the drop, before a restart can take over
		recover   time.Duration // to 85 % of the setting
	}{
		{"seamless", seamless, 1, false, 0, 10 * time.Second},
		{"seamless, encoder at 80 %", seamless, 0.8, false, 0, 10 * time.Second},
		{"seamless, relay", seamless, 1, true, 1, 10 * time.Second},
		{"restart (FFmpeg)", ratePolicy(restartCaps), 1, false, 0, 10 * time.Second},
		{"restart (FFmpeg), encoder at 80 %", ratePolicy(restartCaps), 0.8, false, 0, 10 * time.Second},
		{"flush", ratePolicy(flushCaps), 1, false, 1, 10 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := runSim(simConfig{dur: 60 * time.Second, fps: 60, ceiling: 30000, capacity: capdropRates(1), queue: 50 * time.Millisecond,
				prop: time.Millisecond, policy: c.policy, applyDelay: 400 * time.Millisecond, keyFactor: 2, fill: c.fill, audioKbps: 160,
				relay: c.relay, seed: 1})
			base, dip := s.owdPct(5*time.Second, 20*time.Second, 0.95), s.owdPct(20*time.Second, 40*time.Second, 0.95)
			back := s.heldFrom(40*time.Second, 30000*85/100)
			t.Logf("overflows %v, one-way delay p95 %v before, %v during the dip; back at 85 %% %v after capacity returned; decreases %v\n%s",
				s.overflows, base, dip, back-40*time.Second, s.decreases(0, s.cfg.dur), s.trace())
			if len(s.overflows) > c.overflows {
				t.Errorf("%d frame-queue overflows, want at most %d", len(s.overflows), c.overflows)
			}
			if dip >= base+30*time.Millisecond {
				t.Errorf("one-way delay p95 %v during the dip, want under %v + 30 ms", dip, base)
			}
			if back < 0 || back-40*time.Second > c.recover {
				t.Errorf("bitrate back within 15 %% of the setting %v after capacity returned, want %v", back-40*time.Second, c.recover)
			}
			if s.appliedAt(19*time.Second) != 30000 || s.appliedAt(59*time.Second) != 30000 {
				t.Errorf("target %d before the dip, %d at the end, want the setting", s.appliedAt(19*time.Second), s.appliedAt(59*time.Second))
			}
			if dec := s.decreases(0, 20*time.Second); len(dec) > 0 {
				t.Errorf("decreases before the dip: %v", dec)
			}
		})
	}
}

// TestRateSimProfiles: the other netem profiles at a 20 Mbit/s setting over
// a 50 Mbit/s link: wifi (a gate that opens every 0.5-15 ms, 1 % loss) and
// wan (20 ms each way, 0.5 % loss) keep the bitrate (no more than one
// decrease a minute, 95 % of the setting on average); 5 % loss is
// congestion by the guide's rule (> 2 %) and backs off.
func TestRateSimProfiles(t *testing.T) {
	link := func(time.Duration) int64 { return 50e6 }
	cfg := func(seed uint64) simConfig {
		return simConfig{dur: 60 * time.Second, fps: 60, ceiling: 20000, capacity: link, queue: 50 * time.Millisecond,
			prop: time.Millisecond, policy: seamless, applyDelay: 400 * time.Millisecond, keyFactor: 2, audioKbps: 160, seed: seed}
	}
	for _, c := range []struct {
		name string
		set  func(*simConfig)
	}{
		{"wifi", func(c *simConfig) { c.wifi, c.loss = true, 0.01 }},
		{"wifi, restart", func(c *simConfig) { c.wifi, c.loss, c.policy = true, 0.01, ratePolicy(restartCaps) }},
		{"wan", func(c *simConfig) { c.prop, c.loss = 20*time.Millisecond, 0.005 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := cfg(2)
			c.set(&sc)
			s := runSim(sc)
			dec, mean := s.decreases(0, sc.dur), s.meanApplied(5*time.Second, sc.dur)
			t.Logf("decreases %v, mean target %.0f, one-way delay p50/p95 %v/%v", dec, mean, s.owdPct(0, sc.dur, 0.5), s.owdPct(0, sc.dur, 0.95))
			n := 0
			for _, v := range dec {
				n += v
			}
			if n > 1 || mean < 0.95*20000 || len(s.overflows) > 0 {
				t.Errorf("%d decreases %v, mean %.0f, %d overflows: want at most one, >= 19000, none\n%s", n, dec, mean, len(s.overflows), s.trace())
			}
		})
	}
	t.Run("loss 5%", func(t *testing.T) {
		sc := cfg(4)
		sc.dur, sc.loss = 20*time.Second, 0.05
		s := runSim(sc)
		if d := s.decreases(0, sc.dur); d["loss"] == 0 || s.appliedAt(sc.dur) > 10000 {
			t.Errorf("decreases %v, target %d at the end: want loss decreases to at most half\n%s", d, s.appliedAt(sc.dur), s.trace())
		}
	})
}

// TestRateSimKeyFrames: an FFmpeg session restarted for a key frame every
// 3 s (4 x a frame each, paced out at 1.2 x the target) on a clean link keeps
// its bitrate: sendTrack takes the key frames' own sending time out of the
// delay. Without it the same run backs off again and again.
func TestRateSimKeyFrames(t *testing.T) {
	cfg := simConfig{dur: 60 * time.Second, fps: 60, ceiling: 20000, capacity: func(time.Duration) int64 { return 50e6 },
		queue: 50 * time.Millisecond, prop: time.Millisecond, policy: ratePolicy(restartCaps), applyDelay: 400 * time.Millisecond,
		keyFactor: 4, keyEvery: 3 * time.Second, audioKbps: 160, seed: 5}
	s := runSim(cfg)
	t.Logf("with the pacer's share: decreases %v, mean target %.0f, one-way delay p95 %v", s.decreases(0, cfg.dur),
		s.meanApplied(0, cfg.dur), s.owdPct(0, cfg.dur, 0.95))
	if d := s.decreases(0, cfg.dur); len(d) > 0 || s.meanApplied(0, cfg.dur) != 20000 {
		t.Errorf("decreases %v, mean %.0f: want none\n%s", d, s.meanApplied(0, cfg.dur), s.trace())
	}
	cfg.noComp = true
	s = runSim(cfg)
	t.Logf("without it: decreases %v, mean target %.0f over the last 30 s", s.decreases(0, cfg.dur), s.meanApplied(30*time.Second, cfg.dur))
	n := 0
	for _, v := range s.decreases(0, cfg.dur) {
		n += v
	}
	if n < 5 || s.meanApplied(30*time.Second, cfg.dur) > 10000 {
		t.Errorf("without the pacer's share: %d decreases, mean %.0f; the comparison wants a spiral down", n, s.meanApplied(30*time.Second, cfg.dur))
	}
}
