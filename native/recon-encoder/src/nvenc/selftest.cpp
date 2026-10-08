// recon-encoder --self-test-nvenc[=DLL]: the NVENC backend driven the way the
// pipeline drives it (init; NV12 textures submitted on one thread, the output
// collected on another; forceIdr / recover / setRate / setRoi; shutdown and
// release), against
// - DLL = the test double test/fake_nvenc.cpp (recon-fake-nvenc.dll, built
//   next to the helper): no NVIDIA GPU needed (Wine, CI on windows-latest with
//   WARP). It checks the NVENC API rules and logs every call, so the test also
//   asserts what the backend asked NVENC for (the settings of GUIDE 3.4, the
//   invalidated frames, the reconfigurations, the teardown), and its DPB model
//   names the frame each frame was predicted from;
// - no DLL: the NVIDIA driver (System32's nvEncodeAPI64.dll), the hardware
//   check of docs/VENDOR_NOTES.md 3.4: key frames exactly where forced, loss
//   recovery by invalidation without an IDR, rate changes without one, the
//   flush mode, every codec and preset the GPU has; and of 3.9: HDR10 streams
//   (P010 input, Main10 / AV1 10-bit, the HDR metadata).
// Exit code 0 ok, 1 failed, 77 could not run (no D3D11 device, no NVENC runtime
// or no NVIDIA adapter).
#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <cstdio>
#include <memory>
#include <mutex>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include "backend.hpp"
#include "codec/bitstream.hpp"
#include "codec/hdr.hpp"
#include "d3d/device.hpp"
#include "nvenc/nvenc_policy.hpp"
#include "nvenc/nvenc_runtime.hpp"
#include "probes.hpp"
#include "selftest.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

int failures = 0;

void report(const std::string& name, int before) { std::printf("  %-44s %s\n", name.c_str(), failures == before ? "ok" : "FAIL"); }

void expect(bool cond, const std::string& test, const std::string& what) {
    if (cond) return;
    ++failures;
    std::printf("  %s: FAIL %s\n", test.c_str(), what.c_str());
}

bool contains(const std::string& s, const std::string& x) { return s.find(x) != std::string::npos; }

// "key=value" in a log line or a frame marker ("" if missing).
std::string field(const std::string& line, const std::string& key) {
    const std::string k = key + "=";
    for (size_t p = line.find(k); p != std::string::npos; p = line.find(k, p + 1)) {
        if (p > 0 && line[p - 1] != ' ') continue;
        const size_t b = p + k.size(), e = line.find(' ', b);
        return line.substr(b, e == std::string::npos ? std::string::npos : e - b);
    }
    return "";
}

uint64_t num(const std::string& s) { return std::strtoull(s.c_str(), nullptr, 10); }

// The test double's control export.
struct FakeDriver {
    using ControlFn = int (*)(const char*, char*, int);
    ControlFn fn = nullptr;

    std::string call(const std::string& cmd) {
        if (!fn) return "";
        std::vector<char> buf(size_t(1) << 22);
        const int n = fn(cmd.c_str(), buf.data(), int(buf.size()));
        if (n < 0) {
            ++failures;
            std::printf("  fake driver: command failed: %s\n", cmd.c_str());
            return "";
        }
        return std::string(buf.data(), std::min(size_t(n), buf.size() - 1));
    }
    // Log lines starting with prefix.
    std::vector<std::string> log(const std::string& prefix) {
        std::vector<std::string> out;
        std::istringstream in(call("log"));
        for (std::string line; std::getline(in, line);) {
            if (line.rfind(prefix, 0) == 0) out.push_back(line);
        }
        return out;
    }
    // "encode" lines by frame id, and "invalidate" ids in log order.
    std::string encodeLine(uint64_t ts) {
        for (const std::string& l : log("encode ")) {
            if (num(field(l, "ts")) == ts) return l;
        }
        return "";
    }
};

struct Got {
    uint64_t frameId = 0;
    bool key = false, recovery = false;
    uint64_t refFloor = 0;
    uint32_t gen = 0;
    std::string data;
};

// One encoder backend fed like the pipeline feeds it: init() and the control
// calls on the calling (main) thread, submit() on a capture thread of its
// own, receive() on an output thread.
class Harness {
public:
    Harness(ID3D11Device* device, const AdapterInfo& adapter) : device_(device), adapter_(adapter) {}
    ~Harness() {
        stop();
        if (capture_.joinable()) {
            {
                std::lock_guard<std::mutex> lock(jobMu_);
                quit_ = true;
            }
            jobCv_.notify_all();
            capture_.join();
        }
    }

    // hdrSource: the source is an output in Windows HDR mode (a 1000 cd/m2 panel).
    bool start(const StartParams& p, Started& st, Status& s, bool hdrSource = false) {
        backend_ = createNvencBackend(s);
        if (!backend_) return false;
        SourceInfo src;
        src.width = uint32_t(p.width);
        src.height = uint32_t(p.height);
        src.device = device_;
        src.adapter = adapter_;
        if (hdrSource) {
            src.hdr = true;
            src.display.known = src.display.hdr = true;
            src.display.minLuminance = 0.005, src.display.maxLuminance = 1000, src.display.maxFullFrameLuminance = 400;
        }
        InputSpec in;
        s = backend_->init(p, src, in, st);
        if (!s.ok) {
            backend_.reset();
            return false;
        }
        const InputSpec::Format want = st.hdr ? InputSpec::Format::P010 : InputSpec::Format::Nv12;
        if (in.format != want || in.width != src.width || in.height != src.height) {
            s = Status::Error("test", "the backend asked for an unexpected input");
            return false;
        }
        // Pool textures like the converter's (d3d/convert.cpp): NV12 (P010 for
        // HDR10), render target + shader resource. The test double takes any
        // texture where the device has no NV12 / P010 (Wine).
        for (int i = 0; i < 6; ++i) {
            D3D11_TEXTURE2D_DESC td{};
            td.Width = in.width;
            td.Height = in.height;
            td.MipLevels = td.ArraySize = 1;
            td.SampleDesc.Count = 1;
            td.Usage = D3D11_USAGE_DEFAULT;
            td.Format = st.hdr ? DXGI_FORMAT_P010 : DXGI_FORMAT_NV12;
            td.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
            ComPtr<ID3D11Texture2D> t;
            HRESULT hr = device_->CreateTexture2D(&td, nullptr, t.GetAddressOf());
            if (FAILED(hr)) {
                td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
                hr = device_->CreateTexture2D(&td, nullptr, t.GetAddressOf());
                static bool warned[2] = {false, false};
                if (!warned[st.hdr]) {
                    std::printf("  (no %s textures on this device: BGRA stand-ins for the test double)\n", st.hdr ? "P010" : "NV12");
                }
                warned[st.hdr] = true;
            }
            if (FAILED(hr)) {
                s = Status::Error("test", "CreateTexture2D: " + d3d::hrText(hr));
                return false;
            }
            tex_.push_back(t);
            busy_.push_back(std::make_shared<std::atomic<int>>(0));
        }
        stop_ = false;
        out_ = std::thread([this] { outputLoop(); });
        return true;
    }

