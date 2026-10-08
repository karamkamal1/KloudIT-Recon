package vdisplay

import (
	"encoding/binary"
	"fmt"
	"time"
)

// SudoVDA control protocol: DeviceIoControl on the driver's device interface,
// METHOD_BUFFERED. Source: SudoMaker/SudoVDA Common/Include/sudovda-ioctl.h
// (protocol 0.2.1, identical in Apollo third-party/sudovda/sudovda-ioctl.h) and
// the driver's dispatcher SudoVDAIoDeviceControl (Driver.cpp):
//
//   - ADD (VIRTUAL_DISPLAY_ADD_PARAMS -> VIRTUAL_DISPLAY_ADD_OUT): creates a
//     monitor whose preferred mode is Width x Height at RefreshRate (Hz, or
//     millihertz when >= 1000) and returns the OS adapter LUID and target id
//     from IddCxMonitorArrival. A MonitorGuid that already exists returns
//     that monitor (idempotent); the GUID's Data1 and the strings go into the
//     generated EDID (serial number, monitor name: at most 13 characters).
//   - REMOVE (VIRTUAL_DISPLAY_REMOVE_PARAMS): IddCxMonitorDeparture of that
//     GUID's monitor; STATUS_NOT_FOUND when there is none.
//   - SET_RENDER_ADAPTER (LUID): IddCxAdapterSetRenderAdapter, the GPU that
//     renders the virtual monitors (call before adding one: Microsoft's
//     documentation says a change re-creates existing swapchains).
//   - GET_WATCHDOG -> {Timeout, Countdown} seconds; PING. Every IOCTL except
//     GET_WATCHDOG resets the countdown; when it reaches 0 the driver removes
//     every virtual monitor. Timeout comes from HKLM\SOFTWARE\SudoMaker\
//     SudoVDA\watchdog (default 3 s, 0 = off). Apollo pings every Timeout/3.
//   - GET_PROTOCOL_VERSION -> {Major, Minor, Incremental, TestBuild}. A client
//     works with a driver of the same major version and at least its minor
//     version (sudovda.h isProtocolCompatible).
//
// The device interface's security descriptor (SudoVDA.inf) grants Everyone
// read/write, so no elevation is needed.

// sudovdaInterface is SUVDA_INTERFACE_GUID {e5bcc234-1e0c-418a-a0d4-ef8b7501414d}.
var sudovdaInterface = guid{0xe5bcc234, 0x1e0c, 0x418a, [8]byte{0xa0, 0xd4, 0xef, 0x8b, 0x75, 0x01, 0x41, 0x4d}}

// guid is a Windows GUID (same layout as windows.GUID).
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// ctlCode is the CTL_CODE macro (winioctl.h).
func ctlCode(deviceType, function, method, access uint32) uint32 {
	return deviceType<<16 | access<<14 | function<<2 | method
}

const fileDeviceUnknown = 0x22

var (
	ioctlAddVirtualDisplay    = ctlCode(fileDeviceUnknown, 0x800, 0, 0)
	ioctlRemoveVirtualDisplay = ctlCode(fileDeviceUnknown, 0x801, 0, 0)
	ioctlSetRenderAdapter     = ctlCode(fileDeviceUnknown, 0x802, 0, 0)
	ioctlGetWatchdog          = ctlCode(fileDeviceUnknown, 0x803, 0, 0)
	ioctlDriverPing           = ctlCode(fileDeviceUnknown, 0x888, 0, 0)
	ioctlGetProtocolVersion   = ctlCode(fileDeviceUnknown, 0x8ff, 0, 0)
)

// Buffer sizes (x64 MSVC layouts of the header's structs).
const (
	sudovdaAddParamsSize = 56 // UINT Width, Height, RefreshRate; GUID MonitorGuid; CHAR DeviceName[14], SerialNumber[14]
	sudovdaAddOutSize    = 12 // LUID AdapterLuid; UINT TargetId
	sudovdaRemoveSize    = 16 // GUID MonitorGuid
	sudovdaLUIDSize      = 8  // VIRTUAL_DISPLAY_SET_RENDER_ADAPTER_PARAMS
	sudovdaWatchdogSize  = 8  // UINT Timeout, Countdown
	sudovdaVersionSize   = 4  // uint8 Major, Minor, Incremental; bool TestBuild
)

