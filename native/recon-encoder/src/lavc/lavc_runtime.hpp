// The libavcodec runtime of the fallback encoder backend (GUIDE 3.8): FFmpeg's
// shared libraries avutil-60.dll and avcodec-62.dll (with its dependency
// swresample-6.dll), FFmpeg 8.x, loaded at run time from one directory (never
// required: without them the backend is "unavailable" with the reason). The
// helper compiles against FFmpeg 8.1's public headers
// (native/third_party/ffmpeg) and calls the libraries only through the function
// pointers below, so the binary links nothing of FFmpeg and runs without it.
//
// Where the DLLs are looked for, in this order: --ffmpeg-dir=DIR when given
// (only there; a relative DIR from the current directory); else <helper dir>\ffmpeg-lgpl (where install-host.ps1
// -InstallLibavcodec puts BtbN's LGPL shared build), then the helper's own
// directory. Both are under the install directory, which only administrators
// can change. Each DLL is loaded by its full path with
// LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32, so its
// dependencies come from the same directory or System32 and never from the
// current directory or PATH.
#pragma once

#include <windows.h>

#include <string>
#include <vector>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/hwcontext.h>
#include <libavutil/opt.h>
}

namespace recon {

struct LavcRuntime {
    bool ok = false;
    std::string error;        // why not ok
    std::wstring dir;         // where the DLLs were loaded from
    std::string versionText;  // "libavcodec 62.28.103, libavutil 60.26.103 (n8.1.3-..., LGPL version 3 or later)"
    std::string license;      // avcodec_license()
    HMODULE avutil = nullptr, avcodec = nullptr;  // kept loaded for the process

    // libavutil
    decltype(&::avutil_version) avutil_version = nullptr;
    decltype(&::av_version_info) av_version_info = nullptr;
    decltype(&::av_log_set_callback) av_log_set_callback = nullptr;
    decltype(&::av_log_set_level) av_log_set_level = nullptr;
    decltype(&::av_log_format_line2) av_log_format_line2 = nullptr;
    decltype(&::av_strerror) av_strerror = nullptr;
    decltype(&::av_frame_alloc) av_frame_alloc = nullptr;
    decltype(&::av_frame_free) av_frame_free = nullptr;
    decltype(&::av_buffer_alloc) av_buffer_alloc = nullptr;
    decltype(&::av_buffer_create) av_buffer_create = nullptr;
    decltype(&::av_buffer_ref) av_buffer_ref = nullptr;
    decltype(&::av_buffer_unref) av_buffer_unref = nullptr;
    decltype(&::av_dict_set) av_dict_set = nullptr;
    decltype(&::av_dict_get) av_dict_get = nullptr;
    decltype(&::av_dict_free) av_dict_free = nullptr;
    decltype(&::av_hwdevice_ctx_alloc) av_hwdevice_ctx_alloc = nullptr;
    decltype(&::av_hwdevice_ctx_init) av_hwdevice_ctx_init = nullptr;
    decltype(&::av_hwdevice_ctx_create_derived) av_hwdevice_ctx_create_derived = nullptr;
    decltype(&::av_hwframe_ctx_alloc) av_hwframe_ctx_alloc = nullptr;
    decltype(&::av_hwframe_ctx_init) av_hwframe_ctx_init = nullptr;
    decltype(&::av_hwframe_ctx_create_derived) av_hwframe_ctx_create_derived = nullptr;
    decltype(&::av_hwframe_map) av_hwframe_map = nullptr;

    // libavcodec
    decltype(&::avcodec_version) avcodec_version = nullptr;
    decltype(&::avcodec_license) avcodec_license = nullptr;
    decltype(&::avcodec_find_encoder_by_name) avcodec_find_encoder_by_name = nullptr;
    decltype(&::avcodec_alloc_context3) avcodec_alloc_context3 = nullptr;
    decltype(&::avcodec_free_context) avcodec_free_context = nullptr;
    decltype(&::avcodec_open2) avcodec_open2 = nullptr;
    decltype(&::avcodec_send_frame) avcodec_send_frame = nullptr;
    decltype(&::avcodec_receive_packet) avcodec_receive_packet = nullptr;
    decltype(&::av_packet_alloc) av_packet_alloc = nullptr;
    decltype(&::av_packet_free) av_packet_free = nullptr;

    // "Invalid argument (AVERROR -22)".
    std::string errorText(int err) const;
};

// What --ffmpeg-dir and --lavc-test-encoder set (main.cpp); must be called
// before anything uses lavcRuntime() / the lavc probe.
struct LavcOptions {
    std::wstring dir;  // empty = <helper dir>\ffmpeg-lgpl, then <helper dir>
    // Test only (--lavc-test-encoder): libavcodec encoders by name (e.g.
    // libx264 from a GPL shared build) that the backend drives instead of
    // h264_qsv / hevc_qsv / av1_qsv, through the same code path with frames in
    // system memory, so the backend can be checked on any machine (Wine).
    std::vector<std::string> testEncoders;
};
void setLavcOptions(const LavcOptions& o);
const LavcOptions& lavcOptions();

// Loaded once per process. FFmpeg's log goes to the helper's log (FFmpeg
// errors as warnings, warnings as info, the rest at debug).
const LavcRuntime& lavcRuntime();

}  // namespace recon
