#include "protocol.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <type_traits>

#include <nlohmann/json.hpp>

#include "platform/platform.hpp"

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

Status parseBarcode(const json& j, BarcodeLayout& b) {
    if (!j.is_object()) return bad("barcode must be an object");
    std::string err;
    if (!optField(j, "x", b.x, err) || !optField(j, "y", b.y, err) || !optField(j, "blockW", b.blockW, err) ||
        !optField(j, "blockH", b.blockH, err) || !optField(j, "cols", b.cols, err) || !optField(j, "bits", b.bits, err) ||
        !optField(j, "msbFirst", b.msbFirst, err)) {
        return bad("barcode: " + err);
    }
    // Even positions and sizes: every block covers whole 4:2:0 chroma samples.
    if (!inRange(b.x, 0, 16384) || !inRange(b.y, 0, 16384) || (b.x & 1) || (b.y & 1)) return bad("barcode x/y must be even, 0..16384");
    if (!inRange(b.blockW, 2, 256) || !inRange(b.blockH, 2, 256) || (b.blockW & 1) || (b.blockH & 1)) {
        return bad("barcode blockW/blockH must be even, 2..256");
    }
    if (!inRange(b.cols, 1, 64) || !inRange(b.bits, 1, 64)) return bad("barcode cols and bits must be 1..64");
    b.enabled = true;
    return Status::Ok();
}

