// Encoder-independent bitstream and frame-layout helpers used by the encoder
// backends (no GPU, no vendor API): parameter sets on key frames, coded-size
// alignment and region-of-interest importance maps. Checked by
// --self-test-encoder (codec/selftest.cpp).
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "types.hpp"

namespace recon {

enum class Codec { H264, Hevc, Av1 };

bool parseCodec(const std::string& name, Codec& out);
const char* codecName(Codec c);

// Whether a key frame carries what a decoder needs to start from it:
// H.264 SPS + PPS, HEVC VPS + SPS + PPS (Annex-B access units), AV1 a
// sequence header OBU (a temporal unit in the low-overhead format, OBUs with
// size fields as AMF and NVENC write them).
bool hasParameterSets(Codec c, const uint8_t* data, size_t size);

// The access unit with the encoder's parameter sets (extradata: Annex-B
// SPS/PPS(/VPS), or an AV1 sequence header OBU) inserted where they belong:
// after a leading access unit delimiter (H.264/HEVC) or temporal delimiter
// OBU (AV1), else first. Used when an encoder leaves them out of a forced key
// frame. Returns an empty vector if the extradata is not in the expected form.
std::vector<uint8_t> withParameterSets(Codec c, const uint8_t* data, size_t size, const uint8_t* extra, size_t extraSize);

// What the first sequence parameter set in Annex-B data (an encoder's
// parameter sets, or a key frame) says about the stream: its level and the
// reference frames a decoder keeps for it. H.264 (7.3.2.1.1): level_idc and
// max_num_ref_frames. HEVC (7.3.2.2.1): general_level_idc and
// sps_max_dec_pic_buffering_minus1 of the highest sub-layer (the DPB holds the
// current picture too, so this is the number of reference pictures besides
// it). False when there is no SPS or it cannot be parsed; always false for
// AV1, whose sequence header has no such field (eight reference slots).
struct SpsInfo {
    int levelIdc = 0;   // H.264 level_idc (51 = 5.1), HEVC general_level_idc (153 = 5.1)
    int refFrames = 0;
};
bool parseSps(Codec c, const uint8_t* data, size_t size, SpsInfo& out);
std::string levelText(Codec c, int levelIdc);  // "5.1"

inline uint32_t alignUp(uint32_t v, uint32_t a) { return a > 1 ? (v + a - 1) / a * a : v; }

// Temporal scalability as an access unit / temporal unit signals it (GUIDE 5
// temporal SVC: recon-host may leave out frames no other frame references).
struct LayerInfo {
    // temporal_id: H.264 the SVC prefix NAL unit (nal_unit_type 14,
    // nal_unit_header_svc_extension, H.7.3.1.1); HEVC nuh_temporal_id_plus1 - 1
    // of the first VCL NAL unit (7.3.1.2); AV1 the obu_extension_header of the
    // first frame / frame header / tile group OBU (5.3.3). -1 = not signalled.
    int temporalId = -1;
    // Whether later pictures may reference this one: H.264 nal_ref_idc of the
    // VCL NAL units (0 = non-reference picture); HEVC 0 for a sub-layer
    // non-reference picture (nal_unit_type TRAIL_N, TSA_N, STSA_N, RADL_N,
    // RASL_N, RSV_VCL_N10/12/14) at temporal id highestTemporalId (one at a
    // lower sub-layer may still be referenced by a higher one), else 1; AV1 -1
    // (its refresh_frame_flags sits deep in the frame header and is not parsed).
    int reference = -1;
};
LayerInfo layerInfo(Codec c, const uint8_t* data, size_t size, int highestTemporalId);

// Whether a frame can be left out without breaking the decoding of any other
// frame: never a key frame; where the bitstream says (LayerInfo::reference)
// that; otherwise the top temporal layer of an SVC stream (svcLayers > 1),
// which in the hierarchical-P structure of AMF and NVENC temporal SVC no frame
// references (AV1: VERIFY, docs/VENDOR_NOTES.md Phase 5).
bool isDiscardable(bool key, const LayerInfo& bits, int svcLayers, uint32_t temporalLayer);

// Region-of-interest importance map for encoders that take one value per
// block (AMF ROI_DATA: AMF_SURFACE_GRAY32, 64x64 blocks for HEVC/AV1, 16x16
// macroblocks for H.264; importance 0..10). The protocol's weights (-10..10)
// map to importance 5 + weight/2 (rounded half away from zero), so 0 is the
// background 5, +10 is 10 and -10 is 0; a block covered by several rects takes
// the highest importance. Rects are clipped to width x height.
struct RoiMap {
    uint32_t cols = 0, rows = 0;
    std::vector<uint32_t> values;  // rows * cols, row-major
};
RoiMap roiImportanceMap(uint32_t width, uint32_t height, uint32_t block, const std::vector<RoiRect>& rects);
constexpr uint32_t kRoiBackground = 5;
// Copies the map into a host-memory AMF_SURFACE_GRAY32 plane: row y at
// base + y * pitch, one 32-bit value per block (the AMF SimpleROI sample). The
// bytes between a row's end and the pitch are left alone. False if the pitch
// is smaller than a row.
bool writeRoiPlane(const RoiMap& m, uint8_t* base, size_t pitch);

}  // namespace recon
