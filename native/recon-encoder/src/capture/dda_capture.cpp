// DXGI Desktop Duplication capture (any vendor). Step 3.1 stub.
//
// Step 3.2 implements it: IDXGIOutput5::DuplicateOutput1 -> AcquireNextFrame
// (follows presents), LastPresentTime as presentQpc, GPU copy into a pool
// texture, ReleaseFrame, recreate on DXGI_ERROR_ACCESS_LOST (GUIDE 3.2).
#include "probes.hpp"

namespace recon {

Probe probeDdaCapture() { return {false, "desktop duplication capture is not implemented yet (step 3.2)"}; }

std::unique_ptr<Capture> createDdaCapture(Status& err) {
    err = Status::Error("unavailable", probeDdaCapture().reason);
    return nullptr;
}

}  // namespace recon
