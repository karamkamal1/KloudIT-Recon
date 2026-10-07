#include "encode_test.hpp"

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <mutex>
#include <optional>

#include "codec/bitstream.hpp"
#include "protocol.hpp"
#include "stream.hpp"

namespace recon {

namespace {

bool isInt(const std::string& v, bool allowHex = false) {
    if (v.empty()) return false;
    char* end = nullptr;
    errno = 0;
    std::strtoll(v.c_str(), &end, allowHex ? 0 : 10);
    return errno == 0 && end && *end == '\0';
}

bool isNumber(const std::string& v) {
    if (v.empty()) return false;
    char* end = nullptr;
    errno = 0;
    const double d = std::strtod(v.c_str(), &end);
    return errno == 0 && end && *end == '\0' && std::isfinite(d);
}

bool isWord(const std::string& v) {
    return !v.empty() && std::all_of(v.begin(), v.end(), [](char c) {
        return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_';
    });
}

// The start message is assembled as JSON and parsed by the control protocol's
// parser, so the test validates and defaults exactly like a real "start".
std::string& startFields() {
    static std::string fields;
    return fields;
}

}  // namespace

namespace {

enum class Kind { Str, Int, Hex, Num, Bool };

struct OptionField {
    const char* opt;
    const char* json;
    Kind kind;
};

// Encode test options that become fields of the start message.
const OptionField kStartOptions[] = {
    {"--codec", "codec", Kind::Str},
    {"--capture", "capture", Kind::Str},
    {"--width", "width", Kind::Int},
    {"--height", "height", Kind::Int},
    {"--fps", "fps", Kind::Int},
    {"--kbps", "kbps", Kind::Int},
    {"--rc", "rc", Kind::Str},
    {"--quality", "quality", Kind::Str},
    {"--vbv", "vbvFrames", Kind::Num},
    {"--ltr-slots", "ltrSlots", Kind::Int},
    {"--ltr-interval", "ltrInterval", Kind::Int},
    {"--live-bitrate", "liveBitrate", Kind::Str},
    {"--instance", "encoderInstance", Kind::Int},
    {"--zero-copy", "zeroCopy", Kind::Bool},
    {"--intra-refresh", "intraRefreshFrames", Kind::Int},
    {"--monitor", "monitor", Kind::Int},
    {"--hmonitor", "hmonitor", Kind::Hex},
};

}  // namespace

bool encodeTestOption(const std::string& key, const std::string& val, EncodeTestOptions& o, bool& ok) {
    if (key == "--frames") {
        ok = isInt(val) && (o.frames = std::atoi(val.c_str())) > 0;
        o.used = true;
        return true;
    }
    if (key == "--ack-delay") {
        ok = isInt(val) && (o.ackDelay = std::atoi(val.c_str())) >= 0;
        o.used = true;
        return true;
    }
    if (key == "--at") {
        ok = val.find(':') != std::string::npos && isInt(val.substr(0, val.find(':')));
        o.events.push_back(val);
        o.used = true;
        return true;
    }
    for (const OptionField& m : kStartOptions) {
        if (key != m.opt) continue;
        o.used = true;
        std::string raw;
        switch (m.kind) {
        case Kind::Str: ok = isWord(val), raw = "\"" + val + "\""; break;
        case Kind::Int: ok = isInt(val), raw = val; break;
        case Kind::Hex: ok = isInt(val, true), raw = std::to_string(std::strtoull(val.c_str(), nullptr, 0)); break;
        case Kind::Num: ok = isNumber(val), raw = val; break;
        case Kind::Bool: ok = val == "0" || val == "1", raw = val == "1" ? "true" : "false"; break;
        }
        startFields() += std::string(",\"") + m.json + "\":" + raw;
        return true;
    }
    return false;
}

namespace {

// --- A local frame ring (what recon-host's encoder.Launch creates) ---------------

class LocalRing {
public:
    ~LocalRing() {
        if (base_) UnmapViewOfFile(base_);
        if (mapping_) CloseHandle(mapping_);
        if (event_) CloseHandle(event_);
    }
    Status create(uint32_t slots, uint32_t slotSize) {
        using namespace ring;
        slots_ = slots;
        slotSize_ = slotSize;
        size_ = uint64_t(kHeaderSize) + uint64_t(slots) * slotSize;
        mapping_ = CreateFileMappingW(INVALID_HANDLE_VALUE, nullptr, PAGE_READWRITE, DWORD(size_ >> 32), DWORD(size_), nullptr);
        event_ = CreateEventW(nullptr, FALSE, FALSE, nullptr);
        if (!mapping_ || !event_) return Status::Error("ring", "cannot create the test ring: " + win32ErrorText(GetLastError()));
        base_ = static_cast<uint8_t*>(MapViewOfFile(mapping_, FILE_MAP_READ | FILE_MAP_WRITE, 0, 0, size_t(size_)));
        if (!base_) return Status::Error("ring", "cannot map the test ring: " + win32ErrorText(GetLastError()));
        put<uint64_t>(kOffMagic, kMagic);
        put<uint32_t>(kOffVersion, kVersion);
        put<uint32_t>(kOffHeaderSize, kHeaderSize);
        put<uint32_t>(kOffSlotCount, slots);
        put<uint32_t>(kOffSlotSize, slotSize);
        put<uint64_t>(kOffTotalSize, size_);
        put<uint32_t>(kOffSlotHeaderSize, kSlotHeaderSize);
        return Status::Ok();
    }
    HANDLE mapping() const { return mapping_; }
    HANDLE event() const { return event_; }
    uint64_t size() const { return size_; }

