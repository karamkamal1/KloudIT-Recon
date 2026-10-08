//go:build windows

package vdisplay

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

var (
	user32                          = windows.NewLazySystemDLL("user32.dll")
	procGetDisplayConfigBufferSizes = user32.NewProc("GetDisplayConfigBufferSizes")
	procQueryDisplayConfig          = user32.NewProc("QueryDisplayConfig")
	procSetDisplayConfig            = user32.NewProc("SetDisplayConfig")
	procDisplayConfigGetDeviceInfo  = user32.NewProc("DisplayConfigGetDeviceInfo")
)

// DisplayConfigGetDeviceInfo request types and structs (wingdi.h).
const (
	deviceInfoGetSourceName  = 1 // DISPLAYCONFIG_DEVICE_INFO_GET_SOURCE_NAME
	deviceInfoGetAdapterName = 4 // DISPLAYCONFIG_DEVICE_INFO_GET_ADAPTER_NAME
)

type deviceInfoHeader struct {
	Type      uint32
	Size      uint32
	AdapterID LUID
	ID        uint32
}

type sourceDeviceName struct { // DISPLAYCONFIG_SOURCE_DEVICE_NAME, 84 bytes
	Header            deviceInfoHeader
	ViewGdiDeviceName [32]uint16
}

type adapterName struct { // DISPLAYCONFIG_ADAPTER_NAME, 276 bytes
	Header            deviceInfoHeader
	AdapterDevicePath [128]uint16
}

// winSystem is the real display configuration.
type winSystem struct{}

// slicePtr is the array argument for s: NULL when empty (SetDisplayConfig
// with SDC_USE_DATABASE_CURRENT requires NULL arrays). Convert it to uintptr
// in the call expression, so s stays in place during the call.
func slicePtr[T any](s []T) unsafe.Pointer {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Pointer(&s[0])
}

func (winSystem) Query(flags uint32) (Config, error) {
	return queryDisplayConfig(flags | QDCVirtualModeAware)
}

// queryDisplayConfig is QueryDisplayConfig with exactly these flags.
func queryDisplayConfig(flags uint32) (Config, error) {
	for range 5 {
		var np, nm uint32
		if r, _, _ := procGetDisplayConfigBufferSizes.Call(uintptr(flags), uintptr(unsafe.Pointer(&np)), uintptr(unsafe.Pointer(&nm))); r != 0 {
			return Config{}, fmt.Errorf("GetDisplayConfigBufferSizes: %w", syscall.Errno(r))
		}
		c := Config{Paths: make([]PathInfo, np), Modes: make([]ModeInfo, nm)}
		r, _, _ := procQueryDisplayConfig.Call(uintptr(flags), uintptr(unsafe.Pointer(&np)), uintptr(slicePtr(c.Paths)),
			uintptr(unsafe.Pointer(&nm)), uintptr(slicePtr(c.Modes)), 0)
		if syscall.Errno(r) == windows.ERROR_INSUFFICIENT_BUFFER {
			continue // the topology changed between the two calls
		}
		if r != 0 {
			return Config{}, fmt.Errorf("QueryDisplayConfig: %w", syscall.Errno(r))
		}
		c.Paths, c.Modes = c.Paths[:np], c.Modes[:nm]
		return c, nil
	}
	return Config{}, fmt.Errorf("QueryDisplayConfig: %w", windows.ERROR_INSUFFICIENT_BUFFER)
}

func (winSystem) Apply(c Config, flags uint32) error {
	r, _, _ := procSetDisplayConfig.Call(uintptr(len(c.Paths)), uintptr(slicePtr(c.Paths)), uintptr(len(c.Modes)), uintptr(slicePtr(c.Modes)), uintptr(flags))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func getDeviceInfo(h *deviceInfoHeader) error {
	if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(h))); r != 0 {
		return fmt.Errorf("DisplayConfigGetDeviceInfo(%d): %w", h.Type, syscall.Errno(r))
	}
	return nil
}

func (winSystem) SourceName(adapter LUID, id uint32) (string, error) {
	var n sourceDeviceName
	n.Header = deviceInfoHeader{Type: deviceInfoGetSourceName, Size: uint32(unsafe.Sizeof(n)), AdapterID: adapter, ID: id}
	if err := getDeviceInfo(&n.Header); err != nil {
		return "", err
	}
	return windows.UTF16ToString(n.ViewGdiDeviceName[:]), nil
}

func (winSystem) AdapterInstance(adapter LUID) (string, error) {
	var n adapterName
	n.Header = deviceInfoHeader{Type: deviceInfoGetAdapterName, Size: uint32(unsafe.Sizeof(n)), AdapterID: adapter}
	if err := getDeviceInfo(&n.Header); err != nil {
		return "", err
	}
	return instanceFromInterfacePath(windows.UTF16ToString(n.AdapterDevicePath[:])), nil
}

func (winSystem) Monitor(name string) (platform.Monitor, bool) {
	mons, err := platform.Monitors()
	if err != nil {
		return platform.Monitor{}, false
	}
	for _, m := range mons {
		if strings.EqualFold(m.Name, name) {
			return m, true
		}
	}
	return platform.Monitor{}, false
}
