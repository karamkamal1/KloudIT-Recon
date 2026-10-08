#include "codec/bitstream.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <iterator>

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

// Reads an RBSP: the NAL unit's payload with its emulation prevention bytes
// (00 00 03 -> 00 00, H.264 / HEVC 7.3.1) removed. Reading past the end
// yields zeros and marks the reader failed.
class BitReader {
public:
    BitReader(const uint8_t* p, size_t n) {
        rbsp_.reserve(n);
        int zeros = 0;
        for (size_t i = 0; i < n; ++i) {
            if (zeros >= 2 && p[i] == 3) {
                zeros = 0;  // emulation_prevention_three_byte
                continue;
            }
            rbsp_.push_back(p[i]);
            zeros = p[i] == 0 ? zeros + 1 : 0;
        }
    }
    uint32_t u(int n) {
        uint32_t v = 0;
        while (n-- > 0) v = (v << 1) | bit();
        return v;
    }
    void skip(int n) {
        while (n-- > 0) bit();
    }
    // ue(v), 9.1: leading zero bits, a one, as many info bits.
    uint32_t ue() {
        int zeros = 0;
        while (!bit()) {
            if (++zeros > 31 || bad_) {
                bad_ = true;
                return 0;
            }
        }
        return zeros ? uint32_t((uint64_t(1) << zeros) - 1 + u(zeros)) : 0;
    }
    int32_t se() {
        const uint32_t k = ue();
        return k & 1 ? int32_t((k + 1) / 2) : -int32_t(k / 2);
    }
    bool ok() const { return !bad_; }

private:
    uint32_t bit() {
        if (pos_ >= rbsp_.size() * 8) {
            bad_ = true;
            return 0;
        }
        const uint32_t b = (rbsp_[pos_ / 8] >> (7 - pos_ % 8)) & 1;
        ++pos_;
        return b;
    }
    std::vector<uint8_t> rbsp_;
    size_t pos_ = 0;
    bool bad_ = false;
};

// seq_parameter_set_data() up to max_num_ref_frames (H.264 7.3.2.1.1).
bool parseH264Sps(BitReader& b, SpsInfo& out) {
    const uint32_t profile = b.u(8);
    b.skip(8);  // constraint_set0..5_flag, reserved_zero_2bits
    out.levelIdc = int(b.u(8));
    b.ue();  // seq_parameter_set_id
    static const uint32_t kChromaProfiles[] = {100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135};
    if (std::find(std::begin(kChromaProfiles), std::end(kChromaProfiles), profile) != std::end(kChromaProfiles)) {
        const uint32_t chroma = b.ue();  // chroma_format_idc
        if (chroma == 3) b.skip(1);      // separate_colour_plane_flag
        b.ue();                          // bit_depth_luma_minus8
        b.ue();                          // bit_depth_chroma_minus8
        b.skip(1);                       // qpprime_y_zero_transform_bypass_flag
        if (b.u(1)) {                    // seq_scaling_matrix_present_flag
            for (int i = 0; i < (chroma != 3 ? 8 : 12) && b.ok(); ++i) {
                if (!b.u(1)) continue;  // seq_scaling_list_present_flag[i]
                // scaling_list() (7.3.2.1.1.1): delta_scale until nextScale is 0.
                int last = 8, next = 8;
                for (int j = 0; j < (i < 6 ? 16 : 64) && next != 0 && b.ok(); ++j) {
                    const int32_t delta = b.se();  // delta_scale, -128..127
                    if (delta < -128 || delta > 127) return false;
                    next = (last + delta + 256) % 256;
                    if (next) last = next;
                }
            }
        }
    }
    b.ue();  // log2_max_frame_num_minus4
    const uint32_t poc = b.ue();  // pic_order_cnt_type
    if (poc == 0) {
        b.ue();  // log2_max_pic_order_cnt_lsb_minus4
    } else if (poc == 1) {
        b.skip(1);  // delta_pic_order_always_zero_flag
        b.se();     // offset_for_non_ref_pic
        b.se();     // offset_for_top_to_bottom_field
        const uint32_t n = b.ue();  // num_ref_frames_in_pic_order_cnt_cycle
        if (n > 255) return false;
        for (uint32_t i = 0; i < n; ++i) b.se();  // offset_for_ref_frame[i]
    }
    const uint32_t refs = b.ue();  // max_num_ref_frames
    out.refFrames = int(refs);
    return b.ok() && poc <= 2 && refs <= 16;
}

