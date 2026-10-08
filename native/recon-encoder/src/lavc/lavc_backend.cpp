// libavcodec fallback encoder backend (GUIDE 3.8): for GPUs without an AMF or
// NVENC backend, Intel Quick Sync Video through FFmpeg's h264_qsv, hevc_qsv and
// av1_qsv (oneVPL / libvpl inside avcodec-62.dll), loaded at run time
// (lavc/lavc_runtime.hpp: FFmpeg's shared DLLs, never required).
//
// Input. With a GPU capture the encoder takes the colour converter's NV12
// textures without a copy: a D3D11VA device context wraps the capture's
// ID3D11Device (its lock is the device's ID3D10Multithread critical section,
// the one the converter holds), a QSV device is derived from it (so the QSV
// session runs on the capture's adapter), and a QSV frames context with a
// dynamic surface pool is derived from a D3D11 frames context with a dynamic
// pool (initial_pool_size 0, FFmpeg 7.0+ with oneVPL 2.x): av_hwframe_map then
// turns any D3D11 texture into a QSV surface whose MemId names the texture
// itself (hwcontext_qsv.c qsv_dynamic_pool_map_to). The texture comes 16x16
// aligned (the QSV surface size; the converter repeats the picture's edge into
// the padding) and stays reserved (the converter's hold) until libavcodec drops
// the mapped frame, which qsvenc does once the encoder has unlocked the surface.
// If the encoder does not open that way (an older runtime), or start's zeroCopy
// is false, the frames are read back into system memory instead (a staging
// texture; qsvenc uploads them): started.zeroCopy says which. Without a GPU
// capture (the synthetic source) the backend draws a moving test pattern.
// System-memory frames come in the layout qsvenc passes on as it is (qsvenc.c
// submit_frame copies any other frame into a buffer of its own first): one
// buffer with the CbCr rows right after the luma rows, a pitch that is a
// multiple of 32 (qsvenc's width alignment: 16, 32 for HEVC on runtimes before
// API 1.19) and the height of the encoder's surfaces (FrameInfo.Height: 16
// aligned for H.264, 32 for HEVC / AV1), which AVFrame.height then carries
// (libavcodec does not compare it with the codec context's; the encoder crops
// to the picture). The converter pads the picture to that size (its edge
// repeated, as qsvenc's copy would).
//
// Encoder settings (as Sunshine's quicksync encoder, video.cpp): async_depth 1,
// low_delay_brc 1, look-ahead off, no B frames, forced_idr 1, low_power 1
// (VDENC; retried with 0, which older GPUs need), recovery point and picture
// timing SEI off, the largest GOP QSV takes (GopPicSize 65535: about 18 minutes
// at 60 fps; key frames come on demand), VBR with the peak at the target for
// rc cbr (low_delay_brc works in VBR; Sunshine's CBR_WITH_VBR) and no VBV
// buffer size (NO_RC_BUF_LIMIT), BT.709 limited range in the VUI. A forced IDR
// is AVFrame.pict_type = AV_PICTURE_TYPE_I with AV_FRAME_FLAG_KEY, which qsvenc
// turns into MFX_FRAMETYPE_I | MFX_FRAMETYPE_REF | MFX_FRAMETYPE_IDR.
//
// Live rate (FFmpeg 8.1 qsvenc.c update_parameters): a changed bit_rate,
// rc_max_rate, rc_buffer_size or framerate on the open encoder is noticed on
// the next frame; qsvenc drains the encoder and calls MFXVideoENCODE_Reset with
// the new values. FFmpeg passes no mfxExtEncoderResetOption, so whether the
// runtime starts a new sequence (an IDR) is the runtime's choice. Caps
// liveBitrate / liveFps are therefore "flush" (assumed): the frame that
// carries the new rate is a forced IDR and a new gen; start's liveBitrate
// "seamless" leaves the IDR out (what the step 3.6 qualification measures:
// the stats' key flag shows whether the runtime made one anyway).
// Recovery: none (an IDR); no LTR, SVC, ROI, intra refresh or HDR here.
//
// Threads: submit() (capture thread) maps / reads back the frame and queues it
// (one frame at most: "encoder_busy" otherwise); the backend's encoder thread
// applies forced IDRs and rate changes, calls avcodec_send_frame and
// avcodec_receive_packet (the only thread that touches the codec context;
// async_depth 1 makes the encode synchronous) and queues packets for
// receive() (output thread).
//
// --lavc-test-encoder=NAME (test only): the same backend drives a software
// encoder (libx264 from a GPL shared build) on system-memory frames, so the
// dynamic loading, frame submission, forced IDRs, rate changes and the output
// path run end to end under Wine (docs/VENDOR_NOTES.md 3.8).
#include <d3d10.h>  // ID3D10Multithread

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
#include <thread>

extern "C" {
#include <libavutil/hwcontext_d3d11va.h>
}

#include "codec/bitstream.hpp"
#include "codec/hdr.hpp"
#include "d3d/device.hpp"
#include "lavc/lavc_runtime.hpp"
#include "probes.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

constexpr int kGopFrames = 65535;  // QSV GopPicSize is 16-bit: the longest GOP it takes
constexpr int kMaxErrors = 10;     // consecutive failures before giving up (fatal)
constexpr size_t kMaxQueued = 1;   // frames waiting for the encoder thread
constexpr uint32_t kQsvAlign = 16; // QSV surfaces are 16x16 aligned (hwcontext_qsv.c qsv_init_surface)
constexpr uint32_t kSysPitchAlign = 32;  // system-memory frames: qsvenc's widest width alignment
constexpr int kProbeWidth = 1280, kProbeHeight = 720, kProbeFps = 60, kProbeKbps = 8000;
constexpr int kTestProbeWidth = 320, kTestProbeHeight = 180;

const char* qsvEncoder(Codec c) {
    switch (c) {
    case Codec::H264: return "h264_qsv";
    case Codec::Hevc: return "hevc_qsv";
    case Codec::Av1: return "av1_qsv";
    }
    return "";
}

bool codecFromId(AVCodecID id, Codec& out) {
    switch (id) {
    case AV_CODEC_ID_H264: out = Codec::H264; return true;
    case AV_CODEC_ID_HEVC: out = Codec::Hevc; return true;
    case AV_CODEC_ID_AV1: out = Codec::Av1; return true;
    default: return false;
    }
}

const char* qsvPreset(const std::string& quality) {
    if (quality == "quality") return "slow";
    if (quality == "balanced") return "medium";
    return "veryfast";
}

// One codec as the probe found it.
struct CodecDetails {
    bool available = false;
    std::string reason;
    CodecCaps caps;
    std::string encoder;  // libavcodec encoder name
    bool qsv = true;      // false: a --lavc-test-encoder software encoder
    bool lowPower = true; // QSV opened with low_power 1
};

struct LavcProbe {
    bool ok = false;
    std::string reason;
    AdapterInfo adapter;  // the Intel adapter (QSV), the primary adapter (test encoders)
    std::map<Codec, CodecDetails> codecs;
    bool test = false;
};