    // Submits frame `id` on the capture thread and waits for the result.
    Status submit(uint64_t id) {
        std::unique_lock<std::mutex> lock(jobMu_);
        if (!capture_.joinable()) capture_ = std::thread([this] { captureLoop(); });
        job_ = id;
        hasJob_ = true;
        jobCv_.notify_all();
        jobCv_.wait(lock, [this] { return !hasJob_; });
        return jobResult_;
    }

    // Capture thread: from a free pool texture; retries while the encoder is
    // busy (the pipeline would drop the capture, keeping the id).
    Status submitHere(uint64_t id) {
        const int64_t deadline = qpcNow() + 2 * qpcFrequency();
        for (;;) {
            int free = -1;
            for (size_t i = 0; i < tex_.size() && free < 0; ++i) {
                if (*busy_[i] == 0) free = int(i);
            }
            if (free >= 0) {
                std::shared_ptr<std::atomic<int>> count = busy_[size_t(free)];
                ++*count;
                EncoderFrame ef;
                ef.nv12 = tex_[size_t(free)].Get();
                ef.hold = std::shared_ptr<void>(reinterpret_cast<void*>(uintptr_t(free + 1)), [count](void*) { --*count; });
                ef.poolIndex = free;
                SubmitInfo info;
                info.frameId = id;
                info.captureQpc = info.submitQpc = qpcNow();
                Status s = backend_->submit(ef, info);
                ef = EncoderFrame{};
                if (s.code != "encoder_busy") return s;
            }
            if (qpcNow() > deadline) return Status::Error("test", "frame " + std::to_string(id) + ": no free texture / encoder busy for 2 s");
            Sleep(1);
        }
    }

    // Waits until frame id (or a later one) came out.
    bool waitFor(uint64_t id, int ms = 3000) {
        const int64_t deadline = qpcNow() + int64_t(ms) * qpcFrequency() / 1000;
        while (qpcNow() < deadline) {
            {
                std::lock_guard<std::mutex> lock(mu_);
                if (!got_.empty() && got_.back().frameId >= id) return true;
            }
            Sleep(1);
        }
        return false;
    }

    void stop() {
        if (!backend_) return;
        backend_->shutdown();
        if (out_.joinable()) out_.join();
        backend_.reset();  // release(): EOS, unmap, unregister, destroy
    }

    Backend& backend() { return *backend_; }
    std::vector<Got> got() {
        std::lock_guard<std::mutex> lock(mu_);
        return got_;
    }
    std::vector<Status> errors() {
        std::lock_guard<std::mutex> lock(mu_);
        return errors_;
    }
    int holds() const {
        int n = 0;
        for (const auto& b : busy_) n += *b;
        return n;
    }

private:
    void captureLoop() {
        std::unique_lock<std::mutex> lock(jobMu_);
        for (;;) {
            jobCv_.wait(lock, [this] { return hasJob_ || quit_; });
            if (quit_) return;
            const uint64_t id = job_;
            lock.unlock();
            const Status r = submitHere(id);
            lock.lock();
            jobResult_ = r;
            hasJob_ = false;
            jobCv_.notify_all();
        }
    }

    void outputLoop() {
        while (!stop_) {
            EncodedFrame f;
            Status err;
            const Next r = backend_->receive(f, 100, err);
            if (r == Next::Frame) {
                Got g;
                g.frameId = f.info.frameId;
                g.key = f.key;
                g.recovery = f.recovery;
                g.refFloor = f.refFloor;
                g.gen = f.gen;
                g.data.assign(reinterpret_cast<const char*>(f.data), f.size);
                backend_->releaseOutput(f);
                std::lock_guard<std::mutex> lock(mu_);
                got_.push_back(std::move(g));
            } else if (r == Next::Error) {
                std::lock_guard<std::mutex> lock(mu_);
                errors_.push_back(err);
                if (err.fatal) return;
            } else if (r == Next::Stopped) {
                return;
            }
        }
    }

    ID3D11Device* device_;
    AdapterInfo adapter_;
    std::unique_ptr<Backend> backend_;
    std::vector<ComPtr<ID3D11Texture2D>> tex_;
    std::vector<std::shared_ptr<std::atomic<int>>> busy_;
    std::thread out_;
    std::atomic<bool> stop_{false};
    std::mutex mu_;
    std::vector<Got> got_;
    std::vector<Status> errors_;
    std::thread capture_;
    std::mutex jobMu_;
    std::condition_variable jobCv_;
    uint64_t job_ = 0;
    bool hasJob_ = false, quit_ = false;
    Status jobResult_;
};

struct Ctx {
    bool fake = false;
    FakeDriver driver;
    ID3D11Device* device = nullptr;
    AdapterInfo adapter;
    Caps caps;  // the cached probe's (what init() checks first)
    int delayMs = 2;  // between frames: about the encode time (fake), a frame interval (driver)

    bool has(const std::string& codec, uint32_t w, uint32_t h) {
        auto it = caps.codecs.find(codec);
        return it != caps.codecs.end() && int(w) <= it->second.maxW && int(h) <= it->second.maxH;
    }
};

std::string idList(const std::vector<uint64_t>& v) {
    std::string out;
    for (uint64_t x : v) out += (out.empty() ? "" : ",") + std::to_string(x);
    return out;
}

// The teardown left nothing behind and no rule was broken (test double only).
void expectClean(Ctx& c, const std::string& name) {
    if (!c.fake) return;
    const std::string v = c.driver.call("violations");
    expect(v.empty(), name, "API rule violations:\n" + v);
    const std::string st = c.driver.call("stats");
    expect(st == "sessions=0 buffers=0 events=0 registered=0 mapped=0", name, "left behind: " + st);
    // EOS flushes an encoder that encoded something ("The client must flush
    // the encoder before freeing any resources").
    const bool encoded = !c.driver.log("encode ").empty();
    expect(!c.driver.log("destroy").empty() && (!encoded || !c.driver.log("eos").empty()), name, "no NvEncDestroyEncoder / EOS");
    c.driver.call("reset");
}