// sudovdaClient is the protocol version this client speaks (VDAProtocolVersion).
var sudovdaClient = sudovdaVersion{Major: 0, Minor: 2, Incremental: 1}

// sudovdaMonitorName is the EDID monitor name of the agent's virtual display.
const sudovdaMonitorName = "KloudIT Recon"

type sudovdaVersion struct {
	Major, Minor, Incremental uint8
	TestBuild                 bool
}

func (v sudovdaVersion) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Incremental)
	if v.TestBuild {
		s += "-test"
	}
	return s
}

// compatible applies sudovda.h's isProtocolCompatible to a driver version.
func (v sudovdaVersion) compatible() bool {
	return v.Major == sudovdaClient.Major && v.Minor >= sudovdaClient.Minor
}

func decodeSudovdaVersion(b []byte) (sudovdaVersion, error) {
	if len(b) < sudovdaVersionSize {
		return sudovdaVersion{}, fmt.Errorf("protocol version: %d bytes", len(b))
	}
	return sudovdaVersion{Major: b[0], Minor: b[1], Incremental: b[2], TestBuild: b[3] != 0}, nil
}

// putCString copies s into dst as a NUL-terminated string, truncated to
// len(dst)-1 bytes (strncpy(..., 13) into a zeroed CHAR[14], as Apollo does).
func putCString(dst []byte, s string) {
	n := copy(dst[:len(dst)-1], s)
	clear(dst[n:])
}

// encodeSudovdaAdd builds VIRTUAL_DISPLAY_ADD_PARAMS. The refresh rate is
// passed in millihertz.
func encodeSudovdaAdd(m Mode, id monitorID) []byte {
	b := make([]byte, sudovdaAddParamsSize)
	binary.LittleEndian.PutUint32(b[0:], uint32(m.Width))
	binary.LittleEndian.PutUint32(b[4:], uint32(m.Height))
	binary.LittleEndian.PutUint32(b[8:], uint32(m.Hz)*1000)
	copy(b[12:28], id.GUID[:])
	putCString(b[28:42], sudovdaMonitorName)
	putCString(b[42:56], id.Serial)
	return b
}

// decodeSudovdaAddOut parses VIRTUAL_DISPLAY_ADD_OUT.
func decodeSudovdaAddOut(b []byte) (Target, error) {
	if len(b) < sudovdaAddOutSize {
		return Target{}, fmt.Errorf("add display: %d bytes returned", len(b))
	}
	return Target{
		Adapter: LUID{Low: binary.LittleEndian.Uint32(b[0:]), High: int32(binary.LittleEndian.Uint32(b[4:]))},
		ID:      binary.LittleEndian.Uint32(b[8:]),
	}, nil
}

func encodeSudovdaRemove(id monitorID) []byte {
	b := make([]byte, sudovdaRemoveSize)
	copy(b, id.GUID[:])
	return b
}

func encodeLUID(l LUID) []byte {
	b := make([]byte, sudovdaLUIDSize)
	binary.LittleEndian.PutUint32(b[0:], l.Low)
	binary.LittleEndian.PutUint32(b[4:], uint32(l.High))
	return b
}

// decodeSudovdaWatchdog parses VIRTUAL_DISPLAY_GET_WATCHDOG_OUT.
func decodeSudovdaWatchdog(b []byte) (timeout, countdown uint32, err error) {
	if len(b) < sudovdaWatchdogSize {
		return 0, 0, fmt.Errorf("watchdog: %d bytes returned", len(b))
	}
	return binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint32(b[4:]), nil
}

// sudovdaPingEvery is how often to ping a watchdog of timeout seconds: a third
// of it (Apollo's startPingThread), 0 when the watchdog is off.
func sudovdaPingEvery(timeout uint32) time.Duration {
	if timeout == 0 {
		return 0
	}
	return time.Duration(timeout) * time.Second / 3
}
