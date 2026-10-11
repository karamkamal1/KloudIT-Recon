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
// the ring as Stats.Dirty's dirtyPpm) and which frames are idle repeats of
// the last image (media.Frame.Repeat: nothing was presented for idleRepeatMs).
// An encoder.ActivityMeter follows both over a second (and the encoder's
// output, for the log). The picture is still while at most 0.2 % changes per
// frame (a caret, a clock; the pointer is not in the helper's captures) and
// the capture has at most five new images a second (a caret blinks about
// twice a second; a game or a video presents far more often, whatever its
// dirty rects say). Once the picture has been still for staticHold (3 s),
// the encoder's target goes down to a static floor (host config
// "staticKbps": by default a quarter of the rate controller's target, at
// least 2000 kbit/s), linearly back up to the target between 0.2 % and 5 %
// changed. A CBR encoder then spends less on refining a picture that does
// not change, and the media congestion controller paces at the lower rate,
// so fewer bytes go on the wire. What the encoder makes decides nothing: it
// is what the cut lowers (a cut that wanted the encoder under half the floor
// kept it under half the cap, so it saved nothing, and an encoder spending
// more on a still picture was never cut), and a game or a video is told by
// its new images.
//
// The first hardware run cut a running game this way every second or two:
// the game flipped past the compositor, DDA handed its presents out without
// dirty rects, the helper read that as nothing changed, and every few
// seconds a frame the compositor did draw restored the bitrate, with a burst
// of large frames after each restore (docs/VENDOR_NOTES.md "First hardware
// run: the static desktop during a game"). Since then the helper counts a
// present that names no region as the whole picture, the new-image count
// must agree with the share, and the cut waits for a still picture of
// several seconds: a cut the picture ends within staticShortCut doubles the
// next hold (at most staticHoldMax), so content that changes every few
// seconds is not cut and restored over and over.
//
// A frame that changes raises the bitrate at once (in videoEvents, before
// that frame is queued, so it already goes out at the new pacing rate), with
// no ramp: to the full target from 5 % changed (a scrolled window, a video)
// or with more than five new images a second, by the linear share below
// that (a small window update on a still desktop: about 37 % of the target
// at 1 %). The rate controller keeps its
// own target meanwhile (this caps what the encoder is told, never what the
// controller decides: always the lower of the two, so it never fights the
// congestion control), and it is told the encoder runs at that target
// (controllerKbps): its measurements are in units of its target. While
// capped the VBV stays the size of one frame at the full target (vbvFrames
// target / cap), so the first frame with motion, encoded before anyone knows
// it moved, is meant to be no larger than a frame at the full target, which
// the pacer raised for it carries on schedule (the frame queue overflows
// only where the network does not keep up: queueOverflows). An encoder can
// overshoot it: the first hardware run's AMF made about three such frames'
// worth after a capture loss, which went past its deadline (rung 1) and cost
// a recovery frame (TestOwnerLogFirstOverflow). Only where the pipeline
// changes its bitrate seamlessly in the running encoder
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
	// staticHold: the picture must be still this long before the first cut.
	// staticShortCut, staticHoldMax: a cut the picture ended within
	// staticShortCut doubles the hold before the next one, up to
	// staticHoldMax; a cut that lasted longer sets it back to staticHold.
	staticHold     = 3 * time.Second
	staticShortCut = 10 * time.Second
	staticHoldMax  = 30 * time.Second
)

