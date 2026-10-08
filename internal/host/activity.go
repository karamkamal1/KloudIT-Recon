package host

import (
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
)

// Static desktop bitrate (GUIDE 9 order 3, Phase 5 "dirty rects let the rate
// controller cut bitrate on a static desktop"). The native helper's captures
// report the share of the picture that changed with every frame
// (media.Frame.Dirty: DDA and AMD Direct Capture dirty rects, carried through
// the ring as Stats.Dirty's dirtyPpm). An encoder.ActivityMeter follows it
// over a second: while at most 0.2 % changes (a caret, a clock; the pointer is
// not in the helper's captures) the encoder's target goes down to a static
// floor (host config "staticKbps": by default a quarter of the rate
// controller's target, at least 2000 kbit/s), linearly back up to the target
// between 0.2 % and 5 %. A CBR encoder then spends less on refining a picture
// that does not change, and the media congestion controller paces at the lower
// rate, so fewer bytes go on the wire. The full target comes back with the
// first frame that changes (in videoEvents, before that frame is queued, so it
// already goes out at the full pacing rate), with no ramp: the rate controller
// keeps its own target meanwhile (this caps what the encoder is told, never
// what the controller decides: always the lower of the two, so it never fights
// the congestion control). While capped the VBV stays the size of one frame at
// the full target (vbvFrames target / cap), so the first frame with motion,
// encoded before anyone knows it moved, is not starved of bits. Only where the
// pipeline changes its bitrate seamlessly in the running encoder
// (PipelineCaps.LiveBitrate without flush: a cap that costs key frames or
// restarts is worth nothing); FFmpeg reports no dirty share, and an unknown
// share (WGC, older helpers) never lowers anything. Host config
// "staticBitrate" "off" turns it off.

const (
	// staticCutGap: a lower static bitrate waits this long after the last
	// change of the encoder's rate (and after a generation went live); a
	// higher one goes to the encoder at once.
	staticCutGap = time.Second
	// staticStep: changes by less than this share wait (in the linear range
	// every frame moves the suggestion a little), except a restore of the
	// full target.
	staticStep = 0.1
	// staticMaxVBV bounds the VBV in frame intervals (the helper's limit).
	staticMaxVBV = 30
)

// staticCap is the session's static-desktop cap on the encoder's bitrate.
// Its lock is held across the pipeline's SetRate by both writers (the frames
// in videoEvents, the rate controller's changes in Session.setRate), so the
// bitrate it last set is the encoder's.
type staticCap struct {
	now     func() time.Time // nil: time.Now (tests use a fake clock)
	mu      sync.Mutex
	on      bool // host config "staticBitrate" auto
	kbps    int  // host config "staticKbps" (0: default)
	meter   encoder.ActivityMeter
	sent    int       // the encoder's bitrate as the session last set it (0: none live yet)
	capped  bool      // sent is below the rate controller's target for a static desktop
	changed time.Time // when sent last changed
}

func (c *staticCap) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// floor is a static desktop's bitrate at the rate controller's target.
func (c *staticCap) floor(target int) int {
	f := c.kbps
	if f <= 0 {
		f = max(rateFloorKbps, target/4)
	}
	return min(f, target)
}

// want returns the bitrate the encoder should run at for the rate
// controller's target now (live: the pipeline changes its bitrate
// seamlessly): the target, or less while the desktop is static, with the VBV
// size in frame intervals that keeps the VBV at one frame of the full target
// (0: the encoder's default). Called with c.mu held.
func (c *staticCap) want(now time.Time, target int, live bool) (int, float64) {
	if !c.on || !live || target <= 0 {
		return target, 0
	}
	floor := c.floor(target)
	if floor >= target {
		return target, 0
	}
	c.meter.StaticScale = float64(floor) / float64(target)
	k := c.meter.SuggestKbps(now, target, floor)
	if k >= target {
		return target, 0
	}
	return k, min(staticMaxVBV, float64(target)/float64(k))
}

