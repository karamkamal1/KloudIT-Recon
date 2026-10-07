// AMD Direct Capture (AMFDisplayCapture). Step 3.1 stub: it needs the AMF
// runtime, so it reports that first.
//
// Step 3.2 implements it: AMF_DISPLAYCAPTURE_MODE=WAIT_FOR_PRESENT, FRAMERATE
// (0,1), QueryOutput polling, FRAME_FLIP_TIMESTAMP as presentQpc, dirty rects
// (GUIDE 3.2).
#include "probes.hpp"

namespace recon {

Probe probeAmdDirectCapture() {
    Probe amf = probeAmf();
    if (amf.reason.find("not found") != std::string::npos) return {false, amf.reason};
    return {false, "AMD Direct Capture is not implemented yet (step 3.2)"};
}

std::unique_ptr<Capture> createAmdDirectCapture(Status& err) {
    err = Status::Error("unavailable", probeAmdDirectCapture().reason);
    return nullptr;
}

}  // namespace recon