// The main scenario: forced IDR, three kinds of loss, rate and frame-rate
// changes, ROI on and off.
void testStream(Ctx& c, const std::string& codec, uint32_t w, uint32_t h) {
    const std::string name = "stream " + codec + " " + std::to_string(w) + "x" + std::to_string(h);
    const int before = failures;
    if (!c.has(codec, w, h)) {
        std::printf("  %-44s skipped (not on this GPU)\n", name.c_str());
        return;
    }
    Harness hs(c.device, c.adapter);
    StartParams p;
    p.codec = codec;
    p.width = int(w);
    p.height = int(h);
    p.fps = 60;
    p.kbps = 20000;
    Started st;
    Status s;
    if (!hs.start(p, st, s)) {
        expect(false, name, "start: " + s.code + ": " + s.text);
        return;
    }
    const bool rfi = c.caps.codecs[codec].recovery == "invalidate";
    Codec cc{};
    parseCodec(codec, cc);
    const int refs = nvenc::dpbFramesFor(cc, w, h);
    expect(st.backend == "nvenc" && st.preset == "p" + std::to_string(nvenc::presetFor(w, h, 60, "speed")) && st.usage == "ultra_low_latency" &&
               st.refFrames == refs && st.rateControl == "cbr" && st.codedWidth == int(w) && st.codedHeight == int(h),
           name, "started: preset " + st.preset + ", usage " + st.usage + ", refFrames " + std::to_string(st.refFrames));
    if (c.fake) expect(st.asyncEncode && st.liveBitrate == "seamless" && st.hwInstances == 2, name, "started: async / liveBitrate / engines");
    Backend& b = hs.backend();
    uint64_t next = 1;
    auto run = [&](uint64_t last) {
        for (; next <= last; ++next) {
            const Status r = hs.submit(next);
            expect(r.ok, name, "submit " + std::to_string(next) + ": " + r.text);
            Sleep(DWORD(c.delayMs));
        }
    };
    auto loss = [&](uint64_t lostFrom, uint64_t outFirst) {
        expect(hs.waitFor(outFirst), name, "frame " + std::to_string(outFirst) + " did not come out");
        b.recover(lostFrom, std::nullopt);
    };
    run(10);
    b.forceIdr();
    run(25);
    loss(22, 25);  // invalidate 22..25, frame 26 references 21
    run(40);
    loss(33, 40);  // 33..40 is more than the DPB keeps: IDR at 41
    run(50);
    loss(41, 50);  // the key frame itself: IDR at 51
    run(60);
    loss(58, 60);  // 61 references 57
    run(61);
    loss(61, 61);  // the recovery frame lost too: 62 references 57 (58..60 stay invalid)
    run(70);
    b.setRate(RateParams{10000, 0, 0});
    run(75);
    b.setRate(RateParams{10000, 0, 30});
    run(80);
    b.setRoi({RoiRect{100, 100, 64, 64, 10}});
    run(85);
    b.setRoi({});
    run(100);
    expect(hs.waitFor(100), name, "frame 100 did not come out");
    hs.stop();

    const std::vector<Got> got = hs.got();
    std::vector<uint64_t> ids, keys, recoveries;
    for (const Got& g : got) {
        ids.push_back(g.frameId);
        if (g.key) keys.push_back(g.frameId);
        if (g.recovery) recoveries.push_back(g.frameId);
    }
    bool inOrder = got.size() == 100;
    for (size_t i = 0; inOrder && i < got.size(); ++i) inOrder = got[i].frameId == i + 1 && got[i].gen == 0;
    expect(inOrder, name, "frames out: " + std::to_string(got.size()) + ", not 1..100 in order");
    const std::vector<uint64_t> wantKeys = rfi ? std::vector<uint64_t>{1, 11, 41, 51} : std::vector<uint64_t>{1, 11, 26, 41, 51, 61, 62};
    expect(keys == wantKeys, name, "key frames at " + idList(keys) + ", expected " + idList(wantKeys));
    if (rfi) {
        expect(recoveries == std::vector<uint64_t>{26, 61, 62}, name, "recovery frames at " + idList(recoveries) + ", expected 26,61,62");
        for (const Got& g : got) {
            if (g.frameId == 26) expect(g.refFloor == 21, name, "frame 26 refFloor " + std::to_string(g.refFloor));
            if (g.frameId == 61 || g.frameId == 62) expect(g.refFloor == 57, name, "frame " + std::to_string(g.frameId) + " refFloor " + std::to_string(g.refFloor));
        }
    }
    for (const Got& g : got) {
        if (g.key) {
            expect(hasParameterSets(cc, reinterpret_cast<const uint8_t*>(g.data.data()), g.data.size()), name,
                   "key frame " + std::to_string(g.frameId) + " without parameter sets");
        }
    }
    const std::vector<Status> errors = hs.errors();
    expect(errors.empty(), name, errors.empty() ? "" : "output error: " + errors.front().text);
    expect(hs.holds() == 0, name, std::to_string(hs.holds()) + " pool textures still held after the teardown");

    if (c.fake) {
        // What the backend asked for.
        const std::vector<std::string> init = c.driver.log("init ");
        expect(init.size() == 1, name, "NvEncInitializeEncoder calls: " + std::to_string(init.size()));
        const std::string in = init.empty() ? "" : init.front();
        const nvenc::Rate r = nvenc::rateFor(20000, 1.0, 60);
        const std::string want = "codec=" + codec + " preset=" + std::to_string(nvenc::presetFor(w, h, 60, "speed")) +
                                 " tuning=3 async=1 ptd=1 size=" + std::to_string(w) + "x" + std::to_string(h);
        expect(contains(in, want), name, "init: " + in);
        for (const auto& [k, v] : std::vector<std::pair<std::string, std::string>>{
                 {"gop", "inf"}, {"pint", "1"}, {"rc", "cbr"}, {"avg", std::to_string(r.average)}, {"vbv", std::to_string(r.vbv)},
                 {"ldkfs", "3"}, {"aq", "1"}, {"taq", "0"}, {"qpmap", "2"}, {"zrd", "1"}, {"la", "0"}, {"mp", "1"}, {"idr", "inf"},
                 {"refs", std::to_string(refs)}, {"l0", "1"}, {"repeat", "1"}, {"vui", "1"}, {"ir", "0/0"}, {"layers", "1"}, {"profile", "set"},
                 {"level", codec == "av1" ? std::to_string(int(NV_ENC_LEVEL_AV1_AUTOSELECT)) : "0"}, {"maxsize", std::to_string(w) + "x" + std::to_string(h)}}) {
            expect(field(in, k) == v, name, "init " + k + "=" + field(in, k) + ", expected " + v);
        }
        expect(c.driver.log("register ").size() <= 6, name, "more registrations than pool textures");
        expect(field(c.driver.encodeLine(1), "flags") == "6" && field(c.driver.encodeLine(11), "flags") == "6", name,
               "forced IDRs without FORCEIDR | OUTPUT_SPSPPS");
        // Async: every lock after the frame's completion event, blocking
        // (doNotWait 0, NVENC guide 6.3's setting next to DXGI capture).
        const std::vector<std::string> locks = c.driver.log("lock ts");
        expect(locks.size() == 100, name, "locks: " + std::to_string(locks.size()));
        for (const std::string& l : locks) expect(field(l, "donotwait") == "0", name, "an async lock with doNotWait: " + l);
        // Invalidations, in log order with the encode they precede.
        std::vector<uint64_t> inval;
        for (const std::string& l : c.driver.log("invalidate ")) {
            inval.push_back(num(field(l, "ts")));
            expect(!contains(l, "never encoded") && !contains(l, "still encoding"), name, l);
        }
        expect(inval == std::vector<uint64_t>{22, 23, 24, 25, 58, 59, 60, 61}, name, "invalidated " + idList(inval));
        expect(c.driver.log("intra fallback").empty(), name, "the encoder ran out of references (a recovery without one)");
        expect(field(c.driver.encodeLine(41), "type") == "IDR" && field(c.driver.encodeLine(51), "type") == "IDR", name, "41 / 51 not IDR");
        const std::vector<std::string> reconf = c.driver.log("reconfigure ");
        const nvenc::Rate r2 = nvenc::rateFor(10000, 1.0, 60), r3 = nvenc::rateFor(10000, 1.0, 30);
        expect(reconf.size() == 2, name, "reconfigurations: " + std::to_string(reconf.size()));
        if (reconf.size() == 2) {
            expect(field(reconf[0], "reset") == "0" && field(reconf[0], "forceidr") == "0" && field(reconf[0], "avg") == std::to_string(r2.average) &&
                       field(reconf[0], "max") == std::to_string(r2.max) && field(reconf[0], "vbv") == std::to_string(r2.vbv) &&
                       field(reconf[0], "fps") == "60/1",
                   name, "setRate: " + reconf[0]);
            expect(field(reconf[1], "fps") == "30/1" && field(reconf[1], "vbv") == std::to_string(r3.vbv) && field(reconf[1], "forceidr") == "0",
                   name, "setRate fps: " + reconf[1]);
        }
        const nvenc::QpMap m = nvenc::roiQpDeltaMap(cc, w, h, {RoiRect{100, 100, 64, 64, 10}});
        size_t covered = 0;
        for (int8_t v : m.values) covered += v != 0;
        const std::string roiWant = std::to_string(m.values.size()) + "/" + std::to_string(covered) + "/" + std::to_string(nvenc::qpDeltaFor(cc, 10));
        const uint64_t roiFrames[] = {81, 85, 86};
        for (uint64_t id : roiFrames) {
            const std::string q = field(c.driver.encodeLine(id), "qpmap");
            expect(id <= 85 ? q == roiWant : q == "0/0/0", name, "frame " + std::to_string(id) + " qpmap " + q + (id <= 85 ? ", expected " + roiWant : ""));
        }
        // What each frame was predicted from (the double's DPB model): the
        // previous frame, the recovery frames refFloor, key frames nothing.
        for (const Got& g : got) {
            const size_t marker = g.data.find("NVFAKE");
            expect(marker != std::string::npos, name, "frame " + std::to_string(g.frameId) + " has no NVFAKE marker");
            if (marker == std::string::npos) continue;
            const uint64_t ref = num(field(g.data.substr(marker), "ref"));
            const uint64_t want2 = g.key ? 0 : g.recovery ? g.refFloor : g.frameId - 1;
            expect(ref == want2, name, "frame " + std::to_string(g.frameId) + " predicted from " + std::to_string(ref) + ", expected " + std::to_string(want2));
        }
        expectClean(c, name);
    }
    report(name, before);
}

