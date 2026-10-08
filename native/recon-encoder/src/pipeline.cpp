#include "pipeline.hpp"

#include "platform/platform.hpp"

namespace recon {

namespace {
constexpr int kPollMs = 100;  // how often the threads look at the stop flag when idle
constexpr uint64_t kDumpFrameId = 30;
}

void Pipeline::dump(const d3d::ConvertedFrame& f, uint64_t frameId) {
    dumped_ = true;
    std::vector<uint8_t> data;
    Status s = opt_.converter->readback(f, data);
    HANDLE file = s.ok ? CreateFileW(fromUtf8(opt_.dumpPath).c_str(), GENERIC_WRITE, 0, nullptr, CREATE_ALWAYS,
                                     FILE_ATTRIBUTE_NORMAL, nullptr)
                       : INVALID_HANDLE_VALUE;
    if (file != INVALID_HANDLE_VALUE) {
        DWORD wrote = 0;
        const bool ok = WriteFile(file, data.data(), DWORD(data.size()), &wrote, nullptr) && wrote == data.size();
        CloseHandle(file);
        if (ok) {
            logf(LogLevel::Info, "dumped frame %llu (%ux%u NV12) to %s", static_cast<unsigned long long>(frameId),
                 opt_.converter->width(), opt_.converter->height(), opt_.dumpPath.c_str());
            return;
        }
    }
    logf(LogLevel::Warn, "could not dump frame %llu to %s: %s", static_cast<unsigned long long>(frameId), opt_.dumpPath.c_str(),
         s.ok ? "write failed" : s.text.c_str());
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
    // Sequences (forceIdr): seqBase is the frame id of the latest sequence
    // start, whose barcode value is 0; idrServed counts the forceIdr requests
    // the last sequence start answered. A start stays pending until a frame
    // is submitted (not dropped before the encoder, not refused as busy).
    uint64_t seqBase = 1, idrServed = 0;
    bool firstPending = true;
    int64_t lastPoolWarn = 0, lastBusyWarn = 0;
    while (!stop_) {
        CapturedFrame frame;
        Status err;
        const Next r = cap_.next(frame, kPollMs, err);
        CaptureEvent ev;
        while (cap_.takeEvent(ev)) rep_.captureChanged(ev);
        switch (r) {
        case Next::Timeout:
            continue;
        case Next::Stopped:
            // Only stop() may end capture: anything else would leave a
            // started helper without frames and without an error.
            if (!stop_) rep_.fatal(Status::Error("capture_failed", std::string("capture ") + cap_.name() + " ended unexpectedly", true));
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
        info.frameId = nextId;
        info.presentQpc = frame.presentQpc;
        info.captureQpc = frame.captureQpc;
        info.repeat = frame.repeat;
        info.dirtyPct = frame.dirtyPct;
        const uint64_t idrWanted = idrRequests_.load();
        bool idr = idrWanted != idrServed;
        if (idr) {
            // Recorded by the backend and applied to the next submitted frame:
            // this one (or, should it be dropped or refused as busy, the next
            // capture, which asks again).
            Status fs = enc_.forceIdr();
            if (!fs.ok) {
                rep_.error(fs, "forceIdr");
                idrServed = idrWanted;  // do not repeat a refused request with every frame
                idr = false;
            }
        }
        info.seqStart = firstPending || idr;
        const uint64_t base = info.seqStart ? info.frameId : seqBase;
        EncoderFrame ef;
        ef.captured = &frame;
        if (opt_.converter && frame.texture) {
            d3d::ConvertedFrame cf;
            Status cs = opt_.converter->convert(frame.texture, frame.rotation, uint16_t(info.frameId - base), cf);
            if (!cs.ok) {
                cap_.release(frame);
                if (cs.code == "pool_exhausted") {
                    // The encoder still holds every converted frame: drop this
                    // capture before it gets a frame id (never wait for the encoder).
                    const int64_t now = qpcNow();
                    if (now - lastPoolWarn > qpcFrequency()) {
                        logf(LogLevel::Warn, "encoder is behind: dropping captured frames before encoding");
                        lastPoolWarn = now;
                    }
                    continue;
                }
                if (cs.fatal) {
                    rep_.fatal(cs);
                    return;
                }
                rep_.error(cs, "");
                continue;
            }
            ef.nv12 = cf.nv12 ? cf.nv12 : cf.y;  // planar test mode (stream.cpp): the luma plane stands in
            ef.hold = cf.hold;
            ef.poolIndex = cf.index;
            if (!opt_.dumpPath.empty() && !dumped_ && info.frameId >= kDumpFrameId) dump(cf, info.frameId);
        }
        if (opt_.beforeSubmit) opt_.beforeSubmit(info.frameId);
        info.submitQpc = qpcNow();
        Status s = enc_.submit(ef, info);
        ef = EncoderFrame{};  // the backend kept its own reference if it needs one
        cap_.release(frame);
        if (s.code == "encoder_busy") {
            // The encoder did not take the frame (its input queue is full, or
            // an idle repeat of an image it is still encoding): drop it without
            // using up the frame id, so no gap looks like a loss.
            const int64_t now = qpcNow();
            if (now - lastBusyWarn > qpcFrequency()) {
                logf(LogLevel::Warn, "encoder is behind: dropping a captured frame (%s)", s.text.c_str());
                lastBusyWarn = now;
            }
            continue;
        }
        ++nextId;
        if (s.ok && info.seqStart) {
            seqBase = info.frameId;
            firstPending = false;
            if (idr) idrServed = idrWanted;  // requests that arrived since are answered by the next frame
        }
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
            if (!stop_) rep_.fatal(Status::Error("encode_failed", std::string("encoder ") + enc_.name() + " stopped unexpectedly", true));
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
        st.repeat = f.info.repeat;
        st.dirtyPct = f.info.dirtyPct;
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
