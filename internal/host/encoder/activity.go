package encoder

import "time"

// ActivityMeter tells a static picture from a changing one by the share of
// the picture the capture reports as changed (Frame.Dirty / Stats.Dirty, from
// DDA's and AMD Direct Capture's dirty rects; GUIDE 5 "dirty rects let the
// rate controller cut bitrate on a static desktop"). Feed it every frame;
// SuggestKbps scales the bitrate ceiling down while little or nothing changes
// and gives the whole ceiling back with the first frame that has motion.
// Unknown shares (WGC, the synthetic source, older helpers) make it suggest
// the ceiling. Not safe for concurrent use.
type ActivityMeter struct {
	Window      time.Duration // how far back it looks (default 1 s)
	StaticBelow float64       // largest share that still counts as static (default 0.002: a caret, a clock, a pointer trail)
	FullAbove   float64       // share from which the whole ceiling applies (default 0.05: a scrolled window, a video)
	StaticScale float64       // share of the ceiling for a static picture (default 0.25)

	samples []activitySample
}

type activitySample struct {
	at    time.Time
	dirty float64 // -1 = unknown
}

func (m *ActivityMeter) window() time.Duration {
	if m.Window > 0 {
		return m.Window
	}
	return time.Second
}

func (m *ActivityMeter) thresholds() (static, full, scale float64) {
	static, full, scale = m.StaticBelow, m.FullAbove, m.StaticScale
	if static <= 0 {
		static = 0.002
	}
	if full <= static {
		full = max(0.05, static*2)
	}
	if scale <= 0 || scale > 1 {
		scale = 0.25
	}
	return static, full, scale
}

// Add records a frame that arrived at at. An idle repeat counts as unchanged.
func (m *ActivityMeter) Add(at time.Time, f *Frame) {
	d := f.Dirty
	if f.Repeat {
		d = 0
	}
	m.add(at, d)
}

// AddStats records a frame from its stats (Stats.Dirty; dropped frames too:
// the picture changed all the same).
func (m *ActivityMeter) AddStats(at time.Time, s Stats) {
	d := s.Dirty
	if s.Repeat {
		d = 0
	}
	m.add(at, d)
}

// AddShare records a frame by its share alone (0..1, as Frame.Dirty and
// Stats.Dirty; an idle repeat is 0), or an unknown share (negative): for
// callers that carry the share in frames of their own (recon-host's session,
// media.Frame.Dirty).
func (m *ActivityMeter) AddShare(at time.Time, dirty float64) { m.add(at, dirty) }

func (m *ActivityMeter) add(at time.Time, dirty float64) {
	if dirty < 0 {
		dirty = -1
	}
	m.samples = append(m.samples, activitySample{at: at, dirty: min(dirty, 1)})
	cut := 0
	for cut < len(m.samples) && at.Sub(m.samples[cut].at) > m.window() {
		cut++
	}
	m.samples = m.samples[cut:]
}

// Activity returns the largest share that changed in one frame over the
// window before now, and false when there was no frame or any frame's share
// is unknown.
func (m *ActivityMeter) Activity(now time.Time) (float64, bool) {
	most, seen := 0.0, false
	for _, s := range m.samples {
		if now.Sub(s.at) > m.window() {
			continue
		}
		if s.dirty < 0 {
			return 0, false
		}
		most = max(most, s.dirty)
		seen = true
	}
	return most, seen
}

// Static reports whether nothing beyond StaticBelow changed over the window.
func (m *ActivityMeter) Static(now time.Time) bool {
	a, ok := m.Activity(now)
	static, _, _ := m.thresholds()
	return ok && a < static
}

// SuggestKbps scales ceiling (the bitrate the rate controller would use) by
// the activity: StaticScale for a static picture, the whole ceiling from
// FullAbove on, linear in between; never below floor nor above ceiling.
func (m *ActivityMeter) SuggestKbps(now time.Time, ceiling, floor int) int {
	a, ok := m.Activity(now)
	if !ok || ceiling <= 0 {
		return ceiling
	}
	static, full, scale := m.thresholds()
	k := 1.0
	switch {
	case a < static:
		k = scale
	case a < full:
		k = scale + (1-scale)*(a-static)/(full-static)
	}
	return min(ceiling, max(floor, int(float64(ceiling)*k+0.5)))
}
