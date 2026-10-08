// Real capture methods and encoder backends behind factories: the capture
// methods (step 3.2), the AMF encoder (3.3) and the NVENC encoder (3.4).
#pragma once

#include <windows.h>

#include <memory>
#include <string>

#include "backend.hpp"

namespace recon {

struct Probe {
    bool available = false;
    std::string reason;  // why not (or what was found), for Caps::unavailable and logs
};

// GetProcAddress cast to a typed function pointer (via void* so GCC's
// -Wcast-function-type stays quiet).
template <typename Fn>
Fn procAddress(HMODULE m, const char* name) {
    return reinterpret_cast<Fn>(reinterpret_cast<void*>(GetProcAddress(m, name)));
}

// Encoders (amf/amf_backend.cpp, nvenc/nvenc_backend.cpp). probeAmf creates
// each AMF encoder once on the first AMD adapter and reads its caps, probeNvenc
// opens one NVENC session on the first NVIDIA adapter and reads every codec's
// caps (both cached for the process).
Probe probeAmf();
std::unique_ptr<Backend> createAmfBackend(Status& err);
Probe probeNvenc();
std::unique_ptr<Backend> createNvencBackend(Status& err);
// The NVENC caps from a new probe rather than the cached one (--self-test-nvenc
// changes its test double's capabilities between probes).
Caps probeNvencCaps();

// Capture (capture/*.cpp).
Probe probeDdaCapture();
std::unique_ptr<Capture> createDdaCapture(Status& err);
Probe probeAmdDirectCapture();
std::unique_ptr<Capture> createAmdDirectCapture(Status& err);
Probe probeWgcCapture();
std::unique_ptr<Capture> createWgcCapture(Status& err);
// Test source: a simulated game presenting into a D3D11 texture (capture/test_capture.cpp).
std::unique_ptr<Capture> createGpuTestCapture(Status& err);

}  // namespace recon
