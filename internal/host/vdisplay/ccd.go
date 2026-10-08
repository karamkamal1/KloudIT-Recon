package vdisplay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Windows CCD ("Connecting and Configuring Displays") data: what
// QueryDisplayConfig returns and SetDisplayConfig takes. The structs mirror
// wingdi.h byte for byte (x64: DISPLAYCONFIG_PATH_INFO is 72 bytes,
// DISPLAYCONFIG_MODE_INFO 64), so the Windows code passes slices of them to
// the API as they are, and the topology logic below runs (and is tested) on
// every platform. The agent always queries and sets with the virtual-mode-
// aware flags (QDC_VIRTUAL_MODE_AWARE, SDC_VIRTUAL_MODE_AWARE), as Sunshine's
// libdisplaydevice does (src/windows/win_api_layer.cpp): every path then has
// its own source mode, and the mode indices are 16-bit halves of the
// modeInfoIdx unions.

// LUID is a locally unique identifier (adapter id); it changes across reboots
// and driver restarts.
type LUID struct {
	Low  uint32
	High int32
}

// Uint64 packs the LUID as HighPart<<32 | LowPart (the helper and
// platform.Adapter format).
func (l LUID) Uint64() uint64 { return uint64(uint32(l.High))<<32 | uint64(l.Low) }

// LUIDFrom unpacks Uint64.
func LUIDFrom(v uint64) LUID { return LUID{Low: uint32(v), High: int32(uint32(v >> 32))} }

func (l LUID) String() string { return fmt.Sprintf("%08x:%08x", uint32(l.High), l.Low) }

// Rational is DISPLAYCONFIG_RATIONAL.
type Rational struct{ Num, Den uint32 }

// Hz returns the rate in hertz (0 when the denominator is 0).
func (r Rational) Hz() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// PathSourceInfo is DISPLAYCONFIG_PATH_SOURCE_INFO.
type PathSourceInfo struct {
	AdapterID LUID
	ID        uint32
	// ModeInfoIdx: low 16 bits cloneGroupId, high 16 bits sourceModeInfoIdx
	// (virtual-mode-aware layout).
	ModeInfoIdx uint32
	StatusFlags uint32
}

// PathTargetInfo is DISPLAYCONFIG_PATH_TARGET_INFO.
type PathTargetInfo struct {
	AdapterID LUID
	ID        uint32
	// ModeInfoIdx: low 16 bits desktopModeInfoIdx, high 16 bits
	// targetModeInfoIdx (virtual-mode-aware layout).
	ModeInfoIdx      uint32
	OutputTechnology uint32
	Rotation         uint32
	Scaling          uint32
	RefreshRate      Rational
	ScanLineOrdering uint32
	TargetAvailable  int32
	StatusFlags      uint32
}

// PathInfo is DISPLAYCONFIG_PATH_INFO: one source (desktop surface) driving
// one target (monitor connector).
type PathInfo struct {
	Source PathSourceInfo
	Target PathTargetInfo
	Flags  uint32
}

// ModeInfo is DISPLAYCONFIG_MODE_INFO. Union holds a source mode, a target
// mode (DISPLAYCONFIG_VIDEO_SIGNAL_INFO) or a desktop image info by InfoType;
// the accessors below read and write it little-endian at the wingdi.h offsets.
type ModeInfo struct {
	_         [0]uint64 // the union contains a UINT64: 8-byte alignment, 64 bytes in all
	InfoType  uint32
	ID        uint32
	AdapterID LUID
	Union     [48]byte
}

