// recon-fake-nvenc.dll: a test double of the NVIDIA driver's NVENC runtime
// (nvEncodeAPI64.dll) for `recon-encoder --self-test-nvenc=DLL` and
// `--encode-test ... --nvenc-test-dll=DLL` (recon-host qualify's tests), which
// load it by its full path in place of System32's (it is never installed anywhere).
// It lets the NVENC backend run without an NVIDIA GPU (Wine, CI on
// windows-latest with WARP) and checks how the backend uses the API:
//
// - the rules the NVENC headers and programming guide state, each a
//   "violation" when broken: struct versions, the API version of the session,
//   resources registered / mapped / locked / unlocked / unmapped / released in
//   the documented order (unmap only after the frame's NvEncLockBitstream,
//   bitstreams locked in submission order and, in async mode, only after their
//   completion event, a buffer or event not reused while its frame is pending,
//   everything released and EOS sent before NvEncDestroyEncoder), parameters
//   that cannot be reconfigured, resetEncoder without forceIDR,
//   NvEncGetSequenceParams from another thread than NvEncEncodePicture, a QP
//   delta map of the wrong size;
// - a reference model of the DPB and NvEncInvalidateRefFrames: each P frame
//   references the newest frame still valid in the DPB (maxNumRefFrames
//   frames; invalidation also invalidates frames predicted from an invalid
//   one), an intra frame when none is left;
// - a log line per call with the values the self-test asserts on.
//
// Bitstreams are structurally valid (H.264 / HEVC NAL units with parameter sets
// on IDRs, AV1 OBUs) and carry a text marker "NVFAKE ts=<id> ref=<id> t=<type>"
// (no zero bytes, so no accidental start codes) naming the frame and the frame
// it was predicted from, padded with 'x' after " pad=" to the size the rate
// control would give the frame (averageBitRate / frame rate; IDR and intra frames
// lowDelayKeyFrameScale times that), so a reconfigured bitrate shows in the
// frame sizes from the next frame on, as with a CBR encoder that follows at
// once (recon-host qualify's live-bitrate checks, step 3.6; set padToRate=0 for
// bare markers). The H.264 / HEVC SPS is a real one (level 5.1, the
// reference frames kept); keepRefs=N makes the double keep fewer than asked,
// like a driver that clamps the DPB. An encode takes `encodeUs` on a single engine (frames
// finish one after another); async mode signals the events from a worker thread.
//
// Control (only the self-test calls it): ReconFakeNvencControl(command, reply,
// size): "reset", "set key=value ...", "log", "violations", "stats".
// Exports: test/fake_nvenc.def (the header already declares the two NVENC
// entry points without dllexport).
#include <windows.h>

#include <algorithm>
#include <condition_variable>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <map>
#include <memory>
#include <mutex>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include <ffnvcodec/nvEncodeAPI.h>

