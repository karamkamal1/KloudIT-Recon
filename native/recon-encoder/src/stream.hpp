// Starting a stream: capture, GPU priority, encoder, colour conversion and the
// pipeline for a "start" message (main.cpp) or the --encode-test mode
// (encode_test.cpp). On failure everything created is gone again.
#pragma once

#include <memory>
#include <string>

#include "backend.hpp"
#include "pipeline.hpp"
#include "ring.hpp"

namespace recon {

struct StartResult {
    std::unique_ptr<Capture> capture;
    std::unique_ptr<Pipeline> pipeline;
};

// dumpNv12: --dump-nv12 (empty = off).
Status startStream(const StartParams& p, BackendChoice& choice, RingWriter& ring, Reporter& rep, const std::string& dumpNv12,
                   StartResult& out, Started& st);

// "name: why; ..." of every unavailable backend / capture method.
std::string joinUnavailable(const Caps& caps);

}  // namespace recon
