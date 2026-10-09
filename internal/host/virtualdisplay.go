package host

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Virtual displays for sessions (GUIDE 3.7, internal/host/vdisplay): when the
// host config "virtualDisplay" asks for one ("on", or "auto" and the monitor
// cannot show the client's mode 1:1) and an IddCx driver is installed, a
// session streams a monitor created for its client: the client's size, the
// stream's frame rate as refresh rate (above the physical monitor's), captured
// 1:1 with Desktop Duplication (Windows Graphics Capture when the host config
// asks for gfxcapture). One per session; a reconnect within the linger (host
// config "virtualDisplayLinger") gets the same one back; the previous display
// topology is restored when the session ends, at agent shutdown, and after a
// crash at the next start (the package's journal).

// vdCheckEvery is how often a session checks that Windows still lists its
// virtual display (watchVirtualDisplay); missing twice in a row, it is gone.
// A display can go without its driver's keepalive failing (the Virtual
// Display Driver has none; Windows can remove or deactivate a SudoVDA
// monitor), and the native helper's DDA does not end a generation whose
// output vanished (captureChanged "lost": it retries and repeats the last
// image), so nothing else would move the session off it.
const vdCheckEvery = time.Second

// newVirtualDisplays makes the agent's virtual display manager (tests
// replace it with a vdisplay.Sim's).
var newVirtualDisplays = vdisplay.New

// virtualDisplayOptions are the vdisplay.Options of the agent: the host
// config's, the GPU that renders the virtual display (DXGI adapter 0, where
// ddagrab captures and the encoders run) and the log. The restore journal is
// in the agent's own folder (AgentFilesDir: an elevated agent's is one only
// administrators can change, else the config's), created when the policy may
// use it.
func (a *Agent) virtualDisplayOptions() vdisplay.Options {
	o := a.cfg.virtualDisplayOptions()
	o.Log = a.log
	if ad, err := platform.PrimaryAdapter(); err == nil {
		o.RenderAdapter = ad.LUID
	}
	dir, err := AgentFilesDir(a.cfg)
	if err == nil && dir != "" && o.Policy != vdisplay.PolicyOff && !runsElevated() {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		if o.Policy != vdisplay.PolicyOff {
			a.log.Warn("virtual display: no restore journal (the displays are not put back after a crash)", "dir", dir, "err", err)
		}
		dir = ""
	}
	o.StateDir = dir
	return o
}

// setupVirtualDisplays makes m the agent's virtual display manager. Before
// any session it puts back the displays a virtual display of an agent that
// stopped without removing it (a crash, a power loss) left changed, whatever
// the policy is now; then it logs the policy and the driver once.
func (a *Agent) setupVirtualDisplays(m *vdisplay.Manager) {
	a.vd = m
	if err := m.Recover(); err != nil {
		a.log.Warn("restoring the displays after an unfinished virtual display session failed", "err", err)
	}
	if m.Policy() == vdisplay.PolicyOff {
		return
	}
	o := a.cfg.virtualDisplayOptions()
	a.log.Info("virtual display", "policy", m.Policy(), "layout", o.Layout, "linger", o.Linger, "driver", m.Detect().String())
}

// RestoreVirtualDisplays puts back the displays a virtual display of an agent
// with cfg left changed when the agent was stopped without its cleanup (a
// crash, or a kill: Stop-ScheduledTask and Stop-Process end the windowless
// agent at once), as the agent's next start would: its restore journal in the
// agent's folder (AgentFilesDir; vdisplay.Manager.Recover). It reports whether
// there was one. For "recon-host vdisplay -restore", which uninstall-host.ps1
// runs elevated before it deletes the agent and the journal. Stop the agent
// first. An elevated run without its admin-only folder has nothing to restore
// (the agent kept no journal) or refuses one it cannot trust.
func RestoreVirtualDisplays(cfg *Config, log *slog.Logger) (bool, error) {
	o := cfg.virtualDisplayOptions()
	dir, err := AgentFilesDir(cfg)
	if err != nil {
		if sd, serr := elevatedStateDir(); serr == nil {
			if _, serr := os.Stat(sd); errors.Is(serr, os.ErrNotExist) {
				return false, nil
			}
		}
		return false, err
	}
	o.StateDir = dir
	if o.StateDir == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, vdisplay.JournalName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	o.Log = log
	return true, newVirtualDisplays(o).Recover()
}

