//go:build windows

package platform

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

// reasonContext is REASON_CONTEXT (minwinbase.h) with a simple reason
// string: Version and Flags, then the Reason union, whose larger (detailed)
// form takes 24 bytes on x64.
type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
	_       [16]byte
}

const (
	powerRequestContextSimpleString = 0x1 // POWER_REQUEST_CONTEXT_SIMPLE_STRING
	powerRequestDisplayRequired     = 0   // POWER_REQUEST_TYPE PowerRequestDisplayRequired
)

// DisplayRequest is a power request that keeps the display on while it is
// set (as ES_DISPLAY_REQUIRED does for the thread that sets it; a request is
// held by its handle, whichever thread a goroutine runs on). Windows shows
// its reason in powercfg /requests.
type DisplayRequest struct{ h windows.Handle }

// NewDisplayRequest creates a display request, not yet set.
func NewDisplayRequest(reason string) (*DisplayRequest, error) {
	s, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return nil, err
	}
	rc := reasonContext{flags: powerRequestContextSimpleString, reason: s}
	r, _, e := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
	if h := windows.Handle(r); h != windows.InvalidHandle && h != 0 {
		return &DisplayRequest{h: h}, nil
	}
	return nil, e
}

// Set sets the request (the display stays on) or clears it.
func (d *DisplayRequest) Set(on bool) error {
	p := procPowerClearRequest
	if on {
		p = procPowerSetRequest
	}
	if r, _, e := p.Call(uintptr(d.h), powerRequestDisplayRequired); r == 0 {
		return e
	}
	return nil
}

// Close ends the request (a set one is cleared).
func (d *DisplayRequest) Close() { _ = windows.CloseHandle(d.h) }
