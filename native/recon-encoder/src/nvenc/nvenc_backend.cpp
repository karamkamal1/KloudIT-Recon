// NVENC encoder backend (NVIDIA): H.264, HEVC and AV1 through the driver's
// nvEncodeAPI64.dll (nvenc/nvenc_runtime.cpp: System32 only, API version
// negotiated, NvEncodeAPICreateInstance). GUIDE 3.4.
//
// Set-up: NvEncOpenEncodeSessionEx on the capture's D3D11 device (the device
// DDA captures on and the NV12 converter renders on); caps from
// NvEncGetEncodeCaps; NvEncGetEncodePresetConfigEx(preset by pixel rate,
// NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY) as the base config, then GUIDE 3.4's
// settings (docs/HELPER_PROTOCOL.md "NVENC encoder backend" has the table):
// infinite GOP and IDR period, no B frames, CBR with a one-frame VBV, key frames
// about three P frames large, spatial AQ, quarter-resolution two-pass, a DPB of
// six frames (five where the level 5.x limit is lower: H.264 / HEVC at
// 3840x2160) with one reference per frame (room for reference invalidation),
// parameter sets with every IDR; NvEncInitializeEncoder; four bitstream
// buffers ("The number of IO buffers should be at least 4 + number of B
// frames", NVENC guide 6.1), each with its own completion event in async mode.
//
// Frames: the converter's NV12 pool textures, each registered once
// (NvEncRegisterResource, NV_ENC_INPUT_RESOURCE_TYPE_DIRECTX) and mapped per
// frame (NvEncMapInputResource, which also waits for the conversion's GPU work);
// unmapped after the frame's NvEncLockBitstream has returned ("The client must
// unmap the buffer after NvEncLockBitstream() API returns successfully"), when
// the pool texture goes back to the converter. inputTimeStamp is the frame id, which
// names the frame to NvEncInvalidateRefFrames. At most two frames are in the
// encoder (GUIDE 10: "NVIDIA async with events <= 2 in flight").
//
// Threads (the NVENC guide 6.3 model: the main encoder thread submits, a
// secondary thread waits for the completion events and locks the output):
//   control thread  forceIdr / recover / setRate / setRoi only record the
//                   request (ctlMu_, RfiTracker); no NVENC call.
//   capture thread  submit(): applies them (NvEncReconfigureEncoder,
//                   NvEncInvalidateRefFrames), registers and maps the input,
//                   NvEncEncodePicture, and NvEncGetSequenceParams (which "must
//                   [be called] from the same thread which is being used to
//                   call NvEncEncodePicture"). It never waits for output, except
//                   for the bounded drain before an invalidation.
//   output thread   receive(): waits for the oldest frame's completion event
//                   (async) or polls NvEncLockBitstream with doNotWait 1
//                   (sync); NvEncLockBitstream, the copy and
//                   NvEncUnlockBitstream run under d3d::dxgiGate(), so they
//                   never overlap the DDA capture thread's AcquireNextFrame
//                   (guide 6.3: "calling DXGI APIs like ... AcquireNextFrame
//                   from the primary thread and NvEncLockBitstream /
//                   NvEncUnlockBitstream from secondary thread, can lead to
//                   suboptimal or undefined behavior"); then, outside the
//                   gate, NvEncUnmapInputResource. The settings the same
//                   section prescribes for such applications are used:
//                   enableEncodeAsync 1 (where supported), doNotWait 0 for the
//                   lock after the completion event, output in system memory
//                   (enableOutputInVidmem 0).
//   main thread     init() / release(), while neither of the others runs.
// Every call on the session holds sessionMu_ (the capture and output threads'
// calls are short: async mode locks a bitstream only once its event has fired,
// so the blocking lock returns at once; sync mode polls), since
// NVENC documents the two-thread model but not which other calls may overlap.
// Lock order: d3d::dxgiGate(), then sessionMu_, then flightMu_ / ctlMu_.
#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstring>
#include <deque>
#include <map>
#include <memory>
#include <mutex>
#include <set>

#include "codec/bitstream.hpp"
#include "codec/rfi.hpp"
#include "d3d/device.hpp"
#include "nvenc/nvenc_policy.hpp"
#include "nvenc/nvenc_runtime.hpp"
#include "probes.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

constexpr int kOutputBuffers = 4;     // bitstream buffers / completion events (guide 6.1: at least 4)
constexpr size_t kMaxInFlight = 2;    // frames in the encoder at once (GUIDE 10)
constexpr int kMaxErrors = 10;        // consecutive failures before giving up (fatal)
constexpr int64_t kHangMs = 2000;     // a frame not finished after this long: the encoder hangs (fatal)
constexpr int kDrainBeforeInvalidateMs = 50;
constexpr int kTeardownMs = 200;      // release(): waiting for frames still in the encoder

bool sameGuid(const GUID& a, const GUID& b) { return std::memcmp(&a, &b, sizeof(GUID)) == 0; }

const GUID& codecGuid(Codec c) {
    switch (c) {
    case Codec::H264: return NV_ENC_CODEC_H264_GUID;
    case Codec::Hevc: return NV_ENC_CODEC_HEVC_GUID;
    case Codec::Av1: return NV_ENC_CODEC_AV1_GUID;
    }
    return NV_ENC_CODEC_HEVC_GUID;
}

// 8-bit 4:2:0 profiles, as the AMF backend uses (HDR / Main10 is step 3.9).
const GUID& profileGuid(Codec c) {
    switch (c) {
    case Codec::H264: return NV_ENC_H264_PROFILE_HIGH_GUID;
    case Codec::Hevc: return NV_ENC_HEVC_PROFILE_MAIN_GUID;
    case Codec::Av1: return NV_ENC_AV1_PROFILE_MAIN_GUID;
    }
    return NV_ENC_CODEC_PROFILE_AUTOSELECT_GUID;
}

const GUID& presetGuid(int p) {
    static const GUID* const presets[] = {&NV_ENC_PRESET_P1_GUID, &NV_ENC_PRESET_P2_GUID, &NV_ENC_PRESET_P3_GUID, &NV_ENC_PRESET_P4_GUID,
                                          &NV_ENC_PRESET_P5_GUID, &NV_ENC_PRESET_P6_GUID, &NV_ENC_PRESET_P7_GUID};
    return *presets[std::clamp(p, 1, 7) - 1];
}

std::string nvError(const std::string& what, NVENCSTATUS s) { return what + " failed: " + nvencStatusText(s); }

// NVENC refuses a session when the GPU has no more: GeForce drivers limit how
// many encode sessions run at once (other recorders, streamers, browsers).
std::string sessionHint(NVENCSTATUS s) {
    if (s == NV_ENC_ERR_OUT_OF_MEMORY || s == NV_ENC_ERR_INCOMPATIBLE_CLIENT_KEY || s == NV_ENC_ERR_NO_ENCODE_DEVICE) {
        return " (the GPU's limit of concurrent NVENC sessions may be reached: close other recorders / streamers)";
    }
    return "";
}

// Opens a session on a D3D11 device. "If the creation of encoder session
// fails, the client must call NvEncDestroyEncoder API before exiting."
NVENCSTATUS openSession(const NV_ENCODE_API_FUNCTION_LIST& nv, ID3D11Device* device, void*& enc) {
    NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS p{};
    p.version = NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER;
    p.deviceType = NV_ENC_DEVICE_TYPE_DIRECTX;
    p.device = device;
    p.apiVersion = NVENCAPI_VERSION;
    enc = nullptr;
    const NVENCSTATUS s = nv.nvEncOpenEncodeSessionEx(&p, &enc);
    if (s != NV_ENC_SUCCESS && enc) {
        nv.nvEncDestroyEncoder(enc);
        enc = nullptr;
    }
    return s;
}

std::vector<GUID> encodeGuids(const NV_ENCODE_API_FUNCTION_LIST& nv, void* enc) {
    uint32_t n = 0;
    if (nv.nvEncGetEncodeGUIDCount(enc, &n) != NV_ENC_SUCCESS || !n) return {};
    std::vector<GUID> g(n);
    uint32_t got = 0;
    if (nv.nvEncGetEncodeGUIDs(enc, g.data(), n, &got) != NV_ENC_SUCCESS) return {};
    g.resize(std::min(got, n));
    return g;
}

// --- Capabilities ------------------------------------------------------------------

struct CodecDetails {
    bool available = false;
    std::string reason;  // why not
    CodecCaps caps;
    int minW = 0, minH = 0;
    bool async = false;          // NV_ENC_CAPS_ASYNC_ENCODE_SUPPORT
    bool customVbv = false;      // NV_ENC_CAPS_SUPPORT_CUSTOM_VBV_BUF_SIZE
    bool multiRef = false;       // NV_ENC_CAPS_SUPPORT_MULTIPLE_REF_FRAMES
    bool cabac = false;          // NV_ENC_CAPS_SUPPORT_CABAC (H.264)
    bool emphasisMap = false;    // NV_ENC_CAPS_SUPPORT_EMPHASIS_LEVEL_MAP (H.264 only, not with AQ)
    bool stateAdvance = false;   // NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE (Phase 5 hook)
    bool dynBitrate = false;     // NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE
    bool singleSliceIntraRefresh = false;
    bool nv12 = false;           // NV_ENC_BUFFER_FORMAT_NV12 in NvEncGetInputFormats
    int ltrFrames = 0;           // NV_ENC_CAPS_NUM_MAX_LTR_FRAMES (logged; this backend uses no LTR)
};

