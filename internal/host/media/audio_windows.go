//go:build windows

package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WASAPI loopback capture of the default render device, implemented directly
// against the COM vtables (no cgo, no reflection).

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	subtypeIEEEFloat        = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
)

const (
	clsctxAll                    = 0x17
	eRender                      = 0
	eConsole                     = 0
	sharedMode                   = 0
	streamflagsLoopback          = 0x00020000
	streamflagsAutoConvert       = 0x80000000
	streamflagsSRCDefaultQ       = 0x08000000
	bufferflagsSilent            = 0x2
	waveFormatPCM                = 1
	waveFormatIEEEFloat          = 3
	waveFormatExtensible         = 0xFFFE
	hnsBufferDuration      int64 = 200_000 // 20 ms
)

// Vtable slots.
const (
	slotRelease = 2
	// IMMDeviceEnumerator
	slotGetDefaultAudioEndpoint = 4
	// IMMDevice
	slotActivate = 3
	// IAudioClient
	slotInitialize   = 3
	slotGetMixFormat = 8
	slotStart        = 10
	slotStop         = 11
	slotGetService   = 14
	// IAudioCaptureClient
	slotGetBuffer         = 3
	slotReleaseBuffer     = 4
	slotGetNextPacketSize = 5
)

type comObject struct {
	vtbl *[64]uintptr
}

func (o *comObject) call(slot int, args ...uintptr) error {
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[slot], all...)
	if int32(r) < 0 {
		return fmt.Errorf("HRESULT 0x%08X", uint32(r))
	}
	return nil
}

func (o *comObject) release() {
	if o != nil {
		_ = o.call(slotRelease)
	}
}

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

// WASAPISource captures the default output device ("what you hear").
type WASAPISource struct{}

func (WASAPISource) Name() string { return "wasapi-loopback" }

func (WASAPISource) Run(ctx context.Context, sink func([]float32)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, _ := procCoInitializeEx.Call(0, 0 /*COINIT_MULTITHREADED*/); int32(r) < 0 {
		return fmt.Errorf("CoInitializeEx: 0x%08X", uint32(r))
	}
	defer procCoUninitialize.Call()

	var enum *comObject
	if r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); int32(r) < 0 {
		return fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): 0x%08X", uint32(r))
	}
	defer enum.release()

	var dev *comObject
	if err := enum.call(slotGetDefaultAudioEndpoint, eRender, eConsole, uintptr(unsafe.Pointer(&dev))); err != nil {
		return fmt.Errorf("GetDefaultAudioEndpoint: %w", err)
	}
	defer dev.release()

	var client *comObject
	if err := dev.call(slotActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&client))); err != nil {
		return fmt.Errorf("Activate(IAudioClient): %w", err)
	}
	defer func() { client.release() }()

	// Preferred: let the audio engine convert to float32 stereo 48 kHz.
	want := waveFormatEx{FormatTag: waveFormatIEEEFloat, Channels: 2, SamplesPerSec: 48000, BitsPerSample: 32, BlockAlign: 8, AvgBytesPerSec: 48000 * 8}
	var conv *resampler
	var srcFloat = true
	var srcBits = 32
	var block = 8
	err := client.call(slotInitialize, sharedMode, streamflagsLoopback|streamflagsAutoConvert|streamflagsSRCDefaultQ,
		uintptr(hnsBufferDuration), 0, uintptr(unsafe.Pointer(&want)), 0)
	if err != nil {
		// Fall back to the engine mix format and convert ourselves, on a fresh
		// IAudioClient (a failed Initialize may leave the old one unusable).
		client.release()
		client = nil
		if err := dev.call(slotActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&client))); err != nil {
			return fmt.Errorf("Activate(IAudioClient): %w", err)
		}
		var mix *waveFormatEx
		if err := client.call(slotGetMixFormat, uintptr(unsafe.Pointer(&mix))); err != nil {
			return fmt.Errorf("GetMixFormat: %w", err)
		}
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(mix)))
		srcBits = int(mix.BitsPerSample)
		block = int(mix.BlockAlign)
		srcFloat = mix.FormatTag == waveFormatIEEEFloat
		if mix.FormatTag == waveFormatExtensible && mix.CbSize >= 22 {
			sub := (*windows.GUID)(unsafe.Add(unsafe.Pointer(mix), 24))
			srcFloat = *sub == subtypeIEEEFloat
		}
		if err := client.call(slotInitialize, sharedMode, streamflagsLoopback, uintptr(hnsBufferDuration), 0, uintptr(unsafe.Pointer(mix)), 0); err != nil {
			return fmt.Errorf("IAudioClient.Initialize: %w", err)
		}
		conv = &resampler{inRate: int(mix.SamplesPerSec), channels: int(mix.Channels)}
	}

	var capture *comObject
	if err := client.call(slotGetService, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capture))); err != nil {
		return fmt.Errorf("GetService(IAudioCaptureClient): %w", err)
	}
	defer capture.release()

	if err := client.call(slotStart); err != nil {
		return fmt.Errorf("IAudioClient.Start: %w", err)
	}
	defer client.call(slotStop)

	var floats, out []float32
	for ctx.Err() == nil {
		var next uint32
		if err := capture.call(slotGetNextPacketSize, uintptr(unsafe.Pointer(&next))); err != nil {
			return fmt.Errorf("GetNextPacketSize: %w", err) // e.g. AUDCLNT_E_DEVICE_INVALIDATED
		}
		if next == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		var data *byte
		var frames, flags uint32
		var devPos, qpcPos uint64
		if err := capture.call(slotGetBuffer, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)),
			uintptr(unsafe.Pointer(&flags)), uintptr(unsafe.Pointer(&devPos)), uintptr(unsafe.Pointer(&qpcPos))); err != nil {
			return fmt.Errorf("GetBuffer: %w", err)
		}
		n := int(frames) * block
		floats = floats[:0]
		if flags&bufferflagsSilent != 0 || data == nil {
			floats = append(floats, make([]float32, n/(srcBits/8))...)
		} else {
			raw := unsafe.Slice(data, n)
			switch {
			case srcFloat && srcBits == 32:
				for i := 0; i+4 <= n; i += 4 {
					floats = append(floats, math.Float32frombits(binary.LittleEndian.Uint32(raw[i:])))
				}
			case srcBits == 16:
				for i := 0; i+2 <= n; i += 2 {
					floats = append(floats, float32(int16(binary.LittleEndian.Uint16(raw[i:])))/32768)
				}
			case srcBits == 24:
				for i := 0; i+3 <= n; i += 3 {
					v := int32(raw[i])<<8 | int32(raw[i+1])<<16 | int32(raw[i+2])<<24
					floats = append(floats, float32(v)/2147483648)
				}
			case srcBits == 32:
				for i := 0; i+4 <= n; i += 4 {
					floats = append(floats, float32(int32(binary.LittleEndian.Uint32(raw[i:])))/2147483648)
				}
			}
		}
		if err := capture.call(slotReleaseBuffer, uintptr(frames)); err != nil {
			return fmt.Errorf("ReleaseBuffer: %w", err)
		}
		if conv != nil {
			out = conv.process(floats, out[:0])
			sink(out)
		} else {
			sink(floats)
		}
	}
	return nil
}

// DefaultAudioSource returns the platform capture source.
func DefaultAudioSource() AudioSource { return WASAPISource{} }