// seq_parameter_set_rbsp() up to sps_max_dec_pic_buffering_minus1 (HEVC
// 7.3.2.2.1, profile_tier_level() 7.3.3).
bool parseHevcSps(BitReader& b, SpsInfo& out) {
    b.skip(4);                         // sps_video_parameter_set_id
    const uint32_t maxSub = b.u(3);    // sps_max_sub_layers_minus1
    b.skip(1);                         // sps_temporal_id_nesting_flag
    if (maxSub > 6) return false;
    b.skip(88);  // general_profile_space .. general_inbld_flag / reserved
    out.levelIdc = int(b.u(8));  // general_level_idc
    bool profilePresent[8] = {}, levelPresent[8] = {};
    for (uint32_t i = 0; i < maxSub; ++i) {
        profilePresent[i] = b.u(1) != 0;
        levelPresent[i] = b.u(1) != 0;
    }
    if (maxSub > 0) b.skip(2 * int(8 - maxSub));  // reserved_zero_2bits
    for (uint32_t i = 0; i < maxSub; ++i) {
        if (profilePresent[i]) b.skip(88);  // sub_layer_profile_space .. sub_layer_inbld_flag / reserved
        if (levelPresent[i]) b.skip(8);     // sub_layer_level_idc
    }
    b.ue();                            // sps_seq_parameter_set_id
    if (b.ue() == 3) b.skip(1);        // chroma_format_idc, separate_colour_plane_flag
    b.ue();                            // pic_width_in_luma_samples
    b.ue();                            // pic_height_in_luma_samples
    if (b.u(1)) {                      // conformance_window_flag
        for (int i = 0; i < 4; ++i) b.ue();
    }
    b.ue();                            // bit_depth_luma_minus8
    b.ue();                            // bit_depth_chroma_minus8
    b.ue();                            // log2_max_pic_order_cnt_lsb_minus4
    const bool allLayers = b.u(1) != 0;  // sps_sub_layer_ordering_info_present_flag
    uint32_t buffering = 0;
    for (uint32_t i = allLayers ? 0 : maxSub; i <= maxSub; ++i) {
        buffering = b.ue();  // sps_max_dec_pic_buffering_minus1[i]: the last is HighestTid's
        b.ue();              // sps_max_num_reorder_pics[i]
        b.ue();              // sps_max_latency_increase_plus1[i]
    }
    out.refFrames = int(buffering);
    return b.ok() && buffering <= 15;
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

bool parseSps(Codec c, const uint8_t* data, size_t size, SpsInfo& out) {
    out = SpsInfo{};
    if (c == Codec::Av1 || !data || !size) return false;
    const int spsType = c == Codec::H264 ? kH264Sps : kHevcSps;
    for (const Unit& u : annexBUnits(data, size)) {
        if (nalType(c, data[u.header]) != spsType) continue;
        // The payload after the NAL unit header (H.264 1 byte, HEVC 2).
        const size_t header = c == Codec::H264 ? 1 : 2;
        if (u.end < u.header + header) return false;
        BitReader b(data + u.header + header, u.end - u.header - header);
        SpsInfo info;
        if (!(c == Codec::H264 ? parseH264Sps(b, info) : parseHevcSps(b, info))) return false;
        out = info;
        return true;
    }
    return false;
}

std::string levelText(Codec c, int levelIdc) {
    // H.264 level_idc = 10 x level (9 = level 1b); HEVC general_level_idc = 30 x level.
    if (c == Codec::H264) return levelIdc == 9 ? "1b" : std::to_string(levelIdc / 10) + "." + std::to_string(levelIdc % 10);
    if (c == Codec::Hevc) return std::to_string(levelIdc / 30) + "." + std::to_string(levelIdc % 30 / 3);
    return std::to_string(levelIdc);
}

LayerInfo layerInfo(Codec c, const uint8_t* data, size_t size, int highestTemporalId) {
    LayerInfo li;
    if (!data || !size) return li;
    bool ok = false;
    const std::vector<Unit> us = units(c, data, size, ok);
    if (!ok) return li;
    for (const Unit& u : us) {
        const uint8_t* h = data + u.header;
        const size_t left = u.end - u.header;
        if (c == Codec::Av1) {
            // OBU_FRAME_HEADER 3, OBU_TILE_GROUP 4, OBU_FRAME 6 with obu_extension_flag.
            if ((u.type == 3 || u.type == 4 || u.type == 6) && (h[0] & 0x04) && left >= 2) {
                li.temporalId = h[1] >> 5;
                return li;
            }
            continue;
        }
        if (c == Codec::H264) {
            if (u.type == 14 && left >= 4 && (h[1] & 0x80) && li.temporalId < 0) li.temporalId = h[3] >> 5;  // svc_extension_flag
            if (u.type == 1 || u.type == 5) {
                li.reference = (h[0] & 0x60) ? 1 : 0;  // nal_ref_idc
                return li;
            }
            continue;
        }
        // HEVC VCL NAL units: types 0..31.
        if (u.type <= 31 && left >= 2) {
            li.temporalId = int(h[1] & 0x07) - 1;
            const bool subLayerNonRef = u.type <= 14 && u.type % 2 == 0;
            li.reference = subLayerNonRef && li.temporalId >= highestTemporalId ? 0 : 1;
            return li;
        }
    }
    return li;
}

bool isDiscardable(bool key, const LayerInfo& bits, int svcLayers, uint32_t temporalLayer) {
    if (key) return false;
    if (bits.reference >= 0) return bits.reference == 0;
    return svcLayers > 1 && temporalLayer == uint32_t(svcLayers - 1);
}

bool writeRoiPlane(const RoiMap& m, uint8_t* base, size_t pitch) {
    const size_t row = size_t(m.cols) * sizeof(uint32_t);
    if (!base || pitch < row || m.values.size() != size_t(m.cols) * m.rows) return false;
    for (uint32_t y = 0; y < m.rows; ++y) std::memcpy(base + size_t(y) * pitch, &m.values[size_t(y) * m.cols], row);
    return true;
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
