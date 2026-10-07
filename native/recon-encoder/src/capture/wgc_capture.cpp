// Windows.Graphics.Capture (per-window capture, multi-GPU fallback). Step 3.1 stub.
// Step 3.2 implements it after validating its timing (GUIDE 3.2 (3)).
#include "probes.hpp"

namespace recon {

Probe probeWgcCapture() { return {false, "Windows.Graphics.Capture is not implemented yet (step 3.2)"}; }

std::unique_ptr<Capture> createWgcCapture(Status& err) {
    err = Status::Error("unavailable", probeWgcCapture().reason);
    return nullptr;
}

}  // namespace recon
