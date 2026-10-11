package encoder

import "time"

// ActivityMeter tells a static picture from a changing one by the share of
// the picture the capture reports as changed (Frame.Dirty / Stats.Dirty, from
// DDA's and AMD Direct Capture's dirty rects; GUIDE 5 "dirty rects let the
// rate controller cut bitrate on a static desktop") and by how often the
// capture has a new image at all. Feed it every frame; SuggestKbps scales the
// bitrate ceiling down while little or nothing changes and gives the whole
// ceiling back with the first frame that has motion.
//
// A still picture reaches the encoder as idle repeats (Frame.Repeat: nothing
// was presented for idleRepeatMs) and the odd new image of a caret or a
// clock. A game or a video presents new images all the time, whatever their
// dirty share says: a game that flips past the compositor (independent flip,
// DirectFlip) reaches DDA with no dirty rects at all, which helpers before
// the first hardware run's fix reported as unchanged (docs/VENDOR_NOTES.md
// "First hardware run: the static desktop during a game"). So more than
// QuietImages new images in the window is motion, whatever the share.
//
// Unknown shares (WGC, the synthetic source, older helpers) make it suggest
// the ceiling. It also keeps the encoded sizes it is given (OutputKbps, what
// the encoder made: for the log; it decides nothing, the bitrate a cut sets
// bounds it). Not safe for concurrent use.
type ActivityMeter struct {
	Window      time.Duration // how far back it looks (default 1 s)
	StaticBelow float64       // largest share that still counts as static (default 0.002: a caret, a clock, a pointer trail)
	FullAbove   float64       // share from which the whole ceiling applies (default 0.05: a scrolled window, a video)
	StaticScale float64       // share of the ceiling for a static picture (default 0.25)
	// QuietImages is the most new images (frames that are not idle
	// repeats) per Window of a picture that is still (default 5: a caret
	// blinks about twice a second, a clock ticks once).
	QuietImages int

	samples []activitySample
}

type activitySample struct {
	at    time.Time
	dirty float64 // -1 = unknown
	image bool    // a new image, not an idle repeat
	bytes int     // encoded size (0: unknown, or a key frame: its size is the encoder's, not the picture's)
}

func (m *ActivityMeter) window() time.Duration {
	if m.Window > 0 {
		return m.Window
	}
	return time.Second
}

func (m *ActivityMeter) quietImages() int {
	if m.QuietImages > 0 {
		return m.QuietImages
	}
	return 5
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
	n := len(f.Data)
	if f.Key {
		n = 0
	}
	m.AddFrame(at, d, f.Repeat, n)
}

// AddStats records a frame from its stats (Stats.Dirty; dropped frames too:
// the picture changed all the same).
func (m *ActivityMeter) AddStats(at time.Time, s Stats) {
	d := s.Dirty
	if s.Repeat {
		d = 0
	}
	n := int(s.Bytes)
	if s.Key {
		n = 0
	}
	m.AddFrame(at, d, s.Repeat, n)
}

// AddFrame records a frame by its share (0..1, as Frame.Dirty and
// Stats.Dirty; negative: unknown), whether it is an idle repeat (unchanged
// whatever its share says), and its
// encoded size (0: unknown; callers leave key frames out): for callers that
// carry these in frames of their own (recon-host's session, media.Frame).
func (m *ActivityMeter) AddFrame(at time.Time, dirty float64, repeat bool, bytes int) {
	switch {
	case repeat:
		dirty = 0 // an idle repeat is the previous image again
	case dirty < 0:
		dirty = -1
	}
	m.samples = append(m.samples, activitySample{at: at, dirty: min(dirty, 1), image: !repeat, bytes: max(0, bytes)})
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

// Images returns how many new images (frames that are not idle repeats) the
// window before now holds.
func (m *ActivityMeter) Images(now time.Time) int {
	n := 0
	for _, s := range m.samples {
		if now.Sub(s.at) <= m.window() && s.image {
			n++
		}
	}
	return n
}

// OutputKbps returns the encoded size of the window's frames (key frames
// left out) as a rate over the window (kbit/s).
func (m *ActivityMeter) OutputKbps(now time.Time) float64 {
	b := 0
	for _, s := range m.samples {
		if now.Sub(s.at) <= m.window() {
			b += s.bytes
		}
	}
	return float64(b) * 8 / 1000 / m.window().Seconds()
}

// Static reports whether nothing beyond StaticBelow changed over the window,
// with at most QuietImages new images in it.
func (m *ActivityMeter) Static(now time.Time) bool {
	a, ok := m.Activity(now)
	static, _, _ := m.thresholds()
	return ok && a < static && m.Images(now) <= m.quietImages()
}

// SuggestKbps scales ceiling (the bitrate the rate controller would use) by
// the activity: StaticScale for a static picture, the whole ceiling from
// FullAbove on, linear in between; never below floor nor above ceiling. More
// than QuietImages new images in the window (a game, a video) is the whole
// ceiling, whatever their share.
func (m *ActivityMeter) SuggestKbps(now time.Time, ceiling, floor int) int {
	a, ok := m.Activity(now)
	if !ok || ceiling <= 0 || m.Images(now) > m.quietImages() {
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
