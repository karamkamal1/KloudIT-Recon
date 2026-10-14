package host

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// Screen capture on a PC without GPU capture (first hardware report: a
// Proxmox VM with only the Microsoft Basic Display Adapter). FFmpeg's GPU
// captures (ddagrab, gfxcapture) need a Direct3D 11 video device and fail
// every start without one; GDI (gdigrab) works on any interactive desktop.
// The agent learns which applies from the probe (media.CheckGPUCapture) and
// from sessions whose GPU capture failed that way, and captures with GDI
// instead, without a loop of failures: "auto", "ddagrab" and "gfxcapture"
// alike. A session whose Desktop Duplication keeps refusing its display (a
// remote desktop session) moves to GDI for itself. Capture failures the
// same capture gets past (the secure desktop, a mode change) keep it.
//
// A GPU PC can look like a PC without one for a while: while its graphics
// driver resets (TDR) or is updated, Windows shows the Basic Display Adapter
// and D3D11VA device creation fails the same way, and so does an agent that
// started at a remote desktop logon (RDP sessions render on the Basic Render
// Driver). GDI keeps such a stream going, and whatever moved a session to GDI
// is checked again while it streams (watchGPUCapture), so a GPU PC goes back
// to GPU capture once it works. gdigrab captures a fixed rectangle:
// watchGDIDisplay restarts it when the display's resolution or layout
// changes.

// gpuCaptureRecheck is how long a finding of no GPU capture holds before a
// session start checks again (in the background: the session that starts
// does not wait, its watch takes the result: watchGPUCapture). A GPU driver
// installed, or one that was being updated when the finding was made,
// brings GPU capture back without an agent restart. Tests shorten it.
var gpuCaptureRecheck = time.Minute

// checkGPUCapture is media.CheckGPUCapture, checkDDA media.CheckDDA and
// remoteSession platform.RemoteSession (tests replace them).
var (
	checkGPUCapture = media.CheckGPUCapture
	checkDDA        = media.CheckDDA
	remoteSession   = platform.RemoteSession
)

// gpuCaptureState is what the agent knows about FFmpeg's GPU capture on this
// PC beyond the probe's finding (media.Caps.GPUCaptureBlocked).
type gpuCaptureState struct {
	mu       sync.Mutex
	found    bool          // why is a finding of the agent's (else the probe's stands)
	why      string        // why GPU capture cannot work ("" = it can)
	at       time.Time     // when the finding in force was made, or the last check started
	checking bool          // a check runs (checkGPUCaptureAgain)
	done     chan struct{} // closed when the check that runs (or ran last) ends
}

// gpuCaptureBlocked returns why FFmpeg's GPU capture (ddagrab, gfxcapture)
// cannot work on this PC, or "" when it can (as far as the agent knows).
func (a *Agent) gpuCaptureBlocked() string {
	a.gpuCap.mu.Lock()
	defer a.gpuCap.mu.Unlock()
	if a.gpuCap.found || a.caps == nil {
		return a.gpuCap.why
	}
	return a.caps.GPUCaptureBlocked()
}

// noGPUCapture records that a session's GPU capture found no Direct3D 11
// video device: the sessions after it capture with GDI from the start.
func (a *Agent) noGPUCapture(why string) {
	a.gpuCap.mu.Lock()
	defer a.gpuCap.mu.Unlock()
	a.gpuCap.found, a.gpuCap.why, a.gpuCap.at = true, why, time.Now()
}

// recheckGPUCapture checks the agent's finding that GPU capture cannot work
// again, in the background, when it is older than gpuCaptureRecheck (a
// session starts: the sessions after it, and its own watch, use the result).
func (a *Agent) recheckGPUCapture() { a.checkGPUCaptureAgain(gpuCaptureRecheck) }