// HDR10 (step 3.9): from an HDR source, HEVC Main10 / AV1 10-bit with P010
// input, the BT.2020 PQ colour description and the HDR metadata with every
// picture; forced IDRs and a loss still work. The test double also checks
// the refusals (H.264, no 10-bit encoding, no P010 input) and that an SDR
// source gives an SDR stream.
void testHdr(Ctx& c, const std::string& codec) {
    const std::string name = "HDR10 " + codec;
    const int before = failures;
    if (!c.has(codec, 1280, 720) || !c.caps.codecs[codec].hdr10) {
        std::printf("  %-44s skipped (no HDR10 %s on this GPU)\n", name.c_str(), codec.c_str());
        return;
    }
    Harness hs(c.device, c.adapter);
    StartParams p;
    p.codec = codec;
    p.width = 1280;
    p.height = 720;
    p.fps = 60;
    p.kbps = 20000;
    p.hdr = true;
    Started st;
    Status s;
    if (!hs.start(p, st, s, true)) {
        expect(false, name, "start: " + s.code + ": " + s.text);
        return;
    }
    const HdrMetadata& m = st.hdrMetadata ? *st.hdrMetadata : HdrMetadata{};
    expect(st.hdr && st.bitDepth == 10 && st.colorSpace == "bt2020-pq" && st.hdrMetadata && m.maxLuminance == 1000 && m.minLuminance == 0.005 &&
               m.maxCll == 1000 && m.maxFall == 400 && m.red[0] == 0.708 && m.white[1] == 0.3290,
           name, "started: hdr " + std::to_string(st.hdr) + ", bitDepth " + std::to_string(st.bitDepth) + ", " + st.colorSpace);
    Backend& b = hs.backend();
    for (uint64_t id = 1; id <= 30; ++id) {
        expect(hs.submit(id).ok, name, "submit " + std::to_string(id));
        if (id == 10) b.forceIdr();
        if (id == 20) {
            expect(hs.waitFor(20), name, "frame 20 did not come out");
            b.recover(19, std::nullopt);
        }
        Sleep(DWORD(c.delayMs));
    }
    expect(hs.waitFor(30), name, "frame 30 did not come out");
    hs.stop();
    std::vector<uint64_t> keys;
    for (const Got& g : hs.got()) {
        if (g.key) keys.push_back(g.frameId);
    }
    expect(hs.got().size() == 30 && keys.size() >= 2 && keys[0] == 1 && keys[1] == 11, name, "keys " + idList(keys));
    expect(hs.errors().empty(), name, hs.errors().empty() ? "" : "output error: " + hs.errors().front().text);
    if (c.fake) {
        const std::string in = c.driver.log("init ").empty() ? "" : c.driver.log("init ").back();
        expect(field(in, "bitdepth") == "10" && field(in, "hdrsei") == "1/1" && field(in, "vui") == "1" &&
                   field(in, "profile") == (codec == "hevc" ? "main10" : "set"),
               name, "init: " + in);
        // Every picture with the metadata, in the codec's units.
        const MasteringCodes mc = masteringCodes(m, codec == "av1");
        char want[128];
        std::snprintf(want, sizeof(want), "%u,%u,%u,%u,%u,%u,%u,%u,%u,%u", mc.green[0], mc.green[1], mc.blue[0], mc.blue[1], mc.red[0],
                      mc.red[1], mc.white[0], mc.white[1], mc.maxLuminance, mc.minLuminance);
        const std::vector<std::string> enc = c.driver.log("encode ");
        expect(enc.size() == 30, name, "encodes: " + std::to_string(enc.size()));
        for (const std::string& l : enc) {
            if (field(l, "md") != want || field(l, "cll") != "1000,400") {
                expect(false, name, "picture without the HDR metadata (want md=" + std::string(want) + " cll=1000,400): " + l);
                break;
            }
        }
        expectClean(c, name);

        // An SDR source: an SDR stream (8-bit, no metadata), not an error.
        Harness sdr(c.device, c.adapter);
        if (sdr.start(p, st, s, false)) {
            expect(!st.hdr && st.bitDepth == 8 && st.colorSpace == "bt709" && !st.hdrMetadata, name, "SDR source: started hdr");
            expect(sdr.submit(1).ok && sdr.waitFor(1), name, "SDR source: frame 1");
            sdr.stop();
            const std::string in8 = c.driver.log("init ").empty() ? "" : c.driver.log("init ").back();
            expect(field(in8, "bitdepth") == "8" && field(in8, "hdrsei") == "0/0" && field(in8, "profile") == "set", name, "SDR source: " + in8);
            expect(!contains(c.driver.encodeLine(1), " md="), name, "SDR source: metadata with a picture");
        } else {
            expect(false, name, "SDR source: start: " + s.text);
        }
        expectClean(c, name);
    }
    report(name, before);
}

