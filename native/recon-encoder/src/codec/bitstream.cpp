#include "codec/bitstream.hpp"

#include <algorithm>
#include <cmath>

namespace recon {

namespace {

// One unit of a bitstream: an Annex-B NAL unit (offset of its start code and
// of its header byte) or an AV1 OBU.
struct Unit {
    size_t start = 0;   // first byte of the start code / OBU
    size_t header = 0;  // NAL header byte / OBU header byte
    size_t end = 0;     // one past the last byte
    int type = -1;
};

// Annex-B: units begin at 00 00 01 (a preceding 00 belongs to a 4-byte start code).
std::vector<Unit> annexBUnits(const uint8_t* p, size_t n) {
    std::vector<Unit> out;
    for (size_t i = 0; i + 3 <= n; ++i) {
        if (p[i] != 0 || p[i + 1] != 0 || p[i + 2] != 1) continue;
        Unit u;
        u.start = i > 0 && p[i - 1] == 0 ? i - 1 : i;
        u.header = i + 3;
        if (!out.empty()) out.back().end = u.start;
        out.push_back(u);
        i += 2;
    }
    if (!out.empty()) out.back().end = n;
    // A start code at the very end has no header byte.
    if (!out.empty() && out.back().header >= n) out.pop_back();
    return out;
}

int nalType(Codec c, uint8_t header) { return c == Codec::H264 ? header & 0x1f : (header >> 1) & 0x3f; }

// AV1 low-overhead bitstream format (AV1 spec 5.3): obu_header (forbidden bit,
// 4-bit type, extension flag, has_size_field), optional extension byte,
// leb128 obu_size when has_size_field, else the OBU runs to the end. Returns
// false on malformed data.
bool av1Units(const uint8_t* p, size_t n, std::vector<Unit>& out) {
    size_t pos = 0;
    while (pos < n) {
        const uint8_t h = p[pos];
        if (h & 0x80) return false;  // obu_forbidden_bit
        const size_t hdr = (h & 0x04) ? 2 : 1;
        if (pos + hdr > n) return false;
        size_t len = 0, lebBytes = 0;
        if (h & 0x02) {
            uint64_t v = 0;
            for (;;) {
                if (pos + hdr + lebBytes >= n || lebBytes == 8) return false;
                const uint8_t b = p[pos + hdr + lebBytes];
                v |= uint64_t(b & 0x7f) << (7 * lebBytes);
                ++lebBytes;
                if (!(b & 0x80)) break;
            }
            if (v > n) return false;
            len = size_t(v);
        } else {
            len = n - pos - hdr;
        }
        const size_t total = hdr + lebBytes + len;
        if (total > n - pos) return false;
        Unit u;
        u.start = u.header = pos;
        u.end = pos + total;
        u.type = (h >> 3) & 0x0f;
        out.push_back(u);
        pos += total;
    }
    return true;
}

constexpr int kAv1SequenceHeader = 1, kAv1TemporalDelimiter = 2;
constexpr int kH264Sps = 7, kH264Pps = 8, kH264Aud = 9;
constexpr int kHevcVps = 32, kHevcSps = 33, kHevcPps = 34, kHevcAud = 35;

bool unitsHaveParameterSets(Codec c, const std::vector<Unit>& units) {
    bool vps = false, sps = false, pps = false;
    for (const Unit& u : units) {
        if (c == Codec::Av1) {
            if (u.type == kAv1SequenceHeader) return true;
            continue;
        }
        const int t = u.type;
        if (c == Codec::H264) {
            sps = sps || t == kH264Sps;
            pps = pps || t == kH264Pps;
        } else {
            vps = vps || t == kHevcVps;
            sps = sps || t == kHevcSps;
            pps = pps || t == kHevcPps;
        }
    }
    return c == Codec::H264 ? sps && pps : c == Codec::Hevc && vps && sps && pps;
}

std::vector<Unit> units(Codec c, const uint8_t* p, size_t n, bool& ok) {
    std::vector<Unit> out;
    ok = true;
    if (c == Codec::Av1) {
        ok = av1Units(p, n, out);
        return out;
    }
    out = annexBUnits(p, n);
    for (Unit& u : out) u.type = nalType(c, p[u.header]);
    ok = !out.empty() && out.front().start == 0;
    return out;
}

}  // namespace

bool parseCodec(const std::string& name, Codec& out) {
    if (name == "h264") out = Codec::H264;
    else if (name == "hevc") out = Codec::Hevc;
    else if (name == "av1") out = Codec::Av1;
    else return false;
    return true;
}

const char* codecName(Codec c) {
    switch (c) {
    case Codec::H264: return "h264";
    case Codec::Hevc: return "hevc";
    case Codec::Av1: return "av1";
    }
    return "?";
}

bool hasParameterSets(Codec c, const uint8_t* data, size_t size) {
    if (!data || !size) return false;
    bool ok = false;
    const std::vector<Unit> u = units(c, data, size, ok);
    return ok && unitsHaveParameterSets(c, u);
}

std::vector<uint8_t> withParameterSets(Codec c, const uint8_t* data, size_t size, const uint8_t* extra, size_t extraSize) {
    if (!hasParameterSets(c, extra, extraSize) || !data || !size) return {};
    bool ok = false;
    const std::vector<Unit> u = units(c, data, size, ok);
    if (!ok) return {};
    size_t at = 0;  // insertion point
    if (!u.empty()) {
        const int aud = c == Codec::Av1 ? kAv1TemporalDelimiter : c == Codec::H264 ? kH264Aud : kHevcAud;
        if (u.front().type == aud) at = u.front().end;
    }
    std::vector<uint8_t> out;
    out.reserve(size + extraSize);
    out.insert(out.end(), data, data + at);
    out.insert(out.end(), extra, extra + extraSize);
    out.insert(out.end(), data + at, data + size);
    return out;
}

RoiMap roiImportanceMap(uint32_t width, uint32_t height, uint32_t block, const std::vector<RoiRect>& rects) {
    RoiMap m;
    if (!block || !width || !height) return m;
    m.cols = (width + block - 1) / block;
    m.rows = (height + block - 1) / block;
    // -1 = not covered by any rect (background); else the highest importance seen.
    std::vector<int> best(size_t(m.cols) * m.rows, -1);
    for (const RoiRect& r : rects) {
        if (r.w <= 0 || r.h <= 0 || r.x < 0 || r.y < 0) continue;
        const int64_t x0 = r.x, y0 = r.y;
        const int64_t x1 = std::min<int64_t>(int64_t(r.x) + r.w, width), y1 = std::min<int64_t>(int64_t(r.y) + r.h, height);
        if (x0 >= x1 || y0 >= y1) continue;
        const int w = std::clamp(r.weight, -10, 10);
        const int importance = int(std::clamp<long>(long(kRoiBackground) + std::lround(w / 2.0), 0, 10));
        for (int64_t by = y0 / block; by <= (y1 - 1) / block; ++by) {
            for (int64_t bx = x0 / block; bx <= (x1 - 1) / block; ++bx) {
                int& v = best[size_t(by) * m.cols + size_t(bx)];
                v = std::max(v, importance);
            }
        }
    }
    m.values.resize(best.size());
    for (size_t i = 0; i < best.size(); ++i) m.values[i] = best[i] < 0 ? kRoiBackground : uint32_t(best[i]);
    return m;
}

}  // namespace recon