// What NvEncGetEncodeCaps says about one codec on an open session.
CodecDetails readDetails(const NV_ENCODE_API_FUNCTION_LIST& nv, void* enc, Codec c, const std::vector<GUID>& guids) {
    CodecDetails d;
    const GUID& g = codecGuid(c);
    if (std::none_of(guids.begin(), guids.end(), [&](const GUID& x) { return sameGuid(x, g); })) {
        d.reason = std::string("the GPU cannot encode ") + codecName(c) + " (not in NvEncGetEncodeGUIDs" +
                   (c == Codec::Av1 ? "; AV1 encoding needs a GeForce RTX 40 series or newer)" : ")");
        return d;
    }
    auto cap = [&](NV_ENC_CAPS which) {
        NV_ENC_CAPS_PARAM p{};
        p.version = NV_ENC_CAPS_PARAM_VER;
        p.capsToQuery = which;
        int v = 0;
        return nv.nvEncGetEncodeCaps(enc, g, &p, &v) == NV_ENC_SUCCESS ? v : 0;
    };
    CodecCaps& cc = d.caps;
    cc.maxW = cap(NV_ENC_CAPS_WIDTH_MAX);
    cc.maxH = cap(NV_ENC_CAPS_HEIGHT_MAX);
    d.minW = cap(NV_ENC_CAPS_WIDTH_MIN);
    d.minH = cap(NV_ENC_CAPS_HEIGHT_MIN);
    cc.tenBit = cap(NV_ENC_CAPS_SUPPORT_10BIT_ENCODE) != 0;
    cc.yuv444 = cap(NV_ENC_CAPS_SUPPORT_YUV444_ENCODE) != 0;
    cc.forceIdr = true;  // NV_ENC_PIC_FLAG_FORCEIDR with enablePTD = 1
    d.multiRef = cap(NV_ENC_CAPS_SUPPORT_MULTIPLE_REF_FRAMES) != 0;
    // Invalidation needs older frames to fall back to: without multiple
    // reference frames Sunshine turns RFI off (nvenc_base.cpp configure_reference_frames).
    cc.recovery = cap(NV_ENC_CAPS_SUPPORT_REF_PIC_INVALIDATION) && d.multiRef ? "invalidate" : "none";
    // maxLtr is what start's ltrSlots may ask for (as with AMF): none, this
    // backend recovers by invalidation; the hardware's count is logged at start.
    cc.maxLtr = 0;
    d.ltrFrames = cap(NV_ENC_CAPS_NUM_MAX_LTR_FRAMES);
    cc.intraRefresh = cap(NV_ENC_CAPS_SUPPORT_INTRA_REFRESH) != 0;
    d.dynBitrate = cap(NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE) != 0;
    // NvEncReconfigureEncoder without a reset or an IDR (GUIDE 3.4); recon-host
    // qualify (step 3.6) measures it per codec and rate-control mode.
    cc.liveBitrate = d.dynBitrate ? "seamless" : "restart";
    if (d.dynBitrate) cc.assumed.push_back("liveBitrate");
    cc.maxTemporalLayers = cap(NV_ENC_CAPS_SUPPORT_TEMPORAL_SVC) ? std::max(1, cap(NV_ENC_CAPS_NUM_MAX_TEMPORAL_LAYERS)) : 1;
    // ROI by QP delta map (NV_ENC_QP_MAP_DELTA: every codec, alongside AQ; no
    // cap bit exists for it). The emphasis level map proper is H.264 only and
    // needs AQ off ("This feature is not supported when AQ (Spatial/Temporal)
    // is enabled. This feature is only supported for H264 codec currently",
    // nvEncodeAPI.h NV_ENC_RC_PARAMS::qpMapMode): logged, not used.
    cc.roi = "emphasis";
    cc.assumed.push_back("roi");
    d.emphasisMap = cap(NV_ENC_CAPS_SUPPORT_EMPHASIS_LEVEL_MAP) != 0;
    cc.sliceOutput = cap(NV_ENC_CAPS_SUPPORT_SUBFRAME_READBACK) != 0;
    cc.hwInstances = std::max(1, cap(NV_ENC_CAPS_NUM_ENCODER_ENGINES));
    cc.queryTimeout = false;  // AMF only; NVENC signals completion events
    cc.alignW = cc.alignH = 1;
    cc.dynamicResolution = cap(NV_ENC_CAPS_SUPPORT_DYN_RES_CHANGE) != 0;
    d.async = cap(NV_ENC_CAPS_ASYNC_ENCODE_SUPPORT) != 0;
    d.customVbv = cap(NV_ENC_CAPS_SUPPORT_CUSTOM_VBV_BUF_SIZE) != 0;
    d.cabac = cap(NV_ENC_CAPS_SUPPORT_CABAC) != 0;
    d.stateAdvance = cap(NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE) != 0;
    d.singleSliceIntraRefresh = cap(NV_ENC_CAPS_SINGLE_SLICE_INTRA_REFRESH) != 0;
    uint32_t n = 0;
    if (nv.nvEncGetInputFormatCount(enc, g, &n) == NV_ENC_SUCCESS && n) {
        std::vector<NV_ENC_BUFFER_FORMAT> f(n);
        uint32_t got = 0;
        if (nv.nvEncGetInputFormats(enc, g, f.data(), n, &got) == NV_ENC_SUCCESS) {
            for (uint32_t i = 0; i < std::min(got, n); ++i) d.nv12 = d.nv12 || f[i] == NV_ENC_BUFFER_FORMAT_NV12;
        }
    }
    d.available = cc.maxW > 0 && cc.maxH > 0 && d.nv12;
    if (!d.available) d.reason = cc.maxW <= 0 || cc.maxH <= 0 ? "the encoder reports no maximum size" : "the encoder takes no NV12 input";
    return d;
}

struct NvencProbe {
    bool ok = false;
    std::string reason;
    AdapterInfo adapter;
    std::map<Codec, CodecDetails> codecs;
};

// Opens a session once on the first NVIDIA adapter (a plain D3D11 device,
// released afterwards) and reads every codec's caps. The runtime's test double
// takes any adapter (--self-test-nvenc).
NvencProbe runNvencProbe() {
    NvencProbe p;
    const NvencRuntime& rt = nvencRuntime();
    if (!rt.ok) {
        p.reason = rt.error;
        return p;
    }
    ComPtr<IDXGIFactory1> factory;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf())))) {
        p.reason = "CreateDXGIFactory1 failed";
        return p;
    }
    // Adapter 0 (the primary display's) if it is NVIDIA, else the NVIDIA
    // adapter with the most video memory. init() reads the caps again on the
    // capture's own device.
    ComPtr<IDXGIAdapter1> adapter, a;
    SIZE_T bestMemory = 0;
    for (UINT i = 0; factory->EnumAdapters1(i, a.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++i) {
        DXGI_ADAPTER_DESC1 d{};
        if (FAILED(a->GetDesc1(&d)) || (d.Flags & DXGI_ADAPTER_FLAG_SOFTWARE)) continue;
        if (d.VendorId != 0x10de && !rt.testDouble) continue;
        if (adapter && i > 0 && d.DedicatedVideoMemory <= bestMemory) continue;
        adapter = a;
        bestMemory = d.DedicatedVideoMemory;
        p.adapter = describeAdapter(d.VendorId, d.AdapterLuid, d.Description);
        if (i == 0) break;
    }
    d3d::Device dev;
    Status s;
    if (adapter) {
        s = d3d::createDevice(adapter.Get(), dev);
    } else if (rt.testDouble) {
        s = d3d::createDevice(nullptr, dev, true);  // WARP
    } else {
        p.reason = rt.versionText + " found, but no NVIDIA adapter";
        return p;
    }
    if (!s.ok) {
        p.reason = "D3D11 device for the NVENC probe: " + s.text;
        return p;
    }
    void* enc = nullptr;
    const NV_ENCODE_API_FUNCTION_LIST& nv = rt.api;
    const NVENCSTATUS st = openSession(nv, dev.device.Get(), enc);
    if (st != NV_ENC_SUCCESS) {
        p.reason = nvError("NvEncOpenEncodeSessionEx on " + (p.adapter.found ? p.adapter.name : std::string("the test device")), st) +
                   sessionHint(st);
        return p;
    }
    const int64_t t0 = qpcNow();
    const std::vector<GUID> guids = encodeGuids(nv, enc);
    for (const Codec c : {Codec::H264, Codec::Hevc, Codec::Av1}) {
        p.codecs[c] = readDetails(nv, enc, c, guids);
        p.ok = p.ok || p.codecs[c].available;
    }
    nv.nvEncDestroyEncoder(enc);
    logf(LogLevel::Debug, "nvenc probe: %lld ms", static_cast<long long>((qpcNow() - t0) * 1000 / qpcFrequency()));
    if (!p.ok) p.reason = rt.versionText + ": no usable encoder on " + p.adapter.name;
    return p;
}

const NvencProbe& nvencProbe() {
    static const NvencProbe probe = runNvencProbe();
    return probe;
}

// --- Backend -------------------------------------------------------------------------

class NvencEncoder : public Backend {
public:
    NvencEncoder() : rt_(nvencRuntime()), nv_(rt_.api) {
        stopEvent_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
        freq_ = qpcFrequency();
    }
    ~NvencEncoder() override {
        release();
        if (stopEvent_) CloseHandle(stopEvent_);
    }
    const char* name() const override { return "nvenc"; }
    Caps caps() override;
    Status init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) override;
    void release() override;
    Status submit(const EncoderFrame& frame, const SubmitInfo& info) override;
    Next receive(EncodedFrame& out, int timeoutMs, Status& err) override;
    Status forceIdr() override;
    Status recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) override;
    Status setRate(const RateParams& r) override;
    Status setRoi(const std::vector<RoiRect>& rects) override;
    void shutdown() override;

