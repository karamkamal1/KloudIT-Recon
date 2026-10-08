// DXGI output selection and the D3D11 device that capture, colour conversion
// and the encoder share (GUIDE 3.2).
#pragma once

#include <d3d11.h>
#include <dxgi1_6.h>
#include <wrl/client.h>

#include <atomic>
#include <mutex>
#include <string>
#include <vector>

#include "platform/platform.hpp"
#include "types.hpp"

namespace recon::d3d {

using Microsoft::WRL::ComPtr;

// "0x887A0026 (DXGI_ERROR_ACCESS_LOST)" for the codes capture runs into.
std::string hrText(HRESULT hr);

// DXGI_MODE_ROTATION -> clockwise degrees (0, 90, 180, 270).
int rotationDegrees(DXGI_MODE_ROTATION r);

// Every output of every adapter, in DXGI order (caps "outputs").
std::vector<OutputDesc> enumerateOutputs();

// The output's colour (IDXGIOutput6::GetDesc1): Windows HDR on or off, the
// panel's primaries and luminance. known = false where DXGI cannot tell
// (before Windows 10 1703, Wine).
DisplayColor displayColor(IDXGIOutput* output);

struct OutputRef {
    ComPtr<IDXGIAdapter1> adapter;
    ComPtr<IDXGIOutput> output;
    AdapterInfo adapterInfo;
    OutputDesc desc;
    std::wstring deviceName;  // DXGI_OUTPUT_DESC::DeviceName, stable across mode changes
};

// Picks the output a start message asks for: hmonitor, else adapterLuid +
// monitor, else output `monitor` of adapter 0. Error code "no_output".
Status selectOutput(const StartParams& p, OutputRef& out);
// Finds the output named deviceName on the adapter (after a mode change).
Status findOutput(const LUID& adapter, const std::wstring& deviceName, OutputRef& out);
// The output showing this HMONITOR.
Status outputForMonitor(uint64_t hmonitor, OutputRef& out);

struct Device {
    ComPtr<ID3D11Device> device;
    ComPtr<ID3D11DeviceContext> context;
    D3D_FEATURE_LEVEL level{};
};

// Creates the D3D11 device on `adapter` (capture must run on the adapter that
// owns the output, and the encoder uses the same device), with BGRA support,
// multithread protection (the encoder's threads share it), GPU thread priority
// 7 and maximum frame latency 1 (Sunshine display_base.cpp). warp = true
// creates a WARP device and adapter = nullptr the default hardware device
// (self-test). The debug layer is only requested in debug builds.
Status createDevice(IDXGIAdapter1* adapter, Device& out, bool warp = false);

// True when the device was removed (driver reset / TDR, driver update, GPU
// gone: ID3D11Device::GetDeviceRemovedReason fails); out is then the fatal
// "device_lost" error, its text starting with `what`. A removed device and
// every object created on it must be recreated ("Handle device removed
// scenarios in Direct3D 11"); here that is a new helper, which recon-host
// starts on the fatal error. Captures and the conversion call this whenever a
// D3D11 / DXGI / AMF call fails (and while no frames arrive), so a removed
// device never turns into endless retries or repeated non-fatal errors.
bool deviceRemoved(ID3D11Device* device, const std::string& what, Status& out);

// Keeps the DXGI Desktop Duplication calls of the DDA capture thread
// (AcquireNextFrame, ReleaseFrame, the frame metadata, DuplicateOutput) and
// NVENC's NvEncLockBitstream / NvEncUnlockBitstream on the encoder's output
// thread from running at the same time: "On Windows, when encode device type
// is DirectX, calling DXGI APIs like IDXGIOutputDuplication::AcquireNextFrame
// from the primary thread and NvEncLockBitstream / NvEncUnlockBitstream from
// secondary thread, can lead to suboptimal or undefined behavior. This is
// because NvEncLockBitstream can internally use the application's DirectX
// device." (NVENC Video Encoder API programming guide 13.0, 6.3 Threading
// Model). The DDA capture holds it for one short AcquireNextFrame slice at a
// time (capture/dda_capture.cpp), so the output thread waits at most a slice,
// as it already did for the device lock AcquireNextFrame holds. One helper
// runs one stream: a process-wide lock (std::lock_guard<d3d::DxgiGate>). Take
// it before any other lock.
//
// The same section of the guide names the settings for such applications
// (enableEncodeAsync 1, NV_ENC_LOCK_BITSTREAM::doNotWait 0, output not in
// video memory), which the NVENC backend uses; whether the gate is still
// needed with them is the A/B measurement of docs/VENDOR_NOTES.md 3.4. For it
// the encode test's --dxgi-gate=0 calls disable() before any thread starts:
// lock() / unlock() then do nothing. Never off in normal operation.
class DxgiGate {
public:
    void lock() {
        if (on_.load(std::memory_order_relaxed)) mu_.lock();
    }
    void unlock() {
        if (on_.load(std::memory_order_relaxed)) mu_.unlock();
    }
    void disable() { on_.store(false, std::memory_order_relaxed); }
    bool enabled() const { return on_.load(std::memory_order_relaxed); }

private:
    std::mutex mu_;
    std::atomic<bool> on_{true};
};
DxgiGate& dxgiGate();

}  // namespace recon::d3d
