// Small Win32 helpers: logging to stderr, QPC, system DLL loading, a precise
// waitable timer, adapter description and GPU scheduling (HAGS detection, GPU
// priority).
#pragma once

#include <windows.h>

#include <cstdint>
#include <optional>
#include <string>

namespace recon {

// --- Logging (stderr; recon-host forwards every line to its log) -------------

enum class LogLevel { Error = 0, Warn = 1, Info = 2, Debug = 3 };

void setLogLevel(LogLevel level);
LogLevel logLevel();
bool parseLogLevel(const std::string& s, LogLevel& out);
void logf(LogLevel level, const char* fmt, ...)
#if defined(__MINGW32__) && !defined(__clang__)
    __attribute__((format(gnu_printf, 2, 3)))  // with __USE_MINGW_ANSI_STDIO (CMakeLists.txt)
#elif defined(__GNUC__)
    __attribute__((format(printf, 2, 3)))
#endif
    ;

// --- Time ----------------------------------------------------------------------

inline int64_t qpcNow() {
    LARGE_INTEGER v;
    QueryPerformanceCounter(&v);
    return v.QuadPart;
}

inline int64_t qpcFrequency() {
    LARGE_INTEGER f;
    QueryPerformanceFrequency(&f);
    return f.QuadPart;
}

// PreciseTimer sleeps until a QPC deadline. It uses a high-resolution waitable
// timer where available (Windows 10 1803+) and can be interrupted by an event.
class PreciseTimer {
public:
    PreciseTimer();
    ~PreciseTimer();
    PreciseTimer(const PreciseTimer&) = delete;
    PreciseTimer& operator=(const PreciseTimer&) = delete;

    // Waits until qpcNow() >= deadline, or until wake is signalled (returns false).
    bool sleepUntil(int64_t deadlineQpc, HANDLE wake);

private:
    HANDLE timer_ = nullptr;
    int64_t freq_ = 0;
};

// TimerResolution raises the system timer resolution to 1 ms for this process
// while it exists (timeBeginPeriod / timeEndPeriod from System32's winmm.dll,
// loaded at run time). Since Windows 10 2004 a process that has not asked for
// it gets the default tick of about 15.6 ms, so a Sleep(1), a wait timeout or
// an AcquireNextFrame timeout can last that long ("For processes which have
// not called this function, Windows does not guarantee a higher resolution
// than the default system resolution", timeBeginPeriod docs). The AMD
// Streaming SDK (amf_increase_timer_precision in RemoteDesktopServer.cpp),
// FFmpeg vsrc_amf.c and Sunshine (misc.cpp streaming_will_start) do the same
// while they capture. The helper owns no window, so the Windows 11 rule that
// ignores the request for occluded window-owning processes does not apply.
class TimerResolution {
public:
    TimerResolution();
    ~TimerResolution();
    TimerResolution(const TimerResolution&) = delete;
    TimerResolution& operator=(const TimerResolution&) = delete;

    unsigned periodMs() const { return period_; }  // 0 = not raised

private:
    unsigned period_ = 0;
};

// --- DLL loading -------------------------------------------------------------------

// Loads a DLL from System32 only (LOAD_LIBRARY_SEARCH_SYSTEM32), never from the
// application or current directory, so a planted amfrt64.dll / nvEncodeAPI64.dll
// next to the helper cannot be picked up. On failure err describes why.
HMODULE loadSystemLibrary(const wchar_t* name, std::string& err);

std::string win32ErrorText(DWORD code);
std::string toUtf8(const wchar_t* s);
std::wstring fromUtf8(const std::string& s);

// --- Adapter -----------------------------------------------------------------------

struct AdapterInfo {
    bool found = false;
    uint32_t vendorId = 0;
    std::string vendor = "other";  // "amd" | "nvidia" | "intel" | "other"
    std::string luid;              // "%08x:%08x" (HighPart:LowPart)
    LUID luidValue{};
    std::string name;
    std::optional<bool> hags;  // hardware-accelerated GPU scheduling (queryHags), nullopt = unknown
};

std::string vendorName(uint32_t vendorId);
std::string luidString(const LUID& luid);
bool parseLuid(const std::string& s, LUID& out);
// Fills an AdapterInfo from DXGI_ADAPTER_DESC1 fields (and queries HAGS).
AdapterInfo describeAdapter(uint32_t vendorId, const LUID& luid, const wchar_t* description);

// Describes DXGI adapter 0 (the adapter of the primary display on most
// systems), for caps. The capture reports the adapter it actually uses.
AdapterInfo primaryAdapter();

// --- GPU scheduling (gdi32 D3DKMT* entry points, loaded at run time) -------------

// Whether hardware-accelerated GPU scheduling is enabled on the adapter:
// D3DKMTQueryAdapterInfo(KMTQAITYPE_WDDM_2_7_CAPS).HwSchEnabled. nullopt if the
// query is not available (Windows < 10 2004, Wine) or fails.
std::optional<bool> queryHags(const LUID& adapter);

// Enables SeIncreaseBasePriorityPrivilege on the process token (held by
// elevated administrators; IDXGIDevice::SetGPUThreadPriority and a REALTIME GPU
// priority need it). Returns false if the token does not hold it.
bool enableIncreaseBasePriority();

// Sets this process's GPU scheduling priority class (GUIDE 1.3):
// mode "off" leaves it alone; otherwise REALTIME, except HIGH when mode is
// "high" or when mode is "auto" on NVIDIA with HAGS on or unknown; a refused
// REALTIME is retried as HIGH. Enables SeIncreaseBasePriorityPrivilege first.
// Returns "realtime" | "high" | "failed" | "off" and logs the outcome.
std::string applyGpuPriority(const std::string& mode, const AdapterInfo& adapter);

}  // namespace recon
