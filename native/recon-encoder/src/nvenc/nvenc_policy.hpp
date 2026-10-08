// NVENC settings that depend on the stream but not on the driver (GUIDE 3.4,
// 10): API version negotiation, the preset by pixel rate, rate-control values,
// the reference frames by level, the ROI QP delta map. No NVENC API calls
// here; checked by --self-test-encoder.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "codec/bitstream.hpp"
#include "types.hpp"

namespace recon::nvenc {

// An NVENC API version as NvEncodeAPIGetMaxSupportedVersion reports it:
// (major << 4) | minor.
constexpr uint32_t apiVersion(uint32_t major, uint32_t minor) { return (major << 4) | minor; }
std::string apiVersionText(uint32_t v);  // "13.0"

// The oldest Windows NVIDIA driver with NVENC API `v` (the table of FFmpeg
// nvenc.c nvenc_print_driver_requirement: 13.0 needs 570.0), or "" if unknown.
std::string minimumDriver(uint32_t v);

// Empty when a driver whose newest NVENC API is driverMax can run a helper
// built against API `built`; else why not, naming the driver to install. A
// driver also accepts every older API version, and the helper always asks for
// exactly the version it was built with (NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS::
// apiVersion = NVENCAPI_VERSION and the struct versions derived from it), so a
// newer driver is fine; FFmpeg nvenc_load_libraries makes the same check.
std::string versionProblem(uint32_t driverMax, uint32_t built);

// Preset P1..P7 for the stream (GUIDE 3.4 / 10: "P4 <= 1440p, toward P1 at
// 4K120"): P4 up to the pixel rate of 2560x1440 at 120 fps, P1 from 3840x2160
// at 120 fps on, in between by pixel rate (logarithmically). quality
// "balanced" / "quality": one / two presets slower, at most P7.
int presetFor(uint32_t width, uint32_t height, int fps, const std::string& quality);

// Rate-control values in bits (NV_ENC_RC_PARAMS): average = max = kbps (CBR
// ignores max; VBR is capped at the average), VBV = one frame's worth x
// vbvFrames ("Very low VBV buffer size (e.g. single frame = bitrate/framerate)",
// NVENC guide 9 for game streaming; Sunshine nvenc_base.cpp).
struct Rate {
    uint32_t average = 0, max = 0, vbv = 0;
};
Rate rateFor(int kbps, double vbvFrames, int fps);

// NV_ENC_RC_PARAMS::lowDelayKeyFrameScale: "the ratio of I frame bits to P
// frame bits in case of single frame VBV and CBR", default 1 for the
// ultra-low-latency tuning (an IDR no larger than a P frame: a blurry key
// frame that takes many frames to sharpen). GUIDE 3.4: an IDR at most about
// three average frames.
constexpr uint32_t kKeyFrameScale = 3;
// Reference frames kept (maxNumRefFrames / maxNumRefFramesInDPB, GUIDE 3.4:
// 4-6): the recovery window of reference frame invalidation (codec/rfi.hpp).
constexpr int kDpbFrames = 6;
// kDpbFrames, or fewer where the level 5.x DPB limit at the coded size is
// smaller: H.264 and HEVC 3840x2160 keep 5. More references than level 5.2
// allows would make the driver either signal level 6 (decoders limited to
// 5.1 / 5.2, typical for H.264 hardware decoders, refuse the stream) or keep
// fewer than the invalidation window assumes. H.264 A.3.1: MaxDpbFrames =
// Min(MaxDpbMbs / (PicWidthInMbs * FrameHeightInMbs), 16), level 5.1 / 5.2
// MaxDpbMbs 184320 (3840x2160 = 32400 macroblocks: 5). HEVC A.4.2: MaxDpbSize
// from MaxLumaPs 8912896 and maxDpbPicBuf 6 (3840x2160: 6), which counts the
// current picture (sps_max_dec_pic_buffering_minus1 + 1 <= MaxDpbSize), so one
// reference fewer. A picture larger than level 5 allows takes level 6's
// limits (it needs level 6 anyway). AV1: kDpbFrames (eight reference slots at
// every level). Sunshine keeps 5 for H.264 / HEVC and 8 for AV1
// (nvenc_base.cpp configure_reference_frames); FFmpeg h264_levels.c /
// h265_profile_level.c apply the same limits when they pick a level.
int dpbFramesFor(Codec c, uint32_t width, uint32_t height);

// ROI as a QP delta map (NV_ENC_RC_PARAMS::qpMapMode = NV_ENC_QP_MAP_DELTA):
// one signed byte per block, "per MB for H264, per CTB for HEVC and per SB for
// AV1" (NV_ENC_PIC_PARAMS::qpDeltaMap); NVENC codes HEVC with 32x32 CTBs and
// AV1 with 64x64 superblocks (OBS obs-nvenc nvenc.c add_roi).
uint32_t qpMapBlock(Codec c);
// A protocol weight (-10..10, positive = better) as a QP delta: -weight for
// H.264 / HEVC (QP 0..51), -4 x weight for AV1 (quantizer index 0..255, about
// four index steps per QP step; OBS scales AV1 constant QP by 4 the same way).
int qpDeltaFor(Codec c, int weight);
struct QpMap {
    uint32_t cols = 0, rows = 0;
    std::vector<int8_t> values;  // rows * cols, row-major; 0 = rate control's QP
};
// The map for width x height: a block covered by several rects takes the
// highest weight; rects are clipped to the picture.
QpMap roiQpDeltaMap(Codec c, uint32_t width, uint32_t height, const std::vector<RoiRect>& rects);

}  // namespace recon::nvenc
