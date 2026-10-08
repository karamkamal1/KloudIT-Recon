// AMF encoder backend (AMD VCN): H.264, HEVC and AV1 through amfrt64.dll,
// loaded from System32 at run time (amf/amf_runtime.cpp: LoadLibraryExW with
// LOAD_LIBRARY_SEARCH_SYSTEM32 -> AMFQueryVersion -> AMFInit). GUIDE 3.3.
//
// Set-up: AMFContext::InitDX11 on the capture's D3D11 device (or the AMD
// Direct Capture context, so its surfaces can go to the encoder unconverted),
// AMFFactory::CreateComponent(AMFVideoEncoder_HEVC / _AV1 / VCE_AVC), every
// property of GUIDE 3.3 (USAGE first: it "fully configures parameter set"),
// Init(NV12 | the capture format, coded size), the dynamic ones again after
// Init (FFmpeg's amfenc_h264.c sets them there; GUIDE 2 "controls must be
// applied after Init()").
//
// Frames: converted NV12 pool textures are wrapped with
// CreateSurfaceFromDX11Native and this backend as the AMFSurfaceObserver,
// which returns the pool texture when AMF releases the surface (OBS
// texture-amf.cpp); AMD Direct Capture surfaces are submitted as they are
// (Streaming SDK AVStreamer.cpp). Per-frame properties carry forced IDRs
// (+ parameter sets, as FFmpeg's forced_idr), LTR marks / references
// (codec/ltr.hpp) and the ROI map; a custom property carries the frame id
// to the output buffer (FFmpeg amfenc.c "PtsProp", OBS "PTS").
//
// Threads: the capture thread submits, the output thread queries (the AMF
// SimpleEncoder sample's polling thread), the control thread records
// requests that the capture thread applies before the next SubmitInput
// ("dynamic properties ... will be flushed to encoder only before the next
// Submit() call", AMF_Video_Encode_HEVC_API.md 2.2.2). Flush/ReInit exclude
// SubmitInput and QueryOutput (a shared_mutex). No AMF call is made with the
// D3D11 device lock (ID3D10Multithread) held: AMF takes it itself
// (AMFContext::LockDX11), and holding it across a call that waits for AMF's own
// threads could deadlock. (FFmpeg's amf_submit_frame_locked takes a plain
// mutex of its own, not the device lock.)
//
// Phase 5 (GUIDE 9): temporal SVC (MAX_NUM_TEMPORAL_LAYERS before Init,
// NUM_TEMPORAL_LAYERS; LTR marks and recovery frames on base-layer frames only,
// codec/ltr.hpp; OUTPUT_TEMPORAL_LAYER, AV1 the OBU extension, and the
// discardable flag from codec/bitstream.hpp), runtime frame-rate changes
// (FRAMERATE before the next SubmitInput; a key frame right after one is
// logged: VERIFY no IDR), INSTANCE_INDEX (caps instanceSelect), and the
// sub-frame output experiment (start sliceOutput: OUTPUT_MODE SLICE / TILE,
// the parts put back together by codec/slices.hpp, stats firstSliceQpc).
// AMF cannot encode a frame without advancing its state: no reencodeOversized.
//
// HDR10 (GUIDE 3.9; start hdr from an HDR source): P010 input from the colour
// conversion (BT.2020 PQ, limited range), COLOR_BIT_DEPTH 10, HEVC
// PROFILE_MAIN_10 (AV1 Main), input and output colour profile / transfer /
// primaries BT.2020 / SMPTE 2084 / BT.2020, and INPUT_HDR_METADATA, an
// AMFBuffer of AMFHDRMetadata (codec/hdr.hpp), which the encoder writes as the
// mastering display and content light level SEI / metadata OBUs (OBS
// texture-amf.cpp and FFmpeg amfenc.c set the same properties).
#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstring>
#include <deque>
#include <map>
#include <mutex>
#include <set>
#include <shared_mutex>
#include <unordered_map>

#include <AMF/components/Component.h>
#include <AMF/components/ComponentCaps.h>
#include <AMF/core/Buffer.h>
#include <AMF/core/Context.h>
#include <AMF/core/Surface.h>
#include <AMF/core/Trace.h>

#include "amf/amf_props.hpp"
#include "amf/amf_runtime.hpp"
#include "codec/bitstream.hpp"
#include "codec/hdr.hpp"
#include "codec/ltr.hpp"
#include "codec/slices.hpp"
#include "d3d/device.hpp"
#include "probes.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

// Carries the frame id from the input surface to the output buffer (AMF copies
// a surface's properties to its output buffer).
constexpr const wchar_t* kFrameIdProp = L"ReconFrameId";
// QUERY_TIMEOUT when the encoder supports it: QueryOutput blocks up to this
// long for a frame (GUIDE 3.3: 1-20 ms; FFmpeg uses 1, the Streaming SDK 20).
// It also bounds how long Flush/ReInit and shutdown wait for the output thread.
constexpr int kQueryTimeoutMs = 5;
constexpr int kInputQueueSize = 2;   // GUIDE 3.3 / 10 (FFmpeg async_depth=1 equivalent)
constexpr int kDefaultRefs = 4;      // MAX_NUM_REFRAMES (GUIDE 3.3)
constexpr int kSubmitRetries = 3;    // AMF_INPUT_FULL: 1 ms apart, then the frame is dropped
constexpr int kMaxErrors = 10;       // consecutive SubmitInput / QueryOutput failures before giving up
constexpr int64_t kLtrAckTimeoutMs = 1000;

std::string resultText(AMF_RESULT r) {
    std::string name;
    const AmfRuntime& rt = amfRuntime();
    amf::AMFTrace* trace = nullptr;
    if (rt.factory && rt.factory->GetTrace(&trace) == AMF_OK && trace) {
        if (const wchar_t* t = trace->GetResultText(r)) name = toUtf8(t);
    }
    return "AMF_RESULT " + std::to_string(int(r)) + (name.empty() ? "" : " " + name);
}

std::string amfError(const std::string& what, AMF_RESULT r) { return what + " failed (" + resultText(r) + ")"; }

template <typename T>
bool getProp(amf::AMFPropertyStorage* s, const wchar_t* name, T& out) {
    if (!s || !name) return false;
    T v{};
    if (s->GetProperty(name, &v) != AMF_OK) return false;
    out = v;
    return true;
}

// Sets properties and remembers the ones that failed: required ones fail the
// start, the rest are logged.
class PropSetter {
public:
    explicit PropSetter(amf::AMFPropertyStorage* s) : s_(s) {}
    template <typename T>
    AMF_RESULT set(const wchar_t* name, const T& value, bool required = false) {
        if (!name) return AMF_NOT_SUPPORTED;
        const AMF_RESULT r = s_->SetProperty(name, value);
        if (r != AMF_OK) (required ? errors : warnings).push_back(toUtf8(name) + " (" + resultText(r) + ")");
        return r;
    }
    AMF_RESULT setInt(const wchar_t* name, amf_int64 v, bool required = false) { return set(name, v, required); }
    AMF_RESULT setBool(const wchar_t* name, bool v, bool required = false) { return set(name, amf_bool(v), required); }
    std::string errorText() const { return join(errors); }
    std::string warningText() const { return join(warnings); }
    std::vector<std::string> errors, warnings;

private:
    static std::string join(const std::vector<std::string>& v) {
        std::string out;
        for (const auto& s : v) out += (out.empty() ? "" : ", ") + s;
        return out;
    }
    amf::AMFPropertyStorage* s_;
};

// --- Capabilities ------------------------------------------------------------------

struct CodecDetails {
    bool available = false;
    std::string reason;  // why not
    CodecCaps caps;
    int minW = 0, minH = 0;
    int maxRefs = 16;
    int64_t maxBitrate = 0;
    std::vector<int> inputFormats;  // amf::AMF_SURFACE_FORMAT
};

bool hasFormat(const CodecDetails& d, int f) { return std::find(d.inputFormats.begin(), d.inputFormats.end(), f) != d.inputFormats.end(); }

// Zero-copy input is 8-bit UNORM BGRA / RGBA only. The AMD Streaming SDK sends
// sRGB and R10G10B10A2 capture surfaces through its VideoConverter instead
// ("EFC is not supported for sRGB formats, force use of VideoConverter",
// VideoEncodeEngine::IsFormatSupported, which checks the D3D11 texture format
// of every input surface), and an FP16 surface (HDR desktop) would be encoded
// as 8-bit BT.709 without the scRGB -> sRGB step of the NV12 converter.
bool zeroCopyFormat(int amfFormat) { return amfFormat == amf::AMF_SURFACE_BGRA || amfFormat == amf::AMF_SURFACE_RGBA; }

// The texture behind a zero-copy surface: an sRGB-typed (game swap chain),
// 10-bit or FP16 one is refused although AMF may call it BGRA. 8-bit TYPELESS
// passes, as in the Streaming SDK (its check lists only the sRGB and 10-bit types).
bool zeroCopyTexture(amf::AMFPlane* plane, DXGI_FORMAT& format) {
    format = DXGI_FORMAT_UNKNOWN;
    auto* tex = static_cast<ID3D11Texture2D*>(plane->GetNative());
    if (!tex) return false;
    D3D11_TEXTURE2D_DESC d{};
    tex->GetDesc(&d);
    format = d.Format;
    switch (d.Format) {
    case DXGI_FORMAT_B8G8R8A8_UNORM:
    case DXGI_FORMAT_B8G8R8X8_UNORM:
    case DXGI_FORMAT_R8G8B8A8_UNORM:
    case DXGI_FORMAT_B8G8R8A8_TYPELESS:
    case DXGI_FORMAT_B8G8R8X8_TYPELESS:
    case DXGI_FORMAT_R8G8B8A8_TYPELESS:
        return true;
    default:
        return false;
    }
}

