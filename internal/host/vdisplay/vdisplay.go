// Package vdisplay gives a streaming session a virtual monitor that matches the
// client: an IddCx (indirect display) driver creates a monitor at the
// client's resolution and the stream's frame rate (e.g. 2560x1440@120, above
// the host monitor's refresh rate), the package optionally makes it the
// primary display (or the only one), never rotates it, and restores the
// previous display topology when the session ends (GUIDE 3.7). The caller
// captures it with Desktop Duplication (FFmpeg ddagrab / the helper's "dda"),
// never with AMD Direct Capture, which reads the GPU's own display outputs.
//
// Supported drivers (docs/VENDOR_NOTES.md 3.7 has the research and sources):
//
//   - SudoVDA (SudoMaker, used by Apollo): IOCTLs on its device interface add
//     and remove monitors at any mode; a watchdog in the driver removes them
//     when the agent stops pinging (crash safety).
//   - Virtual Display Driver (VirtualDrivers/MikeTheTech, "VDD"): a fixed
//     number of monitors whose modes come from vdd_settings.xml; the agent
//     adds the client's mode to the file when missing and restarts the device
//     (PnP), or enables it for the session when it is disabled.
//
// Topology changes use the CCD API (QueryDisplayConfig / SetDisplayConfig);
// ccd.go holds that logic in platform-independent form.
package vdisplay

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Policy is the host config "virtualDisplay".
const (
	PolicyOff  = "off"  // never (default)
	PolicyAuto = "auto" // when a driver is installed and the monitor cannot show the client's mode 1:1
	PolicyOn   = "on"   // whenever a driver is installed
)

// Layout is the host config "virtualDisplayLayout": where the virtual display
// goes in the desktop.
const (
	LayoutPrimary = "primary" // primary display, the others stay on (default)
	LayoutExtend  = "extend"  // secondary display right of the others
	LayoutOnly    = "only"    // the only display: the others are off for the session
)

// Capture backends for a virtual display: Desktop Duplication. AMD Direct
// Capture reads the GPU's display engine, which never scans out an IddCx
// monitor.
const (
	FFmpegCapture = "ddagrab"
	HelperCapture = "dda"
)

// Driver names (Info.Driver, Status.Driver).
const (
	DriverSudoVDA = "sudovda"
	DriverVDD     = "vdd"
)

// Limits of the virtual monitor's mode.
const (
	MinWidth, MaxWidth   = 640, 7680
	MinHeight, MaxHeight = 360, 4320
	MinHz, MaxHz         = 24, 500
)

var (
	// ErrUnsupported: no virtual displays on this platform.
	ErrUnsupported = errors.New("virtual displays need Windows")
	// ErrNoDriver: no supported IddCx driver is installed.
	ErrNoDriver = errors.New("no virtual display driver installed (SudoVDA or Virtual Display Driver)")
)

// ValidPolicy reports whether s is a virtualDisplay value ("" = off).
func ValidPolicy(s string) bool {
	return s == "" || s == PolicyOff || s == PolicyAuto || s == PolicyOn
}

// ValidLayout reports whether s is a virtualDisplayLayout value ("" = primary).
func ValidLayout(s string) bool {
	return s == "" || s == LayoutPrimary || s == LayoutExtend || s == LayoutOnly
}

// Mode is a virtual monitor's resolution and refresh rate.
type Mode struct {
	Width, Height int
	Hz            int
}

func (m Mode) String() string { return fmt.Sprintf("%dx%d@%d", m.Width, m.Height, m.Hz) }

func (m Mode) hz() float64 { return float64(m.Hz) }

func (m Mode) rational() Rational { return Rational{uint32(m.Hz) * 1000, 1000} }

// Valid checks m against the limits.
func (m Mode) Valid() error {
	if m.Width < MinWidth || m.Width > MaxWidth || m.Height < MinHeight || m.Height > MaxHeight || m.Hz < MinHz || m.Hz > MaxHz {
		return fmt.Errorf("mode %v outside %dx%d-%dx%d at %d-%d Hz", m, MinWidth, MinHeight, MaxWidth, MaxHeight, MinHz, MaxHz)
	}
	return nil
}