private:
    struct OutputBuffer {
        NV_ENC_OUTPUT_PTR bitstream = nullptr;
        HANDLE event = nullptr;  // async mode: the completion event, registered with the session
        bool registered = false;
    };
    struct Registration {
        ComPtr<ID3D11Texture2D> texture;
        NV_ENC_REGISTERED_PTR registered = nullptr;
    };
    struct InFlight {
        SubmitInfo info;
        int slot = 0;
        NV_ENC_INPUT_PTR mapped = nullptr;
        std::shared_ptr<void> hold;                    // the converter's pool texture, reserved
        ComPtr<ID3D11Texture2D> texture;               // and alive
        std::shared_ptr<const nvenc::QpMap> qpMap;     // the ROI map passed with the frame
        bool forcedIdr = false;
        bool recovery = false;
        uint64_t refFloor = 0;
        uint32_t gen = 0;
        bool signaled = false;                         // async: its event fired (consumed) already
    };

    Status validate(const StartParams& p);
    Status configure();
    void configureCodec();
    Status createBuffers();
    void readSequenceParams();
    Status applyRate(const RateParams& r, bool& idr);
    Status mapInput(ID3D11Texture2D* texture, NV_ENC_INPUT_PTR& mapped, NV_ENC_BUFFER_FORMAT& format);
    void unmap(NV_ENC_INPUT_PTR mapped);
    bool waitInFlightDone(int maxMs);
    NVENCSTATUS lockFront(const InFlight& f, NV_ENC_LOCK_BITSTREAM& lk, bool copy);
    Status submitFailed(NVENCSTATUS s, const char* what);
    void sendEos(int64_t deadline);
    void warnOnce(const std::string& key, const std::string& text);

    const NvencRuntime& rt_;
    const NV_ENCODE_API_FUNCTION_LIST& nv_;
    void* enc_ = nullptr;
    ComPtr<ID3D11Device> device_;
    Codec codec_ = Codec::Hevc;
    CodecDetails det_;
    StartParams start_;
    NV_ENC_INITIALIZE_PARAMS init_{};
    NV_ENC_CONFIG config_{};
    uint32_t width_ = 0, height_ = 0;
    int preset_ = 4;
    bool async_ = false;
    bool flushMode_ = false;
    int refs_ = nvenc::kDpbFrames;
    int intraRefresh_ = 0;
    int64_t freq_ = 1;
    std::mutex sessionMu_;  // every call on enc_ from the capture and output threads
    std::vector<OutputBuffer> out_;

    // Current rate (capture thread after init).
    int kbps_ = 0, fps_ = 60;
    double vbvFrames_ = 1.0;

    // Requests from the control thread, applied by the capture thread.
    std::mutex ctlMu_;
    bool idrPending_ = true;
    uint64_t idrSeq_ = 0;  // forceIdr() calls: an IDR request that came in meanwhile is not cleared
    bool rateDirty_ = false;
    RateParams pendingRate_;
    bool roiDirty_ = false;
    std::vector<RoiRect> roiRects_;
    RfiTracker rfi_;

    // Capture thread.
    std::vector<Registration> regs_;
    std::shared_ptr<const nvenc::QpMap> qpMap_;  // nullptr = no ROI
    int nextSlot_ = 0;
    uint32_t encodeCount_ = 0;  // frames submitted (NV_ENC_PIC_PARAMS::frameIdx)
    int submitErrors_ = 0;
    bool needSequenceParams_ = true;

    std::mutex flightMu_;
    std::condition_variable flightCv_;
    std::deque<InFlight> flight_;

    std::atomic<bool> stopped_{false};
    std::atomic<uint32_t> gen_{0};
    HANDLE stopEvent_ = nullptr;

    // Output thread.
    PreciseTimer timer_;
    std::vector<uint8_t> outBuf_;     // the copied bitstream of the frame receive() returned
    std::vector<uint8_t> patched_;    // a key frame with parameter sets inserted
    std::mutex extraMu_;
    std::vector<uint8_t> extradata_;  // NvEncGetSequenceParams (capture thread)
    int lockErrors_ = 0;

    std::mutex warnMu_;
    std::set<std::string> warned_;
};

Caps capsFrom(const NvencProbe& probe) {
    Caps c;
    c.backend = probe.ok ? "nvenc" : "none";
    c.vendor = "nvidia";
    if (probe.adapter.found) {
        c.adapterLuid = probe.adapter.luid;
        c.adapterName = probe.adapter.name;
        c.hagsEnabled = probe.adapter.hags;
    }
    for (const auto& [codec, d] : probe.codecs) {
        if (d.available) c.codecs[codecName(codec)] = d.caps;
        else c.unavailable.emplace_back(std::string("nvenc-") + codecName(codec), d.reason);
    }
    if (!probe.ok) c.unavailable.emplace_back("nvenc", probe.reason);
    return c;
}

Caps NvencEncoder::caps() { return capsFrom(nvencProbe()); }

void NvencEncoder::warnOnce(const std::string& key, const std::string& text) {
    {
        std::lock_guard<std::mutex> lock(warnMu_);
        if (!warned_.insert(key).second) return;
    }
    logf(LogLevel::Warn, "nvenc: %s", text.c_str());
}

// End of stream: "The client must flush the encoder before freeing any
// resources ... pass a NULL encode picture packet and ... wait for the flush
// event to be signaled by the encoder in asynchronous mode" (NvEncDestroyEncoder).
// The completion event of a free buffer (the NVIDIA samples' SendEOS); waits
// for it until deadline (QPC).
void NvencEncoder::sendEos(int64_t deadline) {
    NV_ENC_PIC_PARAMS pic{};
    pic.version = NV_ENC_PIC_PARAMS_VER;
    pic.encodePicFlags = NV_ENC_PIC_FLAG_EOS;
    HANDLE ev = async_ && !out_.empty() ? out_[size_t(nextSlot_)].event : nullptr;
    pic.completionEvent = ev;
    NVENCSTATUS s;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncEncodePicture(enc_, &pic);
    }
    if (s != NV_ENC_SUCCESS) {
        logf(LogLevel::Debug, "nvenc: %s", nvError("NvEncEncodePicture(EOS)", s).c_str());
        return;
    }
    if (ev) WaitForSingleObject(ev, DWORD(std::max<int64_t>(0, (deadline - qpcNow()) * 1000 / freq_)));
}

void NvencEncoder::release() {
    // The threads that used the stream have been joined, or never ran
    // (Backend contract).
    if (enc_) {
        // One budget for the whole flush, well inside the helper's 500 ms
        // exit watchdog (docs/HELPER_PROTOCOL.md "Lifecycle").
        const int64_t deadline = qpcNow() + int64_t(kTeardownMs) * freq_ / 1000;
        if (encodeCount_ > 0) sendEos(deadline);
        // Frames whose output nobody collected: let them finish, lock and
        // unlock their buffers ("all bit stream buffers are unlocked before
        // destroying"), unmap their inputs ("all mapped input buffer handles
        // are unmapped").
        std::deque<InFlight> left;
        {
            std::lock_guard<std::mutex> lock(flightMu_);
            left.swap(flight_);
        }
        for (InFlight& f : left) {
            if (async_ && !f.signaled) {
                // Signaled: lockFront's lock returns at once; else it polls
                // until the deadline (a hung encoder).
                f.signaled = WaitForSingleObject(out_[size_t(f.slot)].event,
                                                 DWORD(std::max<int64_t>(0, (deadline - qpcNow()) * 1000 / freq_))) == WAIT_OBJECT_0;
            }
            NV_ENC_LOCK_BITSTREAM lk{};
            for (;;) {
                if (lockFront(f, lk, false) != NV_ENC_ERR_LOCK_BUSY || qpcNow() >= deadline) break;
                Sleep(1);
            }
            unmap(f.mapped);
        }
        left.clear();
        for (const Registration& r : regs_) nv_.nvEncUnregisterResource(enc_, r.registered);
        for (OutputBuffer& b : out_) {
            if (b.registered) {
                NV_ENC_EVENT_PARAMS ep{};
                ep.version = NV_ENC_EVENT_PARAMS_VER;
                ep.completionEvent = b.event;
                nv_.nvEncUnregisterAsyncEvent(enc_, &ep);
            }
            if (b.bitstream) nv_.nvEncDestroyBitstreamBuffer(enc_, b.bitstream);
        }
        const NVENCSTATUS s = nv_.nvEncDestroyEncoder(enc_);
        if (s != NV_ENC_SUCCESS) logf(LogLevel::Warn, "nvenc: %s", nvError("NvEncDestroyEncoder", s).c_str());
        enc_ = nullptr;
    }
    for (OutputBuffer& b : out_) {
        if (b.event) CloseHandle(b.event);
    }
    out_.clear();
    regs_.clear();
    qpMap_.reset();
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        flight_.clear();
    }
    encodeCount_ = 0;
    device_.Reset();
}

