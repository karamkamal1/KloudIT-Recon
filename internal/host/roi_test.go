package host

import (
	"context"
	"image"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// Phase 5 session wiring, part B: regions of interest from the pointer input
// (roi.go).

// TestROIFocus: what each host config "roi" mode asks for from the pointer
// input (absolute positions: the pointer; relative motion, pointer lock: the
// host's pointer while it shows, else the centre), and when it goes to the
// pipeline: at once the first time, then only for another kind of focus or a
// pointer that moved by more than roiMove, at most every roiInterval.
func TestROIFocus(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	pointer := func(x, y uint16) media.Focus { return media.Focus{Pointer: true, X: x, Y: y} }
	center := media.Focus{Center: true, Background: roiCenterBackground}
	type step struct {
		ms       int
		abs, rel bool   // input at ms before the tick
		x, y     uint16 // abs position
		shown    bool   // the host's pointer shows at hx, hy (polled each tick)
		hx, hy   uint16
		want     *media.Focus
	}
	none := media.Focus{}
	for _, c := range []struct {
		mode  string
		steps []step
	}{
		{"auto", []step{
			{ms: 0, want: nil}, // no input yet: nothing
			{ms: 10, abs: true, x: 32768, y: 32768, want: ptr(pointer(32768, 32768))},
			{ms: 300, abs: true, x: 32768 + roiMove, y: 32768, want: nil},                                      // within the square
			{ms: 400, abs: true, x: 32768, y: 32768 - roiMove - 1, want: ptr(pointer(32768, 32768-roiMove-1))}, // moved enough
			{ms: 450, abs: true, x: 0, y: 0, want: nil},                                                        // only 50 ms later
			{ms: 500, want: ptr(pointer(0, 0))},                                                                // the interval is over
			{ms: 700, rel: true, want: ptr(center)},                                                            // pointer lock
			{ms: 900, rel: true, want: nil},                                                                    // still the centre
			{ms: 1000, abs: true, x: 0, y: 0, want: ptr(pointer(0, 0))},                                        // back to the desktop
		}},
		{"auto", []step{ // pointer lock with the host's pointer showing (a menu, a strategy game)
			{ms: 0, rel: true, shown: true, hx: 1000, hy: 60000, want: ptr(pointer(1000, 60000))},
			{ms: 100, rel: true, shown: true, hx: 1000 + roiMove, hy: 60000, want: nil}, // within the square
			{ms: 200, rel: true, shown: true, hx: 30000, hy: 60000, want: ptr(pointer(30000, 60000))},
			{ms: 300, rel: true, want: ptr(center)},                                                       // the game hides it: a crosshair
			{ms: 400, rel: true, shown: true, hx: 30000, hy: 60000, want: ptr(pointer(30000, 60000))},     // a menu again
			{ms: 500, abs: true, x: 5, y: 5, shown: true, hx: 30000, hy: 60000, want: ptr(pointer(5, 5))}, // desktop mode: the client's position
			{ms: 600, shown: true, hx: 60000, hy: 60000, want: nil},                                       // the host's pointer counts under pointer lock only
		}},
		{"", []step{ // "" is auto
			{ms: 0, rel: true, want: ptr(center)},
		}},
		{"cursor", []step{
			{ms: 0, rel: true, want: nil}, // no pointer position yet
			{ms: 10, abs: true, x: 100, y: 200, want: ptr(pointer(100, 200))},
			{ms: 500, rel: true, want: nil},                                                       // relative motion: the pointer square stays
			{ms: 600, rel: true, shown: true, hx: 40000, hy: 200, want: ptr(pointer(40000, 200))}, // the host's pointer shows
			{ms: 700, rel: true, want: nil},                                                       // hidden again: it stays
		}},
		{"center", []step{
			{ms: 0, want: ptr(center)},
			{ms: 500, abs: true, x: 100, y: 100, want: nil},
		}},
		{"off", []step{
			{ms: 0, abs: true, x: 100, y: 100, want: nil},
			{ms: 500, rel: true, want: nil},
		}},
	} {
		r := &roiFocus{mode: c.mode}
		for i, s := range c.steps {
			switch {
			case s.abs:
				r.pointerAbs(s.x, s.y, at(s.ms))
			case s.rel:
				r.pointerRel(at(s.ms))
			}
			r.hostCursor(s.shown, s.hx, s.hy) // as roiTick polls it
			f, ok := r.next(at(s.ms))
			if ok != (s.want != nil) || ok && f != *s.want {
				t.Fatalf("mode %q step %d (%d ms): %+v %v, want %+v", c.mode, i, s.ms, f, ok, s.want)
			}
		}
	}
	// Clearing: once something was handed over, a focus on nothing goes too
	// (cursor mode never clears; auto never either: a lost pointer keeps its
	// last place). Here: a session whose mode the test changes.
	r := &roiFocus{mode: roiCenter}
	if _, ok := r.next(at(0)); !ok {
		t.Fatal("centre not handed over")
	}
	r.mode = settingOff
	if f, ok := r.next(at(500)); !ok || f != none {
		t.Fatalf("off after the centre: %+v %v, want the regions cleared", f, ok)
	}
	// A burst of pointer events (1000 per second, each far from the last)
	// reaches the pipeline once per roiLoop tick, every roiInterval: ticks
	// that jitter (one 4 ms late, the next on time, the time.Now() roiLoop
	// reads) take every change, and polls between ticks (every 10 ms, up to
	// 80 ms after one) none.
	r = &roiFocus{mode: settingAuto}
	sent, ticks := 0, 0
	for ms := 0; ms < 1000; ms++ {
		r.pointerAbs(uint16(ms*7919%65536), 100, at(ms)) // jumps across the picture
		tick := ms%100 == 4*((ms/100)%2)                 // 0, 104, 200, 304, ...
		if !tick && (ms%10 != 0 || ms%100 < 10 || ms%100 > 80) {
			continue
		}
		_, ok := r.next(at(ms))
		switch {
		case tick:
			ticks++
			if !ok {
				t.Fatalf("tick at %d ms: the moved pointer waits a tick", ms)
			}
		case ok:
			t.Fatalf("poll at %d ms between ticks: focus handed over", ms)
		}
		if ok {
			sent++
		}
	}
	if sent < 9 || sent > 10 || ticks != 10 {
		t.Fatalf("%d focus changes in a second of pointer events (%d ticks), want at most %d", sent, ticks, time.Second/roiInterval)
	}
}

func ptr(f media.Focus) *media.Focus { return &f }

// TestSessionROI: a session on a fake helper whose encoder has a region of
// interest map turns the pointer input into setRoi messages (mapped to the
// stream: the fake starts 320x180), rate-limited, the centre square under
// pointer lock; the decision and the kind of focus are logged once.
func TestSessionROI(t *testing.T) {
	codec := `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",` +
		`"roi":"importance","alignW":1,"alignH":1}`
	r := newP5Rig(t, codec, 4, Config{}, "seamless")
	s := r.s
	r.publish(t, svcFrame(1)) // the stream is live
	if !s.vid().Capabilities().ROI {
		t.Fatalf("capabilities %+v: want ROI", s.vid().Capabilities())
	}
	t0 := time.Unix(3_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	// noSetRoi checks that no setRoi reached the helper before a key frame
	// request it is asked for now.
	noSetRoi := func(why string) {
		t.Helper()
		if err := s.vid().ForceKeyframe(); err != nil {
			t.Fatal(err)
		}
		for {
			m := expectFakeMsgAny(t, r.f)
			if m["t"] == "setRoi" {
				t.Fatalf("%s: setRoi %v", why, m["rects"])
			}
			if m["t"] == "forceIdr" {
				return
			}
		}
	}
	if !s.roiTick(at(0)) {
		t.Fatal("roiTick ended on the helper pipeline")
	}
	noSetRoi("no input yet")
	s.roi.pointerAbs(65535, 65535, at(10))
	s.roiTick(at(10))
	want := encoder.FocusROI(320, 180, 320, 180, &image.Point{X: 319, Y: 179}, encoder.FocusOptions{CenterSize: -1})
	if got := fakeRects(t, expectFakeMsg(t, r.f, "setRoi")); !slices.Equal(got, want) {
		t.Fatalf("pointer at the bottom right: %v, want %v", got, want)
	}
	for ms := 11; ms < 100; ms++ { // a burst within the interval
		s.roi.pointerAbs(uint16(ms*100), 0, at(ms))
		s.roiTick(at(ms))
	}
	noSetRoi("within the interval")
	s.roi.pointerRel(at(150))
	s.roiTick(at(150))
	want = encoder.FocusROI(320, 180, 320, 180, nil, encoder.FocusOptions{Background: roiCenterBackground})
	if got := fakeRects(t, expectFakeMsg(t, r.f, "setRoi")); !slices.Equal(got, want) {
		t.Fatalf("pointer lock: %v, want %v", got, want)
	}
	if l := r.logs.lines(`msg="regions of interest" roi=auto used=true`); len(l) != 1 {
		t.Fatalf("decision logged %d times", len(l))
	}
	for _, k := range []string{"focus=\"around the pointer\"", "focus=\"around the centre"} {
		if l := r.logs.lines(k); len(l) != 1 {
			t.Fatalf("%s logged %d times", k, len(l))
		}
	}
}

// expectFakeMsgAny returns the next message the fake helper received.
func expectFakeMsgAny(t *testing.T, f *encoder.Fake) map[string]any {
	t.Helper()
	select {
	case m := <-f.Messages():
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("the helper got no message")
	}
	return nil
}

func fakeRects(t *testing.T, m map[string]any) []encoder.ROIRect {
	t.Helper()
	var out []encoder.ROIRect
	for _, x := range m["rects"].([]any) {
		o := x.(map[string]any)
		out = append(out, encoder.ROIRect{X: int(o["x"].(float64)), Y: int(o["y"].(float64)), W: int(o["w"].(float64)),
			H: int(o["h"].(float64)), Weight: int(o["weight"].(float64))})
	}
	return out
}

// TestROITickPipelines: no regions on FFmpeg (the loop ends, logged once), nor
// on a helper whose encoder has no map (logged once, the loop goes on), and
// none before anything streams; "off" ends at once.
func TestROITickPipelines(t *testing.T) {
	logs := &lockedLog{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newS := func(caps media.PipelineCaps, mode string) *Session {
		return &Session{video: &ladderPipeline{caps: caps}, ctx: ctx, roi: roiFocus{mode: mode},
			log: slog.New(slog.NewTextHandler(logs, nil))}
	}
	now := time.Unix(1, 0)
	s := newS(media.PipelineCaps{Name: media.PipelineFFmpeg, Recovery: media.RecoveryKeyframe}, settingAuto)
	s.roi.pointerAbs(1, 1, now)
	if s.roiTick(now) || s.roiTick(now) {
		t.Fatal("roiTick goes on with FFmpeg")
	}
	if l := logs.lines("FFmpeg pipeline has no region of interest map"); len(l) != 1 {
		t.Fatalf("FFmpeg decision logged %d times", len(l))
	}
	s = newS(media.PipelineCaps{Name: media.PipelineHelper, Recovery: media.RecoveryNone}, settingAuto)
	if !s.roiTick(now) || len(logs.lines("helper caps roi none")) != 0 {
		t.Fatal("a decision before anything streams")
	}
	s = newS(media.PipelineCaps{Name: media.PipelineHelper, Recovery: media.RecoveryKeyframe}, roiCenter)
	for i := 0; i < 3; i++ {
		if !s.roiTick(now.Add(time.Duration(i) * time.Second)) {
			t.Fatal("roiTick ended on the helper pipeline")
		}
	}
	if l := logs.lines("helper caps roi none"); len(l) != 1 {
		t.Fatalf("no-map decision logged %d times", len(l))
	}
	done := make(chan struct{})
	go func() { newS(media.PipelineCaps{}, settingOff).roiLoop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(`roiLoop runs with "roi" "off"`)
	}
	if l := logs.lines(`roi=off reason="host config`); len(l) != 1 {
		t.Fatalf("off logged %q", l)
	}
}
