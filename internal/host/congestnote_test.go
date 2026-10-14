package host

import (
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// TestCongestionNote: the user is told of a cut below half the ceiling that
// lasts congestionHold, once per spell and at most every congestionEvery;
// the routine cuts of a few percent (the second hardware run's LAN: 250 to
// 212.5 Mbit/s with 24 ms of delay) are not told.
func TestCongestionNote(t *testing.T) {
	var n congestionNote
	t0 := time.Now()
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	steps := []struct {
		at         float64
		kbps, ceil int
		told       bool
	}{
		{0, 212500, 250000, false},   // a routine cut
		{1, 180000, 250000, false},   // deeper, still above half
		{2, 120000, 250000, false},   // below half: the spell starts
		{8, 100000, 250000, false},   // 6 s into it
		{12.5, 110000, 250000, true}, // 10.5 s: told
		{20, 90000, 250000, false},   // the same spell: told once
		{21, 130000, 250000, false},  // back above half: the spell ends
		{22, 100000, 250000, false},  // a new spell
		{33, 100000, 250000, false},  // 11 s into it, but 20.5 s after the last notice
		{43, 100000, 250000, true},   // 30.5 s after it
		{44, 0, 0, false},            // no ceiling (not adaptive)
	}
	for _, c := range steps {
		msg := n.change(at(c.at), c.kbps, c.ceil)
		if (msg != "") != c.told {
			t.Fatalf("at %.1f s, %d of %d kbps: notice %q, want told %v", c.at, c.kbps, c.ceil, msg, c.told)
		}
		if msg != "" && !strings.HasPrefix(msg, "Network congestion: for ") {
			t.Fatalf("notice %q", msg)
		}
	}
}

// TestCongestionNoticeSession: a session whose rate controller moves the
// bitrate up and down by a few percent (a LAN with a little queueing delay)
// sends no notice; one held below half its ceiling for congestionHold sends
// one, as information, never a warning.
func TestCongestionNoticeSession(t *testing.T) {
	defer func(h time.Duration) { congestionHold = h }(congestionHold)
	congestionHold = 50 * time.Millisecond
	s, _, ctrl := testSession(t, testFaults{})
	s.video = &ladderPipeline{caps: media.PipelineCaps{LiveBitrate: true}, events: make(chan media.VideoEvent)}
	s.rate.target(20000, 60)
	kbps := 20000
	for i := 0; i < 40; i++ {
		to := min(20000, kbps*103/100)
		down := i%4 == 3
		if down {
			to = kbps * 85 / 100
		}
		s.applyRate(rateChange{fromKbps: kbps, toKbps: to, fromFPS: 60, toFPS: 60, down: down, why: "delay"}, 0)
		kbps = to
	}
	if got := noticeLines(t, ctrl); len(got) != 0 {
		t.Fatalf("routine cuts sent notices %q", got)
	}
	s.applyRate(rateChange{fromKbps: kbps, toKbps: 8000, fromFPS: 60, toFPS: 60, down: true, why: "loss"}, 0)
	time.Sleep(60 * time.Millisecond)
	s.applyRate(rateChange{fromKbps: 8000, toKbps: 7000, fromFPS: 60, toFPS: 60, down: true, why: "delay"}, 0)
	got := noticeLines(t, ctrl)
	if len(got) != 1 || !strings.HasPrefix(got[0], "info: Network congestion: for 0 s the network has carried less than half the bitrate set; "+
		"streaming at 7.0 Mbps instead of 20.0 Mbps.") {
		t.Fatalf("notices %q, want one informational notice of the deep cut", got)
	}
}
