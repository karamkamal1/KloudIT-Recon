// Pipeline: the capture thread feeds the encoder, the encoder's output thread
// writes into the ring. Neither ever waits for recon-host.
#pragma once

#include <atomic>
#include <mutex>
#include <string_view>
#include <thread>

#include "backend.hpp"
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
};

class Pipeline {
public:
    Pipeline(Backend& enc, Capture& cap, RingWriter& ring, Reporter& rep, const RateParams& rate)
        : enc_(enc), cap_(cap), ring_(ring), rep_(rep), rate_(rate) {}
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

private:
    void captureLoop();
    void outputLoop();
    RateParams rate();

    Backend& enc_;
    Capture& cap_;
    RingWriter& ring_;
    Reporter& rep_;

    std::mutex rateMu_;
    RateParams rate_;

    std::atomic<bool> stop_{false};
    std::thread capture_;
    std::thread output_;
};

}  // namespace recon
