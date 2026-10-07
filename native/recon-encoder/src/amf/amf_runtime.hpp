// The AMF runtime (amfrt64.dll, shipped with the AMD driver), loaded once from
// System32 and kept loaded: AMD Direct Capture (3.2) and the AMF encoder
// backend (3.3) share its factory.
#pragma once

#include <string>

#include <AMF/core/Factory.h>

namespace recon {

struct AmfRuntime {
    amf::AMFFactory* factory = nullptr;  // nullptr: not available (error says why)
    amf_uint64 version = 0;              // AMFQueryVersion
    std::string versionText;             // "1.5.2.0"
    std::string error;
};

// Loads amfrt64.dll (LOAD_LIBRARY_SEARCH_SYSTEM32) and calls AMFInit on the
// first call; thread-safe.
const AmfRuntime& amfRuntime();

}  // namespace recon
