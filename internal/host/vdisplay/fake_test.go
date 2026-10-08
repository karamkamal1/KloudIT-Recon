package vdisplay

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

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
			return platform.Monitor{Index: i, Name: name, X: t.x, Y: t.y, W: t.w, H: t.h, Primary: t.x == 0 && t.y == 0,
				Hz: int(t.hz + 0.5), HMonitor: uint64(0x10000 + i), DXGIOutput: i}, true
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

// fakeDriver plugs monitors into a fakeSys.
type fakeDriver struct {
	sys        *fakeSys
	name       string
	detectErr  error
	plugErr    error
	persistent bool
	vddLike    bool   // reports the adapter instance instead of the target
	reportLUID LUID   // adapter LUID it reports (findTarget fallback when it differs)
	adapter    LUID   // where its monitors appear
	inactive   bool   // monitors arrive inactive
	rotation   uint32 // rotation the monitor arrives with
	cloned     bool   // the monitor arrives duplicating the first display
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
	if p.Departs {
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
