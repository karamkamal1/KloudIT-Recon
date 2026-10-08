package vdisplay

import (
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// TestCCDLayout pins the Go mirrors of the CCD structs to wingdi.h's x64
// layout, which the Windows code passes to the API as they are.
func TestCCDLayout(t *testing.T) {
	var p PathInfo
	var m ModeInfo
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof DISPLAYCONFIG_PATH_INFO", unsafe.Sizeof(p), 72},
		{"sizeof DISPLAYCONFIG_PATH_SOURCE_INFO", unsafe.Sizeof(p.Source), 20},
		{"sizeof DISPLAYCONFIG_PATH_TARGET_INFO", unsafe.Sizeof(p.Target), 48},
		{"targetInfo", unsafe.Offsetof(p.Target), 20},
		{"flags", unsafe.Offsetof(p.Flags), 68},
		{"targetInfo.rotation", unsafe.Offsetof(p.Target.Rotation), 20},
		{"targetInfo.refreshRate", unsafe.Offsetof(p.Target.RefreshRate), 28},
		{"targetInfo.targetAvailable", unsafe.Offsetof(p.Target.TargetAvailable), 40},
		{"sizeof DISPLAYCONFIG_MODE_INFO", unsafe.Sizeof(m), 64},
		{"alignof DISPLAYCONFIG_MODE_INFO", unsafe.Alignof(m), 8},
		{"mode union", unsafe.Offsetof(m.Union), 16},
		{"mode adapterId", unsafe.Offsetof(m.AdapterID), 8},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	s := SourceMode{Width: 2560, Height: 1440, PixelFormat: 4, X: -1920, Y: 7}
	m.SetSource(s)
	if m.Source() != s || m.Union[12] != 0x80 || m.Union[15] != 0xff { // x = -1920 = 0xfffff880 LE
		t.Fatalf("source mode %+v %x", m.Source(), m.Union[:20])
	}
	l := LUID{Low: 0xc3a1, High: 2}
	if LUIDFrom(l.Uint64()) != l || l.String() != "00000002:0000c3a1" {
		t.Fatal(l)
	}
}

// desk builds an active configuration: one path per rectangle, on adapter
// gpuLUID, target ids 1.., source ids 0.., 60 Hz.
func desk(rects ...[4]int) Config {
	var c Config
	for i, r := range rects {
		var sm, tm ModeInfo
		sm.InfoType, sm.ID, sm.AdapterID = modeTypeSource, uint32(i), gpuLUID
		sm.SetSource(SourceMode{Width: uint32(r[2]), Height: uint32(r[3]), X: int32(r[0]), Y: int32(r[1])})
		tm.InfoType, tm.ID, tm.AdapterID = modeTypeTarget, uint32(i+1), gpuLUID
		c.Modes = append(c.Modes, sm, tm)
		c.Paths = append(c.Paths, PathInfo{
			Source: PathSourceInfo{AdapterID: gpuLUID, ID: uint32(i), ModeInfoIdx: uint32(2*i)<<16 | idx16Invalid},
			Target: PathTargetInfo{AdapterID: gpuLUID, ID: uint32(i + 1), ModeInfoIdx: uint32(2*i+1)<<16 | idx16Invalid,
				Rotation: 2, RefreshRate: Rational{60000, 1000}, TargetAvailable: 1},
			Flags: pathActive | pathSupportVirtualMode,
		})
	}
	return c
}

func rectOf(t *testing.T, c Config, i int) [4]int {
	t.Helper()
	sm := c.sourceMode(i)
	if sm == nil {
		t.Fatalf("path %d has no source mode", i)
	}
	s := sm.Source()
	return [4]int{int(s.X), int(s.Y), int(s.Width), int(s.Height)}
}

func TestPlanLayouts(t *testing.T) {
	// A monitor left of the primary, the primary, and the new display (target
	// 3, Windows put it below the primary at 1024x768).
	base := desk([4]int{-1920, 100, 1920, 1080}, [4]int{0, 0, 2560, 1440}, [4]int{0, 1440, 1024, 768})
	virt := Target{gpuLUID, 3}
	want := mode1440

	got, err := plan(base, virt, want, LayoutExtend)
	if err != nil {
		t.Fatal(err)
	}
	if r := rectOf(t, got, 2); r != [4]int{2560, 0, 2560, 1440} {
		t.Fatalf("extend: virtual %v", r)
	}
	if r := rectOf(t, got, 0); r != [4]int{-1920, 100, 1920, 1080} {
		t.Fatalf("extend moved another display: %v", r)
	}
	p := got.Paths[2]
	if p.Target.Rotation != RotationIdentity || p.Target.RefreshRate != (Rational{120000, 1000}) || p.targetIdx() != idx16Invalid || p.desktopIdx() != idx16Invalid {
		t.Fatalf("extend: path %+v", p.Target)
	}
	if base.Paths[2].Target.Rotation != 2 || rectOf(t, base, 2)[2] != 1024 {
		t.Fatal("plan changed its input")
	}

	got, err = plan(base, virt, want, LayoutPrimary)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range [][4]int{{-4480, 100, 1920, 1080}, {-2560, 0, 2560, 1440}, {0, 0, 2560, 1440}} {
		if r := rectOf(t, got, i); r != w {
			t.Fatalf("primary: display %d at %v, want %v", i, r, w)
		}
	}
	if _, err := checkPlan(got, virt, want, LayoutPrimary); err != nil {
		t.Fatal(err)
	}

	got, err = plan(base, virt, want, LayoutOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || len(got.Modes) != 1 || got.Paths[0].target() != virt || rectOf(t, got, 0) != [4]int{0, 0, 2560, 1440} {
		t.Fatalf("only: %+v", got)
	}
	if _, err := checkPlan(got, virt, want, LayoutOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPlan(base, virt, want, LayoutOnly); err == nil {
		t.Fatal("checkPlan accepted the unchanged configuration")
	}

	// Windows restored a saved layout with t primary: extend makes the
	// display nearest to the origin primary again.
	saved := desk([4]int{-1920, 0, 1920, 1080}, [4]int{0, 0, 2560, 1440})
	got, err = plan(saved, Target{gpuLUID, 2}, want, LayoutExtend)
	if err != nil || rectOf(t, got, 0) != [4]int{0, 0, 1920, 1080} || rectOf(t, got, 1) != [4]int{1920, 0, 2560, 1440} {
		t.Fatalf("extend from a saved primary: %v %v %v", rectOf(t, got, 0), rectOf(t, got, 1), err)
	}

	// The virtual display alone (a headless host).
	solo := desk([4]int{0, 0, 800, 600})
	got, err = plan(solo, Target{gpuLUID, 1}, want, LayoutPrimary)
	if err != nil || rectOf(t, got, 0) != [4]int{0, 0, 2560, 1440} {
		t.Fatalf("solo: %v %v", rectOf(t, got, 0), err)
	}

	if _, err := plan(base, Target{gpuLUID, 9}, want, LayoutPrimary); err == nil {
		t.Fatal("plan for an inactive target")
	}
	cloned := base.Clone()
	cloned.Paths[2].Source = cloned.Paths[1].Source
	if _, err := plan(cloned, virt, want, LayoutPrimary); !errors.Is(err, errCloned) {
		t.Fatalf("cloned: %v", err)
	}
}

func TestCheckPlan(t *testing.T) {
	virt := Target{gpuLUID, 1}
	c := desk([4]int{0, 0, 2560, 1440})
	c.Paths[0].Target.Rotation = RotationIdentity
	c.Paths[0].Target.RefreshRate = Rational{119880, 1000}
	if hz, err := checkPlan(c, virt, mode1440, LayoutPrimary); err != nil || hz != 119.88 {
		t.Fatalf("119.88 Hz within tolerance: %v %v", hz, err)
	}
	c.Paths[0].Target.RefreshRate = Rational{60, 1}
	if _, err := checkPlan(c, virt, mode1440, LayoutPrimary); !errors.Is(err, errRefresh) {
		t.Fatalf("refresh: %v", err)
	}
	for _, mut := range []func(*Config){
		func(c *Config) { c.Paths[0].Target.Rotation = 4 },
		func(c *Config) { c.Modes[0].SetSource(SourceMode{Width: 1440, Height: 2560}) },
		func(c *Config) { c.Modes[0].SetSource(SourceMode{Width: 2560, Height: 1440, X: 1920}) },
		func(c *Config) { c.Paths[0].Flags = 0 },
	} {
		d := c.Clone()
		d.Paths[0].Target.RefreshRate = Rational{120, 1}
		mut(&d)
		if _, err := checkPlan(d, virt, mode1440, LayoutPrimary); err == nil || errors.Is(err, errRefresh) {
			t.Errorf("mismatch not reported: %v", err)
		}
	}
}

func TestCheckLayout(t *testing.T) {
	if err := checkLayout(desk([4]int{0, 0, 1920, 1080}, [4]int{1920, 0, 1920, 1080})); err != nil {
		t.Fatal(err)
	}
	if err := checkLayout(desk([4]int{0, 0, 1920, 1080}, [4]int{1919, 0, 1920, 1080})); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlap: %v", err)
	}
	if err := checkLayout(desk([4]int{0, 0, 1920, 1080}, [4]int{32000, 0, 7680, 4320})); err == nil {
		t.Fatal("out of range accepted")
	}
	// Two monitors duplicating one source (each with its own source mode, as
	// QDC_VIRTUAL_MODE_AWARE reports them) are one rectangle.
	clone := desk([4]int{0, 0, 1920, 1080}, [4]int{0, 0, 1920, 1080}, [4]int{1920, 0, 2560, 1440})
	clone.Paths[1].Source.ID = clone.Paths[0].Source.ID
	if err := checkLayout(clone); err != nil {
		t.Fatalf("clone group: %v", err)
	}
	clone.Paths[2].Target.AdapterID = virtLUID
	got, err := plan(clone, Target{virtLUID, 3}, mode1440, LayoutPrimary)
	if err != nil || rectOf(t, got, 0) != [4]int{-1920, 0, 1920, 1080} || rectOf(t, got, 1) != [4]int{-1920, 0, 1920, 1080} {
		t.Fatalf("primary next to a clone group: %v", err)
	}
}

func TestActivateTarget(t *testing.T) {
	active := desk([4]int{0, 0, 1920, 1080})
	all := active.Clone()
	virt := Target{virtLUID, 7}
	for sid := uint32(0); sid < 3; sid++ {
		all.Paths = append(all.Paths, PathInfo{
			Source: PathSourceInfo{AdapterID: virtLUID, ID: sid, ModeInfoIdx: 0xffffffff},
			Target: PathTargetInfo{AdapterID: virtLUID, ID: 7, ModeInfoIdx: 0xffffffff, TargetAvailable: 1},
		})
	}
	paths, err := activateTarget(all, active, virt)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[1].target() != virt || paths[1].Source.ID != 0 || paths[0].target() != (Target{gpuLUID, 1}) {
		t.Fatalf("paths %+v", paths)
	}
	for i, p := range paths {
		if !p.active() || p.sourceIdx() != idx16Invalid || p.targetIdx() != idx16Invalid || p.desktopIdx() != idx16Invalid || p.Source.ModeInfoIdx&0xffff != uint32(i) {
			t.Fatalf("path %d: %+v", i, p)
		}
	}
	// The virtual target duplicating source 0 of its adapter gets source 1.
	active.Paths = append(active.Paths, PathInfo{Source: PathSourceInfo{AdapterID: virtLUID, ID: 0}, Target: PathTargetInfo{AdapterID: gpuLUID, ID: 9}, Flags: pathActive})
	if paths, err = activateTarget(all, active, virt); err != nil || paths[2].Source.ID != 1 {
		t.Fatalf("free source: %+v %v", paths, err)
	}
	if _, err := activateTarget(all, active, Target{virtLUID, 8}); err == nil {
		t.Fatal("activated a target that is not connected")
	}
}

func TestFindTarget(t *testing.T) {
	before := desk([4]int{0, 0, 1920, 1080})
	now := desk([4]int{0, 0, 1920, 1080}, [4]int{1920, 0, 2560, 1440})
	now.Paths[1].Target.AdapterID = virtLUID
	if got, ok := findTarget(before, now, Target{virtLUID, 2}); !ok || got != (Target{virtLUID, 2}) {
		t.Fatalf("exact: %v %v", got, ok)
	}
	if got, ok := findTarget(before, now, Target{LUID{Low: 1}, 2}); !ok || got != (Target{virtLUID, 2}) {
		t.Fatalf("by new target id: %v %v", got, ok)
	}
	if _, ok := findTarget(before, now, Target{LUID{Low: 1}, 1}); ok {
		t.Fatal("matched a monitor that was there before")
	}
	if _, ok := findTarget(before, before, Target{virtLUID, 2}); ok {
		t.Fatal("found a monitor that has not appeared")
	}
}