// Checks a start against the encoder's caps (det_).
Status NvencEncoder::validate(const StartParams& p) {
    const CodecCaps& cc = det_.caps;
    if (int(width_) > cc.maxW || int(height_) > cc.maxH || int(width_) < det_.minW || int(height_) < det_.minH) {
        return Status::Error("unsupported", std::to_string(width_) + "x" + std::to_string(height_) + " is outside the " + p.codec +
                                                " encoder's " + std::to_string(det_.minW) + "x" + std::to_string(det_.minH) + " .. " +
                                                std::to_string(cc.maxW) + "x" + std::to_string(cc.maxH));
    }
    if (p.ltrSlots > 0) {
        return Status::Error("unsupported", "ltrSlots " + std::to_string(p.ltrSlots) +
                                                ": the NVENC backend recovers by reference frame invalidation (caps recovery " +
                                                cc.recovery + "), start it with ltrSlots 0");
    }
    if (p.svcLayers > cc.maxTemporalLayers) {
        return Status::Error("unsupported", "svcLayers " + std::to_string(p.svcLayers) + ": the encoder supports " +
                                                std::to_string(cc.maxTemporalLayers));
    }
    if (p.intraRefreshFrames > 0 && !cc.intraRefresh) {
        return Status::Error("unsupported", "the " + p.codec + " encoder has no intra refresh (NV_ENC_CAPS_SUPPORT_INTRA_REFRESH 0)");
    }
    if (p.encoderInstance > 0) {
        return Status::Error("unsupported", "encoderInstance " + std::to_string(p.encoderInstance) +
                                                ": NVENC distributes work over its engines itself (split-frame encoding); "
                                                "leave encoderInstance unset");
    }
    if (!p.liveBitrate.empty() && !det_.dynBitrate) {
        return Status::Error("unsupported", "liveBitrate " + p.liveBitrate +
                                                ": this encoder cannot change the bitrate of a running session "
                                                "(NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE 0; caps liveBitrate restart)");
    }
    return Status::Ok();
}

void setVui(NV_ENC_CONFIG_H264_VUI_PARAMETERS& v) {
    // BT.709 limited range, chroma_sample_loc_type 0: what the NV12
    // converter writes (d3d/convert.hpp), signalled as Sunshine does
    // (nvenc_base.cpp configure_h264_hevc_metadata); bitstream restrictions
    // let decoders know there is no reordering (max_dec_frame_buffering).
    v.videoSignalTypePresentFlag = 1;
    v.videoFormat = NV_ENC_VUI_VIDEO_FORMAT_UNSPECIFIED;
    v.videoFullRangeFlag = 0;
    v.colourDescriptionPresentFlag = 1;
    v.colourPrimaries = NV_ENC_VUI_COLOR_PRIMARIES_BT709;
    v.transferCharacteristics = NV_ENC_VUI_TRANSFER_CHARACTERISTIC_BT709;
    v.colourMatrix = NV_ENC_VUI_MATRIX_COEFFS_BT709;
    v.chromaSampleLocationFlag = 1;
    v.chromaSampleLocationTop = 0;
    v.chromaSampleLocationBot = 0;
    v.bitstreamRestrictionFlag = 1;
}

// The codec part of config_ (GUIDE 3.4 for H.264 / HEVC / AV1).
void NvencEncoder::configureCodec() {
    const int layers = start_.svcLayers;
    const uint32_t irPeriod = uint32_t(intraRefresh_);
    switch (codec_) {
    case Codec::H264: {
        NV_ENC_CONFIG_H264& h = config_.encodeCodecConfig.h264Config;
        h.level = NV_ENC_LEVEL_AUTOSELECT;
        h.idrPeriod = NVENC_INFINITE_GOPLENGTH;  // "so that IDR frames are not inserted automatically"
        h.repeatSPSPPS = 1;                      // SPS + PPS with every IDR: each key frame is self-contained
        h.disableSPSPPS = 0;
        h.outputAUD = 0;
        h.sliceMode = 3;  // numSlices in picture: one slice
        h.sliceModeData = 1;
        h.chromaFormatIDC = 1;
        h.entropyCodingMode = det_.cabac ? NV_ENC_H264_ENTROPY_CODING_MODE_CABAC : NV_ENC_H264_ENTROPY_CODING_MODE_CAVLC;
        h.maxNumRefFrames = uint32_t(refs_);
        h.numRefL0 = NV_ENC_NUM_REF_FRAMES_1;  // one reference per frame, the rest kept for invalidation
        h.enableLTR = 0;
        h.inputBitDepth = h.outputBitDepth = NV_ENC_BIT_DEPTH_8;
        setVui(h.h264VUIParameters);
        if (irPeriod) {
            h.enableIntraRefresh = 1;
            h.intraRefreshPeriod = irPeriod;
            h.intraRefreshCnt = irPeriod - 1;
            h.singleSliceIntraRefresh = det_.singleSliceIntraRefresh ? 1 : 0;
            h.outputRecoveryPointSEI = 1;
        }
        if (layers > 1) {
            h.enableTemporalSVC = 1;
            h.numTemporalLayers = uint32_t(layers);
            h.maxTemporalLayers = uint32_t(layers);
        }
        break;
    }
    case Codec::Hevc: {
        NV_ENC_CONFIG_HEVC& h = config_.encodeCodecConfig.hevcConfig;
        h.level = NV_ENC_LEVEL_AUTOSELECT;
        h.tier = NV_ENC_TIER_HEVC_MAIN;
        h.idrPeriod = NVENC_INFINITE_GOPLENGTH;
        h.repeatSPSPPS = 1;  // VPS + SPS + PPS with every IDR
        h.disableSPSPPS = 0;
        h.outputAUD = 0;
        h.sliceMode = 3;
        h.sliceModeData = 1;
        h.chromaFormatIDC = 1;
        h.maxNumRefFramesInDPB = uint32_t(refs_);
        h.numRefL0 = NV_ENC_NUM_REF_FRAMES_1;
        h.enableLTR = 0;
        h.inputBitDepth = h.outputBitDepth = NV_ENC_BIT_DEPTH_8;
        setVui(h.hevcVUIParameters);
        if (irPeriod) {
            h.enableIntraRefresh = 1;
            h.intraRefreshPeriod = irPeriod;
            h.intraRefreshCnt = irPeriod - 1;
            h.singleSliceIntraRefresh = det_.singleSliceIntraRefresh ? 1 : 0;
            h.outputRecoveryPointSEI = 1;
        }
        if (layers > 1) {
            h.enableTemporalSVC = 1;
            h.numTemporalLayers = uint32_t(layers);
            h.maxTemporalLayersMinus1 = uint32_t(layers - 1);
        }
        break;
    }
    case Codec::Av1: {
        NV_ENC_CONFIG_AV1& a = config_.encodeCodecConfig.av1Config;
        a.level = NV_ENC_LEVEL_AV1_AUTOSELECT;
        a.tier = NV_ENC_TIER_AV1_0;
        a.idrPeriod = NVENC_INFINITE_GOPLENGTH;
        a.repeatSeqHdr = 1;  // a sequence header with every key frame
        a.disableSeqHdr = 0;
        a.outputAnnexBFormat = 0;  // low-overhead OBUs with sizes (the ring's AV1 temporal units)
        a.chromaFormatIDC = 1;
        a.maxNumRefFramesInDPB = uint32_t(refs_);
        a.numFwdRefs = NV_ENC_NUM_REF_FRAMES_1;
        a.enableLTR = 0;
        a.enableBitstreamPadding = 0;
        a.inputBitDepth = a.outputBitDepth = NV_ENC_BIT_DEPTH_8;
        a.colorPrimaries = NV_ENC_VUI_COLOR_PRIMARIES_BT709;
        a.transferCharacteristics = NV_ENC_VUI_TRANSFER_CHARACTERISTIC_BT709;
        a.matrixCoefficients = NV_ENC_VUI_MATRIX_COEFFS_BT709;
        a.colorRange = 0;
        a.chromaSamplePosition = 1;  // horizontally co-sited with luma, vertically between: the converter's siting
        if (irPeriod) {
            a.enableIntraRefresh = 1;
            a.intraRefreshPeriod = irPeriod;
            a.intraRefreshCnt = irPeriod - 1;
        }
        if (layers > 1) {
            a.enableTemporalSVC = 1;
            a.numTemporalLayers = uint32_t(layers);
            a.maxTemporalLayersMinus1 = uint32_t(layers - 1);
        }
        break;
    }
    }
}

