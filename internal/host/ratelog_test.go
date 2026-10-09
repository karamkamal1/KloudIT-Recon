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

// On a pipeline that cannot change its bitrate live (FFmpeg) each change of
// the rate controller is a new generation. Its "restarting video", and the
// generation's "starting encoder" and "encoder ready" (media.Params.Quiet),
// are debug lines: they used to add three lines at the default level per
// change, about one change a second where the path carries less than the
// setting. A restart for another reason (settings, key frame, failure) is
// still logged.
func TestRateRestartLogVolume(t *testing.T) {
	logs := &lockedLog{}
	s, p, _ := fakePipelineSession(t, slog.New(slog.NewTextHandler(logs, nil)), media.PipelineCaps{}) // the default level
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	kbps := 4000
	for i := 0; i < 30; i++ {
		to := kbps * 105 / 100
		down := i%10 == 9
		if down {
			to = kbps * 85 / 100
		}
		s.applyRate(rateChange{fromKbps: kbps, toKbps: to, fromFPS: 30, toFPS: 30, down: down, why: "delay"}, 0)
		kbps = to
	}
	p.mu.Lock()
	params := append([]media.Params(nil), p.params...)
	p.mu.Unlock()
	if len(params) != 31 || params[0].Quiet {
		t.Fatalf("%d generations (first quiet %v), want the session's and one per change", len(params), len(params) > 0 && params[0].Quiet)
	}
	for i, pr := range params[1:] {
		if !pr.Quiet {
			t.Fatalf("rate change %d: its generation is not quiet", i)
		}
	}
	if l := logs.lines(`msg="restarting video"`); len(l) != 0 {
		t.Fatalf("30 rate changes logged %d restarts at the default level, want none: %q", len(l), l)
	}
	if n := len(logs.lines("bitrate recovery: raising bitrate")) + len(logs.lines("congestion: lowering bitrate")); n != 2 {
		t.Fatalf("%d rate change lines at the default level, want 2 (rateLog)", n)
	}
	if err := s.startVideo(true, "keyframe request"); err != nil {
		t.Fatal(err)
	}
	if l := logs.lines(`msg="restarting video"`); len(l) != 1 || !strings.Contains(l[0], `reason="keyframe request"`) {
		t.Fatalf("a key-frame restart logged %q, want its line", l)
	}
	p.mu.Lock()
	quiet := p.params[len(p.params)-1].Quiet
	p.mu.Unlock()
	if quiet {
		t.Fatal("a key-frame restart's generation is quiet")
	}
}