// What GetCaps says about one codec on this GPU (AMF_Video_Encode_*_API.md
// table A-3; the AMF EncoderLatency sample reads NUM_OF_HW_INSTANCES the same way).
// svcLayers > 1: MAX_NUM_TEMPORAL_LAYERS is set first, since some caps depend
// on it (AV1 CAP_MAX_NUM_LTR_FRAMES "is calculated based on current value of
// AMF_VIDEO_ENCODER_AV1_MAX_NUM_TEMPORAL_LAYERS").
CodecDetails readDetails(amf::AMFComponent* enc, const AmfCodecProps& P, int svcLayers = 1) {
    CodecDetails d;
    if (svcLayers > 1 && P.maxTemporalLayers) enc->SetProperty(P.maxTemporalLayers, amf_int64(svcLayers));
    amf::AMFCapsPtr caps;
    AMF_RESULT r = enc->GetCaps(&caps);
    if (r != AMF_OK || !caps) {
        d.reason = amfError("AMFComponent::GetCaps", r);
        return d;
    }
    CodecCaps& c = d.caps;
    amf::AMFIOCapsPtr in;
    if (caps->GetInputCaps(&in) == AMF_OK && in) {
        amf_int32 lo = 0, hi = 0;
        in->GetWidthRange(&lo, &hi);
        d.minW = lo, c.maxW = hi;
        in->GetHeightRange(&lo, &hi);
        d.minH = lo, c.maxH = hi;
        for (amf_int32 i = 0; i < in->GetNumOfFormats(); ++i) {
            amf::AMF_SURFACE_FORMAT f{};
            amf_bool native = false;
            if (in->GetFormatAt(i, &f, &native) == AMF_OK) d.inputFormats.push_back(int(f));
        }
    }
    amf_int64 v = 0;
    amf_bool b = false;
    if (getProp(caps.GetPtr(), P.capHwInstances, v)) c.hwInstances = int(std::max<amf_int64>(1, v));
    if (getProp(caps.GetPtr(), P.capMaxTemporalLayers, v)) c.maxTemporalLayers = int(std::max<amf_int64>(1, v));
    if (getProp(caps.GetPtr(), P.capMaxRefs, v) && v > 0) d.maxRefs = int(std::min<amf_int64>(v, P.maxRefsLimit));
    else d.maxRefs = P.maxRefsLimit;
    getProp(caps.GetPtr(), P.capMaxBitrate, d.maxBitrate);
    // AV1 has no ROI cap; "For some reason there's no specific CAP for AV1, but
    // should always be supported" (OBS texture-amf.cpp). Reported, but marked
    // as assumed.
    const bool roi = P.capRoi ? getProp(caps.GetPtr(), P.capRoi, b) && b : P.roiData != nullptr;
    c.roi = roi ? "importance" : "none";
    if (!P.capRoi && P.roiData) c.assumed.push_back("roi");
    if (P.capQueryTimeout && getProp(caps.GetPtr(), P.capQueryTimeout, b)) {
        c.queryTimeout = b;
    } else {
        // FFmpeg amfenc_*.c: set QUERY_TIMEOUT and read it back.
        amf_int64 t = 0;
        c.queryTimeout = enc->SetProperty(P.queryTimeout, amf_int64(1)) == AMF_OK && getProp(enc, P.queryTimeout, t) && t == 1;
    }
    c.sliceOutput = getProp(caps.GetPtr(), P.capSliceOutput, b) && b;
    const bool p010 = hasFormat(d, amf::AMF_SURFACE_P010);
    if (P.codec == Codec::Hevc) c.tenBit = getProp(caps.GetPtr(), P.capMaxProfile, v) && v >= P.profileMain10 && p010;
    else if (P.codec == Codec::Av1) c.tenBit = p010;  // AV1 Main covers 10-bit
    // HDR10: 10-bit P010 input plus the HDR metadata property (HEVC, AV1).
    c.hdr10 = c.tenBit && P.inputHdrMetadata && P.codec != Codec::H264;
    // LTR: AV1 reports its maximum; H.264 is documented as 0..2; HEVC as
    // 0..16 shared with the short-term references (one stays short-term).
    c.maxLtr = P.docMaxLtr;
    if (P.capMaxLtr && getProp(caps.GetPtr(), P.capMaxLtr, v)) c.maxLtr = int(std::clamp<amf_int64>(v, 0, P.docMaxLtr));
    else c.assumed.push_back("maxLtr");
    if (P.codec == Codec::Hevc) c.maxLtr = std::min(c.maxLtr, std::max(0, d.maxRefs - 1));
    c.recovery = c.maxLtr >= 2 ? "ltr" : "none";  // the A/B slot policy needs two slots (codec/ltr.hpp)
    if (P.capAlignW) {
        // RDNA3 codes AV1 in 64x16 multiples; RDNA4 relaxes it. FFmpeg's
        // amfenc_av1.c reads the factors from the encoder and assumes 64x16
        // ("older driver and Navi3x") when they are missing.
        // init() reads them again from the initialized encoder, where FFmpeg
        // reads them.
        amf_int64 w = 0, h = 0;
        const bool gotW = (getProp(caps.GetPtr(), P.capAlignW, w) || getProp(enc, P.capAlignW, w)) && w > 0;
        const bool gotH = (getProp(caps.GetPtr(), P.capAlignH, h) || getProp(enc, P.capAlignH, h)) && h > 0;
        c.alignW = int(gotW ? w : 64);
        c.alignH = int(gotH ? h : 16);
        if (!gotW || !gotH) c.assumed.insert(c.assumed.end(), {"alignW", "alignH"});
    }
    c.forceIdr = true;
    c.liveBitrate = "seamless";     // AMD Streaming SDK UpdateBitrate: SetProperty, no flush; recon-host qualify (3.6) measures it
    c.assumed.push_back("liveBitrate");
    // FRAMERATE is a dynamic property like the bitrate (applied before the
    // next SubmitInput); whether it changes without an IDR is a VERIFY item
    // (docs/VENDOR_NOTES.md Phase 5): assumed.
    c.liveFps = "seamless";
    c.assumed.push_back("liveFps");
    c.instanceSelect = P.instanceIndex != nullptr;  // INSTANCE_INDEX picks the VCN engine
    c.reencode = false;  // no encode without advancing the encoder's state
    c.yuv444 = false;
    // Intra refresh (without user LTR and SVC: AMF docs, MAX_LTR_FRAMES
    // remarks): reported only when this encoder takes the property and reads
    // it back, after USAGE and MAX_NUM_REFRAMES as a start sets them (H.264's
    // "IntraRefreshNumMBsPerSlot ... available only when MaxOfReferenceFrames
    // is greater than 1"; FFmpeg checks QUERY_TIMEOUT the same way). Last: the
    // USAGE resets the other properties of this never-initialized probe encoder.
    if (const wchar_t* prop = P.intraRefreshPerSlot ? P.intraRefreshPerSlot : P.intraRefreshMode) {
        const amf_int64 want = P.intraRefreshPerSlot ? 1 : P.intraRefreshContinuous;
        amf_int64 got = -1;
        enc->SetProperty(P.usage, P.usageUltraLowLatency);
        enc->SetProperty(P.maxRefs, amf_int64(kDefaultRefs));
        c.intraRefresh = enc->SetProperty(prop, want) == AMF_OK && getProp(enc, prop, got) && got == want;
    }
    d.available = c.maxW > 0 && c.maxH > 0;
    if (!d.available) d.reason = "the encoder reports no input size range";
    return d;
}

struct AmfProbe {
    bool ok = false;
    std::string reason;
    AdapterInfo adapter;
    std::map<Codec, CodecDetails> codecs;
};

// Creates every encoder once on the first AMD adapter (a plain D3D11 device and
// an AMF context, released afterwards) and reads its caps. Once per process.
const AmfProbe& amfProbe() {
    static const AmfProbe probe = [] {
        AmfProbe p;
        const AmfRuntime& rt = amfRuntime();
        if (!rt.factory) {
            p.reason = rt.error;
            return p;
        }
        ComPtr<IDXGIFactory1> factory;
        ComPtr<IDXGIAdapter1> adapter;
        if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf())))) {
            p.reason = "CreateDXGIFactory1 failed";
            return p;
        }
        // The AMD adapter streams are encoded on: adapter 0 (the primary
        // display's) if it is AMD, else the one with the most video memory (a
        // Radeon rather than a Ryzen iGPU). init() reads the caps again on the
        // capture's own device.
        ComPtr<IDXGIAdapter1> a;
        SIZE_T bestMemory = 0;
        for (UINT i = 0; factory->EnumAdapters1(i, a.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++i) {
            DXGI_ADAPTER_DESC1 d{};
            if (FAILED(a->GetDesc1(&d)) || (d.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) || d.VendorId != 0x1002) continue;
            if (adapter && i > 0 && d.DedicatedVideoMemory <= bestMemory) continue;
            adapter = a;
            bestMemory = d.DedicatedVideoMemory;
            p.adapter = describeAdapter(d.VendorId, d.AdapterLuid, d.Description);
            if (i == 0) break;
        }
        if (!adapter) {
            p.reason = "AMF runtime " + rt.versionText + " found, but no AMD adapter";
            return p;
        }
        static const D3D_FEATURE_LEVEL levels[] = {D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0};
        ComPtr<ID3D11Device> device;
        D3D_FEATURE_LEVEL level{};
        auto create = [&](const D3D_FEATURE_LEVEL* lv, UINT n) {
            return D3D11CreateDevice(adapter.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr, D3D11_CREATE_DEVICE_BGRA_SUPPORT, lv, n,
                                     D3D11_SDK_VERSION, device.ReleaseAndGetAddressOf(), &level, nullptr);
        };
        HRESULT hr = create(levels, 2);
        if (hr == E_INVALIDARG) hr = create(levels + 1, 1);
        if (FAILED(hr)) {
            p.reason = "D3D11CreateDevice on " + p.adapter.name + " failed: " + d3d::hrText(hr);
            return p;
        }
        amf::AMFContextPtr ctx;
        AMF_RESULT r = rt.factory->CreateContext(&ctx);
        if (r == AMF_OK) r = ctx->InitDX11(device.Get(), level >= D3D_FEATURE_LEVEL_11_1 ? amf::AMF_DX11_1 : amf::AMF_DX11_0);
        if (r != AMF_OK) {
            p.reason = amfError("AMF context on " + p.adapter.name, r);
            if (ctx) ctx->Terminate();
            return p;
        }
        const int64_t t0 = qpcNow();
        for (const Codec c : {Codec::H264, Codec::Hevc, Codec::Av1}) {
            const AmfCodecProps& P = amfProps(c);
            amf::AMFComponentPtr enc;
            r = rt.factory->CreateComponent(ctx, P.component, &enc);
            CodecDetails d;
            if (r != AMF_OK || !enc) d.reason = amfError("creating " + toUtf8(P.component), r) + (c == Codec::Av1 ? " (AV1 needs RDNA3 or newer)" : "");
            else d = readDetails(enc, P);
            enc = nullptr;  // never initialized: nothing to Terminate
            p.codecs[c] = d;
            p.ok = p.ok || d.available;
        }
        ctx->Terminate();
        logf(LogLevel::Debug, "amf probe: %lld ms", static_cast<long long>((qpcNow() - t0) * 1000 / qpcFrequency()));
        if (!p.ok) p.reason = "AMF runtime " + rt.versionText + ": no usable encoder on " + p.adapter.name;
        return p;
    }();
    return probe;
}

// --- Backend -------------------------------------------------------------------------

struct RateValues {
    amf_int64 target = 0, peak = 0, vbv = 0;
};

// Peak = target (CBR, GUIDE 3.3; for LATENCY_CONSTRAINED_VBR and
// PEAK_CONSTRAINED_VBR too, as Sunshine passes rc_max_rate = bit_rate: the
// target is what the rate controller lets the network carry); VBV = bitrate / fps x vbvFrames (GUIDE 3.3:
// 1.0-1.5 frames; the Streaming SDK uses one frame).
RateValues rateValues(int kbps, double vbvFrames, int fps) {
    RateValues v;
    v.target = v.peak = amf_int64(kbps) * 1000;
    v.vbv = std::max<amf_int64>(1, amf_int64(double(v.target) * vbvFrames / std::max(1, fps) + 0.5));
    return v;
}

class AmfEncoder : public Backend, public amf::AMFSurfaceObserver {
public:
    AmfEncoder() {
        stopEvent_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
        freq_ = qpcFrequency();
    }
    ~AmfEncoder() override {
        release();
        if (stopEvent_) CloseHandle(stopEvent_);
    }
    const char* name() const override { return "amf"; }
    Caps caps() override;
    Status init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) override;
    Status submit(const EncoderFrame& frame, const SubmitInfo& info) override;
    Next receive(EncodedFrame& out, int timeoutMs, Status& err) override;
    void releaseOutput(EncodedFrame& f) override;
    Status forceIdr() override;
    Status recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) override;
    Status setRate(const RateParams& r) override;
    Status setRoi(const std::vector<RoiRect>& rects) override;
    Status ack(uint64_t frameId) override;
    void shutdown() override;
    // Releases everything of a stream: the destructor, a start after a failed
    // one, and stream.cpp when the start fails after init().
    void release() override;

    // AMFSurfaceObserver: AMF is done with a wrapped pool texture.
    void AMF_STD_CALL OnSurfaceDataRelease(amf::AMFSurface* surface) override;

