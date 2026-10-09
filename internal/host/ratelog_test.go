package host

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// A path that carries less than the setting keeps the rate controller
// moving: 2 % steps up a few hundred milliseconds apart, a cut, up again
// (about 2.6 changes a second in the simulation of a 20 Mbit/s link under a
// 50 Mbit/s setting). At the default log level each change used to make two
// lines (applyRate's and setRate's "changing the bitrate in the encoder"),
// about 3 MB of host.log an hour; now a direction logs at most once per
// rateLogEvery, with the count of the changes in between, and a frame-rate
// step always.
func TestRateChangeLogVolume(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	logs := &lockedLog{}
	s.log = slog.New(slog.NewTextHandler(logs, nil)) // the default level
	s.video = &ladderPipeline{caps: media.PipelineCaps{LiveBitrate: true}, events: make(chan media.VideoEvent)}
	kbps := 20000
	for i := 0; i < 30; i++ {
		to := kbps * 102 / 100
		down := i%10 == 9
		if down {
			to = kbps * 85 / 100
		}
		s.applyRate(rateChange{fromKbps: kbps, toKbps: to, fromFPS: 60, toFPS: 60, down: down, why: "delay"}, 0)
		kbps = to
	}
	ups, downs := logs.lines(`msg="bitrate recovery: raising bitrate"`), logs.lines(`msg="congestion: lowering bitrate"`)
	if len(ups) != 1 || len(downs) != 1 || len(logs.lines("changing the bitrate in the encoder")) != 0 {
		t.Fatalf("30 changes within a second logged %d raises, %d cuts, %d encoder lines at the default level, want 1, 1, 0",
			len(ups), len(downs), len(logs.lines("changing the bitrate in the encoder")))
	}
	// A frame-rate step is logged whenever it comes.
	s.applyRate(rateChange{fromKbps: kbps, toKbps: kbps, fromFPS: 60, toFPS: 50, down: true, why: "delay"}, 0)
	if l := logs.lines(`msg="congestion: lowering bitrate"`); len(l) != 2 || !strings.Contains(l[1], "fps=50") || !strings.Contains(l[1], "suppressed=2") {
		t.Fatalf("frame-rate step: %q, want it logged with the two cuts before it counted", l)
	}

	// The window per direction, on the controller's clock.
	var l rateLog
	t0 := time.Now()
	up := rateChange{fromFPS: 60, toFPS: 60}
	logged, counted := 0, 0
	for i := 0; i < 100; i++ { // 2 % every 300 ms for 30 s
		lvl, n := l.level(up, t0.Add(time.Duration(i)*300*time.Millisecond), slog.LevelInfo)
		if lvl == slog.LevelInfo {
			logged++
			counted += n
		}
	}
	if logged != 3 || counted != 66 {
		t.Fatalf("30 s of raises 300 ms apart: %d logged, %d counted as suppressed; want 3 (at 0, 10.2 and 20.4 s) counting 33 each", logged, counted)
	}
	if lvl, n := l.level(rateChange{down: true, fromFPS: 60, toFPS: 60}, t0.Add(29*time.Second), slog.LevelWarn); lvl != slog.LevelWarn || n != 0 {
		t.Fatalf("the first cut after raises: level %v, %d suppressed; want logged (its own direction)", lvl, n)
	}
}
