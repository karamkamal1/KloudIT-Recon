// Pipeline: the capture thread feeds the encoder, the encoder's output thread
// writes into the ring. Neither ever waits for recon-host.
#pragma once

#include <atomic>
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
    // --dump-nv12: writes the converted frame with id kDumpFrameId (or the
    // first one after it) to this file, raw NV12, then logs it.
    std::string dumpPath;
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

private:
    void captureLoop();
    void outputLoop();
    RateParams rate();
    void dump(const d3d::ConvertedFrame& f, uint64_t frameId);

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
};

}  // namespace recon
