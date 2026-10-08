#include "lavc/lavc_runtime.hpp"

#include <atomic>
#include <cstdarg>
#include <mutex>

#include "platform/platform.hpp"
#include "probes.hpp"

namespace recon {

namespace {

// The libraries the helper is built for: FFmpeg 8.x (avcodec major 62, avutil
// major 60; FFmpeg keeps the ABI within a major version, so any 8.x release
// works). The file names carry the major version.
const std::wstring kAvutilDll = L"avutil-" + std::to_wstring(LIBAVUTIL_VERSION_MAJOR) + L".dll";
const std::wstring kAvcodecDll = L"avcodec-" + std::to_wstring(LIBAVCODEC_VERSION_MAJOR) + L".dll";

std::mutex g_mu;
LavcOptions g_options;
bool g_used = false;  // lavcRuntime() has been asked: the options are fixed
// For the log callback, which must not wait for lavcRuntime()'s initialisation.
std::atomic<void*> g_formatLine{nullptr};  // av_log_format_line2

std::wstring helperDirectory() {
    wchar_t path[MAX_PATH * 4];
    const DWORD n = GetModuleFileNameW(nullptr, path, DWORD(sizeof(path) / sizeof(path[0])));
    if (!n || n >= sizeof(path) / sizeof(path[0])) return L"";
    std::wstring s(path, n);
    const size_t slash = s.find_last_of(L"\\/");
    return slash == std::wstring::npos ? L"" : s.substr(0, slash);
}

// A fully qualified path (LoadLibraryExW with LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR
// refuses a relative one): a relative --ffmpeg-dir is taken from the current
// directory.
std::wstring fullPath(const std::wstring& path) {
    const DWORD n = GetFullPathNameW(path.c_str(), 0, nullptr, nullptr);
    if (!n) return path;
    std::wstring out(n, L'\0');
    const DWORD m = GetFullPathNameW(path.c_str(), n, out.data(), nullptr);
    if (!m || m >= n) return path;
    out.resize(m);
    while (out.size() > 3 && (out.back() == L'\\' || out.back() == L'/')) out.pop_back();
    return out;
}

bool fileExists(const std::wstring& path) {
    const DWORD a = GetFileAttributesW(path.c_str());
    return a != INVALID_FILE_ATTRIBUTES && !(a & FILE_ATTRIBUTE_DIRECTORY);
}

// FFmpeg's log -> the helper's log. Called from any thread FFmpeg logs on.
void logCallback(void* avcl, int level, const char* fmt, va_list vl) {
    const auto formatLine = reinterpret_cast<decltype(&::av_log_format_line2)>(g_formatLine.load());
    if (level > AV_LOG_DEBUG || !formatLine) return;
    const LogLevel ours = level <= AV_LOG_ERROR ? LogLevel::Warn : level <= AV_LOG_WARNING ? LogLevel::Info : LogLevel::Debug;
    if (ours > logLevel()) return;
    char line[1024];
    int prefix = 1;
    va_list copy;
    va_copy(copy, vl);
    formatLine(avcl, level, fmt, copy, line, sizeof(line), &prefix);
    va_end(copy);
    std::string s(line);
    while (!s.empty() && (s.back() == '\n' || s.back() == '\r')) s.pop_back();
    if (!s.empty()) logf(ours, "ffmpeg: %s", s.c_str());
}

template <typename Fn>
bool resolve(HMODULE m, const char* name, Fn& out, std::string& missing) {
    out = procAddress<Fn>(m, name);
    if (!out) missing += std::string(missing.empty() ? "" : ", ") + name;
    return out != nullptr;
}

LavcRuntime load(const LavcOptions& opt) {
    LavcRuntime rt;
    std::vector<std::wstring> dirs;
    if (!opt.dir.empty()) {
        dirs.push_back(fullPath(opt.dir));
    } else if (const std::wstring here = helperDirectory(); !here.empty()) {
        dirs.push_back(here + L"\\ffmpeg-lgpl");
        dirs.push_back(here);
    }
    std::wstring dir;
    for (const std::wstring& d : dirs) {
        if (fileExists(d + L"\\" + kAvcodecDll) && fileExists(d + L"\\" + kAvutilDll)) {
            dir = d;
            break;
        }
    }
    if (dir.empty()) {
        std::string where;
        for (const std::wstring& d : dirs) where += std::string(where.empty() ? "" : " or ") + toUtf8(d.c_str());
        rt.error = "libavcodec (" + toUtf8(kAvcodecDll.c_str()) + ", " + toUtf8(kAvutilDll.c_str()) + " of FFmpeg 8.x) not found in " +
                   (where.empty() ? std::string("the helper's directory") : where) +
                   " (install-host.ps1 -InstallLibavcodec downloads them)";
        return rt;
    }
    rt.dir = dir;
    // Dependencies (swresample-6.dll) from the same directory or System32.
    const DWORD flags = LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32;
    rt.avutil = LoadLibraryExW((dir + L"\\" + kAvutilDll).c_str(), nullptr, flags);
    if (!rt.avutil) {
        rt.error = "cannot load " + toUtf8((dir + L"\\" + kAvutilDll).c_str()) + ": " + win32ErrorText(GetLastError());
        return rt;
    }
    rt.avcodec = LoadLibraryExW((dir + L"\\" + kAvcodecDll).c_str(), nullptr, flags);
    if (!rt.avcodec) {
        rt.error = "cannot load " + toUtf8((dir + L"\\" + kAvcodecDll).c_str()) + ": " + win32ErrorText(GetLastError()) +
                   " (its swresample DLL must be next to it)";
        return rt;
    }
    std::string missing;
    const HMODULE u = rt.avutil, c = rt.avcodec;
    resolve(u, "avutil_version", rt.avutil_version, missing);
    resolve(u, "av_version_info", rt.av_version_info, missing);
    resolve(u, "av_log_set_callback", rt.av_log_set_callback, missing);
    resolve(u, "av_log_set_level", rt.av_log_set_level, missing);
    resolve(u, "av_log_format_line2", rt.av_log_format_line2, missing);
    resolve(u, "av_strerror", rt.av_strerror, missing);
    resolve(u, "av_frame_alloc", rt.av_frame_alloc, missing);
    resolve(u, "av_frame_free", rt.av_frame_free, missing);
    resolve(u, "av_buffer_alloc", rt.av_buffer_alloc, missing);
    resolve(u, "av_buffer_create", rt.av_buffer_create, missing);
    resolve(u, "av_buffer_ref", rt.av_buffer_ref, missing);
    resolve(u, "av_buffer_unref", rt.av_buffer_unref, missing);
    resolve(u, "av_dict_set", rt.av_dict_set, missing);
    resolve(u, "av_dict_get", rt.av_dict_get, missing);
    resolve(u, "av_dict_free", rt.av_dict_free, missing);
    resolve(u, "av_hwdevice_ctx_alloc", rt.av_hwdevice_ctx_alloc, missing);
    resolve(u, "av_hwdevice_ctx_init", rt.av_hwdevice_ctx_init, missing);
    resolve(u, "av_hwdevice_ctx_create_derived", rt.av_hwdevice_ctx_create_derived, missing);
    resolve(u, "av_hwframe_ctx_alloc", rt.av_hwframe_ctx_alloc, missing);
    resolve(u, "av_hwframe_ctx_init", rt.av_hwframe_ctx_init, missing);
    resolve(u, "av_hwframe_ctx_create_derived", rt.av_hwframe_ctx_create_derived, missing);
    resolve(u, "av_hwframe_map", rt.av_hwframe_map, missing);
    resolve(c, "avcodec_version", rt.avcodec_version, missing);
    resolve(c, "avcodec_license", rt.avcodec_license, missing);
    resolve(c, "avcodec_find_encoder_by_name", rt.avcodec_find_encoder_by_name, missing);
    resolve(c, "avcodec_alloc_context3", rt.avcodec_alloc_context3, missing);
    resolve(c, "avcodec_free_context", rt.avcodec_free_context, missing);
    resolve(c, "avcodec_open2", rt.avcodec_open2, missing);
    resolve(c, "avcodec_send_frame", rt.avcodec_send_frame, missing);
    resolve(c, "avcodec_receive_packet", rt.avcodec_receive_packet, missing);
    resolve(c, "av_packet_alloc", rt.av_packet_alloc, missing);
    resolve(c, "av_packet_free", rt.av_packet_free, missing);
    if (!missing.empty()) {
        rt.error = "the FFmpeg DLLs in " + toUtf8(dir.c_str()) + " do not export " + missing;
        return rt;
    }
    const unsigned uv = rt.avutil_version(), cv = rt.avcodec_version();
    auto text = [](unsigned v) {
        return std::to_string(AV_VERSION_MAJOR(v)) + "." + std::to_string(AV_VERSION_MINOR(v)) + "." + std::to_string(AV_VERSION_MICRO(v));
    };
    rt.license = rt.avcodec_license() ? rt.avcodec_license() : "";
    rt.versionText = "libavcodec " + text(cv) + ", libavutil " + text(uv) + " (FFmpeg " +
                     (rt.av_version_info() ? rt.av_version_info() : "?") + ", " + rt.license + ")";
    // The struct layouts compiled in (AVFrame, AVCodecContext, AVPacket, ...)
    // are those of these major versions.
    if (AV_VERSION_MAJOR(uv) != LIBAVUTIL_VERSION_MAJOR || AV_VERSION_MAJOR(cv) != LIBAVCODEC_VERSION_MAJOR) {
        rt.error = rt.versionText + " in " + toUtf8(dir.c_str()) + ": the helper needs libavcodec " +
                   std::to_string(LIBAVCODEC_VERSION_MAJOR) + " / libavutil " + std::to_string(LIBAVUTIL_VERSION_MAJOR) + " (FFmpeg 8.x)";
        return rt;
    }
    rt.ok = true;
    return rt;
}

}  // namespace

std::string LavcRuntime::errorText(int err) const {
    char buf[256] = {};
    if (!av_strerror || av_strerror(err, buf, sizeof(buf)) < 0) return "AVERROR " + std::to_string(err);
    return std::string(buf) + " (AVERROR " + std::to_string(err) + ")";
}

void setLavcOptions(const LavcOptions& o) {
    std::lock_guard<std::mutex> lock(g_mu);
    if (g_used) {
        logf(LogLevel::Warn, "lavc: options set after the runtime was loaded: ignored");
        return;
    }
    g_options = o;
}

const LavcOptions& lavcOptions() {
    std::lock_guard<std::mutex> lock(g_mu);
    g_used = true;
    return g_options;
}

const LavcRuntime& lavcRuntime() {
    static const LavcRuntime runtime = [] {
        LavcRuntime rt = load(lavcOptions());
        if (rt.ok) {
            rt.av_log_set_level(logLevel() >= LogLevel::Debug ? AV_LOG_VERBOSE : AV_LOG_WARNING);
            g_formatLine = reinterpret_cast<void*>(rt.av_log_format_line2);
            rt.av_log_set_callback(logCallback);
            logf(LogLevel::Debug, "lavc: %s from %s", rt.versionText.c_str(), toUtf8(rt.dir.c_str()).c_str());
        } else {
            // Partly loaded libraries are useless; nothing else holds them.
            if (rt.avcodec) FreeLibrary(rt.avcodec);
            if (rt.avutil) FreeLibrary(rt.avutil);
            rt.avcodec = rt.avutil = nullptr;
        }
        return rt;
    }();
    return runtime;
}

}  // namespace recon
