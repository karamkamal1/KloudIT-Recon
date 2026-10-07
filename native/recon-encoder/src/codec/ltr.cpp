#include "codec/ltr.hpp"

#include <algorithm>
#include <limits>

namespace recon {

void LtrTracker::reset(const Config& c) {
    std::lock_guard<std::mutex> lock(mu_);
    cfg_ = c;
    cfg_.slots = std::clamp(c.slots, 0, 32);
    cfg_.interval = std::max(1, c.interval);
    slots_.assign(size_t(cfg_.slots), Slot{});
    inflight_.clear();
    lastKey_ = 0;
    sinceMark_ = cfg_.interval;
    recoveryPending_ = false;
    recoverySlot_ = -1;
    recoveryFrame_ = recoveryLostFrom_ = 0;
    stats_ = {};
}

bool LtrTracker::enabled() const {
    std::lock_guard<std::mutex> lock(mu_);
    return cfg_.slots > 0;
}

int LtrTracker::slotCount() const {
    std::lock_guard<std::mutex> lock(mu_);
    return cfg_.slots;
}

LtrTracker::Effective LtrTracker::effective(int slot) const {
    // A mark submitted after the newest key frame wins (it is encoded before
    // the next frame); a key frame submitted after the confirmed content clears it.
    for (auto it = inflight_.rbegin(); it != inflight_.rend(); ++it) {
        if (it->slot == slot && it->frameId > lastKey_) return {it->frameId, false, it->at};
    }
    const Slot& s = slots_[size_t(slot)];
    if (s.frameId > lastKey_) return {s.frameId, s.acked, s.markedAt};
    return {};
}

int LtrTracker::newestAckedSlot(uint64_t below) const {
    int best = -1;
    for (int i = 0; i < cfg_.slots; ++i) {
        const Slot& s = slots_[size_t(i)];
        if (!s.acked || s.frameId <= lastKey_ || s.frameId >= below) continue;
        // A mark into this slot is already on its way into the encoder: the
        // slot will no longer hold s.frameId when the next frame is encoded.
        const bool overwritten = std::any_of(inflight_.begin(), inflight_.end(), [&](const Mark& m) { return m.slot == i; });
        if (overwritten) continue;
        if (best < 0 || s.frameId > slots_[size_t(best)].frameId) best = i;
    }
    return best;
}

LtrTracker::Plan LtrTracker::plan(uint64_t frameId, int64_t now, bool idrRequested) {
    (void)frameId;
    std::lock_guard<std::mutex> lock(mu_);
    Plan p;
    if (cfg_.slots == 0) {
        p.idr = idrRequested;
        return p;
    }
    if (idrRequested) {
        p.idr = true;
    } else if (recoveryPending_) {
        const Slot& s = slots_[size_t(recoverySlot_)];
        const bool overwritten =
            std::any_of(inflight_.begin(), inflight_.end(), [&](const Mark& m) { return m.slot == recoverySlot_; });
        if (s.frameId == recoveryFrame_ && s.acked && recoveryFrame_ > lastKey_ && !overwritten) {
            p.recovery = true;
            p.refMask = 1u << recoverySlot_;
            p.refFloor = recoveryFrame_;
        } else {
            p.idr = true;  // the LTR is gone after all
        }
    }
    // Marks `interval` frames apart (sinceMark_ counts the frames submitted
    // since the last one). A key frame clears the slots: the frame after it is marked.
    if (p.idr || sinceMark_ + 1 < cfg_.interval) return p;

    const int keep = newestAckedSlot(std::numeric_limits<uint64_t>::max());
    int best = -1;
    uint64_t bestFrame = std::numeric_limits<uint64_t>::max();
    for (int i = 0; i < cfg_.slots; ++i) {
        if (i == keep || (p.recovery && i == recoverySlot_)) continue;
        const Effective e = effective(i);
        if (e.frameId == 0) {
            best = i;
            break;
        }
        if (!e.acked && now - e.at < cfg_.ackTimeout) continue;  // still waiting for its ACK
        if (e.frameId < bestFrame) {
            best = i;
            bestFrame = e.frameId;
        }
    }
    p.markSlot = best;
    return p;
}

void LtrTracker::submitted(uint64_t frameId, const Plan& p, int64_t now) {
    std::lock_guard<std::mutex> lock(mu_);
    if (cfg_.slots == 0) return;
    if (p.idr) {
        if (recoveryPending_) ++stats_.idrFallbacks;
        lastKey_ = frameId;
        sinceMark_ = cfg_.interval;
        recoveryPending_ = false;
    } else {
        ++sinceMark_;
    }
    if (p.recovery) {
        recoveryPending_ = false;
        ++stats_.ltrRecoveries;
    }
    if (p.markSlot >= 0 && p.markSlot < cfg_.slots) {
        inflight_.push_back({frameId, p.markSlot, now});
        sinceMark_ = 0;
        ++stats_.marks;
    }
}

bool LtrTracker::output(uint64_t frameId, const Output& o, const Plan& planned, int64_t now) {
    std::lock_guard<std::mutex> lock(mu_);
    if (cfg_.slots == 0) return true;
    int64_t markedAt = now;
    // Outputs come in submission order: marks of earlier frames that never
    // came out (flushed) are gone too.
    while (!inflight_.empty() && inflight_.front().frameId <= frameId) {
        if (inflight_.front().frameId == frameId) markedAt = inflight_.front().at;
        inflight_.pop_front();
    }
    if (o.key) {
        for (Slot& s : slots_) s = Slot{};
    }
    if (o.markedSlot >= 0 && o.markedSlot < cfg_.slots) slots_[size_t(o.markedSlot)] = Slot{frameId, false, markedAt};
    if (!planned.recovery) return true;
    const bool ok = o.intra || (o.refMask & planned.refMask) != 0;
    if (!ok) ++stats_.failedRecoveries;
    return ok;
}

void LtrTracker::ack(uint64_t frameId) {
    std::lock_guard<std::mutex> lock(mu_);
    for (Slot& s : slots_) {
        if (s.frameId == frameId && frameId != 0 && !s.acked) {
            s.acked = true;
            ++stats_.acked;
        }
    }
}

bool LtrTracker::recover(uint64_t lostFrom, std::optional<uint64_t> ackedLtr) {
    std::lock_guard<std::mutex> lock(mu_);
    if (cfg_.slots == 0) return false;
    const uint64_t lost = recoveryPending_ ? std::min(recoveryLostFrom_, lostFrom) : lostFrom;
    int slot = -1;
    if (ackedLtr && *ackedLtr < lost && *ackedLtr > lastKey_) {
        for (int i = 0; i < cfg_.slots; ++i) {
            if (slots_[size_t(i)].frameId == *ackedLtr) {
                slots_[size_t(i)].acked = true;  // recon-host saw its ACK
                slot = i;
            }
        }
        if (slot >= 0 && newestAckedSlot(*ackedLtr + 1) != slot) slot = -1;  // a mark is overwriting it
    }
    if (slot < 0) slot = newestAckedSlot(lost);
    if (slot < 0) {
        recoveryPending_ = false;
        ++stats_.idrFallbacks;
        return false;
    }
    recoveryPending_ = true;
    recoverySlot_ = slot;
    recoveryFrame_ = slots_[size_t(slot)].frameId;
    recoveryLostFrom_ = lost;
    return true;
}

std::vector<LtrTracker::SlotView> LtrTracker::slots() const {
    std::lock_guard<std::mutex> lock(mu_);
    std::vector<SlotView> out;
    for (const Slot& s : slots_) out.push_back({s.frameId, s.acked});
    return out;
}

LtrTracker::Stats LtrTracker::stats() const {
    std::lock_guard<std::mutex> lock(mu_);
    return stats_;
}

}  // namespace recon
