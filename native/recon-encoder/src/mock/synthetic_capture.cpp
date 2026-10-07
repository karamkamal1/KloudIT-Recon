#include "mock/mock.hpp"

namespace recon {

SyntheticCapture::SyntheticCapture() { stop_ = CreateEventW(nullptr, TRUE, FALSE, nullptr); }

SyntheticCapture::~SyntheticCapture() {
    if (stop_) CloseHandle(stop_);
}

Status SyntheticCapture::init(const StartParams& p) {
    if (!stop_) return Status::Error("init_failed", "cannot create the stop event");
    std::lock_guard<std::mutex> lock(mu_);
    period_ = qpcFrequency() / p.fps;
    deadline_ = qpcNow();
    index_ = 0;
    width_ = static_cast<uint32_t>(p.width ? p.width : ReplayEncoder::kClipWidth);
    height_ = static_cast<uint32_t>(p.height ? p.height : ReplayEncoder::kClipHeight);
    return Status::Ok();
}

Next SyntheticCapture::next(CapturedFrame& out, int timeoutMs, Status&) {
    if (WaitForSingleObject(stop_, 0) == WAIT_OBJECT_0) return Next::Stopped;
    int64_t deadline;
    {
        std::lock_guard<std::mutex> lock(mu_);
        deadline = deadline_;
    }
    const int64_t limit = qpcNow() + int64_t(timeoutMs) * qpcFrequency() / 1000;
    if (deadline > limit) return timer_.sleepUntil(limit, stop_) ? Next::Timeout : Next::Stopped;
    if (!timer_.sleepUntil(deadline, stop_)) return Next::Stopped;

    std::lock_guard<std::mutex> lock(mu_);
    const int64_t now = qpcNow();
    out = CapturedFrame{};
    out.index = index_++;
    out.presentQpc = deadline_;  // the "game" presented exactly on its tick
    out.captureQpc = now;
    out.width = width_;
    out.height = height_;
    deadline_ += period_;
    // Fell more than two frames behind (debugger, suspended VM): resync
    // instead of bursting frames to catch up.
    if (now - deadline_ > 2 * period_) deadline_ = now + period_;
    return Next::Frame;
}

void SyntheticCapture::setFps(int fps) {
    if (fps <= 0) return;
    std::lock_guard<std::mutex> lock(mu_);
    period_ = qpcFrequency() / fps;
}

void SyntheticCapture::shutdown() {
    if (stop_) SetEvent(stop_);
}

}  // namespace recon
