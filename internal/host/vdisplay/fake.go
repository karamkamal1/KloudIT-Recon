package vdisplay

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// Test double of Windows' display configuration and an IddCx driver (tests
// only, here and, through Sim, in the packages that use a Manager): the
// manager's logic runs against it on any OS.

// fakeSys simulates Windows' display configuration closely enough for the
// manager: connected monitors (targets) with their supported refresh rates,
// which of them are active with which source mode, and the CCD arrays
// QueryDisplayConfig would return for that (virtual-mode-aware indices).
type fakeSys struct {
	mu        sync.Mutex
	targets   []*fakeTarget
	nextSrc   map[LUID]uint32
	applies   []uint32       // flags of every Apply
	fail      map[uint32]int // Apply flags -> how many more calls with them fail
	instances map[LUID]string
	database  map[Target]fakeState // what SDC_USE_DATABASE_CURRENT restores
	dxgiOff   map[LUID]bool        // adapters whose outputs are not on DXGI adapter 0 (DXGIOutput -1)
	events    []string             // "apply" and "depart <target>", in order
}

type fakeState struct {
	active   bool
	w, h     int
	x, y     int
	hz       float64
	rotation uint32
}

type fakeTarget struct {
	t         Target
	name      string // GDI name of its source
	hzs       []float64
	srcID     uint32
	cloneOf   *fakeTarget // shares this target's source (duplicate projection)
	connected bool
	fakeState
}

func newFakeSys() *fakeSys {
	return &fakeSys{nextSrc: map[LUID]uint32{}, instances: map[LUID]string{}, database: map[Target]fakeState{}}
}

var gpuLUID = LUID{Low: 0xc3a1}

// addPhysical connects an active physical monitor on the GPU adapter.
func (s *fakeSys) addPhysical(id uint32, w, h, x, y int, hz float64) *fakeTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &fakeTarget{t: Target{gpuLUID, id}, hzs: []float64{hz}, connected: true,
		fakeState: fakeState{active: true, w: w, h: h, x: x, y: y, hz: hz, rotation: RotationIdentity}}
	t.srcID = s.nextSrc[gpuLUID]
	s.nextSrc[gpuLUID]++
	t.name = fmt.Sprintf(`\\.\DISPLAY%d`, len(s.targets)+1)
	s.targets = append(s.targets, t)
	s.database[t.t] = t.fakeState
	return t
}

// plugVirtual connects a monitor whose preferred mode is m, as an IddCx
// monitor arrives: active (extended right of the desktop) unless inactive.
func (s *fakeSys) plugVirtual(t Target, m Mode, inactive bool, rotation uint32, hzs []float64) *fakeTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.targets {
		if o.t == t {
			o.connected, o.active = true, !inactive
			return o
		}
	}
	if hzs == nil {
		hzs = []float64{float64(m.Hz), 60}
	}
	right := 0
	for _, o := range s.targets {
		if o.active && o.connected && o.x+o.w > right {
			right = o.x + o.w
		}
	}
	w, h := m.Width, m.Height
	if rotation == 2 || rotation == 4 {
		w, h = h, w
	}
	ft := &fakeTarget{t: t, hzs: hzs, connected: true, name: fmt.Sprintf(`\\.\DISPLAY%d`, len(s.targets)+1),
		fakeState: fakeState{active: !inactive, w: w, h: h, x: right, hz: float64(m.Hz), rotation: rotation}}
	ft.srcID = s.nextSrc[t.Adapter]
	s.nextSrc[t.Adapter]++
	s.targets = append(s.targets, ft)
	return ft
}

func (s *fakeSys) unplug(t Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.targets {
		if o.t == t && o.connected {
			o.connected, o.active = false, false
			s.events = append(s.events, fmt.Sprintf("depart %v", t))
			// Windows applies its database to the remaining displays.
			for _, p := range s.targets {
				if st, ok := s.database[p.t]; ok && p.connected {
					p.fakeState = st
				}
			}
		}
	}
}

func (s *fakeSys) find(t Target) *fakeTarget {
	for _, o := range s.targets {
		if o.t == t && o.connected {
			return o
		}
	}
	return nil
}