// Output by polling NvEncLockBitstream (no async support), and the flush mode.
void testModes(Ctx& c) {
    std::string name = "sync output (no async encode support)";
    int before = failures;
    if (c.fake) {
        c.driver.call("set async=0");
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "h264";
        p.width = 1280;
        p.height = 720;
        Started st;
        Status s;
        if (!hs.start(p, st, s)) {
            expect(false, name, "start: " + s.text);
        } else {
            expect(!st.asyncEncode, name, "started.asyncEncode true");
            for (uint64_t id = 1; id <= 30; ++id) {
                expect(hs.submit(id).ok, name, "submit");
                if (id == 10) hs.backend().forceIdr();
                if (id == 16) {
                    expect(hs.waitFor(16), name, "frame 16 did not come out");
                    hs.backend().recover(14, std::nullopt);
                }
                Sleep(DWORD(c.delayMs));
            }
            expect(hs.waitFor(30), name, "frame 30 did not come out");
            hs.stop();
            std::vector<uint64_t> keys;
            uint64_t rec = 0, floor = 0;
            for (const Got& g : hs.got()) {
                if (g.key) keys.push_back(g.frameId);
                if (g.recovery) rec = g.frameId, floor = g.refFloor;
            }
            expect(hs.got().size() == 30 && keys == std::vector<uint64_t>{1, 11} && rec == 17 && floor == 13, name,
                   "keys " + idList(keys) + ", recovery " + std::to_string(rec) + " from " + std::to_string(floor));
            expect(field(c.driver.log("init ").front(), "async") == "0", name, "init async");
            for (const std::string& l : c.driver.log("lock ts")) expect(field(l, "donotwait") == "1", name, "a blocking lock: " + l);
            expectClean(c, name);
        }
        report(name, before);
    }

    name = "flush mode (reset + IDR on setRate)";
    before = failures;
    if (!c.has("hevc", 1280, 720)) return;
    if (c.fake || c.caps.codecs["hevc"].liveBitrate != "restart") {
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "hevc";
        p.width = 1280;
        p.height = 720;
        p.liveBitrate = "flush";
        Started st;
        Status s;
        if (!hs.start(p, st, s)) {
            expect(false, name, "start: " + s.text);
            return;
        }
        expect(st.liveBitrate == "flush", name, "started.liveBitrate " + st.liveBitrate);
        for (uint64_t id = 1; id <= 20; ++id) {
            expect(hs.submit(id).ok, name, "submit");
            if (id == 10) {
                expect(hs.waitFor(10), name, "frame 10 did not come out");
                expect(hs.backend().setRate(RateParams{5000, 0, 0}).ok, name, "setRate");
            }
            Sleep(DWORD(c.delayMs));
        }
        expect(hs.waitFor(20), name, "frame 20 did not come out");
        hs.stop();
        std::vector<uint64_t> keys;
        bool gens = true;
        for (const Got& g : hs.got()) {
            if (g.key) keys.push_back(g.frameId);
            gens = gens && g.gen == (g.frameId > 10 ? 1u : 0u);
        }
        expect(keys == std::vector<uint64_t>{1, 11} && gens, name, "keys " + idList(keys) + (gens ? "" : ", generations wrong"));
        if (c.fake) {
            const std::vector<std::string> rc = c.driver.log("reconfigure ");
            expect(rc.size() == 1 && field(rc.front(), "reset") == "1" && field(rc.front(), "forceidr") == "1" &&
                       field(rc.front(), "avg") == "5000000",
                   name, rc.empty() ? "no reconfigure" : rc.front());
            expect(c.driver.log("sequence params").size() == 2, name, "parameter sets not read again after the reset");
            expectClean(c, name);
        }
        report(name, before);
    }
}

