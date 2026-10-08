#include "codec/rfi.hpp"

#include <algorithm>
#include <limits>

namespace recon {

void RfiTracker::reset(int dpbSize) {
    std::lock_guard<std::mutex> lock(mu_);
    dpb_ = size_t(std::max(1, dpbSize));
    window_.clear();
    invalid_.clear();
    pending_.clear();
    seq_ = 0;
    lastKey_ = lastSubmitted_ = 0;
    stats_ = {};
}

void RfiTracker::resize(int dpbSize) {
    std::lock_guard<std::mutex> lock(mu_);
    dpb_ = size_t(std::max(1, dpbSize));
    while (window_.size() > dpb_) {
        invalid_.erase(window_.front());
        window_.pop_front();
    }
}

int RfiTracker::dpbSize() const {
    std::lock_guard<std::mutex> lock(mu_);
    return int(dpb_);
}

void RfiTracker::recover(uint64_t lostFrom) {
    std::lock_guard<std::mutex> lock(mu_);
    pending_.push_back({lostFrom, ++seq_});
    ++stats_.requests;
}

bool RfiTracker::pending() const {
    std::lock_guard<std::mutex> lock(mu_);
    return !pending_.empty();
}

RfiTracker::Plan RfiTracker::plan(uint64_t frameId, bool idrRequested) const {
    (void)frameId;
    std::lock_guard<std::mutex> lock(mu_);
    Plan p;
    p.idr = idrRequested;
    if (pending_.empty()) return p;
    uint64_t lost = std::numeric_limits<uint64_t>::max();
    for (const Pending& x : pending_) lost = std::min(lost, x.lostFrom);
    p.lostFrom = lost;
    p.covered = lastSubmitted_;
    p.seq = seq_;
    if (idrRequested) return p;
    // Nothing from before the last key frame is kept, and frames that were
    // never submitted cannot be invalidated: an IDR.
    if (lost == 0 || lost > lastSubmitted_ || lost <= lastKey_) {
        p.idr = true;
        return p;
    }
    // The newest frame before the loss that is still a valid reference.
    bool found = false;
    for (auto it = window_.rbegin(); it != window_.rend(); ++it) {
        if (*it < lost && !invalid_.count(*it)) {
            p.refFloor = *it;
            found = true;
            break;
        }
    }
    if (!found) {
        p.idr = true;  // every frame the encoder still keeps would be invalid
        return p;
    }
    p.recovery = true;
    for (uint64_t id : window_) {
        if (id >= lost && !invalid_.count(id)) p.invalidate.push_back(id);
    }
    return p;
}

void RfiTracker::push(uint64_t frameId) {
    window_.push_back(frameId);
    while (window_.size() > dpb_) {
        invalid_.erase(window_.front());
        window_.pop_front();
    }
}

void RfiTracker::submitted(uint64_t frameId, const Plan& p) {
    std::lock_guard<std::mutex> lock(mu_);
    if (p.idr) {
        if (p.lostFrom) ++stats_.idrFallbacks;
        window_.clear();
        invalid_.clear();
        lastKey_ = frameId;
        // A key frame is a decoder entry point after every earlier loss.
        std::erase_if(pending_, [&](const Pending& x) { return x.lostFrom < frameId; });
    } else if (p.recovery) {
        ++stats_.recoveries;
        for (uint64_t id : p.invalidate) {
            if (std::find(window_.begin(), window_.end(), id) != window_.end()) invalid_.insert(id);
            ++stats_.invalidated;
        }
        // Done: the requests the plan included, and later ones for frames it
        // invalidated anyway (this frame references only frames before them).
        std::erase_if(pending_, [&](const Pending& x) {
            return x.seq <= p.seq || (x.lostFrom >= p.lostFrom && x.lostFrom <= p.covered);
        });
    }
    push(frameId);
    lastSubmitted_ = std::max(lastSubmitted_, frameId);
}

void RfiTracker::unplannedKey(uint64_t frameId) {
    std::lock_guard<std::mutex> lock(mu_);
    if (frameId <= lastKey_) return;
    lastKey_ = frameId;
    while (!window_.empty() && window_.front() < frameId) {
        invalid_.erase(window_.front());
        window_.pop_front();
    }
    std::erase_if(pending_, [&](const Pending& x) { return x.lostFrom < frameId; });
}

RfiTracker::Stats RfiTracker::stats() const {
    std::lock_guard<std::mutex> lock(mu_);
    return stats_;
}

}  // namespace recon