// applied records the bitrate the session set for the rate controller's
// target (Session.setRate). Called with c.mu held.
func (c *staticCap) applied(now time.Time, kbps, target int) {
	if kbps != c.sent {
		c.changed = now
	}
	c.sent, c.capped = kbps, kbps < target
}

// generation records a generation going live at kbps (its VideoConfig):
// below the rate controller's target it runs capped (a helper restarted at
// a capped rate).
func (c *staticCap) generation(now time.Time, kbps, target int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent, c.capped, c.changed = kbps, kbps < target, now
}

// rate records the live encoder's bitrate as the pipeline announced it
// (VideoEvent.Rate): whoever changed it (this cap, the rate controller, a
// settings change in place), it is what the encoder runs at.
func (c *staticCap) rate(now time.Time, kbps, target int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied(now, kbps, target)
}

// staticChange is a change of the encoder's bitrate for the desktop's
// activity.
type staticChange struct {
	kbps     int
	vbv      float64
	activity float64 // the meter's activity (largest share changed per frame in the window); -1: unknown
	target   int
}

// frame records a frame's dirty share and returns the change of the
// encoder's bitrate it calls for (ok): the full target back (or more of it)
// at once when the picture changes, a cut for a static one at most every
// staticCutGap. target is the rate controller's; live as in want. Called with
// c.mu held.
func (c *staticCap) frame(now time.Time, f *media.Frame, target int, live bool) (staticChange, bool) {
	d := -1.0
	if f.HasDirty {
		d = f.Dirty
	}
	c.meter.AddShare(now, d)
	if c.sent <= 0 || !c.on || !live {
		c.capped = false // a pipeline that is not live starts its generations at the target
		return staticChange{}, false
	}
	k, vbv := c.want(now, target, live)
	switch {
	case c.capped && k > c.sent && (k >= target || float64(k) >= float64(c.sent)*(1+staticStep)):
	case k < target && float64(k) <= float64(c.sent)*(1-staticStep) && now.Sub(c.changed) >= staticCutGap:
	default:
		return staticChange{}, false
	}
	a, ok := c.meter.Activity(now)
	if !ok {
		a = -1
	}
	c.applied(now, k, target)
	return staticChange{kbps: k, vbv: vbv, activity: a, target: target}, true
}

// staticFrame feeds a frame to the static-desktop cap and puts the change it
// calls for into the running encoder (before the frame is queued: a restore
// raises the congestion controller's pacing for this very frame). Called by
// videoEvents.
func (s *Session) staticFrame(f *media.Frame) {
	v := s.vid()
	caps := v.Capabilities()
	live := caps.LiveBitrate && !caps.LiveBitrateFlush
	target, _ := s.rate.kbps()
	s.static.mu.Lock()
	wasCapped := s.static.capped
	c, ok := s.static.frame(s.static.clock(), f, target, live)
	var err error
	if ok {
		err = v.SetRate(c.kbps, 0, c.vbv)
	}
	s.static.mu.Unlock()
	if !ok {
		return
	}
	if err != nil {
		s.log.Warn("static desktop: bitrate change failed", "kbps", c.kbps, "err", err)
		return
	}
	if p, ok := v.Current(); ok {
		s.setCongestionTarget(p)
	}
	switch capped := c.kbps < c.target; {
	case capped && !wasCapped:
		s.log.Info("static desktop: lowering the bitrate", "kbps", c.kbps, "target", c.target, "vbv_frames", c.vbv,
			"activity_pct", c.activity*100)
	case !capped:
		s.log.Info("desktop changes: full bitrate back", "kbps", c.kbps, "activity_pct", c.activity*100)
	default:
		s.log.Debug("static desktop: bitrate", "kbps", c.kbps, "target", c.target, "vbv_frames", c.vbv, "activity_pct", c.activity*100)
	}
}

// isCapped reports whether the encoder runs below the rate controller's
// target for a static desktop (for the stats log).
func (c *staticCap) isCapped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capped
}