// closeVirtualDisplays removes a virtual display (a session's, or one kept
// for a reconnect) and restores the displays: agent shutdown.
func (a *Agent) closeVirtualDisplays() {
	if a.vd != nil {
		a.vd.Close()
	}
}

// virtualDisplayBlocker returns why a session with prefs cannot stream a
// virtual display whatever the policy says, or "": only a monitor capture
// can, and not after the session lost one.
func (s *Session) virtualDisplayBlocker(prefs proto.Prefs) string {
	switch {
	case s.a.cfg.Capture == "test":
		return `capture "test" streams a test pattern`
	case s.a.cfg.Capture == "x11grab":
		return `capture "x11grab" streams an X11 display`
	case prefs.Window != "":
		return "the client captures a window"
	}
	return s.vdOff
}

// requestedMode is the virtual display for a client with prefs
// (vdisplay.RequestedMode): the size the client streams at, else its screen
// in device pixels, else fallback's; the stream's frame rate as refresh rate.
func (s *Session) requestedMode(prefs proto.Prefs, fallback platform.Monitor) vdisplay.Mode {
	return vdisplay.RequestedMode(s.hello.Client, prefs, s.a.cfg.DefaultFPS, s.a.cfg.MaxFPS).Complete(&fallback)
}

// openVirtualDisplay gives the session a virtual display when the policy asks
// for one. Called as the session starts, before its pipeline (the helper's
// caps then list the display's output) and its welcome (which lists the
// display). Returns a notice for the user when it failed.
func (s *Session) openVirtualDisplay(prefs proto.Prefs) string {
	s.vdMu.Lock()
	defer s.vdMu.Unlock()
	_, notice := s.decideVirtualDisplay(prefs)
	return notice
}

// decideVirtualDisplay creates a virtual display for prefs when the policy
// asks for one (vdisplay.Manager.Decide, with the monitor the session would
// capture otherwise) and reports whether it did; notice tells the user why
// it failed. The decision is logged once per change. Called with vdMu held
// while the session has none.
func (s *Session) decideVirtualDisplay(prefs proto.Prefs) (created bool, notice string) {
	vd := s.a.vd
	if vd == nil || vd.Policy() == vdisplay.PolicyOff {
		return false, ""
	}
	phys := s.a.monitorFor(prefs)
	req := s.requestedMode(prefs, phys)
	use, why := false, s.virtualDisplayBlocker(prefs)
	if why == "" {
		use, why = vd.Decide(req, &phys)
	}
	if !use {
		if why != s.vdWhy {
			s.vdWhy = why
			s.log.Info("virtual display not used", "reason", why, "mode", req, "monitor", phys.Name,
				"monitor_mode", fmt.Sprintf("%dx%d@%d", phys.W, phys.H, phys.Hz), "policy", vd.Policy())
		}
		return false, ""
	}
	s.vdWhy = ""
	d, err := vd.Create(req)
	if err != nil {
		s.log.Warn("virtual display unavailable, streaming the monitor", "reason", why, "mode", req, "monitor", phys.Name, "err", err)
		return false, "Virtual display unavailable: " + trunc(err.Error(), 160) + "; streaming the monitor."
	}
	info := d.Info()
	// FFmpeg captures a virtual display with ddagrab (an output of DXGI
	// adapter 0, the render adapter) or gfxcapture; the helper with DDA on
	// any adapter.
	if info.Monitor.DXGIOutput < 0 && !s.a.caps.Filters["gfxcapture"] && s.a.launchHelper == nil {
		_ = d.Remove()
		s.log.Warn("virtual display not used: FFmpeg cannot capture it", "name", info.Name, "mode", req,
			"reason", "not an output of DXGI adapter 0 (ddagrab's output_idx) and this FFmpeg has no gfxcapture")
		return false, "Virtual display unavailable: FFmpeg cannot capture it (not on the render GPU); streaming the monitor."
	}
	s.vd, s.vdMode = d, req
	s.log.Info("streaming a virtual display", "reason", why, "mode", req, "name", info.Name, "driver", info.Driver,
		"refresh", fmt.Sprintf("%.3f", info.RefreshHz), "layout", info.Layout,
		"rect", fmt.Sprintf("%dx%d at (%d,%d)", info.Monitor.W, info.Monitor.H, info.Monitor.X, info.Monitor.Y),
		"dxgi_output", info.Monitor.DXGIOutput, "hmonitor", fmt.Sprintf("%#x", info.Monitor.HMonitor), "capture_config", s.a.cfg.Capture)
	go s.watchVirtualDisplay(d)
	return true, ""
}