private:
    struct Hold {
        std::shared_ptr<void> pool;        // keeps the converter's pool texture reserved
        ComPtr<ID3D11Texture2D> texture;   // and alive (the converter may be destroyed first)
    };
    struct InFlight {
        uint64_t frameId = 0;
        SubmitInfo info;
        LtrTracker::Plan plan;
        uint32_t gen = 0;
        amf::AMFSurface* nativeSurface = nullptr;  // zero-copy: the capture's surface (not referenced)
    };

    Status validate(const StartParams& p);
    Status createAndConfigure(amf_int64 usage);
    Status initEncoder();
    void applyDynamic(PropSetter& s);
    amf_int64 intraRefreshBlocks() const;
    int readIntraRefresh();
    void readExtradata();
    Status applyRate(const RateParams& r, bool& idr);
    Status roiSurface(const RoiMap& m, amf::AMFSurfacePtr& out);
    Status buildRoi(const std::vector<RoiRect>& rects);
    Status submitFailed(AMF_RESULT r, const char* what);
    void warnOnce(const std::string& key, const std::string& text);
    amf_pts toPts(int64_t qpc) const { return amf_pts(qpc / freq_ * AMF_SECOND + qpc % freq_ * AMF_SECOND / freq_); }

    const AmfCodecProps* P_ = nullptr;
    Codec codec_ = Codec::Hevc;
    CodecDetails det_;
    StartParams start_;
    amf::AMFContextPtr ctx_;
    bool ownContext_ = false;
    amf::AMFComponentPtr enc_;
    ComPtr<ID3D11Device> device_;
    uint32_t width_ = 0, height_ = 0, codedW_ = 0, codedH_ = 0;
    amf::AMF_SURFACE_FORMAT inputFormat_ = amf::AMF_SURFACE_NV12;
    bool zeroCopy_ = false;
    std::optional<HdrMetadata> hdr_;  // HDR10 stream: its metadata
    amf::AMFBufferPtr hdrBuffer_;     // INPUT_HDR_METADATA
    bool flushMode_ = false;
    int queryTimeoutMs_ = 0;
    amf_int64 usage_ = 0;
    int svcLayers_ = 1;  // temporal layers the encoder runs
    int baseRun_ = 0;    // output thread: consecutive non-key layer-0 frames (SVC)
    int64_t freq_ = 1;

    // Current rate (capture thread after init).
    int kbps_ = 0, fps_ = 60;
    double vbvFrames_ = 1.0;

    // Requests from the control thread, applied by the capture thread.
    std::mutex ctlMu_;
    bool idrPending_ = true;
    bool rateDirty_ = false;
    RateParams pendingRate_;
    bool roiDirty_ = false;
    std::vector<RoiRect> roiRects_;

    // Capture thread.
    amf::AMFSurfacePtr roiMap_, roiUniform_;
    bool roiEverSet_ = false;
    int submitErrors_ = 0;

    std::mutex flightMu_;
    std::condition_variable flightCv_;
    std::deque<InFlight> flight_;

    std::mutex holdsMu_;
    std::unordered_map<amf::AMFSurface*, Hold> holds_;

    LtrTracker ltr_;
    std::shared_mutex componentMu_;  // shared: SubmitInput / QueryOutput; exclusive: Flush + ReInit
    std::atomic<bool> stopped_{false};
    std::atomic<uint32_t> gen_{0};
    HANDLE stopEvent_ = nullptr;

    // A FRAMERATE change was applied before this frame (0 = none): a key
    // frame right after it means the change was not seamless (VERIFY).
    std::atomic<uint64_t> fpsChangeFrame_{0};

    // Output thread.
    PreciseTimer timer_;
    // Sub-frame output (start sliceOutput): the parts of the frame being put
    // together, and their buffers for the frame properties (output thread only).
    SliceAssembler slices_;
    std::vector<amf::AMFBufferPtr> parts_;
    std::atomic<bool> resetSlices_{false};  // set by a flush (capture thread)
    std::vector<uint8_t> extradata_;  // guarded by extraMu_ (rewritten after ReInit)
    std::mutex extraMu_;
    std::vector<uint8_t> patched_;    // a key frame with parameter sets inserted
    int queryErrors_ = 0;

    std::mutex warnMu_;
    std::set<std::string> warned_;
};

Caps AmfEncoder::caps() {
    const AmfProbe& probe = amfProbe();
    Caps c;
    c.backend = probe.ok ? "amf" : "none";
    c.vendor = "amd";
    if (probe.adapter.found) {
        c.adapterLuid = probe.adapter.luid;
        c.adapterName = probe.adapter.name;
        c.hagsEnabled = probe.adapter.hags;
    }
    for (const auto& [codec, d] : probe.codecs) {
        if (d.available) c.codecs[codecName(codec)] = d.caps;
        else c.unavailable.emplace_back(std::string("amf-") + codecName(codec), d.reason);
    }
    return c;
}

void AmfEncoder::warnOnce(const std::string& key, const std::string& text) {
    {
        std::lock_guard<std::mutex> lock(warnMu_);
        if (!warned_.insert(key).second) return;
    }
    logf(LogLevel::Warn, "amf: %s", text.c_str());
}

void AmfEncoder::release() {
    // The threads that used the stream have been joined, or never ran
    // (Backend contract).
    if (enc_) enc_->Terminate();  // releases the surfaces still queued (OnSurfaceDataRelease)
    enc_ = nullptr;
    roiMap_ = nullptr;
    roiUniform_ = nullptr;
    hdrBuffer_ = nullptr;
    if (ctx_ && ownContext_) ctx_->Terminate();
    ctx_ = nullptr;
    ownContext_ = false;
    {
        std::lock_guard<std::mutex> lock(holdsMu_);
        holds_.clear();
    }
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        flight_.clear();
    }
    device_.Reset();
}

void AmfEncoder::OnSurfaceDataRelease(amf::AMFSurface* surface) {
    Hold h;
    {
        std::lock_guard<std::mutex> lock(holdsMu_);
        auto it = holds_.find(surface);
        if (it == holds_.end()) return;
        h = std::move(it->second);
        holds_.erase(it);
    }
    // h goes out of scope here, outside the lock: the pool texture is free again.
}

// Creates the encoder component and sets every property before Init (GUIDE 3.3
// table, AMF docs Annex A). usage: ULTRA_LOW_LATENCY, or LOW_LATENCY for the
// H.264 fallback.
Status AmfEncoder::createAndConfigure(amf_int64 usage) {
    enc_ = nullptr;
    AMF_RESULT r = amfRuntime().factory->CreateComponent(ctx_, P_->component, &enc_);
    if (r != AMF_OK || !enc_) return Status::Error("init_failed", amfError("creating " + toUtf8(P_->component), r));
    const StartParams& p = start_;
    PropSetter s(enc_);
    // "AMF_VIDEO_ENCODER_USAGE needs to be set before the rest" (AMF EncoderLatency sample).
    s.setInt(P_->usage, usage, true);
    // Required when a particular engine was asked for: a driver that refuses
    // it must not leave the start reporting that engine.
    s.setInt(P_->instanceIndex, std::max(0, p.encoderInstance), p.encoderInstance > 0);
    s.set(P_->frameSize, AMFConstructSize(amf_int32(codedW_), amf_int32(codedH_)), true);
    s.set(P_->frameRate, AMFConstructRate(amf_uint32(fps_), 1));
    s.setInt(P_->profile, hdr_ ? P_->profile10Value : P_->profileValue, bool(hdr_));
    s.setBool(P_->lowLatencyMode, true);  // "sets high priority queue ... POC mode 2" (Streaming SDK GPUEncoderHEVC.cpp)
    s.setInt(P_->encodingLatencyMode, P_->lowestLatency);
    const amf_int64 preset = p.quality == "quality" ? P_->presetQuality : p.quality == "balanced" ? P_->presetBalanced : P_->presetSpeed;
    s.setInt(P_->qualityPreset, preset);
    s.setInt(P_->rateControl, p.rc == "cbr" ? P_->rcCbr : p.rc == "vbr_peak" ? P_->rcPeakVbr : P_->rcLatencyVbr, true);
    s.setBool(P_->preAnalysis, false);
    if (P_->preEncodeIsInt) s.setInt(P_->preEncode, AMF_VIDEO_ENCODER_PREENCODE_DISABLED);
    else s.setBool(P_->preEncode, false);
    s.setBool(P_->vbaq, true);
    s.setInt(P_->aqMode, P_->aqCaq);
    s.setInt(P_->gopSize, 0);  // infinite GOP: IDRs only when forced (A4)
    s.setInt(P_->gopsPerIdr, 1);  // Sunshine gops_per_idr 1
    s.setInt(P_->headerInsertion, P_->headerKeyAligned);
    s.setInt(P_->bPicPattern, 0);  // no B frames (output in submission order)
    const int ltr = start_.ltrSlots;
    const int refs = std::min(det_.maxRefs, std::max(kDefaultRefs, ltr + 1));
    s.setInt(P_->maxRefs, refs);
    if (ltr > 0) {
        s.setInt(P_->maxLtr, ltr, true);
        s.setInt(P_->ltrMode, P_->ltrKeepUnused, true);
    }
    // NUM_TEMPORAL_LAYERS is dynamic: applyDynamic (after this maximum).
    if (p.svcLayers > 1) s.setInt(P_->maxTemporalLayers, p.svcLayers, true);
    if (p.sliceOutput > 0) {
        // Sub-frame output (Phase 5 experiment): every slice / tile comes out
        // as a buffer of its own (OUTPUT_BUFFER_TYPE SLICE.., SLICE_LAST);
        // AV1 one tile per tile group OBU. The count is "treated as
        // suggestion" (AV1) and read back after Init.
        s.setInt(P_->slicesPerFrame, p.sliceOutput, true);
        s.setInt(P_->outputMode, P_->outputModeParts, true);
        if (P_->tileGroupObu) s.setBool(P_->tileGroupObu, true);
    }
    if (queryTimeoutMs_) s.setInt(P_->queryTimeout, queryTimeoutMs_);  // read back after Init
    s.setInt(P_->inputQueueSize, kInputQueueSize);
    // AV1 switch frames clear every LTR slot ("When we encode a key frame or
    // switch frame, all save LTR slots will be cleared", AMF_Video_Encode_AV1_API.md
    // 2.2.7), and their insertion "depends on USAGE": off. receive() still
    // treats one as clearing the slots.
    if (P_->switchFrameMode) s.setInt(P_->switchFrameMode, P_->switchFrameNone);
    // AV1 screen content tools for desktop text (GUIDE 4.2): palette mode
    // codes a block of few colours (text, UI) as a palette and indices. Both
    // are documented on by default (VideoEncoderAV1.h); set here so that a
    // usage or driver default cannot leave them off. Not FORCE_INTEGER_MV:
    // whole-pixel motion suits scrolling text, not a game's sub-pixel motion.
    if (P_->screenContentTools) {
        s.setBool(P_->screenContentTools, true);
        s.setBool(P_->paletteMode, true);
    }
    if (P_->alignmentMode && det_.caps.alignW == 64 && det_.caps.alignH == 16) {
        // We pad to 64x16 ourselves (InputSpec::content*), so the strict mode
        // holds and nothing is padded or cropped behind our back.
        s.setInt(P_->alignmentMode, AMF_VIDEO_ENCODER_AV1_ALIGNMENT_MODE_64X16_ONLY);
    } else if (P_->alignmentMode) {
        s.setInt(P_->alignmentMode, AMF_VIDEO_ENCODER_AV1_ALIGNMENT_MODE_NO_RESTRICTIONS);
    }
    if (hdr_) {
        // HDR10: what the converter writes into P010 (BT.2020 matrix, PQ,
        // limited range) and the VUI / AV1 colour config the client decodes
        // by; the bit depth and profile are required (an 8-bit stream would
        // be wrong, not just worse).
        s.setInt(P_->colorBitDepth, AMF_COLOR_BIT_DEPTH_10, true);
        s.setInt(P_->inputColorProfile, AMF_VIDEO_CONVERTER_COLOR_PROFILE_2020);
        s.setInt(P_->inputTransfer, AMF_COLOR_TRANSFER_CHARACTERISTIC_SMPTE2084);
        s.setInt(P_->inputPrimaries, AMF_COLOR_PRIMARIES_BT2020);
        s.setBool(P_->inputFullRange, false);
        s.setInt(P_->outputColorProfile, AMF_VIDEO_CONVERTER_COLOR_PROFILE_2020);
        s.setInt(P_->outputTransfer, AMF_COLOR_TRANSFER_CHARACTERISTIC_SMPTE2084);
        s.setInt(P_->outputPrimaries, AMF_COLOR_PRIMARIES_BT2020);
        s.setBool(P_->outputFullRange, false);
        // Not required: without it the stream is still HDR10 by its VUI,
        // only without the metadata (logged).
        if (hdrBuffer_) s.set(P_->inputHdrMetadata, static_cast<amf::AMFInterface*>(hdrBuffer_.GetPtr()));
    } else {
        // Colour: BT.709 limited range out (what the converter writes, and the VUI
        // the browser decodes by). RGB input (zero-copy BGRA) is full range.
        s.setInt(P_->colorBitDepth, AMF_COLOR_BIT_DEPTH_8);
        if (inputFormat_ == amf::AMF_SURFACE_NV12) s.setInt(P_->inputColorProfile, AMF_VIDEO_CONVERTER_COLOR_PROFILE_709);
        s.setInt(P_->inputTransfer, AMF_COLOR_TRANSFER_CHARACTERISTIC_BT709);
        s.setInt(P_->inputPrimaries, AMF_COLOR_PRIMARIES_BT709);
        s.setBool(P_->inputFullRange, inputFormat_ != amf::AMF_SURFACE_NV12);
        s.setInt(P_->outputColorProfile, AMF_VIDEO_CONVERTER_COLOR_PROFILE_709);
        s.setInt(P_->outputTransfer, AMF_COLOR_TRANSFER_CHARACTERISTIC_BT709);
        s.setInt(P_->outputPrimaries, AMF_COLOR_PRIMARIES_BT709);
        s.setBool(P_->outputFullRange, false);
    }
    applyDynamic(s);
    if (!s.errors.empty()) return Status::Error("init_failed", "AMF " + std::string(codecName(codec_)) + " rejected " + s.errorText());
    if (!s.warnings.empty()) {
        logf(LogLevel::Info, "amf: %s: properties not accepted (their defaults stay): %s", codecName(codec_), s.warningText().c_str());
    }
    return Status::Ok();
}