namespace {

enum class Codec { H264, Hevc, Av1, None };

struct Config {
    uint32_t maxVersion = (13u << 4) | 0u;
    int h264 = 1, hevc = 1, av1 = 1;
    int async = 1, rfi = 1, multiRef = 1, dynBitrate = 1, dynRes = 1, svc = 1, maxLayers = 4, ltr = 8, engines = 2;
    int emphasis = 1, tenBit = 1, yuv444 = 1, customVbv = 1, intraRefresh = 1, cabac = 1, subframe = 1, stateAdvance = 1;
    int maxW = 8192, maxH = 8192, minW = 145, minH = 49;
    int encodeUs = 1500;    // simulated encode time
    uint64_t failEncodeTs = 0;  // NvEncEncodePicture fails (NV_ENC_ERR_GENERIC) for this inputTimeStamp
    int keepRefs = 0;       // > 0: keep at most this many reference frames (DPB and SPS), whatever was asked
    int padToRate = 1;      // frames padded to the configured bitrate (the marker alone with 0)
};

struct Frame {
    uint64_t ts = 0;
    void* output = nullptr;
    HANDLE event = nullptr;
    void* mapped = nullptr;
    int64_t doneAt = 0;  // QPC
    bool locked = false;
    NV_ENC_PIC_TYPE type = NV_ENC_PIC_TYPE_P;
    uint64_t ref = 0;
    uint32_t frameIdx = 0;
    std::string data;
};

struct DpbEntry {
    uint64_t ts;
    uint64_t ref;  // 0 = intra
    bool invalid;
};

struct Session {
    void* device = nullptr;
    bool initialized = false;
    NV_ENC_INITIALIZE_PARAMS init{};
    NV_ENC_CONFIG config{};
    Codec codec = Codec::None;
    std::set<HANDLE> events;
    std::set<void*> buffers;
    std::map<void*, void*> regs;     // registered handle -> texture
    std::map<void*, void*> mapped;   // mapped handle -> registered handle
    std::deque<Frame> pending;       // submitted, not unlocked yet (submission order)
    std::deque<DpbEntry> dpb;
    bool forceIdrNext = false;
    bool eos = false;
    uint64_t encoded = 0;
    uint64_t lastTs = 0;
    int64_t lastDone = 0;  // QPC when the engine finishes the last submitted frame
    DWORD encodeThread = 0;  // the thread of the last NvEncEncodePicture
    DWORD seqThread = 0;     // the thread of the last NvEncGetSequenceParams
    std::vector<uint64_t> history;  // every encoded ts
};

std::mutex g_mu;
Config g_cfg;
std::vector<std::string> g_log, g_violations;
std::set<Session*> g_sessions;
int64_t g_freq = 1;
int g_handles = 0;

// Async completion: events to signal at a QPC time.
std::mutex g_qmu;
std::condition_variable g_qcv;
std::deque<std::pair<int64_t, HANDLE>> g_queue;
bool g_worker = false;

int64_t now() {
    LARGE_INTEGER v;
    QueryPerformanceCounter(&v);
    return v.QuadPart;
}

void logLine(const char* fmt, ...) {
    char buf[1024];
    va_list ap;
    va_start(ap, fmt);
    std::vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    g_log.emplace_back(buf);
}

void violation(const char* fmt, ...) {
    char buf[1024];
    va_list ap;
    va_start(ap, fmt);
    std::vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    g_violations.emplace_back(buf);
    g_log.push_back(std::string("VIOLATION ") + buf);
}

void* newHandle() { return reinterpret_cast<void*>(static_cast<uintptr_t>(0x10000 + 16 * ++g_handles)); }

void worker() {
    std::unique_lock<std::mutex> lock(g_qmu);
    for (;;) {
        g_qcv.wait(lock, [] { return !g_queue.empty(); });
        const auto [at, ev] = g_queue.front();
        const int64_t t = now();
        if (t < at) {
            g_qcv.wait_for(lock, std::chrono::microseconds((at - t) * 1000000 / g_freq));
            continue;
        }
        g_queue.pop_front();
        lock.unlock();
        SetEvent(ev);
        lock.lock();
    }
}

void schedule(int64_t at, HANDLE ev) {
    std::lock_guard<std::mutex> lock(g_qmu);
    if (!g_worker) {
        std::thread(worker).detach();
        g_worker = true;
    }
    g_queue.emplace_back(at, ev);
    g_qcv.notify_all();
}

Codec codecOf(const GUID& g) {
    if (!std::memcmp(&g, &NV_ENC_CODEC_H264_GUID, sizeof(GUID))) return Codec::H264;
    if (!std::memcmp(&g, &NV_ENC_CODEC_HEVC_GUID, sizeof(GUID))) return Codec::Hevc;
    if (!std::memcmp(&g, &NV_ENC_CODEC_AV1_GUID, sizeof(GUID))) return Codec::Av1;
    return Codec::None;
}

bool codecEnabled(Codec c) {
    return (c == Codec::H264 && g_cfg.h264) || (c == Codec::Hevc && g_cfg.hevc) || (c == Codec::Av1 && g_cfg.av1);
}

const char* codecName(Codec c) { return c == Codec::H264 ? "h264" : c == Codec::Hevc ? "hevc" : c == Codec::Av1 ? "av1" : "?"; }

int presetNumber(const GUID& g) {
    const GUID* p[] = {&NV_ENC_PRESET_P1_GUID, &NV_ENC_PRESET_P2_GUID, &NV_ENC_PRESET_P3_GUID, &NV_ENC_PRESET_P4_GUID,
                       &NV_ENC_PRESET_P5_GUID, &NV_ENC_PRESET_P6_GUID, &NV_ENC_PRESET_P7_GUID};
    for (int i = 0; i < 7; ++i) {
        if (!std::memcmp(&g, p[i], sizeof(GUID))) return i + 1;
    }
    return 0;
}

Session* session(void* enc) {
    auto* s = static_cast<Session*>(enc);
    if (!g_sessions.count(s)) {
        violation("call on an unknown or destroyed session %p", enc);
        return nullptr;
    }
    return s;
}

uint32_t dpbSize(const Session& s) {
    uint32_t n = 1;
    switch (s.codec) {
    case Codec::H264: n = s.config.encodeCodecConfig.h264Config.maxNumRefFrames; break;
    case Codec::Hevc: n = s.config.encodeCodecConfig.hevcConfig.maxNumRefFramesInDPB; break;
    case Codec::Av1: n = s.config.encodeCodecConfig.av1Config.maxNumRefFramesInDPB; break;
    default: break;
    }
    // A driver that keeps fewer than asked (set keepRefs=N).
    if (g_cfg.keepRefs > 0) n = std::min(n, uint32_t(g_cfg.keepRefs));
    return std::max<uint32_t>(1, n);
}

uint32_t qpBlock(Codec c) { return c == Codec::H264 ? 16 : c == Codec::Hevc ? 32 : 64; }

// RBSP writer for the SPS: u(n), ue(v), trailing bits, then emulation
// prevention (00 00 0x -> 00 00 03 0x for x <= 3, 7.4.1).
struct BitWriter {
    std::vector<int> bits;
    void u(int n, uint32_t v) {
        for (int i = n - 1; i >= 0; --i) bits.push_back(int((v >> i) & 1));
    }
    void ue(uint32_t v) {
        const uint64_t x = uint64_t(v) + 1;
        int n = 0;
        while ((x >> n) > 1) ++n;
        u(n, 0);
        for (int i = n; i >= 0; --i) bits.push_back(int((x >> i) & 1));
    }
    std::string nal(const std::string& header) {
        bits.push_back(1);  // rbsp_stop_one_bit
        while (bits.size() % 8) bits.push_back(0);
        std::string out = header;
        int zeros = 0;
        for (size_t i = 0; i < bits.size(); i += 8) {
            int b = 0;
            for (size_t j = 0; j < 8; ++j) b = (b << 1) | bits[i + j];
            if (zeros >= 2 && b <= 3) {
                out += char(3);
                zeros = 0;
            }
            out += char(b);
            zeros = b == 0 ? zeros + 1 : 0;
        }
        return out;
    }
};

// A real SPS (what the backend reads the level and reference frames from):
// H.264 High (7.3.2.1.1) with max_num_ref_frames, HEVC Main (7.3.2.2.1) with
// sps_max_dec_pic_buffering_minus1 = the reference frames kept (dpbSize), level
// 5.1, the session's size.
std::string sps(const Session& s) {
    const uint32_t w = s.init.encodeWidth, h = s.init.encodeHeight, refs = dpbSize(s);
    BitWriter b;
    if (s.codec == Codec::H264) {
        const uint32_t mbW = (w + 15) / 16, mbH = (h + 15) / 16;
        b.u(8, 100), b.u(8, 0), b.u(8, 51), b.ue(0);          // profile_idc High, constraints, level 5.1, sps id
        b.ue(1), b.ue(0), b.ue(0), b.u(1, 0), b.u(1, 0);       // 4:2:0, 8 bit, no bypass, no scaling matrix
        b.ue(0), b.ue(2), b.ue(refs), b.u(1, 0);               // log2_max_frame_num_minus4, poc type 2, max_num_ref_frames, no gaps
        b.ue(mbW - 1), b.ue(mbH - 1), b.u(1, 1), b.u(1, 1);    // size in macroblocks, frame_mbs_only, direct_8x8_inference
        const uint32_t cropBottom = (mbH * 16 - h) / 2;
        b.u(1, cropBottom ? 1 : 0);
        if (cropBottom) b.ue(0), b.ue(0), b.ue(0), b.ue(cropBottom);
        b.u(1, 0);  // vui_parameters_present_flag
        return b.nal(std::string("\x67", 1));
    }
    b.u(4, 0), b.u(3, 0), b.u(1, 1);  // vps id, sps_max_sub_layers_minus1, temporal_id_nesting
    // profile_tier_level: Main, Main tier, progressive / frame only, level 5.1
    b.u(2, 0), b.u(1, 0), b.u(5, 1), b.u(32, 0x60000000), b.u(4, 9), b.u(32, 0), b.u(11, 0), b.u(1, 0), b.u(8, 153);
    b.ue(0), b.ue(1), b.ue(w), b.ue(h), b.u(1, 0);  // sps id, 4:2:0, size, no conformance window
    b.ue(0), b.ue(0), b.ue(4), b.u(1, 1);            // 8 bit, log2_max_pic_order_cnt_lsb_minus4, ordering info present
    b.ue(refs), b.ue(0), b.ue(0);                    // sps_max_dec_pic_buffering_minus1, no reordering, no latency limit
    b.ue(0), b.ue(2), b.ue(0), b.ue(3), b.ue(0), b.ue(0);  // coding / transform block sizes and depths (CTB 32)
    b.u(1, 0), b.u(1, 0), b.u(1, 0), b.u(1, 0);      // no scaling list, AMP, SAO, PCM
    b.ue(0), b.u(1, 0), b.u(1, 0), b.u(1, 0), b.u(1, 0), b.u(1, 0);  // no short / long-term RPS, TMVP, strong intra, VUI, extension
    return b.nal(std::string("\x42\x01", 2));
}

// Parameter sets of the session's codec (also NvEncGetSequenceParams).
std::string parameterSets(const Session& s) {
    std::string out;
    if (s.codec == Codec::H264) {
        out += std::string("\0\0\0\1", 4) + sps(s);
        out += std::string("\0\0\0\1\x68", 5) + "PPS";
    } else if (s.codec == Codec::Hevc) {
        out += std::string("\0\0\0\1\x40\x01", 6) + "VPS";
        out += std::string("\0\0\0\1", 4) + sps(s);
        out += std::string("\0\0\0\1\x44\x01", 6) + "PPS";
    } else if (s.codec == Codec::Av1) {
        out += std::string("\x0a\x05", 2) + "SEQHD";  // OBU_SEQUENCE_HEADER, has_size, 5 bytes
    }
    return out;
}

// The bytes a frame of this type gets from the configured rate (padToRate).
size_t rateBytes(const Session& s, NV_ENC_PIC_TYPE type) {
    const NV_ENC_RC_PARAMS& rc = s.config.rcParams;
    const uint32_t num = std::max(1u, s.init.frameRateNum), den = std::max(1u, s.init.frameRateDen);
    size_t n = size_t(uint64_t(rc.averageBitRate) / 8 * den / num);
    if (type != NV_ENC_PIC_TYPE_P) n *= std::max<size_t>(1, rc.lowDelayKeyFrameScale);
    return n;
}

std::string bitstream(const Session& s, const Frame& f, bool withHeaders) {
    char marker[96];
    const char* t = f.type == NV_ENC_PIC_TYPE_IDR ? "IDR" : f.type == NV_ENC_PIC_TYPE_I ? "I" : "P";
    std::snprintf(marker, sizeof(marker), "NVFAKE ts=%llu ref=%llu t=%s", static_cast<unsigned long long>(f.ts),
                  static_cast<unsigned long long>(f.ref), t);
    const bool idr = f.type == NV_ENC_PIC_TYPE_IDR;
    std::string out, payload = marker;
    const size_t target = g_cfg.padToRate ? rateBytes(s, f.type) : 0;
    if (s.codec == Codec::Av1) {
        out += std::string("\x12\x00", 2);  // temporal delimiter
        if (idr && withHeaders) out += parameterSets(s);
        if (target > out.size() + payload.size() + 16) payload += " pad=" + std::string(target - out.size() - payload.size() - 16, 'x');
        out += char(0x32);  // OBU_FRAME, has_size
        for (size_t v = payload.size();; v >>= 7) {  // obu_size, leb128
            out += char((v & 0x7f) | (v > 0x7f ? 0x80 : 0));
            if (v <= 0x7f) break;
        }
        out += payload;
        return out;
    }
    if (idr && withHeaders) out += parameterSets(s);
    if (s.codec == Codec::H264) out += std::string("\0\0\0\1", 4) + char(idr ? 0x65 : 0x41);
    else out += std::string("\0\0\0\1", 4) + char(idr ? 19 << 1 : 1 << 1) + char(1);
    if (target > out.size() + payload.size() + 5) payload += " pad=" + std::string(target - out.size() - payload.size() - 5, 'x');
    out += payload;
    return out;
}

// --- NVENC API ----------------------------------------------------------------------------

NVENCSTATUS NVENCAPI openSessionEx(NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS* p, void** encoder) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!p || !encoder) return NV_ENC_ERR_INVALID_PTR;
    if (p->version != NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER) {
        violation("NvEncOpenEncodeSessionEx: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (p->apiVersion != NVENCAPI_VERSION) {
        violation("NvEncOpenEncodeSessionEx: apiVersion %08x, not NVENCAPI_VERSION", p->apiVersion);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (p->deviceType != NV_ENC_DEVICE_TYPE_DIRECTX || !p->device) {
        violation("NvEncOpenEncodeSessionEx: device type %d / device %p", int(p->deviceType), p->device);
        return NV_ENC_ERR_INVALID_DEVICE;
    }
    auto* s = new Session;
    s->device = p->device;
    g_sessions.insert(s);
    *encoder = s;
    logLine("open session");
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getEncodeGuidCount(void* enc, uint32_t* n) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    *n = uint32_t(g_cfg.h264 + g_cfg.hevc + g_cfg.av1);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getEncodeGuids(void* enc, GUID* guids, uint32_t size, uint32_t* count) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    uint32_t n = 0;
    auto add = [&](const GUID& g) {
        if (n < size) guids[n] = g;
        ++n;
    };
    if (g_cfg.h264) add(NV_ENC_CODEC_H264_GUID);
    if (g_cfg.hevc) add(NV_ENC_CODEC_HEVC_GUID);
    if (g_cfg.av1) add(NV_ENC_CODEC_AV1_GUID);
    *count = std::min(n, size);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getEncodeCaps(void* enc, GUID codec, NV_ENC_CAPS_PARAM* p, int* value) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!p || !value) return NV_ENC_ERR_INVALID_PTR;
    if (p->version != NV_ENC_CAPS_PARAM_VER) {
        violation("NvEncGetEncodeCaps: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    const Codec c = codecOf(codec);
    if (!codecEnabled(c)) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    int v = 0;
    switch (p->capsToQuery) {
    case NV_ENC_CAPS_WIDTH_MAX: v = g_cfg.maxW; break;
    case NV_ENC_CAPS_HEIGHT_MAX: v = g_cfg.maxH; break;
    case NV_ENC_CAPS_WIDTH_MIN: v = g_cfg.minW; break;
    case NV_ENC_CAPS_HEIGHT_MIN: v = g_cfg.minH; break;
    case NV_ENC_CAPS_SUPPORT_10BIT_ENCODE: v = g_cfg.tenBit; break;
    case NV_ENC_CAPS_SUPPORT_YUV444_ENCODE: v = c == Codec::Av1 ? 0 : g_cfg.yuv444; break;
    case NV_ENC_CAPS_SUPPORT_MULTIPLE_REF_FRAMES: v = g_cfg.multiRef; break;
    case NV_ENC_CAPS_SUPPORT_REF_PIC_INVALIDATION: v = g_cfg.rfi; break;
    case NV_ENC_CAPS_NUM_MAX_LTR_FRAMES: v = g_cfg.ltr; break;
    case NV_ENC_CAPS_SUPPORT_INTRA_REFRESH: v = g_cfg.intraRefresh; break;
    case NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE: v = g_cfg.dynBitrate; break;
    case NV_ENC_CAPS_SUPPORT_DYN_RES_CHANGE: v = g_cfg.dynRes; break;
    case NV_ENC_CAPS_SUPPORT_TEMPORAL_SVC: v = g_cfg.svc; break;
    case NV_ENC_CAPS_NUM_MAX_TEMPORAL_LAYERS: v = g_cfg.maxLayers; break;
    case NV_ENC_CAPS_SUPPORT_EMPHASIS_LEVEL_MAP: v = c == Codec::H264 ? g_cfg.emphasis : 0; break;
    case NV_ENC_CAPS_SUPPORT_SUBFRAME_READBACK: v = g_cfg.subframe; break;
    case NV_ENC_CAPS_NUM_ENCODER_ENGINES: v = g_cfg.engines; break;
    case NV_ENC_CAPS_ASYNC_ENCODE_SUPPORT: v = g_cfg.async; break;
    case NV_ENC_CAPS_SUPPORT_CUSTOM_VBV_BUF_SIZE: v = g_cfg.customVbv; break;
    case NV_ENC_CAPS_SUPPORT_CABAC: v = c == Codec::H264 ? g_cfg.cabac : 0; break;
    case NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE: v = g_cfg.stateAdvance; break;
    case NV_ENC_CAPS_SINGLE_SLICE_INTRA_REFRESH: v = 1; break;
    default: v = 0;
    }
    *value = v;
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getInputFormatCount(void* enc, GUID codec, uint32_t* n) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!codecEnabled(codecOf(codec))) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    *n = 3;
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getInputFormats(void* enc, GUID codec, NV_ENC_BUFFER_FORMAT* f, uint32_t size, uint32_t* count) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!codecEnabled(codecOf(codec))) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    const NV_ENC_BUFFER_FORMAT all[] = {NV_ENC_BUFFER_FORMAT_NV12, NV_ENC_BUFFER_FORMAT_ARGB, NV_ENC_BUFFER_FORMAT_YUV420_10BIT};
    uint32_t n = 0;
    for (; n < 3 && n < size; ++n) f[n] = all[n];
    *count = n;
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getPresetConfigEx(void* enc, GUID codec, GUID preset, NV_ENC_TUNING_INFO tuning, NV_ENC_PRESET_CONFIG* pc) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!session(enc)) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!pc) return NV_ENC_ERR_INVALID_PTR;
    if (pc->version != NV_ENC_PRESET_CONFIG_VER || pc->presetCfg.version != NV_ENC_CONFIG_VER) {
        violation("NvEncGetEncodePresetConfigEx: struct versions %08x / %08x", pc->version, pc->presetCfg.version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    const Codec c = codecOf(codec);
    if (!codecEnabled(c) || !presetNumber(preset) || tuning <= NV_ENC_TUNING_INFO_UNDEFINED || tuning >= NV_ENC_TUNING_INFO_COUNT) {
        return NV_ENC_ERR_UNSUPPORTED_PARAM;
    }
    // Defaults a low-latency preset might have, several of which the backend
    // must override (a finite GOP, VBR, no AQ, the driver's DPB, AV1 level 0 =
    // level 2.0, the ULL key frame scale 1).
    NV_ENC_CONFIG& cfg = pc->presetCfg;
    cfg.profileGUID = NV_ENC_CODEC_PROFILE_AUTOSELECT_GUID;
    cfg.gopLength = 250;
    cfg.frameIntervalP = 1;
    cfg.rcParams.version = NV_ENC_RC_PARAMS_VER;
    cfg.rcParams.rateControlMode = NV_ENC_PARAMS_RC_VBR;
    cfg.rcParams.averageBitRate = 5000000;
    cfg.rcParams.lowDelayKeyFrameScale = 1;
    cfg.encodeCodecConfig.h264Config.idrPeriod = 250;
    cfg.encodeCodecConfig.hevcConfig.idrPeriod = 250;
    cfg.encodeCodecConfig.av1Config.idrPeriod = 250;
    cfg.encodeCodecConfig.av1Config.level = 0;
    logLine("presetconfig codec=%s preset=%d tuning=%d", codecName(c), presetNumber(preset), int(tuning));
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI initializeEncoder(void* enc, NV_ENC_INITIALIZE_PARAMS* ip) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!ip || !ip->encodeConfig) return NV_ENC_ERR_INVALID_PTR;
    if (ip->version != NV_ENC_INITIALIZE_PARAMS_VER || ip->encodeConfig->version != NV_ENC_CONFIG_VER ||
        ip->encodeConfig->rcParams.version != NV_ENC_RC_PARAMS_VER) {
        violation("NvEncInitializeEncoder: struct versions %08x / %08x / %08x", ip->version, ip->encodeConfig->version,
                  ip->encodeConfig->rcParams.version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (s->initialized) violation("NvEncInitializeEncoder twice");
    const Codec c = codecOf(ip->encodeGUID);
    const NV_ENC_CONFIG& cfg = *ip->encodeConfig;
    if (!codecEnabled(c)) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    if (!presetNumber(ip->presetGUID)) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    if (ip->enableEncodeAsync && !g_cfg.async) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    if (int(ip->encodeWidth) > g_cfg.maxW || int(ip->encodeHeight) > g_cfg.maxH || int(ip->encodeWidth) < g_cfg.minW ||
        int(ip->encodeHeight) < g_cfg.minH) {
        return NV_ENC_ERR_INVALID_PARAM;
    }
    if (!ip->enablePTD) violation("enablePTD 0: NV_ENC_PIC_FLAG_FORCEIDR needs picture type decision by the encoder");
    if (cfg.gopLength == NVENC_INFINITE_GOPLENGTH && cfg.frameIntervalP != 1) {
        violation("frameIntervalP %d with an infinite GOP (should be 1)", cfg.frameIntervalP);
    }
    if (ip->maxEncodeWidth && (!g_cfg.dynRes || ip->maxEncodeWidth < ip->encodeWidth || ip->maxEncodeHeight < ip->encodeHeight)) {
        violation("maxEncodeWidth/Height %ux%u for %ux%u (dynamic resolution cap %d)", ip->maxEncodeWidth, ip->maxEncodeHeight,
                  ip->encodeWidth, ip->encodeHeight, g_cfg.dynRes);
    }
    if (cfg.rcParams.qpMapMode == NV_ENC_QP_MAP_EMPHASIS && (cfg.rcParams.enableAQ || cfg.rcParams.enableTemporalAQ || c != Codec::H264)) {
        violation("emphasis map with AQ or on a codec other than H.264");
    }
    if (cfg.rcParams.vbvBufferSize && !g_cfg.customVbv) violation("custom VBV size without NV_ENC_CAPS_SUPPORT_CUSTOM_VBV_BUF_SIZE");
    s->initialized = true;
    s->init = *ip;
    s->config = cfg;
    s->init.encodeConfig = &s->config;
    s->codec = c;
    uint32_t idrPeriod = 0, refs = 0, l0 = 0, repeat = 0, vuiOk = 0, irPeriod = 0, irCnt = 0, layers = 0, level = 0;
    if (c == Codec::H264) {
        const NV_ENC_CONFIG_H264& h = cfg.encodeCodecConfig.h264Config;
        idrPeriod = h.idrPeriod, refs = h.maxNumRefFrames, l0 = uint32_t(h.numRefL0), repeat = h.repeatSPSPPS;
        const NV_ENC_CONFIG_H264_VUI_PARAMETERS& v = h.h264VUIParameters;
        vuiOk = v.videoSignalTypePresentFlag && !v.videoFullRangeFlag && v.colourPrimaries == NV_ENC_VUI_COLOR_PRIMARIES_BT709 &&
                v.colourMatrix == NV_ENC_VUI_MATRIX_COEFFS_BT709 && v.transferCharacteristics == NV_ENC_VUI_TRANSFER_CHARACTERISTIC_BT709;
        irPeriod = h.enableIntraRefresh ? h.intraRefreshPeriod : 0, irCnt = h.intraRefreshCnt;
        layers = h.enableTemporalSVC ? h.numTemporalLayers : 1;
        level = h.level;
    } else if (c == Codec::Hevc) {
        const NV_ENC_CONFIG_HEVC& h = cfg.encodeCodecConfig.hevcConfig;
        idrPeriod = h.idrPeriod, refs = h.maxNumRefFramesInDPB, l0 = uint32_t(h.numRefL0), repeat = h.repeatSPSPPS;
        const NV_ENC_CONFIG_HEVC_VUI_PARAMETERS& v = h.hevcVUIParameters;
        vuiOk = v.videoSignalTypePresentFlag && !v.videoFullRangeFlag && v.colourPrimaries == NV_ENC_VUI_COLOR_PRIMARIES_BT709 &&
                v.colourMatrix == NV_ENC_VUI_MATRIX_COEFFS_BT709 && v.transferCharacteristics == NV_ENC_VUI_TRANSFER_CHARACTERISTIC_BT709;
        irPeriod = h.enableIntraRefresh ? h.intraRefreshPeriod : 0, irCnt = h.intraRefreshCnt;
        layers = h.enableTemporalSVC ? h.numTemporalLayers : 1;
        level = h.level;
    } else {
        const NV_ENC_CONFIG_AV1& a = cfg.encodeCodecConfig.av1Config;
        idrPeriod = a.idrPeriod, refs = a.maxNumRefFramesInDPB, l0 = uint32_t(a.numFwdRefs), repeat = a.repeatSeqHdr;
        vuiOk = a.colorPrimaries == NV_ENC_VUI_COLOR_PRIMARIES_BT709 && a.matrixCoefficients == NV_ENC_VUI_MATRIX_COEFFS_BT709 &&
                a.transferCharacteristics == NV_ENC_VUI_TRANSFER_CHARACTERISTIC_BT709 && a.colorRange == 0;
        irPeriod = a.enableIntraRefresh ? a.intraRefreshPeriod : 0, irCnt = a.intraRefreshCnt;
        layers = a.enableTemporalSVC ? a.numTemporalLayers : 1;
        level = a.level;
    }
    const NV_ENC_RC_PARAMS& rc = cfg.rcParams;
    logLine("init codec=%s preset=%d tuning=%d async=%u ptd=%u size=%ux%u maxsize=%ux%u fps=%u/%u gop=%s pint=%d rc=%s avg=%u max=%u "
            "vbv=%u ldkfs=%u aq=%u taq=%u qpmap=%d zrd=%u la=%u mp=%d idr=%s refs=%u l0=%u repeat=%u vui=%u ir=%u/%u layers=%u "
            "level=%u profile=%s",
            codecName(c), presetNumber(ip->presetGUID), int(ip->tuningInfo), ip->enableEncodeAsync, ip->enablePTD, ip->encodeWidth,
            ip->encodeHeight, ip->maxEncodeWidth, ip->maxEncodeHeight, ip->frameRateNum, ip->frameRateDen,
            cfg.gopLength == NVENC_INFINITE_GOPLENGTH ? "inf" : "finite", cfg.frameIntervalP,
            rc.rateControlMode == NV_ENC_PARAMS_RC_CBR ? "cbr" : rc.rateControlMode == NV_ENC_PARAMS_RC_VBR ? "vbr" : "other",
            rc.averageBitRate, rc.maxBitRate, rc.vbvBufferSize, rc.lowDelayKeyFrameScale, rc.enableAQ, rc.enableTemporalAQ,
            int(rc.qpMapMode), rc.zeroReorderDelay, rc.enableLookahead, int(rc.multiPass),
            idrPeriod == NVENC_INFINITE_GOPLENGTH ? "inf" : "finite", refs, l0, repeat, vuiOk, irPeriod, irCnt, layers, level,
            std::memcmp(&cfg.profileGUID, &NV_ENC_CODEC_PROFILE_AUTOSELECT_GUID, sizeof(GUID)) ? "set" : "auto");
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI createBitstreamBuffer(void* enc, NV_ENC_CREATE_BITSTREAM_BUFFER* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_CREATE_BITSTREAM_BUFFER_VER) {
        violation("NvEncCreateBitstreamBuffer: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (!s->initialized) violation("NvEncCreateBitstreamBuffer before NvEncInitializeEncoder");
    p->bitstreamBuffer = newHandle();
    s->buffers.insert(p->bitstreamBuffer);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI destroyBitstreamBuffer(void* enc, NV_ENC_OUTPUT_PTR b) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!s->buffers.erase(b)) {
        violation("NvEncDestroyBitstreamBuffer of an unknown buffer");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    for (const Frame& f : s->pending) {
        if (f.output == b && f.locked) violation("bitstream buffer destroyed while locked");
    }
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI registerAsyncEvent(void* enc, NV_ENC_EVENT_PARAMS* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_EVENT_PARAMS_VER) {
        violation("NvEncRegisterAsyncEvent: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (!s->init.enableEncodeAsync) violation("NvEncRegisterAsyncEvent in sync mode");
    if (!p->completionEvent || !s->events.insert(static_cast<HANDLE>(p->completionEvent)).second) {
        violation("NvEncRegisterAsyncEvent: null or already registered event");
        return NV_ENC_ERR_INVALID_EVENT;
    }
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI unregisterAsyncEvent(void* enc, NV_ENC_EVENT_PARAMS* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_EVENT_PARAMS_VER) {
        violation("NvEncUnregisterAsyncEvent: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (!s->events.erase(static_cast<HANDLE>(p->completionEvent))) {
        violation("NvEncUnregisterAsyncEvent of an unregistered event");
        return NV_ENC_ERR_INVALID_EVENT;
    }
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI registerResource(void* enc, NV_ENC_REGISTER_RESOURCE* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_REGISTER_RESOURCE_VER) {
        violation("NvEncRegisterResource: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (p->resourceType != NV_ENC_INPUT_RESOURCE_TYPE_DIRECTX || p->bufferUsage != NV_ENC_INPUT_IMAGE ||
        p->bufferFormat != NV_ENC_BUFFER_FORMAT_NV12 || p->pitch != 0 || !p->resourceToRegister) {
        violation("NvEncRegisterResource: type %d usage %d format %d pitch %u resource %p", int(p->resourceType), int(p->bufferUsage),
                  int(p->bufferFormat), p->pitch, p->resourceToRegister);
        return NV_ENC_ERR_INVALID_PARAM;
    }
    for (const auto& [h, tex] : s->regs) {
        if (tex == p->resourceToRegister) violation("NvEncRegisterResource: texture %p registered twice", tex);
    }
    p->registeredResource = newHandle();
    s->regs[p->registeredResource] = p->resourceToRegister;
    logLine("register %ux%u", p->width, p->height);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI unregisterResource(void* enc, NV_ENC_REGISTERED_PTR r) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    for (const auto& [m, reg] : s->mapped) {
        if (reg == r) violation("NvEncUnregisterResource while mapped");
    }
    if (!s->regs.erase(r)) {
        violation("NvEncUnregisterResource of an unregistered resource");
        return NV_ENC_ERR_RESOURCE_NOT_REGISTERED;
    }
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI mapInputResource(void* enc, NV_ENC_MAP_INPUT_RESOURCE* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_MAP_INPUT_RESOURCE_VER) {
        violation("NvEncMapInputResource: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (!s->regs.count(p->registeredResource)) {
        violation("NvEncMapInputResource of an unregistered resource");
        return NV_ENC_ERR_RESOURCE_NOT_REGISTERED;
    }
    for (const auto& [m, reg] : s->mapped) {
        if (reg == p->registeredResource) violation("NvEncMapInputResource: resource mapped twice (still in use)");
    }
    p->mappedResource = newHandle();
    p->mappedBufferFmt = NV_ENC_BUFFER_FORMAT_NV12;
    s->mapped[p->mappedResource] = p->registeredResource;
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI unmapInputResource(void* enc, NV_ENC_INPUT_PTR m) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!s->mapped.erase(m)) {
        violation("NvEncUnmapInputResource of an unmapped resource");
        return NV_ENC_ERR_RESOURCE_NOT_MAPPED;
    }
    for (const Frame& f : s->pending) {
        // "The client must unmap the buffer after NvEncLockBitstream() API
        // returns successfully for encode work submitted using the mapped input buffer."
        if (f.mapped == m) violation("input of frame %llu unmapped before its NvEncLockBitstream", static_cast<unsigned long long>(f.ts));
    }
    return NV_ENC_SUCCESS;
}

void invalidateLocked(Session& s, uint64_t ts) {
    for (DpbEntry& e : s.dpb) {
        if (e.ts == ts) e.invalid = true;
    }
    // "any frames which have been reconstructed using the corrupt frame"
    for (DpbEntry& e : s.dpb) {
        if (e.ref) {
            for (const DpbEntry& r : s.dpb) {
                if (r.ts == e.ref && r.invalid) e.invalid = true;
            }
        }
    }
}

NVENCSTATUS NVENCAPI encodePicture(void* enc, NV_ENC_PIC_PARAMS* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_PIC_PARAMS_VER) {
        violation("NvEncEncodePicture: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    if (!s->initialized) return NV_ENC_ERR_ENCODER_NOT_INITIALIZED;
    const bool async = s->init.enableEncodeAsync != 0;
    const int64_t t = now();
    if (p->encodePicFlags & NV_ENC_PIC_FLAG_EOS) {
        if (p->inputBuffer || p->outputBitstream) violation("EOS with an input or output buffer");
        if (async && !s->events.count(static_cast<HANDLE>(p->completionEvent))) violation("EOS in async mode without a registered event");
        s->eos = true;
        int64_t done = t;
        for (const Frame& f : s->pending) done = std::max(done, f.doneAt);
        if (async) schedule(done, static_cast<HANDLE>(p->completionEvent));
        logLine("eos");
        return NV_ENC_SUCCESS;
    }
    s->encodeThread = GetCurrentThreadId();
    if (s->seqThread && s->seqThread != s->encodeThread) {
        violation("NvEncEncodePicture on another thread than NvEncGetSequenceParams");
        s->seqThread = 0;  // once
    }
    if (!s->mapped.count(p->inputBuffer)) {
        violation("NvEncEncodePicture: input not mapped");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    if (!s->buffers.count(p->outputBitstream)) {
        violation("NvEncEncodePicture: unknown output buffer");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    for (const Frame& f : s->pending) {
        if (f.output == p->outputBitstream) violation("output buffer reused while frame %llu is pending", static_cast<unsigned long long>(f.ts));
        if (async && f.event == p->completionEvent) violation("completion event reused while frame %llu is pending", static_cast<unsigned long long>(f.ts));
    }
    if (async && !s->events.count(static_cast<HANDLE>(p->completionEvent))) violation("async mode: completion event not registered");
    if (!async && p->completionEvent) violation("sync mode with a completion event");
    if (p->inputTimeStamp <= s->lastTs) violation("inputTimeStamp %llu not increasing", static_cast<unsigned long long>(p->inputTimeStamp));
    if (p->inputWidth != s->init.encodeWidth || p->inputHeight != s->init.encodeHeight || p->bufferFmt != NV_ENC_BUFFER_FORMAT_NV12 ||
        p->pictureStruct != NV_ENC_PIC_STRUCT_FRAME) {
        violation("NvEncEncodePicture: input %ux%u format %d structure %d", p->inputWidth, p->inputHeight, int(p->bufferFmt),
                  int(p->pictureStruct));
    }
    int qpNonZero = 0, qpMin = 0;
    if (p->qpDeltaMap) {
        const uint32_t b = qpBlock(s->codec);
        const uint32_t need = ((s->init.encodeWidth + b - 1) / b) * ((s->init.encodeHeight + b - 1) / b);
        if (s->config.rcParams.qpMapMode != NV_ENC_QP_MAP_DELTA) violation("qpDeltaMap without NV_ENC_QP_MAP_DELTA");
        if (p->qpDeltaMapSize != need) violation("qpDeltaMapSize %u, %u blocks", p->qpDeltaMapSize, need);
        for (uint32_t i = 0; i < std::min(need, p->qpDeltaMapSize); ++i) {
            qpNonZero += p->qpDeltaMap[i] != 0;
            qpMin = std::min(qpMin, int(p->qpDeltaMap[i]));
        }
    }
    if (g_cfg.failEncodeTs && p->inputTimeStamp == g_cfg.failEncodeTs) {
        logLine("encode ts=%llu failed (injected)", static_cast<unsigned long long>(p->inputTimeStamp));
        return NV_ENC_ERR_GENERIC;
    }
    s->lastTs = p->inputTimeStamp;
    Frame f;
    f.ts = p->inputTimeStamp;
    f.output = p->outputBitstream;
    f.event = static_cast<HANDLE>(p->completionEvent);
    f.mapped = p->inputBuffer;
    f.frameIdx = p->frameIdx;
    // One engine: frames finish one after another.
    f.doneAt = std::max(t, s->lastDone) + int64_t(g_cfg.encodeUs) * g_freq / 1000000;
    s->lastDone = f.doneAt;
    const bool forced = (p->encodePicFlags & NV_ENC_PIC_FLAG_FORCEIDR) || s->forceIdrNext || s->encoded == 0;
    s->forceIdrNext = false;
    if (forced) {
        f.type = NV_ENC_PIC_TYPE_IDR;
        s->dpb.clear();
    } else {
        f.type = NV_ENC_PIC_TYPE_I;
        for (auto it = s->dpb.rbegin(); it != s->dpb.rend(); ++it) {
            if (!it->invalid) {
                f.type = NV_ENC_PIC_TYPE_P;
                f.ref = it->ts;
                break;
            }
        }
        if (f.type == NV_ENC_PIC_TYPE_I) logLine("intra fallback ts=%llu: no valid reference left", static_cast<unsigned long long>(f.ts));
    }
    s->dpb.push_back({f.ts, f.ref, false});
    while (s->dpb.size() > std::max<uint32_t>(1, dpbSize(*s))) s->dpb.pop_front();
    const bool headers = f.type == NV_ENC_PIC_TYPE_IDR &&
                         ((p->encodePicFlags & NV_ENC_PIC_FLAG_OUTPUT_SPSPPS) ||
                          (s->codec == Codec::H264 && s->config.encodeCodecConfig.h264Config.repeatSPSPPS) ||
                          (s->codec == Codec::Hevc && s->config.encodeCodecConfig.hevcConfig.repeatSPSPPS) ||
                          (s->codec == Codec::Av1 && s->config.encodeCodecConfig.av1Config.repeatSeqHdr));
    f.data = bitstream(*s, f, headers);
    ++s->encoded;
    s->history.push_back(f.ts);
    logLine("encode ts=%llu flags=%u idx=%u type=%s ref=%llu qpmap=%u/%d/%d", static_cast<unsigned long long>(f.ts), p->encodePicFlags,
            p->frameIdx, f.type == NV_ENC_PIC_TYPE_IDR ? "IDR" : f.type == NV_ENC_PIC_TYPE_I ? "I" : "P",
            static_cast<unsigned long long>(f.ref), p->qpDeltaMap ? p->qpDeltaMapSize : 0u, qpNonZero, qpMin);
    if (async) schedule(f.doneAt, f.event);
    s->pending.push_back(std::move(f));
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI lockBitstream(void* enc, NV_ENC_LOCK_BITSTREAM* p) {
    int64_t waitUntil = 0;
    {
        std::lock_guard<std::mutex> lock(g_mu);
        Session* s = session(enc);
        if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
        if (p->version != NV_ENC_LOCK_BITSTREAM_VER) {
            violation("NvEncLockBitstream: struct version %08x", p->version);
            return NV_ENC_ERR_INVALID_VERSION;
        }
        auto it = std::find_if(s->pending.begin(), s->pending.end(), [&](const Frame& f) { return f.output == p->outputBitstream; });
        if (it == s->pending.end()) {
            violation("NvEncLockBitstream of a buffer without a pending frame");
            return NV_ENC_ERR_INVALID_PARAM;
        }
        if (it != s->pending.begin()) violation("NvEncLockBitstream out of submission order (frame %llu)", static_cast<unsigned long long>(it->ts));
        if (it->locked) violation("NvEncLockBitstream twice");
        const bool async = s->init.enableEncodeAsync != 0;
        const bool done = now() >= it->doneAt;
        if (!done && async) {
            // "Before the locking the output buffers in the secondary thread,
            // the client must wait on NV_ENC_PIC_PARAMS::completionEvent".
            violation("async mode: frame %llu locked before its completion event", static_cast<unsigned long long>(it->ts));
            return NV_ENC_ERR_LOCK_BUSY;
        }
        if (!done && p->doNotWait) {
            logLine("lock busy ts=%llu", static_cast<unsigned long long>(it->ts));
            return NV_ENC_ERR_LOCK_BUSY;
        }
        if (!done) waitUntil = it->doneAt;
    }
    while (waitUntil && now() < waitUntil) Sleep(1);
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    auto it = std::find_if(s->pending.begin(), s->pending.end(), [&](const Frame& f) { return f.output == p->outputBitstream; });
    if (it == s->pending.end()) return NV_ENC_ERR_INVALID_PARAM;
    it->locked = true;
    p->bitstreamBufferPtr = it->data.data();
    p->bitstreamSizeInBytes = uint32_t(it->data.size());
    p->pictureType = it->type;
    p->pictureStruct = NV_ENC_PIC_STRUCT_FRAME;
    p->outputTimeStamp = it->ts;
    p->frameIdx = it->frameIdx;
    p->temporalId = 0;
    logLine("lock ts=%llu donotwait=%u", static_cast<unsigned long long>(it->ts), p->doNotWait);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI unlockBitstream(void* enc, NV_ENC_OUTPUT_PTR b) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    auto it = std::find_if(s->pending.begin(), s->pending.end(), [&](const Frame& f) { return f.output == b; });
    if (it == s->pending.end() || !it->locked) {
        violation("NvEncUnlockBitstream of a buffer that is not locked");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    s->pending.erase(it);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI invalidateRefFrames(void* enc, uint64_t ts) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (!g_cfg.rfi) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    const bool known = std::find(s->history.begin(), s->history.end(), ts) != s->history.end();
    bool pending = false;
    for (const Frame& f : s->pending) pending = pending || (f.ts == ts && now() < f.doneAt);
    invalidateLocked(*s, ts);
    logLine("invalidate ts=%llu%s%s", static_cast<unsigned long long>(ts), known ? "" : " (never encoded)",
            pending ? " (still encoding)" : "");
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI reconfigureEncoder(void* enc, NV_ENC_RECONFIGURE_PARAMS* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_RECONFIGURE_PARAMS_VER || p->reInitEncodeParams.version != NV_ENC_INITIALIZE_PARAMS_VER ||
        (p->reInitEncodeParams.encodeConfig && p->reInitEncodeParams.encodeConfig->version != NV_ENC_CONFIG_VER)) {
        violation("NvEncReconfigureEncoder: struct versions");
        return NV_ENC_ERR_INVALID_VERSION;
    }
    const NV_ENC_INITIALIZE_PARAMS& n = p->reInitEncodeParams;
    const NV_ENC_INITIALIZE_PARAMS& o = s->init;
    // "Currently Reconfiguration of following are not supported. Change in GOP
    // structure. Change in sync-Async mode. Change in MaxWidth & MaxHeight.
    // Change in PTD mode."
    if (n.enableEncodeAsync != o.enableEncodeAsync || n.enablePTD != o.enablePTD || n.maxEncodeWidth != o.maxEncodeWidth ||
        n.maxEncodeHeight != o.maxEncodeHeight || std::memcmp(&n.encodeGUID, &o.encodeGUID, sizeof(GUID))) {
        violation("NvEncReconfigureEncoder changes a parameter that cannot be reconfigured");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    if ((n.encodeWidth != o.encodeWidth || n.encodeHeight != o.encodeHeight) &&
        (!o.maxEncodeWidth || n.encodeWidth > o.maxEncodeWidth || n.encodeHeight > o.maxEncodeHeight)) {
        violation("resolution change without maxEncodeWidth/Height room");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    const NV_ENC_CONFIG& c = n.encodeConfig ? *n.encodeConfig : s->config;
    if (c.gopLength != s->config.gopLength || c.frameIntervalP != s->config.frameIntervalP) {
        violation("NvEncReconfigureEncoder changes the GOP structure");
        return NV_ENC_ERR_INVALID_PARAM;
    }
    const bool rateChange = c.rcParams.averageBitRate != s->config.rcParams.averageBitRate ||
                            c.rcParams.maxBitRate != s->config.rcParams.maxBitRate ||
                            c.rcParams.vbvBufferSize != s->config.rcParams.vbvBufferSize;
    if (rateChange && !g_cfg.dynBitrate) return NV_ENC_ERR_UNSUPPORTED_PARAM;
    if (p->resetEncoder && !p->forceIDR) violation("resetEncoder without forceIDR (\"should be used only with an IDR frame\")");
    s->config = c;
    s->init = n;
    s->init.encodeConfig = &s->config;
    if (p->forceIDR) s->forceIdrNext = true;
    logLine("reconfigure reset=%u forceidr=%u avg=%u max=%u vbv=%u fps=%u/%u size=%ux%u", p->resetEncoder, p->forceIDR,
            c.rcParams.averageBitRate, c.rcParams.maxBitRate, c.rcParams.vbvBufferSize, n.frameRateNum, n.frameRateDen, n.encodeWidth,
            n.encodeHeight);
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI getSequenceParams(void* enc, NV_ENC_SEQUENCE_PARAM_PAYLOAD* p) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    if (p->version != NV_ENC_SEQUENCE_PARAM_PAYLOAD_VER) {
        violation("NvEncGetSequenceParams: struct version %08x", p->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    // "The client must call NvEncGetSequenceParams() function from the same
    // thread which is being used to call NvEncEncodePicture() function."
    s->seqThread = GetCurrentThreadId();
    if (s->encodeThread && s->encodeThread != s->seqThread) violation("NvEncGetSequenceParams from another thread than NvEncEncodePicture");
    const std::string ps = parameterSets(*s);
    if (!p->spsppsBuffer || p->inBufferSize < ps.size() || !p->outSPSPPSPayloadSize) return NV_ENC_ERR_NOT_ENOUGH_BUFFER;
    std::memcpy(p->spsppsBuffer, ps.data(), ps.size());
    *p->outSPSPPSPayloadSize = uint32_t(ps.size());
    logLine("sequence params");
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI destroyEncoder(void* enc) {
    std::lock_guard<std::mutex> lock(g_mu);
    Session* s = session(enc);
    if (!s) return NV_ENC_ERR_INVALID_ENCODERDEVICE;
    // "The client must flush the encoder before freeing any resources ...
    // must free all the input and output resources ... must also unregister the
    // completion events" (NvEncDestroyEncoder); "all mapped input buffer
    // handles are unmapped" (guide 5.3).
    if (s->encoded && !s->eos) violation("NvEncDestroyEncoder without EOS after %llu frames", static_cast<unsigned long long>(s->encoded));
    if (!s->buffers.empty()) violation("NvEncDestroyEncoder with %zu bitstream buffers", s->buffers.size());
    if (!s->events.empty()) violation("NvEncDestroyEncoder with %zu registered events", s->events.size());
    if (!s->regs.empty()) violation("NvEncDestroyEncoder with %zu registered resources", s->regs.size());
    if (!s->mapped.empty()) violation("NvEncDestroyEncoder with %zu mapped inputs", s->mapped.size());
    logLine("destroy encoded=%llu", static_cast<unsigned long long>(s->encoded));
    g_sessions.erase(s);
    delete s;
    return NV_ENC_SUCCESS;
}

const char* NVENCAPI lastError(void*) { return ""; }

bool setKey(const std::string& k, const std::string& v) {
    auto i = [&](int& out) {
        out = std::atoi(v.c_str());
        return true;
    };
    if (k == "maxVersion") {
        // "13.0" -> (13 << 4) | 0
        char* end = nullptr;
        const unsigned long major = std::strtoul(v.c_str(), &end, 10);
        if (!end || *end != '.') return false;
        const unsigned long minor = std::strtoul(end + 1, &end, 10);
        if (!end || *end != '\0' || minor > 15) return false;
        g_cfg.maxVersion = uint32_t((major << 4) | minor);
        return true;
    }
    if (k == "failEncodeTs") {
        g_cfg.failEncodeTs = std::strtoull(v.c_str(), nullptr, 10);
        return true;
    }
    const std::pair<const char*, int*> ints[] = {
        {"h264", &g_cfg.h264}, {"hevc", &g_cfg.hevc}, {"av1", &g_cfg.av1}, {"async", &g_cfg.async}, {"rfi", &g_cfg.rfi},
        {"multiRef", &g_cfg.multiRef}, {"dynBitrate", &g_cfg.dynBitrate}, {"dynRes", &g_cfg.dynRes}, {"svc", &g_cfg.svc},
        {"maxLayers", &g_cfg.maxLayers}, {"ltr", &g_cfg.ltr}, {"engines", &g_cfg.engines}, {"emphasis", &g_cfg.emphasis},
        {"tenBit", &g_cfg.tenBit}, {"yuv444", &g_cfg.yuv444}, {"customVbv", &g_cfg.customVbv}, {"intraRefresh", &g_cfg.intraRefresh},
        {"cabac", &g_cfg.cabac}, {"subframe", &g_cfg.subframe}, {"stateAdvance", &g_cfg.stateAdvance}, {"maxW", &g_cfg.maxW},
        {"maxH", &g_cfg.maxH}, {"minW", &g_cfg.minW}, {"minH", &g_cfg.minH}, {"encodeUs", &g_cfg.encodeUs},
        {"keepRefs", &g_cfg.keepRefs}, {"padToRate", &g_cfg.padToRate},
    };
    for (const auto& [name, ptr] : ints) {
        if (k == name) return i(*ptr);
    }
    return false;
}

std::string join(const std::vector<std::string>& v) {
    std::string out;
    for (const std::string& s : v) out += s + "\n";
    return out;
}

}  // namespace

extern "C" {

NVENCSTATUS NVENCAPI NvEncodeAPIGetMaxSupportedVersion(uint32_t* version) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!version) return NV_ENC_ERR_INVALID_PTR;
    *version = g_cfg.maxVersion;
    return NV_ENC_SUCCESS;
}

NVENCSTATUS NVENCAPI NvEncodeAPICreateInstance(NV_ENCODE_API_FUNCTION_LIST* list) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (!list) return NV_ENC_ERR_INVALID_PTR;
    if (list->version != NV_ENCODE_API_FUNCTION_LIST_VER) {
        violation("NvEncodeAPICreateInstance: struct version %08x", list->version);
        return NV_ENC_ERR_INVALID_VERSION;
    }
    LARGE_INTEGER f;
    QueryPerformanceFrequency(&f);
    g_freq = f.QuadPart;
    const uint32_t version = list->version;
    std::memset(list, 0, sizeof(*list));
    list->version = version;
    list->nvEncOpenEncodeSessionEx = openSessionEx;
    list->nvEncGetEncodeGUIDCount = getEncodeGuidCount;
    list->nvEncGetEncodeGUIDs = getEncodeGuids;
    list->nvEncGetEncodeCaps = getEncodeCaps;
    list->nvEncGetInputFormatCount = getInputFormatCount;
    list->nvEncGetInputFormats = getInputFormats;
    list->nvEncGetEncodePresetConfigEx = getPresetConfigEx;
    list->nvEncInitializeEncoder = initializeEncoder;
    list->nvEncCreateBitstreamBuffer = createBitstreamBuffer;
    list->nvEncDestroyBitstreamBuffer = destroyBitstreamBuffer;
    list->nvEncRegisterAsyncEvent = registerAsyncEvent;
    list->nvEncUnregisterAsyncEvent = unregisterAsyncEvent;
    list->nvEncRegisterResource = registerResource;
    list->nvEncUnregisterResource = unregisterResource;
    list->nvEncMapInputResource = mapInputResource;
    list->nvEncUnmapInputResource = unmapInputResource;
    list->nvEncEncodePicture = encodePicture;
    list->nvEncLockBitstream = lockBitstream;
    list->nvEncUnlockBitstream = unlockBitstream;
    list->nvEncInvalidateRefFrames = invalidateRefFrames;
    list->nvEncReconfigureEncoder = reconfigureEncoder;
    list->nvEncGetSequenceParams = getSequenceParams;
    list->nvEncDestroyEncoder = destroyEncoder;
    list->nvEncGetLastErrorString = lastError;
    return NV_ENC_SUCCESS;
}

// The self-test's control: returns the reply length (or -1 for an unknown
// command / a failed "set"); the reply is truncated to size - 1 bytes.
int ReconFakeNvencControl(const char* command, char* reply, int size) {
    std::lock_guard<std::mutex> lock(g_mu);
    const std::string cmd = command ? command : "";
    std::string out;
    if (cmd == "reset") {
        g_cfg = Config{};
        g_log.clear();
        g_violations.clear();
    } else if (cmd.rfind("set ", 0) == 0) {
        std::istringstream in(cmd.substr(4));
        std::string kv;
        while (in >> kv) {
            const size_t eq = kv.find('=');
            if (eq == std::string::npos || !setKey(kv.substr(0, eq), kv.substr(eq + 1))) return -1;
        }
    } else if (cmd == "log") {
        out = join(g_log);
    } else if (cmd == "clearlog") {
        g_log.clear();
    } else if (cmd == "violations") {
        out = join(g_violations);
    } else if (cmd == "stats") {
        size_t buffers = 0, events = 0, regs = 0, mapped = 0;
        for (const Session* s : g_sessions) {
            buffers += s->buffers.size();
            events += s->events.size();
            regs += s->regs.size();
            mapped += s->mapped.size();
        }
        char b[160];
        std::snprintf(b, sizeof(b), "sessions=%zu buffers=%zu events=%zu registered=%zu mapped=%zu", g_sessions.size(), buffers, events,
                      regs, mapped);
        out = b;
    } else {
        return -1;
    }
    if (reply && size > 0) {
        const size_t n = std::min(out.size(), size_t(size - 1));
        std::memcpy(reply, out.data(), n);
        reply[n] = '\0';
    }
    return int(out.size());
}

}  // extern "C"