// updateVirtualDisplay follows a change of the client's size, frame rate,
// monitor or window setting (prefs): a session on a virtual display gets one
// at the new mode when the mode changed (vdisplay.Manager.Create replaces
// it), or none when it captures a window now (removed at once: the window
// stays on the desktop; a later monitor capture decides again); a session
// without one decides again. It reports whether the capture changed: the
// caller then starts the next generation at once (the display the current
// one captures is gone, or the desktop was rearranged).
func (s *Session) updateVirtualDisplay(prefs proto.Prefs) bool {
	vd := s.a.vd
	if vd == nil || vd.Policy() == vdisplay.PolicyOff || s.ctx.Err() != nil || !s.a.isActive(s) {
		return false // a session being replaced never rearranges its successor's displays
	}
	s.vdMu.Lock()
	defer s.vdMu.Unlock()
	if s.vd == nil {
		created, notice := s.decideVirtualDisplay(prefs)
		if notice != "" {
			s.notice("warn", notice)
		}
		return created
	}
	old := s.vd
	if why := s.virtualDisplayBlocker(prefs); why != "" {
		s.vid().Suspend() // its capture goes with the display
		s.vd, s.vdWhy = nil, why
		err := old.Remove()
		s.log.Info("leaving the virtual display", "reason", why, "name", old.Info().Name, "restore_err", err)
		return true
	}
	req := s.requestedMode(prefs, old.Info().Monitor)
	if req == s.vdMode {
		return false
	}
	s.vid().Suspend() // its capture goes with the display
	d, err := vd.Create(req)
	if err != nil {
		// Create removed the old display first (or it is unusable now).
		_ = old.Remove()
		s.vd = nil
		s.log.Warn("virtual display unavailable, streaming the monitor", "mode", req, "was", s.vdMode, "err", err)
		s.notice("warn", "Virtual display unavailable: "+trunc(err.Error(), 160)+"; streaming the monitor.")
		return true
	}
	info := d.Info()
	s.log.Info("virtual display changed", "mode", req, "was", s.vdMode, "name", info.Name,
		"refresh", fmt.Sprintf("%.3f", info.RefreshHz), "dxgi_output", info.Monitor.DXGIOutput, "hmonitor", fmt.Sprintf("%#x", info.Monitor.HMonitor))
	s.vd, s.vdMode = d, req
	go s.watchVirtualDisplay(d)
	return true
}

// watchVirtualDisplay moves the session off display d when its driver stops
// answering (SudoVDA's watchdog then removes the monitor) or Windows no
// longer lists it (vdCheckEvery), while d is the session's.
func (s *Session) watchVirtualDisplay(d *vdisplay.Display) {
	t := time.NewTicker(vdCheckEvery)
	defer t.Stop()
	missing := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-d.Lost():
			s.leaveVirtualDisplay(d, "its driver stopped answering")
			return
		case <-t.C:
		}
		s.vdMu.Lock()
		mine := s.vd == d
		s.vdMu.Unlock()
		if !mine || !s.a.isActive(s) {
			return // replaced, released, or the session is being replaced
		}
		if _, ok := d.Info().Find(s.a.monitors()); ok {
			missing = 0
		} else if missing++; missing >= 2 {
			s.leaveVirtualDisplay(d, "Windows no longer lists it")
			return
		}
	}
}

// leaveVirtualDisplay moves the session off display d, which is gone (why):
// the video is suspended, d removed and the topology restored, and the
// stream restarted at once on the monitor.
func (s *Session) leaveVirtualDisplay(d *vdisplay.Display, why string) {
	if !s.takeVirtualDisplay(d, why) {
		return
	}
	s.vid().Suspend() // its capture goes with the display
	s.removeVirtualDisplay(d, why)
	if s.ctx.Err() == nil && !s.paused.Load() {
		if err := s.startVideo(true, "virtual display lost"); err != nil {
			s.notice("error", "Could not restart video: "+err.Error())
		}
	}
}

// takeVirtualDisplay ends the session's use of display d, which it cannot
// capture (why), and reports whether d was still the session's: the session
// then captures the monitor prefs name and creates no other virtual display.
func (s *Session) takeVirtualDisplay(d *vdisplay.Display, why string) bool {
	s.vdMu.Lock()
	defer s.vdMu.Unlock()
	if s.vd != d {
		return false
	}
	s.vd, s.vdOff = nil, "the session's virtual display was lost ("+why+")"
	return true
}