// The dynamic properties: before Init, and again after Init / ReInit.
void AmfEncoder::applyDynamic(PropSetter& s) {
    const RateValues v = rateValues(kbps_, vbvFrames_, fps_);
    s.set(P_->frameRate, AMFConstructRate(amf_uint32(fps_), 1));
    s.setInt(P_->peakBitrate, v.peak);
    s.setInt(P_->targetBitrate, v.target, true);
    s.setInt(P_->vbvSize, v.vbv);
    s.setBool(P_->enforceHrd, false);  // A6: Sunshine warns HRD can cause artifacts
    s.setBool(P_->fillerData, false);
    s.setBool(P_->skipFrame, false);   // A5: ULL turns rate-control frame skipping on
    // Temporal SVC: "NUM_TEMPORAL_LAYERS is a dynamic property and can be
    // changed at any time during an encoding session" (AMF_Video_Encode_HEVC_API.md),
    // up to MAX_NUM_TEMPORAL_LAYERS (createAndConfigure, before Init only).
    if (start_.svcLayers > 1) s.setInt(P_->numTemporalLayers, start_.svcLayers, true);
    // Intra refresh: the requested cycle, else explicitly off. H.264's
    // ULTRA_LOW_LATENCY and LOW_LATENCY usages default
    // INTRA_REFRESH_NUM_MBS_PER_SLOT to 255 (AMF_Video_Encode_API.md: "Ultra low
    // latency: 255, Low latency: 255"), a refresh band in every stream without
    // LTR. Required when asked for; "off" is best effort (with user LTR the
    // property is not available, "NumOfLTR is 0" only, and intra refresh does not
    // run then anyway). init() reads back what the encoder runs.
    const int frames = start_.intraRefreshFrames;
    if (P_->intraRefreshPerSlot) {
        const amf_int64 perSlot = frames > 0 ? (intraRefreshBlocks() + frames - 1) / frames : 0;
        s.setInt(P_->intraRefreshPerSlot, perSlot, frames > 0);
    } else if (P_->intraRefreshMode && frames > 0) {
        s.setInt(P_->intraRefreshMode, P_->intraRefreshContinuous, true);
        s.setInt(P_->intraRefreshStripes, frames, true);
    } else if (P_->intraRefreshMode) {
        s.setInt(P_->intraRefreshMode, P_->intraRefreshDisabled);
    }
}

// Blocks per frame in intra refresh units (H.264 16x16 MBs, HEVC 64x64 CTBs).
amf_int64 AmfEncoder::intraRefreshBlocks() const {
    const uint32_t b = std::max(1u, P_->intraRefreshBlock);
    return amf_int64((codedW_ + b - 1) / b) * amf_int64((codedH_ + b - 1) / b);
}

// The intra refresh cycle the initialized encoder runs, in frames (0 = off),
// read from its properties rather than taken from the request.
int AmfEncoder::readIntraRefresh() {
    // "With user control of LTR, Intra-refresh features are not supported";
    // "Intra-refresh feature is not supported with SVC" (AMF encoder docs).
    if (start_.ltrSlots > 0 || start_.svcLayers > 1) return 0;
    amf_int64 v = 0;
    if (P_->intraRefreshPerSlot) {
        if (!getProp(enc_.GetPtr(), P_->intraRefreshPerSlot, v)) return start_.intraRefreshFrames;  // set (required when asked for)
        return v > 0 ? int((intraRefreshBlocks() + v - 1) / v) : 0;
    }
    if (!P_->intraRefreshMode) return 0;
    if (!getProp(enc_.GetPtr(), P_->intraRefreshMode, v)) return start_.intraRefreshFrames;
    if (v == P_->intraRefreshDisabled) return 0;
    amf_int64 stripes = 0;
    if (getProp(enc_.GetPtr(), P_->intraRefreshStripes, stripes) && stripes > 0) return int(stripes);
    return std::max(1, start_.intraRefreshFrames);  // on, cycle unknown
}

void AmfEncoder::readExtradata() {
    std::vector<uint8_t> out;
    amf::AMFInterfacePtr iface;
    if (enc_->GetProperty(P_->extradata, &iface) == AMF_OK && iface) {
        amf::AMFBufferPtr buf(iface);
        if (buf && buf->GetNative() && buf->GetSize()) {
            const auto* p = static_cast<const uint8_t*>(buf->GetNative());
            out.assign(p, p + buf->GetSize());
        }
    }
    std::lock_guard<std::mutex> lock(extraMu_);
    extradata_ = std::move(out);
}

// createAndConfigure + Init(inputFormat_, coded size), with the H.264
// LOW_LATENCY fallback. On failure enc_ may be left for release().
Status AmfEncoder::initEncoder() {
    usage_ = P_->usageUltraLowLatency;
    Status s = createAndConfigure(usage_);
    AMF_RESULT r = s.ok ? enc_->Init(inputFormat_, amf_int32(codedW_), amf_int32(codedH_)) : AMF_FAIL;
    if (s.ok && r != AMF_OK && codec_ == Codec::H264) {
        // AMF issue #410: ULTRA_LOW_LATENCY H.264 fails to initialize on some
        // drivers/GPUs; Sunshine retries h264_amf with LOW_LATENCY usage.
        logf(LogLevel::Warn, "amf: h264 Init with ULTRA_LOW_LATENCY usage failed (%s), retrying with LOW_LATENCY (AMF #410)",
             resultText(r).c_str());
        enc_->Terminate();
        usage_ = P_->usageLowLatency;
        s = createAndConfigure(usage_);
        r = s.ok ? enc_->Init(inputFormat_, amf_int32(codedW_), amf_int32(codedH_)) : AMF_FAIL;
    }
    if (!s.ok) return s;
    if (r != AMF_OK) {
        return Status::Error("init_failed", amfError("AMF " + std::string(codecName(codec_)) + " Init(" + std::to_string(codedW_) +
                                                         "x" + std::to_string(codedH_) + ")", r));
    }
    return Status::Ok();
}

// Checks a start against the encoder's caps (det_) and sets the coded size.
Status AmfEncoder::validate(const StartParams& p) {
    const CodecCaps& cc = det_.caps;
    codedW_ = alignUp(width_, uint32_t(cc.alignW));
    codedH_ = alignUp(height_, uint32_t(cc.alignH));
    if (int(codedW_) > cc.maxW || int(codedH_) > cc.maxH || int(width_) < det_.minW || int(height_) < det_.minH) {
        return Status::Error("unsupported", std::to_string(width_) + "x" + std::to_string(height_) + " is outside the " + p.codec +
                                                " encoder's " + std::to_string(det_.minW) + "x" + std::to_string(det_.minH) + " .. " +
                                                std::to_string(cc.maxW) + "x" + std::to_string(cc.maxH));
    }
    if (p.ltrSlots == 1) return Status::Error("unsupported", "ltrSlots 1: the LTR recovery policy needs 0 or at least 2 slots");
    if (p.ltrSlots > cc.maxLtr) {
        return Status::Error("unsupported", "ltrSlots " + std::to_string(p.ltrSlots) + ": " + p.codec + " supports " +
                                                std::to_string(cc.maxLtr) + " here");
    }
    if (p.svcLayers > cc.maxTemporalLayers) {
        return Status::Error("unsupported", "svcLayers " + std::to_string(p.svcLayers) + ": the encoder supports " +
                                                std::to_string(cc.maxTemporalLayers));
    }
    if (p.intraRefreshFrames > 0 && (p.ltrSlots > 0 || p.svcLayers > 1)) {
        return Status::Error("unsupported", "intra refresh does not work with user LTR or SVC (AMF MAX_LTR_FRAMES remarks)");
    }
    if (p.intraRefreshFrames > 0 && !cc.intraRefresh) {
        return Status::Error("unsupported", "the " + p.codec + " encoder does not take the intra refresh property (caps intraRefresh false)");
    }
    if (p.sliceOutput > 0 && !cc.sliceOutput) {
        return Status::Error("unsupported", "sliceOutput: the " + p.codec + " encoder has no " + (p.codec == "av1" ? "tile" : "slice") +
                                                " output (caps sliceOutput false)");
    }
    if (p.reencodeOversized > 0) {
        return Status::Error("unsupported", "reencodeOversized: AMF cannot encode a frame without advancing its state (caps reencode false)");
    }
    if (p.encoderInstance >= cc.hwInstances) {
        return Status::Error("unsupported", "encoderInstance " + std::to_string(p.encoderInstance) + ": the GPU has " +
                                                std::to_string(cc.hwInstances) + " " + p.codec + " encoder(s)");
    }
    // Whatever the source: the same request gets the same answer whether or
    // not Windows HDR is on at the moment.
    if (p.hdr && !cc.hdr10) {
        return Status::Error("unsupported", "hdr: the " + p.codec + " encoder cannot make HDR10 here (caps hdr10 false: " +
                                                (p.codec == "h264" ? "HDR10 needs hevc or av1" : "no 10-bit P010 input") + ")");
    }
    return Status::Ok();
}

