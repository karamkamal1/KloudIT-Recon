//go:build windows

package platform

import (
	"testing"
	"unsafe"
)

// TestPrimaryAdapter reads adapter 0's DXGI_ADAPTER_DESC1 (layout as in
// dxgi.h on x64: VendorId at 256, AdapterLuid at 296, 312 bytes).
func TestPrimaryAdapter(t *testing.T) {
	var d dxgiAdapterDesc1
	if unsafe.Offsetof(d.vendorID) != 256 || unsafe.Offsetof(d.luid) != 296 || unsafe.Sizeof(d) != 312 {
		t.Fatalf("DXGI_ADAPTER_DESC1 layout: VendorId at %d, AdapterLuid at %d, size %d", unsafe.Offsetof(d.vendorID), unsafe.Offsetof(d.luid), unsafe.Sizeof(d))
	}
	a, err := PrimaryAdapter()
	if err != nil {
		t.Skipf("no DXGI adapter: %v", err)
	}
	t.Logf("adapter 0: %q vendor %s (%#x) luid %#x", a.Name, a.Vendor, a.VendorID, a.LUID)
	if a.Vendor != adapterVendor(a.VendorID) || a.Name == "" {
		t.Fatalf("bad adapter %+v", a)
	}
	for id, want := range map[uint32]string{0x10DE: "nvidia", 0x1002: "amd", 0x8086: "intel", 0x1414: "other"} {
		if got := adapterVendor(id); got != want {
			t.Errorf("vendor %#x: %s, want %s", id, got, want)
		}
	}
}

func TestMonitorsAndCursor(t *testing.T) {
	EnableDPIAwareness()
	mons, err := Monitors()
	if err != nil || len(mons) == 0 {
		t.Fatalf("monitors: %v %v", mons, err)
	}
	for _, m := range mons {
		t.Logf("monitor %d %q %dx%d@%d at (%d,%d) primary=%v hmon=%#x dxgi=%d rotated=%v", m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.HMonitor, m.DXGIOutput, m.Rotated)
		if m.W <= 0 || m.H <= 0 || m.HMonitor == 0 {
			t.Fatal("bad monitor")
		}
	}
	if !mons[0].Primary {
		t.Fatal("primary monitor not first")
	}
	cs, err := GetCursor()
	if err != nil {
		t.Skipf("GetCursorInfo: %v", err)
	}
	t.Logf("cursor visible=%v at (%d,%d) handle=%#x", cs.Visible, cs.X, cs.Y, cs.Handle)
	if cs.Handle == 0 {
		t.Skip("no cursor handle")
	}
	shape, err := CursorImage(cs.Handle)
	if err != nil {
		t.Fatal(err)
	}
	opaque := 0
	for i := 3; i < len(shape.RGBA); i += 4 {
		if shape.RGBA[i] > 0 {
			opaque++
		}
	}
	t.Logf("cursor image %dx%d hot=(%d,%d) opaque pixels=%d", shape.W, shape.H, shape.HotX, shape.HotY, opaque)
	if shape.W == 0 || len(shape.RGBA) != shape.W*shape.H*4 || opaque == 0 {
		t.Fatal("empty cursor image")
	}
}

// TestStockCursorImages exercises the GDI conversion on the system arrow and
// I-beam cursors (colour and monochrome-mask formats).
func TestStockCursorImages(t *testing.T) {
	load := user32.NewProc("LoadCursorW")
	for _, id := range []uintptr{32512 /*IDC_ARROW*/, 32513 /*IDC_IBEAM*/, 32515 /*IDC_CROSS*/, 32649 /*IDC_HAND*/} {
		h, _, err := load.Call(0, id)
		if h == 0 {
			t.Fatalf("LoadCursor(%d): %v", id, err)
		}
		shape, err := CursorImage(uint64(h))
		if err != nil {
			t.Fatalf("cursor %d: %v", id, err)
		}
		opaque := 0
		for i := 3; i < len(shape.RGBA); i += 4 {
			if shape.RGBA[i] > 0 {
				opaque++
			}
		}
		t.Logf("cursor %d: %dx%d hot=(%d,%d) opaque=%d", id, shape.W, shape.H, shape.HotX, shape.HotY, opaque)
		if shape.W == 0 || shape.H == 0 || opaque == 0 || opaque == shape.W*shape.H {
			t.Fatalf("cursor %d: implausible image", id)
		}
	}
}
