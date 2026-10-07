// AMF encoder backend (AMD). Stub until step 3.3: probes the AMF runtime only
// (amf/amf_runtime.cpp loads it; AMD Direct Capture shares it).
//
// Step 3.3 implements it: AMFInit -> CreateContext -> InitDX11 on the capture
// device, AMFVideoEncoder_HEVC / AMFVideoEncoder_AV1 / AMFVideoEncoderVCE_AVC,
// GetCaps() -> Caps, per-surface FORCE_PICTURE_TYPE / LTR properties, and a
// QueryOutput loop on the output thread (GUIDE 3.3).
#include <cstdio>
#include <string_view>

#include <AMF/core/Factory.h>

#include "amf/amf_runtime.hpp"
#include "probes.hpp"

namespace recon {

static_assert(std::string_view(AMF_DLL_NAMEA) == "amfrt64.dll", "64-bit build expected (_M_AMD64)");

Probe probeAmf() {
    const AmfRuntime& rt = amfRuntime();
    if (!rt.factory) return {false, rt.error};
    return {false, "AMF runtime " + rt.versionText + " found; the AMF encoder backend is not implemented yet (step 3.3)"};
}

std::unique_ptr<Backend> createAmfBackend(Status& err) {
    err = Status::Error("unavailable", probeAmf().reason);
    return nullptr;
}

}  // namespace recon