// checkGPUCaptureAgain checks the agent's finding that GPU capture cannot
// work again, in the background, unless the last check started less than
// age ago (a check is stamped when it starts, so a watch that asks every
// period runs one every period), and returns a channel that is closed when
// the check ends: the one it started, or the one that runs already (one at
// a time); nil when there is no finding or no check is due. Only a capture
// that opens lifts the finding: one that fails for another reason (the
// secure desktop) or times out leaves it to the next check.
func (a *Agent) checkGPUCaptureAgain(age time.Duration) <-chan struct{} {
	if a.caps == nil || a.caps.FFmpeg == "" || a.gpuCaptureBlocked() == "" {
		return nil
	}
	a.gpuCap.mu.Lock()
	defer a.gpuCap.mu.Unlock()
	if a.gpuCap.checking {
		return a.gpuCap.done
	}
	if time.Since(a.gpuCap.at) < age {
		return nil
	}
	done := make(chan struct{})
	a.gpuCap.checking, a.gpuCap.at, a.gpuCap.done = true, time.Now(), done
	go func() {
		blocked, detail := checkGPUCapture(context.Background(), a.caps.FFmpeg)
		ok := blocked == "" && detail == ""
		a.gpuCap.mu.Lock()
		a.gpuCap.checking = false
		if ok {
			a.gpuCap.found, a.gpuCap.why = true, ""
		}
		a.gpuCap.mu.Unlock()
		close(done)
		switch {
		case ok:
			a.log.Info("GPU screen capture works again: sessions capture with it")
		case blocked != "":
			a.log.Debug("GPU screen capture still unavailable", "reason", blocked)
		default:
			a.log.Debug("GPU screen capture check inconclusive, sessions keep capturing with GDI", "detail", detail)
		}
	}()
	return done
}

// gpuCapture reports whether backend captures on the GPU: FFmpeg's ddagrab,
// gfxcapture and AMD Direct Capture (amf, which falls back to ddagrab).
func gpuCapture(backend string) bool {
	return backend == "ddagrab" || backend == "gfxcapture" || backend == "amf"
}

// gdiFor returns gdigrab for a GPU capture backend when GPU capture cannot
// work on this PC and this FFmpeg has gdigrab, else backend.
func (a *Agent) gdiFor(backend string) string {
	if gpuCapture(backend) && a.caps.CanGDIGrab() && a.gpuCaptureBlocked() != "" {
		return "gdigrab"
	}
	return backend
}

// sessionGDI returns why the session captures with GDI after Desktop
// Duplication refused its display (captureFailed), or "".
func (s *Session) sessionGDI() string {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.gdi
}

// ddaRefusalWindow is how long Desktop Duplication must keep refusing a
// session's display, with no live generation in between, before the session
// captures with GDI (captureFailed), outside a remote desktop session: a
// display that is not there for a moment (a monitor switched off, to
// another input or re-plugged, a topology change) refuses too, and
// Microsoft documents DXGI_ERROR_UNSUPPORTED as one to retry after a mode
// change. The restarts until then retry Desktop Duplication.
var ddaRefusalWindow = 5 * time.Second

// captureFailed moves the session's next generations to GDI capture
// (gdigrab) when its FFmpeg generation's GPU capture failed in a way the same
// capture does not get past, and reports whether it did:
//   - no Direct3D 11 video device (media.CaptureNoDevice): at once, and for
//     the sessions after this one too (the agent's finding);
//   - Desktop Duplication refusing the display (media.CaptureUnsupported):
//     at once in a remote desktop session (remoteSession), which it refuses
//     every time; elsewhere once the refusals have lasted ddaRefusalWindow
//     without a live generation in between, or when the next failure would
//     end the session. For this session only.
//
// Either way watchGPUCapture checks again while the session streams and
// goes back to GPU capture once it works. Anything else (the secure desktop,
// a lost or changed display, an encoder fault) keeps the capture, which the
// restart retries; it also ends a run of refusals. The switch is logged
// once with FFmpeg's line (the "encoder failed" line before it has FFmpeg's
// whole error); the user's notice comes with the next generation's
// parameters (noteGDI).
func (s *Session) captureFailed(ev media.VideoEvent) bool {
	if ev.Failed == nil {
		return false
	}
	src, cf := ev.Failed.Source.Backend, ev.CaptureFailure
	if src != "ddagrab" && src != "gfxcapture" {
		return false
	}
	scope := "this PC"
	var attrs []any
	switch {
	case cf.Fault == media.CaptureNoDevice:
		s.a.noGPUCapture(cf.Text)
	case cf.Fault == media.CaptureUnsupported && src == "ddagrab":
		remote := remoteSession()
		s.prefsMu.Lock()
		if s.ddaRefused == 0 {
			s.ddaRefusedAt = time.Now()
		}
		s.ddaRefused++
		refused, lasted := s.ddaRefused, time.Since(s.ddaRefusedAt)
		again := remote || lasted >= ddaRefusalWindow || s.failures >= maxVideoFailures
		if again && s.a.caps.CanGDIGrab() {
			s.gdi = cf.Text
		}
		s.prefsMu.Unlock()
		if !again {
			return false
		}
		scope = "this session"
		attrs = []any{"refusals", refused, "over", lasted.Round(time.Millisecond), "remote_session", remote}
	default:
		s.prefsMu.Lock()
		s.ddaRefused = 0
		s.prefsMu.Unlock()
		return false
	}
	if !s.a.caps.CanGDIGrab() {
		s.log.Warn("GPU screen capture failed and this FFmpeg has no gdigrab to capture with instead", "capture", src, "reason", cf.Text,
			"ffmpeg", cf.Line)
		return false
	}
	s.log.Warn("GPU screen capture failed: capturing with GDI (gdigrab)",
		append([]any{"for", scope, "capture", src, "reason", cf.Text, "ffmpeg", cf.Line}, attrs...)...)
	return true
}

