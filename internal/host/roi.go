package host

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// Regions of interest where the player looks (GUIDE 9 order 3, Phase 5
// "sharper crosshair / cursor"). The native helper's encoders take a region of
// interest map (AMF ROI_DATA importance per 64x64 block, H.264 16x16; NVENC a
// QP delta map beside its spatial AQ: the helper's choice, since NVENC's
// emphasis map proper is H.264-only and needs AQ off), so the encoder spends
// more of the frame's bits around the pointer or a game's crosshair. The host
// knows where the player looks from the input path: the client's absolute
// pointer positions (desktop mouse mode: normalised across the picture it
// shows, which is the captured picture) put a square around the pointer; its
// relative motion (game mouse mode, pointer lock) one around the host's own
// pointer while that shows on the picture (a game's menu or inventory, a
// strategy or point-and-click game: the client draws the host's pointer there,
// stream.js onCursorPos), else (the game hides the pointer and draws a
// crosshair at the centre) one around the picture's centre, taking some bits
// from the rest. The helper's captures report no pointer position of their own
// (DDA's pointer shape and position are not used, the pointer is drawn by the
// client), so the host's pointer is polled here (pollHostCursor, as
// cursorLoop does for the client).
//
// Host config "roi": "auto" (default) follows the input as above, nothing
// before the first pointer input; "cursor" only the pointer square (where the
// pointer was last seen), "center" only the centre square, "off" nothing.
// HelperVideo.SetFocus maps the focus to the stream (encoder.FocusROI with
// its capture and encoded sizes) and re-applies it to every helper it starts;
// only pipelines whose encoder has a map (PipelineCaps.ROI) get it. The map
// changes at most every roiInterval and only when the pointer moved by more
// than roiMove across the picture or the kind of focus changed, so pointer
// events (up to 1000 per second) never reach the encoder one by one.

const (
	// roiInterval: the focus goes to the pipeline at most this often (each
	// change makes the encoder build a new map; AMF allocates a surface).
	roiInterval = 100 * time.Millisecond
	// roiMinGap is the shortest gap roiFocus.next allows between two foci:
	// roiInterval less a tenth for the ticker's jitter (a tick a little late
	// and the next one on time are less than roiInterval apart, and a moving
	// pointer must not wait a second tick).
	roiMinGap = roiInterval - roiInterval/10
	// roiMove: the pointer square moves only when the pointer moved by more
	// than this across the picture (1/32 of its width or height, 60 x 34 px
	// at 1920x1080: a quarter of the square), so motion inside the square
	// changes nothing.
	roiMove = 65535 / 32
	// roiCenterBackground is the rest of the picture's weight beside the
	// centre square (a game: bits from the scene for the crosshair; AMF
	// importance 4 of 10, NVENC +2 QP). The pointer square leaves the rest
	// alone: a desktop has text everywhere.
	roiCenterBackground = -2
)

// roiFocus follows the pointer input for the encoder's regions of interest.
type roiFocus struct {
	mode string // host config "roi": auto ("" too) | cursor | center | off

	mu sync.Mutex
	// x, y: where the pointer the player follows was last seen, 0..65535
	// across the picture (seen: at all): the client's absolute position, or
	// the host's pointer while it shows under pointer lock.
	seen      bool
	x, y      uint16
	abs, rel  time.Time // when the last absolute position / relative motion arrived
	hostShown bool      // the host's pointer shows on the picture (the last poll)
	sent      media.Focus
	sentAt    time.Time
	have      bool   // the pipeline has been handed sent (it keeps it for the helpers it starts)
	logged    string // the last logged decision
	kind      string // the last logged kind of focus (roiTick only)
}

// pointerAbs records an absolute pointer position (datagram DgMouseAbs).
func (r *roiFocus) pointerAbs(x, y uint16, now time.Time) {
	r.mu.Lock()
	r.x, r.y, r.abs, r.seen = x, y, now, true
	r.mu.Unlock()
}

// pointerRel records relative pointer motion (DgMouseRel: pointer lock).
func (r *roiFocus) pointerRel(now time.Time) {
	r.mu.Lock()
	r.rel = now
	r.mu.Unlock()
}

// hostCursor records the host's own pointer (pollHostCursor, every roiTick):
// whether it shows on the picture and where, 0..65535 across it. Under
// pointer lock a pointer that shows (a game's menu or inventory, a strategy
// game) is the one the player follows: the client draws it there.
func (r *roiFocus) hostCursor(shown bool, x, y uint16) {
	r.mu.Lock()
	r.hostShown = shown
	if shown && r.locked() {
		r.x, r.y, r.seen = x, y, true
	}
	r.mu.Unlock()
}

// locked reports whether relative motion is the latest pointer input
// (pointer lock). Called with r.mu held.
func (r *roiFocus) locked() bool { return !r.rel.IsZero() && r.rel.After(r.abs) }