// Preset by pixel rate and quality, per codec the GPU has.
void testPresets(Ctx& c) {
    const char* name = "preset by pixel rate";
    const int before = failures;
    struct Case {
        const char* codec;
        uint32_t w, h;
        int fps;
        const char* quality;
        const char* preset;
    };
    const Case cases[] = {{"h264", 1920, 1080, 60, "speed", "p4"},
                          {"hevc", 2560, 1440, 120, "quality", "p6"},
                          {"hevc", 3840, 2160, 90, "speed", "p2"},
                          {"h264", 3840, 2160, 60, "speed", "p4"},
                          {"av1", 3840, 2160, 120, "speed", "p1"}};
    for (const Case& k : cases) {
        if (!c.has(k.codec, k.w, k.h)) continue;
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = k.codec;
        p.width = int(k.w);
        p.height = int(k.h);
        p.fps = k.fps;
        p.quality = k.quality;
        p.kbps = 50000;
        Started st;
        Status s;
        if (!hs.start(p, st, s)) {
            expect(false, name, std::string(k.codec) + ": start: " + s.text);
            continue;
        }
        expect(st.preset == k.preset, name, std::string(k.codec) + " " + std::to_string(k.w) + "x" + std::to_string(k.h) + "@" +
                                                std::to_string(k.fps) + " " + k.quality + ": " + st.preset + ", expected " + k.preset);
        // The level 5.x DPB limit: 5 reference frames for H.264 / HEVC at 4K.
        Codec cc{};
        parseCodec(k.codec, cc);
        const int refs = nvenc::dpbFramesFor(cc, k.w, k.h);
        expect(st.refFrames == refs, name, std::string(k.codec) + " " + std::to_string(k.w) + "x" + std::to_string(k.h) + ": refFrames " +
                                               std::to_string(st.refFrames) + ", expected " + std::to_string(refs));
        for (uint64_t id = 1; id <= 3; ++id) expect(hs.submit(id).ok, name, "submit");
        expect(hs.waitFor(3), name, std::string(k.codec) + ": frame 3 did not come out");
        hs.stop();
        expect(!hs.got().empty() && hs.got().front().key, name, std::string(k.codec) + ": the first frame is not a key frame");
        if (c.fake) {
            const std::vector<std::string> pc = c.driver.log("presetconfig ");
            expect(!pc.empty() && field(pc.front(), "preset") == std::string(k.preset).substr(1) && field(pc.front(), "tuning") == "3", name,
                   pc.empty() ? "no NvEncGetEncodePresetConfigEx" : pc.front());
            const std::vector<std::string> init = c.driver.log("init ");
            expect(!init.empty() && field(init.front(), "refs") == std::to_string(refs), name,
                   init.empty() ? "no NvEncInitializeEncoder" : "init refs=" + field(init.front(), "refs"));
            expectClean(c, name);
        }
    }
    report(name, before);
}

