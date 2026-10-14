// Package platform wraps OS facilities the host needs besides capture/encode:
// monitor enumeration, DPI awareness, cursor shapes, virtual gamepads and
// keeping the display on.
package platform

import (
	"errors"
	"strings"
)

// Monitor describes a display.
type Monitor struct {
	Index      int
	Name       string
	X, Y, W, H int // physical pixels in virtual-desktop coordinates
	Primary    bool
	Hz         int
	HMonitor   uint64 // Windows HMONITOR (gfxcapture hmonitor=)
	DXGIOutput int    // output index on adapter 0 (ddagrab output_idx=), -1 if unknown
	Rotated    bool   // the DXGI output is rotated 90, 180 or 270 degrees
}

// Adapter describes a GPU adapter (DXGI_ADAPTER_DESC1).
type Adapter struct {
	Vendor   string // nvidia, amd, intel or other, from VendorID
	VendorID uint32 // PCI vendor ID
	LUID     uint64 // AdapterLuid: HighPart<<32 | LowPart
	Name     string
	Software bool // DXGI_ADAPTER_FLAG_SOFTWARE: the Microsoft Basic Render Driver (WARP)
}

// NoGPU returns why adapters (Adapters) have no GPU: none is listed, or
// each is a software adapter (the Microsoft Basic Render Driver, which a PC
// or VM without a GPU renders on); "" when one is a GPU. No GPU encoder can
// work then (AMF, NVENC and Quick Sync each need their vendor's GPU).
func NoGPU(adapters []Adapter) string {
	var names []string
	for _, a := range adapters {
		if !a.Software {
			return ""
		}
		names = append(names, a.Name)
	}
	if len(names) == 0 {
		return "Windows lists no graphics adapter"
	}
	return "Windows lists no GPU, only " + strings.Join(names, ", ")
}

// adapterVendor names a PCI vendor ID as the native helper does.
func adapterVendor(id uint32) string {
	switch id {
	case 0x1002:
		return "amd"
	case 0x10DE:
		return "nvidia"
	case 0x8086:
		return "intel"
	}
	return "other"
}

// CursorShape is a cursor image in RGBA.
type CursorShape struct {
	ID         uint64
	W, H       int
	HotX, HotY int
	RGBA       []byte
}

// CursorState is the current cursor.
type CursorState struct {
	Visible bool
	X, Y    int // virtual-desktop pixels
	Handle  uint64
}

// Pad is the state applied to a virtual Xbox 360 controller.
type Pad struct {
	Buttons uint16
	LT, RT  uint8
	LX, LY  int16
	RX, RY  int16
}

var ErrUnsupported = errors.New("not supported on this platform")

// Desktop is the state of this process's Windows session and its input
// desktop (DesktopState): whether there is a desktop on a screen to capture
// and give input to.
type Desktop struct {
	Session uint32 // this process's session
	Console uint32 // the session attached to the console (^0: none at the moment)
	Remote  bool   // a remote desktop client session (SM_REMOTESESSION)
	// Known: the session's state was read (Disconnected, Locked).
	Known bool
	// Disconnected: the session has no display: a remote desktop client
	// closed it and it was not moved to the console.
	Disconnected bool
	// Locked: the session is locked (the lock screen).
	Locked bool
	// Input is the input desktop's name ("Default"; "" when it could not be
	// opened); Secure: it is a secure desktop (Winlogon: the lock screen, a
	// UAC prompt, Ctrl+Alt+Del), which no program but SYSTEM's may open.
	Input  string
	Secure bool
}

// Reasons a desktop is away (Desktop.Away).
const (
	DesktopDisconnected = "disconnected"
	DesktopLocked       = "locked"
	DesktopSecure       = "secure"
)

// Away returns why the session has no desktop to capture and control now
// (DesktopDisconnected, DesktopLocked, DesktopSecure), or "".
func (d Desktop) Away() string {
	switch {
	case d.Disconnected:
		return DesktopDisconnected
	case d.Locked:
		return DesktopLocked
	case d.Secure:
		return DesktopSecure
	}
	return ""
}
