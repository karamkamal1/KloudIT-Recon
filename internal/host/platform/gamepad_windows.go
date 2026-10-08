//go:build windows

package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// Virtual Xbox 360 controllers via the ViGEmBus driver, spoken to directly over
// its IOCTL interface (no ViGEmClient.dll needed). Requires ViGEmBus to be
// installed: https://github.com/nefarius/ViGEmBus/releases
//
// Force feedback (step 4.6): a game's XInputSetState reaches the virtual
// controller, and ViGEmBus completes a pending IOCTL_XUSB_REQUEST_NOTIFICATION
// with the motor speeds. Each plugged pad has a listener that keeps one such
// request pending and passes every completion to the onRumble callback. A
// pending request on a handle opened for synchronous I/O would hold up every
// other request on it (the I/O manager serialises them), so the bus handle is
// opened for overlapped I/O, as ViGEmClient does, and every IOCTL goes through
// an overlappedIO.

var guidViGEmBus = windows.GUID{Data1: 0x96E42B22, Data2: 0xF5E9, Data3: 0x42F8, Data4: [8]byte{0xB0, 0x43, 0xED, 0x0F, 0x93, 0x2F, 0x01, 0x4F}}

// IOCTL codes: CTL_CODE(FILE_DEVICE_BUS_EXTENDER, IOCTL_VIGEM_BASE + n,
// METHOD_BUFFERED, access) with IOCTL_VIGEM_BASE 0x801 (ViGEmClient
// include/ViGEm/km/BusShared.h; vigemIoctl in the test spells the formula).
const (
	ioctlPluginTarget            = 0x2AA004 // +0x000, FILE_WRITE_DATA
	ioctlUnplugTarget            = 0x2AA008 // +0x001, FILE_WRITE_DATA
	ioctlCheckVersion            = 0x2AA00C // +0x002, FILE_WRITE_DATA
	ioctlWaitDeviceReady         = 0x2AA010 // +0x003, FILE_WRITE_DATA
	ioctlXusbRequestNotification = 0x2AE804 // +0x200, FILE_READ_DATA | FILE_WRITE_DATA
	ioctlXusbSubmit              = 0x2AA808 // +0x201, FILE_WRITE_DATA
	vigemCommonVersion           = 0x0001
	maxPads                      = 4

	// sizeof(XUSB_REQUEST_NOTIFICATION): ULONG Size, ULONG SerialNo, UCHAR
	// LargeMotor, SmallMotor, LedNumber, padded to the ULONG alignment.
	xusbNotificationSize = 12
)

// Gamepads manages up to four virtual controllers.
type Gamepads struct {
	mu       sync.Mutex
	h        windows.Handle // overlapped I/O
	io       *overlappedIO  // the IOCTLs made under mu
	serials  [maxPads]uint32
	notify   [maxPads]*padNotify
	onRumble func(idx int, large, small uint8)
}

// OpenGamepads connects to ViGEmBus. onRumble (may be nil) receives every
// force-feedback change of a plugged pad, from that pad's listener goroutine.
func OpenGamepads(onRumble func(idx int, large, small uint8)) (*Gamepads, error) {
	path, err := vigemPath()
	if err != nil {
		return nil, err
	}
	p16, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p16, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("opening ViGEmBus: %w", err)
	}
	io, err := newOverlappedIO()
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	g := &Gamepads{h: h, io: io, onRumble: onRumble}
	var ver [8]byte
	binary.LittleEndian.PutUint32(ver[0:], 8)
	binary.LittleEndian.PutUint32(ver[4:], vigemCommonVersion)
	if err := g.ioctl(ioctlCheckVersion, ver[:]); err != nil {
		io.close()
		windows.CloseHandle(h)
		return nil, fmt.Errorf("ViGEmBus version check failed (update ViGEmBus): %w", err)
	}
	return g, nil
}

// overlappedIO makes one IOCTL at a time on a handle opened for overlapped
// I/O and waits for it. It lives on the heap (Go never moves heap memory):
// the I/O manager writes its OVERLAPPED, and buf when buf is the output
// buffer, until the request completes, also after a goroutine stack moved.
type overlappedIO struct {
	ov  windows.Overlapped
	n   uint32
	buf [xusbNotificationSize]byte
}

func newOverlappedIO() (*overlappedIO, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil) // manual reset; the I/O manager clears it per request
	if err != nil {
		return nil, fmt.Errorf("CreateEvent: %w", err)
	}
	return &overlappedIO{ov: windows.Overlapped{HEvent: ev}}, nil
}

func (o *overlappedIO) close() { windows.CloseHandle(o.ov.HEvent) }

// do sends code with in (copied when the request starts: METHOD_BUFFERED)
// and waits for the result. out is nil or o.buf.
func (o *overlappedIO) do(h windows.Handle, code uint32, in, out []byte) error {
	o.ov = windows.Overlapped{HEvent: o.ov.HEvent}
	var inP, outP *byte
	if len(in) > 0 {
		inP = &in[0]
	}
	if len(out) > 0 {
		outP = &out[0]
	}
	err := windows.DeviceIoControl(h, code, inP, uint32(len(in)), outP, uint32(len(out)), &o.n, &o.ov)
	if err == windows.ERROR_IO_PENDING {
		err = windows.GetOverlappedResult(h, &o.ov, &o.n, true)
	}
	return err
}

// padNotify is a plugged pad's force-feedback listener.
type padNotify struct {
	code uint32 // ioctlXusbRequestNotification (a test uses a pipe's listen)
	io   *overlappedIO
	stop atomic.Bool
	done chan struct{}
}