Status AmfEncoder::init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) {
    release();
    stopped_ = false;
    if (stopEvent_) ResetEvent(stopEvent_);
    Codec codec;
    if (!parseCodec(p.codec, codec)) return Status::Error("unsupported", "unknown codec " + p.codec);
    const AmfProbe& probe = amfProbe();
    const auto it = probe.codecs.find(codec);
    if (it == probe.codecs.end() || !it->second.available) {
        return Status::Error("unsupported", "AMF cannot encode " + p.codec + " here: " +
                                                (it == probe.codecs.end() ? probe.reason : it->second.reason));
    }
    if (!src.device) {
        return Status::Error("unsupported", "the AMF encoder needs a GPU capture (dda, amd-direct, wgc or synthetic-gpu)");
    }
    if (src.adapter.found && src.adapter.vendor != "amd") {
        return Status::Error("unsupported", "the capture runs on " + src.adapter.name + " (" + src.adapter.vendor +
                                                "): AMF encodes on the AMD adapter's own device only");
    }
    codec_ = codec;
    P_ = &amfProps(codec);
    det_ = it->second;
    start_ = p;
    width_ = uint32_t(p.width ? p.width : int(src.width)) & ~1u;
    height_ = uint32_t(p.height ? p.height : int(src.height)) & ~1u;
    kbps_ = p.kbps;
    fps_ = p.fps;
    vbvFrames_ = p.vbvFrames;
    if (vbvFrames_ < 1.0 || vbvFrames_ > 1.5) {
        logf(LogLevel::Info, "amf: vbvFrames %.2f is outside GUIDE 3.3's 1.0-1.5", vbvFrames_);
    }
    // HDR10 when asked for and the source is HDR; an SDR source gives an SDR stream.
    hdr_.reset();
    if (p.hdr && src.hdr) hdr_ = hdrMetadataFor(src.display);

    // The context: AMD Direct Capture's (its surfaces can then be encoded as
    // they are), else one on the capture device.
    device_ = src.device;
    const AmfRuntime& rt = amfRuntime();
    AMF_RESULT r = AMF_OK;
    if (src.amfContext) {
        ctx_ = static_cast<amf::AMFContext*>(src.amfContext);
        ownContext_ = false;
    } else {
        r = rt.factory->CreateContext(&ctx_);
        if (r == AMF_OK) {
            ownContext_ = true;
            // OBS texture-amf.cpp initializes its context with AMF_DX11_1.
            r = ctx_->InitDX11(device_.Get(), device_->GetFeatureLevel() >= D3D_FEATURE_LEVEL_11_1 ? amf::AMF_DX11_1 : amf::AMF_DX11_0);
        }
        if (r != AMF_OK) {
            release();
            return Status::Error("init_failed", amfError("AMF context on the capture device", r));
        }
    }

    // The caps of the encoder on this very device (the probe used the first
    // AMD adapter, which need not be the capture's).
    {
        amf::AMFComponentPtr probeEnc;
        if (rt.factory->CreateComponent(ctx_, P_->component, &probeEnc) == AMF_OK && probeEnc) {
            CodecDetails d = readDetails(probeEnc, *P_, p.svcLayers);
            if (d.available) det_ = d;
        }
    }
    {
        Status v = validate(p);
        if (!v.ok) {
            release();
            return v;
        }
    }
    const CodecCaps& cc = det_.caps;
    flushMode_ = (p.liveBitrate.empty() ? cc.liveBitrate : p.liveBitrate) == "flush";
    queryTimeoutMs_ = cc.queryTimeout ? kQueryTimeoutMs : 0;
    if (hdr_) {
        // AMFHDRMetadata: chromaticity "normalized to 50000", luminance
        // "normalized to 10000" (ColorSpace.h), the HEVC SEI units, for AV1 too
        // (FFmpeg amfenc.c fills it the same way for every codec).
        AMF_RESULT hr = ctx_->AllocBuffer(amf::AMF_MEMORY_HOST, sizeof(AMFHDRMetadata), &hdrBuffer_);
        if (hr == AMF_OK && hdrBuffer_ && hdrBuffer_->GetNative()) {
            const MasteringCodes mc = masteringCodes(*hdr_, false);
            AMFHDRMetadata md{};
            for (int i = 0; i < 2; ++i) {
                md.redPrimary[i] = mc.red[i], md.greenPrimary[i] = mc.green[i], md.bluePrimary[i] = mc.blue[i];
                md.whitePoint[i] = mc.white[i];
            }
            md.maxMasteringLuminance = mc.maxLuminance;
            md.minMasteringLuminance = mc.minLuminance;
            md.maxContentLightLevel = amf_uint16(std::min(hdr_->maxCll, 65535));
            md.maxFrameAverageLightLevel = amf_uint16(std::min(hdr_->maxFall, 65535));
            std::memcpy(hdrBuffer_->GetNative(), &md, sizeof(md));
        } else {
            hdrBuffer_ = nullptr;
            logf(LogLevel::Warn, "amf: %s: the HDR10 stream goes without HDR metadata", amfError("AllocBuffer(AMFHDRMetadata)", hr).c_str());
        }
    }

    for (int pass = 0;; ++pass) {
        // Zero-copy: AMD Direct Capture surfaces straight into the encoder
        // when nothing has to be done to them (same size, upright, no barcode,
        // no padding) and they are 8-bit UNORM BGRA / RGBA the encoder takes
        // (zeroCopyFormat; submit() checks every surface again); else the NV12
        // conversion.
        zeroCopy_ = src.amfContext && p.zeroCopy && !p.barcode.enabled && width_ == src.width && height_ == src.height &&
                    codedW_ == width_ && codedH_ == height_ && src.rotation == 0 && zeroCopyFormat(src.amfFormat) &&
                    hasFormat(det_, src.amfFormat) && !hdr_;
        inputFormat_ = zeroCopy_ ? amf::AMF_SURFACE_FORMAT(src.amfFormat) : hdr_ ? amf::AMF_SURFACE_P010 : amf::AMF_SURFACE_NV12;
        if (pass == 0 && src.amfContext && p.zeroCopy && src.amfFormat && !zeroCopyFormat(src.amfFormat)) {
            logf(LogLevel::Info, "amf: AMD Direct Capture surface format %d is not 8-bit BGRA/RGBA: converting to %s", src.amfFormat,
                 hdr_ ? "P010" : "NV12");
        }
        Status s = initEncoder();
        if (!s.ok) {
            release();
            return s;
        }
        if (pass > 0 || !P_->capAlignW) break;
        // AV1: the alignment the initialized encoder reports. FFmpeg's
        // amfenc_av1.c reads Av1Width/HeightAlignmentFactor here, after Init
        // ("assume older driver and Navi3x" = 64x16 when they are missing).
        amf_int64 aw = 0, ah = 0;
        if (!getProp(enc_.GetPtr(), P_->capAlignW, aw) || !getProp(enc_.GetPtr(), P_->capAlignH, ah) || aw <= 0 || ah <= 0) break;
        if (codedW_ % uint32_t(aw) == 0 && codedH_ % uint32_t(ah) == 0) {
            if (aw != det_.caps.alignW || ah != det_.caps.alignH) {
                logf(LogLevel::Info, "amf: av1: the initialized encoder reports %lldx%lld alignment (caps: %dx%d); coded %ux%u fits it",
                     static_cast<long long>(aw), static_cast<long long>(ah), det_.caps.alignW, det_.caps.alignH, codedW_, codedH_);
            }
            break;
        }
        // The encoder would pad and crop on its own, and started would report
        // the wrong coded size: once more at the size it needs.
        logf(LogLevel::Warn, "amf: av1: the initialized encoder needs %lldx%lld-aligned sizes, not %dx%d as its caps said: "
                             "re-initializing at the aligned size",
             static_cast<long long>(aw), static_cast<long long>(ah), det_.caps.alignW, det_.caps.alignH);
        enc_->Terminate();
        enc_ = nullptr;
        det_.caps.alignW = int(aw);
        det_.caps.alignH = int(ah);
        Status v = validate(p);
        if (!v.ok) {
            release();
            return v;
        }
    }
    {
        PropSetter after(enc_);
        applyDynamic(after);
        if (!after.errors.empty() || !after.warnings.empty()) {
            logf(LogLevel::Debug, "amf: after Init, not accepted: %s %s", after.errorText().c_str(), after.warningText().c_str());
        }
    }
    readExtradata();

    // What the encoder took, read back, for started (the non-required
    // properties only log a rejection).
    if (queryTimeoutMs_) {
        amf_int64 t = 0;
        const bool got = getProp(enc_.GetPtr(), P_->queryTimeout, t);
        if (!got || t != queryTimeoutMs_) {
            logf(LogLevel::Info, "amf: QUERY_TIMEOUT %d ms %s: %s", queryTimeoutMs_,
                 got ? ("reads " + std::to_string(t)).c_str() : "cannot be read back",
                 got && t > 0 ? "using it" : "QueryOutput is polled every 1 ms");
            queryTimeoutMs_ = got && t > 0 ? int(std::min<amf_int64>(t, 1000)) : 0;
        }
    }
    int instance = std::max(0, p.encoderInstance);
    {
        amf_int64 v = 0;
        if (getProp(enc_.GetPtr(), P_->instanceIndex, v) && v != instance) {
            logf(LogLevel::Warn, "amf: asked for encoder instance %d, the encoder reads %lld", instance, static_cast<long long>(v));
            instance = int(v);
        }
    }
    const int intraRefresh = readIntraRefresh();
    if (intraRefresh != p.intraRefreshFrames) {
        logf(intraRefresh > 0 && p.intraRefreshFrames > 0 ? LogLevel::Info : LogLevel::Warn,
             "amf: intra refresh: asked for %d frames (0 = off), the encoder runs %d", p.intraRefreshFrames, intraRefresh);
    }
    if (p.ltrSlots > 0) {
        amf_int64 granted = 0;
        if (getProp(enc_.GetPtr(), P_->maxLtr, granted) && granted != p.ltrSlots) {
            logf(LogLevel::Warn, "amf: asked for %d LTR slots, the encoder has %lld", p.ltrSlots, static_cast<long long>(granted));
        }
    }
    // What the encoder runs: temporal layers and slices / tiles per frame.
    int layers = p.svcLayers, slices = p.sliceOutput;
    {
        amf_int64 v = 0;
        if (p.svcLayers > 1 && getProp(enc_.GetPtr(), P_->numTemporalLayers, v) && v != p.svcLayers) {
            logf(LogLevel::Warn, "amf: asked for %d temporal layers, the encoder reads %lld", p.svcLayers, static_cast<long long>(v));
            layers = int(std::clamp<amf_int64>(v, 1, 4));
        }
        if (p.sliceOutput > 0 && getProp(enc_.GetPtr(), P_->slicesPerFrame, v) && v > 0 && v != p.sliceOutput) {
            logf(LogLevel::Info, "amf: asked for %d %s per frame, the encoder reads %lld", p.sliceOutput, codec_ == Codec::Av1 ? "tiles" : "slices",
                 static_cast<long long>(v));
            slices = int(v);
        }
    }
    svcLayers_ = layers;
    LtrTracker::Config lc;
    lc.slots = p.ltrSlots;
    lc.interval = p.ltrInterval ? p.ltrInterval : std::max(1, (p.fps + 5) / 10);
    lc.ackTimeout = kLtrAckTimeoutMs * freq_ / 1000;
    lc.layers = layers;
    ltr_.reset(lc);
    slices_.reset();
    parts_.clear();
    fpsChangeFrame_ = 0;
    {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idrPending_ = true;  // the first frame: IDR with parameter sets
        rateDirty_ = false;
        roiDirty_ = false;
        roiRects_.clear();
    }
    roiEverSet_ = false;
    submitErrors_ = queryErrors_ = 0;
    gen_ = 0;

    in = InputSpec{};
    if (!zeroCopy_) {
        in.format = hdr_ ? InputSpec::Format::P010 : InputSpec::Format::Nv12;
        in.width = codedW_;
        in.height = codedH_;
        if (codedW_ != width_ || codedH_ != height_) {
            in.contentWidth = width_;
            in.contentHeight = height_;
        }
    }
    out.backend = name();
    out.codec = p.codec;
    out.width = int(width_);
    out.height = int(height_);
    out.codedWidth = int(codedW_);
    out.codedHeight = int(codedH_);
    out.fps = p.fps;
    out.kbps = p.kbps;
    out.liveBitrate = flushMode_ ? "flush" : "seamless";
    out.rateControl = p.rc == "cbr" ? "cbr" : p.rc == "vbr_peak" ? "vbr_peak" : "vbr_latency";
    out.usage = usage_ == P_->usageUltraLowLatency ? "ultra_low_latency" : "low_latency";
    out.ltrSlots = p.ltrSlots;
    out.ltrInterval = p.ltrSlots ? lc.interval : 0;
    out.encoderInstance = instance;
    out.hwInstances = cc.hwInstances;
    out.queryTimeoutMs = queryTimeoutMs_;
    out.zeroCopy = zeroCopy_;
    out.intraRefreshFrames = intraRefresh;
    out.svcLayers = layers;
    out.liveFps = flushMode_ ? "flush" : "seamless";
    out.sliceOutput = p.sliceOutput > 0 ? slices : 0;
    describeColor(out, hdr_);
    logf(LogLevel::Info,
         "amf: %s %ux%u (coded %ux%u) %d fps %d kbps %s vbv %.2f frames, usage %s, preset %s, instance %d/%d, LTR %d (every %d), "
         "temporal layers %d, %s %d, query timeout %d ms, live bitrate %s, input %s, runtime %s",
         p.codec.c_str(), width_, height_, codedW_, codedH_, fps_, kbps_, out.rateControl.c_str(), vbvFrames_, out.usage.c_str(),
         p.quality.c_str(), out.encoderInstance, cc.hwInstances, p.ltrSlots, out.ltrInterval, layers,
         codec_ == Codec::Av1 ? "tile output" : "slice output", out.sliceOutput, queryTimeoutMs_,
         out.liveBitrate.c_str(),
         zeroCopy_                                 ? "AMD Direct Capture surfaces (zero-copy)"
         : hdr_ && codec_ == Codec::Hevc ? "P010 (HDR10: BT.2020 PQ, Main10)"
         : hdr_                                    ? "P010 (HDR10: BT.2020 PQ, 10-bit)"
                                                   : "NV12",
         rt.versionText.c_str());
    if (hdr_) {
        logf(LogLevel::Info, "amf: HDR10 metadata: mastering display %.0f / %.4f cd/m2, MaxCLL %d, MaxFALL %d%s", hdr_->maxLuminance,
             hdr_->minLuminance, hdr_->maxCll, hdr_->maxFall, hdrBuffer_ ? "" : " (not passed: no buffer)");
    }
    return Status::Ok();
}

