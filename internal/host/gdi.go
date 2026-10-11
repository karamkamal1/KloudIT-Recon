package host

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
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
// and D3D11VA device creation fails the same way. GDI keeps such a stream
// going, and the finding is checked again (recheckGPUCapture,
// watchGPUCapture) so the PC goes back to GPU capture once its driver is
// there.

// gpuCaptureRecheck is how long a finding of no GPU capture holds before a
// session start checks again (in the background: the session that starts
// does not wait). A GPU driver installed, or one that was being updated
// when the finding was made, brings GPU capture back without an agent
// restart. Tests shorten it.
var gpuCaptureRecheck = time.Minute

// checkGPUCapture is media.CheckGPUCapture (tests replace it).
var checkGPUCapture = media.CheckGPUCapture

// gpuCaptureState is what the agent knows about FFmpeg's GPU capture on this
// PC beyond the probe's finding (media.Caps.GPUCaptureBlocked).
type gpuCaptureState struct {
	mu       sync.Mutex
	found    bool      // why is a finding of the agent's (else the probe's stands)
	why      string    // why GPU capture cannot work ("" = it can)
	at       time.Time // when the finding in force was made
	checking bool      // a check runs (recheckGPUCapture)
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

// gpuCaptureLostLive reports whether the finding of no GPU capture in force
// is a session's, made while the agent ran (the probe had found GPU capture
// working, or could not tell): a graphics driver reset or update, more likely
// than a PC without a GPU.
func (a *Agent) gpuCaptureLostLive() bool {
	a.gpuCap.mu.Lock()
	defer a.gpuCap.mu.Unlock()
	return a.gpuCap.found && a.gpuCap.why != ""
}

// recheckGPUCapture checks GPU capture again in the background when the
// agent's finding that it cannot work is older than gpuCaptureRecheck (a
// session starts). Only a capture that opens lifts the finding: one that
// fails for another reason (the secure desktop) or times out leaves it to
// the next check.
func (a *Agent) recheckGPUCapture() {
	if a.caps == nil || a.caps.FFmpeg == "" || a.gpuCaptureBlocked() == "" {
		return
	}
	a.gpuCap.mu.Lock()
	if a.gpuCap.checking || time.Since(a.gpuCap.at) < gpuCaptureRecheck {
		a.gpuCap.mu.Unlock()
		return
	}
	a.gpuCap.checking = true
	a.gpuCap.mu.Unlock()
	go func() {
		blocked, detail := checkGPUCapture(context.Background(), a.caps.FFmpeg)
		ok := blocked == "" && detail == ""
		a.gpuCap.mu.Lock()
		a.gpuCap.checking = false
		a.gpuCap.at = time.Now()
		if ok {
			a.gpuCap.found, a.gpuCap.why = true, ""
		}
		a.gpuCap.mu.Unlock()
		switch {
		case ok:
			a.log.Info("GPU screen capture works again: sessions capture with it")
		case blocked != "":
			a.log.Debug("GPU screen capture still unavailable", "reason", blocked)
		default:
			a.log.Debug("GPU screen capture check inconclusive, sessions keep capturing with GDI", "detail", detail)
		}
	}()
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

// sessionGDI returns why the session captures with GDI after its own GPU
// capture failed (captureFailed), or "".
func (s *Session) sessionGDI() string {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.gdi
}

// captureFailed moves the session's next generations to GDI capture
// (gdigrab) when its FFmpeg generation's GPU capture failed in a way the same
// capture does not get past, and reports whether it did:
//   - no Direct3D 11 video device (media.CaptureNoDevice): at once, and for
//     the sessions after this one too (the agent's finding, which a later
//     check can lift);
//   - Desktop Duplication refusing the display (media.CaptureUnsupported)
//     twice in a row without a live generation in between, for the rest of
//     the session: a mode change can cause one, a remote desktop session
//     causes every one.
//
// Anything else (the secure desktop, a lost or changed display, an encoder
// fault) keeps the capture, which the restart retries; it also ends a run
// of refusals. The switch is logged once with FFmpeg's line (the "encoder
// failed" line before it has FFmpeg's whole error); the user's notice comes
// with the next generation's parameters (noteGDI).
func (s *Session) captureFailed(ev media.VideoEvent) bool {
	if ev.Failed == nil {
		return false
	}
	src, cf := ev.Failed.Source.Backend, ev.CaptureFailure
	if src != "ddagrab" && src != "gfxcapture" {
		return false
	}
	scope := "this PC"
	switch {
	case cf.Fault == media.CaptureNoDevice:
		s.a.noGPUCapture(cf.Text)
	case cf.Fault == media.CaptureUnsupported && src == "ddagrab":
		s.prefsMu.Lock()
		s.ddaRefused++
		again := s.ddaRefused >= 2
		if again && s.a.caps.CanGDIGrab() {
			s.gdi = cf.Text
		}
		s.prefsMu.Unlock()
		if !again {
			return false
		}
		scope = "this session"
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
	s.log.Warn("GPU screen capture failed: capturing with GDI (gdigrab)", "for", scope, "capture", src, "reason", cf.Text, "ffmpeg", cf.Line)
	return true
}

// noteGDI logs, and tells the user, once per reason why the session's next
// generation captures with GDI (backend gdigrab) where it would capture on
// the GPU: no GPU capture on this PC, or Desktop Duplication refusing the
// session's display (captureFailed). Not when the host config asks for
// gdigrab. window: the client asked for a window, which GDI does not
// capture alone.
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
	if pc {
		s.notice("warn", "No GPU screen capture on this PC ("+why+"): capturing with GDI, which uses more CPU and adds latency.")
		if s.a.gpuCaptureLostLive() {
			s.watchGPUCapture()
		}
	} else {
		s.notice("warn", "Screen capture on the GPU failed ("+why+"): capturing with GDI for this session, which uses more CPU and adds latency.")
	}
}

// gpuCaptureWatchEvery is how often a session on GDI after a finding made
// at run time checks whether GPU capture works again (watchGPUCapture;
// tests shorten it).
var gpuCaptureWatchEvery = time.Minute

// watchGPUCapture runs while the session captures with GDI after a session
// found no Direct3D 11 video device at run time (gpuCaptureLostLive: a
// graphics driver reset or update, where the probe had found GPU capture).
// Every gpuCaptureWatchEvery it has the finding checked again
// (recheckGPUCapture, in the background), and once GPU capture works it
// restarts the video on it (overlapped, so the picture does not stop), so a
// GPU PC does not stay on GDI. A session that moved to GDI for its own
// display's sake (sessionGDI) stays. One watch per session at a time.
func (s *Session) watchGPUCapture() {
	if !s.gpuWatch.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.gpuWatch.Store(false)
		t := time.NewTicker(gpuCaptureWatchEvery)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
			if s.a.gpuCaptureBlocked() != "" {
				s.a.recheckGPUCapture()
				continue
			}
			s.prefsMu.Lock()
			s.gdiNoted = "" // a later loss is told again
			s.prefsMu.Unlock()
			if s.sessionGDI() != "" {
				return
			}
			s.log.Info("GPU screen capture works again: restarting the video on it")
			s.notice("info", "GPU screen capture works again: capturing on the GPU.")
			if err := s.startVideo(false, "GPU screen capture works again"); err != nil {
				s.log.Warn("restart on GPU screen capture failed", "err", err)
			}
			return
		}
	}()
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
// one, else FFmpeg's error without its log contexts ("[Parsed_ddagrab_0 @
// 000002bda68effc0] "), which took most of the notice's 160 characters.
func failureNotice(ev media.VideoEvent) string {
	if t := ev.CaptureFailure.Text; t != "" {
		return "Screen capture failed, retrying: " + capitalize(t) + "."
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
