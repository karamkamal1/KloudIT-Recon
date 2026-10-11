package host

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The owner's first hardware run (host.log of 52f801f; docs/VENDOR_NOTES.md
// "First hardware run: the static desktop during a game") replayed against
// the code where a single decision decides: the simulations
// (ratesim_relay_test.go TestRelaySimFirstHardwareRun, TestRelaySimStaticDesktop)
// replay the runs.

// TestOwnerLogFirstOverflow: the log's first frame-queue overflow, at
// 30 Mbit/s with no loss (cc_lost=0), 128 ms after the static-desktop cap
// gave the full bitrate back at the end of an 11 s capture loss: frames=7 (six
// queued and the new one), pts_span_ms=93, sender_busy_ms=22, sender=write,
// cc_target_kbps=30360. 37 ms later the frame being written then, seq 120,
// was cancelled past its deadline: "frame stream cancelled seq=120 ...
// age_ms=59 deadline_ms=59". A 59 ms deadline at 1.2 x 30360 kbit/s is a
// frame of about 190 KB: AMF overshot the cap's VBV (one frame at the full
// target, 63 KB) about 3x on the restore frame, and the path held it past its
// deadline. At 52f801f six queued frames overflowed whatever made them wait.
// Now that queue is on schedule (busy 22 ms of the 59, reached 115 ms ago,
// the delay not over its target): no overflow, so neither the 7-frame drop
// nor the overflow's cut (30000 -> 12750). With the delay over the target, or
// the frame past its deadline, the same queue still overflows. And the loss
// stays: rung 1 cancels seq 120 at 59 ms (a newer frame waits), rung 2 asks
// the encoder for a recovery frame, and the frames queued behind seq 120
// reference it, so they are discarded until the recovery frame comes.
func TestOwnerLogFirstOverflow(t *testing.T) {
	interval := time.Second / 60
	pacing := 1.2 * 30360 * 1000 // bit/s
	frame := int((59*time.Millisecond - interval).Seconds() * pacing / 8)
	deadline := frameDeadline(interval, frame, pacing)
	if d := deadline - 59*time.Millisecond; d < -time.Millisecond || d > time.Millisecond || frame < 3*30360*1000/8/60 {
		t.Fatalf("a %d-byte frame (%.1f full-target frames) at 36.4 Mbit/s: deadline %v, want the log's 59 ms", frame,
			float64(frame)/(30360*1000/8/60), deadline)
	}
	q := queueLoad{queued: 6, sending: true, busy: 22 * time.Millisecond, deadline: deadline, lag: 93*time.Millisecond + 22*time.Millisecond,
		interval: interval, burst: deadline}
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
	// The log's cancel of seq 120 at 59 ms: rung 1, then rung 2.
	out := ladderIn{event: lossOutgoing, gen: 1, seq: 120, age: 59 * time.Millisecond, deadline: deadline, newer: true, live: 1,
		mode: proto.RecoveryLTR, forceIDR: true}
	if st := ladder(out); st.rung != 1 || st.act != actCancel {
		t.Fatalf("seq 120 at 59 ms: %+v, want rung 1's cancel", st)
	}
	lost := ladderIn{event: lossConfirmed, gen: 1, seq: 120, live: 1, mode: proto.RecoveryLTR, forceIDR: true}
	if st := ladder(lost); st.rung != 2 || st.act != actRecover {
		t.Fatalf("seq 120 lost: %+v, want rung 2's recovery frame", st)
	}
	wait := lossWait{active: true, gen: 1, from: 120, rung: 2}
	queued := ladderIn{event: lossOutgoing, gen: 1, seq: 121, live: 1, mode: proto.RecoveryLTR, wait: wait}
	answer := queued
	answer.seq, answer.recovery, answer.refFloor = 128, true, 119
	if st := ladder(queued); st.act != actDiscard {
		t.Fatalf("seq 121 behind the lost seq 120: %+v, want discarded", st)
	}
	if st := ladder(answer); !st.endsWait {
		t.Fatalf("the recovery frame: %+v, want the end of the wait", st)
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

// TestOwnerLogNoFirstImage: the log's first session had no picture for 54 s
// (helper started 10:30:40.682, "encoder ready ... startup=53.969s", five
// stream stats with fps=0) and nothing said why: the display was most likely
// asleep, and DDA hands out nothing then. The helper's first-image watchdog
// reports captureChanged "noimage" after 2 s and every 10 s while it lasts
// (having tried to wake the display), and "restored" with the first image:
// the session logs both and tells the client while the picture is missing.
func TestOwnerLogNoFirstImage(t *testing.T) {
	r := newP5Rig(t, fakeSVCH264, proto.HelloVersionRecovery, Config{}, "seamless")
	const why = "no image from the capture for 2 s (display off or PC locked?); woke the display"
	r.f.Send(encoder.CaptureChanged{Reason: "noimage", Text: why})
	waitMsg(t, r.ctrl, `"t":"notice"`, `"level":"warn"`, "No picture from the PC's screen yet: "+why)
	r.f.Send(encoder.CaptureChanged{Reason: "noimage", Text: "no image from the capture for 12 s (display off or PC locked?); woke the display"})
	waitMsg(t, r.ctrl, `"t":"notice"`, "for 12 s")
	r.publish(t, &encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: p5Key, CaptureQPC: 2, OutputQPC: 3})
	r.f.Send(encoder.CaptureChanged{Reason: "restored", Width: 320, Height: 180, Text: "first image after 12.6 s"})
	waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`)
	for deadline := time.Now().Add(5 * time.Second); len(r.logs.lines(`reason=restored`)) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no log of the first image")
		}
	}
	if l := r.logs.lines(`msg="capture changed" reason=noimage`); len(l) != 2 || !strings.Contains(l[0], why) {
		t.Fatalf("log %q", l)
	}
	if l := r.logs.lines(`msg="capture changed" reason=restored size=320x180`); len(l) != 1 || !strings.Contains(l[0], "first image after 12.6 s") {
		t.Fatalf("log %q", l)
	}
	if n := strings.Count(strings.Join(r.ctrl.messages(t), "\n"), `"t":"notice"`); n != 2 {
		t.Fatalf("%d notices, want the two for the missing picture", n)
	}
}