// config_ from the preset's config, and init_.
Status NvencEncoder::configure() {
    NV_ENC_PRESET_CONFIG pc{};
    pc.version = NV_ENC_PRESET_CONFIG_VER;
    pc.presetCfg.version = NV_ENC_CONFIG_VER;
    NVENCSTATUS s =
        nv_.nvEncGetEncodePresetConfigEx(enc_, codecGuid(codec_), presetGuid(preset_), NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY, &pc);
    if (s != NV_ENC_SUCCESS) return Status::Error("init_failed", nvError("NvEncGetEncodePresetConfigEx(P" + std::to_string(preset_) + ")", s));
    config_ = pc.presetCfg;
    config_.version = NV_ENC_CONFIG_VER;
    config_.profileGUID = profileGuid(codec_);
    // No B frames, no automatic key frames ("If goplength is set to
    // NVENC_INFINITE_GOPLENGTH frameIntervalP should be set to 1").
    config_.gopLength = NVENC_INFINITE_GOPLENGTH;
    config_.frameIntervalP = 1;
    config_.frameFieldMode = NV_ENC_PARAMS_FRAME_FIELD_MODE_FRAME;

    NV_ENC_RC_PARAMS& rc = config_.rcParams;
    const nvenc::Rate r = nvenc::rateFor(kbps_, vbvFrames_, fps_);
    // "vbr" and "vbr_peak" are both VBR capped at the target (maxBitRate =
    // averageBitRate): NVENC has no separate peak-constrained mode.
    rc.rateControlMode = start_.rc == "cbr" ? NV_ENC_PARAMS_RC_CBR : NV_ENC_PARAMS_RC_VBR;
    rc.averageBitRate = r.average;
    rc.maxBitRate = r.max;
    rc.vbvBufferSize = det_.customVbv ? r.vbv : 0;  // 0 = the driver's default where a custom size is not supported
    rc.vbvInitialDelay = 0;
    rc.enableLookahead = 0;
    rc.zeroReorderDelay = 1;  // "zero latency operation (no reordering delay, num_reorder_frames=0)"
    rc.enableNonRefP = 0;     // every frame a reference: the invalidation model (codec/rfi.hpp)
    rc.enableAQ = 1;          // spatial AQ (GUIDE 3.4; ROI uses delta maps, which work alongside it)
    rc.aqStrength = 0;        // driver's choice
    rc.enableTemporalAQ = 0;
    rc.lowDelayKeyFrameScale = uint8_t(nvenc::kKeyFrameScale);
    // NVENC guide 9, game streaming: "Multi Pass - Quarter/Full (evaluate and
    // decide)"; Sunshine's default is quarter resolution (better VBV adherence
    // with a one-frame VBV, larger motion vectors). VERIFY the encode time at 4K120.
    rc.multiPass = NV_ENC_TWO_PASS_QUARTER_RESOLUTION;
    rc.qpMapMode = NV_ENC_QP_MAP_DELTA;  // ROI (OBS obs-nvenc sets it the same way, map or not)
    configureCodec();

    init_ = {};
    init_.version = NV_ENC_INITIALIZE_PARAMS_VER;
    init_.encodeGUID = codecGuid(codec_);
    init_.presetGUID = presetGuid(preset_);  // the same preset as the config ("recommended to pass the same preset guid")
    init_.tuningInfo = NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY;
    init_.encodeWidth = width_;
    init_.encodeHeight = height_;
    init_.darWidth = width_;
    init_.darHeight = height_;
    init_.frameRateNum = uint32_t(fps_);
    init_.frameRateDen = 1;
    init_.enableEncodeAsync = async_ ? 1 : 0;
    init_.enablePTD = 1;  // the encoder decides picture types; FORCEIDR needs it
    init_.encodeConfig = &config_;
    if (det_.caps.dynamicResolution) {
        // "Resolution change is possible only if maxEncodeWidth & maxEncodeHeight
        // ... is set while creating encoder session" (NvEncReconfigureEncoder).
        // The start size: a running session can then go down under pressure
        // and back up (GUIDE 5 "FPS before resolution"); more room would make
        // the driver size its reference frames for a resolution nobody asked
        // for. No control message changes the size yet; that reconfiguration
        // needs forceIDR = 1 ("advisable to force the next frame ... as an IDR").
        init_.maxEncodeWidth = width_;
        init_.maxEncodeHeight = height_;
    }
    // splitEncodeMode stays NV_ENC_SPLIT_AUTO_MODE: the driver splits a frame
    // over several engines where it helps (4K on GPUs with two or three NVENCs).
    //
    // Phase 5 hook (GUIDE 9 "Re-encode oversized frames: NVENC
    // DISABLE_ENC_STATE_ADVANCE + restore"), not implemented: needs
    // NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE (det_.stateAdvance, logged below),
    // init_.numStateBuffers > 0, per frame NV_ENC_PIC_FLAG_DISABLE_ENC_STATE_ADVANCE
    // with stateBufferIdx and frameIdx (monotonic from 0: encodeCount_ already
    // is), and when the output is too large NvEncRestoreEncoderState after all
    // earlier encodes finished, then the same input again at a lower QP (NVENC
    // guide "encoding the same frame multiple times").
    return Status::Ok();
}

Status NvencEncoder::createBuffers() {
    out_.assign(kOutputBuffers, OutputBuffer{});
    for (OutputBuffer& b : out_) {
        NV_ENC_CREATE_BITSTREAM_BUFFER cb{};
        cb.version = NV_ENC_CREATE_BITSTREAM_BUFFER_VER;
        NVENCSTATUS s = nv_.nvEncCreateBitstreamBuffer(enc_, &cb);
        if (s != NV_ENC_SUCCESS) return Status::Error("init_failed", nvError("NvEncCreateBitstreamBuffer", s));
        b.bitstream = cb.bitstreamBuffer;
        if (!async_) continue;
        // One auto-reset event per output buffer ("Each output buffer should be
        // associated with a distinct event pointer"), registered once.
        b.event = CreateEventW(nullptr, FALSE, FALSE, nullptr);
        if (!b.event) return Status::Error("init_failed", "CreateEvent: " + win32ErrorText(GetLastError()));
        NV_ENC_EVENT_PARAMS ep{};
        ep.version = NV_ENC_EVENT_PARAMS_VER;
        ep.completionEvent = b.event;
        s = nv_.nvEncRegisterAsyncEvent(enc_, &ep);
        if (s != NV_ENC_SUCCESS) return Status::Error("init_failed", nvError("NvEncRegisterAsyncEvent", s));
        b.registered = true;
    }
    return Status::Ok();
}

Status NvencEncoder::init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) {
    release();
    stopped_ = false;
    if (stopEvent_) ResetEvent(stopEvent_);
    if (!rt_.ok) return Status::Error("unavailable", rt_.error);
    Codec codec;
    if (!parseCodec(p.codec, codec)) return Status::Error("unsupported", "unknown codec " + p.codec);
    const NvencProbe& probe = nvencProbe();
    const auto it = probe.codecs.find(codec);
    if (it == probe.codecs.end() || !it->second.available) {
        return Status::Error("unsupported", "NVENC cannot encode " + p.codec + " here: " +
                                                (it == probe.codecs.end() ? probe.reason : it->second.reason));
    }
    if (p.hdr) return Status::Error("unsupported", "HDR10 encoding comes with step 3.9");
    if (!src.device) return Status::Error("unsupported", "the NVENC encoder needs a GPU capture (dda, wgc or synthetic-gpu)");
    if (src.adapter.found && src.adapter.vendor != "nvidia" && !rt_.testDouble) {
        return Status::Error("unsupported", "the capture runs on " + src.adapter.name + " (" + src.adapter.vendor +
                                                "): NVENC encodes on the NVIDIA adapter's own device only");
    }
    codec_ = codec;
    det_ = it->second;
    start_ = p;
    width_ = uint32_t(p.width ? p.width : int(src.width)) & ~1u;
    height_ = uint32_t(p.height ? p.height : int(src.height)) & ~1u;
    kbps_ = p.kbps;
    fps_ = p.fps;
    vbvFrames_ = p.vbvFrames;
    device_ = src.device;

    NVENCSTATUS s = openSession(nv_, device_.Get(), enc_);
    if (s != NV_ENC_SUCCESS) {
        release();
        return Status::Error("init_failed", nvError("NvEncOpenEncodeSessionEx on the capture device", s) + sessionHint(s));
    }
    // The caps on this very device (the probe used the first NVIDIA adapter).
    {
        CodecDetails d = readDetails(nv_, enc_, codec_, encodeGuids(nv_, enc_));
        if (d.available) det_ = d;
    }
    if (Status v = validate(p); !v.ok) {
        release();
        return v;
    }
    const CodecCaps& cc = det_.caps;
    flushMode_ = (p.liveBitrate.empty() ? cc.liveBitrate : p.liveBitrate) == "flush";
    async_ = det_.async;
    refs_ = det_.multiRef ? nvenc::dpbFramesFor(codec_, width_, height_) : 1;
    intraRefresh_ = p.intraRefreshFrames > 0 ? std::max(2, p.intraRefreshFrames) : 0;  // period, count = period - 1
    preset_ = nvenc::presetFor(width_, height_, fps_, p.quality);
    if (vbvFrames_ < 1.0 || vbvFrames_ > 1.5) logf(LogLevel::Info, "nvenc: vbvFrames %.2f (GUIDE 10: one frame for NVIDIA)", vbvFrames_);

    Status st = configure();
    if (st.ok) {
        s = nv_.nvEncInitializeEncoder(enc_, &init_);
        if (s != NV_ENC_SUCCESS) {
            st = Status::Error("init_failed", nvError("NvEncInitializeEncoder(" + std::string(codecName(codec_)) + " " + std::to_string(width_) +
                                                          "x" + std::to_string(height_) + " P" + std::to_string(preset_) + ")",
                                                      s));
        }
    }
    if (st.ok) st = createBuffers();
    if (!st.ok) {
        release();
        return st;
    }
    rfi_.reset(refs_);
    {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idrPending_ = true;  // the first frame: IDR with parameter sets
        rateDirty_ = false;
        roiDirty_ = false;
        roiRects_.clear();
    }
    needSequenceParams_ = true;
    nextSlot_ = 0;
    encodeCount_ = 0;
    submitErrors_ = lockErrors_ = 0;
    gen_ = 0;

    in = InputSpec{};
    in.format = InputSpec::Format::Nv12;
    in.width = width_;
    in.height = height_;
    out.backend = name();
    out.codec = p.codec;
    out.width = int(width_);
    out.height = int(height_);
    out.codedWidth = int(width_);
    out.codedHeight = int(height_);
    out.fps = p.fps;
    out.kbps = p.kbps;
    out.liveBitrate = !det_.dynBitrate ? "restart" : flushMode_ ? "flush" : "seamless";
    out.rateControl = p.rc == "cbr" ? "cbr" : "vbr";
    out.usage = "ultra_low_latency";
    out.ltrSlots = 0;
    out.ltrInterval = 0;
    out.encoderInstance = 0;
    out.hwInstances = cc.hwInstances;
    out.queryTimeoutMs = 0;
    out.zeroCopy = false;
    out.intraRefreshFrames = intraRefresh_;
    out.preset = "p" + std::to_string(preset_);
    out.asyncEncode = async_;
    out.refFrames = refs_;
    logf(LogLevel::Info,
         "nvenc: %s %ux%u %d fps %d kbps %s vbv %.2f frames (%u bits), preset P%d ultra-low-latency, %s output, %d reference frames, "
         "recovery %s, live bitrate %s, two-pass quarter resolution, spatial AQ, key frame scale %u, intra refresh %d, %d engine(s), "
         "dynamic resolution %s (max %ux%u), emphasis map cap %d, state-advance cap %d, LTR frames cap %d (unused), %s on %s",
         p.codec.c_str(), width_, height_, fps_, kbps_, out.rateControl.c_str(), vbvFrames_, config_.rcParams.vbvBufferSize, preset_,
         async_ ? "async (events)" : "sync (polled)", refs_, cc.recovery.c_str(), out.liveBitrate.c_str(), nvenc::kKeyFrameScale,
         intraRefresh_, cc.hwInstances, cc.dynamicResolution ? "yes" : "no", init_.maxEncodeWidth, init_.maxEncodeHeight,
         int(det_.emphasisMap), int(det_.stateAdvance), det_.ltrFrames, rt_.versionText.c_str(),
         src.adapter.found ? src.adapter.name.c_str() : "the capture device");
    return Status::Ok();
}