// noteGDI logs, and tells the user (an informational notice), once per
// reason why the session's next generation captures with GDI (backend
// gdigrab) where it would capture on the GPU: no GPU capture on this PC, or
// Desktop Duplication refusing the session's display (captureFailed); and
// starts the session's watch that goes back to the GPU (watchGPUCapture).
// Not when the host config asks for gdigrab. window: the client asked for a
// window, which GDI does not capture alone.
func (s *Session) noteGDI(backend string, window bool) {
	if backend != "gdigrab" || s.a.cfg.Capture == "gdigrab" {
		return
	}
	why, pc := s.a.gpuCaptureBlocked(), true
	if why == "" {
		why, pc = s.sessionGDI(), false
	}
	if why == "" {
		return // an FFmpeg build with no GPU capture at all
	}
	s.watchGPUCapture()
	s.prefsMu.Lock()
	repeat := why == s.gdiNoted
	s.gdiNoted = why
	s.prefsMu.Unlock()
	if repeat {
		return
	}
	attrs := []any{"reason", why, "config", s.a.cfg.Capture}
	if window {
		attrs = append(attrs, "window", "not captured alone: GDI captures the whole monitor")
	}
	s.log.Info("capturing with GDI (gdigrab)", attrs...)
	// Informational: the stream works, this says how (an error or warning
	// toast for a fallback that is handled read as a failure on a VM).
	if pc {
		s.notice("info", "No GPU screen capture on this PC ("+why+"): capturing with GDI, which uses more CPU and adds latency.")
	} else {
		s.notice("info", "Screen capture on the GPU failed ("+why+"): capturing with GDI until it works again, which uses more CPU and adds latency.")
	}
}

// gpuCaptureWatchEvery is how often a session that captures with GDI where
// it would capture on the GPU checks whether GPU capture works again
// (watchGPUCapture; tests shorten it).
var gpuCaptureWatchEvery = time.Minute

// watchGPUCapture runs from the session's first generation that captures
// with GDI where it would capture on the GPU (noteGDI) to the session's end,
// so a PC with a GPU does not stay on GDI. While the session's generation
// captures with GDI, once every gpuCaptureWatchEvery, it checks again what
// moved it there: the agent's finding of no Direct3D 11 video device (the
// probe's or a session's: a PC without a GPU, a graphics driver reset or
// update, an agent started at a remote desktop logon; checkGPUCaptureAgain,
// shared with session starts), else Desktop Duplication refusing the
// session's display (sessionGDI: ddagrab on its output, ddaCapturesAgain).
// Once that capture opens it restarts the video on the GPU at once
// (overlapped, so the picture does not stop). Its first look takes the
// result of a check that runs already: a session start's, of a finding a
// minute old (an agent started at a remote desktop logon, streaming from
// the console after tscon). On a PC without a GPU the check fails in a
// fraction of a second. One watch per session.
func (s *Session) watchGPUCapture() {
	if !s.gpuWatch.CompareAndSwap(false, true) {
		return
	}
	s.watches.Add(1)
	go func() {
		defer s.watches.Done()
		t := time.NewTicker(gpuCaptureWatchEvery)
		defer t.Stop()
		for first := true; s.ctx.Err() == nil; first = false {
			if !first {
				select {
				case <-s.ctx.Done():
					return
				case <-t.C:
				}
			}
			s.gpuCaptureBack(first)
		}
	}()
}