// Test double only: version negotiation, caps mapping, and the paths a GPU
// cannot be asked to take (no invalidation, no live bitrate, a failing encode).
void testFakeOnly(Ctx& c, HMODULE module) {
    std::string name = "API version negotiation";
    int before = failures;
    c.driver.call("set maxVersion=12.2");
    NvencRuntime old = loadNvencRuntime(module);
    expect(!old.ok && contains(old.error, "12.2") && contains(old.error, "13.0") && contains(old.error, "570.0"), name,
           "driver 12.2: " + (old.ok ? std::string("accepted") : old.error));
    c.driver.call("set maxVersion=13.2");
    NvencRuntime newer = loadNvencRuntime(module);
    expect(newer.ok && contains(newer.versionText, "driver supports 13.2"), name, "driver 13.2: " + newer.error);
    c.driver.call("reset");
    report(name, before);

    name = "caps from NvEncGetEncodeCaps";
    before = failures;
    Caps caps = probeNvencCaps();
    const CodecCaps& h = caps.codecs["hevc"];
    expect(caps.backend == "nvenc" && caps.vendor == "nvidia" && caps.codecs.size() == 3, name, "backend / codecs");
    // maxLtr: the slots start's ltrSlots may ask for (none; the double reports 8 LTR frames).
    expect(h.maxW == 8192 && h.maxH == 8192 && h.tenBit && h.yuv444 && h.forceIdr && h.recovery == "invalidate" && h.maxLtr == 0 &&
               h.intraRefresh && h.liveBitrate == "seamless" && h.maxTemporalLayers == 4 && h.roi == "emphasis" && h.sliceOutput &&
               h.hwInstances == 2 && !h.queryTimeout && h.alignW == 1 && h.alignH == 1 && h.dynamicResolution,
           name, "hevc caps");
    expect(std::find(h.assumed.begin(), h.assumed.end(), "liveBitrate") != h.assumed.end() &&
               std::find(h.assumed.begin(), h.assumed.end(), "roi") != h.assumed.end(),
           name, "assumed");
    expect(!caps.codecs["av1"].yuv444, name, "av1 yuv444");
    expect(h.hdr10 && caps.codecs["av1"].hdr10 && !caps.codecs["h264"].hdr10, name, "hdr10: hevc / av1 yes, h264 no");
    c.driver.call("set av1=0 multiRef=0 dynBitrate=0 engines=3 dynRes=0");
    caps = probeNvencCaps();
    bool av1Why = false;
    for (const auto& [k, v] : caps.unavailable) av1Why = av1Why || (k == "nvenc-av1" && contains(v, "RTX 40"));
    const CodecCaps& h2 = caps.codecs["hevc"];
    expect(!caps.codecs.count("av1") && av1Why, name, "av1 missing without its reason");
    expect(h2.recovery == "none" && h2.liveBitrate == "restart" && h2.hwInstances == 3 && !h2.dynamicResolution &&
               std::find(h2.assumed.begin(), h2.assumed.end(), "liveBitrate") == h2.assumed.end(),
           name, "hevc caps without multiple references / live bitrate");
    c.driver.call("reset");
    c.driver.call("set p010=0");
    expect(!probeNvencCaps().codecs["hevc"].hdr10, name, "hdr10 without YUV420_10BIT input");
    c.driver.call("set p010=1 tenBit=0");
    caps = probeNvencCaps();
    expect(!caps.codecs["hevc"].hdr10 && !caps.codecs["hevc"].tenBit, name, "hdr10 without 10-bit encoding");
    c.driver.call("reset");
    report(name, before);

    // From an HDR and from an SDR output alike: the answer must not depend on
    // whether Windows HDR is on at the moment.
    name = "HDR10 refusals";
    before = failures;
    for (const bool hdrSource : {true, false}) {
        for (const auto& [codec, setting] : std::vector<std::pair<std::string, std::string>>{{"h264", ""}, {"hevc", "tenBit=0"}, {"av1", "p010=0"}}) {
            if (!setting.empty()) c.driver.call("set " + setting);
            Harness hs(c.device, c.adapter);
            StartParams p;
            p.codec = codec;
            p.width = 640;
            p.height = 360;
            p.hdr = true;
            Started st;
            Status s;
            expect(!hs.start(p, st, s, hdrSource) && s.code == "unsupported" && contains(s.text, "hdr10"), name,
                   codec + (setting.empty() ? "" : " with " + setting) + (hdrSource ? ", HDR" : ", SDR") + " source: " +
                       (s.ok ? "started" : s.code + ": " + s.text));
            c.driver.call("reset");
        }
    }
    report(name, before);

    name = "no invalidation: recovery by IDR";
    before = failures;
    {
        c.driver.call("set rfi=0");
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "hevc";
        p.width = 640;
        p.height = 360;
        Started st;
        Status s;
        if (hs.start(p, st, s)) {
            for (uint64_t id = 1; id <= 12; ++id) {
                expect(hs.submit(id).ok, name, "submit");
                if (id == 8) {
                    expect(hs.waitFor(8), name, "frame 8 did not come out");
                    hs.backend().recover(6, std::nullopt);
                }
                Sleep(DWORD(c.delayMs));
            }
            expect(hs.waitFor(12), name, "frame 12 did not come out");
            hs.stop();
            std::vector<uint64_t> keys;
            for (const Got& g : hs.got()) {
                if (g.key) keys.push_back(g.frameId);
                expect(!g.recovery, name, "a recovery frame without invalidation");
            }
            expect(keys == std::vector<uint64_t>{1, 9}, name, "keys " + idList(keys));
            expect(c.driver.log("invalidate ").empty(), name, "NvEncInvalidateRefFrames called");
            expectClean(c, name);
        } else {
            expect(false, name, "start: " + s.text);
        }
    }
    report(name, before);

    name = "invalidation after the frames in the encoder";
    before = failures;
    {
        // Encodes take 8 ms: at the loss two frames are still in the encoder,
        // and the recovery frame's invalidations must wait for them.
        c.driver.call("set encodeUs=8000");
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "hevc";
        p.width = 640;
        p.height = 360;
        Started st;
        Status s;
        if (hs.start(p, st, s)) {
            for (uint64_t id = 1; id <= 10; ++id) expect(hs.submit(id).ok, name, "submit");
            hs.backend().recover(9, std::nullopt);  // 9 and 10 still encoding
            for (uint64_t id = 11; id <= 14; ++id) expect(hs.submit(id).ok, name, "submit");
            expect(hs.waitFor(14), name, "frame 14 did not come out");
            hs.stop();
            uint64_t rec = 0, floor = 0;
            for (const Got& g : hs.got()) {
                if (g.recovery) rec = g.frameId, floor = g.refFloor;
            }
            expect(rec == 11 && floor == 8, name, "recovery frame " + std::to_string(rec) + " from " + std::to_string(floor));
            std::vector<uint64_t> inval;
            for (const std::string& l : c.driver.log("invalidate ")) {
                inval.push_back(num(field(l, "ts")));
                expect(!contains(l, "still encoding"), name, l);
            }
            expect(inval == std::vector<uint64_t>{9, 10}, name, "invalidated " + idList(inval));
            expectClean(c, name);
        } else {
            expect(false, name, "start: " + s.text);
        }
    }
    report(name, before);

    name = "fewer reference frames than asked (SPS)";
    before = failures;
    for (const char* codec : {"h264", "hevc"}) {
        // A driver that keeps only 3 reference frames, and says so in its
        // SPS: the backend reads it back before the first frame and recovers
        // only within those 3. Without that, the loss at 20 (20..22, the whole
        // 3-frame DPB) would be planned as a recovery from 19, a frame the
        // encoder no longer has: an intra frame flagged as a recovery.
        c.driver.call("set keepRefs=3");
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = codec;
        p.width = 640;
        p.height = 360;
        Started st;
        Status s;
        if (!hs.start(p, st, s)) {
            expect(false, name, std::string(codec) + ": start: " + s.text);
            c.driver.call("reset");
            continue;
        }
        expect(st.refFrames == nvenc::kDpbFrames, name, std::string(codec) + ": started.refFrames " + std::to_string(st.refFrames));
        for (uint64_t id = 1; id <= 30; ++id) {
            expect(hs.submit(id).ok, name, "submit");
            if (id == 12 || id == 22) {
                expect(hs.waitFor(id), name, "frame " + std::to_string(id) + " did not come out");
                hs.backend().recover(id == 12 ? 11 : 20, std::nullopt);
            }
            Sleep(DWORD(c.delayMs));
        }
        expect(hs.waitFor(30), name, "frame 30 did not come out");
        hs.stop();
        std::vector<uint64_t> keys, recoveries;
        for (const Got& g : hs.got()) {
            if (g.key) keys.push_back(g.frameId);
            if (g.recovery) {
                recoveries.push_back(g.frameId);
                expect(g.refFloor == 10, name, std::string(codec) + ": frame " + std::to_string(g.frameId) + " refFloor " + std::to_string(g.refFloor));
            }
            const size_t marker = g.data.find("NVFAKE");
            const uint64_t ref = marker == std::string::npos ? ~uint64_t(0) : num(field(g.data.substr(marker), "ref"));
            const uint64_t want = g.key ? 0 : g.recovery ? g.refFloor : g.frameId - 1;
            expect(ref == want, name, std::string(codec) + ": frame " + std::to_string(g.frameId) + " predicted from " + std::to_string(ref));
        }
        expect(hs.got().size() == 30 && keys == std::vector<uint64_t>{1, 23} && recoveries == std::vector<uint64_t>{13}, name,
               std::string(codec) + ": keys " + idList(keys) + ", recoveries " + idList(recoveries) + ", expected keys 1,23, recovery 13");
        expect(c.driver.log("intra fallback").empty(), name, std::string(codec) + ": a recovery without a reference left");
        expectClean(c, name);
    }
    report(name, before);

    name = "no live bitrate: restart";
    before = failures;
    {
        c.driver.call("set dynBitrate=0");
        Harness bad(c.device, c.adapter);
        StartParams p;
        p.codec = "hevc";
        p.width = 640;
        p.height = 360;
        p.liveBitrate = "seamless";
        Started st;
        Status s;
        expect(!bad.start(p, st, s) && s.code == "unsupported", name, "liveBitrate seamless accepted: " + s.text);
        p.liveBitrate.clear();
        Harness hs(c.device, c.adapter);
        if (hs.start(p, st, s)) {
            expect(st.liveBitrate == "restart", name, "started.liveBitrate " + st.liveBitrate);
            expect(hs.backend().setRate(RateParams{5000, 0, 0}).code == "unsupported", name, "setRate accepted");
            hs.stop();
        } else {
            expect(false, name, "start: " + s.text);
        }
        expectClean(c, name);
    }
    report(name, before);

    name = "a failing NvEncEncodePicture";
    before = failures;
    {
        c.driver.call("set failEncodeTs=5");
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "h264";
        p.width = 640;
        p.height = 360;
        Started st;
        Status s;
        if (hs.start(p, st, s)) {
            for (uint64_t id = 1; id <= 10; ++id) {
                const Status r = hs.submit(id);
                expect(id == 5 ? (!r.ok && !r.fatal && r.code == "encode_failed") : r.ok, name, "submit " + std::to_string(id) + ": " + r.text);
                Sleep(DWORD(c.delayMs));
            }
            expect(hs.waitFor(10), name, "frame 10 did not come out");
            hs.stop();
            std::vector<uint64_t> ids;
            for (const Got& g : hs.got()) ids.push_back(g.frameId);
            expect(ids == std::vector<uint64_t>{1, 2, 3, 4, 6, 7, 8, 9, 10}, name, "frames out " + idList(ids));
            expect(hs.holds() == 0, name, "pool texture of the failed frame still held");
            expectClean(c, name);
        } else {
            expect(false, name, "start: " + s.text);
        }
    }
    report(name, before);

    name = "start checks";
    before = failures;
    {
        Harness hs(c.device, c.adapter);
        StartParams p;
        p.codec = "hevc";
        p.width = 640;
        p.height = 360;
        p.ltrSlots = 2;
        Started st;
        Status s;
        expect(!hs.start(p, st, s) && s.code == "unsupported" && contains(s.text, "invalidation"), name, "ltrSlots accepted: " + s.text);
        p.ltrSlots = 0;
        p.width = 9000;
        expect(!hs.start(p, st, s) && s.code == "unsupported", name, "9000 wide accepted: " + s.text);
        p.width = 640;
        p.encoderInstance = 1;
        expect(!hs.start(p, st, s) && s.code == "unsupported", name, "encoderInstance 1 accepted: " + s.text);
        p.encoderInstance = -1;
        p.intraRefreshFrames = 30;
        p.svcLayers = 2;
        if (hs.start(p, st, s)) {
            expect(st.intraRefreshFrames == 30, name, "started.intraRefreshFrames");
            hs.stop();
            const std::string in = c.driver.log("init ").empty() ? "" : c.driver.log("init ").back();
            expect(field(in, "ir") == "30/29" && field(in, "layers") == "2", name, "intra refresh / layers: " + in);
        } else {
            expect(false, name, "start with intra refresh: " + s.text);
        }
        expectClean(c, name);
    }
    report(name, before);
}

}  // namespace