// Caps of a lavc codec. The libavcodec API has no capability query, so the
// sizes are the documented QSV limits (H.264 4096x4096, HEVC / AV1 8192x8192
// since Ice Lake / Arc) and the live-rate behaviour is what qsvenc does
// (drain + MFXVideoENCODE_Reset: "flush" until step 3.6 measures it).
CodecCaps lavcCaps(Codec c) {
    CodecCaps cc;
    cc.maxW = cc.maxH = c == Codec::H264 ? 4096 : 8192;
    cc.forceIdr = true;
    cc.recovery = "none";
    cc.liveBitrate = "flush";
    cc.liveFps = "flush";
    cc.assumed = {"maxW", "maxH", "liveBitrate", "liveFps"};
    return cc;
}

// --- Contexts --------------------------------------------------------------------------

void lockDevice(void* ctx) { static_cast<ID3D10Multithread*>(ctx)->Enter(); }
void unlockDevice(void* ctx) { static_cast<ID3D10Multithread*>(ctx)->Leave(); }

// A D3D11VA device context on `device` and a QSV device derived from it (the
// QSV session on the same adapter and device). FFmpeg's own D3D11 calls take
// the device's critical section (mt, may be null: FFmpeg's default mutex).
Status makeQsvDevice(const LavcRuntime& rt, ID3D11Device* device, ID3D10Multithread* mt, AVBufferRef*& d3dOut, AVBufferRef*& qsvOut) {
    d3dOut = rt.av_hwdevice_ctx_alloc(AV_HWDEVICE_TYPE_D3D11VA);
    if (!d3dOut) return Status::Error("init_failed", "lavc: av_hwdevice_ctx_alloc(d3d11va) failed");
    auto* hw = static_cast<AVD3D11VADeviceContext*>(reinterpret_cast<AVHWDeviceContext*>(d3dOut->data)->hwctx);
    device->AddRef();  // released by the device context
    hw->device = device;
    if (mt) {
        hw->lock = lockDevice;
        hw->unlock = unlockDevice;
        hw->lock_ctx = mt;
    }
    int r = rt.av_hwdevice_ctx_init(d3dOut);
    if (r < 0) {
        rt.av_buffer_unref(&d3dOut);
        return Status::Error("init_failed", "lavc: av_hwdevice_ctx_init(d3d11va): " + rt.errorText(r));
    }
    r = rt.av_hwdevice_ctx_create_derived(&qsvOut, AV_HWDEVICE_TYPE_QSV, d3dOut, 0);
    if (r < 0) {
        rt.av_buffer_unref(&d3dOut);
        return Status::Error("init_failed", "lavc: no QSV session on this D3D11 device (av_hwdevice_ctx_create_derived): " +
                                                rt.errorText(r));
    }
    return Status::Ok();
}

// Rate-control fields of the codec context (at open and on setRate).
void setRateFields(AVCodecContext* ctx, bool qsv, bool cbr, int kbps, double vbvFrames, int fps) {
    const int64_t bps = int64_t(kbps) * 1000;
    if (qsv) {
        // Sunshine (CBR_WITH_VBR, NO_RC_BUF_LIMIT): low_delay_brc is a VBR
        // feature, so constant bitrate is VBR with its peak at the target
        // (bit_rate != rc_max_rate selects VBR in qsvenc select_rc_mode), and
        // the HRD buffer is the encoder's own.
        ctx->rc_max_rate = cbr ? bps : bps * 3 / 2;
        ctx->bit_rate = cbr ? bps - 1 : bps;
        (void)vbvFrames;
    } else {
        ctx->bit_rate = bps;
        ctx->rc_max_rate = cbr ? bps : bps * 3 / 2;
        ctx->rc_buffer_size = int(std::min<double>(double(bps) * vbvFrames / std::max(1, fps), 2e9));
    }
}

struct OpenSpec {
    std::string encoder;
    bool qsv = true;
    int width = 0, height = 0, fps = 60, kbps = 10000;
    double vbvFrames = 1.0;
    bool cbr = true;
    std::string quality = "speed";
    bool lowPower = true;
    AVBufferRef* hwDevice = nullptr;  // QSV device: system-memory frames encoded on that adapter
    AVBufferRef* hwFrames = nullptr;  // QSV frames: zero copy
};

// Opens the encoder; nullptr with the reason in err.
AVCodecContext* openEncoder(const LavcRuntime& rt, const OpenSpec& s, std::string& err) {
    const AVCodec* codec = rt.avcodec_find_encoder_by_name(s.encoder.c_str());
    if (!codec) {
        err = s.encoder + " is not in this FFmpeg build";
        return nullptr;
    }
    AVCodecContext* ctx = rt.avcodec_alloc_context3(codec);
    if (!ctx) {
        err = "avcodec_alloc_context3 failed";
        return nullptr;
    }
    ctx->width = s.width;
    ctx->height = s.height;
    ctx->time_base = AVRational{1, s.fps};
    ctx->framerate = AVRational{s.fps, 1};
    ctx->pix_fmt = s.hwFrames ? AV_PIX_FMT_QSV : AV_PIX_FMT_NV12;
    ctx->sw_pix_fmt = AV_PIX_FMT_NV12;
    ctx->gop_size = kGopFrames;
    ctx->max_b_frames = 0;
    // Parameter sets also as extradata (inserted into key frames that come
    // without them); closed GOPs; no reordering.
    ctx->flags |= AV_CODEC_FLAG_GLOBAL_HEADER | AV_CODEC_FLAG_CLOSED_GOP | AV_CODEC_FLAG_LOW_DELAY;
    // What the NV12 converter writes (d3d/convert.hpp): BT.709, limited
    // range, chroma co-sited left (chroma_sample_loc_type 0).
    ctx->color_range = AVCOL_RANGE_MPEG;
    ctx->color_primaries = AVCOL_PRI_BT709;
    ctx->color_trc = AVCOL_TRC_BT709;
    ctx->colorspace = AVCOL_SPC_BT709;
    ctx->chroma_sample_location = AVCHROMA_LOC_LEFT;
    setRateFields(ctx, s.qsv, s.cbr, s.kbps, s.vbvFrames, s.fps);
    if (s.hwFrames) ctx->hw_frames_ctx = rt.av_buffer_ref(s.hwFrames);
    else if (s.hwDevice) ctx->hw_device_ctx = rt.av_buffer_ref(s.hwDevice);

    AVDictionary* opts = nullptr;
    auto set = [&](const char* k, const char* v) { rt.av_dict_set(&opts, k, v, 0); };
    if (s.qsv) {
        set("async_depth", "1");
        set("forced_idr", "1");
        set("low_delay_brc", "1");
        set("low_power", s.lowPower ? "1" : "0");
        set("preset", qsvPreset(s.quality));
        set("look_ahead_depth", "0");
        set("adaptive_i", "0");  // no I frames on scene changes: key frames only on demand
        if (s.encoder == "h264_qsv") {
            set("look_ahead", "0");
            set("profile", "high");
            set("max_dec_frame_buffering", "1");
        }
        if (s.encoder == "h264_qsv" || s.encoder == "hevc_qsv") {
            set("recovery_point_sei", "0");
            set("pic_timing_sei", "0");
        }
    } else if (s.encoder == "libx264" || s.encoder == "libx265") {
        set("preset", "ultrafast");
        set("tune", "zerolatency");
        set("forced-idr", "1");
        set(s.encoder == "libx264" ? "x264-params" : "x265-params", "scenecut=0");
    }
    const int r = rt.avcodec_open2(ctx, codec, &opts);
    std::string unused;
    for (const AVDictionaryEntry* e = nullptr; (e = rt.av_dict_get(opts, "", e, AV_DICT_IGNORE_SUFFIX));) {
        unused += std::string(unused.empty() ? "" : ", ") + e->key;
    }
    rt.av_dict_free(&opts);
    if (r < 0) {
        err = s.encoder + ": avcodec_open2 " + std::to_string(s.width) + "x" + std::to_string(s.height) + ": " + rt.errorText(r);
        rt.avcodec_free_context(&ctx);
        return nullptr;
    }
    if (!unused.empty()) logf(LogLevel::Debug, "lavc: %s does not take %s", s.encoder.c_str(), unused.c_str());
    return ctx;
}

