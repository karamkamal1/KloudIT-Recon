#include "d3d/device.hpp"

#include <d3d10.h>  // ID3D10Multithread

#include <cstdio>

namespace recon::d3d {

std::string hrText(HRESULT hr) {
    const char* name = nullptr;
    switch (static_cast<uint32_t>(hr)) {
    case 0x887A0001: name = "DXGI_ERROR_INVALID_CALL"; break;
    case 0x887A0004: name = "DXGI_ERROR_UNSUPPORTED"; break;
    case 0x887A0005: name = "DXGI_ERROR_DEVICE_REMOVED"; break;
    case 0x887A0006: name = "DXGI_ERROR_DEVICE_HUNG"; break;
    case 0x887A0007: name = "DXGI_ERROR_DEVICE_RESET"; break;
    case 0x887A0020: name = "DXGI_ERROR_DRIVER_INTERNAL_ERROR"; break;
    case 0x887A0022: name = "DXGI_ERROR_NOT_CURRENTLY_AVAILABLE"; break;
    case 0x887A0026: name = "DXGI_ERROR_ACCESS_LOST"; break;
    case 0x887A0027: name = "DXGI_ERROR_WAIT_TIMEOUT"; break;
    case 0x887A0028: name = "DXGI_ERROR_SESSION_DISCONNECTED"; break;
    case 0x887A002B: name = "DXGI_ERROR_ACCESS_DENIED"; break;
    case 0x80004001: name = "E_NOTIMPL"; break;
    case 0x80004002: name = "E_NOINTERFACE"; break;
    case 0x80004005: name = "E_FAIL"; break;
    case 0x80070005: name = "E_ACCESSDENIED"; break;
    case 0x8007000E: name = "E_OUTOFMEMORY"; break;
    case 0x80070057: name = "E_INVALIDARG"; break;
    default: break;
    }
    char buf[96];
    if (name) std::snprintf(buf, sizeof(buf), "0x%08lX (%s)", static_cast<unsigned long>(hr), name);
    else std::snprintf(buf, sizeof(buf), "0x%08lX", static_cast<unsigned long>(hr));
    return buf;
}

int rotationDegrees(DXGI_MODE_ROTATION r) {
    switch (r) {
    case DXGI_MODE_ROTATION_ROTATE90: return 90;
    case DXGI_MODE_ROTATION_ROTATE180: return 180;
    case DXGI_MODE_ROTATION_ROTATE270: return 270;
    default: return 0;  // IDENTITY, UNSPECIFIED
    }
}

namespace {

// Walks every adapter and output; fn returns true to stop.
template <typename Fn>
bool forEachOutput(Fn&& fn) {
    ComPtr<IDXGIFactory1> factory;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf())))) {
        return false;
    }
    int global = 0;
    ComPtr<IDXGIAdapter1> adapter;
    for (UINT a = 0; factory->EnumAdapters1(a, adapter.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++a) {
        DXGI_ADAPTER_DESC1 ad{};
        if (FAILED(adapter->GetDesc1(&ad))) continue;
        AdapterInfo info;
        bool described = false;
        ComPtr<IDXGIOutput> output;
        for (UINT o = 0; adapter->EnumOutputs(o, output.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++o) {
            DXGI_OUTPUT_DESC od{};
            if (FAILED(output->GetDesc(&od))) continue;
            if (!described) {
                info = describeAdapter(ad.VendorId, ad.AdapterLuid, ad.Description);
                described = true;
            }
            OutputRef ref;
            ref.adapter = adapter;
            ref.output = output;
            ref.adapterInfo = info;
            ref.deviceName = od.DeviceName;
            OutputDesc& d = ref.desc;
            d.index = global++;
            d.adapterIndex = static_cast<int>(a);
            d.outputIndex = static_cast<int>(o);
            d.adapterLuid = info.luid;
            d.adapterName = info.name;
            d.vendor = info.vendor;
            d.name = toUtf8(od.DeviceName);
            d.hmonitor = reinterpret_cast<uint64_t>(od.Monitor);
            d.x = od.DesktopCoordinates.left;
            d.y = od.DesktopCoordinates.top;
            d.width = od.DesktopCoordinates.right - od.DesktopCoordinates.left;
            d.height = od.DesktopCoordinates.bottom - od.DesktopCoordinates.top;
            d.rotation = rotationDegrees(od.Rotation);
            d.attached = od.AttachedToDesktop != FALSE;
            if (fn(ref)) return true;
        }
    }
    return false;
}

Status noOutput(std::string text) { return Status::Error("no_output", std::move(text)); }

Status attachedOr(const OutputRef& ref) {
    if (!ref.desc.attached) return noOutput("output " + ref.desc.name + " is not attached to the desktop");
    return Status::Ok();
}

}  // namespace

std::vector<OutputDesc> enumerateOutputs() {
    std::vector<OutputDesc> out;
    forEachOutput([&](const OutputRef& r) {
        out.push_back(r.desc);
        return false;
    });
    return out;
}

Status outputForMonitor(uint64_t hmonitor, OutputRef& out) {
    if (forEachOutput([&](const OutputRef& r) {
            if (r.desc.hmonitor != hmonitor) return false;
            out = r;
            return true;
        })) {
        return attachedOr(out);
    }
    char buf[64];
    std::snprintf(buf, sizeof(buf), "no DXGI output shows HMONITOR 0x%llx", static_cast<unsigned long long>(hmonitor));
    return noOutput(buf);
}

