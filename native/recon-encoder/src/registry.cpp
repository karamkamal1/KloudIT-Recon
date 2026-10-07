#include <algorithm>

#include "d3d/device.hpp"
#include "mock/mock.hpp"
#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

BackendChoice chooseBackend(const std::string& name, const MockOptions& mock) {
    BackendChoice out;
    const AdapterInfo adapter = primaryAdapter();
    if (name == "mock") {
        auto b = std::make_unique<ReplayEncoder>(mock);
        out.caps = b->caps();
        if (out.caps.backend == "mock") out.backend = std::move(b);
    } else {
        // Real backends. "auto" tries the primary adapter's vendor first, so one
        // binary runs on either vendor (the runtimes are loaded dynamically).
        std::vector<std::string> order;
        if (name == "amf" || name == "nvenc") order = {name};
        else if (adapter.vendor == "nvidia") order = {"nvenc", "amf"};
        else order = {"amf", "nvenc"};
        for (const auto& n : order) {
            Status err;
            auto b = n == "amf" ? createAmfBackend(err) : createNvencBackend(err);
            if (b) {
                out.caps = b->caps();
                out.backend = std::move(b);
                break;
            }
        }
        if (!out.backend) out.caps.vendor = adapter.vendor;
    }
    // The adapter of the primary display (the mock too: it runs the real
    // capture methods on it).
    if (adapter.found) {
        if (out.caps.adapterLuid.empty()) out.caps.adapterLuid = adapter.luid;
        if (out.caps.adapterName.empty()) out.caps.adapterName = adapter.name;
        out.caps.hagsEnabled = adapter.hags;
    }

    // Everything that was probed and is not usable, with the reason. The mock
    // reports them too, so tests can see the probing work. Usable capture
    // methods are added to the backend's list (the mock: after "synthetic").
    const std::string chosen = out.backend ? out.backend->name() : "";
    const std::pair<const char*, Probe> probes[] = {
        {"amf", chosen == "amf" ? Probe{true, {}} : probeAmf()},
        {"nvenc", chosen == "nvenc" ? Probe{true, {}} : probeNvenc()},
        {"dda", probeDdaCapture()},
        {"amd-direct", probeAmdDirectCapture()},
        {"wgc", probeWgcCapture()},
    };
    for (const auto& [n, p] : probes) {
        const std::string probed = n;
        if (!p.available) {
            out.caps.unavailable.emplace_back(probed, p.reason);
        } else if (out.backend && probed != "amf" && probed != "nvenc" &&
                   std::find(out.caps.capture.begin(), out.caps.capture.end(), probed) == out.caps.capture.end()) {
            out.caps.capture.push_back(probed);
        }
    }
    // DDA and AMD Direct Capture frames never contain the pointer, and WGC is
    // configured without it: recon-host draws the cursor on the client.
    out.caps.cursorInVideo = false;
    out.caps.outputs = d3d::enumerateOutputs();
    return out;
}

std::unique_ptr<Capture> createCapture(const std::string& name, Status& err) {
    if (name == "synthetic") return std::make_unique<SyntheticCapture>();
    if (name == "dda") return createDdaCapture(err);
    if (name == "amd-direct") return createAmdDirectCapture(err);
    if (name == "wgc") return createWgcCapture(err);
    if (name == "synthetic-gpu") return createGpuTestCapture(err);
    err = Status::Error("bad_message", "unknown capture method \"" + name + "\"");
    return nullptr;
}

}  // namespace recon