// Opens a QSV encoder with low_power 1, else 0 (Sunshine's fallback: "Some
// old/low-end Intel GPUs don't support low power encoding").
AVCodecContext* openQsv(const LavcRuntime& rt, OpenSpec& s, std::string& err) {
    s.lowPower = true;
    AVCodecContext* ctx = openEncoder(rt, s, err);
    if (ctx || !s.qsv) return ctx;
    std::string lowPowerErr = err;
    s.lowPower = false;
    ctx = openEncoder(rt, s, err);
    if (ctx) logf(LogLevel::Info, "lavc: %s without low_power (%s)", s.encoder.c_str(), lowPowerErr.c_str());
    else err = lowPowerErr + "; with low_power 0: " + err;
    return ctx;
}

// --- Probe ---------------------------------------------------------------------------

// The Intel adapter QSV runs on: adapter 0 if it is Intel, else the Intel
// adapter with the most video memory (an Arc card next to an iGPU).
bool findIntelAdapter(ComPtr<IDXGIAdapter1>& out, AdapterInfo& info) {
    ComPtr<IDXGIFactory1> factory;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf())))) return false;
    ComPtr<IDXGIAdapter1> a;
    SIZE_T best = 0;
    for (UINT i = 0; factory->EnumAdapters1(i, a.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++i) {
        DXGI_ADAPTER_DESC1 d{};
        if (FAILED(a->GetDesc1(&d)) || (d.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) || d.VendorId != 0x8086) continue;
        if (out && d.DedicatedVideoMemory <= best) continue;
        out = a;
        best = d.DedicatedVideoMemory;
        info = describeAdapter(d.VendorId, d.AdapterLuid, d.Description);
        if (i == 0) break;
    }
    return out != nullptr;
}

const char* kNoIntel = "no Intel adapter: the libavcodec backend encodes with Intel Quick Sync Video (h264_qsv, hevc_qsv, av1_qsv)";

LavcProbe runLavcProbe() {
    LavcProbe p;
    const LavcRuntime& rt = lavcRuntime();
    if (!rt.ok) {
        p.reason = rt.error;
        return p;
    }
    const int64_t t0 = qpcNow();
    const LavcOptions& opt = lavcOptions();
    if (!opt.testEncoders.empty()) {
        // Test encoders: each one's codec, opened once on system memory.
        p.test = true;
        p.adapter = primaryAdapter();
        for (const std::string& name : opt.testEncoders) {
            const AVCodec* codec = rt.avcodec_find_encoder_by_name(name.c_str());
            Codec c;
            if (!codec || !codecFromId(codec->id, c)) {
                p.reason += std::string(p.reason.empty() ? "" : "; ") + name +
                            (codec ? " does not encode h264, hevc or av1" : " is not in this FFmpeg build");
                continue;
            }
            CodecDetails d;
            d.encoder = name;
            d.qsv = false;
            d.caps = lavcCaps(c);
            OpenSpec s;
            s.encoder = name;
            s.qsv = false;
            s.width = kTestProbeWidth;
            s.height = kTestProbeHeight;
            s.kbps = 1000;
            std::string err;
            AVCodecContext* ctx = openEncoder(rt, s, err);
            d.available = ctx != nullptr;
            d.reason = err;
            if (ctx) rt.avcodec_free_context(&ctx);
            // Several encoders of one codec: the first that opens is the
            // codec's; the errors of the others are kept with it.
            auto it = p.codecs.find(c);
            if (it == p.codecs.end()) {
                p.codecs[c] = d;
            } else if (!it->second.available) {
                if (!d.available) d.reason = it->second.reason + "; " + d.reason;
                it->second = d;
            } else if (!d.available) {
                logf(LogLevel::Info, "lavc: %s (%s is used for %s)", err.c_str(), it->second.encoder.c_str(), codecName(c));
            }
            p.ok = p.ok || d.available;
        }
    } else {
        ComPtr<IDXGIAdapter1> adapter;
        if (!findIntelAdapter(adapter, p.adapter)) {
            p.reason = kNoIntel;
            return p;
        }
        d3d::Device dev;
        Status s = d3d::createDevice(adapter.Get(), dev);
        if (!s.ok) {
            p.reason = "D3D11 device for the QSV probe: " + s.text;
            return p;
        }
        ComPtr<ID3D10Multithread> mt;
        dev.device.As(&mt);
        AVBufferRef *d3dDev = nullptr, *qsvDev = nullptr;
        s = makeQsvDevice(rt, dev.device.Get(), mt.Get(), d3dDev, qsvDev);
        if (!s.ok) {
            p.reason = s.text + " (" + p.adapter.name + ")";
            return p;
        }
        // Each codec opened once on the adapter (system-memory input: whether
        // the runtime has the encoder; start opens it again on its frames).
        for (const Codec c : {Codec::H264, Codec::Hevc, Codec::Av1}) {
            CodecDetails d;
            d.encoder = qsvEncoder(c);
            d.caps = lavcCaps(c);
            OpenSpec o;
            o.encoder = d.encoder;
            o.width = kProbeWidth;
            o.height = kProbeHeight;
            o.fps = kProbeFps;
            o.kbps = kProbeKbps;
            o.hwDevice = qsvDev;
            std::string err;
            AVCodecContext* ctx = openQsv(rt, o, err);
            d.available = ctx != nullptr;
            d.lowPower = o.lowPower;
            d.reason = err;
            if (ctx) rt.avcodec_free_context(&ctx);
            p.codecs[c] = d;
            p.ok = p.ok || d.available;
        }
        rt.av_buffer_unref(&qsvDev);
        rt.av_buffer_unref(&d3dDev);
    }
    logf(LogLevel::Debug, "lavc probe: %lld ms", static_cast<long long>((qpcNow() - t0) * 1000 / qpcFrequency()));
    if (!p.ok) {
        // No backend, so no caps.unavailable.lavc-<codec> entries: each
        // encoder's error goes into unavailable.lavc (they start with its name).
        std::string why = p.reason;
        for (const auto& [codec, d] : p.codecs) {
            if (!d.reason.empty()) why += std::string(why.empty() ? "" : "; ") + d.reason;
        }
        p.reason = (why.empty() ? std::string("no usable encoder") : why) + " (" + rt.versionText + ")";
    }
    return p;
}

