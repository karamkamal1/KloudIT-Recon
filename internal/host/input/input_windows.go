//go:build windows

package input

import (
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004
	keyeventfScancode    = 0x0008

	mouseeventfMove        = 0x0001
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfXDown       = 0x0080
	mouseeventfXUp         = 0x0100
	mouseeventfWheel       = 0x0800
	mouseeventfHWheel      = 0x1000
	mouseeventfMoveNoCoal  = 0x2000
	mouseeventfVirtualDesk = 0x4000
	mouseeventfAbsolute    = 0x8000

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	vkPause    = 0x13
	vkNumLock  = 0x90
	vkSnapshot = 0x2C
)

// INPUT structures for 64-bit Windows (40 bytes each).
type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extra     uintptr
}

type keybdInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	_     uint32
	extra uintptr
	_     [8]byte
}

func init() {
	if unsafe.Sizeof(mouseInput{}) != 40 || unsafe.Sizeof(keybdInput{}) != 40 {
		panic("input: unexpected INPUT struct size (only 64-bit Windows is supported)")
	}
}

// extraInfo tags our injected events (visible to hooks as dwExtraInfo).
const extraInfo = 0x5245434F // "RECO"

func sendMouse(in ...mouseInput) error {
	for i := range in {
		in[i].typ = inputMouse
		in[i].extra = extraInfo
	}
	n, _, err := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	if int(n) != len(in) {
		return fmt.Errorf("SendInput(mouse): %v", err)
	}
	return nil
}

func sendKeys(in ...keybdInput) error {
	for i := range in {
		in[i].typ = inputKeyboard
		in[i].extra = extraInfo
	}
	n, _, err := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	if int(n) != len(in) {
		return fmt.Errorf("SendInput(keyboard): %v", err)
	}
	return nil
}

func metric(i int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r)
}

type winBackend struct{}

// NewBackend returns the SendInput backend.
func NewBackend() (Backend, error) { return winBackend{}, nil }

func (winBackend) Key(sc uint16, ext, down bool) error {
	var k keybdInput
	// Pause, NumLock and PrintScreen cannot be expressed as a single set-1
	// scancode through SendInput; use virtual keys for them.
	switch {
	case sc == 0x45 && !ext:
		k.vk = vkPause
	case sc == 0x45 && ext:
		k.vk = vkNumLock
		k.flags = keyeventfExtendedKey
	case sc == 0x37 && ext:
		k.vk = vkSnapshot
		k.flags = keyeventfExtendedKey
	default:
		k.scan = sc
		k.flags = keyeventfScancode
		if ext {
			k.flags |= keyeventfExtendedKey
		}
	}
	if !down {
		k.flags |= keyeventfKeyUp
	}
	return sendKeys(k)
}

func (winBackend) Button(b uint8, down bool) error {
	var m mouseInput
	switch b {
	case 0:
		m.flags = pick(down, mouseeventfLeftDown, mouseeventfLeftUp)
	case 1:
		m.flags = pick(down, mouseeventfMiddleDown, mouseeventfMiddleUp)
	case 2:
		m.flags = pick(down, mouseeventfRightDown, mouseeventfRightUp)
	case 3, 4:
		m.flags = pick(down, mouseeventfXDown, mouseeventfXUp)
		m.mouseData = uint32(b - 2) // XBUTTON1 = 1, XBUTTON2 = 2
	default:
		return nil
	}
	return sendMouse(m)
}

func pick(c bool, a, b uint32) uint32 {
	if c {
		return a
	}
	return b
}

func (winBackend) MoveRel(dx, dy int32) error {
	return sendMouse(mouseInput{dx: dx, dy: dy, flags: mouseeventfMove | mouseeventfMoveNoCoal})
}

func (winBackend) MoveAbs(x, y uint16, t Rect) error {
	vx, vy := metric(smXVirtualScreen), metric(smYVirtualScreen)
	vw, vh := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
	if vw <= 1 || vh <= 1 {
		return nil
	}
	if t.W <= 0 || t.H <= 0 {
		t = Rect{int(vx), int(vy), int(vw), int(vh)}
	}
	px := int64(t.X) + int64(x)*int64(t.W-1)/65535
	py := int64(t.Y) + int64(y)*int64(t.H-1)/65535
	ax := (px - int64(vx)) * 65535 / int64(vw-1)
	ay := (py - int64(vy)) * 65535 / int64(vh-1)
	return sendMouse(mouseInput{dx: int32(ax), dy: int32(ay), flags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDesk})
}

func (winBackend) Wheel(dy, dx int16) error {
	var in []mouseInput
	if dy != 0 {
		in = append(in, mouseInput{mouseData: uint32(int32(dy)), flags: mouseeventfWheel})
	}
	if dx != 0 {
		in = append(in, mouseInput{mouseData: uint32(int32(dx)), flags: mouseeventfHWheel})
	}
	if len(in) == 0 {
		return nil
	}
	return sendMouse(in...)
}

func (winBackend) Text(s string) error {
	var in []keybdInput
	for _, u := range utf16.Encode([]rune(s)) {
		in = append(in,
			keybdInput{scan: u, flags: keyeventfUnicode},
			keybdInput{scan: u, flags: keyeventfUnicode | keyeventfKeyUp})
	}
	for len(in) > 0 { // keep batches small so other input can interleave
		n := min(len(in), 64)
		if err := sendKeys(in[:n]...); err != nil {
			return err
		}
		in = in[n:]
	}
	return nil
}

func (winBackend) Close() {}
