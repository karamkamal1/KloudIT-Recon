// AMF encoder backend (AMD). Step 3.1 stub: probes the AMF runtime only.
//
// Step 3.3 implements it: AMFInit -> CreateContext -> InitDX11 on the capture
// device, AMFVideoEncoder_HEVC / AMFVideoEncoder_AV1 / AMFVideoEncoderVCE_AVC,
// GetCaps() -> Caps, per-surface FORCE_PICTURE_TYPE / LTR properties, and a
// QueryOutput loop on the output thread (GUIDE 3.3).
#include <cstdio>
#include <string_view>

#include <AMF/core/Factory.h>
#include <AMF/core/Version.h>

#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

static_assert(std::string_view(AMF_DLL_NAMEA) == "amfrt64.dll", "64-bit build expected (_M_AMD64)");

Probe probeAmf() {
    std::string err;
    HMODULE m = loadSystemLibrary(AMF_DLL_NAME, err);
    if (!m) return {false, std::string("AMF runtime (") + AMF_DLL_NAMEA + ") not found in System32: " + err};
    auto queryVersion = procAddress<AMFQueryVersion_Fn>(m, AMF_QUERY_VERSION_FUNCTION_NAME);
    auto init = procAddress<AMFInit_Fn>(m, AMF_INIT_FUNCTION_NAME);
    amf_uint64 v = 0;
    const bool haveVersion = queryVersion && queryVersion(&v) == AMF_OK;
    FreeLibrary(m);
    if (!queryVersion || !init) return {false, std::string(AMF_DLL_NAMEA) + " does not export AMFInit/AMFQueryVersion"};
    char ver[64] = "unknown version";
    if (haveVersion) {
        std::snprintf(ver, sizeof(ver), "%u.%u.%u.%u", unsigned(AMF_GET_MAJOR_VERSION(v)), unsigned(AMF_GET_MINOR_VERSION(v)),
                      unsigned(AMF_GET_SUBMINOR_VERSION(v)), unsigned(AMF_GET_BUILD_VERSION(v)));
    }
    return {false, std::string("AMF runtime ") + ver + " found; the AMF encoder backend is not implemented yet (step 3.3)"};
}

std::unique_ptr<Backend> createAmfBackend(Status& err) {
    err = Status::Error("unavailable", probeAmf().reason);
    return nullptr;
}

}  // namespace recon