// CCD constants (wingdi.h).
const (
	modeTypeSource       = 1 // DISPLAYCONFIG_MODE_INFO_TYPE_SOURCE
	modeTypeTarget       = 2 // DISPLAYCONFIG_MODE_INFO_TYPE_TARGET
	modeTypeDesktopImage = 3 // DISPLAYCONFIG_MODE_INFO_TYPE_DESKTOP_IMAGE

	pathActive             = 0x1 // DISPLAYCONFIG_PATH_ACTIVE
	pathSupportVirtualMode = 0x8 // DISPLAYCONFIG_PATH_SUPPORT_VIRTUAL_MODE

	idx16Invalid = 0xffff // DISPLAYCONFIG_PATH_{SOURCE,TARGET}_MODE_IDX_INVALID, ..._DESKTOP_IMAGE_IDX_INVALID, ..._CLONE_GROUP_INVALID

	RotationIdentity = 1 // DISPLAYCONFIG_ROTATION_IDENTITY (2-4: 90, 180, 270 degrees)

	QDCAllPaths         = 0x01 // QDC_ALL_PATHS
	QDCOnlyActivePaths  = 0x02 // QDC_ONLY_ACTIVE_PATHS
	QDCVirtualModeAware = 0x10 // QDC_VIRTUAL_MODE_AWARE

	SDCUseDatabaseCurrent     = 0x000f // SDC_USE_DATABASE_CURRENT
	SDCUseSuppliedDisplayConf = 0x0020 // SDC_USE_SUPPLIED_DISPLAY_CONFIG
	SDCApply                  = 0x0080 // SDC_APPLY
	SDCSaveToDatabase         = 0x0200 // SDC_SAVE_TO_DATABASE
	SDCAllowChanges           = 0x0400 // SDC_ALLOW_CHANGES
	SDCVirtualModeAware       = 0x8000 // SDC_VIRTUAL_MODE_AWARE

	sdcSupplied         = SDCApply | SDCUseSuppliedDisplayConf | SDCVirtualModeAware
	refreshTolerance    = 0.9     // Hz, as libdisplaydevice's fuzzyCompareRefreshRates
	maxSourceCoordinate = 1 << 15 // GDI desktop coordinates are 16-bit in places
)

// Config is a display configuration: QueryDisplayConfig's path and mode
// arrays (QDC_VIRTUAL_MODE_AWARE).
type Config struct {
	Paths []PathInfo
	Modes []ModeInfo
}

// Clone deep-copies c.
func (c Config) Clone() Config {
	return Config{Paths: append([]PathInfo(nil), c.Paths...), Modes: append([]ModeInfo(nil), c.Modes...)}
}

// Target identifies a monitor connector: the adapter and the target id CCD
// paths carry (SudoVDA's VIRTUAL_DISPLAY_ADD_OUT reports exactly these).
type Target struct {
	Adapter LUID
	ID      uint32
}

func (t Target) String() string { return fmt.Sprintf("%v/%d", t.Adapter, t.ID) }

func (p *PathInfo) active() bool       { return p.Flags&pathActive != 0 }
func (p *PathInfo) target() Target     { return Target{p.Target.AdapterID, p.Target.ID} }
func (p *PathInfo) sourceIdx() uint32  { return p.Source.ModeInfoIdx >> 16 }
func (p *PathInfo) targetIdx() uint32  { return p.Target.ModeInfoIdx >> 16 }
func (p *PathInfo) desktopIdx() uint32 { return p.Target.ModeInfoIdx & 0xffff }

func (p *PathInfo) setSourceIdx(i uint32) {
	p.Source.ModeInfoIdx = p.Source.ModeInfoIdx&0xffff | i<<16
}

func (p *PathInfo) setCloneGroup(g uint32) {
	p.Source.ModeInfoIdx = p.Source.ModeInfoIdx&^0xffff | g&0xffff
}

// clearTargetModes lets Windows pick the target (monitor timing) and desktop
// image modes for the source mode and refresh rate (libdisplaydevice
// win_display_device_modes.cpp does the same before a mode change).
func (p *PathInfo) clearTargetModes() { p.Target.ModeInfoIdx = idx16Invalid<<16 | idx16Invalid }

// SourceMode is DISPLAYCONFIG_SOURCE_MODE: the desktop surface of a path.
type SourceMode struct {
	Width, Height uint32
	PixelFormat   uint32
	X, Y          int32 // position in desktop coordinates; the primary display is at (0, 0)
}