int runNvencSelfTest(const std::wstring& testDouble) {
    failures = 0;
    Ctx c;
    c.fake = !testDouble.empty();
    std::string err;
    if (c.fake && !useTestNvencRuntime(testDouble, err)) {
        std::printf("self-test-nvenc: %s\n", err.c_str());
        return 1;
    }
    const NvencRuntime& rt = nvencRuntime();
    if (!rt.ok) {
        std::printf("self-test-nvenc: skipped: %s\n", rt.error.c_str());
        return c.fake ? 1 : kSelfTestSkip;
    }
    if (c.fake) {
        c.driver.fn = reinterpret_cast<FakeDriver::ControlFn>(reinterpret_cast<void*>(GetProcAddress(rt.module, "ReconFakeNvencControl")));
        if (!c.driver.fn) {
            std::printf("self-test-nvenc: %ls is not the NVENC test double (no ReconFakeNvencControl)\n", testDouble.c_str());
            return 1;
        }
        c.driver.call("reset");
    }
    // The device: WARP (else the default adapter) for the test double, the
    // NVIDIA adapter for the driver.
    d3d::Device dev;
    Status s;
    if (c.fake) {
        s = d3d::createDevice(nullptr, dev, true);
        if (!s.ok) s = d3d::createDevice(nullptr, dev);
        c.adapter.found = true;
        c.adapter.vendor = "nvidia";
        c.adapter.name = "NVENC test double";
    } else {
        ComPtr<IDXGIFactory1> factory;
        ComPtr<IDXGIAdapter1> a;
        s = Status::Error("skip", "no NVIDIA adapter");
        if (SUCCEEDED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf())))) {
            for (UINT i = 0; factory->EnumAdapters1(i, a.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++i) {
                DXGI_ADAPTER_DESC1 d{};
                if (FAILED(a->GetDesc1(&d)) || d.VendorId != 0x10de) continue;
                c.adapter = describeAdapter(d.VendorId, d.AdapterLuid, d.Description);
                s = d3d::createDevice(a.Get(), dev);
                break;
            }
        }
    }
    if (!s.ok) {
        std::printf("self-test-nvenc: skipped: no D3D11 device: %s\n", s.text.c_str());
        return kSelfTestSkip;
    }
    c.device = dev.device.Get();
    c.delayMs = c.fake ? 2 : 16;
    Status be;
    if (auto b = createNvencBackend(be)) {
        c.caps = b->caps();
    } else {
        std::printf("self-test-nvenc: %s: %s\n", c.fake ? "FAIL" : "skipped", be.text.c_str());
        return c.fake ? 1 : kSelfTestSkip;
    }
    std::printf("self-test-nvenc: %s, %s\n", c.fake ? "test double" : c.adapter.name.c_str(), rt.versionText.c_str());
    if (c.fake) testFakeOnly(c, rt.module);
    testStream(c, "hevc", 1920, 1080);
    testStream(c, "h264", 1920, 1080);
    testStream(c, "av1", 1920, 1080);
    testHdr(c, "hevc");
    testHdr(c, "av1");
    testModes(c);
    testPresets(c);
    std::printf("self-test-nvenc: %s\n", failures ? "FAIL" : "ok");
    return failures ? 1 : 0;
}

}  // namespace recon