std::atomic<bool> g_probed{false};  // lavcProbe() has run

const LavcProbe& lavcProbe() {
    static const LavcProbe probe = [] {
        LavcProbe p = runLavcProbe();
        g_probed = true;
        return p;
    }();
    return probe;
}

Caps capsFrom(const LavcProbe& probe) {
    Caps c;
    c.backend = probe.ok ? "lavc" : "none";
    c.vendor = probe.adapter.found ? probe.adapter.vendor : "other";
    if (probe.adapter.found) {
        c.adapterLuid = probe.adapter.luid;
        c.adapterName = probe.adapter.name;
        c.hagsEnabled = probe.adapter.hags;
    }
    for (const auto& [codec, d] : probe.codecs) {
        if (d.available) c.codecs[codecName(codec)] = d.caps;
        else c.unavailable.emplace_back(std::string("lavc-") + codecName(codec), d.reason);
    }
    if (!probe.ok) c.unavailable.emplace_back("lavc", probe.reason);
    return c;
}

// --- Backend -------------------------------------------------------------------------

// The converter's texture while libavcodec references it (the mapped frame's
// source): its pool reservation and a COM reference.
struct TextureRef {
    AVD3D11FrameDescriptor desc{};
    std::shared_ptr<void> hold;
    ComPtr<ID3D11Texture2D> texture;
};

void freeTextureRef(void* opaque, uint8_t*) { delete static_cast<TextureRef*>(opaque); }

// An output packet (EncodedFrame::token) and, when the encoder left the
// parameter sets out of a key frame, the frame with them inserted.
struct OutPacket {
    AVPacket* packet = nullptr;
    std::vector<uint8_t> patched;
};

class LavcEncoder : public Backend {
public:
    explicit LavcEncoder(const LavcProbe& probe) : rt_(lavcRuntime()), probe_(probe) {}
    ~LavcEncoder() override { release(); }
    const char* name() const override { return "lavc"; }
    Caps caps() override { return capsFrom(probe_); }
    Status init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) override;
    void release() override;
    Status submit(const EncoderFrame& frame, const SubmitInfo& info) override;
    Next receive(EncodedFrame& out, int timeoutMs, Status& err) override;
    void releaseOutput(EncodedFrame& f) override;
    Status forceIdr() override;
    Status recover(uint64_t lostFromFrameId, std::optional<uint64_t> ackedLtrFrameId) override;
    Status setRate(const RateParams& r) override;
    Status setRoi(const std::vector<RoiRect>& rects) override;
    void shutdown() override;

private:
    enum class Mode { ZeroCopy, Readback, Pattern };
    struct Input {
        AVFrame* frame = nullptr;
        SubmitInfo info;
    };
    struct Pending {
        int64_t pts = 0;
        SubmitInfo info;
        uint32_t gen = 0;
        bool forcedIdr = false;
    };
    struct Output {
        EncodedFrame frame;
        Status err;  // !ok: an error for receive()
    };

    Status validate(const StartParams& p, const CodecDetails& d);
    Status open(const StartParams& p);
    void freeContexts();
    Status makeFrame(const EncoderFrame& ef, const SubmitInfo& info, AVFrame*& out);
    Status mapTexture(const EncoderFrame& ef, AVFrame*& out);
    Status readback(const EncoderFrame& ef, AVFrame* f);
    struct PlaneCopy {
        uint8_t* dst = nullptr;
        int dstPitch = 0;
        uint32_t rowBytes = 0, rows = 0;
        uint32_t rowsBefore = 0;  // rows of the mapped texture before this plane (NV12: the luma rows before the chroma)
    };
    Status readTexture(ID3D11Texture2D* tex, ComPtr<ID3D11Texture2D>& staging, const PlaneCopy* planes, int count);
    void encodeLoop();
    void encodeOne(Input& in);
    void deliver(AVPacket* pkt);
    void fail(const std::string& what, int err);
    void push(Output o);
    void warnOnce(const std::string& key, const std::string& text);

    const LavcRuntime& rt_;
    LavcProbe probe_;

    // Set by init(), constant while streaming.
    Codec codec_ = Codec::Hevc;
    CodecDetails det_;
    Mode mode_ = Mode::Pattern;
    uint32_t width_ = 0, height_ = 0;  // the picture
    uint32_t frameW_ = 0, frameH_ = 0;  // system-memory frames: pitch and height (the picture padded, above)
    bool cbr_ = true;
    bool flushMode_ = true;
    bool lowPower_ = false;
    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    ComPtr<ID3D10Multithread> mt_;
    AVBufferRef *d3dDevice_ = nullptr, *qsvDevice_ = nullptr, *d3dFrames_ = nullptr, *qsvFrames_ = nullptr;
    AVCodecContext* enc_ = nullptr;
    std::vector<uint8_t> extradata_;
    ComPtr<ID3D11Texture2D> stagingY_, stagingUv_;  // readback (capture thread)

    // Encoder thread state.
    std::thread thread_;
    int64_t pts_ = 0;
    uint32_t gen_ = 0;
    int errors_ = 0;
    std::deque<Pending> pending_;
    RateParams rate_;  // in effect

    std::mutex mu_;
    std::condition_variable inCv_, outCv_;
    std::deque<Input> inputs_;
    std::deque<Output> outputs_;
    bool stopped_ = false;

    std::mutex ctlMu_;
    bool idrPending_ = false;
    bool rateDirty_ = false;
    RateParams pendingRate_;

    std::mutex warnMu_;
    std::set<std::string> warned_;
};

void LavcEncoder::warnOnce(const std::string& key, const std::string& text) {
    {
        std::lock_guard<std::mutex> lock(warnMu_);
        if (!warned_.insert(key).second) return;
    }
    logf(LogLevel::Warn, "lavc: %s", text.c_str());
}