// removeVirtualDisplay removes display d (takeVirtualDisplay) at once,
// restoring the topology, and tells the user.
func (s *Session) removeVirtualDisplay(d *vdisplay.Display, why string) {
	err := d.Remove()
	s.log.Warn("virtual display lost, streaming the monitor", "reason", why, "name", d.Info().Name, "restore_err", err)
	s.notice("warn", "The virtual display is gone ("+why+"); streaming the monitor.")
}

// closeVirtualDisplay releases the session's virtual display as the session
// ends: the topology is restored after the linger (host config
// "virtualDisplayLinger"), unless a reconnecting client takes the display
// over by then.
func (s *Session) closeVirtualDisplay() {
	s.vdMu.Lock()
	d := s.vd
	s.vd = nil
	s.vdMu.Unlock()
	if d == nil {
		return
	}
	if err := d.Close(); err != nil {
		s.log.Warn("virtual display removed, display restore incomplete", "err", err)
	}
}

// captureMonitor returns the monitor the session captures for prefs: its
// virtual display d, as Windows lists it now, else the monitor prefs name (d
// nil). A virtual display Windows no longer lists is gone: the session leaves
// it, removing it (and restoring the topology, which can move the monitor)
// before the monitor is looked up.
func (s *Session) captureMonitor(prefs proto.Prefs) (platform.Monitor, *vdisplay.Display) {
	s.vdMu.Lock()
	d := s.vd
	s.vdMu.Unlock()
	if d == nil {
		return s.a.monitorFor(prefs), nil
	}
	if m, ok := d.Info().Find(s.a.monitors()); ok {
		return m, d
	}
	const why = "Windows no longer lists it"
	if s.takeVirtualDisplay(d, why) {
		s.removeVirtualDisplay(d, why)
	}
	return s.a.monitorFor(prefs), nil
}

// onVirtualDisplay reports whether the session streams a virtual display.
func (s *Session) onVirtualDisplay() bool {
	s.vdMu.Lock()
	defer s.vdMu.Unlock()
	return s.vd != nil
}

// capture returns the capture method (host config "capture") the session's
// video follows: the configured one, except that a virtual display is
// captured with Desktop Duplication (FFmpeg's ddagrab, the helper's dda)
// unless the host config asks for gfxcapture: AMD Direct Capture reads the
// GPU's own display outputs, which never scan out an IddCx monitor, and the
// display has the stream's size already (nothing to scale).
func (s *Session) capture() string {
	if s.a.cfg.Capture != "gfxcapture" && s.onVirtualDisplay() {
		return "ddagrab"
	}
	return s.a.cfg.Capture
}

// captureBackend returns FFmpeg's capture source for monitor mon
// (backendFor). A virtual display (virt) is captured whole, 1:1: with
// ddagrab, or with gfxcapture of its HMONITOR when the host config asks for
// that, when it is not an output of DXGI adapter 0 (ddagrab's output_idx
// counts those only) or this FFmpeg has no ddagrab. Capture "amf" stays a
// request, which useAMFCapture turns down for a virtual display. A window
// is captured as without one (updateVirtualDisplay then removes it).
func (s *Session) captureBackend(prefs proto.Prefs, mon platform.Monitor, virt bool) string {
	if !virt || prefs.Window != "" {
		return s.a.backendFor(prefs)
	}
	f := s.a.caps.Filters
	switch {
	case s.a.cfg.Capture == "gfxcapture", (mon.DXGIOutput < 0 || !f["ddagrab"]) && f["gfxcapture"]:
		return "gfxcapture"
	case s.a.cfg.Capture == "amf":
		return "amf"
	}
	return "ddagrab"
}

// welcomeMonitors lists the monitors for the welcome: the system's, or the
// session's virtual display alone (the session captures nothing else, and a
// client offers no display choice for one monitor).
func (s *Session) welcomeMonitors() []proto.MonitorInfo {
	if m, d := s.captureMonitor(s.currentPrefs()); d != nil {
		return []proto.MonitorInfo{{Index: 0, Name: m.Name, Width: m.W, Height: m.H, X: m.X, Y: m.Y, Primary: m.Primary, Hz: m.Hz, Virtual: true}}
	}
	var out []proto.MonitorInfo
	for _, m := range s.a.monitors() {
		out = append(out, proto.MonitorInfo{Index: m.Index, Name: m.Name, Width: m.W, Height: m.H, X: m.X, Y: m.Y, Primary: m.Primary, Hz: m.Hz})
	}
	return out
}
