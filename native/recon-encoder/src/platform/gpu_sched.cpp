// GPU scheduling: HAGS detection and the process GPU priority (GUIDE 1.3,
// 3.2). The D3DKMT* thunks are exported by gdi32.dll; their declarations live
// in the WDK's d3dkmthk.h, which neither MSVC's SDK include path nor mingw-w64
// provide, so the few structures needed are declared here (layouts from
// d3dkmthk.h; KMTQAITYPE_WDDM_2_7_CAPS = 70 as in Wine's include/ddk/d3dkmthk.h)
// and the functions are resolved at run time. Sunshine does the same in
// src/platform/windows/display_base.cpp (display_base_t::init).
#include <cstdio>

#include "platform/platform.hpp"

namespace recon {

namespace {

using KmtHandle = UINT;  // D3DKMT_HANDLE
using NtStatus = LONG;

struct KmtOpenAdapterFromLuid {  // D3DKMT_OPENADAPTERFROMLUID
    LUID AdapterLuid;
    KmtHandle hAdapter;
};
struct KmtQueryAdapterInfo {  // D3DKMT_QUERYADAPTERINFO
    KmtHandle hAdapter;
    int Type;  // KMTQUERYADAPTERINFOTYPE
    void* pPrivateDriverData;
    UINT PrivateDriverDataSize;
};
struct KmtCloseAdapter {  // D3DKMT_CLOSEADAPTER
    KmtHandle hAdapter;
};
// D3DKMT_WDDM_2_7_CAPS: a UINT bit field; bit 0 HwSchSupported, bit 1
// HwSchEnabled, bit 2 HwSchEnabledByDefault, bit 3 IndependentVidPnVSyncControl.
struct KmtWddm27Caps {
    UINT Value;
};
constexpr int kKmtQaiTypeWddm27Caps = 70;  // KMTQAITYPE_WDDM_2_7_CAPS
constexpr UINT kHwSchEnabled = 1u << 1;

static_assert(sizeof(KmtOpenAdapterFromLuid) == 12, "D3DKMT_OPENADAPTERFROMLUID layout");
static_assert(sizeof(KmtQueryAdapterInfo) == 24, "D3DKMT_QUERYADAPTERINFO layout (x64)");
static_assert(sizeof(KmtWddm27Caps) == 4, "D3DKMT_WDDM_2_7_CAPS layout");

// D3DKMT_SCHEDULINGPRIORITYCLASS
constexpr int kPriorityHigh = 4;
constexpr int kPriorityRealtime = 5;

using OpenAdapterFromLuidFn = NtStatus(WINAPI*)(KmtOpenAdapterFromLuid*);
using QueryAdapterInfoFn = NtStatus(WINAPI*)(KmtQueryAdapterInfo*);
using CloseAdapterFn = NtStatus(WINAPI*)(const KmtCloseAdapter*);
using SetProcessSchedulingPriorityClassFn = NtStatus(WINAPI*)(HANDLE, int);

template <typename Fn>
Fn gdiProc(const char* name) {
    static HMODULE gdi32 = [] {
        std::string err;
        return loadSystemLibrary(L"gdi32.dll", err);  // never freed: a system DLL every GUI process has
    }();
    if (!gdi32) return nullptr;
    return reinterpret_cast<Fn>(reinterpret_cast<void*>(GetProcAddress(gdi32, name)));
}

const char* hagsText(const std::optional<bool>& h) { return !h ? "hags unknown" : *h ? "hags on" : "hags off"; }

}  // namespace

bool enableIncreaseBasePriority() {
    HANDLE token = nullptr;
    if (!OpenProcessToken(GetCurrentProcess(), TOKEN_ADJUST_PRIVILEGES | TOKEN_QUERY, &token)) return false;
    TOKEN_PRIVILEGES tp{};
    bool ok = false;
    if (LookupPrivilegeValueW(nullptr, L"SeIncreaseBasePriorityPrivilege", &tp.Privileges[0].Luid)) {
        tp.PrivilegeCount = 1;
        tp.Privileges[0].Attributes = SE_PRIVILEGE_ENABLED;
        // Succeeds with ERROR_NOT_ALL_ASSIGNED when the token lacks the privilege.
        ok = AdjustTokenPrivileges(token, FALSE, &tp, sizeof(tp), nullptr, nullptr) && GetLastError() == ERROR_SUCCESS;
    }
    CloseHandle(token);
    return ok;
}

std::optional<bool> queryHags(const LUID& adapter) {
    auto open = gdiProc<OpenAdapterFromLuidFn>("D3DKMTOpenAdapterFromLuid");
    auto query = gdiProc<QueryAdapterInfoFn>("D3DKMTQueryAdapterInfo");
    auto close = gdiProc<CloseAdapterFn>("D3DKMTCloseAdapter");
    if (!open || !query || !close) return std::nullopt;
    KmtOpenAdapterFromLuid oa{};
    oa.AdapterLuid = adapter;
    if (open(&oa) != 0) return std::nullopt;
    KmtWddm27Caps caps{};
    KmtQueryAdapterInfo qi{};
    qi.hAdapter = oa.hAdapter;
    qi.Type = kKmtQaiTypeWddm27Caps;
    qi.pPrivateDriverData = &caps;
    qi.PrivateDriverDataSize = sizeof(caps);
    // Fails before Windows 10 2004 (WDDM 2.7), where HAGS does not exist.
    const NtStatus st = query(&qi);
    KmtCloseAdapter ca{oa.hAdapter};
    close(&ca);
    if (st != 0) return std::nullopt;
    return (caps.Value & kHwSchEnabled) != 0;
}

const char* gpuPriorityFor(const std::string& mode, const std::string& vendor, std::optional<bool> hags) {
    if (mode == "off") return "off";
    // HAGS unknown counts as on: the safe side for NVIDIA.
    if (mode == "high" || (mode != "realtime" && vendor == "nvidia" && hags.value_or(true))) return "high";
    return "realtime";
}

void printGpuPriorityTable() {
    for (const char* mode : {"auto", "high", "realtime", "off"}) {
        for (const char* vendor : {"amd", "nvidia", "intel", "other"}) {
            for (const std::optional<bool> hags : {std::optional<bool>(true), std::optional<bool>(false), std::optional<bool>()}) {
                std::printf("{\"mode\":\"%s\",\"vendor\":\"%s\",\"hags\":\"%s\",\"priority\":\"%s\"}\n", mode, vendor,
                            !hags ? "unknown" : *hags ? "on" : "off", gpuPriorityFor(mode, vendor, hags));
            }
        }
    }
}

std::string applyGpuPriority(const std::string& mode, const AdapterInfo& adapter) {
    const std::string decision = gpuPriorityFor(mode, adapter.vendor, adapter.hags);
    if (decision == "off") {
        logf(LogLevel::Info, "gpu priority: off (%s, %s)", adapter.vendor.c_str(), hagsText(adapter.hags));
        return "off";
    }
    const int prio = decision == "high" ? kPriorityHigh : kPriorityRealtime;
    const bool privilege = enableIncreaseBasePriority();
    auto set = gdiProc<SetProcessSchedulingPriorityClassFn>("D3DKMTSetProcessSchedulingPriorityClass");
    std::string result = "failed";
    NtStatus st = -1;
    if (set) {
        st = set(GetCurrentProcess(), prio);
        if (st == 0) {
            result = prio == kPriorityRealtime ? "realtime" : "high";
        } else if (prio == kPriorityRealtime) {
            st = set(GetCurrentProcess(), kPriorityHigh);  // REALTIME refused: HIGH (GUIDE 1.3)
            if (st == 0) result = "high";
        }
    }
    if (result == "failed") {
        logf(LogLevel::Warn, "gpu priority: failed (%s, %s; wanted %s): %s", adapter.vendor.c_str(), hagsText(adapter.hags),
             prio == kPriorityRealtime ? "realtime" : "high",
             !set ? "D3DKMTSetProcessSchedulingPriorityClass not available"
                  : privilege ? "refused" : "refused (SeIncreaseBasePriorityPrivilege not held: run recon-host elevated)");
    } else {
        logf(LogLevel::Info, "gpu priority: %s (%s, %s)", result.c_str(), adapter.vendor.c_str(), hagsText(adapter.hags));
    }
    return result;
}

}  // namespace recon
