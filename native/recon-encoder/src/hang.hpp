// Encoder hang detection (AMF, libavcodec, the mock; NVENC has its own, the
// same 2 s). A frame the encoder has not finished kEncoderHangMs after it was
// submitted means the encoder stopped (an encoder or firmware stall, a driver
// bug in some LTR / SVC combination), not that it is slow: the backend's
// receive() then fails with the fatal encode_failed of encoderHangError, the
// helper exits and recon-host replaces it. Nothing else would notice: the
// capture thread only sees encoder_busy (the encoder's input queue stays
// full), and recon-host has no watchdog for a live helper that sends no
// frames. A removed D3D11 device is reported as device_lost instead (the
// backends check it first, as NVENC does).
#pragma once

#include <atomic>
#include <cstdint>
#include <string>

#include "types.hpp"

namespace recon {

constexpr int64_t kEncoderHangMs = 2000;

// Whether a frame submitted at submitQpc (0 = none in the encoder) and still
// not finished at nowQpc means the encoder hangs.
inline bool encoderHung(int64_t submitQpc, int64_t nowQpc, int64_t qpcFrequency) {
    return submitQpc != 0 && nowQpc - submitQpc > kEncoderHangMs * qpcFrequency / 1000;
}

inline Status encoderHangError(const std::string& encoder, uint64_t frameId) {
    return Status::Error("encode_failed",
                         encoder + " did not finish frame " + std::to_string(frameId) + " within " + std::to_string(kEncoderHangMs) + " ms",
                         true);
}

// --test-stall-at=N (tests only; backends amf, lavc and mock): from frame id
// N on the encoder stops finishing frames, as a stalled one does (AMF: no
// QueryOutput for them, so its input queue fills; libavcodec: the encoder
// thread blocks before encoding frame N, as in a driver call that does not
// return; the mock: takes frame N and outputs nothing more), so that the hang
// detection and recon-host's restart of the helper can be checked. 0 = off.
inline std::atomic<uint64_t>& testStallAtRef() {
    static std::atomic<uint64_t> at{0};
    return at;
}
inline uint64_t testStallAt() { return testStallAtRef().load(std::memory_order_relaxed); }
inline void setTestStallAt(uint64_t frameId) { testStallAtRef().store(frameId); }

}  // namespace recon