// ParseMode parses "WxH@Hz" (e.g. 2560x1440@120).
func ParseMode(s string) (Mode, error) {
	var m Mode
	wh, hz, ok := strings.Cut(s, "@")
	w, h, ok2 := strings.Cut(wh, "x")
	var err1, err2, err3 error
	m.Width, err1 = strconv.Atoi(w)
	m.Height, err2 = strconv.Atoi(h)
	m.Hz, err3 = strconv.Atoi(hz)
	if !ok || !ok2 || err1 != nil || err2 != nil || err3 != nil {
		return Mode{}, fmt.Errorf("mode %q: want WIDTHxHEIGHT@HZ, e.g. 2560x1440@120", s)
	}
	return m, m.Valid()
}

// RequestedMode is the virtual monitor for a client: the size the client asks
// to stream at (prefs width/height) or else its screen in device pixels
// (hello client w/h), rounded down to even numbers, and the stream's frame
// rate (prefs fps or the host default, at most maxFPS) as the refresh rate.
// The client's measured display rate (hello client hz) is not used: the
// monitor refreshes as fast as the stream runs. A size the client did not
// report is 0x0 (Complete fills it in).
func RequestedMode(client proto.ClientInfo, prefs proto.Prefs, defaultFPS, maxFPS int) Mode {
	m := Mode{Width: client.Width, Height: client.Height, Hz: prefs.FPS}
	if prefs.Width > 0 && prefs.Height > 0 {
		m.Width, m.Height = prefs.Width, prefs.Height
	}
	if m.Width <= 0 || m.Height <= 0 {
		m.Width, m.Height = 0, 0
	} else {
		m.Width = clamp(m.Width, MinWidth, MaxWidth) &^ 1
		m.Height = clamp(m.Height, MinHeight, MaxHeight) &^ 1
	}
	if m.Hz <= 0 {
		m.Hz = defaultFPS
	}
	if maxFPS > 0 && m.Hz > maxFPS {
		m.Hz = maxFPS
	}
	m.Hz = clamp(m.Hz, MinHz, MaxHz)
	return m
}