Status findOutput(const LUID& adapter, const std::wstring& deviceName, OutputRef& out) {
    if (forEachOutput([&](const OutputRef& r) {
            if (r.adapterInfo.luidValue.LowPart != adapter.LowPart || r.adapterInfo.luidValue.HighPart != adapter.HighPart ||
                r.deviceName != deviceName) {
                return false;
            }
            out = r;
            return true;
        })) {
        return attachedOr(out);
    }
    return noOutput("output " + toUtf8(deviceName.c_str()) + " is gone from adapter " + luidString(adapter));
}

Status selectOutput(const StartParams& p, OutputRef& out) {
    if (p.hmonitor) return outputForMonitor(p.hmonitor, out);
    LUID want{};
    const bool byLuid = !p.adapterLuid.empty();
    if (byLuid && !parseLuid(p.adapterLuid, want)) return Status::Error("bad_message", "adapterLuid must look like 0000abcd:00001234");
    bool adapterSeen = false;
    if (forEachOutput([&](const OutputRef& r) {
            const bool adapterMatch = byLuid ? r.adapterInfo.luidValue.LowPart == want.LowPart &&
                                                   r.adapterInfo.luidValue.HighPart == want.HighPart
                                             : r.desc.adapterIndex == 0;
            if (!adapterMatch) return false;
            adapterSeen = true;
            if (r.desc.outputIndex != p.monitor) return false;
            out = r;
            return true;
        })) {
        return attachedOr(out);
    }
    const std::string adapter = byLuid ? "adapter " + p.adapterLuid : "DXGI adapter 0";
    if (!adapterSeen) return noOutput(adapter + " has no outputs (or does not exist)");
    return noOutput(adapter + " has no output " + std::to_string(p.monitor));
}

Status createDevice(IDXGIAdapter1* adapter, Device& out, bool warp) {
    if (!warp) enableIncreaseBasePriority();  // SetGPUThreadPriority > 0 needs it
    UINT flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT;
#ifndef NDEBUG
    flags |= D3D11_CREATE_DEVICE_DEBUG;  // debug builds only; dropped below if the SDK layers are missing
#endif
    static const D3D_FEATURE_LEVEL levels[] = {D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1,
                                               D3D_FEATURE_LEVEL_10_0};
    const D3D_DRIVER_TYPE type = warp ? D3D_DRIVER_TYPE_WARP : adapter ? D3D_DRIVER_TYPE_UNKNOWN : D3D_DRIVER_TYPE_HARDWARE;
    IDXGIAdapter* a = warp ? nullptr : adapter;
    auto create = [&](UINT f, const D3D_FEATURE_LEVEL* lv, UINT n) {
        return D3D11CreateDevice(a, type, nullptr, f, lv, n, D3D11_SDK_VERSION, out.device.ReleaseAndGetAddressOf(),
                                 &out.level, out.context.ReleaseAndGetAddressOf());
    };
    HRESULT hr = create(flags, levels, 4);
    if (FAILED(hr) && (flags & D3D11_CREATE_DEVICE_DEBUG)) {
        flags &= ~static_cast<UINT>(D3D11_CREATE_DEVICE_DEBUG);
        hr = create(flags, levels, 4);
    }
    // Runtimes that do not know 11_1 reject the whole list (AMF's DeviceDX11.cpp sample retries without it).
    if (hr == E_INVALIDARG) hr = create(flags, levels + 1, 3);
    if (FAILED(hr)) return Status::Error("init_failed", "D3D11CreateDevice failed: " + hrText(hr));

    // AMF and NVENC call into the device from their own threads (AMF's
    // DeviceDX11.cpp sample sets this on the device it hands to InitDX11).
    ComPtr<ID3D10Multithread> mt;
    if (SUCCEEDED(out.device.As(&mt))) mt->SetMultithreadProtected(TRUE);

    if (!warp) {
        // Sunshine display_base.cpp: capture GPU thread priority 7 (needs
        // SeIncreaseBasePriorityPrivilege) and at most one queued frame.
        ComPtr<IDXGIDevice1> dxgi;
        if (SUCCEEDED(out.device.As(&dxgi))) {
            hr = dxgi->SetGPUThreadPriority(7);
            if (FAILED(hr)) logf(LogLevel::Warn, "SetGPUThreadPriority(7) failed: %s (run recon-host elevated)", hrText(hr).c_str());
            hr = dxgi->SetMaximumFrameLatency(1);
            if (FAILED(hr)) logf(LogLevel::Warn, "SetMaximumFrameLatency(1) failed: %s", hrText(hr).c_str());
        }
    }
    return Status::Ok();
}

bool deviceRemoved(ID3D11Device* device, const std::string& what, Status& out) {
    if (!device) return false;
    const HRESULT reason = device->GetDeviceRemovedReason();
    if (SUCCEEDED(reason)) return false;
    out = Status::Error("device_lost", what + ": the D3D11 device was removed: " + hrText(reason), true);
    return true;
}

std::mutex& dxgiGate() {
    static std::mutex gate;
    return gate;
}

}  // namespace recon::d3d
