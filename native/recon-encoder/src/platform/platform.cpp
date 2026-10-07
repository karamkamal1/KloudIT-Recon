#include "platform/platform.hpp"

#include <dxgi.h>

#include <atomic>
#include <cstdarg>
#include <cstdio>
#include <mutex>

#ifndef CREATE_WAITABLE_TIMER_HIGH_RESOLUTION
#define CREATE_WAITABLE_TIMER_HIGH_RESOLUTION 0x00000002
#endif
#ifndef LOAD_LIBRARY_SEARCH_SYSTEM32
#define LOAD_LIBRARY_SEARCH_SYSTEM32 0x00000800
#endif

namespace recon {

namespace {
std::atomic<int> g_logLevel{static_cast<int>(LogLevel::Info)};
std::mutex g_logMu;
}  // namespace

void setLogLevel(LogLevel level) { g_logLevel = static_cast<int>(level); }

bool parseLogLevel(const std::string& s, LogLevel& out) {
    if (s == "error") out = LogLevel::Error;
    else if (s == "warn") out = LogLevel::Warn;
    else if (s == "info") out = LogLevel::Info;
    else if (s == "debug") out = LogLevel::Debug;
    else return false;
    return true;
}

void logf(LogLevel level, const char* fmt, ...) {
    if (static_cast<int>(level) > g_logLevel.load()) return;
    static const char* const names[] = {"error", "warn", "info", "debug"};
    char buf[1024];
    va_list ap;
    va_start(ap, fmt);
    std::vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    std::lock_guard<std::mutex> lock(g_logMu);
    std::fprintf(stderr, "%s: %s\n", names[static_cast<int>(level)], buf);
    std::fflush(stderr);
}

std::string win32ErrorText(DWORD code) {
    wchar_t* msg = nullptr;
    DWORD n = FormatMessageW(FORMAT_MESSAGE_ALLOCATE_BUFFER | FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS,
                             nullptr, code, 0, reinterpret_cast<wchar_t*>(&msg), 0, nullptr);
    std::string out;
    if (n && msg) {
        out = toUtf8(msg);
        while (!out.empty() && (out.back() == '\n' || out.back() == '\r' || out.back() == ' ' || out.back() == '.')) {
            out.pop_back();
        }
    }
    if (msg) LocalFree(msg);
    char num[32];
    std::snprintf(num, sizeof(num), "error %lu", static_cast<unsigned long>(code));
    return out.empty() ? std::string(num) : out + " (" + num + ")";
}

std::string toUtf8(const wchar_t* s) {
    if (!s || !*s) return {};
    int n = WideCharToMultiByte(CP_UTF8, 0, s, -1, nullptr, 0, nullptr, nullptr);
    if (n <= 1) return {};
    std::string out(static_cast<size_t>(n - 1), '\0');
    WideCharToMultiByte(CP_UTF8, 0, s, -1, out.data(), n, nullptr, nullptr);
    return out;
}

HMODULE loadSystemLibrary(const wchar_t* name, std::string& err) {
    HMODULE m = LoadLibraryExW(name, nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!m) err = win32ErrorText(GetLastError());
    return m;
}

PreciseTimer::PreciseTimer() : freq_(qpcFrequency()) {
    timer_ = CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
    if (!timer_) timer_ = CreateWaitableTimerExW(nullptr, nullptr, 0, TIMER_ALL_ACCESS);
}

PreciseTimer::~PreciseTimer() {
    if (timer_) CloseHandle(timer_);
}

bool PreciseTimer::sleepUntil(int64_t deadline, HANDLE wake) {
    for (;;) {
        const int64_t now = qpcNow();
        if (now >= deadline) return true;
        // Relative due time in 100 ns units (negative = relative), at most 1 s per wait.
        const int64_t left = deadline - now < freq_ ? deadline - now : freq_;
        const int64_t ticks100ns = left * 10000000 / freq_;
        HANDLE hs[2] = {wake, timer_};
        DWORD r;
        LARGE_INTEGER due;
        due.QuadPart = -(ticks100ns > 0 ? ticks100ns : 1);
        if (timer_ && SetWaitableTimer(timer_, &due, 0, nullptr, nullptr, FALSE)) {
            r = WaitForMultipleObjects(2, hs, FALSE, INFINITE);
        } else {
            const DWORD ms = static_cast<DWORD>(left * 1000 / freq_);
            r = WaitForSingleObject(wake, ms);
            if (r == WAIT_TIMEOUT) continue;
        }
        if (r == WAIT_OBJECT_0) return false;  // woken
    }
}

AdapterInfo primaryAdapter() {
    AdapterInfo info;
    IDXGIFactory1* factory = nullptr;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(&factory)))) return info;
    IDXGIAdapter1* adapter = nullptr;
    if (SUCCEEDED(factory->EnumAdapters1(0, &adapter))) {
        DXGI_ADAPTER_DESC1 desc{};
        if (SUCCEEDED(adapter->GetDesc1(&desc))) {
            info.found = true;
            info.vendorId = desc.VendorId;
            switch (desc.VendorId) {
            case 0x1002: info.vendor = "amd"; break;
            case 0x10DE: info.vendor = "nvidia"; break;
            case 0x8086: info.vendor = "intel"; break;
            default: info.vendor = "other"; break;
            }
            char luid[32];
            std::snprintf(luid, sizeof(luid), "%08lx:%08lx", static_cast<unsigned long>(desc.AdapterLuid.HighPart),
                          static_cast<unsigned long>(desc.AdapterLuid.LowPart));
            info.luid = luid;
            info.name = toUtf8(desc.Description);
        }
        adapter->Release();
    }
    factory->Release();
    return info;
}

}  // namespace recon