func (s *fakeSys) Query(flags uint32) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Config
	for _, t := range s.targets {
		if !t.connected || !t.active {
			continue
		}
		src := t
		if t.cloneOf != nil {
			src = t.cloneOf
		}
		var sm ModeInfo
		sm.InfoType, sm.ID, sm.AdapterID = modeTypeSource, src.srcID, src.t.Adapter
		sm.SetSource(SourceMode{Width: uint32(src.w), Height: uint32(src.h), PixelFormat: 4, X: int32(src.x), Y: int32(src.y)})
		var tm ModeInfo
		tm.InfoType, tm.ID, tm.AdapterID = modeTypeTarget, t.t.ID, t.t.Adapter
		c.Modes = append(c.Modes, sm, tm)
		si, ti := uint32(len(c.Modes)-2), uint32(len(c.Modes)-1)
		c.Paths = append(c.Paths, PathInfo{
			Source: PathSourceInfo{AdapterID: src.t.Adapter, ID: src.srcID, ModeInfoIdx: si<<16 | idx16Invalid},
			Target: PathTargetInfo{AdapterID: t.t.Adapter, ID: t.t.ID, ModeInfoIdx: ti<<16 | idx16Invalid,
				Rotation: t.rotation, RefreshRate: Rational{uint32(t.hz * 1000), 1000}, TargetAvailable: 1},
			Flags: pathActive | pathSupportVirtualMode,
		})
	}
	if flags&QDCAllPaths != 0 {
		// Inactive candidate paths: every connected target with every
		// source id of its adapter (0-3).
		for _, t := range s.targets {
			if !t.connected {
				continue
			}
			for sid := uint32(0); sid < 4; sid++ {
				c.Paths = append(c.Paths, PathInfo{
					Source: PathSourceInfo{AdapterID: t.t.Adapter, ID: sid, ModeInfoIdx: idx16Invalid<<16 | idx16Invalid},
					Target: PathTargetInfo{AdapterID: t.t.Adapter, ID: t.t.ID, ModeInfoIdx: idx16Invalid<<16 | idx16Invalid, TargetAvailable: 1},
				})
			}
		}
	}
	return c, nil
}

var errFakeApply = errors.New("ERROR_INVALID_PARAMETER")

func (s *fakeSys) Apply(c Config, flags uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies = append(s.applies, flags)
	s.events = append(s.events, "apply")
	if s.fail[flags] > 0 {
		s.fail[flags]--
		return errFakeApply
	}
	if flags&SDCUseDatabaseCurrent == SDCUseDatabaseCurrent && len(c.Paths) == 0 {
		for _, t := range s.targets {
			if st, ok := s.database[t.t]; ok && t.connected {
				t.fakeState = st
			} else {
				t.active = false
			}
		}
		return nil
	}
	if flags&SDCUseSuppliedDisplayConf == 0 {
		return errFakeApply
	}
	// Validate first, then apply.
	type upd struct {
		t   *fakeTarget
		st  fakeState
		src uint32
	}
	var ups []upd
	usedSrc := map[[2]uint64]bool{}
	for i := range c.Paths {
		p := &c.Paths[i]
		if !p.active() {
			continue
		}
		t := s.find(p.target())
		if t == nil {
			return errFakeApply
		}
		key := [2]uint64{p.Source.AdapterID.Uint64(), uint64(p.Source.ID)}
		if usedSrc[key] {
			return errFakeApply // the fake does not do clone mode on Apply
		}
		usedSrc[key] = true
		st := t.fakeState
		st.active = true
		st.rotation = p.Target.Rotation
		if st.rotation == 0 {
			st.rotation = RotationIdentity
		}
		if si := p.sourceIdx(); si != idx16Invalid {
			if int(si) >= len(c.Modes) || c.Modes[si].InfoType != modeTypeSource {
				return errFakeApply
			}
			sm := c.Modes[si].Source()
			st.w, st.h, st.x, st.y = int(sm.Width), int(sm.Height), int(sm.X), int(sm.Y)
		} else if !t.active || t.cloneOf != nil {
			// Windows picks a mode and a place for a newly activated path.
			right := 0
			for _, o := range s.targets {
				if o.active && o.connected && o != t && o.x+o.w > right {
					right = o.x + o.w
				}
			}
			st.x, st.y = right, 0
		}
		if r := p.Target.RefreshRate; r.Den != 0 {
			want := r.Hz()
			best := t.hzs[0]
			for _, hz := range t.hzs {
				if math.Abs(hz-want) < math.Abs(best-want) {
					best = hz
				}
			}
			if math.Abs(best-want) > 0.01 && flags&SDCAllowChanges == 0 {
				return errFakeApply
			}
			st.hz = best
		}
		ups = append(ups, upd{t, st, p.Source.ID})
	}
	if len(ups) == 0 {
		return errFakeApply
	}
	for _, t := range s.targets {
		t.active = false
	}
	for _, u := range ups {
		u.t.fakeState = u.st
		u.t.srcID = u.src
		u.t.cloneOf = nil
	}
	return nil
}