Status LavcEncoder::validate(const StartParams& p, const CodecDetails& d) {
    const CodecCaps& cc = d.caps;
    if (int(width_) > cc.maxW || int(height_) > cc.maxH || width_ < 16 || height_ < 16) {
        return Status::Error("unsupported", std::to_string(width_) + "x" + std::to_string(height_) + " is outside the " + d.encoder +
                                                " encoder's 16x16 .. " + std::to_string(cc.maxW) + "x" + std::to_string(cc.maxH));
    }
    if (p.ltrSlots > 0) {
        return Status::Error("unsupported", "ltrSlots " + std::to_string(p.ltrSlots) +
                                                ": the libavcodec backend has no long-term references (caps recovery none: a loss costs "
                                                "an IDR), start it with ltrSlots 0");
    }
    if (p.svcLayers > 1) return Status::Error("unsupported", "svcLayers " + std::to_string(p.svcLayers) + ": the encoder supports 1");
    if (p.intraRefreshFrames > 0) return Status::Error("unsupported", "intraRefreshFrames: no intra refresh in the libavcodec backend");
    if (p.hdr) return Status::Error("unsupported", "hdr: the libavcodec backend encodes 8-bit SDR only (caps hdr10 false)");
    if (p.sliceOutput > 0) return Status::Error("unsupported", "sliceOutput: no sub-frame output in the libavcodec backend");
    if (p.reencodeOversized > 0) return Status::Error("unsupported", "reencodeOversized: the libavcodec backend cannot re-encode (caps reencode false)");
    if (p.encoderInstance > 0) {
        return Status::Error("unsupported", "encoderInstance " + std::to_string(p.encoderInstance) +
                                                ": the libavcodec backend cannot pick a hardware engine (caps instanceSelect false)");
    }
    return Status::Ok();
}

Status LavcEncoder::init(const StartParams& p, const SourceInfo& src, InputSpec& in, Started& out) {
    release();
    if (!parseCodec(p.codec, codec_)) return Status::Error("unsupported", "unknown codec " + p.codec);
    const auto it = probe_.codecs.find(codec_);
    if (it == probe_.codecs.end() || !it->second.available) {
        return Status::Error("unsupported", "lavc: no " + p.codec + " encoder here" +
                                                (it == probe_.codecs.end() ? std::string() : ": " + it->second.reason));
    }
    det_ = it->second;
    width_ = uint32_t(p.width ? p.width : int(src.width)) & ~1u;
    height_ = uint32_t(p.height ? p.height : int(src.height)) & ~1u;
    if (Status s = validate(p, det_); !s.ok) return s;
    if (det_.qsv && src.device && src.adapter.found && src.adapter.vendor != "intel") {
        return Status::Error("unsupported", "lavc: Quick Sync Video encodes on an Intel adapter, the capture runs on " + src.adapter.name +
                                                " (choose an output of the Intel GPU, or another backend)");
    }
    flushMode_ = (p.liveBitrate.empty() ? det_.caps.liveBitrate : p.liveBitrate) == "flush";
    cbr_ = p.rc != "vbr";
    rate_ = RateParams{p.kbps, p.vbvFrames > 0 ? p.vbvFrames : 1.0, p.fps};
    pendingRate_ = RateParams{};
    rateDirty_ = idrPending_ = false;
    pts_ = 0;
    gen_ = 0;
    errors_ = 0;

    if (src.device) {
        device_ = src.device;
        device_->GetImmediateContext(context_.ReleaseAndGetAddressOf());
        device_.As(&mt_);
        mode_ = det_.qsv && p.zeroCopy ? Mode::ZeroCopy : Mode::Readback;
    } else {
        mode_ = Mode::Pattern;
    }
    Status s = open(p);
    if (!s.ok && mode_ == Mode::ZeroCopy) {
        logf(LogLevel::Warn, "lavc: %s cannot take the converter's textures (%s): reading the frames back into system memory instead",
             det_.encoder.c_str(), s.text.c_str());
        freeContexts();
        mode_ = Mode::Readback;
        s = open(p);
    }
    if (!s.ok) {
        freeContexts();
        device_.Reset();
        context_.Reset();
        mt_.Reset();
        return s;
    }
    extradata_.assign(enc_->extradata, enc_->extradata + std::max(0, enc_->extradata_size));
    frameW_ = alignUp(width_, kSysPitchAlign);
    frameH_ = alignUp(height_, codec_ == Codec::H264 ? 16u : 32u);

    in = InputSpec{};
    if (mode_ == Mode::ZeroCopy) {
        // The QSV surface size; the converter pads the picture into it.
        in.format = InputSpec::Format::Nv12;
        in.width = alignUp(width_, kQsvAlign);
        in.height = alignUp(height_, kQsvAlign);
        in.contentWidth = width_;
        in.contentHeight = height_;
    } else if (mode_ == Mode::Readback) {
        // Read into the system-memory frame as it is (padded, above).
        in.format = InputSpec::Format::Nv12;
        in.width = frameW_;
        in.height = frameH_;
        in.contentWidth = width_;
        in.contentHeight = height_;
        in.planarOk = true;  // read on the CPU: separate Y / CbCr planes do too (Wine)
    }

    out.backend = name();
    out.encoder = det_.encoder;
    out.codec = p.codec;
    out.width = int(width_);
    out.height = int(height_);
    out.fps = p.fps;
    out.kbps = p.kbps;
    out.liveBitrate = flushMode_ ? "flush" : "seamless";
    out.liveFps = out.liveBitrate;
    out.rateControl = det_.qsv ? (cbr_ ? "vbr_capped" : "vbr") : (cbr_ ? "cbr" : "vbr");
    out.usage = det_.qsv ? (lowPower_ ? "low_power" : "default") : "";
    out.preset = det_.qsv ? qsvPreset(p.quality) : det_.encoder == "libx264" || det_.encoder == "libx265" ? "ultrafast" : "";
    out.zeroCopy = mode_ == Mode::ZeroCopy;
    out.encoderInstance = 0;
    out.hwInstances = 1;
    out.svcLayers = 1;
    describeColor(out, std::nullopt);
    logf(LogLevel::Info, "lavc: %s %ux%u@%d %d kbps %s, %s, %s (%s)", det_.encoder.c_str(), width_, height_, p.fps, p.kbps,
         out.rateControl.c_str(),
         mode_ == Mode::ZeroCopy   ? "D3D11 textures mapped into QSV surfaces (zero copy)"
         : mode_ == Mode::Readback ? "frames read back into system memory"
                                   : "test pattern frames",
         out.liveBitrate == "flush" ? "rate changes with an IDR" : "rate changes without a forced IDR", rt_.versionText.c_str());

    {
        std::lock_guard<std::mutex> lock(mu_);
        stopped_ = false;
    }
    thread_ = std::thread([this] { encodeLoop(); });
    return Status::Ok();
}

