#include "nvenc/nvenc_runtime.hpp"

#include <mutex>

#include "nvenc/nvenc_policy.hpp"
#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

namespace {

constexpr const wchar_t* kNvencDll = L"nvEncodeAPI64.dll";
using GetMaxSupportedVersionFn = NVENCSTATUS(NVENCAPI*)(uint32_t*);
using CreateInstanceFn = NVENCSTATUS(NVENCAPI*)(NV_ENCODE_API_FUNCTION_LIST*);

std::mutex g_mu;
std::wstring g_testPath;  // useTestNvencRuntime
bool g_used = false;      // nvencRuntime() has been asked

}  // namespace

std::string nvencStatusText(NVENCSTATUS s) {
    static const char* const names[] = {
        "NV_ENC_SUCCESS", "NV_ENC_ERR_NO_ENCODE_DEVICE", "NV_ENC_ERR_UNSUPPORTED_DEVICE", "NV_ENC_ERR_INVALID_ENCODERDEVICE",
        "NV_ENC_ERR_INVALID_DEVICE", "NV_ENC_ERR_DEVICE_NOT_EXIST", "NV_ENC_ERR_INVALID_PTR", "NV_ENC_ERR_INVALID_EVENT",
        "NV_ENC_ERR_INVALID_PARAM", "NV_ENC_ERR_INVALID_CALL", "NV_ENC_ERR_OUT_OF_MEMORY", "NV_ENC_ERR_ENCODER_NOT_INITIALIZED",
        "NV_ENC_ERR_UNSUPPORTED_PARAM", "NV_ENC_ERR_LOCK_BUSY", "NV_ENC_ERR_NOT_ENOUGH_BUFFER", "NV_ENC_ERR_INVALID_VERSION",
        "NV_ENC_ERR_MAP_FAILED", "NV_ENC_ERR_NEED_MORE_INPUT", "NV_ENC_ERR_ENCODER_BUSY", "NV_ENC_ERR_EVENT_NOT_REGISTERD",
        "NV_ENC_ERR_GENERIC", "NV_ENC_ERR_INCOMPATIBLE_CLIENT_KEY", "NV_ENC_ERR_UNIMPLEMENTED", "NV_ENC_ERR_RESOURCE_REGISTER_FAILED",
        "NV_ENC_ERR_RESOURCE_NOT_REGISTERED", "NV_ENC_ERR_RESOURCE_NOT_MAPPED", "NV_ENC_ERR_NEED_MORE_OUTPUT",
    };
    const int i = int(s);
    const std::string name = i >= 0 && size_t(i) < sizeof(names) / sizeof(names[0]) ? names[i] : "NVENCSTATUS";
    return name + " (" + std::to_string(i) + ")";
}

NvencRuntime loadNvencRuntime(HMODULE module) {
    NvencRuntime rt;
    rt.module = module;
    if (!module) {
        rt.error = "no NVENC runtime module";
        return rt;
    }
    auto getMax = procAddress<GetMaxSupportedVersionFn>(module, "NvEncodeAPIGetMaxSupportedVersion");
    auto create = procAddress<CreateInstanceFn>(module, "NvEncodeAPICreateInstance");
    if (!getMax || !create) {
        rt.error = "nvEncodeAPI64.dll does not export NvEncodeAPIGetMaxSupportedVersion / NvEncodeAPICreateInstance";
        return rt;
    }
    uint32_t v = 0;
    NVENCSTATUS s = getMax(&v);
    if (s != NV_ENC_SUCCESS) {
        rt.error = "NvEncodeAPIGetMaxSupportedVersion failed: " + nvencStatusText(s);
        return rt;
    }
    rt.driverVersion = v;
    const uint32_t built = nvenc::apiVersion(NVENCAPI_MAJOR_VERSION, NVENCAPI_MINOR_VERSION);
    rt.versionText = "NVENC API " + nvenc::apiVersionText(built) + " (driver supports " + nvenc::apiVersionText(v) + ")";
    if (const std::string problem = nvenc::versionProblem(v, built); !problem.empty()) {
        rt.error = problem;
        return rt;
    }
    rt.api = {};
    rt.api.version = NV_ENCODE_API_FUNCTION_LIST_VER;
    s = create(&rt.api);
    if (s != NV_ENC_SUCCESS) {
        rt.error = "NvEncodeAPICreateInstance failed: " + nvencStatusText(s);
        return rt;
    }
    const NV_ENCODE_API_FUNCTION_LIST& a = rt.api;
    if (!a.nvEncOpenEncodeSessionEx || !a.nvEncGetEncodeGUIDCount || !a.nvEncGetEncodeGUIDs || !a.nvEncGetEncodeCaps ||
        !a.nvEncGetInputFormatCount || !a.nvEncGetInputFormats || !a.nvEncGetEncodePresetConfigEx || !a.nvEncInitializeEncoder ||
        !a.nvEncCreateBitstreamBuffer || !a.nvEncDestroyBitstreamBuffer || !a.nvEncRegisterAsyncEvent || !a.nvEncUnregisterAsyncEvent ||
        !a.nvEncRegisterResource || !a.nvEncUnregisterResource || !a.nvEncMapInputResource || !a.nvEncUnmapInputResource ||
        !a.nvEncEncodePicture || !a.nvEncLockBitstream || !a.nvEncUnlockBitstream || !a.nvEncInvalidateRefFrames ||
        !a.nvEncReconfigureEncoder || !a.nvEncGetSequenceParams || !a.nvEncDestroyEncoder) {
        rt.error = "NvEncodeAPICreateInstance returned an incomplete function list";
        return rt;
    }
    rt.ok = true;
    return rt;
}

const NvencRuntime& nvencRuntime() {
    static const NvencRuntime runtime = [] {
        std::wstring test;
        {
            std::lock_guard<std::mutex> lock(g_mu);
            g_used = true;
            test = g_testPath;
        }
        std::string err;
        HMODULE m = nullptr;
        if (test.empty()) {
            m = loadSystemLibrary(kNvencDll, err);
        } else {
            m = LoadLibraryExW(test.c_str(), nullptr, LOAD_WITH_ALTERED_SEARCH_PATH);
            if (!m) err = win32ErrorText(GetLastError());
        }
        if (!m) {
            NvencRuntime r;
            r.error = (test.empty() ? "NVENC runtime (nvEncodeAPI64.dll) not found in System32: " : "cannot load the NVENC test double: ") + err;
            return r;
        }
        NvencRuntime r = loadNvencRuntime(m);
        r.testDouble = !test.empty();
        if (!r.ok) {
            FreeLibrary(m);
            r.module = nullptr;
        }
        return r;
    }();
    return runtime;
}

bool useTestNvencRuntime(const std::wstring& path, std::string& err) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (g_used) {
        err = "the NVENC runtime is already loaded";
        return false;
    }
    g_testPath = path;
    return true;
}

}  // namespace recon