// gpuCaptureBack is one look of watchGPUCapture (first: its first, from the
// session's first GDI generation's parameters).
func (s *Session) gpuCaptureBack(first bool) {
	onGDI := func() bool {
		p, ok := s.vid().Current()
		return ok && p.Source.Backend == "gdigrab" && !s.paused.Load()
	}
	if !first && !onGDI() {
		return // on the GPU, between generations, or hidden (the resume starts afresh)
	}
	if s.a.gpuCaptureBlocked() != "" {
		age := gpuCaptureWatchEvery / 2 // once a period, whoever asks
		if first {
			age = gpuCaptureRecheck // the session start's check, or one that is due
		}
		if done := s.a.checkGPUCaptureAgain(age); done != nil {
			select {
			case <-s.ctx.Done():
				return
			case <-done:
			}
		}
		if s.a.gpuCaptureBlocked() != "" {
			return
		}
	}
	if s.sessionGDI() != "" && (first || !s.ddaCapturesAgain()) {
		return // a first look comes right after the refusals
	}
	s.prefsMu.Lock()
	mon := s.monitor
	s.prefsMu.Unlock()
	if !onGDI() || s.captureBackend(s.currentPrefs(), mon, s.onVirtualDisplay()) == "gdigrab" {
		return // not started yet, or nothing on the GPU to go back to
	}
	s.prefsMu.Lock()
	s.gdiNoted = "" // a later loss is told again
	s.prefsMu.Unlock()
	s.log.Info("GPU screen capture works again: restarting the video on it")
	s.notice("info", "GPU screen capture works again: capturing on the GPU.")
	if err := s.startVideo(false, "GPU screen capture works again"); err != nil {
		s.log.Warn("restart on GPU screen capture failed", "err", err)
	}
}

// ddaOutput is ddagrab's output_idx for monitor mon: its output on DXGI
// adapter 0, else its index.
func ddaOutput(mon platform.Monitor) int {
	if mon.DXGIOutput < 0 {
		return mon.Index
	}
	return mon.DXGIOutput
}

// ddaCapturesAgain checks whether Desktop Duplication captures the session's
// display again (ddagrab on its output, as the session's generations open
// it) after it moved to GDI for Desktop Duplication's refusals
// (sessionGDI), and if so clears that.
func (s *Session) ddaCapturesAgain() bool {
	s.prefsMu.Lock()
	out := ddaOutput(s.monitor)
	s.prefsMu.Unlock()
	ok, _, detail := checkDDA(s.ctx, s.a.caps.FFmpeg, out)
	if !ok {
		s.log.Debug("Desktop Duplication still cannot capture the session's display", "output", out, "detail", detail)
		return false
	}
	s.prefsMu.Lock()
	s.gdi, s.ddaRefused = "", 0
	s.prefsMu.Unlock()
	s.log.Info("Desktop Duplication captures the session's display again", "output", out)
	return true
}

// gdiDisplayEvery is how often a session that captures with GDI looks at its
// monitor's geometry (watchGDIDisplay; tests shorten it).
var gdiDisplayEvery = time.Second