// Applies a setRate before the next SubmitInput. Seamless: SetProperty only
// (AMD Streaming SDK GPUEncoderHEVC::UpdateBitrate; "changes will be flushed to
// encoder only before the next Submit()"). Flush: forced IDR + Flush + ReInit,
// OBS's path for VBR modes (amf_hevc_update). Frame-rate changes go through
// FRAMERATE (dynamic; VERIFY: no IDR).
Status AmfEncoder::applyRate(const RateParams& r, bool& idr) {
    const int oldKbps = kbps_, oldFps = fps_;
    kbps_ = r.kbps > 0 ? r.kbps : kbps_;
    if (r.vbvFrames > 0) vbvFrames_ = r.vbvFrames;
    if (r.fps > 0) fps_ = r.fps;
    const RateValues v = rateValues(kbps_, vbvFrames_, fps_);
    if (!flushMode_) {
        PropSetter s(enc_);
        if (kbps_ >= oldKbps) {  // PEAK_BITRATE ">= TargetBitrate" at every step
            s.setInt(P_->peakBitrate, v.peak);
            s.setInt(P_->targetBitrate, v.target);
        } else {
            s.setInt(P_->targetBitrate, v.target);
            s.setInt(P_->peakBitrate, v.peak);
        }
        s.setInt(P_->vbvSize, v.vbv);
        if (fps_ != oldFps) {
            // "FPS before resolution" (GUIDE 5): a dynamic property like the
            // bitrate. VERIFY: no IDR (receive() logs a key frame right after it).
            s.set(P_->frameRate, AMFConstructRate(amf_uint32(fps_), 1));
            if (!start_.ltrInterval) ltr_.setInterval(std::max(1, (fps_ + 5) / 10));  // marks stay ~100 ms apart
        }
        if (!s.warnings.empty()) logf(LogLevel::Warn, "amf: setRate: not accepted: %s", s.warningText().c_str());
        logf(LogLevel::Debug, "amf: rate %d kbps, vbv %lld bits, %d fps (seamless)", kbps_, static_cast<long long>(v.vbv), fps_);
        return Status::Ok();
    }
    // Flush: let the frames in the encoder come out first (bounded), so the
    // flush loses none of them, then stop both threads' calls into it.
    const auto wait = std::chrono::milliseconds(std::max(20, 2000 / std::max(1, oldFps)));
    {
        std::unique_lock<std::mutex> lock(flightMu_);
        flightCv_.wait_for(lock, wait, [this] { return flight_.empty() || stopped_; });
    }
    AMF_RESULT res = AMF_OK;
    {
        std::unique_lock<std::shared_mutex> exclusive(componentMu_);
        res = enc_->Flush();
        if (res == AMF_OK) {
            PropSetter s(enc_);
            applyDynamic(s);
            res = enc_->ReInit(amf_int32(codedW_), amf_int32(codedH_));
            if (res == AMF_OK) {
                PropSetter after(enc_);
                applyDynamic(after);
            }
        }
    }
    if (res != AMF_OK) return Status::Error("encode_failed", amfError("setRate: Flush + ReInit", res), true);
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        flight_.clear();  // anything still queued was flushed
    }
    ++gen_;
    LtrTracker::Config lc;
    lc.slots = start_.ltrSlots;
    lc.interval = start_.ltrInterval ? start_.ltrInterval : std::max(1, (fps_ + 5) / 10);
    lc.ackTimeout = kLtrAckTimeoutMs * freq_ / 1000;
    lc.layers = svcLayers_;
    ltr_.reset(lc);
    // Parts of a flushed frame still in slices_: the output thread drops them
    // before the next part (also when the parts carry no frame id).
    resetSlices_ = true;
    readExtradata();
    idr = true;
    logf(LogLevel::Info, "amf: rate %d kbps, %d fps (Flush + ReInit, generation %u)", kbps_, fps_, gen_.load());
    return Status::Ok();
}

// A host-memory GRAY32 surface with one 32-bit importance per block (the AMF
// SimpleROI sample: AllocSurfaceEx(HOST, GRAY32, ..., DEFAULT | LINEAR), filled
// row by row with the plane's pitch). A new surface per change: frames still in
// the encoder keep the map they were submitted with.
Status AmfEncoder::roiSurface(const RoiMap& m, amf::AMFSurfacePtr& out) {
    out = nullptr;
    amf::AMFContext1Ptr ctx1(ctx_);
    amf::AMFSurfacePtr surf;
    AMF_RESULT r = AMF_NOT_SUPPORTED;
    if (ctx1) {
        r = ctx1->AllocSurfaceEx(amf::AMF_MEMORY_HOST, amf::AMF_SURFACE_GRAY32, amf_int32(m.cols), amf_int32(m.rows),
                                 amf::AMF_SURFACE_USAGE(amf::AMF_SURFACE_USAGE_DEFAULT | amf::AMF_SURFACE_USAGE_LINEAR),
                                 amf::AMF_MEMORY_CPU_ACCESS(amf::AMF_MEMORY_CPU_DEFAULT), &surf);
    }
    if (r != AMF_OK) r = ctx_->AllocSurface(amf::AMF_MEMORY_HOST, amf::AMF_SURFACE_GRAY32, amf_int32(m.cols), amf_int32(m.rows), &surf);
    if (r != AMF_OK || !surf || !surf->GetPlaneAt(0)) return Status::Error("encode_failed", amfError("allocating the ROI map", r));
    amf::AMFPlane* plane = surf->GetPlaneAt(0);
    auto* base = static_cast<uint8_t*>(plane->GetNative());
    const amf_int32 pitch = plane->GetHPitch();
    if (pitch <= 0 || !writeRoiPlane(m, base, size_t(pitch))) return Status::Error("encode_failed", "the ROI map surface has no usable memory");
    out = surf;
    return Status::Ok();
}

Status AmfEncoder::buildRoi(const std::vector<RoiRect>& rects) {
    roiMap_ = nullptr;
    if (rects.empty()) return Status::Ok();
    Status s = roiSurface(roiImportanceMap(width_, height_, P_->roiBlock, rects), roiMap_);
    if (s.ok) roiEverSet_ = true;
    return s;
}

Status AmfEncoder::submitFailed(AMF_RESULT r, const char* what) {
    Status s;
    if (d3d::deviceRemoved(device_.Get(), amfError(std::string("amf: ") + what, r), s)) return s;
    if (++submitErrors_ >= kMaxErrors) {
        return Status::Error("encode_failed", amfError(std::string(what) + " (" + std::to_string(submitErrors_) + " times in a row)", r), true);
    }
    return Status::Error("encode_failed", amfError(what, r));
}