// Source decodes the union as a source mode.
func (m *ModeInfo) Source() SourceMode {
	u := m.Union[:]
	return SourceMode{
		Width:       binary.LittleEndian.Uint32(u[0:]),
		Height:      binary.LittleEndian.Uint32(u[4:]),
		PixelFormat: binary.LittleEndian.Uint32(u[8:]),
		X:           int32(binary.LittleEndian.Uint32(u[12:])),
		Y:           int32(binary.LittleEndian.Uint32(u[16:])),
	}
}

// SetSource encodes s into the union.
func (m *ModeInfo) SetSource(s SourceMode) {
	u := m.Union[:]
	binary.LittleEndian.PutUint32(u[0:], s.Width)
	binary.LittleEndian.PutUint32(u[4:], s.Height)
	binary.LittleEndian.PutUint32(u[8:], s.PixelFormat)
	binary.LittleEndian.PutUint32(u[12:], uint32(s.X))
	binary.LittleEndian.PutUint32(u[16:], uint32(s.Y))
}

// TargetActive decodes a target mode's active size and vertical sync
// frequency (DISPLAYCONFIG_VIDEO_SIGNAL_INFO: pixelRate u64, hSyncFreq,
// vSyncFreq, activeSize, totalSize, ...).
func (m *ModeInfo) TargetActive() (w, h uint32, vsync Rational) {
	u := m.Union[:]
	vsync = Rational{binary.LittleEndian.Uint32(u[16:]), binary.LittleEndian.Uint32(u[20:])}
	return binary.LittleEndian.Uint32(u[24:]), binary.LittleEndian.Uint32(u[28:]), vsync
}

// sourceMode returns the source mode of path i, or nil.
func (c *Config) sourceMode(i int) *ModeInfo {
	idx := c.Paths[i].sourceIdx()
	if idx == idx16Invalid || int(idx) >= len(c.Modes) || c.Modes[idx].InfoType != modeTypeSource {
		return nil
	}
	return &c.Modes[idx]
}

// activePath returns the index of the active path driving target t, or -1.
func (c *Config) activePath(t Target) int {
	for i := range c.Paths {
		if c.Paths[i].active() && c.Paths[i].target() == t {
			return i
		}
	}
	return -1
}

// hasTarget reports whether any path (active or not) leads to t with the
// monitor available.
func (c *Config) hasTarget(t Target) bool {
	for i := range c.Paths {
		if c.Paths[i].target() == t && c.Paths[i].Target.TargetAvailable != 0 {
			return true
		}
	}
	return false
}

