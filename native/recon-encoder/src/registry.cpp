#include "mock/mock.hpp"
#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

BackendChoice chooseBackend(const std::string& name, const MockOptions& mock) {
    BackendChoice out;
    if (name == "mock") {
        auto b = std::make_unique<ReplayEncoder>(mock);
        out.caps = b->caps();
        if (out.caps.backend == "mock") out.backend = std::move(b);
    } else {
        // Real backends. "auto" tries the primary adapter's vendor first, so one
        // binary runs on either vendor (the runtimes are loaded dynamically).
        const AdapterInfo adapter = primaryAdapter();
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
        if (adapter.found) {
            if (out.caps.adapterLuid.empty()) out.caps.adapterLuid = adapter.luid;
            if (out.caps.adapterName.empty()) out.caps.adapterName = adapter.name;
        }
    }

    // Everything that was probed and is not usable, with the reason. The mock
    // reports them too, so tests can see the probing work.
    const std::string chosen = out.backend ? out.backend->name() : "";
    const std::pair<const char*, Probe> probes[] = {
        {"amf", chosen == "amf" ? Probe{true, {}} : probeAmf()},
        {"nvenc", chosen == "nvenc" ? Probe{true, {}} : probeNvenc()},
        {"dda", probeDdaCapture()},
        {"amd-direct", probeAmdDirectCapture()},
        {"wgc", probeWgcCapture()},
    };
    for (const auto& [n, p] : probes) {
        if (!p.available) out.caps.unavailable.emplace_back(n, p.reason);
    }
    return out;
}

std::unique_ptr<Capture> createCapture(const std::string& name, Status& err) {
    if (name == "synthetic") return std::make_unique<SyntheticCapture>();
    if (name == "dda") return createDdaCapture(err);
    if (name == "amd-direct") return createAmdDirectCapture(err);
    if (name == "wgc") return createWgcCapture(err);
    err = Status::Error("bad_message", "unknown capture method \"" + name + "\"");
    return nullptr;
}

}  // namespace recon
