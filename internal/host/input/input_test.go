package input

import "testing"

func TestRelTrackerLossAndReorder(t *testing.T) {
	var r RelTracker
	var sx, sy int32
	apply := func(seq uint32, cx, cy int32) {
		dx, dy := r.Update(seq, cx, cy)
		sx += dx
		sy += dy
	}
	apply(1, 3, 4)
	// seq 2 lost; seq 3 carries the running total
	apply(3, 10, -2)
	apply(2, 6, 1) // late arrival must be ignored
	apply(3, 10, -2)
	apply(4, 11, -2)
	if sx != 11 || sy != -2 {
		t.Fatalf("applied (%d,%d), want (11,-2)", sx, sy)
	}
	// sequence wrap-around
	var w RelTracker
	w.Update(0xfffffffe, 0, 0)
	if dx, _ := w.Update(1, 5, 0); dx != 5 {
		t.Fatalf("wrap: dx=%d", dx)
	}
}

type recBackend struct{ keys, buttons map[uint32]bool }

func (b *recBackend) Key(sc uint16, ext, down bool) error {
	k := uint32(sc)
	if ext {
		k |= 1 << 16
	}
	b.keys[k] = down
	return nil
}
func (b *recBackend) Button(btn uint8, down bool) error { b.buttons[uint32(btn)] = down; return nil }
func (b *recBackend) MoveRel(dx, dy int32) error        { return nil }
func (b *recBackend) MoveAbs(x, y uint16, t Rect) error { return nil }
func (b *recBackend) Wheel(dy, dx int16) error          { return nil }
func (b *recBackend) Text(s string) error               { return nil }
func (b *recBackend) Close()                            {}

func TestReleaseAll(t *testing.T) {
	b := &recBackend{keys: map[uint32]bool{}, buttons: map[uint32]bool{}}
	in := NewInjector(b)
	in.Key(0x1d, false, true)
	in.Key(0x48, true, true)
	in.Button(0, true)
	in.ReleaseAll()
	for k, down := range b.keys {
		if down {
			t.Fatalf("key %x still down", k)
		}
	}
	if b.buttons[0] {
		t.Fatal("button still down")
	}
}
