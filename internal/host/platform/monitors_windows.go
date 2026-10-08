//go:build windows

package platform

import (
	"fmt"
	"sort"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                            = windows.NewLazySystemDLL("user32.dll")
	procEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procEnumDisplaySettingsW          = user32.NewProc("EnumDisplaySettingsW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	dxgi                              = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1            = dxgi.NewProc("CreateDXGIFactory1")
	iidIDXGIFactory1                  = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
)

// EnableDPIAwareness makes all coordinates (monitors, cursor, SendInput) physical
// pixels, matching what the capture API sees. Call once at startup.
func EnableDPIAwareness() {
	const perMonitorAwareV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(perMonitorAwareV2); r != 0 {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
}

type monitorInfoEx struct {
	cbSize    uint32
	rcMonitor windows.Rect
	rcWork    windows.Rect
	dwFlags   uint32
	szDevice  [32]uint16
}

var (
	enumMu   sync.Mutex
	enumAcc  []Monitor
	enumProc = syscall.NewCallback(func(hmon, hdc, rect, data uintptr) uintptr {
		var mi monitorInfoEx
		mi.cbSize = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return 1
		}
		enumAcc = append(enumAcc, Monitor{
			Name:       windows.UTF16ToString(mi.szDevice[:]),
			X:          int(mi.rcMonitor.Left),
			Y:          int(mi.rcMonitor.Top),
			W:          int(mi.rcMonitor.Right - mi.rcMonitor.Left),
			H:          int(mi.rcMonitor.Bottom - mi.rcMonitor.Top),
			Primary:    mi.dwFlags&1 != 0,
			HMonitor:   uint64(hmon),
			DXGIOutput: -1,
			Hz:         refreshRate(mi.szDevice[:]),
		})
		return 1
	})
)

// Monitors lists displays, primary first.
func Monitors() ([]Monitor, error) {
	enumMu.Lock()
	enumAcc = nil
	r, _, err := procEnumDisplayMonitors.Call(0, 0, enumProc, 0)
	mons := enumAcc
	enumAcc = nil
	enumMu.Unlock()
	if r == 0 {
		return nil, err
	}
	// Map HMONITOR -> DXGI output index on adapter 0 (what ddagrab enumerates).
	for idx, hmon := range dxgiOutputs() {
		for i := range mons {
			if mons[i].HMonitor == hmon {
				mons[i].DXGIOutput = idx
			}
		}
	}
	sort.SliceStable(mons, func(i, j int) bool {
		if mons[i].Primary != mons[j].Primary {
			return mons[i].Primary
		}
		return mons[i].X < mons[j].X
	})
	for i := range mons {
		mons[i].Index = i
	}
	return mons, nil
}

func refreshRate(device []uint16) int {
	var dm [220]byte
	*(*uint16)(unsafe.Pointer(&dm[68])) = 220 // dmSize
	const enumCurrentSettings = ^uintptr(0)   // (DWORD)-1
	if r, _, _ := procEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(&device[0])), enumCurrentSettings, uintptr(unsafe.Pointer(&dm[0]))); r == 0 {
		return 0
	}
	return int(*(*uint32)(unsafe.Pointer(&dm[184]))) // dmDisplayFrequency
}

type comObj struct{ vtbl *[64]uintptr }

// call calls method slot. Arguments may be pointers converted to uintptr in
// the call expression: uintptrescapes keeps their memory in place.
//
//go:uintptrescapes
func (o *comObj) call(slot int, args ...uintptr) uintptr {
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[slot], all...)
	return r
}
func (o *comObj) release() { o.call(2) }

type dxgiOutputDesc struct {
	deviceName [32]uint16
	desktop    windows.Rect
	attached   int32
	rotation   uint32
	monitor    uintptr
}

type dxgiAdapterDesc1 struct {
	description                                                     [128]uint16
	vendorID, deviceID, subSysID, revision                          uint32
	dedicatedVideoMemory, dedicatedSystemMemory, sharedSystemMemory uintptr
	luid                                                            windows.LUID
	flags                                                           uint32
}

// dxgiAdapter0 opens adapter 0 of a new DXGI factory (what ddagrab
// enumerates); release frees both.
func dxgiAdapter0() (adapter *comObj, release func(), err error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, nil, err
	}
	var factory *comObj
	if r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); int32(r) < 0 || factory == nil {
		return nil, nil, fmt.Errorf("CreateDXGIFactory1: HRESULT %#x", uint32(r))
	}
	if r := factory.call(12 /*EnumAdapters1*/, 0, uintptr(unsafe.Pointer(&adapter))); int32(r) < 0 || adapter == nil {
		factory.release()
		return nil, nil, fmt.Errorf("IDXGIFactory1::EnumAdapters1(0): HRESULT %#x", uint32(r))
	}
	return adapter, func() { adapter.release(); factory.release() }, nil
}

// PrimaryAdapter describes adapter 0, the GPU that ddagrab captures on.
func PrimaryAdapter() (Adapter, error) {
	adapter, release, err := dxgiAdapter0()
	if err != nil {
		return Adapter{}, err
	}
	defer release()
	var d dxgiAdapterDesc1
	if r := adapter.call(10 /*GetDesc1*/, uintptr(unsafe.Pointer(&d))); int32(r) < 0 {
		return Adapter{}, fmt.Errorf("IDXGIAdapter1::GetDesc1: HRESULT %#x", uint32(r))
	}
	return Adapter{
		Vendor:   adapterVendor(d.vendorID),
		VendorID: d.vendorID,
		LUID:     uint64(uint32(d.luid.HighPart))<<32 | uint64(d.luid.LowPart),
		Name:     windows.UTF16ToString(d.description[:]),
	}, nil
}

// dxgiOutputs returns the HMONITOR of each output of adapter 0, in order.
func dxgiOutputs() []uint64 {
	adapter, release, err := dxgiAdapter0()
	if err != nil {
		return nil
	}
	defer release()
	var out []uint64
	for i := uintptr(0); i < 16; i++ {
		var output *comObj
		if r := adapter.call(7 /*EnumOutputs*/, i, uintptr(unsafe.Pointer(&output))); int32(r) < 0 || output == nil {
			break
		}
		var desc dxgiOutputDesc
		if r := output.call(7 /*GetDesc*/, uintptr(unsafe.Pointer(&desc))); int32(r) >= 0 {
			out = append(out, uint64(desc.monitor))
		} else {
			out = append(out, 0)
		}
		output.release()
	}
	return out
}

// RaisePriority gives the agent above-normal CPU priority so input injection
// and frame forwarding are not starved while a game saturates the CPU.
func RaisePriority() {
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.ABOVE_NORMAL_PRIORITY_CLASS)
}
