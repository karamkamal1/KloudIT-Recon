// Types shared by the control protocol, the pipeline and the backends.
// docs/HELPER_PROTOCOL.md is the reference for every field.
#pragma once

#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace recon {

// Version of the control protocol and of the shared-memory ring layout
// (docs/HELPER_PROTOCOL.md). Bump on any incompatible change.
constexpr int kProtocolVersion = 1;

// Largest control message accepted in either direction (same as proto.MaxControlMsg).
constexpr uint32_t kMaxControlMsg = 1u << 20;

// Process exit codes.
constexpr int kExitOk = 0;
constexpr int kExitUsage = 2;  // bad arguments or the ring could not be attached
constexpr int kExitFatal = 3;  // a fatal runtime error was reported
constexpr int kExitStuck = 4;  // the threads did not stop in time after the helper decided to exit

// Status is the result of an operation that can fail. Non-fatal errors are
// reported to Go and the helper keeps running; a fatal error ends the helper
// (Go restarts it and asks for an IDR).
struct Status {
    bool ok = true;
    std::string code;  // machine-readable, e.g. "unsupported" (see docs/HELPER_PROTOCOL.md)
    std::string text;  // human-readable
    bool fatal = false;

    static Status Ok() { return {}; }
    static Status Error(std::string code, std::string text, bool fatal = false) {
        Status s;
        s.ok = false;
        s.code = std::move(code);
        s.text = std::move(text);
        s.fatal = fatal;
        return s;
    }
};

// BarcodeLayout places an in-band barcode of the frame id into every encoded
// frame (GUIDE 0.2): bit k of the frame id (k = bits-1 .. 0 with msbFirst) is
// block k, blocks run left to right, `cols` per row, top to bottom. A block is
// luma 235 (bit 1) or 16 (bit 0) with neutral chroma, drawn in output pixels
// (after scaling). x, y, blockW and blockH are even so every block covers whole
// 4:2:0 chroma samples.
struct BarcodeLayout {
    bool enabled = false;
    int x = 0, y = 0;            // top-left corner in output pixels
    int blockW = 8, blockH = 8;  // block size in output pixels
    int cols = 32;               // blocks per row
    int bits = 32;               // low bits of the frame id drawn (1..64)
    bool msbFirst = true;        // first block = most significant bit

    int rows() const { return (bits + cols - 1) / cols; }
};

// StartParams is the "start" control message.
struct StartParams {
    std::string capture;  // "dda" | "amd-direct" | "wgc" | "synthetic" | "synthetic-gpu"; empty = backend default
    // Monitor selection, in this order: hmonitor; adapterLuid + monitor (output
    // index on that adapter); monitor alone (output index on DXGI adapter 0,
    // like FFmpeg ddagrab's output_idx).
    int monitor = 0;
    uint64_t hmonitor = 0;    // HMONITOR, 0 = not set
    std::string adapterLuid;  // "%08x:%08x" (HighPart:LowPart) as in caps, empty = adapter 0
    // Window capture (WGC only): a top-level window handle, or the first
    // visible top-level window whose title contains windowTitle (case-insensitive).
    uint64_t window = 0;
    std::string windowTitle;
    std::string gpuPriority = "auto";  // "auto" | "high" | "realtime" | "off" (GUIDE 1.3)
    int idleRepeatMs = 100;            // re-submit the last image after this long without a new one
    BarcodeLayout barcode;
    std::string codec;    // "h264" | "hevc" | "av1"
    int width = 0;        // 0 = capture size
    int height = 0;
    int fps = 60;
    int kbps = 10000;
    double vbvFrames = 1.0;     // VBV buffer in frame intervals
    std::string rc = "cbr";     // "cbr" | "vbr" (the backend picks its low-latency flavour)
    std::string quality = "speed";  // "speed" | "balanced" | "quality"
    bool hdr = false;
    int ltrSlots = 0;   // long-term reference slots to reserve (ACK-based recovery, 3.5)
    int svcLayers = 1;  // temporal layers
    // Encoder knobs (optional; defaults are the backend's).
    std::string liveBitrate;     // "" = the codec's caps liveBitrate | "seamless" | "flush" (step 3.6 tests both)
    int encoderInstance = -1;    // hardware encoder engine (AMF INSTANCE_INDEX), -1 = backend default
    int ltrInterval = 0;         // frames between LTR marks, 0 = fps/10 (about 100 ms)
    int intraRefreshFrames = 0;  // intra refresh cycle in frames, 0 = off (not with ltrSlots or svcLayers > 1)
    bool zeroCopy = true;        // AMD Direct Capture surfaces go to the AMF encoder unconverted when possible
};

