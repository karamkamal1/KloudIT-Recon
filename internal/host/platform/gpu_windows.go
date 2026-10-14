//go:build windows

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Whether this PC has a GPU that FFmpeg's GPU capture and the GPU encoders
// can use, without starting FFmpeg: a PC or VM without one (Proxmox, Hyper-V,
// VMware or VirtualBox without a passthrough GPU) shows only the Microsoft
// Basic Display Adapter (or the hypervisor's display-only adapter: VirtIO-GPU,
// QXL, the Standard VGA, Hyper-V Video), and Direct3D renders on the Microsoft
// Basic Render Driver (WARP), a software adapter with no video support.

var (
	d3d11                 = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice = d3d11.NewProc("D3D11CreateDevice")
	iidID3D11VideoDevice  = windows.GUID{Data1: 0x10EC4D5B, Data2: 0x975A, Data3: 0x4689, Data4: [8]byte{0xB9, 0xE4, 0xD0, 0xAA, 0xC3, 0x0F, 0xE3, 0x33}}
	iidID3D11VideoContext = windows.GUID{Data1: 0x61F21C45, Data2: 0x3C0E, Data3: 0x4A74, Data4: [8]byte{0x9C, 0xEA, 0x67, 0x10, 0x0D, 0x9A, 0xD5, 0xE4}}
)

const (
	d3dDriverTypeHardware         = 1     // D3D_DRIVER_TYPE_HARDWARE
	d3d11CreateDeviceVideoSupport = 0x800 // D3D11_CREATE_DEVICE_VIDEO_SUPPORT
	d3d11SDKVersion               = 7     // D3D11_SDK_VERSION
	dxgiAdapterFlagSoftware       = 0x2   // DXGI_ADAPTER_FLAG_SOFTWARE
)

// D3D11VideoDevice opens a Direct3D 11 device with video support on the
// default adapter and its video interfaces, as FFmpeg's D3D11VA hardware
// device does for ddagrab and gfxcapture (libavutil/hwcontext_d3d11va.c:
// D3D11CreateDevice with D3D11_CREATE_DEVICE_VIDEO_SUPPORT, then
// ID3D11VideoDevice and ID3D11VideoContext), and releases them. An error
// means FFmpeg's GPU capture fails with "Failed to create D3D11VA device":
// no GPU (WARP has no video support), a GPU without its driver, a driver
// being installed or reset, a remote desktop session rendering on WARP. It
// takes milliseconds and needs no desktop: it works at the lock screen and
// in a disconnected session, where a test capture cannot tell.
func D3D11VideoDevice() error {
	if err := procD3D11CreateDevice.Find(); err != nil {
		return err
	}
	var dev, ctx *comObj
	r, _, _ := procD3D11CreateDevice.Call(0, d3dDriverTypeHardware, 0, d3d11CreateDeviceVideoSupport, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&dev)), 0, uintptr(unsafe.Pointer(&ctx)))
	if int32(r) < 0 || dev == nil {
		return fmt.Errorf("D3D11CreateDevice with video support: HRESULT %#x", uint32(r))
	}
	defer dev.release()
	if ctx != nil {
		defer ctx.release()
	}
	var vd *comObj
	if r := dev.call(0 /*QueryInterface*/, uintptr(unsafe.Pointer(&iidID3D11VideoDevice)), uintptr(unsafe.Pointer(&vd))); int32(r) < 0 || vd == nil {
		return fmt.Errorf("ID3D11VideoDevice: HRESULT %#x", uint32(r))
	}
	vd.release()
	if ctx == nil {
		return fmt.Errorf("D3D11CreateDevice gave no immediate context")
	}
	var vc *comObj
	if r := ctx.call(0 /*QueryInterface*/, uintptr(unsafe.Pointer(&iidID3D11VideoContext)), uintptr(unsafe.Pointer(&vc))); int32(r) < 0 || vc == nil {
		return fmt.Errorf("ID3D11VideoContext: HRESULT %#x", uint32(r))
	}
	vc.release()
	return nil
}

// Adapters lists the DXGI adapters (IDXGIFactory1::EnumAdapters1): the GPUs,
// then the Microsoft Basic Render Driver (Software).
func Adapters() ([]Adapter, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	var factory *comObj
	if r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); int32(r) < 0 || factory == nil {
		return nil, fmt.Errorf("CreateDXGIFactory1: HRESULT %#x", uint32(r))
	}
	defer factory.release()
	var out []Adapter
	for i := uintptr(0); i < 16; i++ {
		var adapter *comObj
		if r := factory.call(12 /*EnumAdapters1*/, i, uintptr(unsafe.Pointer(&adapter))); int32(r) < 0 || adapter == nil {
			break
		}
		var d dxgiAdapterDesc1
		r := adapter.call(10 /*GetDesc1*/, uintptr(unsafe.Pointer(&d)))
		adapter.release()
		if int32(r) < 0 {
			continue
		}
		out = append(out, Adapter{
			Vendor:   adapterVendor(d.vendorID),
			VendorID: d.vendorID,
			LUID:     uint64(uint32(d.luid.HighPart))<<32 | uint64(d.luid.LowPart),
			Name:     windows.UTF16ToString(d.description[:]),
			Software: d.flags&dxgiAdapterFlagSoftware != 0,
		})
	}
	return out, nil
}
