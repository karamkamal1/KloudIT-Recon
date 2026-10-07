// Package input injects keyboard/mouse events on the host.
package input

import (
	"sync"
)

// Rect is a monitor rectangle in virtual-desktop pixel coordinates.
type Rect struct{ X, Y, W, H int }

// Backend performs the raw OS injection.
type Backend interface {
	Key(sc uint16, ext, down bool) error
	Button(b uint8, down bool) error
	MoveRel(dx, dy int32) error
	MoveAbs(x, y uint16, target Rect) error // x,y normalised 0..65535 within target
	Wheel(dy, dx int16) error
	Text(s string) error
	Close()
}

// Injector wraps a Backend, tracks pressed keys/buttons so they can always be
// released (a dropped connection must never leave a key stuck down), and maps
// absolute coordinates onto the captured monitor.
type Injector struct {
	b Backend

	mu      sync.Mutex
	keys    map[uint32]bool // sc | ext<<16
	buttons map[uint8]bool
	target  Rect
}

func NewInjector(b Backend) *Injector {
	return &Injector{b: b, keys: map[uint32]bool{}, buttons: map[uint8]bool{}}
}

func (in *Injector) SetTarget(r Rect) {
	in.mu.Lock()
	in.target = r
	in.mu.Unlock()
}

func (in *Injector) Key(sc uint16, ext, down bool) error {
	k := uint32(sc)
	if ext {
		k |= 1 << 16
	}
	in.mu.Lock()
	if down {
		in.keys[k] = true
	} else {
		delete(in.keys, k)
	}
	in.mu.Unlock()
	return in.b.Key(sc, ext, down)
}

func (in *Injector) Button(b uint8, down bool) error {
	in.mu.Lock()
	if down {
		in.buttons[b] = true
	} else {
		delete(in.buttons, b)
	}
	in.mu.Unlock()
	return in.b.Button(b, down)
}

func (in *Injector) MoveRel(dx, dy int32) error {
	if dx == 0 && dy == 0 {
		return nil
	}
	return in.b.MoveRel(dx, dy)
}

func (in *Injector) MoveAbs(x, y uint16) error {
	in.mu.Lock()
	t := in.target
	in.mu.Unlock()
	return in.b.MoveAbs(x, y, t)
}

func (in *Injector) Wheel(dy, dx int16) error { return in.b.Wheel(dy, dx) }

func (in *Injector) Text(s string) error {
	if len(s) > 4096 {
		s = s[:4096]
	}
	return in.b.Text(s)
}

// ReleaseAll lifts every key and button that is currently held.
func (in *Injector) ReleaseAll() {
	in.mu.Lock()
	keys := in.keys
	buttons := in.buttons
	in.keys = map[uint32]bool{}
	in.buttons = map[uint8]bool{}
	in.mu.Unlock()
	for k := range keys {
		_ = in.b.Key(uint16(k&0xffff), k>>16 != 0, false)
	}
	for b := range buttons {
		_ = in.b.Button(b, false)
	}
}

func (in *Injector) Close() {
	in.ReleaseAll()
	in.b.Close()
}

// RelTracker turns cumulative relative-motion datagrams into deltas. Datagrams
// carry running totals, so a lost datagram only delays motion; stale or
// duplicated datagrams are ignored.
type RelTracker struct {
	init       bool
	seq        uint32
	cumX, cumY int32
}

// Update returns the delta to apply for a datagram (0,0 if stale).
func (t *RelTracker) Update(seq uint32, cx, cy int32) (int32, int32) {
	if !t.init {
		t.init = true
		t.seq, t.cumX, t.cumY = seq, cx, cy
		return cx, cy // totals start at zero on the client
	}
	if int32(seq-t.seq) <= 0 {
		return 0, 0
	}
	dx, dy := cx-t.cumX, cy-t.cumY
	t.seq, t.cumX, t.cumY = seq, cx, cy
	return dx, dy
}

// SeqGate drops out-of-order datagrams for latest-wins state (absolute mouse, gamepad).
type SeqGate struct {
	init bool
	seq  uint32
}

func (g *SeqGate) Accept(seq uint32) bool {
	if g.init && int32(seq-g.seq) <= 0 {
		return false
	}
	g.init, g.seq = true, seq
	return true
}
