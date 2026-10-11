package encoder

import (
	"image"
	"reflect"
	"testing"
	"time"
)

// The rects pinned here are the ones the helper's --self-test-encoder ("ROI
// maps of the cursor / crosshair rects") turns into AMF importance and NVENC
// QP delta maps: keep both in step.
func TestFocusROI(t *testing.T) {
	got := FocusROI(1920, 1080, 1920, 1080, &image.Point{X: 100, Y: 100}, FocusOptions{Background: -2})
	want := []ROIRect{{0, 0, 1920, 1080, -2}, {33, 33, 135, 135, 6}, {870, 450, 180, 180, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("1080p: %+v, want %+v", got, want)
	}
	// The pointer in the bottom-left corner: its square clipped.
	got = FocusROI(1920, 1080, 1920, 1080, &image.Point{X: 5, Y: 1075}, FocusOptions{})
	want = []ROIRect{{0, 1008, 73, 72, 6}, {870, 450, 180, 180, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("corner: %+v, want %+v", got, want)
	}
	// A 4K capture streamed at 1080p: the squares scale with the stream
	// (sizes from the source height: 270 / 360 source pixels; the pointer
	// square [2865, 3135) rounds outwards to [1432, 1568)).
	got = FocusROI(3840, 2160, 1920, 1080, &image.Point{X: 3000, Y: 200}, FocusOptions{})
	want = []ROIRect{{1432, 32, 136, 136, 6}, {870, 450, 180, 180, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("4K -> 1080p: %+v, want %+v", got, want)
	}
	// Odd scaling rounds outwards: 1000 -> 333 wide, the pointer square
	// [450, 550) becomes [149, 184).
	got = FocusROI(1000, 1000, 333, 333, &image.Point{X: 500, Y: 500}, FocusOptions{CursorSize: 100, CenterSize: -1, CursorWeight: 12})
	if len(got) != 1 || got[0] != (ROIRect{149, 149, 35, 35, 10}) {
		t.Fatalf("outward rounding / weight clamp: %+v", got)
	}
	// No pointer, no centre: nothing (not a background alone).
	if got := FocusROI(1920, 1080, 1920, 1080, nil, FocusOptions{CenterSize: -1, Background: -3}); got != nil {
		t.Fatalf("nothing to emphasise: %+v", got)
	}
	// A pointer outside the captured monitor: centre only; no stream size: nil.
	if got := FocusROI(1920, 1080, 1920, 1080, &image.Point{X: -500, Y: 100}, FocusOptions{}); len(got) != 1 || got[0].X != 870 {
		t.Fatalf("pointer outside: %+v", got)
	}
	if FocusROI(1920, 1080, 0, 0, nil, FocusOptions{}) != nil {
		t.Fatal("rects without a stream size")
	}
	// Every rect is something the helper accepts (x, y >= 0, w, h > 0, weight -10..10).
	for _, cx := range []int{-200, 0, 1, 959, 1919, 1920, 3000} {
		for _, r := range FocusROI(1920, 1080, 1280, 720, &image.Point{X: cx, Y: cx / 2}, FocusOptions{Background: -20}) {
			if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > 1280 || r.Y+r.H > 720 || r.Weight < -10 || r.Weight > 10 {
				t.Fatalf("pointer %d: invalid rect %+v", cx, r)
			}
		}
	}
}

func TestActivityMeter(t *testing.T) {
	var m ActivityMeter
	t0 := time.Unix(1000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	if _, ok := m.Activity(t0); ok || m.Static(t0) || m.SuggestKbps(t0, 20000, 2000) != 20000 {
		t.Fatal("no frames: not unknown")
	}
	// A static desktop: idle repeats every 100 ms, a blinking caret's new
	// image every 500 ms.
	for ms := 0; ms <= 1000; ms += 100 {
		f := &Frame{Repeat: true, Dirty: -1, Data: make([]byte, 300)} // a repeat counts as unchanged whatever it says
		if ms%500 == 0 {
			f = &Frame{Dirty: 40.0 / (1920 * 1080), Data: make([]byte, 600)}
		}
		m.Add(at(ms), f)
	}
	now := at(1000)
	if !m.Static(now) || m.SuggestKbps(now, 20000, 2000) != 5000 || m.Images(now) != 3 {
		t.Fatalf("static: %v, %d kbps, %d images", m.Static(now), m.SuggestKbps(now, 20000, 2000), m.Images(now))
	}
	if o := m.OutputKbps(now); o != float64(8*300+2*600+600)*8/1000 {
		t.Fatalf("output %.1f kbps", o)
	}
	if m.SuggestKbps(now, 20000, 8000) != 8000 {
		t.Fatal("floor ignored")
	}
	// A window opens: 30 % changed -> the whole ceiling at once.
	m.Add(at(1050), &Frame{Dirty: 0.3})
	if m.Static(at(1050)) || m.SuggestKbps(at(1050), 20000, 2000) != 20000 {
		t.Fatal("motion did not restore the ceiling")
	}
	// ... and is remembered for the window (1 s), then static again.
	for ms := 1100; ms <= 2000; ms += 100 {
		m.Add(at(ms), &Frame{Repeat: true})
	}
	if k := m.SuggestKbps(at(2000), 20000, 2000); k != 20000 {
		t.Fatalf("window: %d kbps at 2.0 s", k)
	}
	m.Add(at(2100), &Frame{Repeat: true})
	if k := m.SuggestKbps(at(2100), 20000, 2000); k != 5000 {
		t.Fatalf("window: %d kbps at 2.1 s", k)
	}
	// A game whose presents report nothing changed (a game flipping past
	// the compositor, as helpers before the first hardware run's fix
	// reported it) or a caret-sized change: 60 new images a second is
	// motion, whatever the share. Six in a second already are; five are not.
	var game ActivityMeter
	for i := 0; i <= 60; i++ {
		game.AddFrame(at(i*16), 0, false, 0)
	}
	if game.Static(at(960)) || game.SuggestKbps(at(960), 20000, 2000) != 20000 || game.Images(at(960)) != 61 {
		t.Fatalf("a game's presents without dirty rects: static %v, %d kbps", game.Static(at(960)), game.SuggestKbps(at(960), 20000, 2000))
	}
	var few ActivityMeter
	for i := 0; i < 5; i++ {
		few.AddFrame(at(i*200), 0.0001, false, 0)
	}
	if !few.Static(at(800)) {
		t.Fatal("five new images a second not still")
	}
	few.AddFrame(at(900), 0.0001, false, 0)
	if few.Static(at(900)) || few.SuggestKbps(at(900), 20000, 2000) != 20000 {
		t.Fatal("six new images a second still")
	}
	// Small changes scale linearly between StaticBelow and FullAbove.
	var lin ActivityMeter
	lin.AddStats(t0, Stats{Dirty: 0.026})
	if k := lin.SuggestKbps(t0, 20000, 0); k != 12500 {
		t.Fatalf("2.6 %% changed: %d kbps, want 12500", k)
	}
	// An unknown share (WGC, an older helper) suggests the ceiling.
	lin.AddStats(t0, Stats{Dirty: -1})
	if _, ok := lin.Activity(t0); ok || lin.SuggestKbps(t0, 20000, 0) != 20000 {
		t.Fatal("unknown share not reported as unknown")
	}
	// AddFrame: the share, the repeat and the size alone, as recon-host's
	// frames carry them; a negative share is unknown, a repeat unchanged.
	var sh ActivityMeter
	sh.AddFrame(t0, 0.001, false, 1000)
	sh.AddFrame(t0.Add(10*time.Millisecond), -1, true, 1000)
	if !sh.Static(t0.Add(10*time.Millisecond)) || sh.SuggestKbps(t0, 20000, 2000) != 5000 || sh.OutputKbps(t0.Add(10*time.Millisecond)) != 16 {
		t.Fatal("AddFrame: static share not static")
	}
	sh.AddFrame(t0.Add(20*time.Millisecond), -1, false, 0)
	if _, ok := sh.Activity(t0.Add(20 * time.Millisecond)); ok {
		t.Fatal("AddFrame: negative share not unknown")
	}
}

func TestDroppable(t *testing.T) {
	for _, c := range []struct {
		f    Frame
		want bool
	}{
		{Frame{Discardable: true, TemporalLayer: 1}, true},
		{Frame{TemporalLayer: 1}, false},                                    // a reference frame in layer 1
		{Frame{Discardable: true, Recovery: true, TemporalLayer: 1}, false}, // the client waits for it after a loss
		{Frame{Discardable: true, Key: true}, false},
	} {
		if got := c.f.Droppable(); got != c.want {
			t.Errorf("%+v: Droppable %v, want %v", c.f, got, c.want)
		}
	}
}

func TestFPSSteps(t *testing.T) {
	for _, c := range []struct{ fps, floor, want int }{
		{120, 30, 100}, {100, 30, 90}, {90, 60, 75}, {60, 60, 60}, {144, 30, 120}, {30, 30, 30}, {61, 30, 60}, {240, 200, 240},
	} {
		if got := LowerFPS(c.fps, c.floor); got != c.want {
			t.Errorf("LowerFPS(%d, %d) = %d, want %d", c.fps, c.floor, got, c.want)
		}
	}
	for _, c := range []struct{ fps, ceiling, want int }{
		{60, 120, 75}, {100, 120, 120}, {90, 95, 95}, {120, 120, 120}, {30, 144, 45},
	} {
		if got := RaiseFPS(c.fps, c.ceiling); got != c.want {
			t.Errorf("RaiseFPS(%d, %d) = %d, want %d", c.fps, c.ceiling, got, c.want)
		}
	}
}

func TestEncoderInstanceFor(t *testing.T) {
	amf := CodecCaps{HWInstances: 2, InstanceSelect: true}
	nvenc := CodecCaps{HWInstances: 2}
	one := CodecCaps{HWInstances: 1, InstanceSelect: true}
	val := func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	}
	for _, c := range []struct {
		choice string
		cc     CodecCaps
		want   int // -1 = nil
		err    bool
	}{
		{"", amf, -1, false}, {"default", amf, -1, false}, {"auto", amf, -1, false}, {"auto", nvenc, -1, false},
		{"dedicated", amf, 1, false}, {"dedicated", nvenc, -1, false}, {"dedicated", one, -1, false},
		{"0", amf, 0, false}, {" 1 ", amf, 1, false}, {"2", amf, -1, true}, {"-1", amf, -1, true},
		{"1", nvenc, -1, true}, {"0", nvenc, -1, true}, {"fast", amf, -1, true}, {"0", one, 0, false},
	} {
		got, err := EncoderInstanceFor(c.choice, c.cc)
		if val(got) != c.want || (err != nil) != c.err {
			t.Errorf("EncoderInstanceFor(%q, %+v) = %d, %v; want %d, error %v", c.choice, c.cc, val(got), err, c.want, c.err)
		}
	}
}
