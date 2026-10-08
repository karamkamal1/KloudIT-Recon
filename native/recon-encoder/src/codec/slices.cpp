#include "codec/slices.hpp"

namespace recon {

void SliceAssembler::reset() {
    buf_.clear();
    parts_ = 0;
    first_ = 0;
    id_ = -1;
}

void SliceAssembler::finish() {
    out_.swap(buf_);
    outParts_ = parts_;
    outFirst_ = first_;
    outId_ = id_;
    reset();
}

SliceAssembler::Result SliceAssembler::add(Part part, int64_t frameId, const uint8_t* data, size_t size, int64_t qpc) {
    Result r;
    // An unfinished frame is abandoned by a whole frame or a part of another one.
    const bool otherFrame = parts_ > 0 && frameId >= 0 && id_ >= 0 && frameId != id_;
    if (parts_ > 0 && (part == Part::Frame || otherFrame)) {
        r.droppedParts = parts_;
        ++droppedFrames_;
        reset();
    }
    if (parts_ == 0) first_ = qpc;
    if (id_ < 0) id_ = frameId;
    if (data && size) buf_.insert(buf_.end(), data, data + size);
    ++parts_;
    if (part != Part::Slice) {
        finish();
        r.complete = true;
    }
    return r;
}

}  // namespace recon
