// Mock backend: a synthetic capture source and a "replay encoder" that plays a
// canned H.264 clip (testdata/mock_clip.h264, compiled in) frame by frame, so
// the helper, the ring and recon-host's client can be tested without a GPU.
#pragma once

#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <vector>

#include "backend.hpp"
#include "platform/platform.hpp"

namespace recon {

// SyntheticCapture produces empty frames (a frame counter) at the requested fps.
class SyntheticCapture : public Capture {
public:
    SyntheticCapture();
    ~SyntheticCapture() override;
    const char* name() const override { return "synthetic"; }
    Status init(const StartParams& p) override;
    SourceInfo source() const override;
    Next next(CapturedFrame& out, int timeoutMs, Status& err) override;
    void setFps(int fps) override;
    void shutdown() override;

private:
    PreciseTimer timer_;
    HANDLE stop_ = nullptr;  // manual-reset event
    std::mutex mu_;
    int64_t period_ = 0;    // QPC ticks per frame
    int64_t deadline_ = 0;  // QPC time of the next frame
    uint64_t index_ = 0;
    uint32_t width_ = 0, height_ = 0;
};

// ReplayEncoder outputs the canned clip: frame 0 is an IDR, 1..59 are P frames,
// and it loops back to the IDR. forceIdr (and recover, which has no LTR to use)
// jumps back to frame 0. setRate is accepted (recon-host sees it in the stats)
// but cannot change the canned bitstream. With a GPU capture (dda, amd-direct,
// wgc) it asks for NV12 input (P010 with hdr from an HDR source: started then
// describes an HDR10 stream although the canned one is 8-bit H.264), so
// capture and the colour conversion run for real on a host without an encoder
// backend; the converted frames are ignored.
// It enforces the init() / release() contract: an init() after a start that
// failed after init() succeeded fails unless release() was called in between.
class ReplayEncoder : public Backend {
public:
    explicit ReplayEncoder(const MockOptions& opt);
    const char* name() const override { return "mock"; }
    Caps caps() override;
    Status init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) override;
    void release() override;
    Status submit(const EncoderFrame& frame, const SubmitInfo& info) override;
    Next receive(EncodedFrame& out, int timeoutMs, Status& err) override;
    Status forceIdr() override;
    Status recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) override;
    Status setRate(const RateParams& r) override;
    Status setRoi(const std::vector<RoiRect>& rects) override;
    void shutdown() override;

    // Splits an Annex-B H.264 stream into access units at access unit
    // delimiters (NAL type 9). Exposed for the clip sanity check.
    static std::vector<std::pair<size_t, size_t>> splitAccessUnits(const uint8_t* data, size_t size);

    static constexpr int kClipWidth = 320;
    static constexpr int kClipHeight = 180;
    static constexpr size_t kClipFrames = 60;

private:
    MockOptions opt_;
    std::vector<std::pair<size_t, size_t>> aus_;  // offset, size into the clip
    std::string clipError_;

    std::mutex mu_;
    std::condition_variable cv_;
    std::deque<EncodedFrame> queue_;
    bool stopped_ = false;
    bool idrPending_ = false;
    bool initialized_ = false;  // init() succeeded, release() not called since
    size_t pos_ = 0;
};

}  // namespace recon