Status LavcEncoder::open(const StartParams& p) {
    OpenSpec s;
    s.encoder = det_.encoder;
    s.qsv = det_.qsv;
    s.width = int(width_);
    s.height = int(height_);
    s.fps = p.fps;
    s.kbps = p.kbps;
    s.vbvFrames = rate_.vbvFrames;
    s.cbr = cbr_;
    s.quality = p.quality;
    if (det_.qsv && device_) {
        Status st = makeQsvDevice(rt_, device_.Get(), mt_.Get(), d3dDevice_, qsvDevice_);
        if (!st.ok && mode_ == Mode::ZeroCopy) return st;
        if (!st.ok) {
            // System-memory frames can still go to a session qsvenc creates
            // itself (the default Quick Sync adapter).
            logf(LogLevel::Warn, "%s: encoding on the default Quick Sync adapter instead", st.text.c_str());
        } else if (mode_ == Mode::ZeroCopy) {
            // A dynamic D3D11 pool (any texture of the device can be mapped)
            // and the QSV pool derived from it.
            d3dFrames_ = rt_.av_hwframe_ctx_alloc(d3dDevice_);
            if (!d3dFrames_) return Status::Error("init_failed", "lavc: av_hwframe_ctx_alloc(d3d11) failed");
            auto* fc = reinterpret_cast<AVHWFramesContext*>(d3dFrames_->data);
            fc->format = AV_PIX_FMT_D3D11;
            fc->sw_format = AV_PIX_FMT_NV12;
            fc->width = int(width_);
            fc->height = int(height_);
            fc->initial_pool_size = 0;
            static_cast<AVD3D11VAFramesContext*>(fc->hwctx)->BindFlags = D3D11_BIND_RENDER_TARGET;  // the converter renders into them
            int r = rt_.av_hwframe_ctx_init(d3dFrames_);
            if (r < 0) return Status::Error("init_failed", "lavc: av_hwframe_ctx_init(d3d11): " + rt_.errorText(r));
            r = rt_.av_hwframe_ctx_create_derived(&qsvFrames_, AV_PIX_FMT_QSV, qsvDevice_, d3dFrames_, AV_HWFRAME_MAP_DIRECT);
            if (r < 0) {
                return Status::Error("init_failed", "lavc: no QSV frames on D3D11 textures (av_hwframe_ctx_create_derived): " +
                                                        rt_.errorText(r));
            }
            s.hwFrames = qsvFrames_;
        } else {
            s.hwDevice = qsvDevice_;
        }
    }
    std::string err;
    enc_ = openQsv(rt_, s, err);
    lowPower_ = s.qsv && s.lowPower;
    if (!enc_) return Status::Error("init_failed", "lavc: " + err);
    return Status::Ok();
}

void LavcEncoder::freeContexts() {
    if (enc_) rt_.avcodec_free_context(&enc_);
    rt_.av_buffer_unref(&qsvFrames_);
    rt_.av_buffer_unref(&d3dFrames_);
    rt_.av_buffer_unref(&qsvDevice_);
    rt_.av_buffer_unref(&d3dDevice_);
}

void LavcEncoder::release() {
    shutdown();
    if (thread_.joinable()) thread_.join();
    for (Input& i : inputs_) rt_.av_frame_free(&i.frame);
    inputs_.clear();
    for (Output& o : outputs_) {
        if (o.err.ok) releaseOutput(o.frame);
    }
    outputs_.clear();
    pending_.clear();
    // The codec context first: it holds the mapped frames, which hold the
    // converter's textures and the frames / device contexts.
    freeContexts();
    stagingY_.Reset();
    stagingUv_.Reset();
    context_.Reset();
    mt_.Reset();
    device_.Reset();
}

// --- Frames (capture thread) ---------------------------------------------------------

Status LavcEncoder::submit(const EncoderFrame& frame, const SubmitInfo& info) {
    {
        std::lock_guard<std::mutex> lock(mu_);
        if (stopped_) return Status::Ok();
        if (inputs_.size() >= kMaxQueued) {
            return Status::Error("encoder_busy", "the encoder has not taken the previous frame yet");
        }
    }
    AVFrame* f = nullptr;
    Status s = makeFrame(frame, info, f);
    if (!s.ok) return s;
    {
        std::lock_guard<std::mutex> lock(mu_);
        if (!stopped_) {
            inputs_.push_back(Input{f, info});
            f = nullptr;
        }
    }
    if (f) rt_.av_frame_free(&f);
    inCv_.notify_one();
    return Status::Ok();
}

Status LavcEncoder::makeFrame(const EncoderFrame& ef, const SubmitInfo& info, AVFrame*& out) {
    if (mode_ == Mode::ZeroCopy) return mapTexture(ef, out);
    AVFrame* f = rt_.av_frame_alloc();
    if (!f) return Status::Error("encode_failed", "lavc: av_frame_alloc failed");
    // One buffer, CbCr after the luma rows, frameW_ x frameH_ (the layout
    // qsvenc takes without copying, above).
    const size_t lumaBytes = size_t(frameW_) * frameH_;
    f->buf[0] = rt_.av_buffer_alloc(lumaBytes + lumaBytes / 2);
    if (!f->buf[0]) {
        rt_.av_frame_free(&f);
        return Status::Error("encode_failed", "lavc: av_buffer_alloc failed");
    }
    f->format = AV_PIX_FMT_NV12;
    f->width = int(width_);
    f->height = int(frameH_);
    f->data[0] = f->buf[0]->data;
    f->data[1] = f->data[0] + lumaBytes;
    f->linesize[0] = f->linesize[1] = int(frameW_);
    Status s;
    if (mode_ == Mode::Readback) {
        s = readback(ef, f);
    } else {
        // A moving diagonal gradient on neutral chroma: every frame differs,
        // so the encoder has work (the synthetic source has no image; the
        // barcode needs the GPU conversion). The padding too.
        const unsigned shift = unsigned(info.frameId * 4);
        for (uint32_t y = 0; y < frameH_; ++y) {
            uint8_t* row = f->data[0] + size_t(y) * size_t(f->linesize[0]);
            for (uint32_t x = 0; x < frameW_; ++x) row[x] = uint8_t(16 + ((x + y + shift) & 0xff) * 219 / 255);
        }
        std::memset(f->data[1], 128, size_t(f->linesize[1]) * (frameH_ / 2));
    }
    if (!s.ok) {
        rt_.av_frame_free(&f);
        return s;
    }
    out = f;
    return Status::Ok();
}

Status LavcEncoder::mapTexture(const EncoderFrame& ef, AVFrame*& out) {
    if (!ef.nv12) return Status::Error("encode_failed", "lavc: no converted NV12 texture to encode");
    AVFrame* src = rt_.av_frame_alloc();
    if (!src) return Status::Error("encode_failed", "lavc: av_frame_alloc failed");
    auto* ref = new TextureRef;
    ref->desc.texture = ef.nv12;
    ref->desc.index = 0;
    ref->hold = ef.hold;
    ref->texture = ef.nv12;
    src->buf[0] = rt_.av_buffer_create(reinterpret_cast<uint8_t*>(&ref->desc), sizeof(ref->desc), freeTextureRef, ref,
                                       AV_BUFFER_FLAG_READONLY);
    if (!src->buf[0]) {
        delete ref;
        rt_.av_frame_free(&src);
        return Status::Error("encode_failed", "lavc: av_buffer_create failed");
    }
    src->format = AV_PIX_FMT_D3D11;
    src->width = int(width_);
    src->height = int(height_);
    src->data[0] = reinterpret_cast<uint8_t*>(ef.nv12);
    src->data[1] = nullptr;  // array index 0 (a single texture)
    src->hw_frames_ctx = rt_.av_buffer_ref(d3dFrames_);
    AVFrame* q = rt_.av_frame_alloc();
    if (!q || !src->hw_frames_ctx) {
        rt_.av_frame_free(&q);
        rt_.av_frame_free(&src);
        return Status::Error("encode_failed", "lavc: out of memory");
    }
    q->format = AV_PIX_FMT_QSV;
    q->hw_frames_ctx = rt_.av_buffer_ref(qsvFrames_);
    const int r = rt_.av_hwframe_map(q, src, AV_HWFRAME_MAP_READ | AV_HWFRAME_MAP_DIRECT);
    rt_.av_frame_free(&src);  // the mapping keeps its own reference
    if (r < 0) {
        rt_.av_frame_free(&q);
        return Status::Error("encode_failed", "lavc: av_hwframe_map(d3d11 -> qsv): " + rt_.errorText(r));
    }
    out = q;
    return Status::Ok();
}