// Complete fills an unknown size (0x0) from fallback (the physical monitor,
// else 1920x1080).
func (m Mode) Complete(fallback *platform.Monitor) Mode {
	if m.Width > 0 && m.Height > 0 {
		return m
	}
	m.Width, m.Height = 1920, 1080
	if fallback != nil && fallback.W > 0 && fallback.H > 0 {
		m.Width = clamp(fallback.W, MinWidth, MaxWidth) &^ 1
		m.Height = clamp(fallback.H, MinHeight, MaxHeight) &^ 1
	}
	return m
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// Decide says whether a session should get a virtual display under policy:
// driverOK from Manager.Detect, req from RequestedMode, phys the physical
// monitor the session would capture otherwise (nil: none). The reason is for
// the log.
func Decide(policy string, driverOK bool, req Mode, phys *platform.Monitor) (bool, string) {
	switch policy {
	case PolicyOn:
		return true, "virtualDisplay is on"
	case PolicyAuto:
	default:
		return false, ""
	}
	switch {
	case !driverOK:
		return false, "no virtual display driver"
	case phys == nil || phys.W <= 0 || phys.H <= 0:
		return true, "no physical monitor to capture"
	case req.Width > 0 && req.Height > 0 && (req.Width != phys.W || req.Height != phys.H):
		return true, fmt.Sprintf("client wants %dx%d, monitor is %dx%d", req.Width, req.Height, phys.W, phys.H)
	case phys.Hz > 0 && req.Hz > phys.Hz:
		return true, fmt.Sprintf("client wants %d fps, monitor refreshes at %d Hz", req.Hz, phys.Hz)
	}
	return false, "the monitor matches the client"
}

// Info describes a virtual display the session can capture.
type Info struct {
	Driver    string  // DriverSudoVDA or DriverVDD
	Name      string  // GDI device name, e.g. \\.\DISPLAY9 (platform.Monitor.Name)
	Target    Target  // CCD target (IddCx adapter LUID and target id)
	Mode      Mode    // as applied (Hz rounded from RefreshHz)
	RefreshHz float64 // the refresh rate Windows applied
	Layout    string
	// Monitor is the display as platform.Monitors lists it right after the
	// change: HMonitor (the helper's "hmonitor"), the DXGI output index on
	// adapter 0 (ddagrab's output_idx; -1 when the driver renders on another
	// GPU), rectangle, Hz. Its Index is that list's order, which differs from
	// the monitor list sent in the session's welcome.
	Monitor platform.Monitor
}

// Find returns the entry of mons (platform.Monitors) that is this display.
func (i Info) Find(mons []platform.Monitor) (platform.Monitor, bool) {
	for _, m := range mons {
		if strings.EqualFold(m.Name, i.Name) {
			return m, true
		}
	}
	return platform.Monitor{}, false
}

// Status is the result of Detect.
type Status struct {
	Driver string // DriverSudoVDA, DriverVDD or "" (none usable)
	Detail string // version, watchdog, device state
	Err    error  // why no driver is usable (ErrNoDriver, ErrUnsupported, or a driver's error)
}

func (s Status) String() string {
	if s.Driver == "" {
		return "none (" + s.Err.Error() + ")"
	}
	return s.Driver + " " + s.Detail
}

// Options configure a Manager.
type Options struct {
	Policy string // virtualDisplay
	Layout string // virtualDisplayLayout
	// StateDir holds the restore journal (vdisplay-restore.json) while a
	// virtual display exists, so Recover can put the topology back after a
	// crash; "" = no journal.
	StateDir string
	// MonitorID makes the virtual monitor's identity (SudoVDA monitor GUID and
	// EDID serial) stable per host, so Windows remembers its settings (DPI
	// scale, the layout saved for SudoVDA sessions) across sessions. Use the
	// host id.
	MonitorID string
	// RenderAdapter is the LUID (platform.Adapter.LUID) of the GPU that
	// renders the virtual display: SudoVDA's IOCTL_SET_RENDER_ADAPTER before
	// the monitor is added. Use DXGI adapter 0, on which ddagrab captures and
	// the encoders run. 0 = the driver's choice.
	RenderAdapter uint64
	// Linger keeps a released virtual display for this long, so a client
	// that reconnects with the same mode gets it back without the desktop
	// being rearranged twice. 0 = restore at once.
	Linger time.Duration
	Log    *slog.Logger
}

// system is the OS display configuration (Windows: CCD and GDI); tests fake it.
type system interface {
	// Query is QueryDisplayConfig(flags | QDC_VIRTUAL_MODE_AWARE).
	Query(flags uint32) (Config, error)
	// Apply is SetDisplayConfig(c.Paths, c.Modes, flags); no paths with
	// SDC_USE_DATABASE_CURRENT.
	Apply(c Config, flags uint32) error
	// SourceName is a source's GDI device name (\\.\DISPLAYn).
	SourceName(adapter LUID, id uint32) (string, error)
	// AdapterInstance is the PnP device instance ID of an adapter, from its
	// CCD device path.
	AdapterInstance(adapter LUID) (string, error)
	// Monitor finds a display by GDI name in platform.Monitors.
	Monitor(name string) (platform.Monitor, bool)
}

// driver is one IddCx virtual display driver.
type driver interface {
	Name() string
	// Detect reports whether the driver is installed and usable; the string
	// describes it (version, watchdog, device state) for logs.
	Detect() (string, error)
	// Plug makes a monitor available that can show m.
	Plug(m Mode, id monitorID, renderAdapter LUID) (plug, error)
	// Unplug undoes Plug.
	Unplug(p plug) error
	// Keepalive is called every KeepaliveEvery while a monitor is plugged.
	Keepalive() error
	KeepaliveEvery() time.Duration // 0 = no keepalive
	// Persistent: the monitor survives the agent (and reboots), so its layout
	// is never saved to the display database.
	Persistent() bool
	// Recover undoes a plug a journal recorded (the agent stopped before
	// restoring).
	Recover(p plug, id monitorID) error
	// Close releases driver handles.
	Close()
}

// plug says where a plugged monitor appears.
type plug struct {
	Target   Target `json:"target"`             // exact CCD target, when the driver reports it (SudoVDA)
	Instance string `json:"instance,omitempty"` // else: its IddCx adapter's device instance ID (VDD)
	Departs  bool   `json:"departs"`            // Unplug removes the monitor (VDD: the device was enabled for the session)
}

// monitorID is the stable identity of the agent's virtual monitor.
type monitorID struct {
	GUID   [16]byte // SudoVDA MonitorGuid (Windows GUID byte order)
	Serial string   // EDID serial string, at most 13 characters
}

func newMonitorID(seed string) monitorID {
	sum := sha256.Sum256([]byte("kloudit-recon virtual display\x00" + seed))
	var id monitorID
	copy(id.GUID[:], sum[:16])
	id.GUID[7] = id.GUID[7]&0x0f | 0x80 // UUID version 8 (custom); Data3 is little-endian
	id.GUID[8] = id.GUID[8]&0x3f | 0x80 // RFC 4122 variant
	id.Serial = fmt.Sprintf("%X", sum[16:22])
	return id
}

// Manager creates and removes the agent's virtual display. One exists at a
// time (one streaming session per host).
type Manager struct {
	opts    Options
	log     *slog.Logger
	sys     system
	drivers []driver
	id      monitorID

	poll        time.Duration // CCD polling interval
	appearWait  time.Duration // how long a plugged monitor may take to show up
	activateAt  time.Duration // when an available but inactive monitor is switched on
	departWait  time.Duration // how long an unplugged monitor may take to go
	monitorWait time.Duration // how long GDI may take to list the configured display

	mu  sync.Mutex
	cur *monitor // the plugged virtual display, owned or lingering
}

// monitor is a plugged virtual display.
type monitor struct {
	drv    driver
	plug   plug
	mode   Mode
	layout string
	before Config // active topology before the plug: what Close restores
	info   Info
	owner  *Display // nil while lingering
	linger *time.Timer
	stop   chan struct{} // ends the keepalive
	done   chan struct{} // keepalive ended
	lost   chan struct{} // closed when the driver stops answering
	lostMu sync.Once
}

func newManager(opts Options, sys system, drivers []driver) *Manager {
	if opts.Policy == "" {
		opts.Policy = PolicyOff
	}
	if opts.Layout == "" {
		opts.Layout = LayoutPrimary
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		opts: opts, log: log, sys: sys, drivers: drivers, id: newMonitorID(opts.MonitorID),
		poll: 50 * time.Millisecond, appearWait: 6 * time.Second, activateAt: 1500 * time.Millisecond,
		departWait: 5 * time.Second, monitorWait: 3 * time.Second,
	}
}

// Policy is the configured virtualDisplay policy.
func (m *Manager) Policy() string { return m.opts.Policy }

// Detect returns the first usable driver: SudoVDA (any mode on demand, crash
// watchdog), then the Virtual Display Driver.
func (m *Manager) Detect() Status {
	if m.sys == nil {
		return Status{Err: ErrUnsupported}
	}
	var errs []string
	for _, d := range m.drivers {
		detail, err := d.Detect()
		if err == nil {
			return Status{Driver: d.Name(), Detail: detail}
		}
		if !errors.Is(err, ErrNoDriver) {
			errs = append(errs, d.Name()+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return Status{Err: errors.New(strings.Join(errs, "; "))}
	}
	return Status{Err: ErrNoDriver}
}

// Decide applies Decide with this manager's policy and driver detection. When
// phys is the agent's own virtual display (left by the previous session,
// lingering or still owned, and as primary or only display the monitor a
// session picks first), the answer is yes: Create then reuses it for the same
// mode or replaces it. Capturing it without owning it would let the linger
// timer remove it under the running stream.
func (m *Manager) Decide(req Mode, phys *platform.Monitor) (bool, string) {
	if m.opts.Policy != PolicyAuto && m.opts.Policy != PolicyOn {
		return false, ""
	}
	if phys != nil && m.isCurrent(phys.Name) {
		return true, "the monitor is the previous session's virtual display"
	}
	return Decide(m.opts.Policy, m.Detect().Driver != "", req, phys)
}

// isCurrent reports whether name (a GDI display name) is the agent's virtual
// display and it is still there.
func (m *Manager) isCurrent(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == "" || m.cur == nil || !strings.EqualFold(m.cur.info.Name, name) {
		return false
	}
	select {
	case <-m.cur.lost:
		return false
	default:
		return true
	}
}

// Display is a session's handle on the virtual display.
type Display struct {
	m    *Manager
	mon  *monitor
	once sync.Once
}

// Info describes the display.
func (d *Display) Info() Info { return d.mon.info }

// Lost is closed when the driver stops answering (SudoVDA's watchdog then
// removes the monitor): the session should capture something else.
func (d *Display) Lost() <-chan struct{} { return d.mon.lost }

// Close releases the display: after Options.Linger (at once without) the
// monitor is removed and the previous topology restored, unless a new
// session has taken it over by then.
func (d *Display) Close() error {
	var err error
	d.once.Do(func() { err = d.m.release(d) })
	return err
}

// Create gives the caller a virtual display at mode req with the configured
// layout. A display left by a previous session (lingering, or still owned by
// a session the new one replaces) is reused when it has the same mode and
// layout and is still alive, else removed first.
func (m *Manager) Create(req Mode) (*Display, error) {
	if m.sys == nil {
		return nil, ErrUnsupported
	}
	if err := req.Valid(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.cur; cur != nil {
		alive := true
		select {
		case <-cur.lost:
			alive = false
		default:
		}
		if alive && cur.mode == req && cur.layout == m.opts.Layout {
			if cur.linger != nil {
				cur.linger.Stop()
				cur.linger = nil
			}
			d := &Display{m: m, mon: cur}
			cur.owner = d
			m.log.Info("virtual display reused", "name", cur.info.Name, "mode", req)
			return d, nil
		}
		m.teardown(cur)
	}
	mon, err := m.create(req)
	if err != nil {
		return nil, err
	}
	d := &Display{m: m, mon: mon}
	mon.owner = d
	m.cur = mon
	return d, nil
}

// Close removes the virtual display at once (agent shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		m.teardown(m.cur)
	}
}

func (m *Manager) release(d *Display) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon := m.cur
	if mon == nil || mon != d.mon || mon.owner != d {
		return nil // already replaced or removed
	}
	mon.owner = nil
	if m.opts.Linger > 0 {
		mon.linger = time.AfterFunc(m.opts.Linger, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.cur == mon && mon.owner == nil {
				m.teardown(mon)
			}
		})
		return nil
	}
	return m.teardown(mon)
}