func (s *fakeSys) SourceName(adapter LUID, id uint32) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.targets {
		if t.connected && t.active && t.t.Adapter == adapter && t.srcID == id {
			return t.name, nil
		}
	}
	return "", errors.New("no such source")
}

func (s *fakeSys) AdapterInstance(adapter LUID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := s.instances[adapter]; ok {
		return inst, nil
	}
	return `PCI\VEN_1002&DEV_744C\0`, nil
}

func (s *fakeSys) Monitor(name string) (platform.Monitor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.targets {
		if t.connected && t.active && t.name == name {
			out := i
			if s.dxgiOff[t.t.Adapter] {
				out = -1
			}
			return platform.Monitor{Index: i, Name: name, X: t.x, Y: t.y, W: t.w, H: t.h, Primary: t.x == 0 && t.y == 0,
				Hz: int(t.hz + 0.5), HMonitor: uint64(0x10000 + i), DXGIOutput: out}, true
		}
	}
	return platform.Monitor{}, false
}

// state returns a target's current state.
func (s *fakeSys) state(t Target) (fakeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ft := s.find(t); ft != nil {
		return ft.fakeState, true
	}
	return fakeState{}, false
}

func (s *fakeSys) flags() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint32(nil), s.applies...)
}

func (s *fakeSys) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// fakeDriver plugs monitors into a fakeSys.
type fakeDriver struct {
	sys        *fakeSys
	name       string
	detectErr  error
	plugErr    error
	persistent bool
	vddLike    bool          // reports the adapter instance instead of the target
	reportLUID LUID          // adapter LUID it reports (findTarget fallback when it differs)
	adapter    LUID          // where its monitors appear
	inactive   bool          // monitors arrive inactive
	rotation   uint32        // rotation the monitor arrives with
	cloned     bool          // the monitor arrives duplicating the first display
	departIn   time.Duration // an unplugged monitor leaves this much later (0: at once)
	hzs        []float64
	every      time.Duration
	pingErr    atomic.Bool

	mu       sync.Mutex
	plugs    int
	unplugs  int
	recovers int
	calls    []string
	target   Target
}

func (d *fakeDriver) Name() string { return d.name }

func (d *fakeDriver) Detect() (string, error) { return "fake", d.detectErr }

func (d *fakeDriver) log(s string) {
	d.mu.Lock()
	d.calls = append(d.calls, s)
	d.mu.Unlock()
}

func (d *fakeDriver) Plug(m Mode, id monitorID, render LUID) (plug, error) {
	if d.plugErr != nil {
		return plug{}, d.plugErr
	}
	d.mu.Lock()
	d.plugs++
	d.target = Target{d.adapter, 0x1000 + uint32(d.plugs)}
	t := d.target
	d.mu.Unlock()
	d.log("plug")
	ft := d.sys.plugVirtual(t, m, d.inactive, max(d.rotation, RotationIdentity), d.hzs)
	if d.cloned {
		d.sys.mu.Lock()
		ft.cloneOf = d.sys.targets[0]
		d.sys.mu.Unlock()
	}
	if d.vddLike {
		d.sys.mu.Lock()
		d.sys.instances[d.adapter] = `ROOT\DISPLAY\0001`
		d.sys.mu.Unlock()
		return plug{Instance: `ROOT\DISPLAY\0001`, Departs: !d.persistent}, nil
	}
	rt := t
	if d.reportLUID != (LUID{}) {
		rt.Adapter = d.reportLUID
	}
	return plug{Target: rt, Departs: true}, nil
}

func (d *fakeDriver) Unplug(p plug) error {
	d.mu.Lock()
	d.unplugs++
	t := d.target
	d.mu.Unlock()
	d.log("unplug")
	if p.Departs && d.departIn > 0 {
		time.AfterFunc(d.departIn, func() { d.sys.unplug(t) })
	} else if p.Departs {
		d.sys.unplug(t)
	}
	return nil
}

func (d *fakeDriver) Keepalive() error {
	if d.pingErr.Load() {
		return errors.New("ping failed")
	}
	return nil
}

func (d *fakeDriver) KeepaliveEvery() time.Duration { return d.every }
func (d *fakeDriver) Persistent() bool              { return d.persistent }

