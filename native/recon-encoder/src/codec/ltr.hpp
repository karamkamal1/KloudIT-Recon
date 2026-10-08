// LtrTracker: the long-term reference (LTR) policy of ACK-based loss recovery
// (GUIDE 3.3 / 3.5), independent of the encoder API: which frame to mark into
// which LTR slot, which slot a recovery frame may reference, and what the
// slots hold, from the encoder's own per-frame output and recon-host's ACKs.
//
// Policy (GUIDE 3.5):
// - Every `interval` frames (default fps/10, ~100 ms) the next frame is marked
//   into a slot.
// - The slot holding the newest ACKed LTR is never overwritten until a newer
//   LTR has been ACKed. The other slots rotate (empty first, then the oldest).
//   A marked LTR that is not ACKed yet is not overwritten either until
//   `ackTimeout` has passed, so a round trip longer than the interval cannot
//   starve the ACKs (the mark rate then follows the round trip).
// - Recovery from a loss at frame L: the newest ACKed LTR F < L that the
//   encoder still holds; the next frame references only F's slot (AMF
//   FORCE_LTR_REFERENCE_BITFIELD = 1 << slot) and is flagged RECOVERY with
//   refFloor = F. None (or a key frame was submitted after F): an IDR.
// - A key frame clears every slot ("When we encode a key frame or switch frame,
//   all saved LTR slots will be cleared", AMF_Video_Encode_HEVC_API.md 2.2.8),
//   so a key frame is never marked; the frame after it is. So does an AV1
//   switch frame (Output::clearsSlots): the AMF backend turns their insertion
//   off, but one the encoder makes anyway must not leave the tracker believing
//   the slots still hold ACKed LTRs ("Referring to a LTR frame not existing in
//   LTR slot will generate an Intra only frame", AMF_Video_Encode_AV1_API.md).
//
// What a slot holds is taken from the encoder's output (AMF
// OUTPUT_MARKED_LTR_INDEX), not from the request, so a mark the encoder moved
// (SVC: only base-layer frames can be LTR) or dropped is tracked correctly.
//
// Temporal SVC (Config::layers > 1, GUIDE 5): "only base temporal layer
// pictures can be coded as LTR ... the request to mark the current picture as
// LTR would be delayed to the next base temporal layer picture"
// (AMF_Video_Encode_API.md, MARK_CURRENT_WITH_LTR_INDEX), and a recovery frame
// must be a base-layer frame too: FORCE_LTR_REFERENCE_BITFIELD only applies to
// the frame it is set on, and the next base-layer frame references the previous
// base-layer frame, so a recovery coded as an enhancement frame (which no
// frame references) would leave the next base frame predicted from a lost one.
// The tracker predicts each submitted frame's layer from its position since
// the last key frame in the hierarchical-P pattern (layerAt: with 2 layers
// even positions are the base layer) and plans marks and recoveries only on
// predicted base-layer frames; the encoder's reported layer (AMF
// OUTPUT_TEMPORAL_LAYER, or the bitstream's temporal id) re-synchronizes the
// prediction (an unplanned key frame restarts the encoder's pattern), and a
// recovery frame that came out in an enhancement layer is rejected (IDR).
// Thread-safe: plan/submitted run on the capture thread, output on the output
// thread, ack/recover on the control thread.
#pragma once

#include <cstdint>
#include <deque>
#include <mutex>
#include <optional>
#include <utility>
#include <vector>

namespace recon {

class LtrTracker {
public:
    struct Config {
        int slots = 0;            // LTR slots the encoder was configured with; 0 = LTR off
        int interval = 6;         // frames between marks
        int64_t ackTimeout = 0;   // how long an unACKed mark is kept from being overwritten (clock units)
        int layers = 1;           // temporal layers (SVC); marks and recoveries only on base-layer frames
    };

    // What to set on the next frame.
    struct Plan {
        int markSlot = -1;     // mark this frame into the slot (-1 = no)
        uint32_t refMask = 0;  // reference only these LTR slots (0 = the encoder's default references)
        bool recovery = false; // refers only to an acknowledged LTR (refFloor)
        uint64_t refFloor = 0;
        bool idr = false;      // encode as IDR / key frame
        int layer = 0;         // predicted temporal layer (0 without SVC)
    };

