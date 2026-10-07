// Frame pacing for the present-driven captures (DDA, AMD Direct Capture, WGC).
//
// Policy (docs/HELPER_PROTOCOL.md "Frame pacing"):
//  1. Capture follows presents: an image is taken when the desktop or the game
//     presents, never on a timer of our own (GUIDE B7).
//  2. Never more than `fps` frames per second: output slots are one frame
//     interval apart and every delivered frame uses one (the next slot is
//     max(slot, now) + interval). A frame may go out up to a quarter interval
//     before its slot, so present jitter around a matching refresh rate adds no
//     delay, while the long-run rate still cannot exceed `fps`.
//  3. Presents faster than `fps` (a 144 Hz game streamed at 120): the newest
//     image wins and goes out when its slot opens; older ones are dropped
//     before encoding.
//  4. Nothing new for idleRepeatMs (default 100 ms: Sunshine's default minimum
//     of 10 fps, video.cpp min_fps_factor): the last image is submitted again,
//     and again every idleRepeatMs, so the encoder's rate control, the network
//     path and the browser's stall detection never see a silent stream, and a
//     static desktop keeps sharpening. Repeats are flagged (stats "repeat").
#pragma once

#include <cstdint>

namespace recon {

class FramePacer {
public:
    struct Decision {
        enum Kind { DeliverPending, DeliverRepeat, Wait } kind = Wait;
        int64_t until = 0;  // Wait: when to decide again at the latest (QPC)
    };

    void reset(int64_t qpcFrequency, int fps, int idleRepeatMs, int64_t now);
    void setFps(int fps);
    // havePending: a new image is waiting; haveLast: an image was delivered before.
    Decision decide(int64_t now, bool havePending, bool haveLast) const;
    void delivered(int64_t now);

    int64_t period() const { return period_; }

private:
    int64_t freq_ = 1;
    int64_t period_ = 1;  // QPC ticks per frame, rounded up (never faster than fps)
    int64_t early_ = 0;   // how early a frame may use its slot
    int64_t idle_ = 0;    // repeat interval
    int64_t nextDue_ = 0;
    int64_t lastDelivered_ = 0;
};

}  // namespace recon
