// HDR10 (GUIDE 3.9), encoder-independent: the static metadata a stream
// carries and its fixed-point encodings for the encoders. The colour
// conversion itself (scRGB -> BT.2020 PQ, P010) is in d3d/convert.cpp.
#pragma once

#include <cstdint>
#include <optional>

#include "types.hpp"

namespace recon {

// Peak luminance assumed for an output that reports none (or an implausible
// one, e.g. some virtual displays): a common HDR monitor class.
constexpr double kDefaultHdrPeak = 1000.0;

// The HDR metadata of a stream captured from an output with colour d, as
// Sunshine fills it (display_base.cpp get_hdr_metadata): BT.2020 primaries
// with a D65 white point (the stream's container; Sunshine found the panel
// primaries DXGI reports unreliable, and clients mostly ignore them), the
// output's luminance range as the mastering display's, and its peak and
// full-frame luminance as MaxCLL / MaxFALL: the content as the host's display
// showed it (games tone-map to DXGI's MaxLuminance). Missing or implausible
// values fall back to a kDefaultHdrPeak display with a black level of 0.
HdrMetadata hdrMetadataFor(const DisplayColor& d);

// The mastering display colour volume as codes.
// HEVC SEI (D.3.28) and AMF's AMFHDRMetadata ("normalized to 50000" /
// "normalized to 10000"): chromaticity in units of 0.00002, luminance in units
// of 0.0001 cd/m2. AV1 metadata (6.7.4; NVENC's MASTERING_DISPLAY_INFO for
// AV1, as FFmpeg's nvenc.c fills it): chromaticity 0.16 fixed point, maximum
// luminance 24.8, minimum luminance 18.14 fixed point.
struct MasteringCodes {
    uint16_t red[2] = {}, green[2] = {}, blue[2] = {}, white[2] = {};
    uint32_t maxLuminance = 0, minLuminance = 0;
};
MasteringCodes masteringCodes(const HdrMetadata& m, bool av1);

// Started's colour fields: HDR10 (10-bit, "bt2020-pq", the metadata) when m
// is set, else 8-bit "bt709".
void describeColor(Started& out, const std::optional<HdrMetadata>& m);

}  // namespace recon
