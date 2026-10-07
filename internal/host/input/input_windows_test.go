//go:build windows

package input

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetCursorPos = user32.NewProc("GetCursorPos")

func cursorPos(t *testing.T) (int32, int32) {
	var p struct{ X, Y int32 }
	if r, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		t.Skipf("GetCursorPos unavailable (no interactive desktop?): %v", err)
	}
	return p.X, p.Y
}

// TestSendInputMovesCursor injects real absolute and relative motion and
// checks the OS cursor position, validating the INPUT layout and the
// virtual-desktop normalisation.
func TestSendInputMovesCursor(t *testing.T) {
	b, _ := NewBackend()
	vw, vh := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
	vx, vy := metric(smXVirtualScreen), metric(smYVirtualScreen)
	if vw < 100 || vh < 100 {
		t.Skip("no desktop")
	}
	target := Rect{X: int(vx), Y: int(vy), W: int(vw), H: int(vh)}
	if err := b.MoveAbs(32767, 32767, target); err != nil {
		t.Fatal(err)
	}
	x, y := cursorPos(t)
	cx, cy := vx+vw/2, vy+vh/2
	if abs(x-cx) > 2 || abs(y-cy) > 2 {
		t.Fatalf("absolute move landed at (%d,%d), want ~(%d,%d)", x, y, cx, cy)
	}
	if err := b.MoveRel(25, -15); err != nil {
		t.Fatal(err)
	}
	x2, y2 := cursorPos(t)
	if x2-x < 20 || y-y2 < 10 {
		t.Fatalf("relative move (+25,-15) moved by (%d,%d)", x2-x, y2-y)
	}
	if err := b.MoveAbs(0, 0, target); err != nil {
		t.Fatal(err)
	}
	if x, y := cursorPos(t); abs(x-vx) > 1 || abs(y-vy) > 1 {
		t.Fatalf("corner move landed at (%d,%d)", x, y)
	}
	for _, k := range []struct {
		sc  uint16
		ext bool
	}{{0x1e, false}, {0x48, true}, {0x45, false}, {0x45, true}, {0x37, true}} {
		if err := b.Key(k.sc, k.ext, true); err != nil {
			t.Fatalf("key %x down: %v", k.sc, err)
		}
		if err := b.Key(k.sc, k.ext, false); err != nil {
			t.Fatalf("key %x up: %v", k.sc, err)
		}
	}
	if err := b.Wheel(120, -120); err != nil {
		t.Fatal(err)
	}
	if err := b.Text("héllo ✓"); err != nil {
		t.Fatal(err)
	}
	_ = windows.GetCurrentThreadId()
}

func abs(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
