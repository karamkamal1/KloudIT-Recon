package host

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// The session's desktop. The agent runs in the user's Windows session and
// can capture and control only that session's desktop while it is on a
// screen: not while the session is locked or shows a secure desktop (the
// lock screen, a UAC prompt: the Winlogon desktop, which no program but
// SYSTEM's may open), and not while it is disconnected (a remote desktop
// client closed it, as happens on a VM someone reached over RDP: nothing
// draws its desktop). Every FFmpeg generation fails then (ddagrab "Desktop
// duplication access denied", gdigrab "Failed to capture image"), and the
// session used to end after its seventh failure in a row, about 10 s, with
// "Video encoder keeps failing". Now such a failure waits for the desktop:
// it does not count, the user is told why in an informational notice, and
// the video restarts as soon as the desktop is back. A disconnected session
// is moved to the console first (host config "consoleSession": as "tscon
// <session> /dest:console" does, when nobody else is signed in there), which
// gives it the console's display, unlocked: by default ("auto") only on a
// virtual machine or a PC without a GPU, whose console only the
// hypervisor's users see, not on a PC whose own monitor would show the
// unlocked desktop ("on" moves it there too); only while the stream plays
// (not while the client is hidden), and only after consoleLooks looks in a
// row found it disconnected (not a moment of an RDP client taking the
// session over).

// desktopState is platform.DesktopState, connectConsole
// platform.ConnectToConsole and osInfo platform.OSInfo (tests replace them).
var (
	desktopState   = platform.DesktopState
	connectConsole = platform.ConnectToConsole
	osInfo         = platform.OSInfo
)

// consoleOn: host config "consoleSession" "on" (also on a PC with a GPU).
const consoleOn = "on"

// consoleLooks is how many looks in a row (desktopEvery apart) must find
// the session disconnected before it is moved to the console.
var consoleLooks = 2

// watchDesktops: sessions look at their desktop (Windows; tests set it).
var watchDesktops = runtime.GOOS == "windows"

// desktopEvery is how often a session looks at its desktop (watchDesktop;
// tests shorten it).
var desktopEvery = time.Second

// desktopRetry is how often a session whose video waits for its desktop
// restarts it anyway, in case the desktop is there after all (a state
// Windows misreports): a generation that fails again waits again.
var desktopRetry = 30 * time.Second

// consoleRetry is how long after a move to the console the next one may be
// tried (one that failed for a reason of the moment: the console's session
// changing).
var consoleRetry = 30 * time.Second

// consoleSettle bounds how long a session start waits for its desktop after
// moving it to the console.
var consoleSettle = 3 * time.Second

// deskState is what a session knows about its desktop (desktop.go).
type deskState struct {
	mu         sync.Mutex
	away       string    // why the desktop was away at the last look ("": there)
	waiting    bool      // a generation failed while it was away: the video restarts when it is back
	waitedAt   time.Time // when the video last started waiting, or was retried meanwhile (desktopRetry)
	noted      string    // the reason the user was last told, "" once the desktop is back
	consoleAt  time.Time // when a move to the console was last tried
	consoleErr string    // why it failed ("": it worked, or none was tried)
	watching   bool      // watchDesktop runs
	discLooks  int       // looks in a row that found the session disconnected
}

// consoleMove reports whether a session left disconnected by a remote
// desktop client is moved to the console, and why (or why not): host
// config "consoleSession" "on"; "auto" ("" = auto) on a virtual machine (the
// firmware names a hypervisor) or a PC without a GPU, whose console only
// the hypervisor's users see; never with "off". A PC with a GPU is not a VM
// as far as "auto" knows (a VM with a passthrough GPU whose firmware names
// no hypervisor: set "on").
func (a *Agent) consoleMove() (bool, string) {
	switch a.cfg.ConsoleSession {
	case settingOff:
		return false, `host config "consoleSession" is "off"`
	case consoleOn:
		return true, `host config "consoleSession" is "on"`
	}
	if _, machine := osInfo(); virtualMachine(machine) {
		return true, "a virtual machine (" + machine + ")"
	}
	if why := a.noGPU(); why != "" {
		return true, "a PC without a GPU"
	}
	return false, `on a PC with a GPU its own monitor would show the unlocked desktop; host config "consoleSession" "on" allows it`
}

// virtualMachine reports whether the firmware's machine name (OSInfo) is a
// hypervisor's: QEMU/KVM (Proxmox: "QEMU Standard PC (Q35 + ICH9, 2009)"),
// Hyper-V ("Microsoft Corporation Virtual Machine"), VMware, VirtualBox,
// Xen, Parallels, bhyve.
func virtualMachine(machine string) bool {
	m := strings.ToLower(machine)
	for _, h := range []string{"qemu", "kvm", "virtual machine", "vmware", "virtualbox", "innotek", "xen", "parallels", "bochs",
		"bhyve", "proxmox", "standard pc ("} {
		if strings.Contains(m, h) {
			return true
		}
	}
	return false
}

