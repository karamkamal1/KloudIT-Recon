#include "amf/amf_runtime.hpp"

#include <cstdio>

#include <AMF/core/Version.h>

#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

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
        }
        return r;
    }();
    return rt;
}

}  // namespace recon
