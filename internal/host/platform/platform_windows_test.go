//go:build windows

package platform

import "testing"

func TestMonitorsAndCursor(t *testing.T) {
	EnableDPIAwareness()
	mons, err := Monitors()
	if err != nil || len(mons) == 0 {
		t.Fatalf("monitors: %v %v", mons, err)
	}
	for _, m := range mons {
		t.Logf("monitor %d %q %dx%d@%d at (%d,%d) primary=%v hmon=%#x dxgi=%d", m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.HMonitor, m.DXGIOutput)
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