// The encoder's parameter sets (inserted into a key frame that comes out
// without them). On the capture thread, before the next NvEncEncodePicture:
// "The client must call NvEncGetSequenceParams() function from the same thread
// which is being used to call NvEncEncodePicture() function."
void NvencEncoder::readSequenceParams() {
    needSequenceParams_ = false;
    std::vector<uint8_t> buf(NV_MAX_SEQ_HDR_LEN);
    uint32_t size = 0;
    NV_ENC_SEQUENCE_PARAM_PAYLOAD sp{};
    sp.version = NV_ENC_SEQUENCE_PARAM_PAYLOAD_VER;
    sp.inBufferSize = uint32_t(buf.size());
    sp.spsppsBuffer = buf.data();
    sp.outSPSPPSPayloadSize = &size;
    NVENCSTATUS s;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncGetSequenceParams(enc_, &sp);
    }
    if (s != NV_ENC_SUCCESS || size > buf.size()) {
        warnOnce("seqparams", nvError("NvEncGetSequenceParams", s));
        return;
    }
    buf.resize(size);
    // What the encoder writes, rather than what it was asked for: the level
    // (a stream above 5.2 is one many hardware decoders refuse) and the
    // reference frames it keeps. With fewer than refs_ the invalidation
    // window shrinks to match, so a recovery never relies on a frame the
    // encoder dropped (NvEncInvalidateRefFrames would then make an intra frame
    // that goes out flagged as a recovery from refFloor). started.refFrames
    // stays what was configured; the warning says what the encoder does.
    SpsInfo sps;
    if (codec_ != Codec::Av1) {
        if (!parseSps(codec_, buf.data(), buf.size(), sps)) {
            warnOnce("sps", std::string("cannot read the level and reference frames from the encoder's ") + codecName(codec_) +
                                " SPS: the invalidation window stays " + std::to_string(rfi_.dpbSize()));
        } else {
            logf(LogLevel::Info, "nvenc: the encoder's SPS: level %s, %d reference frames (configured %d)",
                 levelText(codec_, sps.levelIdc).c_str(), sps.refFrames, refs_);
            if (sps.refFrames >= 1 && sps.refFrames < rfi_.dpbSize()) {
                warnOnce("spsrefs", "the encoder keeps " + std::to_string(sps.refFrames) + " reference frames, not " +
                                        std::to_string(refs_) + ": the invalidation window is " + std::to_string(sps.refFrames) +
                                        " frames (started.refFrames said " + std::to_string(refs_) + ")");
                rfi_.resize(sps.refFrames);
            }
        }
    }
    std::lock_guard<std::mutex> lock(extraMu_);
    extradata_ = std::move(buf);
}

// A setRate before the next frame: NvEncReconfigureEncoder with the new
// averageBitRate / maxBitRate / vbvBufferSize (and frame rate). Seamless:
// resetEncoder 0, forceIDR 0 (GUIDE 3.4; the NVENC guide 8.4: "If the client
// wishes to reset the internal rate control states, set resetEncoder to 1").
// Flush: resetEncoder 1 + forceIDR 1 (what FFmpeg's and OBS's bitrate changes
// do), a new generation.
Status NvencEncoder::applyRate(const RateParams& r, bool& idr) {
    const int kbps = r.kbps > 0 ? r.kbps : kbps_;
    const double vbv = r.vbvFrames > 0 ? r.vbvFrames : vbvFrames_;
    const int fps = r.fps > 0 ? r.fps : fps_;
    NV_ENC_CONFIG cfg = config_;
    const nvenc::Rate v = nvenc::rateFor(kbps, vbv, fps);
    cfg.rcParams.averageBitRate = v.average;
    cfg.rcParams.maxBitRate = v.max;
    if (det_.customVbv) cfg.rcParams.vbvBufferSize = v.vbv;
    NV_ENC_RECONFIGURE_PARAMS rp{};
    rp.version = NV_ENC_RECONFIGURE_PARAMS_VER;
    rp.reInitEncodeParams = init_;
    rp.reInitEncodeParams.encodeConfig = &cfg;
    rp.reInitEncodeParams.frameRateNum = uint32_t(fps);
    rp.reInitEncodeParams.frameRateDen = 1;
    rp.resetEncoder = flushMode_ ? 1 : 0;
    rp.forceIDR = flushMode_ ? 1 : 0;
    NVENCSTATUS s;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncReconfigureEncoder(enc_, &rp);
    }
    if (s != NV_ENC_SUCCESS) {
        Status d;
        if (d3d::deviceRemoved(device_.Get(), nvError("nvenc: NvEncReconfigureEncoder", s), d)) return d;
        return Status::Error("encode_failed", nvError("setRate: NvEncReconfigureEncoder(" + std::to_string(kbps) + " kbps, " +
                                                          std::to_string(fps) + " fps)",
                                                      s) +
                                                  ": the previous rate stays");
    }
    config_ = cfg;
    init_ = rp.reInitEncodeParams;
    init_.encodeConfig = &config_;
    kbps_ = kbps;
    vbvFrames_ = vbv;
    fps_ = fps;
    if (flushMode_) {
        ++gen_;
        idr = true;
        needSequenceParams_ = true;
    }
    logf(LogLevel::Debug, "nvenc: rate %d kbps, vbv %u bits, %d fps (%s)", kbps_, config_.rcParams.vbvBufferSize, fps_,
         flushMode_ ? "reset + IDR" : "seamless");
    return Status::Ok();
}

Status NvencEncoder::submitFailed(NVENCSTATUS s, const char* what) {
    Status d;
    if (d3d::deviceRemoved(device_.Get(), nvError(std::string("nvenc: ") + what, s), d)) return d;
    if (s == NV_ENC_ERR_DEVICE_NOT_EXIST) return Status::Error("device_lost", nvError(what, s) + ": the encoder's device is gone", true);
    if (++submitErrors_ >= kMaxErrors) {
        return Status::Error("encode_failed", nvError(std::string(what) + " (" + std::to_string(submitErrors_) + " times in a row)", s), true);
    }
    return Status::Error("encode_failed", nvError(what, s));
}

// The texture's registration (once per pool texture), then its mapping for this frame.
Status NvencEncoder::mapInput(ID3D11Texture2D* texture, NV_ENC_INPUT_PTR& mapped, NV_ENC_BUFFER_FORMAT& format) {
    NV_ENC_REGISTERED_PTR reg = nullptr;
    for (const Registration& r : regs_) {
        if (r.texture.Get() == texture) reg = r.registered;
    }
    NVENCSTATUS s;
    if (!reg) {
        D3D11_TEXTURE2D_DESC td{};
        texture->GetDesc(&td);
        NV_ENC_REGISTER_RESOURCE rr{};
        rr.version = NV_ENC_REGISTER_RESOURCE_VER;
        rr.resourceType = NV_ENC_INPUT_RESOURCE_TYPE_DIRECTX;
        rr.width = td.Width;
        rr.height = td.Height;
        rr.pitch = 0;  // "For NV_ENC_INPUT_RESOURCE_TYPE_DIRECTX resources, set this to 0"
        rr.subResourceIndex = 0;
        rr.resourceToRegister = texture;
        rr.bufferFormat = NV_ENC_BUFFER_FORMAT_NV12;
        rr.bufferUsage = NV_ENC_INPUT_IMAGE;
        {
            std::lock_guard<std::mutex> lock(sessionMu_);
            s = nv_.nvEncRegisterResource(enc_, &rr);
        }
        if (s != NV_ENC_SUCCESS) return submitFailed(s, "NvEncRegisterResource");
        regs_.push_back({ComPtr<ID3D11Texture2D>(texture), rr.registeredResource});
        reg = rr.registeredResource;
        logf(LogLevel::Debug, "nvenc: registered input texture %zu (%ux%u)", regs_.size(), td.Width, td.Height);
    }
    NV_ENC_MAP_INPUT_RESOURCE m{};
    m.version = NV_ENC_MAP_INPUT_RESOURCE_VER;
    m.registeredResource = reg;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncMapInputResource(enc_, &m);
    }
    if (s != NV_ENC_SUCCESS) return submitFailed(s, "NvEncMapInputResource");
    mapped = m.mappedResource;
    format = m.mappedBufferFmt;
    return Status::Ok();
}

