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

LogLevel logLevel() { return static_cast<LogLevel>(g_logLevel.load()); }

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

std::wstring fromUtf8(const std::string& s) {
    if (s.empty()) return {};
    const int n = MultiByteToWideChar(CP_UTF8, 0, s.data(), int(s.size()), nullptr, 0);
    if (n <= 0) return {};
    std::wstring out(size_t(n), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, s.data(), int(s.size()), out.data(), n);
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

namespace {

using TimePeriodFn = UINT(WINAPI*)(UINT);  // timeBeginPeriod / timeEndPeriod (MMRESULT is a UINT)

TimePeriodFn winmmFunction(const char* name) {
    static const HMODULE winmm = [] {
        std::string err;
        HMODULE m = loadSystemLibrary(L"winmm.dll", err);  // kept loaded
        if (!m) logf(LogLevel::Warn, "winmm.dll: %s (timer resolution stays at the system default)", err.c_str());
        return m;
    }();
    return winmm ? reinterpret_cast<TimePeriodFn>(reinterpret_cast<void*>(GetProcAddress(winmm, name))) : nullptr;
}

}  // namespace

TimerResolution::TimerResolution() {
    const TimePeriodFn begin = winmmFunction("timeBeginPeriod");
    if (!begin) return;
    // As amf_increase_timer_precision (AMF ThreadWindows.cpp) and FFmpeg
    // vsrc_amf.c: the finest period the system accepts, from 1 ms up
    // (TIMERR_NOCANDO = 97 for a period out of range).
    for (unsigned p = 1; p <= 16; ++p) {
        const UINT r = begin(p);
        if (r == 0) {  // TIMERR_NOERROR
            period_ = p;
            return;
        }
        if (r != 97) break;
    }
    logf(LogLevel::Warn, "timeBeginPeriod failed: waits keep the default timer resolution (~15.6 ms)");
}

TimerResolution::~TimerResolution() {
    if (!period_) return;
    if (const TimePeriodFn end = winmmFunction("timeEndPeriod")) end(period_);
}

std::string vendorName(uint32_t vendorId) {
    switch (vendorId) {
    case 0x1002: return "amd";
    case 0x10DE: return "nvidia";
    case 0x8086: return "intel";
    default: return "other";
    }
}

std::string luidString(const LUID& luid) {
    char buf[32];
    std::snprintf(buf, sizeof(buf), "%08lx:%08lx", static_cast<unsigned long>(luid.HighPart),
                  static_cast<unsigned long>(luid.LowPart));
    return buf;
}

bool parseLuid(const std::string& s, LUID& out) {
    const size_t colon = s.find(':');
    if (colon == std::string::npos || colon == 0 || colon + 1 >= s.size() || colon > 8 || s.size() - colon - 1 > 8) {
        return false;
    }
    auto hex = [](const std::string& h, unsigned long& v) {
        if (h.empty()) return false;
        v = 0;
        for (char c : h) {
            int d;
            if (c >= '0' && c <= '9') d = c - '0';
            else if (c >= 'a' && c <= 'f') d = c - 'a' + 10;
            else if (c >= 'A' && c <= 'F') d = c - 'A' + 10;
            else return false;
            v = v * 16 + static_cast<unsigned long>(d);
        }
        return true;
    };
    unsigned long hi = 0, lo = 0;
    if (!hex(s.substr(0, colon), hi) || !hex(s.substr(colon + 1), lo)) return false;
    out.HighPart = static_cast<LONG>(hi);
    out.LowPart = static_cast<DWORD>(lo);
    return true;
}

AdapterInfo describeAdapter(uint32_t vendorId, const LUID& luid, const wchar_t* description) {
    AdapterInfo info;
    info.found = true;
    info.vendorId = vendorId;
    info.vendor = vendorName(vendorId);
    info.luidValue = luid;
    info.luid = luidString(luid);
    info.name = toUtf8(description);
    info.hags = queryHags(luid);
    return info;
}

AdapterInfo primaryAdapter() {
    AdapterInfo info;
    IDXGIFactory1* factory = nullptr;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(&factory)))) return info;
    IDXGIAdapter1* adapter = nullptr;
    if (SUCCEEDED(factory->EnumAdapters1(0, &adapter))) {
        DXGI_ADAPTER_DESC1 desc{};
        if (SUCCEEDED(adapter->GetDesc1(&desc))) info = describeAdapter(desc.VendorId, desc.AdapterLuid, desc.Description);
        adapter->Release();
    }
    factory->Release();
    return info;
}

}  // namespace recon
