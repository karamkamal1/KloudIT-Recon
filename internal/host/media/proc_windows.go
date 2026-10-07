//go:build windows

package media

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

var (
	gdi32                                       = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTSetProcessSchedulingPriorityClass = gdi32.NewProc("D3DKMTSetProcessSchedulingPriorityClass")
	procD3DKMTOpenAdapterFromLuid               = gdi32.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo                  = gdi32.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter                      = gdi32.NewProc("D3DKMTCloseAdapter")
	advapi32                                    = windows.NewLazySystemDLL("advapi32.dll")
	procAdjustTokenPrivileges                   = advapi32.NewProc("AdjustTokenPrivileges")
)

// hideWindow keeps child processes from flashing a console window and gives the
// encoder elevated CPU priority so capture is not starved by the game.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.ABOVE_NORMAL_PRIORITY_CLASS,
	}
}

// raisePriority gives the started encoder process HIGH CPU priority and the
// GPU scheduling priority for gpuMode (setGPUPriority). It returns what the
// GPU priority became and the adapter and HAGS it was based on, for the log.
func raisePriority(cmd *exec.Cmd, vendor, gpuMode string) (gpu string, host gpuHost, err error) {
	if cmd.Process == nil {
		return "", gpuHost{}, nil
	}
	if h, oerr := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(cmd.Process.Pid)); oerr == nil {
		_ = windows.SetPriorityClass(h, windows.HIGH_PRIORITY_CLASS)
		windows.CloseHandle(h)
	}
	return setGPUPriority(cmd.Process.Pid, vendor, gpuMode)
}

// setGPUPriority sets the GPU scheduling priority class of process pid with
// D3DKMTSetProcessSchedulingPriorityClass (applyGPUPriority: REALTIME, HIGH
// for NVIDIA with HAGS on or unknown in auto mode, a refused REALTIME retried
// as HIGH).
// It runs right after the process starts, before FFmpeg creates its D3D11
// device for capture and encode.
func setGPUPriority(pid int, vendor, mode string) (string, gpuHost, error) {
	host := gpuHostInfo()
	if mode == GPUPriorityOff {
		return gpuGotOff, host, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return gpuGotFailed, host, fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(h)
	got, err := applyGPUPriority(vendor, mode, host, func(class uint32) error {
		if err := procD3DKMTSetProcessSchedulingPriorityClass.Find(); err != nil {
			return err // gdi32 without it (Wine): refused like any other status
		}
		status, _, _ := procD3DKMTSetProcessSchedulingPriorityClass.Call(uintptr(h), uintptr(class))
		if st := windows.NTStatus(uint32(status)); st != 0 { // NTSTATUS is 32 bits wide
			return fmt.Errorf("D3DKMTSetProcessSchedulingPriorityClass(%d): %w", class, st)
		}
		return nil
	})
	return got, host, err
}

// gpuHostInfo detects adapter 0 and HAGS on it once (detectGPUHost).
var gpuHostInfo = sync.OnceValue(func() gpuHost { return detectGPUHost(platform.PrimaryAdapter) })

// detectGPUHost asks the kernel whether HAGS is on for adapter 0 (kernelHAGS,
// as Sunshine does) and falls back to HwSchMode when it cannot.
func detectGPUHost(adapter0 func() (platform.Adapter, error)) gpuHost {
	var h gpuHost
	a, err := adapter0()
	if err == nil {
		h.adapter, h.name = a.Vendor, a.Name
		var on bool
		if on, err = kernelHAGS(a.LUID); err == nil {
			h.hags, h.hagsFrom = hagsOff, "kernel"
			if on {
				h.hags = hagsOn
			}
			return h
		}
	}
	h.hags, h.hagsFrom, h.err = hwSchModeHAGS(readHwSchMode()), "registry", err
	return h
}

// KMTQAITYPE_WDDM_2_7_CAPS, and HwSchEnabled in D3DKMT_WDDM_2_7_CAPS
// (HwSchSupported is bit 0, HwSchEnabledByDefault bit 2).
const (
	kmtqaitypeWDDM27Caps = 70
	wddm27HwSchEnabled   = 1 << 1
)

type d3dkmtOpenAdapterFromLUID struct {
	luid    windows.LUID
	adapter uint32 // D3DKMT_HANDLE; D3DKMT_CLOSEADAPTER is just this handle
}

type d3dkmtQueryAdapterInfo struct {
	adapter uint32
	typ     uint32
	data    unsafe.Pointer
	size    uint32
}

// kernelHAGS reports whether hardware-accelerated GPU scheduling is on for
// the adapter with the given LUID: D3DKMT_WDDM_2_7_CAPS.HwSchEnabled, the
// state in effect, also when HwSchMode is not set.
func kernelHAGS(luid uint64) (bool, error) {
	open := d3dkmtOpenAdapterFromLUID{luid: windows.LUID{LowPart: uint32(luid), HighPart: int32(luid >> 32)}}
	if err := d3dkmtCall(procD3DKMTOpenAdapterFromLuid, unsafe.Pointer(&open)); err != nil {
		return false, err
	}
	defer func() { _ = d3dkmtCall(procD3DKMTCloseAdapter, unsafe.Pointer(&open.adapter)) }()
	var caps uint32
	q := d3dkmtQueryAdapterInfo{adapter: open.adapter, typ: kmtqaitypeWDDM27Caps, data: unsafe.Pointer(&caps), size: uint32(unsafe.Sizeof(caps))}
	if err := d3dkmtCall(procD3DKMTQueryAdapterInfo, unsafe.Pointer(&q)); err != nil {
		return false, err
	}
	return caps&wddm27HwSchEnabled != 0, nil
}

// d3dkmtCall calls a gdi32 D3DKMT function that takes one struct and returns
// an NTSTATUS.
func d3dkmtCall(proc *windows.LazyProc, arg unsafe.Pointer) error {
	if err := proc.Find(); err != nil {
		return err
	}
	r, _, _ := proc.Call(uintptr(arg))
	if st := windows.NTStatus(uint32(r)); st != 0 { // NTSTATUS is 32 bits wide
		return fmt.Errorf("%s: %w", proc.Name, st)
	}
	return nil
}

func readHwSchMode() (uint64, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\GraphicsDrivers`, registry.QUERY_VALUE)
	if err != nil {
		return 0, err
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("HwSchMode")
	return v, err
}

// EnableGPUPriorityPrivilege enables SeIncreaseBasePriorityPrivilege on the
// agent's token, which REALTIME GPU priority needs. An elevated token holds it
// (disabled); otherwise the result is ERROR_NOT_ALL_ASSIGNED and encoders get
// HIGH instead.
func EnableGPUPriorityPrivilege() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer tok.Close()
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	name, _ := windows.UTF16PtrFromString("SeIncreaseBasePriorityPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &tp.Privileges[0].Luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue: %w", err)
	}
	// AdjustTokenPrivileges succeeds without enabling a privilege the token
	// does not hold and reports that only through the last error, which
	// windows.AdjustTokenPrivileges drops.
	ok, _, lastErr := procAdjustTokenPrivileges.Call(uintptr(tok), 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
	if ok == 0 {
		return fmt.Errorf("AdjustTokenPrivileges: %w", lastErr)
	}
	if lastErr == windows.ERROR_NOT_ALL_ASSIGNED {
		return lastErr
	}
	return nil
}