// watchGDIDisplay runs from the session's first GDI generation to its end.
// gdigrab captures the rectangle it was given when it started and never
// looks again: when the display's resolution or layout changes (a VM's
// display raised after connecting, a remote desktop client resizing its
// session, tscon to the console) it keeps capturing the old rectangle,
// black or cropped, and does not fail. Every gdiDisplayEvery, while the
// session's generation captures with GDI, it compares that rectangle with
// the session's monitor (gdiDisplayMoved), and when they still differ after
// resizeSettle restarts the video (overlapped, so the picture does not
// stop): the new generation captures the new rectangle and input maps to
// it, as after the native helper's capture changes (captureChanged) and
// ddagrab's restarts after a mode change. One watch per session.
func (s *Session) watchGDIDisplay() {
	if !s.gdiWatch.CompareAndSwap(false, true) {
		return
	}
	s.watches.Add(1)
	go func() {
		defer s.watches.Done()
		t := time.NewTicker(gdiDisplayEvery)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
			if _, moved := s.gdiDisplayMoved(); !moved {
				continue
			}
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(resizeSettle):
			}
			how, moved := s.gdiDisplayMoved()
			if !moved {
				continue // a restart came first, or it changed back
			}
			s.log.Info("display changed", "monitor", how)
			if err := s.startVideo(false, "display changed"); err != nil {
				s.log.Warn("restart after a display change failed", "err", err)
			}
		}
	}()
}

// gdiDisplayMoved reports whether the session's generation captures with
// GDI another rectangle than its monitor has now (monitorNow), and says
// how: "WxH at (X,Y) is now W'xH' at (X',Y')", or "... is gone".
func (s *Session) gdiDisplayMoved() (string, bool) {
	p, ok := s.vid().Current()
	if !ok || p.Source.Backend != "gdigrab" || s.paused.Load() {
		return "", false
	}
	src := p.Source
	mon, listed := s.monitorNow()
	if listed && mon.X == src.X && mon.Y == src.Y && mon.W&^1 == src.NativeW && mon.H&^1 == src.NativeH {
		return "", false
	}
	how := fmt.Sprintf("%dx%d at (%d,%d) is ", src.NativeW, src.NativeH, src.X, src.Y)
	if !listed {
		return how + "gone", true
	}
	return how + fmt.Sprintf("now %dx%d at (%d,%d)", mon.W, mon.H, mon.X, mon.Y), true
}

// monitorNow returns the monitor the session's next generation captures
// (captureMonitor without its side effects): its virtual display (false
// when Windows no longer lists it), else the one prefs name.
func (s *Session) monitorNow() (platform.Monitor, bool) {
	s.vdMu.Lock()
	d := s.vd
	s.vdMu.Unlock()
	if d == nil {
		return s.a.monitorFor(s.currentPrefs()), true
	}
	return d.Info().Find(s.a.monitors())
}

// restartNoticeRepeat is how long an identical restart notice is held back
// after a generation went live since it was sent (restartNotice).
const restartNoticeRepeat = time.Minute

// restartNotice tells the user that the video restarts after a failure. An
// identical notice is not repeated until a generation has gone live since,
// and a minute has passed: a capture that fails every restart (the secure
// desktop for a while) or a stream that keeps failing after a moment shows
// one notice, not a stack of the same one. host.log has every failure.
func (s *Session) restartNotice(msg string) {
	s.prefsMu.Lock()
	repeat := msg == s.restartNote && (!s.liveSinceNote || time.Since(s.restartNoteAt) < restartNoticeRepeat)
	if !repeat {
		s.restartNote, s.restartNoteAt, s.liveSinceNote = msg, time.Now(), false
	}
	s.prefsMu.Unlock()
	if repeat {
		s.log.Debug("notice not repeated", "msg", msg)
		return
	}
	s.notice("warn", msg)
}

// failureNotice is the user's notice for a failed generation: what its
// capture failure means in a sentence where FFmpeg's stderr shows a known
// one, FFmpeg's line for a capture failure no sentence says more about
// (AcquireNextFrame's HRESULT: distinct errors are distinct notices), else
// FFmpeg's error without its log contexts ("[Parsed_ddagrab_0 @
// 000002bda68effc0] "), which took most of the notice's 160 characters.
func failureNotice(ev media.VideoEvent) string {
	cf := ev.CaptureFailure
	switch {
	case cf.Text != "":
		return "Screen capture failed, retrying: " + capitalize(cf.Text) + "."
	case cf.Fault != media.CaptureFaultNone && cf.Line != "":
		return "Screen capture failed, retrying: " + trunc(cf.Line, 160)
	}
	return "Video encoder restarted (" + trunc(strings.TrimSpace(media.PlainFFmpegError(ev.Err.Error())), 160) + ")"
}

// capitalize upper-cases s's first letter.
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