Status parseStart(const json& j, StartParams& p) {
    std::string err;
    if (!optField(j, "hmonitor", p.hmonitor, err) || !optField(j, "adapterLuid", p.adapterLuid, err) ||
        !optField(j, "window", p.window, err) || !optField(j, "windowTitle", p.windowTitle, err) ||
        !optField(j, "gpuPriority", p.gpuPriority, err) || !optField(j, "idleRepeatMs", p.idleRepeatMs, err)) {
        return bad(err);
    }
    if (auto it = j.find("barcode"); it != j.end() && !it->is_null()) {
        Status bs = parseBarcode(*it, p.barcode);
        if (!bs.ok) return bs;
    }
    if (p.gpuPriority.empty()) p.gpuPriority = "auto";
    if (p.gpuPriority != "auto" && p.gpuPriority != "high" && p.gpuPriority != "realtime" && p.gpuPriority != "off") {
        return bad("gpuPriority must be auto, high, realtime or off");
    }
    if (p.idleRepeatMs == 0) p.idleRepeatMs = 100;
    if (!inRange(p.idleRepeatMs, 20, 2000)) return bad("idleRepeatMs out of range (20..2000)");
    if (p.windowTitle.size() > 512) return bad("windowTitle too long");
    if (!p.adapterLuid.empty()) {
        LUID l;
        if (!parseLuid(p.adapterLuid, l)) return bad("adapterLuid must look like 0000abcd:00001234");
    }
    if (!optField(j, "capture", p.capture, err) || !optField(j, "monitor", p.monitor, err) ||
        !optField(j, "codec", p.codec, err) || !optField(j, "width", p.width, err) ||
        !optField(j, "height", p.height, err) || !optField(j, "fps", p.fps, err) ||
        !optField(j, "kbps", p.kbps, err) || !optField(j, "vbvFrames", p.vbvFrames, err) ||
        !optField(j, "rc", p.rc, err) || !optField(j, "quality", p.quality, err) ||
        !optField(j, "hdr", p.hdr, err) || !optField(j, "ltrSlots", p.ltrSlots, err) ||
        !optField(j, "svcLayers", p.svcLayers, err) || !optField(j, "liveBitrate", p.liveBitrate, err) ||
        !optField(j, "encoderInstance", p.encoderInstance, err) || !optField(j, "ltrInterval", p.ltrInterval, err) ||
        !optField(j, "intraRefreshFrames", p.intraRefreshFrames, err) || !optField(j, "zeroCopy", p.zeroCopy, err) ||
        !optField(j, "reencodeOversized", p.reencodeOversized, err) || !optField(j, "sliceOutput", p.sliceOutput, err)) {
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
    if (!p.liveBitrate.empty() && p.liveBitrate != "seamless" && p.liveBitrate != "flush") {
        return bad("liveBitrate must be seamless or flush");
    }
    if (!inRange(p.encoderInstance, -1, 15)) return bad("encoderInstance out of range (-1..15)");
    if (!inRange(p.ltrInterval, 0, 1000)) return bad("ltrInterval out of range (0..1000)");
    if (!inRange(p.intraRefreshFrames, 0, 1000)) return bad("intraRefreshFrames out of range (0..1000)");
    if (p.reencodeOversized != 0 && !(p.reencodeOversized >= 1.5 && p.reencodeOversized <= 100)) {
        return bad("reencodeOversized must be 0 (off) or 1.5..100 average frames");
    }
    if (!inRange(p.sliceOutput, 0, 64)) return bad("sliceOutput out of range (0..64)");
    if ((p.window || !p.windowTitle.empty()) && !p.capture.empty() && p.capture != "wgc") {
        return bad("window capture needs capture \"wgc\"");
    }
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
    if (m.type == "ack") {
        if (!j.contains("frameId")) return bad("ack needs frameId");
        if (!optField(j, "frameId", m.ackFrameId, err)) return bad(err);
        return Status::Ok();
    }
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
        // kbps 0 / absent = unchanged (a frame-rate change alone, Phase 5).
        if (!inRange(m.rate.kbps, 0, 2000000)) return bad("kbps out of range");
        if (!(m.rate.vbvFrames >= 0 && m.rate.vbvFrames <= 30)) return bad("vbvFrames out of range");
        if (!inRange(m.rate.fps, 0, 480)) return bad("fps out of range");
        if (m.rate.kbps == 0 && m.rate.vbvFrames == 0 && m.rate.fps == 0) return bad("setRate changes nothing (kbps, vbvFrames and fps all 0)");
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
            {"dynamicResolution", cc.dynamicResolution},
            {"hdr10", cc.hdr10},
            {"liveFps", cc.liveFps},
            {"instanceSelect", cc.instanceSelect},
            {"reencode", cc.reencode},
        };
        if (!cc.assumed.empty()) codecs[name]["assumed"] = cc.assumed;
    }
    json unavailable = json::object();
    for (const auto& [name, why] : c.unavailable) unavailable[name] = why;
    json outputs = json::array();
    for (const OutputDesc& o : c.outputs) {
        outputs.push_back({
            {"index", o.index},
            {"adapterIndex", o.adapterIndex},
            {"outputIndex", o.outputIndex},
            {"adapterLuid", o.adapterLuid},
            {"adapterName", o.adapterName},
            {"vendor", o.vendor},
            {"name", o.name},
            {"hmonitor", o.hmonitor},
            {"x", o.x},
            {"y", o.y},
            {"width", o.width},
            {"height", o.height},
            {"rotation", o.rotation},
            {"attached", o.attached},
            {"hdr", o.color.hdr},
            {"bitsPerColor", o.color.bitsPerColor},
            {"minLuminance", o.color.minLuminance},
            {"maxLuminance", o.color.maxLuminance},
            {"maxFullFrameLuminance", o.color.maxFullFrameLuminance},
        });
    }
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
        {"cursorInVideo", c.cursorInVideo},
        {"outputs", outputs},
        {"unavailable", unavailable},
        {"qpcFrequency", qpcFrequency},
    };
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