void NvencEncoder::unmap(NV_ENC_INPUT_PTR mapped) {
    if (!mapped) return;
    NVENCSTATUS s;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncUnmapInputResource(enc_, mapped);
    }
    if (s != NV_ENC_SUCCESS) warnOnce("unmap", nvError("NvEncUnmapInputResource", s));
}

// Waits until the frames in the encoder have come out (the output thread
// collects them). False on timeout or shutdown.
bool NvencEncoder::waitInFlightDone(int maxMs) {
    std::unique_lock<std::mutex> lock(flightMu_);
    return flightCv_.wait_for(lock, std::chrono::milliseconds(maxMs), [this] { return flight_.empty() || stopped_; }) && !stopped_;
}

Status NvencEncoder::submit(const EncoderFrame& frame, const SubmitInfo& info) {
    if (stopped_ || !enc_) return Status::Ok();
    // Requests from the control thread.
    bool idr = false, rateDirty = false, roiDirty = false;
    uint64_t idrSeq = 0;
    RateParams rate;
    std::vector<RoiRect> rects;
    {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idr = idrPending_;
        idrSeq = idrSeq_;
        if ((rateDirty = rateDirty_)) rate = pendingRate_, rateDirty_ = false;
        if ((roiDirty = roiDirty_)) rects = roiRects_, roiDirty_ = false;
    }
    if (needSequenceParams_) readSequenceParams();
    Status rateError;
    if (rateDirty) {
        bool forced = false;
        rateError = applyRate(rate, forced);
        if (rateError.fatal) return rateError;
        if (forced) {
            std::lock_guard<std::mutex> lock(ctlMu_);
            idrPending_ = idr = true;  // resetEncoder "should be used only with an IDR frame"
            idrSeq = ++idrSeq_;
        }
        if (!rateError.ok) logf(LogLevel::Warn, "nvenc: %s", rateError.text.c_str());
    }
    if (roiDirty) {
        if (rects.empty()) {
            qpMap_.reset();
        } else {
            qpMap_ = std::make_shared<const nvenc::QpMap>(nvenc::roiQpDeltaMap(codec_, width_, height_, rects));
        }
    }
    if (!frame.nv12) return Status::Error("encode_failed", "no NV12 frame to encode");
    {
        // Never more than kMaxInFlight frames in the encoder: the pipeline
        // drops this capture instead (it keeps its frame id).
        std::lock_guard<std::mutex> lock(flightMu_);
        if (flight_.size() >= kMaxInFlight) return Status::Error("encoder_busy", "two frames are still in the encoder");
    }

    // Loss recovery (codec/rfi.hpp): invalidate the lost frames and everything
    // after them, then this frame references only older ones. The frames still
    // in the encoder finish first: NVIDIA documents invalidation of frames in
    // the DPB, and Sunshine, encoding synchronously, only ever invalidates
    // finished ones; the wait costs at most one encode time, on a loss only.
    RfiTracker::Plan plan = rfi_.plan(info.frameId, idr);
    if (!plan.invalidate.empty()) {
        if (!waitInFlightDone(kDrainBeforeInvalidateMs)) {
            if (stopped_) return Status::Ok();
            logf(LogLevel::Warn, "nvenc: frames still in the encoder after %d ms: recovering from frame %llu with an IDR",
                 kDrainBeforeInvalidateMs, static_cast<unsigned long long>(plan.lostFrom));
            plan.recovery = false;
            plan.idr = true;
        }
        for (size_t i = 0; plan.recovery && i < plan.invalidate.size(); ++i) {
            const uint64_t ts = plan.invalidate[i];
            NVENCSTATUS s;
            {
                std::lock_guard<std::mutex> lock(sessionMu_);
                s = nv_.nvEncInvalidateRefFrames(enc_, ts);
            }
            if (s != NV_ENC_SUCCESS) {
                // Sunshine: a failed invalidation means an IDR.
                warnOnce("invalidate", nvError("NvEncInvalidateRefFrames(" + std::to_string(ts) + ")", s) + ": recovering with an IDR");
                plan.recovery = false;
                plan.idr = true;
            }
        }
        if (plan.recovery) {
            logf(LogLevel::Debug, "nvenc: frames %llu..%llu invalidated, frame %llu references frame %llu",
                 static_cast<unsigned long long>(plan.invalidate.front()), static_cast<unsigned long long>(plan.invalidate.back()),
                 static_cast<unsigned long long>(info.frameId), static_cast<unsigned long long>(plan.refFloor));
        }
    } else if (plan.idr && plan.lostFrom && !idr) {
        logf(LogLevel::Debug, "nvenc: no valid reference before frame %llu: IDR", static_cast<unsigned long long>(plan.lostFrom));
    }

    NV_ENC_INPUT_PTR mapped = nullptr;
    NV_ENC_BUFFER_FORMAT format = NV_ENC_BUFFER_FORMAT_NV12;
    if (Status ms = mapInput(frame.nv12, mapped, format); !ms.ok) return ms;

    const int slot = nextSlot_;
    NV_ENC_PIC_PARAMS pic{};
    pic.version = NV_ENC_PIC_PARAMS_VER;
    pic.inputWidth = width_;
    pic.inputHeight = height_;
    pic.inputPitch = width_;  // "If pitch value is not known, set this to inputWidth"
    // A forced IDR carries its parameter sets (repeatSPSPPS / repeatSeqHdr do
    // too; the flag asks explicitly: "To include SPS/PPS (H.264 and HEVC) or
    // Sequence Header OBU (AV1) along with the currently encoded frame").
    pic.encodePicFlags = plan.idr ? uint32_t(NV_ENC_PIC_FLAG_FORCEIDR | NV_ENC_PIC_FLAG_OUTPUT_SPSPPS) : 0u;
    pic.frameIdx = encodeCount_;
    // "This opaque data can be used later to uniquely refer to the corresponding
    // encoded frame. For example, it can be used for identifying the frame to be
    // invalidated in the reference picture buffer, if lost at the client."
    pic.inputTimeStamp = info.frameId;
    pic.inputBuffer = mapped;
    pic.bufferFmt = format;
    pic.pictureStruct = NV_ENC_PIC_STRUCT_FRAME;
    pic.outputBitstream = out_[size_t(slot)].bitstream;
    pic.completionEvent = async_ ? out_[size_t(slot)].event : nullptr;
    const std::shared_ptr<const nvenc::QpMap> qpMap = qpMap_;
    if (qpMap) {
        pic.qpDeltaMap = const_cast<int8_t*>(qpMap->values.data());
        pic.qpDeltaMapSize = uint32_t(qpMap->values.size());
    }
    NVENCSTATUS s;
    {
        std::lock_guard<std::mutex> lock(sessionMu_);
        s = nv_.nvEncEncodePicture(enc_, &pic);
    }
    if (s == NV_ENC_ERR_NEED_MORE_INPUT) {
        // Only with B frames or lookahead, both off: the frame is queued and
        // its event comes later, in order, like any other.
        warnOnce("needmore", "NvEncEncodePicture answered NV_ENC_ERR_NEED_MORE_INPUT (no B frames are configured)");
    } else if (s != NV_ENC_SUCCESS) {
        unmap(mapped);
        if (s == NV_ENC_ERR_ENCODER_BUSY) return Status::Error("encoder_busy", "NVENC is busy (NV_ENC_ERR_ENCODER_BUSY)");
        return submitFailed(s, "NvEncEncodePicture");
    }
    nextSlot_ = (slot + 1) % kOutputBuffers;
    ++encodeCount_;
    submitErrors_ = 0;
    InFlight f;
    f.info = info;
    f.slot = slot;
    f.mapped = mapped;
    f.hold = frame.hold;
    f.texture = frame.nv12;
    f.qpMap = qpMap;
    f.forcedIdr = plan.idr;
    f.recovery = plan.recovery;
    f.refFloor = plan.refFloor;
    f.gen = gen_;
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        flight_.push_back(std::move(f));
    }
    flightCv_.notify_all();
    rfi_.submitted(info.frameId, plan);
    if (plan.idr) {
        std::lock_guard<std::mutex> lock(ctlMu_);
        if (idrSeq_ == idrSeq) idrPending_ = false;  // a forceIdr() that came in meanwhile stays pending
    }
    return rateError;
}