// desktopNow reads the session's desktop and returns why it is away ("" when
// it is there, or for captures of no Windows desktop: the test pattern,
// x11grab).
func (s *Session) desktopNow() (string, platform.Desktop) {
	if cfg := s.a.cfg; cfg != nil && (cfg.Capture == "test" || cfg.Capture == "x11grab") {
		return "", platform.Desktop{}
	}
	d := desktopState()
	return d.Away(), d
}

// desktopAttrs are a desktop's state for the log.
func desktopAttrs(d platform.Desktop) []any {
	return []any{"session", d.Session, "console_session", int64(int32(d.Console)), "remote", d.Remote, "disconnected", d.Disconnected,
		"locked", d.Locked, "input_desktop", d.Input, "secure", d.Secure}
}

// prepareDesktop runs before the session's first generation: a session
// that a remote desktop client left disconnected (on consoleLooks looks,
// desktopEvery apart) is moved to the console (toConsole), and the start
// waits up to consoleSettle for its desktop, so the first generation has
// one to capture.
func (s *Session) prepareDesktop() {
	why, d := s.desktopNow()
	if why == "" {
		return
	}
	s.log.Info("no desktop on a screen at the session's start", append([]any{"reason", why}, desktopAttrs(d)...)...)
	if move, _ := s.a.consoleMove(); why != platform.DesktopDisconnected || !move {
		return
	}
	for i := 1; i < consoleLooks && why == platform.DesktopDisconnected; i++ {
		time.Sleep(desktopEvery)
		why, d = s.desktopNow()
	}
	if why != platform.DesktopDisconnected || s.paused.Load() || !s.toConsole(d) {
		return
	}
	for end := time.Now().Add(consoleSettle); time.Now().Before(end) && s.ctx.Err() == nil; {
		if why, _ := s.desktopNow(); why == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// toConsole moves the session to the console (connectConsole) where
// consoleMove allows it and no move was tried within consoleRetry, and
// reports whether it did.
func (s *Session) toConsole(d platform.Desktop) bool {
	if move, _ := s.a.consoleMove(); !move {
		return false
	}
	s.desk.mu.Lock()
	if !s.desk.consoleAt.IsZero() && time.Since(s.desk.consoleAt) < consoleRetry {
		s.desk.mu.Unlock()
		return false
	}
	s.desk.consoleAt = time.Now()
	s.desk.mu.Unlock()
	err := connectConsole()
	s.desk.mu.Lock()
	s.desk.consoleErr = ""
	if err != nil {
		s.desk.consoleErr = err.Error()
	}
	s.desk.mu.Unlock()
	if err != nil {
		s.log.Warn("cannot move the disconnected session to the console", append([]any{"err", err}, desktopAttrs(d)...)...)
		return false
	}
	_, why := s.a.consoleMove()
	s.log.Warn("moved the disconnected session to the console (as tscon <session> /dest:console): the console shows it unlocked",
		append([]any{"because", why}, desktopAttrs(d)...)...)
	s.notice("info", fmt.Sprintf("This PC's Windows session was disconnected (a remote desktop client closed): it was moved to the PC's console "+
		"(session %d) to stream it, where it shows unlocked (on a VM: in the hypervisor's console).", d.Session))
	return true
}

// consolePending reports whether a disconnected session is to be moved to
// the console (consoleMove allows it and no move failed): its notice waits
// for the move, which watchDesktop makes while the stream plays once
// consoleLooks looks in a row found it disconnected, or for its failure.
func (s *Session) consolePending() bool {
	move, _ := s.a.consoleMove()
	s.desk.mu.Lock()
	defer s.desk.mu.Unlock()
	return move && s.desk.consoleErr == ""
}

// watchDesktop looks at the session's desktop every desktopEvery until the
// session ends (one per session, Windows): it logs when the desktop goes
// away and comes back, tells the user why it is away (desktopNotice), moves
// a disconnected session to the console (toConsole: while the stream plays,
// after consoleLooks looks in a row), and restarts the video once the
// desktop is back where a generation failed meanwhile (awaitDesktop).
func (s *Session) watchDesktop() {
	s.desk.mu.Lock()
	if s.desk.watching {
		s.desk.mu.Unlock()
		return
	}
	s.desk.watching = true
	s.desk.mu.Unlock()
	s.watches.Add(1)
	go func() {
		defer s.watches.Done()
		t := time.NewTicker(desktopEvery)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
			s.lookAtDesktop()
		}
	}()
}

// lookAtDesktop is one look of watchDesktop.
func (s *Session) lookAtDesktop() {
	why, d := s.desktopNow()
	s.desk.mu.Lock()
	prev := s.desk.away
	s.desk.away = why
	waiting := s.desk.waiting
	retry := waiting && why != "" && time.Since(s.desk.waitedAt) >= desktopRetry
	if retry {
		s.desk.waitedAt = time.Now()
	}
	if why == "" {
		s.desk.waiting, s.desk.noted = false, ""
	}
	if why == platform.DesktopDisconnected {
		s.desk.discLooks++
	} else {
		s.desk.discLooks = 0
	}
	looks := s.desk.discLooks
	s.desk.mu.Unlock()
	switch {
	case why != "" && why != prev:
		s.log.Info("the session's desktop is away: capture waits for it", append([]any{"reason", why}, desktopAttrs(d)...)...)
	case why == "" && prev != "":
		s.log.Info("the session's desktop is back", "was", prev, "waiting", waiting)
	}
	if why != "" {
		if why == platform.DesktopDisconnected && !s.paused.Load() && looks >= consoleLooks && s.toConsole(d) {
			return // the next look sees whether it came back
		}
		if why != platform.DesktopDisconnected || !s.consolePending() {
			s.desktopNotice(why, d)
		}
		if !retry || s.ctx.Err() != nil || s.paused.Load() {
			return
		}
	}
	if waiting && s.ctx.Err() == nil && !s.paused.Load() {
		reason := "the desktop is back"
		if why != "" {
			reason = "retrying while the desktop is away (" + why + ")"
		}
		if err := s.startVideo(true, reason); err != nil {
			s.notice("error", "Could not restart video: "+err.Error())
		}
	}
}

// desktopAway reports whether the session's desktop was away at the last
// look (watchDesktop).
func (s *Session) desktopAway() bool {
	s.desk.mu.Lock()
	defer s.desk.mu.Unlock()
	return s.desk.away != ""
}

// awaitDesktop takes a failed generation whose session's desktop is away
// (desktopNow) and reports whether it did: the failure does not count
// against the session (nor its encoder, nor its capture), the user is told
// why (desktopNotice), and watchDesktop restarts the video once the desktop
// is back. Otherwise the failure is handled as before.
func (s *Session) awaitDesktop(ev media.VideoEvent) bool {
	why, d := s.desktopNow()
	if why == "" {
		return false
	}
	s.failures = 0
	s.desk.mu.Lock()
	if !s.desk.waiting {
		s.desk.waitedAt = time.Now()
	}
	s.desk.waiting = true
	s.desk.away = why
	s.desk.mu.Unlock()
	s.log.Info("video waits for the session's desktop", append([]any{"reason", why, "failed", ev.Err}, desktopAttrs(d)...)...)
	s.watchDesktop() // where the session start did not (tests)
	if why == platform.DesktopDisconnected && s.consolePending() {
		return true // watchDesktop moves it to the console (or says why it could not)
	}
	s.desktopNotice(why, d)
	return true
}

// desktopNotice tells the user, once per spell, why the session's desktop
// is away and what brings it back: an informational notice, as the stream
// comes back by itself.
func (s *Session) desktopNotice(why string, d platform.Desktop) {
	s.desk.mu.Lock()
	repeat := why == s.desk.noted
	s.desk.noted = why
	consoleErr := s.desk.consoleErr
	s.desk.mu.Unlock()
	if repeat {
		return
	}
	var msg string
	switch why {
	case platform.DesktopLocked:
		msg = "The PC is locked: Windows lets no program show or control its sign-in screen. Sign in at the PC itself " +
			"(on a VM: its console in the hypervisor); the stream comes back by itself."
	case platform.DesktopSecure:
		msg = "The PC shows a secure screen (a UAC prompt, or the sign-in or Ctrl+Alt+Del screen) that Windows lets no program " +
			"capture or control: answer it at the PC itself; the stream comes back by itself."
	case platform.DesktopDisconnected:
		msg = fmt.Sprintf("This PC's Windows session has no screen: a remote desktop client disconnected from it. Sign in at the PC "+
			"itself, or run \"tscon %d /dest:console\" as administrator in that session; the stream comes back by itself.", d.Session)
		switch move, why := s.a.consoleMove(); {
		case !move:
			msg += " (Not moved to the console by itself: " + why + ".)"
		case consoleErr != "":
			msg += " (Moving it to the console failed: " + trunc(consoleErr, 80) + ".)"
		}
	default:
		return
	}
	s.notice("info", msg)
}
