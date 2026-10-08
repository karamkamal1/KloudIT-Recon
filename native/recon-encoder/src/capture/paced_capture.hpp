// PacedCapture: the common next() of the present-driven captures (DDA, AMD
// Direct Capture, WGC). Subclasses only wait for and keep images; when an
// image goes out, how often, and when the last one is repeated is decided here
// by FramePacer (capture/pacer.hpp). It also raises the timer resolution while
// the capture exists (TimerResolution) and turns a removed D3D11 device into
// the fatal device_lost while no frames arrive (a removed device stops
// presents and frame events without failing every capture API).
#pragma once

#include <deque>
#include <mutex>

#include "backend.hpp"
#include "capture/pacer.hpp"
#include "platform/platform.hpp"

namespace recon {

class PacedCapture : public Capture {
public:
    PacedCapture();
    ~PacedCapture() override;

    Next next(CapturedFrame& out, int timeoutMs, Status& err) final;
    void setFps(int fps) override;
    bool takeEvent(CaptureEvent& ev) override;
    void shutdown() override;

protected:
    struct Acquired {
        int64_t presentQpc = 0;  // 0 = unknown
        int64_t captureQpc = 0;
        int dirtyPct = -1;
    };

    // Waits up to timeoutMs (0 = just look) for a new image. On Frame the
    // subclass has stored it as its pending image (replacing an older pending
    // one, which is then dropped). Capture thread only.
    virtual Next acquire(int timeoutMs, Acquired& a, Status& err) = 0;
    // The pending image becomes the current one (it is being delivered).
    virtual void promote() = 0;
    // Fills texture, rotation, size (and AMF surface) of the current image.
    virtual void describe(CapturedFrame& out) = 0;

    // Called by init(): resets the pacer for the stream's fps and repeat
    // interval; device is the capture's D3D11 device (checked for removal).
    void startPacing(const StartParams& p, ID3D11Device* device);
    void postEvent(CaptureEvent ev);
    bool stopping() const { return WaitForSingleObject(stop_, 0) == WAIT_OBJECT_0; }
    HANDLE stopEvent() const { return stop_; }
    // Sleeps until the QPC deadline or until shutdown (returns false).
    bool sleepUntil(int64_t deadlineQpc) { return timer_.sleepUntil(deadlineQpc, stop_); }

private:
    // A removed device_ (only checked when no new image comes: before a
    // repeat and when next() times out, i.e. at most every idleRepeatMs).
    bool deviceLost(Status& err);

    TimerResolution timerResolution_;  // 1 ms ticks for every wait of the capture thread
    PreciseTimer timer_;
    ID3D11Device* device_ = nullptr;
    HANDLE stop_ = nullptr;  // manual-reset
    std::mutex mu_;          // pacer_ (setFps from the control thread), events_
    FramePacer pacer_;
    std::deque<CaptureEvent> events_;
    bool pending_ = false, haveLast_ = false, awake_ = false;
    Acquired pendingInfo_;
    uint64_t index_ = 0;
};

}  // namespace recon
