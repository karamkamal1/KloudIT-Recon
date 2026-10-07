#include "protocol.hpp"

#include <cstdint>
#include <limits>
#include <type_traits>

#include <nlohmann/json.hpp>

namespace recon {

using json = nlohmann::ordered_json;  // keeps "t" first in every message

namespace {

// Reads an optional field; a present field of the wrong type is an error.
template <typename T>
bool optField(const json& j, const char* key, T& out, std::string& err) {
    auto it = j.find(key);
    if (it == j.end() || it->is_null()) return true;
    bool typeOk;
    if constexpr (std::is_same_v<T, bool>) {
        typeOk = it->is_boolean();
    } else if constexpr (std::is_same_v<T, std::string>) {
        typeOk = it->is_string();
    } else if constexpr (std::is_floating_point_v<T>) {
        typeOk = it->is_number();
    } else if constexpr (std::is_unsigned_v<T>) {
        typeOk = it->is_number_unsigned();
    } else {
        typeOk = it->is_number_integer();
    }
    if (!typeOk) {
        err = std::string("field \"") + key + "\" has the wrong type";
        return false;
    }
    if constexpr (std::is_integral_v<T> && !std::is_same_v<T, bool>) {
        // Range-check before narrowing so 2^32+60 does not become 60.
        if (it->is_number_unsigned()) {
            if (it->get<uint64_t>() > static_cast<uint64_t>(std::numeric_limits<T>::max())) typeOk = false;
        } else {
            const int64_t v = it->get<int64_t>();
            if (v < static_cast<int64_t>(std::numeric_limits<T>::min()) ||
                (v > 0 && static_cast<uint64_t>(v) > static_cast<uint64_t>(std::numeric_limits<T>::max()))) {
                typeOk = false;
            }
        }
        if (!typeOk) {
            err = std::string("field \"") + key + "\" is out of range";
            return false;
        }
    }
    out = it->get<T>();
    return true;
}

bool inRange(int v, int lo, int hi) { return v >= lo && v <= hi; }

Status bad(std::string text) { return Status::Error("bad_message", std::move(text)); }

Status parseStart(const json& j, StartParams& p) {
    std::string err;
    if (!optField(j, "capture", p.capture, err) || !optField(j, "monitor", p.monitor, err) ||
        !optField(j, "codec", p.codec, err) || !optField(j, "width", p.width, err) ||
        !optField(j, "height", p.height, err) || !optField(j, "fps", p.fps, err) ||
        !optField(j, "kbps", p.kbps, err) || !optField(j, "vbvFrames", p.vbvFrames, err) ||
        !optField(j, "rc", p.rc, err) || !optField(j, "quality", p.quality, err) ||
        !optField(j, "hdr", p.hdr, err) || !optField(j, "ltrSlots", p.ltrSlots, err) ||
        !optField(j, "svcLayers", p.svcLayers, err)) {
        return bad(err);
    }
    if (p.codec != "h264" && p.codec != "hevc" && p.codec != "av1") return bad("codec must be h264, hevc or av1");
    if (!inRange(p.monitor, 0, 63)) return bad("monitor out of range");
    if (!inRange(p.width, 0, 16384) || !inRange(p.height, 0, 16384)) return bad("size out of range");
    if (!inRange(p.fps, 1, 480)) return bad("fps out of range");
    if (!inRange(p.kbps, 1, 2000000)) return bad("kbps out of range");
    if (!(p.vbvFrames > 0 && p.vbvFrames <= 30)) return bad("vbvFrames out of range");
    if (p.rc != "cbr" && p.rc != "vbr") return bad("rc must be cbr or vbr");
    if (p.quality != "speed" && p.quality != "balanced" && p.quality != "quality") return bad("unknown quality");
    if (!inRange(p.ltrSlots, 0, 8)) return bad("ltrSlots out of range");
    if (!inRange(p.svcLayers, 1, 4)) return bad("svcLayers out of range");
    return Status::Ok();
}

}  // namespace

Status parseControl(std::string_view text, ControlMsg& m) {
    json j = json::parse(text.begin(), text.end(), nullptr, false);
    if (j.is_discarded() || !j.is_object()) return bad("not a JSON object");
    std::string err;
    if (!optField(j, "t", m.type, err)) return bad(err);
    if (m.type == "start") return parseStart(j, m.start);
    if (m.type == "forceIdr" || m.type == "shutdown") return Status::Ok();
    if (m.type == "recover") {
        if (!j.contains("lostFromFrameId")) return bad("recover needs lostFromFrameId");
        uint64_t acked = 0;
        if (!optField(j, "lostFromFrameId", m.lostFromFrameId, err) || !optField(j, "ackedLtrFrameId", acked, err)) {
            return bad(err);
        }
        if (j.contains("ackedLtrFrameId") && !j["ackedLtrFrameId"].is_null()) m.ackedLtrFrameId = acked;
        return Status::Ok();
    }
    if (m.type == "setRate") {
        if (!optField(j, "kbps", m.rate.kbps, err) || !optField(j, "vbvFrames", m.rate.vbvFrames, err) ||
            !optField(j, "fps", m.rate.fps, err)) {
            return bad(err);
        }
        if (!inRange(m.rate.kbps, 1, 2000000)) return bad("kbps out of range");
        if (!(m.rate.vbvFrames >= 0 && m.rate.vbvFrames <= 30)) return bad("vbvFrames out of range");
        if (!inRange(m.rate.fps, 0, 480)) return bad("fps out of range");
        return Status::Ok();
    }
    if (m.type == "setRoi") {
        auto it = j.find("rects");
        if (it == j.end() || !it->is_array()) return bad("setRoi needs a rects array");
        if (it->size() > 256) return bad("too many ROI rects");
        for (const auto& r : *it) {
            if (!r.is_object()) return bad("ROI rect must be an object");
            RoiRect rr;
            if (!optField(r, "x", rr.x, err) || !optField(r, "y", rr.y, err) || !optField(r, "w", rr.w, err) ||
                !optField(r, "h", rr.h, err) || !optField(r, "weight", rr.weight, err)) {
                return bad(err);
            }
            if (rr.x < 0 || rr.y < 0 || rr.w <= 0 || rr.h <= 0 || !inRange(rr.weight, -10, 10)) {
                return bad("ROI rect out of range");
            }
            m.rects.push_back(rr);
        }
        return Status::Ok();
    }
    return bad("unknown message type \"" + m.type + "\"");
}

std::string encodeCaps(const Caps& c, int64_t qpcFrequency) {
    json codecs = json::object();
    for (const auto& [name, cc] : c.codecs) {
        codecs[name] = {
            {"maxW", cc.maxW},
            {"maxH", cc.maxH},
            {"tenBit", cc.tenBit},
            {"yuv444", cc.yuv444},
            {"forceIdr", cc.forceIdr},
            {"recovery", cc.recovery},
            {"maxLtr", cc.maxLtr},
            {"intraRefresh", cc.intraRefresh},
            {"liveBitrate", cc.liveBitrate},
            {"maxTemporalLayers", cc.maxTemporalLayers},
            {"roi", cc.roi},
            {"sliceOutput", cc.sliceOutput},
            {"hwInstances", cc.hwInstances},
            {"queryTimeout", cc.queryTimeout},
            {"alignW", cc.alignW},
            {"alignH", cc.alignH},
        };
    }
    json unavailable = json::object();
    for (const auto& [name, why] : c.unavailable) unavailable[name] = why;
    json j = {
        {"t", "caps"},
        {"v", kProtocolVersion},
        {"helperVersion", RECON_ENCODER_VERSION},
        {"backend", c.backend},
        {"vendor", c.vendor},
        {"adapterLuid", c.adapterLuid},
        {"adapterName", c.adapterName},
        {"hagsEnabled", c.hagsEnabled ? json(*c.hagsEnabled) : json(nullptr)},
        {"codecs", codecs},
        {"capture", c.capture},
        {"unavailable", unavailable},
        {"qpcFrequency", qpcFrequency},
    };
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

std::string encodeStarted(const Started& s) {
    json j = {
        {"t", "started"}, {"backend", s.backend}, {"capture", s.capture}, {"codec", s.codec},
        {"width", s.width}, {"height", s.height}, {"fps", s.fps}, {"kbps", s.kbps},
    };
    return j.dump();
}

std::string encodeStats(const FrameStats& s) {
    json j = {
        {"t", "stats"},
        {"frameId", s.frameId},
        {"gen", s.gen},
        {"dropped", s.dropped},
        {"key", s.key},
        {"recovery", s.recovery},
        {"bytes", s.bytes},
        {"presentQpc", s.presentQpc},
        {"captureQpc", s.captureQpc},
        {"submitQpc", s.submitQpc},
        {"outputQpc", s.outputQpc},
        {"ltrSlot", s.ltrSlot},
        {"temporalLayer", s.temporalLayer},
        {"refLtrMask", s.refLtrMask},
        {"kbps", s.rate.kbps},
        {"vbvFrames", s.rate.vbvFrames},
        {"fps", s.rate.fps},
        {"ringDropped", s.ringDropped},
    };
    if (s.dropped) j["reason"] = s.dropReason;
    if (s.recovery) j["refFloor"] = s.refFloor;
    return j.dump();
}

std::string encodeError(const Status& s, std::string_view re) {
    json j = {{"t", "error"}, {"code", s.code}, {"text", s.text}, {"fatal", s.fatal}};
    if (!re.empty()) j["re"] = std::string(re);
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

}  // namespace recon
