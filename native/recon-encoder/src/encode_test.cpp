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

#include <nlohmann/json.hpp>

#include "codec/bitstream.hpp"
#include "d3d/device.hpp"
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
    {"--hdr", "hdr", Kind::Bool},
    {"--intra-refresh", "intraRefreshFrames", Kind::Int},
    {"--svc", "svcLayers", Kind::Int},
    {"--reencode", "reencodeOversized", Kind::Num},
    {"--slices", "sliceOutput", Kind::Int},
    {"--monitor", "monitor", Kind::Int},
    {"--hmonitor", "hmonitor", Kind::Hex},
    {"--motion", "motion", Kind::Bool},
    {"--test-format", "testFormat", Kind::Str},
};

// "A,B,..." -> non-negative integers.
bool parseInts(const std::string& v, std::vector<int>& out) {
    out.clear();
    size_t pos = 0;
    for (;;) {
        const size_t comma = v.find(',', pos);
        const std::string part = v.substr(pos, comma == std::string::npos ? std::string::npos : comma - pos);
        if (!isInt(part) || part[0] == '-') return false;
        out.push_back(std::atoi(part.c_str()));
        if (comma == std::string::npos) return true;
        pos = comma + 1;
    }
}

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
    if (key == "--dxgi-gate") {
        ok = val == "0" || val == "1";
        o.dxgiGate = val != "0";
        o.used = true;
        return true;
    }
    if (key == "--rate-schedule") {
        // K1[,K2...]:N
        const size_t colon = val.rfind(':');
        ok = colon != std::string::npos && parseInts(val.substr(0, colon), o.rateLevels) && isInt(val.substr(colon + 1)) &&
             (o.rateEvery = std::atoi(val.substr(colon + 1).c_str())) > 0 &&
             std::all_of(o.rateLevels.begin(), o.rateLevels.end(), [](int k) { return k > 0 && k <= 2000000; });
        o.used = true;
        return true;
    }
    if (key == "--frame-log") {
        ok = !(o.frameLog = val).empty();
        o.used = true;
        return true;
    }
    if (key == "--barcode") {
        // X,Y,CELL: the start's barcode (validated by the start parser).
        std::vector<int> v;
        ok = parseInts(val, v) && v.size() == 3;
        if (ok) {
            startFields() += ",\"barcode\":{\"x\":" + std::to_string(v[0]) + ",\"y\":" + std::to_string(v[1]) +
                             ",\"cell\":" + std::to_string(v[2]) + "}";
        }
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
        uint32_t flags = 0, droppedBefore = 0, refLtrMask = 0, width = 0, height = 0, gen = 0, temporalLayer = 0, dirtyPpm = 0;
        int32_t ltrSlot = -1;
        int64_t captureQpc = 0, submitQpc = 0, outputQpc = 0;
        size_t bytes = 0;
        bool written = false;  // went into the output file (not discarded after a simulated loss)
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
            x.temporalLayer = get<uint32_t>(s + kSlotTemporalLayer);
            x.dirtyPpm = get<uint32_t>(s + kSlotDirtyPpm);
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
        std::lock_guard<std::mutex> lock(mu_);
        if (s.dropped) ++dropped_;
        if (s.reencoded) {
            ++reencoded_;
            largestFirst_ = std::max<uint64_t>(largestFirst_, s.oversizeBytes);
            reencodedBytes_.push_back({s.oversizeBytes, s.bytes});
        }
        if (s.slices > 0 && s.firstSliceQpc && s.outputQpc >= s.firstSliceQpc) {
            slicesTotal_ += uint64_t(s.slices);
            ++slicedFrames_;
            firstSliceMs_.push_back(double(s.outputQpc - s.firstSliceQpc) * 1000.0 / double(qpcFrequency()));
            if (s.submitQpc) firstPartMs_.push_back(double(s.firstSliceQpc - s.submitQpc) * 1000.0 / double(qpcFrequency()));
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
    // Phase 5: re-encoded frames (first encode size, sent size), sub-frame output timing.
    struct Phase5 {
        uint64_t reencoded = 0, largestFirst = 0, slicesTotal = 0, slicedFrames = 0;
        std::vector<std::pair<uint64_t, uint64_t>> reencodedBytes;
        std::vector<double> firstSliceMs, firstPartMs;
    };
    Phase5 phase5() {
        std::lock_guard<std::mutex> lock(mu_);
        return {reencoded_, largestFirst_, slicesTotal_, slicedFrames_, reencodedBytes_, firstSliceMs_, firstPartMs_};
    }

private:
    std::mutex mu_;
    int errors_ = 0;
    uint64_t dropped_ = 0;
    uint64_t reencoded_ = 0, largestFirst_ = 0, slicesTotal_ = 0, slicedFrames_ = 0;
    std::vector<std::pair<uint64_t, uint64_t>> reencodedBytes_;
    std::vector<double> firstSliceMs_, firstPartMs_;
    std::atomic<bool> fatal_{false};
};

struct Event {
    uint64_t at = 0;
    std::string what;  // idr | loss | rate | fps | roi | roi-off
    int value = 0;
    std::vector<RoiRect> rects;
    bool done = false;
    uint64_t firedAt = 0;  // frame id it fired at
    // loss outcome
    uint64_t recoveredAt = 0, refFloor = 0, lostFrames = 0;
    uint32_t refMask = 0;
    bool byLtr = false;  // by a recovery frame (AMF: an LTR reference; NVENC: invalidation, refMask 0), not a key frame
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
        // X,Y,W,H,WEIGHT, several joined with '+' (e.g. the cursor / crosshair
        // rects of encoder.FocusROI).
        e.what = "roi";
        std::string list = w.substr(4);
        for (size_t start = 0; start <= list.size();) {
            const size_t plus = list.find('+', start);
            const std::string one = list.substr(start, plus == std::string::npos ? std::string::npos : plus - start);
            RoiRect r;
            int* fields[] = {&r.x, &r.y, &r.w, &r.h, &r.weight};
            size_t pos = 0;
            for (size_t i = 0; i < 5; ++i) {
                const size_t comma = one.find(',', pos);
                const std::string v = one.substr(pos, comma == std::string::npos ? std::string::npos : comma - pos);
                if (!isInt(v) || (i < 4) == (comma == std::string::npos)) return false;
                *fields[i] = std::atoi(v.c_str());
                pos = comma + 1;
            }
            e.rects.push_back(r);
            if (plus == std::string::npos) break;
            start = plus + 1;
        }
        return !e.rects.empty() && e.rects.size() <= 256;
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
    if (!o.dxgiGate) {
        // Before any capture or encoder thread runs (d3d/device.hpp).
        d3d::dxgiGate().disable();
        std::printf("encode-test: dxgi gate off: DDA AcquireNextFrame and NVENC NvEncLockBitstream / NvEncUnlockBitstream are not "
                    "serialized (--dxgi-gate=0)\n");
    }
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
    // Temporal SVC: a second file with the frames a congested recon-host
    // would keep (the discardable ones left out), which must decode cleanly.
    std::FILE* baseFile = nullptr;
    std::string basePath;
    if (st.svcLayers > 1) {
        const size_t slash = o.output.find_last_of("/\\"), dot = o.output.find_last_of('.');
        basePath = dot != std::string::npos && (slash == std::string::npos || dot > slash) ? o.output.substr(0, dot) + ".base" + o.output.substr(dot)
                                                                                           : o.output + ".base";
        if (_wfopen_s(&baseFile, fromUtf8(basePath).c_str(), L"wb") != 0) baseFile = nullptr;
        if (baseFile && codec == Codec::Av1) ivfHeader(baseFile, codedW, codedH, uint32_t(p.fps), 0);
    }
    uint64_t baseWritten = 0;
    const auto writeFrame = [&](std::FILE* to, const std::vector<uint8_t>& payload, uint64_t index) {
        if (codec == Codec::Av1) {
            uint8_t h[12];
            const uint32_t n = uint32_t(payload.size());
            for (int b = 0; b < 4; ++b) h[b] = uint8_t(n >> (8 * b));
            for (int b = 0; b < 8; ++b) h[4 + b] = uint8_t(index >> (8 * b));
            std::fwrite(h, 1, sizeof(h), to);
        }
        std::fwrite(payload.data(), 1, payload.size(), to);
    };
    std::fflush(stdout);

    // The rate schedule: set on the capture thread right before the frame
    // that starts each step is submitted (Pipeline::setBeforeSubmit).
    std::mutex scheduleMu;
    std::vector<std::pair<uint64_t, int>> rateChanges;  // first frame id at the rate, kbps
    if (o.rateEvery > 0 && !o.rateLevels.empty()) {
        Pipeline* pl = sr.pipeline.get();
        sr.pipeline->setBeforeSubmit([&, pl](uint64_t id) {
            const uint64_t every = uint64_t(o.rateEvery);
            if (id <= 1 || (id - 1) % every != 0) return;
            const int next = o.rateLevels[size_t(((id - 1) / every - 1) % o.rateLevels.size())];
            {
                std::lock_guard<std::mutex> lock(scheduleMu);
                if (!rateChanges.empty() && rateChanges.back().first == id) return;  // the same frame again (encoder busy)
                rateChanges.emplace_back(id, next);
            }
            const Status rs = pl->setRate(RateParams{next, 0, 0});
            if (!rs.ok) std::printf("encode-test: rate schedule: %d kbps at frame %llu: %s\n", next, static_cast<unsigned long long>(id), rs.text.c_str());
        });
    }

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
                    sr.pipeline->forceIdr();  // as main.cpp does for "forceIdr"
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
                    // A frame-rate change alone (kbps 0 = unchanged): "FPS before resolution".
                    es = sr.pipeline->setRate(RateParams{0, 0, e.value});
                } else if (e.what == "roi") {
                    es = choice.backend->setRoi(e.rects);
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
            writeFrame(file, f.payload, written);
            ++written;
            if (baseFile && !(f.flags & ring::kFlagDiscardable)) writeFrame(baseFile, f.payload, baseWritten++);
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
            f.written = true;
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
    if (baseFile) {
        if (codec == Codec::Av1) {
            std::fseek(baseFile, 0, SEEK_SET);
            ivfHeader(baseFile, codedW, codedH, uint32_t(p.fps), uint32_t(baseWritten));
        }
        std::fclose(baseFile);
    }

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
    // Phase 5: temporal layers, dirty share, re-encoded frames, sub-frame output.
    {
        uint64_t layers[4] = {}, discardable = 0, dirtyKnown = 0, dirtyZero = 0, dirtyUnknown = 0;
        double dirtySum = 0, dirtyMax = 0;
        for (const auto& f : all) {
            layers[std::min<uint32_t>(f.temporalLayer, 3)]++;
            discardable += (f.flags & ring::kFlagDiscardable) != 0;
            // Frame 1 is the first image of the duplication: wholly new (dirty 1).
            if (f.frameId <= 1) continue;
            if (f.flags & ring::kFlagDirty) {
                const double d = double(f.dirtyPpm) / 1e6;
                ++dirtyKnown;
                dirtySum += d;
                dirtyMax = std::max(dirtyMax, d);
                dirtyZero += f.dirtyPpm == 0;
            } else {
                ++dirtyUnknown;
            }
        }
        if (st.svcLayers > 1) {
            std::printf("encode-test: temporal layers: %llu / %llu / %llu / %llu frames in layer 0 / 1 / 2 / 3, %llu discardable; %llu frames "
                        "without them in %s (check: ffmpeg -v error -i %s -f null -)\n",
                        static_cast<unsigned long long>(layers[0]), static_cast<unsigned long long>(layers[1]),
                        static_cast<unsigned long long>(layers[2]), static_cast<unsigned long long>(layers[3]),
                        static_cast<unsigned long long>(discardable), static_cast<unsigned long long>(baseWritten), basePath.c_str(),
                        basePath.c_str());
        }
        if (dirtyKnown) {
            std::printf("encode-test: dirty share after frame 1: mean %.4f, max %.4f, %llu of %llu frames unchanged (%llu frames "
                        "without dirty information)\n",
                        dirtySum / double(dirtyKnown), dirtyMax, static_cast<unsigned long long>(dirtyZero),
                        static_cast<unsigned long long>(dirtyKnown), static_cast<unsigned long long>(dirtyUnknown));
        }
        const TestReporter::Phase5 p5 = rep.phase5();
        if (p.reencodeOversized > 0) {
            std::string list;
            for (size_t i = 0; i < p5.reencodedBytes.size() && i < 8; ++i) {
                list += (list.empty() ? "" : ", ") + std::to_string(p5.reencodedBytes[i].first) + " -> " + std::to_string(p5.reencodedBytes[i].second);
            }
            std::printf("encode-test: re-encoded %llu frames above %.1f average frames (bytes: %s)\n",
                        static_cast<unsigned long long>(p5.reencoded), p.reencodeOversized, list.empty() ? "none" : list.c_str());
        }
        if (p.sliceOutput > 0) {
            std::printf("encode-test: sub-frame output: %.1f parts per frame; first part -> whole frame ms p50 %.2f p95 %.2f; submit -> "
                        "first part ms p50 %.2f\n",
                        p5.slicedFrames ? double(p5.slicesTotal) / double(p5.slicedFrames) : 0.0, percentile(p5.firstSliceMs, 0.5),
                        percentile(p5.firstSliceMs, 0.95), percentile(p5.firstPartMs, 0.5));
        }
    }
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
                            id(e.firedAt), id(e.recoveredAt), id(e.lostFrames),
                            !e.byLtr ? "by an IDR;" : e.refMask ? "from an LTR (no IDR):" : "by reference invalidation (no IDR):",
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
    if (o.rateEvery > 0) {
        std::lock_guard<std::mutex> lock(scheduleMu);
        std::printf("encode-test: rate schedule: %zu changes every %d frames (%s kbps), first at frame %llu\n", rateChanges.size(),
                    o.rateEvery, [&] {
                        std::string l = std::to_string(p.kbps);
                        for (int k : o.rateLevels) l += " -> " + std::to_string(k);
                        return l;
                    }().c_str(),
                    static_cast<unsigned long long>(rateChanges.empty() ? 0 : rateChanges.front().first));
    }
    if (!o.frameLog.empty()) {
        // One JSON object per line: started, the frames, the end counters.
        std::FILE* log = nullptr;
        if (_wfopen_s(&log, fromUtf8(o.frameLog).c_str(), L"wb") != 0 || !log) {
            std::printf("encode-test: FAIL cannot write the frame log %s\n", o.frameLog.c_str());
            ok = false;
        } else {
            std::lock_guard<std::mutex> lock(scheduleMu);
            std::fprintf(log, "%s\n", encodeStarted(st).c_str());
            size_t change = 0;
            int target = p.kbps;
            for (const auto& f : all) {
                while (change < rateChanges.size() && rateChanges[change].first <= f.frameId) target = rateChanges[change++].second;
                nlohmann::ordered_json j = {{"t", "frame"},
                                            {"id", f.frameId},
                                            {"gen", f.gen},
                                            {"key", (f.flags & ring::kFlagKey) != 0},
                                            {"seqStart", (f.flags & ring::kFlagSeqStart) != 0},
                                            {"recovery", (f.flags & ring::kFlagRecovery) != 0},
                                            {"repeat", (f.flags & ring::kFlagRepeat) != 0},
                                            {"bytes", f.bytes},
                                            {"droppedBefore", f.droppedBefore},
                                            {"written", f.written},
                                            {"kbps", target},
                                            {"captureQpc", f.captureQpc},
                                            {"submitQpc", f.submitQpc},
                                            {"outputQpc", f.outputQpc}};
                std::fprintf(log, "%s\n", j.dump().c_str());
            }
            nlohmann::ordered_json changes = nlohmann::ordered_json::array();
            for (const auto& [id, k] : rateChanges) changes.push_back({{"frameId", id}, {"kbps", k}});
            nlohmann::ordered_json end = {{"t", "end"},
                                          {"frames", all.size()},
                                          {"lastId", lastId},
                                          {"written", written},
                                          {"droppedByHelper", rep.dropped()},
                                          {"errors", rep.errors()},
                                          {"fatal", rep.fatalRaised()},
                                          {"timedOut", timedOut},
                                          {"qpcFrequency", freq},
                                          {"rateChanges", changes}};
            std::fprintf(log, "%s\n", end.dump().c_str());
            std::fclose(log);
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