// Locks the front frame's bitstream, copies it into outBuf_ if asked and
// unlocks it, with d3d::dxgiGate() held (top of file); the caller unmaps the
// frame's input afterwards. doNotWait: 0 once the frame's completion event has
// fired (async), as NVENC guide 6.3 prescribes for applications that call
// AcquireNextFrame on another thread ("NV_ENC_LOCK_BITSTREAM::doNotWait = 0";
// the encode is done, so the lock returns at once; the SDK sample
// NvEncoder::GetEncodedPacket waits for the event and locks with doNotWait
// false too). 1 where the frame may still be encoding: sync mode polls (guide
// 6.2 and NvEncLockBitstream: NV_ENC_ERR_LOCK_BUSY, "retry the function after
// few milliseconds"), and release() polls a frame whose event did not come
// within its deadline, so a hung encoder cannot block the teardown.
NVENCSTATUS NvencEncoder::lockFront(const InFlight& f, NV_ENC_LOCK_BITSTREAM& lk, bool copy) {
    std::lock_guard<d3d::DxgiGate> gate(d3d::dxgiGate());
    std::lock_guard<std::mutex> lock(sessionMu_);
    lk = {};
    lk.version = NV_ENC_LOCK_BITSTREAM_VER;
    lk.outputBitstream = out_[size_t(f.slot)].bitstream;
    lk.doNotWait = async_ && f.signaled ? 0 : 1;
    const NVENCSTATUS s = nv_.nvEncLockBitstream(enc_, &lk);
    if (s != NV_ENC_SUCCESS) return s;
    if (copy) {
        const auto* p = static_cast<const uint8_t*>(lk.bitstreamBufferPtr);
        outBuf_.assign(p, p + lk.bitstreamSizeInBytes);
    }
    const NVENCSTATUS u = nv_.nvEncUnlockBitstream(enc_, lk.outputBitstream);
    if (u != NV_ENC_SUCCESS) warnOnce("unlock", nvError("NvEncUnlockBitstream", u));
    return s;
}

Next NvencEncoder::receive(EncodedFrame& out, int timeoutMs, Status& err) {
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq_ / 1000;
    InFlight front;
    NV_ENC_LOCK_BITSTREAM lk{};
    for (;;) {
        if (stopped_ || !enc_) return Next::Stopped;
        {
            // Nothing in the encoder: wait for a submission.
            std::unique_lock<std::mutex> lock(flightMu_);
            if (flight_.empty()) {
                const int64_t now = qpcNow();
                if (now >= deadline) return Next::Timeout;
                flightCv_.wait_for(lock, std::chrono::microseconds((deadline - now) * 1000000 / freq_),
                                   [this] { return !flight_.empty() || stopped_; });
                if (stopped_) return Next::Stopped;
                if (flight_.empty()) return Next::Timeout;
            }
            front = flight_.front();  // the oldest: outputs come in submission order
        }
        int64_t now = qpcNow();
        if (async_ && !front.signaled) {
            // Wait for its completion event (or shutdown), in slices so a
            // hang is noticed.
            const HANDLE handles[2] = {stopEvent_, out_[size_t(front.slot)].event};
            const int64_t until = std::min(deadline, now + freq_ / 10);
            const DWORD w = WaitForMultipleObjects(2, handles, FALSE, DWORD(std::max<int64_t>(0, (until - now) * 1000 / freq_)));
            if (w == WAIT_OBJECT_0) return Next::Stopped;
            now = qpcNow();
            if (w != WAIT_OBJECT_0 + 1) {
                if (now - front.info.submitQpc > kHangMs * freq_ / 1000) {
                    if (d3d::deviceRemoved(device_.Get(), "nvenc: waiting for frame " + std::to_string(front.info.frameId), err)) return Next::Error;
                    err = Status::Error("encode_failed", "NVENC did not finish frame " + std::to_string(front.info.frameId) + " within " +
                                                             std::to_string(kHangMs) + " ms",
                                        true);
                    return Next::Error;
                }
                if (now >= deadline) return Next::Timeout;
                continue;
            }
            // The auto-reset event is consumed: remember it.
            std::lock_guard<std::mutex> lock(flightMu_);
            if (!flight_.empty() && flight_.front().slot == front.slot) flight_.front().signaled = front.signaled = true;
        }
        const NVENCSTATUS s = lockFront(front, lk, true);
        if (s == NV_ENC_SUCCESS) break;
        now = qpcNow();
        if (s == NV_ENC_ERR_LOCK_BUSY) {
            // Sync mode: not done yet ("the client can retry the function after
            // few milliseconds"); async: not after the event (a blocking lock).
            if (now - front.info.submitQpc > kHangMs * freq_ / 1000) {
                err = Status::Error("encode_failed", "NVENC did not finish frame " + std::to_string(front.info.frameId) + " within " +
                                                         std::to_string(kHangMs) + " ms",
                                    true);
                return Next::Error;
            }
            if (!timer_.sleepUntil(std::min(deadline, now + freq_ / 1000), stopEvent_)) return Next::Stopped;
            if (qpcNow() >= deadline) return Next::Timeout;
            continue;
        }
        // The frame's output is lost: drop it (recon-host sees the gap in
        // frame ids and recovers), give its input back.
        {
            std::lock_guard<std::mutex> lock(flightMu_);
            if (!flight_.empty()) flight_.pop_front();
        }
        flightCv_.notify_all();
        unmap(front.mapped);
        if (d3d::deviceRemoved(device_.Get(), nvError("nvenc: NvEncLockBitstream", s), err)) return Next::Error;
        if (++lockErrors_ >= kMaxErrors || s == NV_ENC_ERR_DEVICE_NOT_EXIST) {
            err = Status::Error("encode_failed", nvError("NvEncLockBitstream (" + std::to_string(lockErrors_) + " times in a row)", s), true);
        } else {
            err = Status::Error("encode_failed", nvError("NvEncLockBitstream for frame " + std::to_string(front.info.frameId), s));
        }
        return Next::Error;
    }
    lockErrors_ = 0;
    unmap(front.mapped);  // after the successful lock, as the API requires; outside the gate (guide 6.3 names Lock / Unlock only)
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        if (!flight_.empty()) flight_.pop_front();
    }
    flightCv_.notify_all();  // submit() may wait for the encoder to drain

    out = EncodedFrame{};
    out.info = front.info;
    out.outputQpc = qpcNow();
    out.gen = front.gen;
    out.key = lk.pictureType == NV_ENC_PIC_TYPE_IDR;
    if (out.key && !front.forcedIdr) rfi_.unplannedKey(front.info.frameId);
    if (front.forcedIdr && !out.key) {
        warnOnce("notidr", "a forced IDR came out as picture type " + std::to_string(int(lk.pictureType)) + ": forcing another");
        forceIdr();
    }
    out.recovery = front.recovery && !out.key;
    out.refFloor = front.refFloor;
    out.ltrSlot = -1;
    out.temporalLayer = lk.temporalId;
    out.refLtrMask = 0;
    out.width = width_;
    out.height = height_;
    out.data = outBuf_.data();
    out.size = outBuf_.size();
    if (out.key && !hasParameterSets(codec_, out.data, out.size)) {
        // A key frame must be a decoder entry point (ring flag KEY).
        std::lock_guard<std::mutex> lock(extraMu_);
        patched_ = withParameterSets(codec_, out.data, out.size, extradata_.data(), extradata_.size());
        if (!patched_.empty()) {
            warnOnce("headers", "key frames come without parameter sets: inserting the encoder's sequence parameters");
            out.data = patched_.data();
            out.size = patched_.size();
        } else {
            warnOnce("headers-missing", "a key frame has no parameter sets and NvEncGetSequenceParams cannot supply them");
        }
    }
    return Next::Frame;  // front (its input hold, ROI map) goes out of scope: the pool texture is free
}

Status NvencEncoder::forceIdr() {
    std::lock_guard<std::mutex> lock(ctlMu_);
    idrPending_ = true;
    ++idrSeq_;
    return Status::Ok();
}

Status NvencEncoder::recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) {
    (void)ackedLtrFrameId;  // LTR is the AMF backend's recovery
    if (det_.caps.recovery != "invalidate") {
        logf(LogLevel::Debug, "nvenc: no reference invalidation here: IDR for the loss at frame %llu",
             static_cast<unsigned long long>(lostFromFrameId));
        return forceIdr();
    }
    rfi_.recover(lostFromFrameId);
    return Status::Ok();
}

Status NvencEncoder::setRate(const RateParams& r) {
    if (!det_.dynBitrate) {
        return Status::Error("unsupported", "this encoder cannot change the bitrate of a running session "
                                            "(NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE 0; liveBitrate restart: start a new helper)");
    }
    std::lock_guard<std::mutex> lock(ctlMu_);
    if (r.kbps > 0) pendingRate_.kbps = r.kbps;
    if (r.vbvFrames > 0) pendingRate_.vbvFrames = r.vbvFrames;
    if (r.fps > 0) pendingRate_.fps = r.fps;
    rateDirty_ = true;
    return Status::Ok();
}

Status NvencEncoder::setRoi(const std::vector<RoiRect>& rects) {
    std::lock_guard<std::mutex> lock(ctlMu_);
    roiRects_ = rects;
    roiDirty_ = true;
    return Status::Ok();
}

void NvencEncoder::shutdown() {
    stopped_ = true;
    if (stopEvent_) SetEvent(stopEvent_);
    {
        // A waiter between its predicate check and the wait must not miss this.
        std::lock_guard<std::mutex> lock(flightMu_);
    }
    flightCv_.notify_all();
}

}  // namespace

Probe probeNvenc() {
    const NvencProbe& p = nvencProbe();
    if (!p.ok) return {false, p.reason};
    std::string codecs;
    for (const auto& [c, d] : p.codecs) {
        if (d.available) codecs += std::string(codecs.empty() ? "" : ", ") + codecName(c);
    }
    return {true, nvencRuntime().versionText + " on " + p.adapter.name + ": " + codecs};
}

Caps probeNvencCaps() { return capsFrom(runNvencProbe()); }

std::unique_ptr<Backend> createNvencBackend(Status& err) {
    const NvencProbe& p = nvencProbe();
    if (!p.ok) {
        err = Status::Error("unavailable", p.reason);
        return nullptr;
    }
    return std::make_unique<NvencEncoder>();
}

}  // namespace recon