    // What the encoder did with a frame.
    struct Output {
        bool key = false;      // IDR / key frame
        bool intra = false;    // intra coded (key or intra-only): references nothing
        int markedSlot = -1;   // slot it was stored in, -1 = none
        uint32_t refMask = 0;  // LTR slots it referenced
        bool clearsSlots = false;  // not a key frame, but the encoder emptied every slot (AV1 switch frame)
        int temporalLayer = -1;    // the layer it was coded in, -1 = unknown
    };

    struct Stats {
        uint64_t marks = 0, acked = 0, ltrRecoveries = 0, idrFallbacks = 0, failedRecoveries = 0;
        uint64_t layerResyncs = 0;  // the encoder's layers differed from the prediction
    };

    // The temporal layer of the frame at position pos after a key frame (pos
    // 0) in the dyadic hierarchical-P pattern of `layers` layers: base layer
    // every 2^(layers-1) frames, the top layer on every odd position.
    static int layerAt(uint64_t pos, int layers);

    void reset(const Config& c);
    // A new mark interval (a frame-rate change keeps it at about 100 ms)
    // without forgetting the slots.
    void setInterval(int interval);
    bool enabled() const;
    int slotCount() const;

    // Capture thread: the plan for frame frameId (no state change).
    // idrRequested: a forced IDR is pending.
    Plan plan(uint64_t frameId, int64_t now, bool idrRequested);
    // Capture thread: frameId went into the encoder with this plan.
    void submitted(uint64_t frameId, const Plan& p, int64_t now);
    // Output thread, in output order. Returns false for a planned recovery
    // frame the encoder did not code from its LTR (neither intra nor
    // referencing the planned slot): it must not be flagged RECOVERY, and the
    // caller forces an IDR. now: for a mark the encoder made on its own.
    bool output(uint64_t frameId, const Output& o, const Plan& planned, int64_t now);
    // Control thread: the client decoded frameId.
    void ack(uint64_t frameId);
    // Control thread: frames from lostFrom on were lost. ackedLtr: recon-host's
    // own choice of an ACKed LTR frame, used if a slot still holds it. True:
    // the next frame recovers from an LTR; false: the caller forces an IDR.
    bool recover(uint64_t lostFrom, std::optional<uint64_t> ackedLtr);

    struct SlotView {
        uint64_t frameId = 0;  // 0 = empty
        bool acked = false;
    };
    std::vector<SlotView> slots() const;  // confirmed by the encoder's output
    Stats stats() const;

private:
    struct Slot {
        uint64_t frameId = 0;  // 0 = empty
        bool acked = false;
        int64_t markedAt = 0;
    };
    struct Mark {
        uint64_t frameId;
        int slot;
        int64_t at;
    };
    struct Effective {
        uint64_t frameId = 0;
        bool acked = false;
        int64_t at = 0;
    };

    Effective effective(int slot) const;  // the slot as the encoder will see it at the next frame
    int newestAckedSlot(uint64_t below) const;

    mutable std::mutex mu_;
    Config cfg_;
    std::vector<Slot> slots_;   // confirmed by output
    std::deque<Mark> inflight_; // marks submitted, output not seen yet
    uint64_t lastKey_ = 0;      // newest frame submitted as a key frame
    int sinceMark_ = 0;         // frames submitted since the last mark
    bool recoveryPending_ = false;
    int recoverySlot_ = -1;
    uint64_t recoveryFrame_ = 0, recoveryLostFrom_ = 0;
    // SVC layer prediction: the next frame's position after the last key
    // frame, the correction learnt from the encoder's output, and the positions
    // of the frames in the encoder.
    uint64_t pos_ = 0, phase_ = 0;
    std::deque<std::pair<uint64_t, uint64_t>> positions_;  // frame id, position
    Stats stats_;
};

}  // namespace recon
