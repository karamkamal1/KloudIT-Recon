#include <algorithm>
#include <chrono>
#include <string>

#include "mock/mock.hpp"

// Generated from testdata/mock_clip.h264 by cmake/embed.cmake.
extern const unsigned char kMockClip[];
extern const std::size_t kMockClipSize;

namespace recon {

namespace {

bool hasNal(const uint8_t* p, size_t n, uint8_t type) {
    for (size_t i = 0; i + 3 < n; ++i) {
        if (p[i] == 0 && p[i + 1] == 0 && p[i + 2] == 1 && (p[i + 3] & 0x1f) == type) return true;
    }
    return false;
}

// Bytes of one frame at kbps and fps.
size_t frameBytes(int kbps, int fps) { return size_t(int64_t(kbps) * 1000 / 8 / std::max(1, fps)); }

// The access unit padded to `target` bytes with a filler data NAL unit (type
// 12, nal_ref_idc 0: 0xFF bytes and the RBSP stop bit, after the slice as H.264
// 7.4.1.2.3 allows; 0xFF never forms a start code). Owned by the caller.
std::vector<uint8_t>* padded(const uint8_t* au, size_t n, size_t target) {
    auto* out = new std::vector<uint8_t>(au, au + n);
    constexpr size_t kOverhead = 6;  // start code, NAL header, stop bit
    if (target > n + kOverhead) {
        const uint8_t head[] = {0, 0, 0, 1, 0x0c};
        out->insert(out->end(), head, head + sizeof(head));
        out->insert(out->end(), target - n - kOverhead, 0xff);
        out->push_back(0x80);
    }
    return out;
}

}  // namespace

std::vector<std::pair<size_t, size_t>> ReplayEncoder::splitAccessUnits(const uint8_t* data, size_t size) {
    std::vector<size_t> starts;
    for (size_t i = 0; i + 3 < size; ++i) {
        if (data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1) {
            if ((data[i + 3] & 0x1f) == 9) starts.push_back(i > 0 && data[i - 1] == 0 ? i - 1 : i);
            i += 2;
        }
    }
    std::vector<std::pair<size_t, size_t>> aus;
    for (size_t k = 0; k < starts.size(); ++k) {
        const size_t end = k + 1 < starts.size() ? starts[k + 1] : size;
        aus.emplace_back(starts[k], end - starts[k]);
    }
    return aus;
}

ReplayEncoder::ReplayEncoder(const MockOptions& opt) : opt_(opt) {
    aus_ = splitAccessUnits(kMockClip, kMockClipSize);
    if (aus_.size() != kClipFrames) {
        clipError_ = "mock clip has " + std::to_string(aus_.size()) + " access units, want " + std::to_string(kClipFrames);
        return;
    }
    const uint8_t* first = kMockClip + aus_[0].first;
    if (!hasNal(first, aus_[0].second, 7) || !hasNal(first, aus_[0].second, 8) || !hasNal(first, aus_[0].second, 5)) {
        clipError_ = "mock clip does not start with SPS + PPS + IDR";
        return;
    }
    for (size_t i = 1; i < aus_.size(); ++i) {
        if (hasNal(kMockClip + aus_[i].first, aus_[i].second, 5)) {
            clipError_ = "mock clip has more than one IDR";
            return;
        }
    }
}

ReplayEncoder::~ReplayEncoder() {
    for (EncodedFrame& f : queue_) releaseOutput(f);
}

Caps ReplayEncoder::caps() {
    Caps c;
    c.vendor = "mock";
    if (!clipError_.empty()) {
        c.unavailable.emplace_back("mock", clipError_);
        return c;
    }
    c.backend = "mock";
    CodecCaps h264;
    h264.maxW = kClipWidth;
    h264.maxH = kClipHeight;
    h264.forceIdr = true;
    h264.recovery = "none";
    h264.liveBitrate = "seamless";
    c.codecs["h264"] = h264;
    c.capture = {"synthetic"};
    return c;
}

Status ReplayEncoder::init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) {
    if (!clipError_.empty()) return Status::Error("unavailable", clipError_);
    if (p.codec != "h264") return Status::Error("unsupported", "the mock backend only encodes h264, not " + p.codec);
    in = InputSpec{};
    if (src.device) {
        in.format = InputSpec::Format::Nv12;
        in.width = uint32_t(p.width ? p.width : int(src.width)) & ~1u;
        in.height = uint32_t(p.height ? p.height : int(src.height)) & ~1u;
    }
    std::lock_guard<std::mutex> lock(mu_);
    if (initialized_) {
        // Only a start that failed after init() leads here (a running stream
        // answers already_started): stream.cpp must have called release().
        return Status::Error("init_failed", "mock: init() again without release() after a failed start (Backend contract)");
    }
    initialized_ = true;
    pos_ = 0;
    idrPending_ = false;
    stopped_ = false;
    for (EncodedFrame& f : queue_) releaseOutput(f);
    queue_.clear();
    kbps_ = lagKbps_ = p.kbps;
    fps_ = p.fps;
    lagLeft_ = 0;
    flush_ = p.liveBitrate == "flush";
    ratePending_ = false;
    gen_ = 0;
    out.backend = name();
    out.codec = "h264";
    out.width = kClipWidth;  // the canned stream has one size, whatever was asked
    out.height = kClipHeight;
    out.fps = p.fps;
    out.kbps = p.kbps;
    out.liveBitrate = flush_ ? "flush" : "seamless";
    out.rateControl = p.rc;
    return Status::Ok();
}

