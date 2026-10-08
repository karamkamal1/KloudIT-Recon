//go:build windows

package vdisplay

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// TestDeviceInfoLayout pins the DisplayConfigGetDeviceInfo structs to wingdi.h.
func TestDeviceInfoLayout(t *testing.T) {
	if s := unsafe.Sizeof(deviceInfoHeader{}); s != 20 {
		t.Errorf("DISPLAYCONFIG_DEVICE_INFO_HEADER: %d bytes", s)
	}
	if s := unsafe.Sizeof(sourceDeviceName{}); s != 84 {
		t.Errorf("DISPLAYCONFIG_SOURCE_DEVICE_NAME: %d bytes", s)
	}
	if s := unsafe.Sizeof(adapterName{}); s != 276 {
		t.Errorf("DISPLAYCONFIG_ADAPTER_NAME: %d bytes", s)
	}
}

// TestQueryDisplayConfig reads the real display configuration and checks it
// against the GDI monitor list (runs on Windows and under Wine with a display).
// Wine 9 rejects QDC_VIRTUAL_MODE_AWARE: there the test queries the classic
// layout, whose mode indices are whole 32-bit values, so the struct layouts
// and the device-info calls are still checked against real data.
func TestQueryDisplayConfig(t *testing.T) {
	platform.EnableDPIAwareness()
	var sys winSystem
	query, classic := sys.Query, false
	c, err := query(QDCOnlyActivePaths)
	if err != nil {
		t.Logf("virtual-mode-aware QueryDisplayConfig: %v; trying the classic layout", err)
		query, classic = queryDisplayConfig, true
		if c, err = query(QDCOnlyActivePaths); err != nil {
			t.Skipf("QueryDisplayConfig: %v", err)
		}
		// Classic indices: move them into the virtual-mode-aware halves.
		for i := range c.Paths {
			p := &c.Paths[i]
			p.Source.ModeInfoIdx = p.Source.ModeInfoIdx<<16 | idx16Invalid
			p.Target.ModeInfoIdx = p.Target.ModeInfoIdx<<16 | idx16Invalid
		}
	}
	all, err := query(QDCAllPaths)
	if err != nil || len(all.Paths) < len(c.Paths) {
		t.Fatalf("all paths: %d, %v", len(all.Paths), err)
	}
	t.Logf("classic layout: %v", classic)
	mons, _ := platform.Monitors()
	t.Logf("%d active paths, %d modes, %d paths in all; %d GDI monitors", len(c.Paths), len(c.Modes), len(all.Paths), len(mons))
	for i := range c.Paths {
		p := &c.Paths[i]
		name, err := sys.SourceName(p.Source.AdapterID, p.Source.ID)
		inst, ierr := sys.AdapterInstance(p.Target.AdapterID)
		sm := c.sourceMode(i)
		t.Logf("path %d: source %v/%d %q target %v rotation %d refresh %.3f flags %#x adapter %q (%v) source mode %+v",
			i, p.Source.AdapterID, p.Source.ID, name, p.target(), p.Target.Rotation, p.Target.RefreshRate.Hz(), p.Flags, inst, ierr, sm)
		if ti := int(p.targetIdx()); ti < len(c.Modes) && c.Modes[ti].InfoType == modeTypeTarget {
			w, h, vs := c.Modes[ti].TargetActive()
			t.Logf("path %d: target mode %dx%d at %.3f Hz", i, w, h, vs.Hz())
		}
		if err != nil || !p.active() || sm == nil {
			t.Fatalf("path %d: name %v, active %v, source mode %v", i, err, p.active(), sm)
		}
		s := sm.Source()
		m, ok := sys.Monitor(name)
		if !ok || m.W != int(s.Width) || m.H != int(s.Height) || m.X != int(s.X) || m.Y != int(s.Y) {
			t.Fatalf("path %d (%s) %+v does not match the GDI monitor %+v", i, name, s, m)
		}
		if !strings.HasPrefix(name, `\\.\DISPLAY`) {
			t.Fatalf("GDI name %q", name)
		}
	}
	if len(c.Paths) > 0 {
		if err := checkLayout(c); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDetectWithoutDriver: without SudoVDA or VDD installed Detect reports
// ErrNoDriver (an installed one is logged).
func TestDetectWithoutDriver(t *testing.T) {
	st := New(Options{Policy: PolicyAuto}).Detect()
	t.Logf("virtual display: %v", st)
	if st.Driver == "" && st.Err == nil {
		t.Fatal("no driver and no error")
	}
}
