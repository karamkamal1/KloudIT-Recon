// Capture and encoder backend interfaces.
//
// Threads (see pipeline.cpp):
//   control thread (main)  start / forceIdr / recover / setRate / setRoi / shutdown
//   capture thread         Capture::next() -> Backend::submit()
//   output thread          Backend::receive() -> ring (one output thread per encoder)
// So Backend control methods can run concurrently with submit() and receive():
// implementations must serialize internally (typically: record the request
// under a lock and apply it on the next submit(), which is what both AMF
// per-surface properties and NVENC per-picture flags need anyway).
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include "types.hpp"

namespace recon {

// --- Capture ---------------------------------------------------------------------

struct CapturedFrame {
    uint64_t index = 0;      // capture counter
    int64_t presentQpc = 0;  // when the content was presented (DDA LastPresentTime, AMD flip timestamp); 0 = unknown
    int64_t captureQpc = 0;  // when the capture API returned the frame
    uint32_t width = 0, height = 0;
    void* texture = nullptr;  // ID3D11Texture2D* for GPU captures, nullptr for the synthetic source
};

enum class Next { Frame, Timeout, Stopped, Error };

class Capture {
public:
    virtual ~Capture() = default;
    virtual const char* name() const = 0;
    virtual Status init(const StartParams& p) = 0;
    // Waits up to timeoutMs for the next frame (paced by the source: presents
    // for DDA/AMD Direct Capture, a timer for the synthetic source).
    virtual Next next(CapturedFrame& out, int timeoutMs, Status& err) = 0;
    // The pipeline is done with the frame (called right after Backend::submit,
    // which must take its own reference / copy of the texture).
    virtual void release(CapturedFrame&) {}
    virtual void setFps(int fps) { (void)fps; }
    // Unblocks next() for good. Safe to call from any thread.
    virtual void shutdown() = 0;
};

// --- Encoder ---------------------------------------------------------------------

// SubmitInfo travels with a frame through the encoder.
struct SubmitInfo {
    uint64_t frameId = 0;
    int64_t presentQpc = 0, captureQpc = 0, submitQpc = 0;
};

// EncodedFrame is one access unit / temporal unit. data stays valid until
// Backend::releaseOutput (it can point into a locked encoder bitstream buffer,
// so the output thread copies it into the ring exactly once).
struct EncodedFrame {
    SubmitInfo info;
    int64_t outputQpc = 0;
    uint32_t gen = 0;  // encoder generation inside this helper (bumped on re-init)
    bool key = false;  // IDR / key frame with parameter sets
    bool recovery = false;  // references only acknowledged frames (refFloor valid)
    uint64_t refFloor = 0;
    int32_t ltrSlot = -1;  // LTR slot this frame was marked into, -1 = none
    uint32_t temporalLayer = 0;
    uint32_t refLtrMask = 0;  // LTR slots this frame references
    uint32_t width = 0, height = 0;
    const uint8_t* data = nullptr;
    size_t size = 0;
    void* token = nullptr;  // backend-private (buffer to unlock)
};

class Backend {
public:
    virtual ~Backend() = default;
    virtual const char* name() const = 0;
    // Capabilities, valid before init(). Also lists the capture methods this
    // backend can encode from (Caps::capture, default first).
    virtual Caps caps() = 0;
    virtual Status init(const StartParams& p, Started& out) = 0;
    // Capture thread. Must not block on the output side.
    virtual Status submit(const CapturedFrame& frame, const SubmitInfo& info) = 0;
    // Output thread: waits up to timeoutMs for the next encoded frame.
    virtual Next receive(EncodedFrame& out, int timeoutMs, Status& err) = 0;
    virtual void releaseOutput(EncodedFrame&) {}
    // Next frame is an IDR / key frame.
    virtual Status forceIdr() = 0;
    // Frames from lostFromFrameId on were lost: NVENC invalidates them, AMF
    // references the LTR slot holding ackedLtrFrameId; otherwise an IDR.
    virtual Status recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) = 0;
    virtual Status setRate(const RateParams& r) = 0;
    virtual Status setRoi(const std::vector<RoiRect>& rects) = 0;
    // Unblocks receive() and releases encoder resources. Safe to call twice.
    virtual void shutdown() = 0;
};

// --- Registry (registry.cpp) ------------------------------------------------------

struct MockOptions {
    uint64_t errorAt = 0;  // report a non-fatal "mock_error" when this frame id is submitted
    uint64_t fatalAt = 0;  // fail fatally ("mock_fatal") when this frame id is submitted
};

struct BackendChoice {
    std::unique_ptr<Backend> backend;  // nullptr if nothing usable
    Caps caps;                         // always filled (with "unavailable" reasons)
};

// Selects the encoder backend ("auto" | "mock" | "amf" | "nvenc") and probes
// every real backend and capture method for the caps' "unavailable" list.
BackendChoice chooseBackend(const std::string& name, const MockOptions& mock);

// Creates the capture method `name` ("synthetic" | "dda" | "amd-direct" | "wgc").
std::unique_ptr<Capture> createCapture(const std::string& name, Status& err);

}  // namespace recon
