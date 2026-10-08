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

#include "platform/platform.hpp"
#include "types.hpp"

struct ID3D11Device;
struct ID3D11Texture2D;

namespace recon {

// --- Capture ---------------------------------------------------------------------

// CapturedFrame is one image from the capture. GPU captures hand out a D3D11
// texture on SourceInfo::device; it stays valid until Capture::release().
struct CapturedFrame {
    uint64_t index = 0;      // capture counter
    int64_t presentQpc = 0;  // when the content was presented (DDA LastPresentTime, AMD flip timestamp); 0 = unknown / repeat
    int64_t captureQpc = 0;  // when the capture API returned the frame (for a repeat: when it was re-submitted)
    uint32_t width = 0, height = 0;  // as displayed (after rotation)
    // Idle re-submit of the previous image: nothing new was presented for
    // StartParams::idleRepeatMs (see the pacing policy in capture/pacer.hpp).
    bool repeat = false;
    int dirtyPct = -1;  // share of the image that changed since the previous frame (dirty rects), -1 = unknown

    ID3D11Texture2D* texture = nullptr;  // nullptr for the synthetic source
    // Clockwise rotation (0/90/180/270) from the texture to the displayed
    // image (DXGI_OUTDUPL_DESC::Rotation): the converter rotates while it scales.
    int rotation = 0;
    // AMD Direct Capture: the amf::AMFSurface* the texture belongs to (a
    // reference held by the capture until release()). An AMF encoder on the
    // same AMFContext can take it directly (step 3.3), unless amfDcc: surfaces
    // with Delta Color Compression cannot go to the encoder as they are
    // (AMF_Display_Capture_API.md) and must be converted or copied first.
    void* amfSurface = nullptr;
    bool amfDcc = false;
};

// SourceInfo describes what a capture produces (valid after Capture::init).
struct SourceInfo {
    uint32_t width = 0, height = 0;  // as displayed
    int rotation = 0;
    ID3D11Device* device = nullptr;  // the device the textures live on; nullptr for the synthetic source
    AdapterInfo adapter;             // that device's adapter (found = false for the synthetic source)
    void* amfContext = nullptr;      // amf::AMFContext* of AMD Direct Capture surfaces, else nullptr
    int amfFormat = 0;               // their amf::AMF_SURFACE_FORMAT (AMF_DISPLAYCAPTURE_FORMAT), 0 = unknown
    bool cursorInVideo = false;      // frames contain the mouse pointer
};

// CaptureEvent (types.hpp) reports a source change, loss or recovery to recon-host.

enum class Next { Frame, Timeout, Stopped, Error };

class Capture {
public:
    virtual ~Capture() = default;
    virtual const char* name() const = 0;
    virtual Status init(const StartParams& p) = 0;
    virtual SourceInfo source() const = 0;
    // Waits up to timeoutMs for the next frame. GPU captures follow presents
    // and pace them to the requested fps (capture/pacer.hpp); the synthetic
    // source runs on a timer.
    virtual Next next(CapturedFrame& out, int timeoutMs, Status& err) = 0;
    // The pipeline is done with the frame (called right after Backend::submit,
    // which must take its own reference / copy of anything it keeps).
    virtual void release(CapturedFrame&) {}
    // Pops a pending source event (resized, lost, restored). Capture thread only.
    virtual bool takeEvent(CaptureEvent&) { return false; }
    virtual void setFps(int fps) { (void)fps; }
    // Unblocks next() for good. Safe to call from any thread. Like
    // Backend::shutdown it must not free what next() / release() use: capture
    // resources are released by the destructor.
    virtual void shutdown() = 0;
};

// --- Encoder ---------------------------------------------------------------------

// InputSpec is what an encoder backend wants from the pipeline (Backend::init
// fills it). With Nv12 the pipeline converts every GPU frame (BT.709 limited
// range, 4:2:0, scaled to width x height, optional barcode) into a pooled NV12
// texture on the capture device (d3d/convert.hpp); Native passes the capture's
// own image (the synthetic source, or AMD Direct Capture surfaces to an AMF
// encoder on the same context, step 3.3).
struct InputSpec {
    enum class Format { Native, Nv12 } format = Format::Native;
    uint32_t width = 0, height = 0;  // encoded size (even)
    // Nv12: the picture fills only the top-left contentWidth x contentHeight
    // of the texture (0 = all of it); the converter repeats the edge pixels
    // into the rest, padding the frame to a coded size the encoder needs (AV1
    // on RDNA3: multiples of 64x16). The barcode is drawn inside the content.
    uint32_t contentWidth = 0, contentHeight = 0;
};

// EncoderFrame is what Backend::submit gets.
struct EncoderFrame {
    const CapturedFrame* captured = nullptr;
    // InputSpec::Nv12: the converted frame, DXGI_FORMAT_NV12 on the capture
    // device. It stays reserved for the encoder as long as a copy of `hold`
    // exists: keep one until the encoder no longer reads the texture (AMF:
    // AMFSurfaceObserver::OnSurfaceDataRelease; NVENC: after the frame's output).
    ID3D11Texture2D* nv12 = nullptr;
    std::shared_ptr<void> hold;
    int poolIndex = -1;  // stable per texture (e.g. for NvEncRegisterResource caching)
};

// SubmitInfo travels with a frame through the encoder.
struct SubmitInfo {
    uint64_t frameId = 0;
    int64_t presentQpc = 0, captureQpc = 0, submitQpc = 0;
    bool repeat = false;
    int dirtyPct = -1;
    // The frame starts a new sequence (ring flag SEQ_START): the stream's first
    // frame, or the frame a forceIdr made an IDR (Pipeline::forceIdr calls
    // Backend::forceIdr right before submitting it). Its barcode value is 0.
    bool seqStart = false;
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
    // src describes the initialized capture (device, size); the backend fills
    // in (what it wants submitted) and out. Called again after a failed start.
    virtual Status init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) = 0;
    // The start failed after init() succeeded (stream.cpp: the colour
    // conversion could not be set up, say): release everything init() created
    // now, while the capture is still alive. An encoder may live on the
    // capture's own context (AMF on AMD Direct Capture's AMFContext), and the
    // capture is destroyed next. No thread has run; init() may follow again.
    virtual void release() {}
    // Capture thread. Must not block on the output side. Error code
    // "encoder_busy" (non-fatal): the frame was not taken (the encoder is
    // behind); the pipeline drops it without using up its frame id.
    virtual Status submit(const EncoderFrame& frame, const SubmitInfo& info) = 0;
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
    // recon-host's client decoded frameId (the "ack" message): backends with
    // long-term references use it to choose recovery references.
    virtual Status ack(uint64_t frameId) {
        (void)frameId;
        return Status::Ok();
    }
    // Unblocks receive() for good (later submit() calls return at once). Safe
    // to call twice and from any thread. Pipeline::stop() calls it BEFORE
    // joining the capture and output threads, which may still be inside
    // submit() / receive() or hold an EncodedFrame, so it must not free
    // anything they use: encoder resources are released by the destructor,
    // which runs only after both threads have been joined.
    virtual void shutdown() = 0;
};