Status AmfEncoder::submit(const EncoderFrame& frame, const SubmitInfo& info) {
    if (stopped_ || !enc_) return Status::Ok();
    // Requests from the control thread.
    bool idr = false, rateDirty = false, roiDirty = false;
    RateParams rate;
    std::vector<RoiRect> rects;
    {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idr = idrPending_;
        if ((rateDirty = rateDirty_)) rate = pendingRate_, rateDirty_ = false;
        if ((roiDirty = roiDirty_)) rects = roiRects_, roiDirty_ = false;
    }
    if (rateDirty) {
        bool flushed = false;
        const int fpsBefore = fps_;
        Status s = applyRate(rate, flushed);
        if (!flushed && fps_ != fpsBefore) fpsChangeFrame_ = info.frameId;  // receive() checks it is not followed by a key frame
        if (flushed) {
            std::lock_guard<std::mutex> lock(ctlMu_);
            idrPending_ = idr = true;  // the first frame after ReInit
        }
        if (!s.ok) return s;  // only a failed Flush / ReInit (fatal)
    }
    if (roiDirty) {
        // A map that cannot be built leaves the frame without one; it is still encoded.
        Status s = buildRoi(rects);
        if (!s.ok) logf(LogLevel::Warn, "amf: %s", s.text.c_str());
    }

    // The surface.
    amf::AMFSurfacePtr surf;
    amf::AMFSurface* native = nullptr;
    AMF_RESULT r = AMF_OK;
    if (zeroCopy_) {
        auto* cap = static_cast<amf::AMFSurface*>(frame.captured ? frame.captured->amfSurface : nullptr);
        amf::AMFPlane* plane = cap ? cap->GetPlaneAt(0) : nullptr;
        if (!plane) return Status::Error("encode_failed", "zero-copy frame without an AMF surface");
        if (uint32_t(plane->GetWidth()) != codedW_ || uint32_t(plane->GetHeight()) != codedH_ || frame.captured->rotation != 0) {
            return Status::Error("capture_failed",
                                 "the AMD Direct Capture source changed to " + std::to_string(plane->GetWidth()) + "x" +
                                     std::to_string(plane->GetHeight()) + " (rotation " + std::to_string(frame.captured->rotation) +
                                     "): zero-copy encoding cannot scale or rotate; restart the helper (or start with zeroCopy false)",
                                 true);
        }
        // The format, every frame: the encoder was initialized with the one
        // the capture reported at its start, and the capture re-initializes
        // itself after a mode change or an HDR switch. An sRGB-typed texture
        // (a game's swap chain) is BGRA to AMF but not to the encoder's input
        // (Streaming SDK VideoEncodeEngine::IsFormatSupported).
        DXGI_FORMAT texFormat = DXGI_FORMAT_UNKNOWN;
        const amf::AMF_SURFACE_FORMAT capFormat = cap->GetFormat();
        if (capFormat != inputFormat_ || !zeroCopyTexture(plane, texFormat)) {
            return Status::Error("capture_failed",
                                 "the AMD Direct Capture surfaces are now AMF format " + std::to_string(int(capFormat)) + ", DXGI format " +
                                     std::to_string(int(texFormat)) + " (the encoder was initialized with AMF format " +
                                     std::to_string(int(inputFormat_)) +
                                     "): zero-copy encoding takes 8-bit BGRA/RGBA only (no sRGB, 10-bit or FP16); restart the "
                                     "helper, with zeroCopy false if this happens again",
                                 true);
        }
        if (frame.captured->amfDcc) {
            // DCC-compressed surfaces cannot go to the encoder as they are
            // (AMF_Display_Capture_API.md): encode a copy.
            warnOnce("dcc", "AMD Direct Capture surfaces are DCC compressed: each one is copied before encoding");
            amf::AMFDataPtr copy;
            r = cap->Duplicate(amf::AMF_MEMORY_DX11, &copy);
            surf = amf::AMFSurfacePtr(copy);
            if (r != AMF_OK || !surf) return submitFailed(r, "copying a DCC capture surface");
        } else {
            if (frame.captured->repeat) {
                // An idle repeat re-submits the same surface object; while its
                // last submission is still in the encoder, changing its
                // per-frame properties would change that one too. Nothing new
                // to send anyway: skip it.
                std::lock_guard<std::mutex> lock(flightMu_);
                for (const InFlight& f : flight_) {
                    if (f.nativeSurface == cap) return Status::Error("encoder_busy", "the repeated capture surface is still in the encoder");
                }
            }
            surf = cap;
            native = cap;
        }
    } else {
        if (!frame.nv12) return Status::Error("encode_failed", "no NV12 frame to encode");
        r = ctx_->CreateSurfaceFromDX11Native(frame.nv12, &surf, this);
        if (r != AMF_OK || !surf) return submitFailed(r, "CreateSurfaceFromDX11Native");
        std::lock_guard<std::mutex> lock(holdsMu_);
        holds_[surf.GetPtr()] = Hold{frame.hold, ComPtr<ID3D11Texture2D>(frame.nv12)};
    }
    surf->SetPts(toPts(info.captureQpc));
    surf->SetDuration(AMF_SECOND / std::max(1, fps_));
    surf->SetProperty(kFrameIdProp, amf_int64(info.frameId));

    // Per-frame properties, every one set explicitly: a zero-copy surface can
    // be submitted again (an idle repeat) and keeps what it had.
    const int64_t now = qpcNow();
    const LtrTracker::Plan plan = ltr_.plan(info.frameId, now, idr);
    PropSetter fs(surf);
    fs.setInt(P_->forcePictureType, plan.idr ? P_->pictureIdr : P_->pictureNone);
    // Parameter sets with every forced IDR (FFmpeg amfenc.c forced_idr: INSERT_SPS
    // + INSERT_PPS / INSERT_HEADER / FORCE_INSERT_SEQUENCE_HEADER).
    fs.setBool(P_->insertHeader, plan.idr);
    fs.setBool(P_->insertSps, plan.idr);
    fs.setBool(P_->insertPps, plan.idr);
    if (start_.ltrSlots > 0) {
        fs.setInt(P_->markLtr, plan.markSlot);
        fs.setInt(P_->forceLtrRef, amf_int64(plan.refMask));
    }
    if (roiMap_) {
        fs.set(P_->roiData, static_cast<amf::AMFInterface*>(roiMap_.GetPtr()));
    } else if (roiEverSet_ && native) {
        // ROI cleared: a zero-copy surface may still carry an old map from an
        // earlier submission; a uniform map has no effect.
        if (!roiUniform_) roiSurface(roiImportanceMap(width_, height_, P_->roiBlock, {}), roiUniform_);
        if (roiUniform_) fs.set(P_->roiData, static_cast<amf::AMFInterface*>(roiUniform_.GetPtr()));
    }
    for (const std::string& w : fs.warnings) warnOnce("frame:" + w, "per-frame property not accepted: " + w);

    InFlight f;
    f.frameId = info.frameId;
    f.info = info;
    f.plan = plan;
    f.gen = gen_;
    f.nativeSurface = native;
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        flight_.push_back(f);  // before SubmitInput: the output can come back at once
    }
    flightCv_.notify_all();  // the output thread waits for frames in flight (receive)
    for (int attempt = 0;; ++attempt) {
        {
            std::shared_lock<std::shared_mutex> shared(componentMu_);
            r = enc_->SubmitInput(surf);
        }
        if (r != AMF_INPUT_FULL || attempt >= kSubmitRetries || stopped_) break;
        Sleep(1);  // "input queue is full: wait, poll and submit again" (AMF SimpleEncoder sample)
    }
    if (r != AMF_OK && r != AMF_NEED_MORE_INPUT) {
        {
            std::lock_guard<std::mutex> lock(flightMu_);
            for (auto it = flight_.begin(); it != flight_.end(); ++it) {
                if (it->frameId == info.frameId) {
                    flight_.erase(it);
                    break;
                }
            }
        }
        if (r == AMF_INPUT_FULL) return Status::Error("encoder_busy", "AMF input queue full");
        return submitFailed(r, "SubmitInput");
    }
    submitErrors_ = 0;
    ltr_.submitted(info.frameId, plan, now);
    if (plan.idr) {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idrPending_ = false;
    }
    return Status::Ok();
}

