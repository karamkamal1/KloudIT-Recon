#include "amf/amf_runtime.hpp"

#include <cstdio>

#include <AMF/core/Trace.h>
#include <AMF/core/Version.h>

#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

namespace {

// AMF's trace goes to our log (stderr). Its console writer must stay off:
// stdout is the control channel to recon-host, and a trace line there would
// break the framing. FFmpeg's hwcontext_amf.c does the same (console writer
// off, its own writer at the configured level).
class LogTraceWriter : public amf::AMFTraceWriter {
public:
    void AMF_CDECL_CALL Write(const wchar_t* scope, const wchar_t* message) override {
        std::string text = toUtf8(message);
        while (!text.empty() && (text.back() == '\n' || text.back() == '\r')) text.pop_back();
        logf(LogLevel::Info, "amf: %s: %s", toUtf8(scope).c_str(), text.c_str());
    }
    void AMF_CDECL_CALL Flush() override {}
};

constexpr const wchar_t* kTraceWriterId = L"recon-encoder";

void routeTrace(amf::AMFFactory* factory, amf_uint64 version) {
    amf::AMFTrace* trace = nullptr;
    if (factory->GetTrace(&trace) != AMF_OK || !trace) return;
    static LogTraceWriter writer;  // registered for the life of the process
    amf_int32 level = logLevel() >= LogLevel::Debug ? AMF_TRACE_INFO : AMF_TRACE_WARNING;
    // FFmpeg: "get around a bug in trace in AMF runtime driver 24.20".
    if (version == AMF_MAKE_FULL_VERSION(1, 4, 35, 0)) level = AMF_TRACE_WARNING;
    trace->EnableWriter(AMF_TRACE_WRITER_CONSOLE, false);
    trace->SetGlobalLevel(level);
    trace->RegisterWriter(kTraceWriterId, &writer, true);
    trace->SetWriterLevel(kTraceWriterId, level);
}

}  // namespace

const AmfRuntime& amfRuntime() {
    static const AmfRuntime rt = [] {
        AmfRuntime r;
        std::string err;
        HMODULE m = loadSystemLibrary(AMF_DLL_NAME, err);  // never freed: AMF objects live as long as the process
        if (!m) {
            r.error = std::string("AMF runtime (") + AMF_DLL_NAMEA + ") not found in System32: " + err;
            return r;
        }
        auto queryVersion = procAddress<AMFQueryVersion_Fn>(m, AMF_QUERY_VERSION_FUNCTION_NAME);
        auto init = procAddress<AMFInit_Fn>(m, AMF_INIT_FUNCTION_NAME);
        if (!queryVersion || !init) {
            r.error = std::string(AMF_DLL_NAMEA) + " does not export AMFInit/AMFQueryVersion";
            return r;
        }
        if (queryVersion(&r.version) == AMF_OK) {
            char ver[64];
            std::snprintf(ver, sizeof(ver), "%u.%u.%u.%u", unsigned(AMF_GET_MAJOR_VERSION(r.version)),
                          unsigned(AMF_GET_MINOR_VERSION(r.version)), unsigned(AMF_GET_SUBMINOR_VERSION(r.version)),
                          unsigned(AMF_GET_BUILD_VERSION(r.version)));
            r.versionText = ver;
        }
        const AMF_RESULT res = init(AMF_FULL_VERSION, &r.factory);
        if (res != AMF_OK || !r.factory) {
            r.factory = nullptr;
            r.error = "AMFInit failed (" + std::to_string(int(res)) + "), runtime " + r.versionText;
            return r;
        }
        routeTrace(r.factory, r.version);
        return r;
    }();
    return rt;
}

}  // namespace recon