// RateParams is the "setRate" control message; fps 0 = unchanged.
struct RateParams {
    int kbps = 0;
    double vbvFrames = 0;
    int fps = 0;
};

struct RoiRect {
    int x = 0, y = 0, w = 0, h = 0;
    int weight = 0;  // importance, backend-scaled (AMF 0..10, NVENC emphasis)
};

// OutputDesc describes one display output (caps "outputs").
struct OutputDesc {
    int index = 0;                     // position in caps "outputs" (adapters in DXGI order, then their outputs)
    int adapterIndex = 0, outputIndex = 0;  // DXGI EnumAdapters1 / EnumOutputs indexes
    std::string adapterLuid, adapterName, vendor;
    std::string name;                  // GDI device name, e.g. \\.\DISPLAY1
    uint64_t hmonitor = 0;
    int x = 0, y = 0, width = 0, height = 0;  // desktop coordinates (as displayed, i.e. rotated)
    int rotation = 0;                  // 0 | 90 | 180 | 270
    bool attached = false;             // attached to the desktop
};

// CodecCaps is one entry of Caps::codecs (GUIDE Arch-2).
struct CodecCaps {
    int maxW = 0, maxH = 0;
    bool tenBit = false;
    bool yuv444 = false;
    bool forceIdr = false;
    std::string recovery = "none";  // "ltr" | "invalidate" | "none"
    int maxLtr = 0;
    bool intraRefresh = false;
    std::string liveBitrate = "restart";  // "seamless" | "flush" | "restart"
    int maxTemporalLayers = 1;
    std::string roi = "none";  // "importance" | "emphasis" | "none"
    bool sliceOutput = false;
    int hwInstances = 1;
    bool queryTimeout = false;
    int alignW = 1, alignH = 1;  // required coded-size alignment (AV1 on RDNA3: 64x16)
    // The running encoder can change its coded size without a new session
    // (NVENC NV_ENC_CAPS_SUPPORT_DYN_RES_CHANGE; the helper has no control
    // message for it yet: GUIDE 5 "FPS before resolution").
    bool dynamicResolution = false;
    // Fields above that are documented or default values rather than detected
    // on this GPU (e.g. AMF AV1 "roi": there is no ROI cap; "liveBitrate" until
    // step 3.6 measures it). Sent only when not empty.
    std::vector<std::string> assumed;
};

// Caps is sent once, right after start-up (GUIDE Arch-2 shape plus diagnostics).
struct Caps {
    std::string backend = "none";  // selected encoder backend, "none" if nothing usable
    std::string vendor = "other";  // "amd" | "nvidia" | "intel" | "other" | "mock"
    std::string adapterLuid;       // "high:low" hex, empty if unknown
    std::string adapterName;
    // Hardware-accelerated GPU scheduling on that adapter
    // (D3DKMTQueryAdapterInfo); nullopt = could not be detected (sent as null).
    std::optional<bool> hagsEnabled;
    std::map<std::string, CodecCaps> codecs;
    std::vector<std::string> capture;  // usable capture backends, default first
    // The captured video contains the mouse pointer. Frames of every listed
    // capture method are without it (DDA and AMD Direct Capture never include
    // it; WGC is only listed where it can exclude it), so recon-host draws the
    // cursor on the client. Started::cursorInVideo is the per-stream answer.
    bool cursorInVideo = false;
    std::vector<OutputDesc> outputs;
    // Backends and capture methods that were probed and are not usable, with
    // the reason (missing runtime DLL, not implemented yet, ...).
    std::vector<std::pair<std::string, std::string>> unavailable;
};