    struct Slot {
        uint64_t frameId = 0, refFloor = 0;
        uint32_t flags = 0, droppedBefore = 0, refLtrMask = 0, width = 0, height = 0, gen = 0;
        int32_t ltrSlot = -1;
        int64_t captureQpc = 0, submitQpc = 0, outputQpc = 0;
        size_t bytes = 0;
        std::vector<uint8_t> payload;
    };
    // Reads every published slot (the consumer side of docs/HELPER_PROTOCOL.md).
    bool readAll(std::vector<Slot>& out, std::string& err) {
        using namespace ring;
        const uint64_t w = std::atomic_ref<uint64_t>(*reinterpret_cast<uint64_t*>(base_ + kOffWriteCount)).load(std::memory_order_acquire);
        if (w < read_ || w - read_ > slots_) {
            err = "ring counters inconsistent";
            return false;
        }
        while (read_ < w) {
            const uint8_t* s = base_ + kHeaderSize + (read_ % slots_) * uint64_t(slotSize_);
            Slot x;
            const uint32_t off = get<uint32_t>(s + kSlotPayloadOffset), size = get<uint32_t>(s + kSlotPayloadSize);
            if (get<uint64_t>(s + kSlotSeq) != read_ || off < kSlotHeaderSize || off > slotSize_ || size > slotSize_ - off) {
                err = "bad slot header";
                return false;
            }
            x.frameId = get<uint64_t>(s + kSlotFrameId);
            x.flags = get<uint32_t>(s + kSlotFlags);
            x.gen = get<uint32_t>(s + kSlotGen);
            x.captureQpc = get<int64_t>(s + kSlotCaptureQpc);
            x.submitQpc = get<int64_t>(s + kSlotSubmitQpc);
            x.outputQpc = get<int64_t>(s + kSlotOutputQpc);
            x.refFloor = get<uint64_t>(s + kSlotRefFloor);
            x.ltrSlot = get<int32_t>(s + kSlotLtrSlot);
            x.refLtrMask = get<uint32_t>(s + kSlotRefLtrMask);
            x.droppedBefore = get<uint32_t>(s + kSlotDroppedBefore);
            x.width = get<uint32_t>(s + kSlotWidth);
            x.height = get<uint32_t>(s + kSlotHeight);
            x.bytes = size;
            x.payload.assign(s + off, s + off + size);
            out.push_back(std::move(x));
            ++read_;
            std::atomic_ref<uint64_t>(*reinterpret_cast<uint64_t*>(base_ + kOffReadCount)).store(read_, std::memory_order_release);
        }
        return true;
    }

private:
    template <typename T>
    void put(size_t off, T v) {
        std::memcpy(base_ + off, &v, sizeof(v));
    }
    template <typename T>
    static T get(const uint8_t* p) {
        T v;
        std::memcpy(&v, p, sizeof(v));
        return v;
    }
    HANDLE mapping_ = nullptr, event_ = nullptr;
    uint8_t* base_ = nullptr;
    uint64_t size_ = 0, read_ = 0;
    uint32_t slots_ = 0, slotSize_ = 0;
};

class TestReporter : public Reporter {
public:
    void stats(const FrameStats& s) override {
        if (s.dropped) {
            std::lock_guard<std::mutex> lock(mu_);
            ++dropped_;
        }
    }
    void error(const Status& s, std::string_view re) override {
        logf(LogLevel::Warn, "%s%s%s: %s", std::string(re).c_str(), re.empty() ? "" : ": ", s.code.c_str(), s.text.c_str());
        std::lock_guard<std::mutex> lock(mu_);
        ++errors_;
    }
    void fatal(const Status& s) override {
        logf(LogLevel::Error, "fatal: %s: %s", s.code.c_str(), s.text.c_str());
        fatal_ = true;
    }
    void captureChanged(const CaptureEvent& ev) override {
        logf(LogLevel::Info, "capture %s: %dx%d %s", ev.reason.c_str(), ev.width, ev.height, ev.text.c_str());
    }
    bool fatalRaised() const { return fatal_; }
    int errors() {
        std::lock_guard<std::mutex> lock(mu_);
        return errors_;
    }
    uint64_t dropped() {
        std::lock_guard<std::mutex> lock(mu_);
        return dropped_;
    }

private:
    std::mutex mu_;
    int errors_ = 0;
    uint64_t dropped_ = 0;
    std::atomic<bool> fatal_{false};
};

struct Event {
    uint64_t at = 0;
    std::string what;  // idr | loss | rate | fps | roi | roi-off
    int value = 0;
    RoiRect rect;
    bool done = false;
    uint64_t firedAt = 0;  // frame id it fired at
    // loss outcome
    uint64_t recoveredAt = 0, refFloor = 0, lostFrames = 0;
    uint32_t refMask = 0;
    bool byLtr = false;
};

bool parseEvent(const std::string& s, Event& e) {
    const size_t c = s.find(':');
    e.at = std::strtoull(s.substr(0, c).c_str(), nullptr, 10);
    const std::string w = s.substr(c + 1);
    if (w == "idr" || w == "loss" || w == "roi=off") {
        e.what = w == "roi=off" ? "roi-off" : w;
        return true;
    }
    if (w.rfind("rate=", 0) == 0 || w.rfind("fps=", 0) == 0) {
        e.what = w.substr(0, w.find('='));
        const std::string v = w.substr(w.find('=') + 1);
        e.value = std::atoi(v.c_str());
        return isInt(v) && e.value > 0;
    }
    if (w.rfind("roi=", 0) == 0) {
        e.what = "roi";
        int* fields[] = {&e.rect.x, &e.rect.y, &e.rect.w, &e.rect.h, &e.rect.weight};
        size_t pos = 4;
        for (size_t i = 0; i < 5; ++i) {
            const size_t comma = w.find(',', pos);
            const std::string v = w.substr(pos, comma == std::string::npos ? std::string::npos : comma - pos);
            if (!isInt(v) || (i < 4) == (comma == std::string::npos)) return false;
            *fields[i] = std::atoi(v.c_str());
            pos = comma + 1;
        }
        return true;
    }
    return false;
}

// IVF (the AV1 test file): 32-byte header, then per frame a 12-byte header.
void ivfHeader(std::FILE* f, uint32_t w, uint32_t h, uint32_t fps, uint32_t frames) {
    uint8_t b[32] = {'D', 'K', 'I', 'F', 0, 0, 32, 0, 'A', 'V', '0', '1'};
    auto le16 = [&](int off, uint32_t v) { b[off] = uint8_t(v), b[off + 1] = uint8_t(v >> 8); };
    auto le32 = [&](int off, uint32_t v) { le16(off, v), le16(off + 2, v >> 16); };
    le16(12, w), le16(14, h), le32(16, fps), le32(20, 1), le32(24, frames);
    std::fwrite(b, 1, sizeof(b), f);
}

double percentile(std::vector<double> v, double p) {
    if (v.empty()) return 0;
    std::sort(v.begin(), v.end());
    return v[std::min(v.size() - 1, size_t(p * double(v.size() - 1) + 0.5))];
}

}  // namespace

int runEncodeTest(EncodeTestOptions& o, BackendChoice& choice) {
    // The start message, validated by the real parser.
    std::string json = "{\"t\":\"start\"";
    if (startFields().find("\"codec\"") == std::string::npos) json += ",\"codec\":\"hevc\"";
    if (startFields().find("\"kbps\"") == std::string::npos) json += ",\"kbps\":20000";
    json += startFields() + "}";
    ControlMsg m;
    Status s = parseControl(json, m);
    if (!s.ok) {
        std::printf("encode-test: bad options: %s\n", s.text.c_str());
        return 2;
    }
    o.start = m.start;
    const StartParams& p = o.start;
    std::vector<Event> events;
    for (const std::string& e : o.events) {
        Event ev;
        if (!parseEvent(e, ev)) {
            std::printf("encode-test: bad --at=%s\n", e.c_str());
            return 2;
        }
        events.push_back(ev);
    }
    Codec codec;
    parseCodec(p.codec, codec);
    std::printf("encode-test: backend %s (%s), %s\n", choice.caps.backend.c_str(), choice.caps.adapterName.c_str(), json.c_str());
    if (auto it = choice.caps.codecs.find(p.codec); it != choice.caps.codecs.end()) {
        const CodecCaps& c = it->second;
        std::printf("encode-test: caps %s: max %dx%d, 10-bit %d, recovery %s (maxLtr %d), liveBitrate %s, roi %s, hwInstances %d, "
                    "queryTimeout %d, temporal layers %d, slice/tile output %d, align %dx%d\n",
                    p.codec.c_str(), c.maxW, c.maxH, c.tenBit, c.recovery.c_str(), c.maxLtr, c.liveBitrate.c_str(), c.roi.c_str(),
                    c.hwInstances, c.queryTimeout, c.maxTemporalLayers, c.sliceOutput, c.alignW, c.alignH);
    }

    LocalRing ring;
    RingWriter writer;
    s = ring.create(8, 4u << 20);
    if (s.ok) s = writer.attach(ring.mapping(), ring.size(), ring.event());
    if (!s.ok) {
        std::printf("encode-test: %s\n", s.text.c_str());
        return 2;
    }
    TestReporter rep;
    StartResult sr;
    Started st;
    s = startStream(p, choice, writer, rep, "", sr, st);
    if (!s.ok) {
        std::printf("encode-test: start failed: %s: %s\n", s.code.c_str(), s.text.c_str());
        return 2;
    }
    std::printf("encode-test: %s\n", encodeStarted(st).c_str());
    // Teardown in main.cpp's order: threads, then encoder, then capture (which
    // may own the encoder's AMF context).
    const auto teardown = [&] {
        sr.pipeline->stop();
        sr.pipeline.reset();
        choice.backend.reset();
        sr.capture.reset();
    };
    std::FILE* file = nullptr;
    if (_wfopen_s(&file, fromUtf8(o.output).c_str(), L"wb") != 0 || !file) {
        std::printf("encode-test: cannot write %s\n", o.output.c_str());
        teardown();
        return 2;
    }
    const uint32_t codedW = uint32_t(st.codedWidth ? st.codedWidth : st.width), codedH = uint32_t(st.codedHeight ? st.codedHeight : st.height);
    if (codec == Codec::Av1) ivfHeader(file, codedW, codedH, uint32_t(p.fps), 0);
    std::fflush(stdout);

    sr.pipeline->start();
    const int64_t freq = qpcFrequency(), t0 = qpcNow();
    const int64_t limit = t0 + freq * (int64_t(o.frames) * 4 / std::max(1, p.fps) + 15);
    std::vector<LocalRing::Slot> got, all;
    std::vector<std::pair<uint64_t, uint64_t>> pendingAcks;  // frame id, due at frame id
    std::optional<size_t> lossEvent;
    uint64_t lossFrom = 0, written = 0, lastId = 0;
    int kbps = p.kbps;
    bool timedOut = false;
    std::string err;
    while (!rep.fatalRaised()) {
        WaitForSingleObject(ring.event(), 100);
        got.clear();
        if (!ring.readAll(got, err)) {
            std::printf("encode-test: %s\n", err.c_str());
            break;
        }
        for (LocalRing::Slot& f : got) {
            lastId = f.frameId;
            // Scripted events, when their frame comes out.
            for (size_t i = 0; i < events.size(); ++i) {
                Event& e = events[i];
                if (e.done || f.frameId < e.at || (e.what == "loss" && lossEvent)) continue;  // one loss at a time
                e.done = true;
                e.firedAt = f.frameId;
                Status es;
                if (e.what == "idr") {
                    es = choice.backend->forceIdr();
                } else if (e.what == "loss") {
                    // This frame and the ones after it never reach the
                    // "client" until a key frame or a recovery frame from
                    // before the loss; the helper hears about it at once.
                    lossEvent = i;
                    lossFrom = f.frameId;
                    es = choice.backend->recover(lossFrom, std::nullopt);
                } else if (e.what == "rate") {
                    kbps = e.value;
                    es = sr.pipeline->setRate(RateParams{kbps, 0, 0});
                } else if (e.what == "fps") {
                    es = sr.pipeline->setRate(RateParams{kbps, 0, e.value});
                } else if (e.what == "roi") {
                    es = choice.backend->setRoi({e.rect});
                } else if (e.what == "roi-off") {
                    es = choice.backend->setRoi({});
                }
                if (!es.ok) std::printf("encode-test: event %s at %llu: %s\n", e.what.c_str(), static_cast<unsigned long long>(e.at), es.text.c_str());
            }
            const bool key = f.flags & ring::kFlagKey, recovery = f.flags & ring::kFlagRecovery;
            if (lossEvent) {
                Event& e = events[*lossEvent];
                if (key || (recovery && f.refFloor < lossFrom)) {
                    e.recoveredAt = f.frameId;
                    e.byLtr = !key;
                    e.refFloor = f.refFloor;
                    e.refMask = f.refLtrMask;
                    e.lostFrames = f.frameId - lossFrom;
                    lossEvent.reset();
                } else {
                    f.payload.clear();
                    all.push_back(std::move(f));
                    continue;  // discarded by the "client"
                }
            }
            // The "client" decodes it: write it, acknowledge LTR frames later.
            if (codec == Codec::Av1) {
                uint8_t h[12];
                const uint32_t n = uint32_t(f.payload.size());
                const uint64_t pts = written;
                for (int b = 0; b < 4; ++b) h[b] = uint8_t(n >> (8 * b));
                for (int b = 0; b < 8; ++b) h[4 + b] = uint8_t(pts >> (8 * b));
                std::fwrite(h, 1, sizeof(h), file);
            }
            std::fwrite(f.payload.data(), 1, f.payload.size(), file);
            ++written;
            if (f.ltrSlot >= 0) pendingAcks.emplace_back(f.frameId, f.frameId + uint64_t(o.ackDelay));
            for (auto it = pendingAcks.begin(); it != pendingAcks.end();) {
                if (it->second <= f.frameId) {
                    choice.backend->ack(it->first);
                    it = pendingAcks.erase(it);
                } else {
                    ++it;
                }
            }
            f.payload.clear();
            all.push_back(std::move(f));
        }
        if (lastId >= uint64_t(o.frames)) break;
        if (qpcNow() > limit) {
            timedOut = true;
            break;
        }
    }
    teardown();
    if (codec == Codec::Av1) {
        std::fseek(file, 0, SEEK_SET);
        ivfHeader(file, codedW, codedH, uint32_t(p.fps), uint32_t(written));
    }
    std::fclose(file);

    // --- Summary ---------------------------------------------------------------
    // Sizes come from the stats-free ring copy: recompute from the frames kept.
    std::vector<double> encodeMs, captureMs;
    std::vector<uint64_t> keys;
    uint64_t recoveries = 0, marks = 0, droppedBefore = 0;
    for (const auto& f : all) {
        if (f.outputQpc && f.submitQpc) encodeMs.push_back(double(f.outputQpc - f.submitQpc) * 1000.0 / double(freq));
        if (f.outputQpc && f.captureQpc) captureMs.push_back(double(f.outputQpc - f.captureQpc) * 1000.0 / double(freq));
        if (f.flags & ring::kFlagKey) keys.push_back(f.frameId);
        recoveries += (f.flags & ring::kFlagRecovery) != 0;
        marks += f.ltrSlot >= 0;
        droppedBefore += f.droppedBefore;
    }
    std::printf("encode-test: %zu frames received (last id %llu), %llu written to %s, %llu dropped by the helper (%llu flagged "
                "in the ring), %d errors\n",
                all.size(), static_cast<unsigned long long>(lastId), static_cast<unsigned long long>(written), o.output.c_str(),
                static_cast<unsigned long long>(rep.dropped()), static_cast<unsigned long long>(droppedBefore), rep.errors());
    std::printf("encode-test: submit->output ms p50 %.2f p95 %.2f max %.2f; capture->output ms p50 %.2f p95 %.2f\n",
                percentile(encodeMs, 0.5), percentile(encodeMs, 0.95), percentile(encodeMs, 1.0), percentile(captureMs, 0.5),
                percentile(captureMs, 0.95));
    std::string keyList;
    for (uint64_t k : keys) keyList += (keyList.empty() ? "" : " ") + std::to_string(k);
    std::printf("encode-test: key frames at %s; %llu recovery frames; %llu LTR marks\n", keyList.c_str(),
                static_cast<unsigned long long>(recoveries), static_cast<unsigned long long>(marks));
    bool ok = !rep.fatalRaised() && !timedOut && !all.empty() && !keys.empty() && keys.front() == all.front().frameId;
    if (timedOut) std::printf("encode-test: FAIL timed out\n");
    if (!keys.empty() && !all.empty() && keys.front() != all.front().frameId) std::printf("encode-test: FAIL the first frame is not a key frame\n");
    // Average bitrate over [from, to) frame ids, key frames left out.
    const auto kbpsOver = [&](uint64_t from, uint64_t to, int fps) {
        uint64_t bytes = 0, n = 0;
        for (const auto& f : all) {
            if (f.frameId < from || f.frameId >= to || (f.flags & ring::kFlagKey)) continue;
            bytes += f.bytes;
            ++n;
        }
        return n ? double(bytes) * 8.0 * fps / double(n) / 1000.0 : 0.0;
    };
    int fpsNow = p.fps;
    for (const Event& e : events) {
        const auto id = [](uint64_t v) { return static_cast<unsigned long long>(v); };
        if (!e.done) {
            std::printf("encode-test: FAIL event %s at %llu never fired\n", e.what.c_str(), id(e.at));
            ok = false;
            continue;
        }
        if (e.what == "idr") {
            const auto k = std::find_if(keys.begin(), keys.end(), [&](uint64_t x) { return x > e.firedAt; });
            const bool hit = k != keys.end() && *k <= e.firedAt + 5;
            std::printf("encode-test: idr requested at %llu: %s%s\n", id(e.firedAt), hit ? "key frame " : "FAIL no key frame within 5 frames",
                        hit ? std::to_string(*k).c_str() : "");
            ok = ok && hit;
        } else if (e.what == "loss") {
            if (!e.recoveredAt) {
                std::printf("encode-test: FAIL loss at %llu: never recovered\n", id(e.firedAt));
                ok = false;
            } else {
                std::printf("encode-test: loss at %llu: recovered at %llu (%llu frames lost) %s refFloor %llu, LTR mask 0x%x\n",
                            id(e.firedAt), id(e.recoveredAt), id(e.lostFrames), e.byLtr ? "from an LTR (no IDR):" : "by an IDR;",
                            id(e.refFloor), e.refMask);
            }
        } else if (e.what == "rate" || e.what == "fps") {
            const int fpsAfter = e.what == "fps" ? e.value : fpsNow;
            const double before = kbpsOver(e.firedAt > 30 ? e.firedAt - 30 : 0, e.firedAt, fpsNow);
            const double after = kbpsOver(e.firedAt + 3, e.firedAt + 33, fpsAfter);
            const auto k = std::find_if(keys.begin(), keys.end(), [&](uint64_t x) { return x >= e.firedAt && x <= e.firedAt + 5; });
            const std::string keyText =
                k != keys.end() ? "key frame " + std::to_string(*k) + " follows (expected only with liveBitrate flush)" : "no key frame";
            std::printf("encode-test: %s %d at %llu: %s; P-frame bitrate %.0f kbps before, %.0f kbps after (frames +3..+33)\n",
                        e.what.c_str(), e.value, id(e.firedAt), keyText.c_str(), before, after);
            fpsNow = fpsAfter;
        } else {
            std::printf("encode-test: %s at %llu applied\n", e.what.c_str(), id(e.firedAt));
        }
    }
    std::printf("encode-test: check: ffprobe -v error -show_frames -show_entries frame=key_frame,pict_type,pkt_size %s\n"
                "encode-test:        ffmpeg -v error -i %s -f null -   (prints nothing when the stream decodes cleanly)\n",
                o.output.c_str(), o.output.c_str());
    std::printf("encode-test: %s\n", ok ? "ok" : "FAIL");
    std::fflush(stdout);
    return ok ? 0 : 1;
}

}  // namespace recon
