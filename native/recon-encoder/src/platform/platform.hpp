// Small Win32 helpers: logging to stderr, QPC, system DLL loading, a precise
// waitable timer and primary-adapter detection.
#pragma once

#include <windows.h>

#include <cstdint>
#include <string>

namespace recon {

// --- Logging (stderr; recon-host forwards every line to its log) -------------

enum class LogLevel { Error = 0, Warn = 1, Info = 2, Debug = 3 };

void setLogLevel(LogLevel level);
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

// --- DLL loading -------------------------------------------------------------------

// Loads a DLL from System32 only (LOAD_LIBRARY_SEARCH_SYSTEM32), never from the
// application or current directory, so a planted amfrt64.dll / nvEncodeAPI64.dll
// next to the helper cannot be picked up. On failure err describes why.
HMODULE loadSystemLibrary(const wchar_t* name, std::string& err);

std::string win32ErrorText(DWORD code);
std::string toUtf8(const wchar_t* s);

// --- Adapter -----------------------------------------------------------------------

struct AdapterInfo {
    bool found = false;
    uint32_t vendorId = 0;
    std::string vendor = "other";  // "amd" | "nvidia" | "intel" | "other"
    std::string luid;              // "%08x:%08x" (HighPart:LowPart)
    std::string name;
};

// Describes DXGI adapter 0 (the adapter of the primary display on most systems).
// Step 3.2 replaces this with the adapter that owns the captured output.
AdapterInfo primaryAdapter();

}  // namespace recon
