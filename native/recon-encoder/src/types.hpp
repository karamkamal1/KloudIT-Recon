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

// StartParams is the "start" control message.
struct StartParams {
    std::string capture;  // "dda" | "amd-direct" | "wgc" | "synthetic"; empty = backend default
    int monitor = 0;      // DXGI output index
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
};

// Caps is sent once, right after start-up (GUIDE Arch-2 shape plus diagnostics).
struct Caps {
    std::string backend = "none";  // selected encoder backend, "none" if nothing usable
    std::string vendor = "other";  // "amd" | "nvidia" | "intel" | "other" | "mock"
    std::string adapterLuid;       // "high:low" hex, empty if unknown
    std::string adapterName;
    // Hardware-accelerated GPU scheduling; nullopt = not detected (sent as
    // null). Not detected yet: step 3.2 adds D3DKMTQueryAdapterInfo.
    std::optional<bool> hagsEnabled;
    std::map<std::string, CodecCaps> codecs;
    std::vector<std::string> capture;  // usable capture backends, default first
    // Backends and capture methods that were probed and are not usable, with
    // the reason (missing runtime DLL, not implemented yet, ...).
    std::vector<std::pair<std::string, std::string>> unavailable;
};

// Started answers a successful "start".
struct Started {
    std::string backend, capture, codec;
    int width = 0, height = 0, fps = 0, kbps = 0;
};

// FrameStats is the per-frame "stats" message (also sent for dropped frames).
struct FrameStats {
    uint64_t frameId = 0;
    uint32_t gen = 0;
    bool dropped = false;
    std::string dropReason;  // "ringFull" | "tooLarge"
    bool key = false;
    bool recovery = false;
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
    std::string type;  // "start" | "forceIdr" | "recover" | "setRate" | "setRoi" | "shutdown"
    StartParams start;
    uint64_t lostFromFrameId = 0;
    std::optional<uint64_t> ackedLtrFrameId;
    RateParams rate;
    std::vector<RoiRect> rects;
};

}  // namespace recon