// Copies planes of `tex` (subresource 0) to the CPU through a staging texture.
Status LavcEncoder::readTexture(ID3D11Texture2D* tex, ComPtr<ID3D11Texture2D>& staging, const PlaneCopy* planes, int count) {
    D3D11_TEXTURE2D_DESC d{};
    tex->GetDesc(&d);
    if (staging) {
        D3D11_TEXTURE2D_DESC sd{};
        staging->GetDesc(&sd);
        if (sd.Width != d.Width || sd.Height != d.Height || sd.Format != d.Format) staging.Reset();
    }
    if (!staging) {
        D3D11_TEXTURE2D_DESC sd = d;
        sd.MipLevels = 1;
        sd.ArraySize = 1;
        sd.Usage = D3D11_USAGE_STAGING;
        sd.BindFlags = 0;
        sd.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        sd.MiscFlags = 0;
        const HRESULT hr = device_->CreateTexture2D(&sd, nullptr, staging.GetAddressOf());
        if (FAILED(hr)) {
            Status s;
            if (d3d::deviceRemoved(device_.Get(), "lavc: staging texture", s)) return s;
            return Status::Error("encode_failed", "lavc: staging texture: " + d3d::hrText(hr));
        }
    }
    // Single calls on the immediate context (multithread protected); Map
    // waits for the conversion and the copy.
    context_->CopyResource(staging.Get(), tex);
    D3D11_MAPPED_SUBRESOURCE m{};
    const HRESULT hr = context_->Map(staging.Get(), 0, D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) {
        Status s;
        if (d3d::deviceRemoved(device_.Get(), "lavc: reading a frame back", s)) return s;
        return Status::Error("encode_failed", "lavc: Map of the staging texture: " + d3d::hrText(hr));
    }
    const uint32_t mappedRows = d.Format == DXGI_FORMAT_NV12 ? d.Height + d.Height / 2 : d.Height;
    for (int i = 0; i < count; ++i) {
        if (planes[i].rowBytes > m.RowPitch || planes[i].rowsBefore + planes[i].rows > mappedRows) {
            context_->Unmap(staging.Get(), 0);
            return Status::Error("encode_failed", "lavc: the converted frame (" + std::to_string(d.Width) + "x" + std::to_string(d.Height) +
                                                      ") is smaller than the encoder's input");
        }
    }
    for (int i = 0; i < count; ++i) {
        const PlaneCopy& pc = planes[i];
        const uint8_t* base = static_cast<const uint8_t*>(m.pData) + size_t(pc.rowsBefore) * m.RowPitch;
        for (uint32_t y = 0; y < pc.rows; ++y) {
            std::memcpy(pc.dst + size_t(y) * size_t(pc.dstPitch), base + size_t(y) * m.RowPitch, pc.rowBytes);
        }
    }
    context_->Unmap(staging.Get(), 0);
    return Status::Ok();
}

Status LavcEncoder::readback(const EncoderFrame& ef, AVFrame* f) {
    // The converter's output is frameW_ x frameH_ (the padded frame).
    const PlaneCopy luma{f->data[0], f->linesize[0], frameW_, frameH_, 0};
    if (ef.nv12) {
        // NV12: the chroma rows follow the texture's luma rows (D3D11 maps
        // both planes of a planar format as one subresource).
        D3D11_TEXTURE2D_DESC d{};
        ef.nv12->GetDesc(&d);
        const PlaneCopy planes[] = {luma, PlaneCopy{f->data[1], f->linesize[1], frameW_, frameH_ / 2, d.Height}};
        return readTexture(ef.nv12, stagingY_, planes, 2);
    }
    if (ef.y && ef.uv) {
        // Planar conversion (devices without NV12 render targets: Wine): R8
        // luma, R8G8 chroma (interleaved Cb Cr, the NV12 layout).
        Status s = readTexture(ef.y, stagingY_, &luma, 1);
        const PlaneCopy chroma{f->data[1], f->linesize[1], frameW_, frameH_ / 2, 0};
        if (s.ok) s = readTexture(ef.uv, stagingUv_, &chroma, 1);
        return s;
    }
    return Status::Error("encode_failed", "lavc: no converted frame to read back");
}

// --- Encoder thread ------------------------------------------------------------------

void LavcEncoder::encodeLoop() {
    for (;;) {
        Input in;
        {
            std::unique_lock<std::mutex> lock(mu_);
            inCv_.wait(lock, [this] { return stopped_ || !inputs_.empty(); });
            if (stopped_) return;
            in = inputs_.front();
            inputs_.pop_front();
        }
        encodeOne(in);
    }
}

void LavcEncoder::encodeOne(Input& in) {
    bool idr = false, rateNow = false;
    RateParams r;
    {
        std::lock_guard<std::mutex> lock(ctlMu_);
        idr = idrPending_;
        idrPending_ = false;
        if (rateDirty_) {
            r = pendingRate_;
            pendingRate_ = RateParams{};
            rateDirty_ = false;
            rateNow = true;
        }
    }
    if (rateNow) {
        // qsvenc sees the changed fields with this frame and resets the
        // encoder (libx264: x264_encoder_reconfig).
        if (r.kbps > 0) rate_.kbps = r.kbps;
        if (r.vbvFrames > 0) rate_.vbvFrames = r.vbvFrames;
        if (r.fps > 0) {
            rate_.fps = r.fps;
            enc_->framerate = AVRational{r.fps, 1};
        }
        setRateFields(enc_, det_.qsv, cbr_, rate_.kbps, rate_.vbvFrames, rate_.fps);
        if (flushMode_) {
            idr = true;
            ++gen_;
        }
        logf(LogLevel::Debug, "lavc: rate %d kbps, vbv %.2f frames, %d fps from frame %llu%s", rate_.kbps, rate_.vbvFrames, rate_.fps,
             static_cast<unsigned long long>(in.info.frameId), flushMode_ ? " (IDR)" : "");
    }
    AVFrame* f = in.frame;
    f->pts = pts_++;
    if (idr) {
        f->pict_type = AV_PICTURE_TYPE_I;
        f->flags |= AV_FRAME_FLAG_KEY;
    }
    pending_.push_back(Pending{f->pts, in.info, gen_, idr});
    int ret = rt_.avcodec_send_frame(enc_, f);
    rt_.av_frame_free(&f);
    if (ret < 0) {
        pending_.pop_back();
        if (idr) {
            std::lock_guard<std::mutex> lock(ctlMu_);
            idrPending_ = true;  // the next frame then
        }
        fail("avcodec_send_frame", ret);
        return;
    }
    for (;;) {
        AVPacket* pkt = rt_.av_packet_alloc();
        if (!pkt) {
            fail("av_packet_alloc", AVERROR(ENOMEM));
            return;
        }
        ret = rt_.avcodec_receive_packet(enc_, pkt);
        if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) {
            rt_.av_packet_free(&pkt);
            return;
        }
        if (ret < 0) {
            rt_.av_packet_free(&pkt);
            fail("avcodec_receive_packet", ret);
            return;
        }
        deliver(pkt);
    }
}

