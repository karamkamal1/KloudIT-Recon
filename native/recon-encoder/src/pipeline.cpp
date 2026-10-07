#include "pipeline.hpp"

#include "platform/platform.hpp"

namespace recon {

namespace {
constexpr int kPollMs = 100;  // how often the threads look at the stop flag when idle
}

void Pipeline::start() {
    stop_ = false;
    output_ = std::thread([this] { outputLoop(); });
    capture_ = std::thread([this] { captureLoop(); });
}

void Pipeline::stop() {
    stop_ = true;
    // Only wakes the threads; nothing is freed until they have been joined.
    cap_.shutdown();
    enc_.shutdown();
    if (capture_.joinable()) capture_.join();
    if (output_.joinable()) output_.join();
}

RateParams Pipeline::rate() {
    std::lock_guard<std::mutex> lock(rateMu_);
    return rate_;
}

Status Pipeline::setRate(const RateParams& r) {
    Status s = enc_.setRate(r);
    if (!s.ok) return s;
    if (r.fps > 0) cap_.setFps(r.fps);
    std::lock_guard<std::mutex> lock(rateMu_);
    rate_.kbps = r.kbps;
    if (r.vbvFrames > 0) rate_.vbvFrames = r.vbvFrames;
    if (r.fps > 0) rate_.fps = r.fps;
    return s;
}

void Pipeline::captureLoop() {
    uint64_t nextId = 1;
    while (!stop_) {
        CapturedFrame frame;
        Status err;
        switch (cap_.next(frame, kPollMs, err)) {
        case Next::Timeout:
            continue;
        case Next::Stopped:
            return;
        case Next::Error:
            if (err.fatal) {
                rep_.fatal(err);
                return;
            }
            rep_.error(err, "");
            continue;
        case Next::Frame:
            break;
        }
        SubmitInfo info;
        info.frameId = nextId++;
        info.presentQpc = frame.presentQpc;
        info.captureQpc = frame.captureQpc;
        info.submitQpc = qpcNow();
        Status s = enc_.submit(frame, info);
        cap_.release(frame);
        if (!s.ok) {
            if (s.fatal) {
                rep_.fatal(s);
                return;
            }
            rep_.error(s, "");
        }
    }
}

void Pipeline::outputLoop() {
    while (!stop_) {
        EncodedFrame f;
        Status err;
        switch (enc_.receive(f, kPollMs, err)) {
        case Next::Timeout:
            continue;
        case Next::Stopped:
            return;
        case Next::Error:
            if (err.fatal) {
                rep_.fatal(err);
                return;
            }
            rep_.error(err, "");
            continue;
        case Next::Frame:
            break;
        }
        if (!f.outputQpc) f.outputQpc = qpcNow();
        const WriteResult wr = ring_.write(f);
        enc_.releaseOutput(f);
        if (wr == WriteResult::Corrupt) {
            rep_.fatal(Status::Error("ring", "ring counters are inconsistent (readCount beyond writeCount)", true));
            return;
        }

        FrameStats st;
        st.frameId = f.info.frameId;
        st.gen = f.gen;
        st.dropped = wr != WriteResult::Written;
        st.dropReason = wr == WriteResult::Full ? "ringFull" : wr == WriteResult::TooLarge ? "tooLarge" : "";
        st.key = f.key;
        st.recovery = f.recovery;
        st.bytes = f.size;
        st.presentQpc = f.info.presentQpc;
        st.captureQpc = f.info.captureQpc;
        st.submitQpc = f.info.submitQpc;
        st.outputQpc = f.outputQpc;
        st.refFloor = f.refFloor;
        st.ltrSlot = f.ltrSlot;
        st.temporalLayer = f.temporalLayer;
        st.refLtrMask = f.refLtrMask;
        st.rate = rate();
        st.ringDropped = ring_.droppedTotal();
        rep_.stats(st);
        if (wr == WriteResult::TooLarge) {
            rep_.error(Status::Error("frame_too_large", "frame " + std::to_string(f.info.frameId) + " (" +
                                                            std::to_string(f.size) + " bytes) does not fit a ring slot"),
                       "");
        }
    }
}

}  // namespace recon
