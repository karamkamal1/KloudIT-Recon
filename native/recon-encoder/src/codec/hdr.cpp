#include "codec/hdr.hpp"

#include <algorithm>
#include <cmath>

namespace recon {

namespace {

uint32_t code(double v, double scale, double max) { return uint32_t(std::lround(std::clamp(v * scale, 0.0, max))); }

}  // namespace

HdrMetadata hdrMetadataFor(const DisplayColor& d) {
    HdrMetadata m;
    // ITU-R BT.2020 table 3 primaries, D65 white (CIE 1931 xy).
    m.red[0] = 0.708, m.red[1] = 0.292;
    m.green[0] = 0.170, m.green[1] = 0.797;
    m.blue[0] = 0.131, m.blue[1] = 0.046;
    m.white[0] = 0.3127, m.white[1] = 0.3290;
    const bool peakOk = d.known && d.maxLuminance >= 80 && d.maxLuminance <= 10000;
    m.maxLuminance = peakOk ? d.maxLuminance : kDefaultHdrPeak;
    m.minLuminance = peakOk && d.minLuminance > 0 && d.minLuminance < std::min(5.0, m.maxLuminance) ? d.minLuminance : 0;
    m.maxCll = int(std::lround(m.maxLuminance));
    const bool fullOk = peakOk && d.maxFullFrameLuminance > 0 && d.maxFullFrameLuminance <= m.maxLuminance;
    m.maxFall = fullOk ? int(std::lround(d.maxFullFrameLuminance)) : m.maxCll;
    return m;
}

MasteringCodes masteringCodes(const HdrMetadata& m, bool av1) {
    // HEVC: chromaticity 0..50000 (D.3.28 "in the range of 0 to 50 000"),
    // luminance up to 10000 cd/m2 = 100 000 000. AV1: 0.16 fixed point
    // chromaticity (at most 0xffff), 24.8 / 18.14 fixed point luminance.
    const double chroma = av1 ? 65536.0 : 50000.0, chromaMax = av1 ? 65535.0 : 50000.0;
    const double maxScale = av1 ? 256.0 : 10000.0, minScale = av1 ? 16384.0 : 10000.0;
    MasteringCodes c;
    for (int i = 0; i < 2; ++i) {
        c.red[i] = uint16_t(code(m.red[i], chroma, chromaMax));
        c.green[i] = uint16_t(code(m.green[i], chroma, chromaMax));
        c.blue[i] = uint16_t(code(m.blue[i], chroma, chromaMax));
        c.white[i] = uint16_t(code(m.white[i], chroma, chromaMax));
    }
    c.maxLuminance = code(m.maxLuminance, maxScale, 10000.0 * maxScale);
    c.minLuminance = code(m.minLuminance, minScale, 10000.0 * minScale);
    return c;
}

void describeColor(Started& out, const std::optional<HdrMetadata>& m) {
    out.hdr = m.has_value();
    out.bitDepth = m ? 10 : 8;
    out.colorSpace = m ? "bt2020-pq" : "bt709";
    out.hdrMetadata = m;
}

}  // namespace recon