// findTarget resolves want in now: the path with exactly that adapter and
// target id, else the one target with that id that is not in before (the
// virtual monitor is the one that appeared). The second rule covers an
// adapter LUID that differs between the driver's report and CCD.
func findTarget(before, now Config, want Target) (Target, bool) {
	if now.hasTarget(want) {
		return want, true
	}
	var found []Target
	for i := range now.Paths {
		t := now.Paths[i].target()
		if t.ID != want.ID || now.Paths[i].Target.TargetAvailable == 0 || before.hasTarget(t) {
			continue
		}
		if len(found) == 0 || found[len(found)-1] != t {
			found = append(found, t)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return Target{}, false
}

// activateTarget returns the paths that keep every active display as it is
// and also drive target t (all: a QDC_ALL_PATHS query; active: a
// QDC_ONLY_ACTIVE_PATHS one). Like
// libdisplaydevice's makePathsForNewTopology (win_api_utils.cpp), every path
// is cleared of mode indices and gets its own clone group (extended desktop),
// and t gets the first path in all whose source id is free on its adapter.
// Apply the result with SetDisplayConfig(paths, no modes,
// SDC_APPLY|SDC_USE_SUPPLIED_DISPLAY_CONFIG|SDC_ALLOW_CHANGES|
// SDC_VIRTUAL_MODE_AWARE): Windows picks the modes.
func activateTarget(all, active Config, t Target) ([]PathInfo, error) {
	type src struct {
		adapter LUID
		id      uint32
	}
	used := map[src]bool{}
	var paths []PathInfo
	for i := range active.Paths {
		p := active.Paths[i]
		if !p.active() || p.target() == t {
			continue
		}
		used[src{p.Source.AdapterID, p.Source.ID}] = true
		paths = append(paths, p)
	}
	pick := -1
	for i := range all.Paths {
		p := &all.Paths[i]
		if p.target() == t && p.Target.TargetAvailable != 0 && !used[src{p.Source.AdapterID, p.Source.ID}] {
			pick = i
			break
		}
	}
	if pick < 0 {
		return nil, fmt.Errorf("no free display source for target %v", t)
	}
	paths = append(paths, all.Paths[pick])
	for i := range paths {
		paths[i].setSourceIdx(idx16Invalid)
		paths[i].clearTargetModes()
		paths[i].setCloneGroup(uint32(i))
		paths[i].Flags |= pathActive
	}
	return paths, nil
}

// plan returns c (a QDC_ONLY_ACTIVE_PATHS query in which target t is active)
// changed so that t shows mode m: source mode m.Width x m.Height, refresh
// m.Hz, never rotated, placed by layout:
//
//   - LayoutExtend: to the right of all other displays, top-aligned with the
//     primary; nothing else moves (unless t was at the origin: then the
//     desktop shifts so that the other display nearest to it is primary).
//   - LayoutPrimary: as extend, then the whole desktop shifts so that t is at
//     (0, 0), which makes it the primary display (libdisplaydevice
//     setAsPrimary); the other displays keep their places relative to each
//     other, and none overlaps t.
//   - LayoutOnly: t alone at (0, 0); every other path is left out, which
//     turns those displays off.
//
// Apply it with SetDisplayConfig(SDC_APPLY|SDC_USE_SUPPLIED_DISPLAY_CONFIG|
// SDC_VIRTUAL_MODE_AWARE, plus SDC_ALLOW_CHANGES first and without it as the
// strict retry, as libdisplaydevice's setDisplayModes does).
func plan(c Config, t Target, m Mode, layout string) (Config, error) {
	c = c.Clone()
	vi := c.activePath(t)
	if vi < 0 {
		return Config{}, fmt.Errorf("target %v is not active", t)
	}
	vm := c.sourceMode(vi)
	if vm == nil {
		return Config{}, fmt.Errorf("target %v has no source mode", t)
	}
	for i := range c.Paths {
		if i != vi && c.Paths[i].active() && c.Paths[i].Source.AdapterID == c.Paths[vi].Source.AdapterID && c.Paths[i].Source.ID == c.Paths[vi].Source.ID {
			return Config{}, errCloned
		}
	}
	if layout == LayoutOnly {
		p := c.Paths[vi]
		mode := *vm
		p.setSourceIdx(0)
		p.setCloneGroup(0)
		c = Config{Paths: []PathInfo{p}, Modes: []ModeInfo{mode}}
		vi, vm = 0, &c.Modes[0]
	}
	p := &c.Paths[vi]
	p.Target.Rotation = RotationIdentity // never rotated: a portrait client gets a tall mode instead
	p.Target.RefreshRate = m.rational()
	p.clearTargetModes()
	s := vm.Source()
	s.Width, s.Height = uint32(m.Width), uint32(m.Height)

	// Place t right of everything else, top-aligned with the primary.
	s.X, s.Y = 0, 0
	others := false
	for i := range c.Paths {
		if i == vi || !c.Paths[i].active() {
			continue
		}
		if sm := c.sourceMode(i); sm != nil {
			o := sm.Source()
			if r := o.X + int32(o.Width); !others || r > s.X {
				s.X = r
			}
			others = true
		}
	}
	vm.SetSource(s)
	// The desktop's origin (0, 0) is the primary display: t for primary,
	// else the other display nearest to the origin (it is there already
	// unless Windows restored a saved layout in which t was primary).
	var dx, dy int32
	switch {
	case layout == LayoutPrimary && others:
		dx = s.X
	case layout == LayoutExtend && others:
		best := int64(math.MaxInt64)
		for i := range c.Paths {
			if sm := c.sourceMode(i); i != vi && c.Paths[i].active() && sm != nil {
				o := sm.Source()
				if d := abs64(int64(o.X)) + abs64(int64(o.Y)); d < best {
					best, dx, dy = d, o.X, o.Y
				}
			}
		}
	}
	if dx != 0 || dy != 0 {
		shifted := map[uint32]bool{}
		for i := range c.Paths {
			idx := c.Paths[i].sourceIdx()
			if sm := c.sourceMode(i); c.Paths[i].active() && sm != nil && !shifted[idx] {
				shifted[idx] = true
				o := sm.Source()
				o.X -= dx
				o.Y -= dy
				sm.SetSource(o)
			}
		}
	}
	if err := checkLayout(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

var errCloned = errors.New("the virtual display duplicates another display")

// checkLayout rejects overlapping desktop rectangles and coordinates out of
// range, which SetDisplayConfig would refuse anyway, with a clearer error.
// Paths of one source (a duplicated display) count once: with
// QDC_VIRTUAL_MODE_AWARE each has its own, identical source mode.
func checkLayout(c Config) error {
	type rect struct{ x0, y0, x1, y1 int64 }
	type src struct {
		adapter LUID
		id      uint32
	}
	var rs []rect
	seen := map[src]bool{}
	for i := range c.Paths {
		p := &c.Paths[i]
		sm := c.sourceMode(i)
		if !p.active() || sm == nil || seen[src{p.Source.AdapterID, p.Source.ID}] {
			continue
		}
		seen[src{p.Source.AdapterID, p.Source.ID}] = true
		s := sm.Source()
		r := rect{int64(s.X), int64(s.Y), int64(s.X) + int64(s.Width), int64(s.Y) + int64(s.Height)}
		if r.x0 < -maxSourceCoordinate || r.y0 < -maxSourceCoordinate || r.x1 > maxSourceCoordinate || r.y1 > maxSourceCoordinate {
			return fmt.Errorf("display at (%d,%d) %dx%d is outside the desktop coordinate range", s.X, s.Y, s.Width, s.Height)
		}
		for _, o := range rs {
			if r.x0 < o.x1 && o.x0 < r.x1 && r.y0 < o.y1 && o.y0 < r.y1 {
				return fmt.Errorf("displays overlap at (%d,%d)", s.X, s.Y)
			}
		}
		rs = append(rs, r)
	}
	return nil
}

// errRefresh: everything matches the plan but the refresh rate.
var errRefresh = errors.New("refresh rate differs")

// checkPlan reports how c (queried after applying a plan) differs from what
// the plan asked for target t: size, rotation, position (primary/only) and
// other active displays (only) must match exactly, the refresh rate within
// refreshTolerance (else an error wrapping errRefresh). It returns the
// target's actual refresh rate in Hz.
func checkPlan(c Config, t Target, m Mode, layout string) (float64, error) {
	vi := c.activePath(t)
	if vi < 0 {
		return 0, fmt.Errorf("target %v is no longer active", t)
	}
	vm := c.sourceMode(vi)
	if vm == nil {
		return 0, fmt.Errorf("target %v has no source mode", t)
	}
	s := vm.Source()
	hz := c.Paths[vi].Target.RefreshRate.Hz()
	switch {
	case int(s.Width) != m.Width || int(s.Height) != m.Height:
		return hz, fmt.Errorf("size is %dx%d, not %dx%d", s.Width, s.Height, m.Width, m.Height)
	case c.Paths[vi].Target.Rotation != RotationIdentity:
		return hz, fmt.Errorf("rotation is %d, not identity", c.Paths[vi].Target.Rotation)
	case (layout == LayoutPrimary || layout == LayoutOnly) && (s.X != 0 || s.Y != 0):
		return hz, fmt.Errorf("position is (%d,%d), not primary (0,0)", s.X, s.Y)
	}
	if layout == LayoutOnly {
		for i := range c.Paths {
			if i != vi && c.Paths[i].active() {
				return hz, fmt.Errorf("display %v is still on", c.Paths[i].target())
			}
		}
	}
	if math.Abs(hz-m.hz()) > refreshTolerance {
		return hz, fmt.Errorf("%w: %.3f Hz, not %.3f Hz", errRefresh, hz, m.hz())
	}
	return hz, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
