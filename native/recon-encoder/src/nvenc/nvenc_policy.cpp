#include "nvenc/nvenc_policy.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>

namespace recon::nvenc {

std::string apiVersionText(uint32_t v) { return std::to_string(v >> 4) + "." + std::to_string(v & 0xf); }

std::string minimumDriver(uint32_t v) {
    // Windows driver versions from FFmpeg nvenc.c nvenc_print_driver_requirement.
    struct Entry {
        uint32_t version;
        const char* driver;
    };
    static const Entry table[] = {
        {apiVersion(11, 0), "456.71"}, {apiVersion(11, 1), "471.41"}, {apiVersion(12, 0), "522.25"},
        {apiVersion(12, 1), "531.61"}, {apiVersion(12, 2), "551.76"}, {apiVersion(13, 0), "570.0"},
    };
    for (const Entry& e : table) {
        if (e.version == v) return e.driver;
    }
    return "";
}

std::string versionProblem(uint32_t driverMax, uint32_t built) {
    if (driverMax == 0) return "NvEncodeAPIGetMaxSupportedVersion reported no NVENC API version";
    if (driverMax >= built) return "";
    const std::string min = minimumDriver(built);
    return "the NVIDIA driver supports NVENC API " + apiVersionText(driverMax) + ", the helper needs " + apiVersionText(built) +
           ": update the NVIDIA driver" + (min.empty() ? "" : " to " + min + " or newer");
}

int presetFor(uint32_t width, uint32_t height, int fps, const std::string& quality) {
    const double rate = double(width) * double(height) * double(std::max(1, fps));
    constexpr double kP4 = 2560.0 * 1440.0 * 120.0, kP1 = 3840.0 * 2160.0 * 120.0;
    int p = 4;
    if (rate >= kP1) {
        p = 1;
    } else if (rate > kP4) {
        p = int(std::lround(4.0 - 3.0 * std::log(rate / kP4) / std::log(kP1 / kP4)));
    }
    p = std::clamp(p, 1, 4);
    if (quality == "balanced") p += 1;
    else if (quality == "quality") p += 2;
    return std::min(p, 7);
}

Rate rateFor(int kbps, double vbvFrames, int fps) {
    Rate r;
    r.average = r.max = uint32_t(std::clamp<int64_t>(int64_t(kbps) * 1000, 1, INT32_MAX));
    const double vbv = double(r.average) * std::max(0.01, vbvFrames) / double(std::max(1, fps));
    r.vbv = uint32_t(std::clamp<double>(std::round(vbv), 1.0, double(INT32_MAX)));
    return r;
}

int dpbFramesFor(Codec c, uint32_t width, uint32_t height) {
    if (!width || !height) return kDpbFrames;
    int limit = kDpbFrames;
    if (c == Codec::H264) {
        // Frame size in macroblocks; level 5.1 / 5.2 MaxFS 36864, level 6.x
        // MaxFS 139264 / MaxDpbMbs 696320 (Table A-1).
        const uint64_t mbs = uint64_t(alignUp(width, 16) / 16) * (alignUp(height, 16) / 16);
        const uint64_t maxDpbMbs = mbs <= 36864 ? 184320 : 696320;
        limit = int(std::min<uint64_t>(16, maxDpbMbs / mbs));
    } else if (c == Codec::Hevc) {
        // pic_width / height_in_luma_samples are multiples of MinCbSizeY (8);
        // 16 is the conservative choice where the encoder pads further.
        const uint64_t w = alignUp(width, 16), h = alignUp(height, 16), ps = w * h;
        uint64_t maxLumaPs = 8912896;  // levels 5 - 5.2 (Table A.8)
        if (ps > maxLumaPs || w * w > 8 * maxLumaPs || h * h > 8 * maxLumaPs) maxLumaPs = 35651584;  // levels 6 - 6.2
        constexpr int kMaxDpbPicBuf = 6;  // Main, Main 10
        int maxDpbSize = kMaxDpbPicBuf;
        if (ps <= maxLumaPs >> 2) maxDpbSize = std::min(4 * kMaxDpbPicBuf, 16);
        else if (ps <= maxLumaPs >> 1) maxDpbSize = std::min(2 * kMaxDpbPicBuf, 16);
        else if (ps <= (3 * maxLumaPs) >> 2) maxDpbSize = std::min(4 * kMaxDpbPicBuf / 3, 16);
        limit = maxDpbSize - 1;  // the current picture takes one buffer
    }
    return std::clamp(limit, 1, kDpbFrames);
}

uint32_t qpMapBlock(Codec c) {
    switch (c) {
    case Codec::H264: return 16;
    case Codec::Hevc: return 32;
    case Codec::Av1: return 64;
    }
    return 16;
}

int qpDeltaFor(Codec c, int weight) {
    const int w = std::clamp(weight, -10, 10);
    return c == Codec::Av1 ? -4 * w : -w;
}

QpMap roiQpDeltaMap(Codec c, uint32_t width, uint32_t height, const std::vector<RoiRect>& rects) {
    QpMap m;
    const uint32_t block = qpMapBlock(c);
    if (!width || !height) return m;
    m.cols = (width + block - 1) / block;
    m.rows = (height + block - 1) / block;
    // INT32_MIN = not covered by any rect; else the highest weight seen.
    std::vector<int> best(size_t(m.cols) * m.rows, INT32_MIN);
    for (const RoiRect& r : rects) {
        if (r.w <= 0 || r.h <= 0 || r.x < 0 || r.y < 0) continue;
        const int64_t x0 = r.x, y0 = r.y;
        const int64_t x1 = std::min<int64_t>(int64_t(r.x) + r.w, width), y1 = std::min<int64_t>(int64_t(r.y) + r.h, height);
        if (x0 >= x1 || y0 >= y1) continue;
        const int w = std::clamp(r.weight, -10, 10);
        for (int64_t by = y0 / block; by <= (y1 - 1) / block; ++by) {
            for (int64_t bx = x0 / block; bx <= (x1 - 1) / block; ++bx) {
                int& v = best[size_t(by) * m.cols + size_t(bx)];
                v = std::max(v, w);
            }
        }
    }
    m.values.resize(best.size());
    for (size_t i = 0; i < best.size(); ++i) m.values[i] = best[i] == INT32_MIN ? int8_t(0) : int8_t(qpDeltaFor(c, best[i]));
    return m;
}

}  // namespace recon::nvenc