std::string encodeStarted(const Started& s) {
    json j = {
        {"t", "started"},
        {"backend", s.backend},
        {"encoder", s.encoder},
        {"capture", s.capture},
        {"codec", s.codec},
        {"width", s.width},
        {"height", s.height},
        {"fps", s.fps},
        {"kbps", s.kbps},
        {"captureWidth", s.captureWidth},
        {"captureHeight", s.captureHeight},
        {"adapterLuid", s.adapterLuid},
        {"adapterName", s.adapterName},
        {"vendor", s.vendor},
        {"hagsEnabled", s.hagsEnabled ? json(*s.hagsEnabled) : json(nullptr)},
        {"gpuPriority", s.gpuPriority},
        {"idleRepeatMs", s.idleRepeatMs},
        {"barcode", s.barcode},
        {"cursorInVideo", s.cursorInVideo},
        {"codedWidth", s.codedWidth ? s.codedWidth : s.width},
        {"codedHeight", s.codedHeight ? s.codedHeight : s.height},
        {"cropRight", s.codedWidth ? s.codedWidth - s.width : 0},
        {"cropBottom", s.codedHeight ? s.codedHeight - s.height : 0},
        {"liveBitrate", s.liveBitrate},
        {"rateControl", s.rateControl},
        {"usage", s.usage},
        {"ltrSlots", s.ltrSlots},
        {"ltrInterval", s.ltrInterval},
        {"encoderInstance", s.encoderInstance},
        {"hwInstances", s.hwInstances},
        {"queryTimeoutMs", s.queryTimeoutMs},
        {"zeroCopy", s.zeroCopy},
        {"intraRefreshFrames", s.intraRefreshFrames},
        {"preset", s.preset},
        {"asyncEncode", s.asyncEncode},
        {"refFrames", s.refFrames},
        {"hdr", s.hdr},
        {"bitDepth", s.bitDepth},
        {"colorSpace", s.colorSpace},
        {"svcLayers", s.svcLayers},
        {"liveFps", s.liveFps},
        {"reencodeOversized", s.reencodeOversized},
        {"sliceOutput", s.sliceOutput},
    };
    if (s.hdrMetadata) {
        const HdrMetadata& m = *s.hdrMetadata;
        j["hdrMetadata"] = {
            {"displayPrimaries", {{m.red[0], m.red[1]}, {m.green[0], m.green[1]}, {m.blue[0], m.blue[1]}}},
            {"whitePoint", {m.white[0], m.white[1]}},
            {"maxLuminance", m.maxLuminance},
            {"minLuminance", m.minLuminance},
            {"maxCll", m.maxCll},
            {"maxFall", m.maxFall},
        };
    }
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

std::string encodeCaptureEvent(const CaptureEvent& e) {
    json j = {{"t", "captureChanged"}, {"reason", e.reason}, {"width", e.width}, {"height", e.height},
              {"rotation", e.rotation}, {"hdr", e.hdr}, {"text", e.text}};
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

std::string encodeStats(const FrameStats& s) {
    json j = {
        {"t", "stats"},
        {"frameId", s.frameId},
        {"gen", s.gen},
        {"dropped", s.dropped},
        {"key", s.key},
        {"recovery", s.recovery},
        {"repeat", s.repeat},
        // dirtyPct: the share in whole percent, rounded up (any change >= 1);
        // dirty: the same share as a fraction, -1 = unknown (Phase 5).
        {"dirtyPct", s.dirty < 0 ? -1 : int(std::min(100.0, std::ceil(double(s.dirty) * 100.0 - 1e-6)))},
        {"dirty", s.dirty < 0 ? -1.0 : std::round(double(s.dirty) * 1e6) / 1e6},
        {"discardable", s.discardable},
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
    if (s.reencoded) {
        j["reencoded"] = true;
        j["oversizeBytes"] = s.oversizeBytes;
    }
    if (s.slices > 0) {
        j["slices"] = s.slices;
        j["firstSliceQpc"] = s.firstSliceQpc;
    }
    return j.dump();
}

std::string encodeError(const Status& s, std::string_view re) {
    json j = {{"t", "error"}, {"code", s.code}, {"text", s.text}, {"fatal", s.fatal}};
    if (!re.empty()) j["re"] = std::string(re);
    return j.dump(-1, ' ', false, json::error_handler_t::replace);
}

}  // namespace recon