// staticCap is the session's static-desktop cap on the encoder's bitrate.
// Its lock is held across the pipeline's SetRate by both writers (the frames
// in videoEvents, the rate controller's changes in Session.setRate), so the
// bitrate it last set is the encoder's.
type staticCap struct {
	now     func() time.Time // nil: time.Now (tests use a fake clock)
	mu      sync.Mutex
	on      bool          // host config "staticBitrate" auto
	kbps    int           // host config "staticKbps" (0: default)
	hold    time.Duration // the first hold before a cut (0: staticHold; tests shorten it)
	meter   encoder.ActivityMeter
	sent    int       // the encoder's bitrate as the session last set it (0: none live yet)
	capped  bool      // sent is below the rate controller's target for a static desktop
	changed time.Time // when sent last changed
	// stillSince: since when the picture has been still (zero: it is not);
	// holdFor: how long it must be before a cut (0: hold), doubled by cuts
	// the picture ended soon (staticShortCut); cutAt: when the encoder last
	// went below the target.
	stillSince time.Time
	holdFor    time.Duration
	cutAt      time.Time
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

// holdTime is how long the picture must be still before a cut now. Called
// with c.mu held.
func (c *staticCap) holdTime() time.Duration {
	switch {
	case c.holdFor > 0:
		return c.holdFor
	case c.hold > 0:
		return c.hold
	}
	return staticHold
}

// observe updates whether the picture is still at the rate controller's
// target: the meter suggests less than the target (few new images, little
// changed). The encoder's output is not asked: under a cut it is at most the
// cap, and a still picture the encoder spends much on is what the cut is
// for. Called with c.mu held.
func (c *staticCap) observe(now time.Time, target int) {
	floor := c.floor(target)
	still := false
	if target > 0 && floor < target {
		c.meter.StaticScale = float64(floor) / float64(target)
		still = c.meter.SuggestKbps(now, target, floor) < target
	}
	switch {
	case !still:
		c.stillSince = time.Time{}
	case c.stillSince.IsZero():
		c.stillSince = now
	}
}

// want returns the bitrate the encoder should run at for the rate
// controller's target now (live: the pipeline changes its bitrate
// seamlessly): the target, or less once the desktop has been still for the
// hold (and while it stays still), with the VBV size in frame intervals that
// keeps the VBV at one frame of the full target (0: the encoder's default).
// Called with c.mu held.
func (c *staticCap) want(now time.Time, target int, live bool) (int, float64) {
	if !c.on || !live || target <= 0 {
		return target, 0
	}
	floor := c.floor(target)
	if floor >= target || c.stillSince.IsZero() || !c.capped && now.Sub(c.stillSince) < c.holdTime() {
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
// target (Session.setRate), and when a cut started. Called with c.mu held.
func (c *staticCap) applied(now time.Time, kbps, target int) {
	if kbps != c.sent {
		c.changed = now
	}
	capped := kbps < target
	if capped && !c.capped {
		c.cutAt = now
	}
	c.sent, c.capped = kbps, capped
}

// generation records a generation going live at kbps (its VideoConfig):
// below the rate controller's target it runs capped (a helper restarted at
// a capped rate). It returns the bitrate for the rate controller (live as in
// want; controllerKbps).
func (c *staticCap) generation(now time.Time, kbps, target int, live bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent, c.capped, c.changed = kbps, kbps < target, now
	return c.controllerKbps(kbps, target, live)
}

// rate records the live encoder's bitrate as the pipeline announced it
// (VideoEvent.Rate): whoever changed it (this cap, the rate controller, a
// settings change in place), it is what the encoder runs at. It returns the
// bitrate for the rate controller (live as in want; controllerKbps).
func (c *staticCap) rate(now time.Time, kbps, target int, live bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied(now, kbps, target)
	return c.controllerKbps(kbps, target, live)
}

// controllerKbps is the bitrate the rate controller is told the encoder runs
// at (rateController.live) when it runs at kbps: the controller's target
// while this cap holds the encoder below it. The controller measures in
// units of its target (the encoder's fill, the delivered rates divided by
// it, the queue's growth from the encoder's rate): told the capped rate, a
// static desktop's few bytes would read as a full encoder at the cap, so any
// decrease would start from the cap (a halving) and no increase could pass
// 1.2 x the cap. Called with c.mu held.
func (c *staticCap) controllerKbps(kbps, target int, live bool) int {
	if c.on && live && c.capped {
		return target
	}
	return kbps
}

// staticChange is a change of the encoder's bitrate for the desktop's
// activity, with what the meter saw (for the log).
type staticChange struct {
	kbps     int
	vbv      float64
	activity float64 // the meter's activity (largest share changed per frame in the window); -1: unknown
	images   int     // new images in the meter's window
	output   float64 // the encoder's output over the window (kbit/s, key frames left out)
	still    time.Duration
	hold     time.Duration // the hold before the next cut (after a restore)
	target   int
}

// frame records a frame (its dirty share, whether it is an idle repeat, its
// size) and returns the change of the encoder's bitrate it calls for (ok):
// the full target back (or more of it) at once when the picture changes, a
// cut for a still one after the hold and at most every staticCutGap. target
// is the rate controller's; live as in want. Called with c.mu held.
func (c *staticCap) frame(now time.Time, f *media.Frame, target int, live bool) (staticChange, bool) {
	d := -1.0
	if f.HasDirty {
		d = f.Dirty
	}
	n := len(f.Data)
	if f.Key {
		n = 0 // a key frame's size is the encoder's, not the picture's
	}
	c.meter.AddFrame(now, d, f.Repeat, n)
	if c.sent <= 0 || !c.on || !live {
		c.capped = false // a pipeline that is not live starts its generations at the target
		c.stillSince = time.Time{}
		return staticChange{}, false
	}
	c.observe(now, target)
	still := time.Duration(0)
	if !c.stillSince.IsZero() {
		still = now.Sub(c.stillSince)
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
	if c.capped && k >= target {
		// The picture ended the cut: one it ended within staticShortCut
		// doubles the next hold (content that changes every few seconds),
		// a longer one sets it back.
		h := c.holdTime()
		c.holdFor = 0
		if now.Sub(c.cutAt) < staticShortCut {
			c.holdFor = min(2*h, max(staticHoldMax, h))
		}
	}
	c.applied(now, k, target)
	return staticChange{kbps: k, vbv: vbv, activity: a, images: c.meter.Images(now), output: c.meter.OutputKbps(now),
		still: still, hold: c.holdTime(), target: target}, true
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
	// images: new images in the last second; output_kbps: the encoder's
	// output over it; still_ms: how long the picture had been still;
	// next_hold_ms: how long it must be still before the next cut.
	switch capped := c.kbps < c.target; {
	case capped && !wasCapped:
		s.log.Info("static desktop: lowering the bitrate", "kbps", c.kbps, "target", c.target, "vbv_frames", c.vbv,
			"activity_pct", c.activity*100, "images", c.images, "output_kbps", int(c.output), "still_ms", c.still.Milliseconds())
	case !capped:
		s.log.Info("desktop changes: full bitrate back", "kbps", c.kbps, "activity_pct", c.activity*100, "images", c.images,
			"output_kbps", int(c.output), "next_hold_ms", c.hold.Milliseconds())
	default:
		s.log.Debug("static desktop: bitrate", "kbps", c.kbps, "target", c.target, "vbv_frames", c.vbv, "activity_pct", c.activity*100,
			"images", c.images, "output_kbps", int(c.output))
	}
}

// isCapped reports whether the encoder runs below the rate controller's
// target for a static desktop (for the stats log).
func (c *staticCap) isCapped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capped
}
