package host

import (
	"fmt"
	"testing"
	"time"
)

// The owner's first hardware run (host.log of 52f801f; docs/VENDOR_NOTES.md
// "First hardware run: the static desktop during a game") replayed against
// the code where a single decision decides: the simulations
// (ratesim_relay_test.go TestRelaySimFirstHardwareRun, TestRelaySimStaticDesktop)
// replay the runs.

// TestOwnerLogFirstOverflow: the log's first frame-queue overflow, at
// 30 Mbit/s with no loss (cc_lost=0), 128 ms after the static-desktop cap
// gave the full bitrate back: frames=7 (six queued and the new one),
// pts_span_ms=93, sender_busy_ms=22, sender=write, cc_target_kbps=30360. At
// 52f801f six queued frames overflowed whatever made them wait. Now the
// frame being written is on schedule (a frame of the full target, the most
// the cap's VBV lets the first frame after a restore be, paced at 1.2 x
// 30360 kbit/s takes 14 ms; its deadline is two frame intervals, 33 ms), it
// reached the queue 115 ms ago (under frameQueueAge), and the delay was not
// over its target: no overflow. With the delay over the target, or the
// frame past its deadline, the same queue still overflows.
func TestOwnerLogFirstOverflow(t *testing.T) {
	interval := time.Second / 60
	frame := 30360 * 1000 / 8 / 60 // bytes: a frame at the full target
	deadline := frameDeadline(interval, frame, 1.2*30360*1000)
	q := queueLoad{queued: 6, sending: true, busy: 22 * time.Millisecond, deadline: deadline, lag: 93*time.Millisecond + 22*time.Millisecond,
		interval: interval, burst: deadline}
	if deadline != 2*interval {
		t.Fatalf("deadline %v of a %d-byte frame at 36.4 Mbit/s, want two frame intervals", deadline, frame)
	}
	if why := queueOverflows(q); why != "" {
		t.Fatalf("the log's first overflow: %q, want none", why)
	}
	congested := q
	congested.congested = true
	late := q
	late.busy = deadline + time.Millisecond
	if queueOverflows(congested) != "delay" || queueOverflows(late) != "deadline" {
		t.Fatalf("the same queue with the delay over the target: %q, past its deadline: %q", queueOverflows(congested), queueOverflows(late))
	}
}

// TestRateAttrs: "congestion: lowering bitrate" logged delayMs=0 for all 20
// of the log's delay decreases: the field was an older client's own delay
// report ({"t":"congestion"}), and the rate reports' decisions passed 0. Now
// a decrease logs what it was decided on: the queueing delay over the base
// and its target (frames_late where frames did not arrive at all), the loss,
// and the rate it started from; an older client's report keeps its delayMs.
func TestRateAttrs(t *testing.T) {
	for _, c := range []struct {
		c       rateChange
		delayMs int
		want    string
	}{
		{rateChange{why: "delay", basis: rateBasis{qd: 23 * time.Millisecond, margin: 8 * time.Millisecond, startKbps: 77000, start: "delivered"}}, 0,
			"[delay_ms 23 delay_target_ms 8 start_kbps 77000 start delivered]"},
		{rateChange{why: "delay", basis: rateBasis{qd: 94 * time.Millisecond, margin: 8 * time.Millisecond, bound: true, startKbps: 75500, start: "halving"}}, 0,
			"[delay_ms 94 delay_target_ms 8 frames_late true start_kbps 75500 start halving]"},
		{rateChange{why: "loss", basis: rateBasis{lossPct: 3.14159, startKbps: 30000, start: "target"}}, 0, "[loss_pct 3.1 start_kbps 30000 start target]"},
		{rateChange{why: "overflow", basis: rateBasis{startKbps: 30000, start: "target"}}, 0, "[start_kbps 30000 start target]"},
		{rateChange{why: "client"}, 120, "[delayMs 120]"},
	} {
		if got := fmt.Sprint(rateAttrs(c.c, c.delayMs)); got != c.want {
			t.Errorf("%s: %s, want %s", c.c.why, got, c.want)
		}
	}
}
