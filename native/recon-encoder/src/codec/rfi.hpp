// RfiTracker: loss recovery by reference frame invalidation (NVENC
// NvEncInvalidateRefFrames; GUIDE 2.3 / 3.4 / 3.5), independent of the encoder
// API: which frames to invalidate before the next frame, whether a valid
// reference is left (else an IDR), and the recovery frame's refFloor.
//
// Encoder model: every frame is a reference frame (no B frames, no
// non-reference P frames) and only the previous frame is referenced
// (numRefL0 = 1), but the encoder keeps the last `dpbSize` reference frames
// (maxNumRefFrames) so that older ones are still there when newer ones are
// invalidated ("The low latency application which wants to invalidate
// reference frame as an error resilience tool is recommended to use a large
// DPB size so that the encoder can keep old reference frames which can be used
// if recent frames are invalidated", nvEncodeAPI.h NV_ENC_CONFIG_H264::
// maxNumRefFrames; Sunshine nvenc_base.cpp: one reference per frame, a larger
// DPB "for RFI fallback"). A key frame empties the DPB. Invalidated frames are
// never referenced again: "The encoder marks any reference frames or any frames
// which have been reconstructed using the corrupt frame as invalid for motion
// estimation and uses older reference frames ... forces the current frame to
// be encoded as an intra frame if no reference frames are left"
// (NvEncInvalidateRefFrames). The frame id is the inputTimeStamp that names a
// frame to NvEncInvalidateRefFrames.
//
// recover(L): frames from L on were lost (protocol "recover"). The next frame
// is planned as follows:
// - every submitted frame from L to the newest one is invalidated (Sunshine
//   invalidate_ref_frames extends the client's range to the last encoded frame:
//   everything after L was predicted from L);
// - the frame is flagged RECOVERY with refFloor = the newest frame before L
//   that is still a valid reference: L-1, or older when an earlier recovery
//   invalidated L-1 (that earlier range was lost too, so the encoder can only
//   use what came before it);
// - an IDR instead when no such frame is left in the DPB window (Sunshine: "rfi
//   request too large, generating IDR"), when L is at or before the last key
//   frame (nothing before a key frame is kept), or when L was never submitted.
//
// The window counts submitted frames, so it is conservative where the real
// DPB holds more (an invalidated frame that no longer takes a slot, SVC
// enhancement frames that are not references): then an IDR comes where the
// encoder could still have recovered, never a recovery frame without a valid
// reference.
//
// plan() does not change the state: if the frame does not reach the encoder
// (a failed submit), the next frame plans the same recovery again
// (NvEncInvalidateRefFrames "can be called multiple times"). A recover() that
// arrives between plan() and submitted() stays pending unless the submitted
// frame already covers it.
// Thread-safe: recover on the control thread, plan / submitted / resize on the
// capture thread, unplannedKey on the output thread.
#pragma once

#include <cstdint>
#include <deque>
#include <mutex>
#include <set>
#include <vector>

namespace recon {

class RfiTracker {
public:
    struct Plan {
        bool idr = false;       // encode as IDR (forced, or the recovery fallback)
        bool recovery = false;  // the frame references only frames before the loss (refFloor)
        uint64_t refFloor = 0;
        std::vector<uint64_t> invalidate;  // frame ids to invalidate before encoding the frame, oldest first
        // Bookkeeping for submitted().
        uint64_t lostFrom = 0;  // the loss this plan answers (0 = none)
        uint64_t covered = 0;   // newest submitted frame when planned (the end of the invalidated range)
        uint64_t seq = 0;       // recover() calls included
    };
    struct Stats {
        uint64_t requests = 0, recoveries = 0, idrFallbacks = 0, invalidated = 0;
    };

    // dpbSize: reference frames the encoder keeps (its maxNumRefFrames).
    void reset(int dpbSize);
    int dpbSize() const;
    // Capture thread: the encoder keeps dpbSize reference frames after all
    // (read back from its sequence parameter set): the window changes size
    // and keeps the frames, invalidations and pending losses it still covers.
    void resize(int dpbSize);

    // Control thread: frames from lostFrom on were lost.
    void recover(uint64_t lostFrom);
    bool pending() const;

    // Capture thread: what to do for frame frameId. idrRequested: a forced
    // IDR is pending (it wins over a recovery).
    Plan plan(uint64_t frameId, bool idrRequested) const;
    // Capture thread: frameId went into the encoder with this plan (the
    // caller may have turned a recovery into an IDR: plan.idr then).
    void submitted(uint64_t frameId, const Plan& p);
    // Output thread: frameId came out as a key frame nobody asked for.
    void unplannedKey(uint64_t frameId);

    Stats stats() const;

private:
    struct Pending {
        uint64_t lostFrom;
        uint64_t seq;
    };

    void push(uint64_t frameId);

    mutable std::mutex mu_;
    size_t dpb_ = 6;
    std::deque<uint64_t> window_;  // the last dpb_ submitted frames since the last key frame, oldest first
    std::set<uint64_t> invalid_;   // invalidated frames still in window_
    std::vector<Pending> pending_;
    uint64_t seq_ = 0;
    uint64_t lastKey_ = 0, lastSubmitted_ = 0;
    Stats stats_;
};

}  // namespace recon