// create plugs a monitor and configures it; on failure it undoes everything.
func (m *Manager) create(req Mode) (*monitor, error) {
	st := m.Detect()
	if st.Driver == "" {
		return nil, st.Err
	}
	var drv driver
	for _, d := range m.drivers {
		if d.Name() == st.Driver {
			drv = d
		}
	}
	before, err := m.sys.Query(QDCOnlyActivePaths)
	if err != nil {
		return nil, fmt.Errorf("reading the display configuration: %w", err)
	}
	mon := &monitor{drv: drv, mode: req, layout: m.opts.Layout, before: before,
		stop: make(chan struct{}), done: make(chan struct{}), lost: make(chan struct{})}
	if err := m.writeJournal(mon, false); err != nil {
		return nil, err
	}
	p, err := drv.Plug(req, m.id, LUIDFrom(m.opts.RenderAdapter))
	if err != nil {
		m.removeJournal()
		drv.Close()
		return nil, fmt.Errorf("%s: %w", drv.Name(), err)
	}
	mon.plug = p
	_ = m.writeJournal(mon, true)
	go m.keepalive(mon)
	if err := m.configure(mon); err != nil {
		m.log.Warn("virtual display setup failed, restoring the displays", "driver", drv.Name(), "mode", req, "err", err)
		m.teardown(mon)
		return nil, err
	}
	m.log.Info("virtual display created", "driver", drv.Name(), "name", mon.info.Name, "mode", req,
		"refresh", fmt.Sprintf("%.3f", mon.info.RefreshHz), "layout", mon.layout, "target", mon.info.Target,
		"hmonitor", fmt.Sprintf("%#x", mon.info.Monitor.HMonitor), "dxgi_output", mon.info.Monitor.DXGIOutput)
	return mon, nil
}

