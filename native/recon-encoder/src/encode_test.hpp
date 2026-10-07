// recon-encoder --encode-test=FILE: one stream through the real capture,
// conversion, encoder backend and frame ring, without recon-host, with
// scripted control events (forced IDR, simulated loss with LTR recovery,
// bitrate / frame-rate changes, ROI). The bitstream goes to FILE (Annex-B for
// H.264/HEVC, IVF for AV1) for ffprobe / ffmpeg, a summary to stdout. For
// checking an encoder backend on real hardware (docs/VENDOR_NOTES.md 3.3).
#pragma once

#include <string>
#include <vector>

#include "backend.hpp"

namespace recon {

struct EncodeTestOptions {
    std::string output;        // --encode-test=FILE
    bool used = false;         // any encode test option was given
    StartParams start;         // codec, capture, size, rate, LTR, ... (defaults: hevc, 60 fps, 20000 kbps)
    int frames = 300;          // stop after this frame id
    int ackDelay = 2;          // an LTR frame is acknowledged this many frames after it was received
    std::vector<std::string> events;  // "N:idr" | "N:loss" | "N:rate=KBPS" | "N:fps=FPS" | "N:roi=X,Y,W,H,W" | "N:roi=off"
};

// Parses one encode test option (--codec=..., --at=..., ...). Returns false if
// key is not one; ok is false for a bad value.
bool encodeTestOption(const std::string& key, const std::string& val, EncodeTestOptions& o, bool& ok);

// Runs the test; exit code 0 = every event handled and no error, 1 = failed,
// 2 = could not start.
int runEncodeTest(EncodeTestOptions& o, BackendChoice& choice);

}  // namespace recon