func (d *fakeDriver) Recover(p plug, id monitorID) error {
	d.mu.Lock()
	d.recovers++
	t := d.target
	d.mu.Unlock()
	d.log("recover")
	if p.Departs {
		d.sys.unplug(t)
	}
	return nil
}

func (d *fakeDriver) Close() {}

func (d *fakeDriver) count() (plugs, unplugs, recovers int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.plugs, d.unplugs, d.recovers
}

// Sim is a simulated PC for tests of code that uses a Manager (the sessions in
// internal/host): physical monitors, the display configuration Windows keeps
// for them (CCD, GDI), and a virtual display driver, all the test double
// above. Configure it before handing its Manager out: the setters are not
// synchronised with a running Manager, except Lose.
type Sim struct {
	sys *fakeSys
	drv *fakeDriver
}

// simLUID is the adapter the simulated driver's monitors appear on.
var simLUID = LUID{Low: 0x5eda, High: 1}

// NewSim returns a PC without monitors and with driver (DriverSudoVDA: a
// monitor added and removed on demand, pinged every 5 ms; DriverVDD: the
// device enabled for a session, no keepalive).
func NewSim(driver string) *Sim {
	sys := newFakeSys()
	drv := &fakeDriver{sys: sys, name: DriverSudoVDA, adapter: simLUID, every: 5 * time.Millisecond}
	if driver == DriverVDD {
		drv.name, drv.vddLike, drv.every = DriverVDD, true, 0
	}
	return &Sim{sys: sys, drv: drv}
}

// AddMonitor connects an active physical monitor of w x h at (x, y) on the
// desktop, refreshing at hz.
func (s *Sim) AddMonitor(w, h, x, y, hz int) {
	s.sys.mu.Lock()
	id := uint32(len(s.sys.targets) + 1)
	s.sys.mu.Unlock()
	s.sys.addPhysical(id, w, h, x, y, float64(hz))
}

// Manager returns a Manager on this PC, as New does on Windows, with
// timeouts and polling for tests (the simulated displays change at once).
func (s *Sim) Manager(opts Options) *Manager {
	m := newManager(opts, s.sys, []driver{s.drv})
	m.poll, m.activateAt, m.appearWait, m.departWait, m.monitorWait = time.Millisecond, 20*time.Millisecond, time.Second, time.Second, time.Second
	return m
}

// Monitors lists the active displays as platform.Monitors does: primary
// first, then left to right, Index in that order; HMonitor and DXGIOutput are
// fixed per display.
func (s *Sim) Monitors() []platform.Monitor {
	s.sys.mu.Lock()
	var names []string
	for _, t := range s.sys.targets {
		if t.connected && t.active && t.cloneOf == nil {
			names = append(names, t.name)
		}
	}
	s.sys.mu.Unlock()
	var mons []platform.Monitor
	for _, n := range names {
		if m, ok := s.sys.Monitor(n); ok {
			mons = append(mons, m)
		}
	}
	sort.SliceStable(mons, func(i, j int) bool {
		if mons[i].Primary != mons[j].Primary {
			return mons[i].Primary
		}
		return mons[i].X < mons[j].X
	})
	for i := range mons {
		mons[i].Index = i
	}
	return mons
}

// RemoveDriver makes Detect report that no driver is installed.
func (s *Sim) RemoveDriver() { s.drv.detectErr = ErrNoDriver }

// FailPlug makes the driver fail to add a monitor with err.
func (s *Sim) FailPlug(err error) { s.drv.plugErr = err }

// OffAdapter0 puts the driver's monitors on another DXGI adapter than 0 (the
// render adapter was not applied): their DXGIOutput is -1.
func (s *Sim) OffAdapter0() {
	s.sys.mu.Lock()
	defer s.sys.mu.Unlock()
	if s.sys.dxgiOff == nil {
		s.sys.dxgiOff = map[LUID]bool{}
	}
	s.sys.dxgiOff[simLUID] = true
}

// Lose makes the driver stop answering and remove its monitor, as SudoVDA's
// watchdog does when the agent stops pinging (Windows then rearranges the
// remaining displays from its database). With DriverVDD nothing reports the
// loss (no keepalive).
func (s *Sim) Lose() {
	s.drv.pingErr.Store(true)
	s.drv.mu.Lock()
	t := s.drv.target
	s.drv.mu.Unlock()
	s.sys.unplug(t)
}

// Counts returns how many monitors the driver plugged, unplugged and removed
// after a crash (Recover).
func (s *Sim) Counts() (plugs, unplugs, recovers int) { return s.drv.count() }
