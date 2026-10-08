// The NVENC runtime: nvEncodeAPI64.dll, which the NVIDIA driver installs into
// System32, loaded at run time (so one helper binary runs on either vendor and
// needs no NVIDIA SDK library at build time), the API version negotiated, and
// the function list from NvEncodeAPICreateInstance.
#pragma once

#include <windows.h>

#include <string>

#include <ffnvcodec/nvEncodeAPI.h>

namespace recon {

struct NvencRuntime {
    bool ok = false;
    std::string error;            // why not ok
    HMODULE module = nullptr;     // kept loaded for the process
    uint32_t driverVersion = 0;   // newest API the driver supports: (major << 4) | minor
    std::string versionText;      // "NVENC API 13.0 (driver supports 13.1)"
    bool testDouble = false;      // loaded by --self-test-nvenc=DLL (not the driver)
    NV_ENCODE_API_FUNCTION_LIST api{};
};

// The driver's runtime from System32 (LOAD_LIBRARY_SEARCH_SYSTEM32 only; a
// planted nvEncodeAPI64.dll next to the helper is never loaded), negotiated
// and instantiated once per process.
const NvencRuntime& nvencRuntime();

// Negotiates the API version on a loaded module and creates the function
// list: NvEncodeAPIGetMaxSupportedVersion must report at least the version the
// helper is built for (NVENCAPI_MAJOR/MINOR_VERSION; older drivers get the
// minimum driver in `error`), then NvEncodeAPICreateInstance with
// NV_ENCODE_API_FUNCTION_LIST_VER. Also used by --self-test-nvenc on its test
// double, with different simulated driver versions.
NvencRuntime loadNvencRuntime(HMODULE module);

// "NV_ENC_ERR_INVALID_PARAM (8)".
std::string nvencStatusText(NVENCSTATUS s);

// --self-test-nvenc=DLL: from now on nvencRuntime() is this DLL (a test double
// of the driver, loaded by its full path) instead of System32's. Must be called
// before anything uses nvencRuntime(). Never used outside the self-test.
bool useTestNvencRuntime(const std::wstring& path, std::string& err);

}  // namespace recon