Next AmfEncoder::receive(EncodedFrame& out, int timeoutMs, Status& err) {
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq_ / 1000;
    amf::AMFDataPtr data;
    for (;;) {
        if (stopped_ || !enc_) return Next::Stopped;
        {
            // Nothing in the encoder: wait for a submission instead of querying.
            // QueryOutput answers AMF_REPEAT when "the output queue is empty"
            // (AMF API reference), and nothing says QUERY_TIMEOUT makes it wait
            // then: FFmpeg's amfenc.c only waits in QueryOutput while frames are
            // queued, the AMF samples' PollingThread sleeps 1 ms after every
            // empty query. Idle (before the first frame, a static desktop) this
            // thread sleeps.
            std::unique_lock<std::mutex> lock(flightMu_);
            if (flight_.empty()) {
                const int64_t now = qpcNow();
                if (now >= deadline) return Next::Timeout;
                flightCv_.wait_for(lock, std::chrono::microseconds((deadline - now) * 1000000 / freq_),
                                   [this] { return !flight_.empty() || stopped_; });
                if (stopped_) return Next::Stopped;
                if (flight_.empty()) return Next::Timeout;
            }
        }
        AMF_RESULT r;
        const int64_t before = qpcNow();
        {
            std::shared_lock<std::shared_mutex> shared(componentMu_);
            r = enc_->QueryOutput(&data);
        }
        if (r == AMF_OK && data) {
            if (start_.sliceOutput <= 0) break;
            // Sub-frame output: one slice / tile per buffer, put back
            // together here (codec/slices.hpp); query on until the last part.
            amf::AMFBufferPtr part(data);
            data = nullptr;
            if (!part || !part->GetNative()) {
                err = Status::Error("encode_failed", "AMF output is not a buffer");
                return Next::Error;
            }
            amf_int64 type = P_->bufferFrame, id = -1;
            getProp(part.GetPtr(), P_->outputBufferType, type);
            getProp(part.GetPtr(), kFrameIdProp, id);
            const SliceAssembler::Part kind = type == P_->bufferPart   ? SliceAssembler::Part::Slice
                                              : type == P_->bufferLast ? SliceAssembler::Part::Last
                                                                       : SliceAssembler::Part::Frame;
            if (resetSlices_.exchange(false) && slices_.collecting()) {
                logf(LogLevel::Debug, "amf: dropping the parts of a frame the flush discarded");
                slices_.reset();
                parts_.clear();
            }
            parts_.push_back(part);
            const SliceAssembler::Result res =
                slices_.add(kind, id, static_cast<const uint8_t*>(part->GetNative()), part->GetSize(), qpcNow());
            if (res.droppedParts > 0) {
                parts_.erase(parts_.begin(), parts_.begin() + std::min<std::ptrdiff_t>(res.droppedParts, std::ptrdiff_t(parts_.size()) - 1));
                logf(LogLevel::Warn, "amf: %d %s of an unfinished frame dropped (no last part came)", res.droppedParts,
                     codec_ == Codec::Av1 ? "tiles" : "slices");
            }
            if (res.complete) break;
            continue;  // more parts of this frame: query again at once
        }
        const int64_t now = qpcNow();
        if (r == AMF_REPEAT || r == AMF_OK || r == AMF_NEED_MORE_INPUT) {
            queryErrors_ = 0;
            if (now >= deadline) return Next::Timeout;
            // A call that waited out QUERY_TIMEOUT (at least half of it) goes
            // again at once; one that came back sooner (no timeout in effect,
            // or an early return) is followed by the 1 ms poll sleep (FFmpeg
            // amfenc.c av_usleep(1000); the AMF samples amf_sleep(1)).
            const bool waited = queryTimeoutMs_ > 0 && now - before >= int64_t(queryTimeoutMs_) * freq_ / 2000;
            if (!waited && !timer_.sleepUntil(std::min(deadline, now + freq_ / 1000), stopEvent_)) return Next::Stopped;
            continue;
        }
        if (r == AMF_EOF) {
            err = Status::Error("encode_failed", "the AMF encoder ended its stream (AMF_EOF without a Drain)", true);
            return Next::Error;
        }
        if (d3d::deviceRemoved(device_.Get(), amfError("amf: QueryOutput", r), err)) return Next::Error;
        if (++queryErrors_ >= kMaxErrors) {
            err = Status::Error("encode_failed", amfError("QueryOutput (" + std::to_string(queryErrors_) + " times in a row)", r), true);
            return Next::Error;
        }
        warnOnce("query", amfError("QueryOutput", r));
        if (!timer_.sleepUntil(now + freq_ / 1000, stopEvent_)) return Next::Stopped;
    }
    queryErrors_ = 0;
    // The frame's buffers: one, or its slices / tiles (sub-frame output). A
    // frame property is taken from the first part that has it.
    const bool sliced = start_.sliceOutput > 0;
    if (!sliced) {
        amf::AMFBufferPtr buf(data);
        if (!buf || !buf->GetNative()) {
            err = Status::Error("encode_failed", "AMF output is not a buffer");
            return Next::Error;
        }
        parts_.assign(1, buf);
    }
    std::vector<amf::AMFBufferPtr> parts = std::move(parts_);
    parts_.clear();
    const auto prop = [&](const wchar_t* name, amf_int64& v) {
        for (amf::AMFBufferPtr& b : parts) {
            if (getProp(b.GetPtr(), name, v)) return true;
        }
        return false;
    };
    amf_int64 id = -1;
    const bool haveId = prop(kFrameIdProp, id);
    InFlight f;
    bool found = false;
    {
        std::lock_guard<std::mutex> lock(flightMu_);
        // In order: entries before this one never came out (flushed).
        while (!flight_.empty() && (!haveId || flight_.front().frameId <= uint64_t(id))) {
            const bool match = !haveId || flight_.front().frameId == uint64_t(id);
            if (match) {
                f = flight_.front();
                found = true;
            }
            flight_.pop_front();
            if (match) break;
        }
    }
    flightCv_.notify_all();
    if (!found) {
        warnOnce("unmatched", "an AMF output did not match a submitted frame");
        f.frameId = haveId ? uint64_t(id) : 0;
        f.gen = gen_;
    }
    out = EncodedFrame{};
    if (sliced) {
        out.data = slices_.frame().data();
        out.size = slices_.frame().size();
        out.slices = slices_.parts();
        out.firstSliceQpc = slices_.firstQpc();
    } else {
        out.data = static_cast<const uint8_t*>(parts.front()->GetNative());
        out.size = parts.front()->GetSize();
    }
    amf_int64 type = -1, marked = -1, refMask = 0, layer = -1;
    prop(P_->outputType, type);
    prop(P_->outputMarkedLtr, marked);
    const bool layerProp = prop(P_->outputTemporalLayer, layer);
    if (!prop(P_->outputRefLtr, refMask) && f.plan.recovery) {
        // No referenced-LTR bitfield on the output: trust the request.
        warnOnce("refmask", "the encoder reports no OUTPUT_REFERENCED_LTR_INDEX_BITFIELD: recovery frames are not verified");
        refMask = amf_int64(f.plan.refMask);
    }
    LtrTracker::Output o;
    o.key = type == P_->outKey;
    o.intra = o.key || type == P_->outIntra;
    o.markedSlot = int(marked);
    o.refMask = uint32_t(refMask);
    // An AV1 switch frame clears the LTR slots like a key frame, but it is not
    // a decoder entry point: not flagged KEY in the ring.
    o.clearsSlots = P_->outSwitch >= 0 && type == P_->outSwitch;
    if (o.clearsSlots) warnOnce("switch", "the AV1 encoder made a switch frame (its LTR slots are cleared) although switch frames are off");
    // Temporal SVC: the layer (OUTPUT_TEMPORAL_LAYER; AV1 has none, its OBU
    // extension says it) and whether any later frame can reference the frame.
    bool discardable = false;
    if (svcLayers_ > 1) {
        const LayerInfo li = layerInfo(codec_, out.data, out.size, svcLayers_ - 1);
        if (!layerProp) {
            layer = li.temporalId;
        } else if (li.temporalId >= 0 && li.temporalId != layer) {
            warnOnce("layer", "OUTPUT_TEMPORAL_LAYER " + std::to_string(layer) + " but the bitstream says temporal id " +
                                  std::to_string(li.temporalId) + " (frame " + std::to_string(f.frameId) + "): using the property");
        }
        if (layer < 0) warnOnce("nolayer", "SVC frames carry no temporal layer (no property, no temporal id): all reported as layer 0");
        // started.svcLayers is read from the property store, which echoes what
        // was set even if the encoder never applied it: the frames tell.
        baseRun_ = layer == 0 && !o.key ? baseRun_ + 1 : 0;
        if (baseRun_ == 8) {
            warnOnce("nosvc", "svcLayers " + std::to_string(svcLayers_) +
                                  ": 8 frames in a row came out in layer 0, the encoder runs no temporal layers");
        }
        discardable = isDiscardable(o.key, li, svcLayers_, uint32_t(std::max<amf_int64>(0, layer)));
    }
    o.temporalLayer = int(layer);
    if (o.key && found && !f.plan.idr) {
        // Not asked for: right after a FRAMERATE change it means the change
        // was not seamless (the Phase 5 VERIFY item); otherwise the encoder
        // decided on its own (scene change, an internal reset).
        const uint64_t fc = fpsChangeFrame_.load();
        if (fc && f.frameId >= fc && f.frameId < fc + 4) {
            logf(LogLevel::Warn, "amf: the frame-rate change at frame %llu made a key frame (frame %llu): FRAMERATE is not seamless here "
                                 "(docs/VENDOR_NOTES.md Phase 5)",
                 static_cast<unsigned long long>(fc), static_cast<unsigned long long>(f.frameId));
        } else {
            warnOnce("unplannedkey", "the encoder made a key frame nobody asked for (frame " + std::to_string(f.frameId) + ")");
        }
    }
    const bool recoveryOk = ltr_.output(f.frameId, o, f.plan, qpcNow());
    if (f.plan.recovery && !recoveryOk) {
        // The encoder did not code the recovery frame from its LTR (or coded it
        // in an SVC enhancement layer): it still depends on lost frames. Not
        // flagged RECOVERY; the next frame is an IDR.
        logf(LogLevel::Warn, "amf: recovery frame %llu did not reference LTR slot mask 0x%x as a base-layer frame (referenced 0x%llx, type %lld, "
                             "layer %lld): forcing an IDR",
             static_cast<unsigned long long>(f.frameId), f.plan.refMask, static_cast<unsigned long long>(refMask),
             static_cast<long long>(type), static_cast<long long>(layer));
        forceIdr();
    }
    out.info = f.info;
    out.info.frameId = f.frameId;
    out.outputQpc = qpcNow();
    out.gen = f.gen;
    out.key = o.key;
    out.recovery = f.plan.recovery && recoveryOk;
    out.refFloor = f.plan.refFloor;
    out.ltrSlot = int32_t(marked);
    out.temporalLayer = uint32_t(std::max<amf_int64>(0, layer));
    out.discardable = discardable;
    out.refLtrMask = uint32_t(refMask);
    out.width = codedW_;
    out.height = codedH_;
    if (out.key && !hasParameterSets(codec_, out.data, out.size)) {
        // A key frame must be a decoder entry point (ring flag KEY): add the
        // encoder's parameter sets if it left them out.
        std::lock_guard<std::mutex> lock(extraMu_);
        patched_ = withParameterSets(codec_, out.data, out.size, extradata_.data(), extradata_.size());
        if (!patched_.empty()) {
            warnOnce("headers", "key frames come without parameter sets: inserting the encoder's extradata");
            out.data = patched_.data();
            out.size = patched_.size();
        } else {
            warnOnce("headers-missing", "a key frame has no parameter sets and the extradata cannot supply them");
        }
    }
    if (!sliced) {
        amf::AMFBuffer* raw = parts.front().GetPtr();
        raw->Acquire();  // released by releaseOutput, after the ring copy
        out.token = raw;
    }  // sliced: out.data is slices_'s copy, valid until the next receive()
    return Next::Frame;
}

void AmfEncoder::releaseOutput(EncodedFrame& f) {
    if (f.token) static_cast<amf::AMFBuffer*>(f.token)->Release();
    f.token = nullptr;
    f.data = nullptr;
}

Status AmfEncoder::forceIdr() {
    std::lock_guard<std::mutex> lock(ctlMu_);
    idrPending_ = true;
    return Status::Ok();
}

Status AmfEncoder::recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) {
    if (ltr_.recover(lostFromFrameId, ackedLtrFrameId)) {
        logf(LogLevel::Debug, "amf: recovering from frame %llu with an LTR", static_cast<unsigned long long>(lostFromFrameId));
        return Status::Ok();
    }
    logf(LogLevel::Debug, "amf: no acknowledged LTR before frame %llu: IDR", static_cast<unsigned long long>(lostFromFrameId));
    return forceIdr();
}

Status AmfEncoder::setRate(const RateParams& r) {
    std::lock_guard<std::mutex> lock(ctlMu_);
    if (r.kbps > 0) pendingRate_.kbps = r.kbps;
    if (r.vbvFrames > 0) pendingRate_.vbvFrames = r.vbvFrames;
    if (r.fps > 0) pendingRate_.fps = r.fps;
    rateDirty_ = true;
    return Status::Ok();
}

Status AmfEncoder::setRoi(const std::vector<RoiRect>& rects) {
    if (det_.caps.roi == "none") return Status::Error("unsupported", "this encoder has no ROI map (CAP_ROI false)");
    std::lock_guard<std::mutex> lock(ctlMu_);
    roiRects_ = rects;
    roiDirty_ = true;
    return Status::Ok();
}

Status AmfEncoder::ack(uint64_t frameId) {
    ltr_.ack(frameId);
    return Status::Ok();
}

void AmfEncoder::shutdown() {
    stopped_ = true;
    if (stopEvent_) SetEvent(stopEvent_);
    {
        // A waiter between its predicate check and the wait must not miss this.
        std::lock_guard<std::mutex> lock(flightMu_);
    }
    flightCv_.notify_all();
}

}  // namespace

Probe probeAmf() {
    const AmfProbe& p = amfProbe();
    if (!p.ok) return {false, p.reason};
    std::string codecs;
    for (const auto& [c, d] : p.codecs) {
        if (d.available) codecs += std::string(codecs.empty() ? "" : ", ") + codecName(c);
    }
    return {true, "AMF runtime " + amfRuntime().versionText + " on " + p.adapter.name + ": " + codecs};
}

std::unique_ptr<Backend> createAmfBackend(Status& err) {
    const AmfProbe& p = amfProbe();
    if (!p.ok) {
        err = Status::Error("unavailable", p.reason);
        return nullptr;
    }
    return std::make_unique<AmfEncoder>();
}

}  // namespace recon