// configure waits for the plugged monitor, switches it on if Windows left it
// off, applies the mode and layout (plan), and fills mon.info.
func (m *Manager) configure(mon *monitor) error {
	t, err := m.waitTarget(mon)
	if err != nil {
		return err
	}
	if mon.plug.Instance == "" && mon.plug.Target != t {
		// The driver reported another adapter LUID than CCD lists (findTarget):
		// waiting for the monitor to depart, here and in Recover, needs CCD's.
		mon.plug.Target = t
		_ = m.writeJournal(mon, true)
	}
	save := !mon.drv.Persistent() && mon.layout != LayoutOnly
	for attempt := 0; ; attempt++ {
		cur, err := m.sys.Query(QDCOnlyActivePaths)
		if err != nil {
			return err
		}
		next, err := plan(cur, t, mon.mode, mon.layout)
		if errors.Is(err, errCloned) && attempt == 0 {
			// Windows duplicates the new monitor (the Win+P "Duplicate"
			// projection): give it a source of its own.
			if err := m.activate(t); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		flags := uint32(sdcSupplied)
		if save {
			flags |= SDCSaveToDatabase
		}
		// Like libdisplaydevice's setDisplayModes: let Windows adjust the
		// mode to one the monitor lists first, then insist on ours. A refresh
		// rate Windows will not set is accepted (the stream then runs at
		// most at it); any other difference fails. degraded: what Windows
		// applied last differs only in the refresh rate (a failed
		// SetDisplayConfig changes nothing).
		var hz float64
		var errs []error
		ok, degraded := false, false
		for _, strict := range []bool{false, true} {
			f := flags
			if !strict {
				f |= SDCAllowChanges
			}
			if err := m.sys.Apply(next, f); err != nil {
				errs = append(errs, fmt.Errorf("SetDisplayConfig: %w", err))
				continue
			}
			after, err := m.sys.Query(QDCOnlyActivePaths)
			if err != nil {
				return err
			}
			got, err := checkPlan(after, t, mon.mode, mon.layout)
			if err == nil {
				hz, ok = got, true
				break
			}
			if degraded = errors.Is(err, errRefresh); degraded {
				hz = got
			}
			errs = append(errs, err)
		}
		if !ok {
			if !degraded {
				return fmt.Errorf("setting %v (%s): %w", mon.mode, mon.layout, errors.Join(errs...))
			}
			m.log.Warn("Windows did not set the virtual display's refresh rate; the stream runs at most at the one it has",
				"mode", mon.mode, "refresh", fmt.Sprintf("%.3f", hz))
		}
		return m.fillInfo(mon, t, hz)
	}
}

// waitTarget polls until the plugged monitor is active, switching it on when
// it is available but still off after activateAt (Windows applies the
// display database to a new monitor; a saved "PC screen only" projection
// leaves it off).
func (m *Manager) waitTarget(mon *monitor) (Target, error) {
	start := time.Now()
	activated := false
	for {
		all, err := m.sys.Query(QDCAllPaths)
		if err != nil {
			return Target{}, err
		}
		if t, ok := m.resolve(mon, all); ok {
			active, err := m.sys.Query(QDCOnlyActivePaths)
			if err != nil {
				return Target{}, err
			}
			if active.activePath(t) >= 0 {
				return t, nil
			}
			if !activated && time.Since(start) >= m.activateAt {
				if err := m.activate(t); err != nil {
					return Target{}, err
				}
				activated = true
				continue
			}
		}
		if time.Since(start) > m.appearWait {
			return Target{}, fmt.Errorf("the virtual monitor did not appear within %v", m.appearWait)
		}
		time.Sleep(m.poll)
	}
}

// resolve finds the plugged monitor's CCD target in all (a QDC_ALL_PATHS query).
func (m *Manager) resolve(mon *monitor, all Config) (Target, bool) {
	if mon.plug.Instance == "" {
		before := mon.before
		return findTarget(before, all, mon.plug.Target)
	}
	// VDD: the first available target of its adapter (lowest target id).
	var best *Target
	insts := map[LUID]string{}
	for i := range all.Paths {
		p := &all.Paths[i]
		if p.Target.TargetAvailable == 0 {
			continue
		}
		inst, ok := insts[p.Target.AdapterID]
		if !ok {
			inst, _ = m.sys.AdapterInstance(p.Target.AdapterID)
			insts[p.Target.AdapterID] = inst
		}
		if !strings.EqualFold(inst, mon.plug.Instance) {
			continue
		}
		if t := p.target(); best == nil || t.ID < best.ID {
			best = &t
		}
	}
	if best == nil {
		return Target{}, false
	}
	return *best, true
}

// activate switches target t on, next to the active displays.
func (m *Manager) activate(t Target) error {
	all, err := m.sys.Query(QDCAllPaths)
	if err != nil {
		return err
	}
	active, err := m.sys.Query(QDCOnlyActivePaths)
	if err != nil {
		return err
	}
	paths, err := activateTarget(all, active, t)
	if err != nil {
		return err
	}
	if err := m.sys.Apply(Config{Paths: paths}, sdcSupplied|SDCAllowChanges); err != nil {
		return fmt.Errorf("switching the virtual monitor on: %w", err)
	}
	return nil
}

// fillInfo records where the configured display is.
func (m *Manager) fillInfo(mon *monitor, t Target, hz float64) error {
	cur, err := m.sys.Query(QDCOnlyActivePaths)
	if err != nil {
		return err
	}
	vi := cur.activePath(t)
	if vi < 0 {
		return fmt.Errorf("target %v is no longer active", t)
	}
	name, err := m.sys.SourceName(cur.Paths[vi].Source.AdapterID, cur.Paths[vi].Source.ID)
	if err != nil {
		return fmt.Errorf("display name: %w", err)
	}
	info := Info{Driver: mon.drv.Name(), Name: name, Target: t, RefreshHz: hz, Layout: mon.layout,
		Mode: Mode{Width: mon.mode.Width, Height: mon.mode.Height, Hz: int(hz + 0.5)}}
	// GDI lists the display (HMONITOR) shortly after the change.
	deadline := time.Now().Add(m.monitorWait)
	for {
		pm, ok := m.sys.Monitor(name)
		if ok && pm.W == mon.mode.Width && pm.H == mon.mode.Height {
			info.Monitor = pm
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("display %s (%v) is not listed as a %dx%d monitor", name, t, mon.mode.Width, mon.mode.Height)
		}
		time.Sleep(m.poll)
	}
	mon.info = info
	return nil
}

// teardown removes mon and restores the topology from before it. Called with
// m.mu held.
func (m *Manager) teardown(mon *monitor) error {
	if mon.linger != nil {
		mon.linger.Stop()
		mon.linger = nil
	}
	mon.owner = nil
	select {
	case <-mon.stop:
	default:
		close(mon.stop)
	}
	<-mon.done
	err := m.undo(mon.drv, mon.plug, mon.before)
	mon.drv.Close()
	if m.cur == mon {
		m.cur = nil
	}
	if err != nil {
		m.log.Warn("virtual display removed, display restore incomplete", "driver", mon.drv.Name(), "err", err)
	} else {
		m.log.Info("virtual display removed, displays restored", "driver", mon.drv.Name(), "name", mon.info.Name)
	}
	m.removeJournal()
	return err
}

// undo unplugs p and puts the topology before back. A monitor that departs
// is unplugged first (restoring before it is gone would let Windows
// rearrange the desktop once more when it leaves); one that stays connected
// (VDD) is switched off by the restore itself.
func (m *Manager) undo(drv driver, p plug, before Config) error {
	var errs []error
	if p.Departs {
		if err := drv.Unplug(p); err != nil {
			errs = append(errs, err)
		} else {
			m.waitDeparture(p)
		}
	}
	if err := m.restore(before); err != nil {
		errs = append(errs, err)
	}
	if !p.Departs {
		if err := drv.Unplug(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitDeparture polls until the unplugged monitor has left the topology.
func (m *Manager) waitDeparture(p plug) {
	deadline := time.Now().Add(m.departWait)
	for time.Now().Before(deadline) {
		all, err := m.sys.Query(QDCAllPaths)
		if err != nil {
			return
		}
		gone := true
		if p.Instance != "" {
			for i := range all.Paths {
				if all.Paths[i].Target.TargetAvailable == 0 {
					continue
				}
				if inst, _ := m.sys.AdapterInstance(all.Paths[i].Target.AdapterID); strings.EqualFold(inst, p.Instance) {
					gone = false
					break
				}
			}
		} else {
			gone = !all.hasTarget(p.Target)
		}
		if gone {
			return
		}
		time.Sleep(m.poll)
	}
	m.log.Warn("the virtual monitor is still connected after unplugging it", "wait", m.departWait)
}

// restore applies before exactly, then with Windows allowed to adjust it,
// then Windows' own saved layout for the connected displays. Nothing is saved
// to the display database.
func (m *Manager) restore(before Config) error {
	if len(before.Paths) == 0 {
		return m.sys.Apply(Config{}, SDCApply|SDCUseDatabaseCurrent)
	}
	err1 := m.sys.Apply(before, sdcSupplied)
	if err1 == nil {
		return nil
	}
	err2 := m.sys.Apply(before, sdcSupplied|SDCAllowChanges)
	if err2 == nil {
		return nil
	}
	if err3 := m.sys.Apply(Config{}, SDCApply|SDCUseDatabaseCurrent); err3 != nil {
		return fmt.Errorf("restoring the displays: %w; %w; database: %w", err1, err2, err3)
	}
	m.log.Info("previous display layout could not be applied, used Windows' saved layout", "err", err1)
	return nil
}

// keepalive pings the driver until mon.stop; after 4 failures in a row it
// reports the display lost.
func (m *Manager) keepalive(mon *monitor) {
	defer close(mon.done)
	every := mon.drv.KeepaliveEvery()
	if every <= 0 {
		<-mon.stop
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-mon.stop:
			return
		case <-t.C:
		}
		if err := mon.drv.Keepalive(); err != nil {
			fails++
			if fails > 3 {
				m.log.Warn("virtual display driver stopped answering, the display is gone", "driver", mon.drv.Name(), "err", err)
				mon.lostMu.Do(func() { close(mon.lost) })
				<-mon.stop
				return
			}
			continue
		}
		fails = 0
	}
}

// journal records a virtual display while it exists, for Recover.
type journal struct {
	Version int       `json:"version"`
	Driver  string    `json:"driver"`
	Mode    string    `json:"mode"`
	Plugged bool      `json:"plugged"`
	Plug    plug      `json:"plug"`
	Before  Config    `json:"before"`
	Time    time.Time `json:"time"`
}

const journalName = "vdisplay-restore.json"

func (m *Manager) journalPath() string {
	if m.opts.StateDir == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, journalName)
}

func (m *Manager) writeJournal(mon *monitor, plugged bool) error {
	path := m.journalPath()
	if path == "" {
		return nil
	}
	b, err := json.Marshal(journal{Version: 1, Driver: mon.drv.Name(), Mode: mon.mode.String(), Plugged: plugged,
		Plug: mon.plug, Before: mon.before, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("virtual display journal: %w", err)
	}
	return os.Rename(tmp, path)
}

func (m *Manager) removeJournal() {
	if path := m.journalPath(); path != "" {
		_ = os.Remove(path)
	}
}

// Recover puts the displays back after an agent that stopped while it had a
// virtual display (crash, power loss): it unplugs the monitor a journal
// recorded and restores the topology from before it. Call it once at startup,
// before any session. Without a journal it does nothing.
func (m *Manager) Recover() error {
	path := m.journalPath()
	if path == "" || m.sys == nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer m.removeJournal()
	var j journal
	if err := json.Unmarshal(b, &j); err != nil || j.Version != 1 {
		return fmt.Errorf("virtual display journal %s unreadable (%v), removed", path, err)
	}
	m.log.Info("restoring the displays after an unfinished virtual display session", "driver", j.Driver, "mode", j.Mode, "since", j.Time)
	var drv driver
	for _, d := range m.drivers {
		if d.Name() == j.Driver {
			drv = d
		}
	}
	var errs []error
	if drv != nil {
		// Also before the plug was recorded: the agent may have stopped
		// right after creating the monitor (SudoVDA removes it by GUID).
		if err := drv.Recover(j.Plug, m.id); err != nil {
			errs = append(errs, err)
		} else if j.Plugged && j.Plug.Departs {
			m.waitDeparture(j.Plug)
		}
		drv.Close()
	}
	// The saved topology names adapters by LUID, which a reboot changes:
	// restore falls back to Windows' saved layout then.
	if err := m.restore(j.Before); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