// listen keeps a notification request pending for the pad with this serial
// and passes each completion to onRumble, until stopped (end).
func (g *Gamepads) listen(idx int, serial uint32, n *padNotify) {
	defer close(n.done)
	// The request belongs to the thread that made it; keep that thread for
	// the goroutine (it ends with it), so the request stays alive.
	runtime.LockOSThread()
	fails := 0
	for !n.stop.Load() {
		b := n.io.buf[:]
		clear(b)
		binary.LittleEndian.PutUint32(b[0:], xusbNotificationSize)
		binary.LittleEndian.PutUint32(b[4:], serial)
		if err := n.io.do(g.h, n.code, b, b); err != nil {
			// Cancelled (unplug, close) or refused. A refusal that keeps
			// coming (a bus without notifications) ends the listener.
			if n.stop.Load() || errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return
			}
			if fails++; fails >= 10 {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fails = 0
		if !n.stop.Load() {
			g.onRumble(idx, b[8], b[9]) // LargeMotor, SmallMotor
		}
	}
}

// end stops the listener: its pending request is cancelled until it has
// returned (the unplug before it normally completes the request already).
// A request that never completes is left to the driver with its memory.
func (n *padNotify) end(h windows.Handle) {
	n.stop.Store(true)
	for i := 0; i < 20; i++ {
		_ = windows.CancelIoEx(h, &n.io.ov)
		select {
		case <-n.done:
			n.io.close()
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func vigemPath() (string, error) {
	list, err := windows.CM_Get_Device_Interface_List("", &guidViGEmBus, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return "", fmt.Errorf("ViGEmBus not installed: %w", err)
	}
	if len(list) == 0 {
		return "", errors.New("ViGEmBus not installed (no device interface)")
	}
	return list[0], nil
}

// ioctl sends an IOCTL without output; callers hold mu (or own g alone).
func (g *Gamepads) ioctl(code uint32, in []byte) error {
	return g.io.do(g.h, code, in, nil)
}

func (g *Gamepads) plug(idx int) error {
	if g.serials[idx] != 0 {
		return nil
	}
	used := map[uint32]bool{}
	for _, s := range g.serials {
		used[s] = true
	}
	var lastErr error
	for serial := uint32(1); serial <= 16; serial++ {
		if used[serial] {
			continue
		}
		var b [16]byte
		binary.LittleEndian.PutUint32(b[0:], 16)
		binary.LittleEndian.PutUint32(b[4:], serial)
		binary.LittleEndian.PutUint32(b[8:], 0)       // Xbox360Wired
		binary.LittleEndian.PutUint16(b[12:], 0x045E) // Microsoft
		binary.LittleEndian.PutUint16(b[14:], 0x028E) // Xbox 360 Controller
		if err := g.ioctl(ioctlPluginTarget, b[:]); err != nil {
			lastErr = err
			continue
		}
		var w [8]byte
		binary.LittleEndian.PutUint32(w[0:], 8)
		binary.LittleEndian.PutUint32(w[4:], serial)
		_ = g.ioctl(ioctlWaitDeviceReady, w[:]) // older bus versions lack this IOCTL
		g.serials[idx] = serial
		if g.onRumble != nil {
			if io, err := newOverlappedIO(); err == nil { // else: no force feedback for this pad
				n := &padNotify{code: ioctlXusbRequestNotification, io: io, done: make(chan struct{})}
				g.notify[idx] = n
				go g.listen(idx, serial, n)
			}
		}
		return nil
	}
	return fmt.Errorf("ViGEm plugin failed: %v", lastErr)
}

// Update applies a state to pad idx, plugging it in on first use.
func (g *Gamepads) Update(idx int, p Pad) error {
	if idx < 0 || idx >= maxPads {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.plug(idx); err != nil {
		return err
	}
	var b [20]byte
	binary.LittleEndian.PutUint32(b[0:], 20)
	binary.LittleEndian.PutUint32(b[4:], g.serials[idx])
	binary.LittleEndian.PutUint16(b[8:], p.Buttons)
	b[10] = p.LT
	b[11] = p.RT
	binary.LittleEndian.PutUint16(b[12:], uint16(p.LX))
	binary.LittleEndian.PutUint16(b[14:], uint16(p.LY))
	binary.LittleEndian.PutUint16(b[16:], uint16(p.RX))
	binary.LittleEndian.PutUint16(b[18:], uint16(p.RY))
	return g.ioctl(ioctlXusbSubmit, b[:])
}

// Unplug removes pad idx.
func (g *Gamepads) Unplug(idx int) {
	if idx < 0 || idx >= maxPads {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unplugLocked(idx)
}

func (g *Gamepads) unplugLocked(idx int) {
	if g.serials[idx] == 0 {
		return
	}
	n := g.notify[idx]
	if n != nil {
		n.stop.Store(true) // before the unplug completes its request
	}
	var b [8]byte
	binary.LittleEndian.PutUint32(b[0:], 8)
	binary.LittleEndian.PutUint32(b[4:], g.serials[idx])
	_ = g.ioctl(ioctlUnplugTarget, b[:])
	g.serials[idx] = 0
	if n != nil {
		g.notify[idx] = nil
		n.end(g.h)
	}
}

// Close unplugs all pads and closes the bus handle.
func (g *Gamepads) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.serials {
		g.unplugLocked(i)
	}
	windows.CloseHandle(g.h)
	g.io.close()
}