// Started answers a successful "start".
struct Started {
    std::string backend, capture, codec;
    int width = 0, height = 0, fps = 0, kbps = 0;
    int captureWidth = 0, captureHeight = 0;  // what the capture delivers (as displayed)
    std::string adapterLuid, adapterName, vendor;  // the capture/encode adapter ("" for synthetic)
    std::optional<bool> hagsEnabled;
    std::string gpuPriority;  // "realtime" | "high" | "failed" | "off" | "" (no GPU)
    int idleRepeatMs = 0;
    bool barcode = false;
    bool cursorInVideo = false;  // this stream's frames contain the pointer (SourceInfo::cursorInVideo)
    // The bitstream's frame size when it differs from width x height (AV1 on
    // RDNA3 is coded in 64x16 multiples: the picture is in the top-left
    // width x height, the rest is padding to crop). 0 = width / height.
    int codedWidth = 0, codedHeight = 0;
    std::string liveBitrate;     // "seamless" | "flush" (how setRate is applied)
    std::string rateControl;     // what the encoder runs, e.g. "cbr" | "vbr_latency"
    std::string usage;           // encoder usage, e.g. AMF "ultra_low_latency" (H.264 may fall back to "low_latency")
    int ltrSlots = 0;            // LTR slots in use (0 = recovery by IDR)
    int ltrInterval = 0;         // frames between LTR marks
    int encoderInstance = 0;     // hardware engine used
    int hwInstances = 1;         // hardware engines the GPU has for this codec
    int queryTimeoutMs = 0;      // the encoder's blocking output wait (0 = polled)
    bool zeroCopy = false;       // capture surfaces go to the encoder without the NV12 conversion
    int intraRefreshFrames = 0;
    std::string preset;          // NVENC preset "p1".."p7" ("" for other backends)
    bool asyncEncode = false;    // NVENC: completion events (async mode), false = polled output (sync mode)
    int refFrames = 0;           // reference frames the encoder keeps (NVENC DPB size; 0 = not reported)
};

// CaptureEvent is the helper -> Go "captureChanged" message.
struct CaptureEvent {
    // "resized": the source changed size or rotation (the stream continues at
    // the old encoded size, scaled; recon-host may restart the helper to
    // follow). "lost": capture is unavailable (secure desktop, output gone, mode
    // switch in progress); the last image is repeated. "restored": capture works again.
    std::string reason;
    int width = 0, height = 0, rotation = 0;  // the source as now displayed (resized/restored)
    std::string text;
};

// FrameStats is the per-frame "stats" message (also sent for dropped frames).
struct FrameStats {
    uint64_t frameId = 0;
    uint32_t gen = 0;
    bool dropped = false;
    std::string dropReason;  // "ringFull" | "tooLarge"
    bool key = false;
    bool recovery = false;
    bool repeat = false;  // idle re-submit of the previous image (nothing new on screen)
    int dirtyPct = -1;    // share of the image the capture reported as changed, -1 = unknown
    uint64_t bytes = 0;
    int64_t presentQpc = 0, captureQpc = 0, submitQpc = 0, outputQpc = 0;
    uint64_t refFloor = 0;
    int32_t ltrSlot = -1;
    uint32_t temporalLayer = 0;
    uint32_t refLtrMask = 0;
    RateParams rate;           // current target (as last set)
    uint64_t ringDropped = 0;  // total frames dropped by the helper so far
};

// ControlMsg is a parsed Go -> helper message; only the fields of its type are set.
struct ControlMsg {
    std::string type;  // "start" | "forceIdr" | "recover" | "setRate" | "setRoi" | "ack" | "shutdown"
    StartParams start;
    uint64_t lostFromFrameId = 0;
    std::optional<uint64_t> ackedLtrFrameId;
    RateParams rate;
    std::vector<RoiRect> rects;
    uint64_t ackFrameId = 0;  // "ack"
};

}  // namespace recon