void LavcEncoder::deliver(AVPacket* pkt) {
    // No B frames: packets come in submission order. Frames the encoder
    // dropped (none are expected) have no packet.
    while (!pending_.empty() && pending_.front().pts < pkt->pts) {
        warnOnce("dropped", "the encoder produced no packet for frame " + std::to_string(pending_.front().info.frameId));
        pending_.pop_front();
    }
    if (pending_.empty()) {
        warnOnce("pts", "a packet (pts " + std::to_string(pkt->pts) + ") for no submitted frame: dropped");
        rt_.av_packet_free(&pkt);
        return;
    }
    const Pending p = pending_.front();
    pending_.pop_front();
    if (p.pts != pkt->pts) {
        warnOnce("pts", "packet pts " + std::to_string(pkt->pts) + " for the frame submitted with pts " + std::to_string(p.pts));
    }
    auto* op = new OutPacket;
    op->packet = pkt;
    EncodedFrame e;
    e.info = p.info;
    e.gen = p.gen;
    e.key = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
    e.width = width_;
    e.height = height_;
    e.data = pkt->data;
    e.size = size_t(std::max(0, pkt->size));
    e.token = op;
    if (e.key && !hasParameterSets(codec_, e.data, e.size)) {
        // A key frame must be a decoder entry point (ring flag KEY):
        // AV_CODEC_FLAG_GLOBAL_HEADER can keep the parameter sets out of the
        // stream (libx264), qsvenc returns them as extradata.
        op->patched = withParameterSets(codec_, e.data, e.size, extradata_.data(), extradata_.size());
        if (!op->patched.empty()) {
            e.data = op->patched.data();
            e.size = op->patched.size();
        } else {
            warnOnce("headers", "a key frame without parameter sets, and the encoder's extradata cannot supply them");
        }
    }
    if (p.forcedIdr && !e.key) {
        warnOnce("idr", "frame " + std::to_string(p.info.frameId) + " was forced to be an IDR but did not come out as a key frame");
    }
    e.discardable = isDiscardable(e.key, layerInfo(codec_, e.data, e.size, 0), 1, 0);
    errors_ = 0;
    Output o;
    o.frame = e;
    push(std::move(o));
}

void LavcEncoder::fail(const std::string& what, int err) {
    Status s = Status::Error("encode_failed", "lavc: " + what + ": " + rt_.errorText(err));
    Status lost;
    if (device_ && d3d::deviceRemoved(device_.Get(), "lavc: " + what, lost)) {
        s = lost;
    } else if (++errors_ >= kMaxErrors) {
        s.fatal = true;
        s.text += " (" + std::to_string(errors_) + " failures in a row)";
    }
    Output o;
    o.err = s;
    push(std::move(o));
}

void LavcEncoder::push(Output o) {
    {
        std::lock_guard<std::mutex> lock(mu_);
        outputs_.push_back(std::move(o));
    }
    outCv_.notify_one();
}

// --- Output thread -------------------------------------------------------------------

Next LavcEncoder::receive(EncodedFrame& out, int timeoutMs, Status& err) {
    std::unique_lock<std::mutex> lock(mu_);
    outCv_.wait_for(lock, std::chrono::milliseconds(timeoutMs), [this] { return stopped_ || !outputs_.empty(); });
    if (stopped_) return Next::Stopped;
    if (outputs_.empty()) return Next::Timeout;
    Output o = std::move(outputs_.front());
    outputs_.pop_front();
    if (!o.err.ok) {
        err = o.err;
        return Next::Error;
    }
    out = o.frame;
    if (!out.outputQpc) out.outputQpc = qpcNow();
    return Next::Frame;
}

void LavcEncoder::releaseOutput(EncodedFrame& f) {
    auto* op = static_cast<OutPacket*>(f.token);
    f.token = nullptr;
    if (!op) return;
    rt_.av_packet_free(&op->packet);
    delete op;
}

// --- Control thread ------------------------------------------------------------------

Status LavcEncoder::forceIdr() {
    std::lock_guard<std::mutex> lock(ctlMu_);
    idrPending_ = true;
    return Status::Ok();
}

Status LavcEncoder::recover(uint64_t lostFromFrameId, std::optional<uint64_t>) {
    // No long-term references or reference invalidation: an IDR.
    logf(LogLevel::Debug, "lavc: loss at frame %llu: IDR", static_cast<unsigned long long>(lostFromFrameId));
    return forceIdr();
}

Status LavcEncoder::setRate(const RateParams& r) {
    std::lock_guard<std::mutex> lock(ctlMu_);
    if (r.kbps > 0) pendingRate_.kbps = r.kbps;
    if (r.vbvFrames > 0) pendingRate_.vbvFrames = r.vbvFrames;
    if (r.fps > 0) pendingRate_.fps = r.fps;
    rateDirty_ = true;
    return Status::Ok();
}

Status LavcEncoder::setRoi(const std::vector<RoiRect>&) {
    return Status::Error("unsupported", "setRoi: the libavcodec backend has no region-of-interest encoding (caps roi none)");
}

void LavcEncoder::shutdown() {
    {
        std::lock_guard<std::mutex> lock(mu_);
        stopped_ = true;
    }
    inCv_.notify_all();
    outCv_.notify_all();
}

}  // namespace

Probe probeLavc() {
    // The full probe's answer when "auto" already ran it (it then failed).
    // Otherwise light: the runtime and an Intel adapter, no encoder opened.
    // chooseBackend calls it only when another backend was chosen (the probe
    // is then just for caps.unavailable), so a helper restart on an AMD /
    // NVIDIA host with an Intel iGPU does not spend time opening QSV encoders.
    if (g_probed) {
        const LavcProbe& p = lavcProbe();
        return {p.ok, p.ok ? "usable" : p.reason};
    }
    const LavcRuntime& rt = lavcRuntime();
    if (!rt.ok) return {false, rt.error};
    if (!lavcOptions().testEncoders.empty()) return {true, rt.versionText + ": test encoders"};
    ComPtr<IDXGIAdapter1> adapter;
    AdapterInfo info;
    if (!findIntelAdapter(adapter, info)) return {false, std::string(kNoIntel) + " (" + rt.versionText + ")"};
    return {true, rt.versionText + " on " + info.name};
}

std::unique_ptr<Backend> createLavcBackend(Status& err) {
    const LavcProbe& p = lavcProbe();
    if (!p.ok) {
        err = Status::Error("unavailable", p.reason);
        return nullptr;
    }
    return std::make_unique<LavcEncoder>(p);
}

}  // namespace recon
