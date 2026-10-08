// SliceAssembler: puts a frame back together from the parts an encoder hands
// out one by one in sub-frame output mode (GUIDE 5 "sub-frame tile/slice
// output": AMF OUTPUT_MODE SLICE (H.264 / HEVC) / TILE (AV1), where each
// QueryOutput buffer is one slice / tile and OUTPUT_BUFFER_TYPE says FRAME,
// SLICE / TILE or SLICE_LAST / TILE_LAST). The helper still publishes whole
// frames; the assembler records when the first part came out, which is what a
// sub-frame transport could gain. No encoder API here; checked by
// --self-test-encoder.
//
// Rules: a FRAME part is a whole frame on its own. SLICE parts collect until
// a LAST part completes the frame. A FRAME part, or a part of another frame
// (the frame ids differ), while parts are collecting means the encoder never
// finished that frame: its parts are dropped (counted; the frame is lost, as
// incomplete data would corrupt decoding) and the new part starts over.
#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

namespace recon {

class SliceAssembler {
public:
    enum class Part { Frame, Slice, Last };

    struct Result {
        bool complete = false;  // frame() holds a whole frame now
        int droppedParts = 0;   // parts of an unfinished earlier frame thrown away by this add
    };

    // One part, in output order. frameId < 0: the part carries no frame id
    // (it then belongs to the frame being collected). qpc: when it came out.
    Result add(Part part, int64_t frameId, const uint8_t* data, size_t size, int64_t qpc);

    // The completed frame (valid after add() returned complete, until the next add()).
    const std::vector<uint8_t>& frame() const { return out_; }
    int parts() const { return outParts_; }
    int64_t firstQpc() const { return outFirst_; }
    int64_t frameId() const { return outId_; }  // the first part's id that had one, -1 = none

    bool collecting() const { return parts_ > 0; }
    void reset();
    uint64_t droppedFrames() const { return droppedFrames_; }

private:
    void finish();

    std::vector<uint8_t> buf_;
    int parts_ = 0;
    int64_t first_ = 0, id_ = -1;
    std::vector<uint8_t> out_;
    int outParts_ = 0;
    int64_t outFirst_ = 0, outId_ = -1;
    uint64_t droppedFrames_ = 0;
};

}  // namespace recon
