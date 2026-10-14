// Pipeline: the capture thread feeds the encoder, the encoder's output thread
// writes into the ring. Neither ever waits for recon-host.
#pragma once

#include <atomic>
#include <condition_variable>
#include <deque>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <string_view>
#include <thread>

#include "backend.hpp"
#include "d3d/convert.hpp"
#include "ring.hpp"

namespace recon {

// Reporter sends messages to recon-host (implemented in main.cpp).
class Reporter {
public:
    virtual ~Reporter() = default;
    virtual void stats(const FrameStats& s) = 0;
    virtual void error(const Status& s, std::string_view re) = 0;
    // Sends the fatal error and makes the main thread shut the helper down.
    virtual void fatal(const Status& s) = 0;
    virtual void captureChanged(const CaptureEvent& ev) = 0;
};

struct PipelineOptions {
    // Converts every GPU frame to NV12 before Backend::submit (InputSpec::Nv12).
    std::unique_ptr<d3d::Nv12Converter> converter;
    // The converter makes separate planes (no NV12 / P010 render targets) for
    // a backend that reads nothing (the mock, the NVENC test double): its
    // EncoderFrame::nv12 is the luma plane (stream.cpp).
    bool lumaStandsIn = false;
    // --dump-nv12: writes the converted frame with id kDumpFrameId (or the
    // first one after it) to this file, raw NV12 (P010 in an HDR10 stream),
    // then logs it.
    std::string dumpPath;
    // Called on the capture thread right before a frame is submitted, with its
    // frame id (again with the same id after the encoder refused it as busy).
    // The encode test's rate schedule (step 3.6) calls setRate here, so a
    // change applies exactly from that frame on (backends apply a setRate at
    // the next submit).
    std::function<void(uint64_t frameId)> beforeSubmit;
};

class Pipeline {
public:
    Pipeline(Backend& enc, Capture& cap, RingWriter& ring, Reporter& rep, const RateParams& rate, PipelineOptions opt = {})
        : enc_(enc), cap_(cap), ring_(ring), rep_(rep), rate_(rate), opt_(std::move(opt)) {}
    ~Pipeline() { stop(); }
    Pipeline(const Pipeline&) = delete;
    Pipeline& operator=(const Pipeline&) = delete;

    void start();
    // Wakes capture and the encoder (Capture/Backend::shutdown) and joins both
    // threads. Idempotent. The caller destroys the backend and the capture,
    // which releases their resources, only after this.
    void stop();

    // setRate is recorded here (it shows up in every stats message) and
    // forwarded to the encoder and, for an fps change, to the capture.
    Status setRate(const RateParams& r);
    // forceIdr ("forceIdr" message): the next captured frame starts a new
    // sequence (SubmitInfo::seqStart, barcode value 0) and the capture thread
    // makes it an IDR (Backend::forceIdr right before submitting it), so the
    // frame recon-host starts a new stream generation on carries barcode 0.
    void forceIdr() { idrRequests_.fetch_add(1); }
    // Sets PipelineOptions::beforeSubmit; only before start().
    void setBeforeSubmit(std::function<void(uint64_t)> fn) { opt_.beforeSubmit = std::move(fn); }

private:
    // Per-frame stamps that come after Backend::submit took the frame's
    // SubmitInfo: when submit returned (capture thread) and when the GPU had
    // finished the frame's copy and conversion (GPU timing thread), read by
    // the output thread for the frame's stats. By frame id; fence: the
    // converter's fence value of this submission (a frame id is used again
    // when the encoder refused the frame as busy).
    struct Stamp {
        uint64_t frameId = 0, fence = 0;
        int64_t submitted = 0, gpuDone = 0;
    };
    static constexpr size_t kStamps = 256;

    void captureLoop();
    void outputLoop();
    // The GPU timing thread: waits for the converter's fence (gpuFence) to
    // reach each converted frame's value and stamps when it did. Only with a
    // converter that has a fence.
    void gpuLoop();
    RateParams rate();
    void dump(const d3d::ConvertedFrame& f, uint64_t frameId);
    Stamp& stamp(uint64_t frameId) { return stamps_[frameId % kStamps]; }

    Backend& enc_;
    Capture& cap_;
    RingWriter& ring_;
    Reporter& rep_;

    std::mutex rateMu_;
    RateParams rate_;
    PipelineOptions opt_;
    bool dumped_ = false;

    std::atomic<uint64_t> idrRequests_{0};  // forceIdr calls so far
    std::atomic<bool> stop_{false};
    std::thread capture_;
    std::thread output_;

    std::mutex stampMu_;  // stamps_
    Stamp stamps_[kStamps];
    std::mutex gpuMu_;  // gpuQueue_
    std::condition_variable gpuCv_;
    std::deque<std::pair<uint64_t, uint64_t>> gpuQueue_;  // (frame id, fence value) to wait for, oldest first
    std::thread gpu_;
};

}  // namespace recon