void ReplayEncoder::release() {
    std::lock_guard<std::mutex> lock(mu_);
    initialized_ = false;
    for (EncodedFrame& f : queue_) releaseOutput(f);
    queue_.clear();
}

Status ReplayEncoder::submit(const EncoderFrame&, const SubmitInfo& info) {
    if (opt_.hangAt && info.frameId == opt_.hangAt) {
        logf(LogLevel::Warn, "mock: hanging in submit at frame %llu", static_cast<unsigned long long>(info.frameId));
        for (;;) Sleep(INFINITE);
    }
    if (opt_.fatalAt && info.frameId == opt_.fatalAt) {
        return Status::Error("mock_fatal", "injected fatal error at frame " + std::to_string(info.frameId), true);
    }
    {
        std::lock_guard<std::mutex> lock(mu_);
        if (stopped_) return Status::Ok();
        if (ratePending_) {
            // A setRate applies to this frame (as AMF's SetProperty before
            // SubmitInput and NVENC's reconfiguration before the picture).
            ratePending_ = false;
            if (opt_.rateLag > 0) {
                if (lagLeft_ == 0) lagKbps_ = kbps_;  // frames keep the size they had for rateLag frames
                lagLeft_ = opt_.rateLag;
            }
            if (pendingRate_.kbps > 0) kbps_ = pendingRate_.kbps;
            if (pendingRate_.fps > 0) fps_ = pendingRate_.fps;
            pendingRate_ = RateParams{};
            if (flush_) ++gen_;
            if (flush_ || opt_.idrOnRate) idrPending_ = true;
        }
        if (idrPending_) {
            pos_ = 0;
            idrPending_ = false;
        }
        EncodedFrame e;
        e.info = info;
        e.key = pos_ == 0;
        e.gen = gen_;
        e.width = kClipWidth;
        e.height = kClipHeight;
        e.data = kMockClip + aus_[pos_].first;
        e.size = aus_[pos_].second;
        if (opt_.followRate) {
            const int kbps = lagLeft_ > 0 ? lagKbps_ : kbps_;
            if (lagLeft_ > 0) --lagLeft_;
            auto* buf = padded(e.data, e.size, frameBytes(kbps, fps_) * (e.key ? 3 : 1));
            e.data = buf->data();
            e.size = buf->size();
            e.token = buf;
        }
        pos_ = (pos_ + 1) % aus_.size();
        queue_.push_back(e);
    }
    cv_.notify_one();
    if (opt_.errorAt && info.frameId == opt_.errorAt) {
        return Status::Error("mock_error", "injected error at frame " + std::to_string(info.frameId));
    }
    return Status::Ok();
}

Next ReplayEncoder::receive(EncodedFrame& out, int timeoutMs, Status&) {
    std::unique_lock<std::mutex> lock(mu_);
    cv_.wait_for(lock, std::chrono::milliseconds(timeoutMs), [this] { return !queue_.empty() || stopped_; });
    if (stopped_) return Next::Stopped;
    if (queue_.empty()) return Next::Timeout;
    out = queue_.front();
    queue_.pop_front();
    out.outputQpc = qpcNow();
    return Next::Frame;
}

Status ReplayEncoder::forceIdr() {
    std::lock_guard<std::mutex> lock(mu_);
    idrPending_ = true;
    return Status::Ok();
}

Status ReplayEncoder::recover(uint64_t lostFromFrameId, std::optional<uint64_t>) {
    // No long-term references in a canned stream: recover with an IDR.
    logf(LogLevel::Debug, "recover from frame %llu: no LTR, sending an IDR",
         static_cast<unsigned long long>(lostFromFrameId));
    return forceIdr();
}

void ReplayEncoder::releaseOutput(EncodedFrame& f) {
    delete static_cast<std::vector<uint8_t>*>(f.token);
    f.token = nullptr;
}

Status ReplayEncoder::setRate(const RateParams& r) {
    std::lock_guard<std::mutex> lock(mu_);
    if (r.kbps > 0) pendingRate_.kbps = r.kbps;
    if (r.fps > 0) pendingRate_.fps = r.fps;
    ratePending_ = true;
    return Status::Ok();
}

Status ReplayEncoder::setRoi(const std::vector<RoiRect>&) { return Status::Ok(); }

void ReplayEncoder::shutdown() {
    {
        std::lock_guard<std::mutex> lock(mu_);
        stopped_ = true;
    }
    cv_.notify_all();
}

}  // namespace recon
