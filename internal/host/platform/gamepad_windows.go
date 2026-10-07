//go:build windows

package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// Virtual Xbox 360 controllers via the ViGEmBus driver, spoken to directly over
// its IOCTL interface (no ViGEmClient.dll needed). Requires ViGEmBus to be
// installed: https://github.com/nefarius/ViGEmBus/releases

var guidViGEmBus = windows.GUID{Data1: 0x96E42B22, Data2: 0xF5E9, Data3: 0x42F8, Data4: [8]byte{0xB0, 0x43, 0xED, 0x0F, 0x93, 0x2F, 0x01, 0x4F}}

const (
	ioctlPluginTarget    = 0x2AA004
	ioctlUnplugTarget    = 0x2AA008
	ioctlCheckVersion    = 0x2AA00C
	ioctlWaitDeviceReady = 0x2AA010
	ioctlXusbSubmit      = 0x2AA808
	vigemCommonVersion   = 0x0001
	maxPads              = 4
)

// Gamepads manages up to four virtual controllers.
type Gamepads struct {
	mu      sync.Mutex
	h       windows.Handle
	serials [maxPads]uint32
}

// OpenGamepads connects to ViGEmBus.
func OpenGamepads() (*Gamepads, error) {
	path, err := vigemPath()
	if err != nil {
		return nil, err
	}
	p16, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p16, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("opening ViGEmBus: %w", err)
	}
	g := &Gamepads{h: h}
	var ver [8]byte
	binary.LittleEndian.PutUint32(ver[0:], 8)
	binary.LittleEndian.PutUint32(ver[4:], vigemCommonVersion)
	if err := g.ioctl(ioctlCheckVersion, ver[:]); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("ViGEmBus version check failed (update ViGEmBus): %w", err)
	}
	return g, nil
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

func (g *Gamepads) ioctl(code uint32, in []byte) error {
	var ret uint32
	return windows.DeviceIoControl(g.h, code, &in[0], uint32(len(in)), nil, 0, &ret, nil)
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
	var b [8]byte
	binary.LittleEndian.PutUint32(b[0:], 8)
	binary.LittleEndian.PutUint32(b[4:], g.serials[idx])
	_ = g.ioctl(ioctlUnplugTarget, b[:])
	g.serials[idx] = 0
}

// Close unplugs all pads and closes the bus handle.
func (g *Gamepads) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.serials {
		g.unplugLocked(i)
	}
	windows.CloseHandle(g.h)
}