// want is the focus the mode and the input ask for. Called with r.mu held.
func (r *roiFocus) want() media.Focus {
	pointer := media.Focus{Pointer: true, X: r.x, Y: r.y}
	center := media.Focus{Center: true, Background: roiCenterBackground}
	switch r.mode {
	case roiCursor:
		if r.seen {
			return pointer
		}
	case roiCenter:
		return center
	case settingAuto, "":
		switch {
		case r.locked() && !r.hostShown:
			return center // pointer lock, the pointer hidden: a game's crosshair
		case r.seen:
			return pointer
		}
	}
	return media.Focus{}
}

// next returns the focus to hand the pipeline at now and whether to: one of
// another kind than the last one handed over, or whose pointer moved by more
// than roiMove, at most every roiInterval (roiMinGap: one per roiLoop tick).
func (r *roiFocus) next(now time.Time) (media.Focus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.want()
	switch {
	case !r.have && f == (media.Focus{}): // nothing to set or to clear
		return f, false
	case r.have && !r.moved(f):
		return f, false
	case r.have && now.Sub(r.sentAt) < roiMinGap:
		return f, false
	}
	r.sent, r.sentAt, r.have = f, now, true
	return f, true
}

// moved reports whether f differs enough from the focus last handed over.
// Called with r.mu held.
func (r *roiFocus) moved(f media.Focus) bool {
	s := r.sent
	if f.Pointer != s.Pointer || f.Center != s.Center || f.Background != s.Background {
		return true
	}
	diff := func(a, b uint16) int { return max(int(a)-int(b), int(b)-int(a)) }
	return f.Pointer && (diff(f.X, s.X) > roiMove || diff(f.Y, s.Y) > roiMove)
}

// decision logs whether the session uses regions of interest, once per
// change.
func (r *roiFocus) decision(log *slog.Logger, used bool, reason string) {
	text := fmt.Sprint(used, reason)
	r.mu.Lock()
	same := text == r.logged
	r.logged = text
	r.mu.Unlock()
	if same {
		return
	}
	mode := r.mode
	if mode == "" {
		mode = settingAuto
	}
	if used {
		log.Info("regions of interest", "roi", mode, "used", true)
	} else {
		log.Info("regions of interest not used", "roi", mode, "reason", reason)
	}
}

// kindOf names a focus for the log.
func kindOf(f media.Focus) string {
	switch {
	case f.Pointer && f.Center:
		return "pointer and centre"
	case f.Pointer:
		return "around the pointer"
	case f.Center:
		return "around the centre (pointer lock: a crosshair)"
	}
	return "none"
}

// roiLoop hands the focus of the pointer input to the pipeline (roiTick)
// until the session ends or runs on FFmpeg for good.
func (s *Session) roiLoop() {
	if s.roi.mode == settingOff {
		s.roi.decision(s.log, false, `host config "roi" is "off"`)
		return
	}
	t := time.NewTicker(roiInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if !s.roiTick(time.Now()) {
			return
		}
	}
}

// roiTick hands the focus to the pipeline when it changed (roiFocus.next)
// and the pipeline's encoder has a region of interest map, logging that
// decision once per change and the kind of focus when it changes. It returns
// false when the session runs on FFmpeg, which it never leaves.
func (s *Session) roiTick(now time.Time) bool {
	pc := s.vid().Capabilities()
	switch {
	case pc.Name == media.PipelineFFmpeg:
		s.roi.decision(s.log, false, "the FFmpeg pipeline has no region of interest map")
		return false
	case pc.Recovery == media.RecoveryNone:
		return true // nothing streams yet
	case !pc.ROI:
		s.roi.decision(s.log, false, "the encoder has no region of interest map (helper caps roi none, or it refused one)")
		return true
	}
	s.roi.decision(s.log, true, "")
	s.roi.hostCursor(s.pollHostCursor())
	f, ok := s.roi.next(now)
	if !ok {
		return true
	}
	if k := kindOf(f); k != s.roi.kind {
		s.roi.kind = k
		s.log.Info("regions of interest: focus", "focus", k)
	}
	if err := s.vid().SetFocus(f); err != nil {
		s.log.Debug("regions of interest", "err", err)
	}
	return true
}

// pollHostCursor reports whether the host's pointer shows on the captured
// monitor and where, normalised across it as cursorLoop sends it to the
// client. Nothing where the picture has no host pointer (capture "test", not
// Windows) or the poll fails.
func (s *Session) pollHostCursor() (shown bool, x, y uint16) {
	if !s.a.cursorSupported() {
		return false, 0, 0
	}
	cs, err := platform.GetCursor()
	s.prefsMu.Lock()
	m := s.monitor
	s.prefsMu.Unlock()
	if err != nil || !cs.Visible || m.W <= 1 || m.H <= 1 ||
		cs.X < m.X || cs.X >= m.X+m.W || cs.Y < m.Y || cs.Y >= m.Y+m.H {
		return false, 0, 0
	}
	return true, clampU16((cs.X - m.X) * 65535 / (m.W - 1)), clampU16((cs.Y - m.Y) * 65535 / (m.H - 1))
}