// --- Registry (registry.cpp) ------------------------------------------------------

struct MockOptions {
    uint64_t errorAt = 0;  // report a non-fatal "mock_error" when this frame id is submitted
    uint64_t fatalAt = 0;  // fail fatally ("mock_fatal") when this frame id is submitted
    uint64_t hangAt = 0;   // never return from submit() for this frame id (a call stuck in the driver)
    // Live-bitrate qualification tests (step 3.6): frames padded with H.264
    // filler data to the target bitrate, so their sizes follow setRate
    // (rateLag frames late); idrOnRate makes every setRate an IDR, like an
    // encoder that cannot change its bitrate seamlessly.
    bool followRate = false;
    uint64_t rateLag = 0;
    bool idrOnRate = false;
};

struct BackendChoice {
    std::unique_ptr<Backend> backend;  // nullptr if nothing usable
    Caps caps;                         // always filled (with "unavailable" reasons)
};

// Selects the encoder backend ("auto" | "mock" | "amf" | "nvenc") and probes
// every real backend and capture method for the caps' "unavailable" list.
BackendChoice chooseBackend(const std::string& name, const MockOptions& mock);

// Creates the capture method `name` ("synthetic" | "dda" | "amd-direct" | "wgc" | "synthetic-gpu").
std::unique_ptr<Capture> createCapture(const std::string& name, Status& err);

}  // namespace recon
