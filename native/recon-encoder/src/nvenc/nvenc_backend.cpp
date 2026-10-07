// NVENC encoder backend (NVIDIA). Step 3.1 stub: probes the driver's NVENC
// API only.
//
// Step 3.4 implements it: NvEncodeAPICreateInstance -> NvEncOpenEncodeSessionEx
// on the capture D3D11 device, ULTRA_LOW_LATENCY tuning, async mode with
// completion events drained by the output thread, NV_ENC_PIC_FLAG_FORCEIDR,
// NvEncInvalidateRefFrames for recovery, NvEncReconfigureEncoder for live
// bitrate (GUIDE 3.4).
#include <cstdio>

#include <ffnvcodec/nvEncodeAPI.h>

#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

namespace {
constexpr const wchar_t* kNvencDll = L"nvEncodeAPI64.dll";
using GetMaxSupportedVersionFn = NVENCSTATUS(NVENCAPI*)(uint32_t*);
}  // namespace

Probe probeNvenc() {
    std::string err;
    HMODULE m = loadSystemLibrary(kNvencDll, err);
    if (!m) return {false, "NVENC runtime (nvEncodeAPI64.dll) not found in System32: " + err};
    auto getMax = procAddress<GetMaxSupportedVersionFn>(m, "NvEncodeAPIGetMaxSupportedVersion");
    auto create = procAddress<void*>(m, "NvEncodeAPICreateInstance");
    uint32_t v = 0;
    const bool haveVersion = getMax && getMax(&v) == NV_ENC_SUCCESS;
    FreeLibrary(m);
    if (!getMax || !create) return {false, "nvEncodeAPI64.dll does not export the NVENC entry points"};
    if (!haveVersion) return {false, "NvEncodeAPIGetMaxSupportedVersion failed"};
    // The version is (major << 4) | minor.
    const unsigned major = v >> 4, minor = v & 0xf;
    const uint32_t required = (uint32_t(NVENCAPI_MAJOR_VERSION) << 4) | uint32_t(NVENCAPI_MINOR_VERSION);
    char text[200];
    if (v < required) {
        std::snprintf(text, sizeof(text), "the driver supports NVENC API %u.%u, the helper is built for %u.%u (update the driver)",
                      major, minor, unsigned(NVENCAPI_MAJOR_VERSION), unsigned(NVENCAPI_MINOR_VERSION));
    } else {
        std::snprintf(text, sizeof(text), "NVENC API %u.%u found; the NVENC encoder backend is not implemented yet (step 3.4)",
                      major, minor);
    }
    return {false, text};
}

std::unique_ptr<Backend> createNvencBackend(Status& err) {
    err = Status::Error("unavailable", probeNvenc().reason);
    return nullptr;
}

}  // namespace recon
