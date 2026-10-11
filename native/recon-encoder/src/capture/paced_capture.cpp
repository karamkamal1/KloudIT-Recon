#include "capture/paced_capture.hpp"

#include <algorithm>
#include <cstdio>
#include <string>

#include "capture/dirty.hpp"
#include "d3d/device.hpp"

namespace recon {

PacedCapture::PacedCapture() { stop_ = CreateEventW(nullptr, TRUE, FALSE, nullptr); }

PacedCapture::~PacedCapture() {
    if (stop_) CloseHandle(stop_);
}

void PacedCapture::startPacing(const StartParams& p, ID3D11Device* device) {
    std::lock_guard<std::mutex> lock(mu_);
    pacer_.reset(qpcFrequency(), p.fps, p.idleRepeatMs, qpcNow());
    firstImage_.reset(qpcFrequency(), qpcNow());
    pending_ = haveLast_ = false;
    device_ = device;
    if (timerResolution_.periodMs() > 1) logf(LogLevel::Info, "timer resolution: %u ms", timerResolution_.periodMs());
}

bool PacedCapture::deviceLost(Status& err) { return d3d::deviceRemoved(device_, std::string("capture ") + name(), err); }

void PacedCapture::setFps(int fps) {
    std::lock_guard<std::mutex> lock(mu_);
    pacer_.setFps(fps);
}

void PacedCapture::postEvent(CaptureEvent ev) {
    if (ev.width || ev.height) {
        logf(LogLevel::Info, "capture %s: %dx%d rotation %d %s", ev.reason.c_str(), ev.width, ev.height, ev.rotation, ev.text.c_str());
    } else {
        logf(LogLevel::Warn, "capture %s: %s", ev.reason.c_str(), ev.text.c_str());
    }
    std::lock_guard<std::mutex> lock(mu_);
    events_.push_back(std::move(ev));
}

void PacedCapture::noImage(int64_t ms) {
    // Wake the display as user input would: a one-off display request (the
    // continuous one of next() keeps a display on, it does not turn one
    // back on everywhere) and a mouse move of zero length (the pointer stays
    // where it is; it fails where the foreground takes no input from us).
    SetThreadExecutionState(ES_DISPLAY_REQUIRED);
    INPUT in{};
    in.type = INPUT_MOUSE;
    in.mi.dwFlags = MOUSEEVENTF_MOVE;
    const std::string woke = SendInput(1, &in, sizeof(in)) == 1
                                 ? "woke the display"
                                 : "could not wake the display (SendInput error " + std::to_string(GetLastError()) + ")";
    CaptureEvent ev;
    ev.reason = "noimage";
    ev.text = "no image from the capture for " + std::to_string(ms / 1000) + " s (display off or PC locked?); " + woke;
    postEvent(std::move(ev));
}

bool PacedCapture::takeEvent(CaptureEvent& ev) {
    std::lock_guard<std::mutex> lock(mu_);
    if (events_.empty()) return false;
    ev = std::move(events_.front());
    events_.pop_front();
    return true;
}

void PacedCapture::shutdown() {
    if (stop_) SetEvent(stop_);
}

Next PacedCapture::next(CapturedFrame& out, int timeoutMs, Status& err) {
    if (!awake_) {
        // Keep the display on while streaming: a display that goes to sleep
        // stops presents, and waking it re-inits capture (Sunshine
        // display_base.cpp does the same in its capture loop).
        SetThreadExecutionState(ES_CONTINUOUS | ES_DISPLAY_REQUIRED);
        awake_ = true;
    }
    const int64_t freq = qpcFrequency();
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq / 1000;
    bool waitedForSlot = false;
    for (;;) {
        if (stopping()) {
            SetThreadExecutionState(ES_CONTINUOUS);
            awake_ = false;
            return Next::Stopped;
        }
        const int64_t now = qpcNow();
        if (!haveLast_ && !pending_) {
            if (const int64_t ms = firstImage_.due(now)) noImage(ms);
        }
        FramePacer::Decision d;
        {
            std::lock_guard<std::mutex> lock(mu_);
            d = pacer_.decide(now, pending_, haveLast_);
        }
        if (d.kind == FramePacer::Decision::DeliverPending) {
            if (waitedForSlot) {
                // The image waited for its slot: take a newer one if the
                // source presented again meanwhile (newest wins).
                Acquired a;
                const Next r = acquire(0, a, err);
                if (r == Next::Error || r == Next::Stopped) return r;
                if (r == Next::Frame) {
                    const float dirty = mergeDirty(pendingInfo_.dirty, a.dirty);
                    pendingInfo_ = a;
                    pendingInfo_.dirty = dirty;
                }
            }
            promote();
            out = CapturedFrame{};
            describe(out);
            out.index = index_++;
            out.presentQpc = pendingInfo_.presentQpc;
            out.captureQpc = pendingInfo_.captureQpc;
            out.dirty = pendingInfo_.dirty;
            pending_ = false;
            if (!haveLast_) {
                if (const int64_t ms = firstImage_.image(now)) {
                    char took[32];
                    std::snprintf(took, sizeof took, "%.1f", double(ms) / 1000);
                    CaptureEvent ev;
                    ev.reason = "restored";
                    ev.width = int(out.width), ev.height = int(out.height), ev.rotation = out.rotation;
                    ev.text = std::string("first image after ") + took + " s";
                    postEvent(std::move(ev));
                }
            }
            haveLast_ = true;
            std::lock_guard<std::mutex> lock(mu_);
            pacer_.delivered(qpcNow(), false);
            return Next::Frame;
        }
        if (d.kind == FramePacer::Decision::DeliverRepeat) {
            // Nothing new for idleRepeatMs: also the moment to notice a removed
            // device, which stops presents (WGC) without failing a call.
            if (deviceLost(err)) return Next::Error;
            out = CapturedFrame{};
            describe(out);
            out.index = index_++;
            out.repeat = true;
            out.captureQpc = now;
            out.dirty = 0;
            std::lock_guard<std::mutex> lock(mu_);
            pacer_.delivered(now, true);
            return Next::Frame;
        }
        if (now >= deadline) return deviceLost(err) ? Next::Error : Next::Timeout;
        const int64_t until = std::min(d.until, deadline);
        if (pending_) {
            // Only the slot is missing: sleep precisely, without sitting in
            // the capture API (AcquireNextFrame holds the device lock).
            sleepUntil(until);
            waitedForSlot = true;
            continue;
        }
        const int ms = int(std::max<int64_t>(1, ((until - now) * 1000 + freq - 1) / freq));
        Acquired a;
        switch (acquire(ms, a, err)) {
        case Next::Frame:
            pending_ = true;
            pendingInfo_ = a;
            continue;
        case Next::Timeout: continue;
        case Next::Stopped: return Next::Stopped;
        case Next::Error: return Next::Error;
        }
    }
}

}  // namespace recon
