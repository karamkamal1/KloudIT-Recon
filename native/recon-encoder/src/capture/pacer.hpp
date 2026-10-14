// Frame pacing for the present-driven captures (DDA, AMD Direct Capture, WGC).
//
// Policy (docs/HELPER_PROTOCOL.md "Frame pacing"):
//  1. Capture follows presents: an image is taken when the desktop or the game
//     presents, never on a timer of our own (GUIDE B7).
//  2. Never more than `fps` new images per second in the long run: output slots
//     are one frame interval apart and every delivered new image uses one (the
//     next slot is max(slot, now) + interval). A frame may go out up to half an
//     interval before its slot (a token bucket of 1.5 frames), so present
//     jitter around a matching refresh rate adds no delay and a source
//     presenting a little faster than `fps` keeps its own rhythm (rule 3),
//     while the long-run rate still cannot exceed `fps`.
//  3. Presents faster than `fps` (a 75 Hz display streamed at 60, a 144 Hz game
//     at 120): an image that comes before its slot waits for it, and a newer
//     one replaces it meanwhile (newest wins; older ones are dropped before
//     encoding). When the slot opens and the source's next present is due
//     less than half an interval later (its present interval measured over
//     the last presents), the waiting image is not sent late: the next present
//     goes out the moment it comes, and the waiting one only if that present
//     does not come in time (at most one interval after its own present). Up
//     to the second hardware run the waiting image went out when the slot
//     opened, so at 60 fps on a 75 Hz display every image waited for the
//     beat of the two rates: 9 ms p50 / 15.5 ms p95 between capture and
//     encoder (docs/VENDOR_NOTES.md "Second hardware run: in-game capture and
//     encoder latency"); with this rule four of five presents go out on
//     arrival and the fifth is left out, as before at 60 per second.
//  4. Nothing new for idleRepeatMs (default 100 ms: Sunshine's default minimum
//     of 10 fps, video.cpp min_fps_factor): the last image is submitted again,
//     and again every idleRepeatMs, so the encoder's rate control, the network
//     path and the browser's stall detection never see a silent stream, and a
//     static desktop keeps sharpening. Repeats are flagged (stats "repeat").
//     A repeat takes no output slot: the first new image after it goes out at
//     once. That image is the latency-critical one (the first change after an
//     idle period: a click or a keystroke on a static desktop); holding it for
//     the slot a repeat used would cost up to half an interval. Sunshine
//     likewise re-encodes the last image for its minimum fps without gating
//     the next captured frame on it. Repeats only come after
//     max(idleRepeatMs, one interval) without any delivery, so they cannot
//     raise the long-run rate above `fps` either.
#pragma once

#include <cstdint>

namespace recon {

class FramePacer {
public:
    struct Decision {
        enum Kind { DeliverPending, DeliverRepeat, Wait } kind = Wait;
        int64_t until = 0;  // Wait: when to decide again at the latest (QPC)
        // Wait with a pending image whose slot is open (rule 3): the source's
        // next present is due before `until`. Wait for it in the capture API
        // (a newer image is taken the moment it comes and goes out at once),
        // not asleep as for a slot that is not open yet.
        bool watch = false;
    };

    void reset(int64_t qpcFrequency, int fps, int idleRepeatMs, int64_t now);
    void setFps(int fps);
    // A new image came: presented at presentQpc (the capture time where the
    // present time is unknown), standing for `presents` presents of the source
    // (DDA's AccumulatedFrames: images the capture skipped since the last
    // one; 1 if unknown). It is the pending image now (replacing an older
    // pending one). Feeds the present-interval estimate of rule 3.
    void arrived(int64_t presentQpc, int presents = 1);
    // havePending: a new image is waiting; haveLast: an image was delivered before.
    Decision decide(int64_t now, bool havePending, bool haveLast) const;
    // An image went out at `now`; repeat: an idle re-submit (takes no slot).
    void delivered(int64_t now, bool repeat);

    int64_t period() const { return period_; }
    // The source's present interval (median of the last kIntervals, each up
    // to two frame intervals; a longer one starts the measurement again),
    // 0 = not known yet (fewer than three).
    int64_t presentInterval() const { return interval_; }

private:
    static constexpr int kIntervals = 8;

    int64_t freq_ = 1;
    int64_t period_ = 1;  // QPC ticks per frame, rounded up (never faster than fps)
    int64_t early_ = 0;   // how early a frame may use its slot
    int64_t idle_ = 0;    // repeat interval
    int64_t nextDue_ = 0;
    int64_t lastDelivered_ = 0;
    int64_t pendingAt_ = 0;    // present time of the pending image
    int64_t lastArrival_ = 0;  // present time of the image before it, 0 = none
    int64_t intervals_[kIntervals] = {};
    int nIntervals_ = 0, nextInterval_ = 0;
    int64_t interval_ = 0;
};

// FirstImageWatchdog: a capture that has handed out no image at all since it
// started. DDA hands out nothing while the display is off or asleep (the
// first hardware run's first session waited 54 s for its first image), and
// until the first image the pacer has nothing to repeat: the stream stays
// empty without an error. PacedCapture reports it (captureChanged "noimage",
// and wakes the display) kFirstMs after the start and every kEveryMs after
// that while it lasts, and the first image once one comes ("restored").
class FirstImageWatchdog {
public:
    static constexpr int64_t kFirstMs = 2000, kEveryMs = 10000;

    void reset(int64_t qpcFrequency, int64_t now);
    // A report due at now: the milliseconds since the start without an image
    // (0: none due, or an image came).
    int64_t due(int64_t now);
    // The first image went out at now: the milliseconds it took if it was
    // reported missing (0: it was not, or this is not the first image).
    int64_t image(int64_t now);

private:
    int64_t freq_ = 1, start_ = 0, next_ = 0;
    bool reported_ = false, done_ = false;
};

}  // namespace recon
