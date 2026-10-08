# recon-encoder.exe: the native capture + encode helper (GUIDE Phase 3).
#
# Windows (MSVC):
#   cmake -S native/recon-encoder -B build/recon-encoder -A x64
#   cmake --build build/recon-encoder --config Release
# Linux (mingw-w64 cross build; `make helper` does this):
#   cmake -S native/recon-encoder -B build/recon-encoder \
#         -DCMAKE_TOOLCHAIN_FILE=native/recon-encoder/cmake/mingw-w64-x86_64.cmake
#   cmake --build build/recon-encoder
# The executable lands in <build>/bin/recon-encoder.exe.
cmake_minimum_required(VERSION 3.21)
project(recon-encoder VERSION 0.1.0 LANGUAGES CXX)

if(NOT WIN32)
    message(FATAL_ERROR "recon-encoder is a Windows program: build it with MSVC, or on Linux with "
                        "-DCMAKE_TOOLCHAIN_FILE=cmake/mingw-w64-x86_64.cmake")
endif()

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release)
endif()
# Static C++ runtime: the helper ships as a single .exe without a VC++ redistributable.
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")

set(THIRD_PARTY ${CMAKE_CURRENT_SOURCE_DIR}/../third_party)

include(cmake/embed.cmake)
embed_binary(${CMAKE_CURRENT_SOURCE_DIR}/testdata/mock_clip.h264 kMockClip
             ${CMAKE_CURRENT_BINARY_DIR}/generated/mock_clip_data.cpp)

add_executable(recon-encoder
    src/main.cpp
    src/stream.cpp
    src/encode_test.cpp
    src/control.cpp
    src/pipeline.cpp
    src/protocol.cpp
    src/registry.cpp
    src/ring.cpp
    src/platform/platform.cpp
    src/platform/gpu_sched.cpp
    src/d3d/device.cpp
    src/d3d/convert.cpp
    src/d3d/selftest.cpp
    src/capture/dirty.cpp
    src/capture/pacer.cpp
    src/capture/paced_capture.cpp
    src/mock/replay_encoder.cpp
    src/mock/synthetic_capture.cpp
    src/amf/amf_backend.cpp
    src/amf/amf_runtime.cpp
    src/codec/bitstream.cpp
    src/codec/hdr.cpp
    src/codec/ltr.cpp
    src/codec/rfi.cpp
    src/codec/slices.cpp
    src/codec/selftest.cpp
    src/nvenc/nvenc_backend.cpp
    src/nvenc/nvenc_policy.cpp
    src/nvenc/nvenc_runtime.cpp
    src/nvenc/selftest.cpp
    src/lavc/lavc_backend.cpp
    src/lavc/lavc_runtime.cpp
    src/capture/dda_capture.cpp
    src/capture/amd_direct_capture.cpp
    src/capture/wgc_capture.cpp
    src/capture/test_capture.cpp
    ${CMAKE_CURRENT_BINARY_DIR}/generated/mock_clip_data.cpp
)
target_include_directories(recon-encoder PRIVATE src)
target_include_directories(recon-encoder SYSTEM PRIVATE
    ${THIRD_PARTY}/amf/include
    ${THIRD_PARTY}/nv-codec-headers/include
    ${THIRD_PARTY}/nlohmann/include
    ${THIRD_PARTY}/ffmpeg/include
)
target_compile_definitions(recon-encoder PRIVATE
    WIN32_LEAN_AND_MEAN NOMINMAX UNICODE _UNICODE _WIN32_WINNT=0x0A00
    RECON_ENCODER_VERSION="${PROJECT_VERSION}"
)
target_link_libraries(recon-encoder PRIVATE d3d11 dxgi)

# Windows.Graphics.Capture is written against C++/WinRT: part of the Windows
# SDK for MSVC (linked through WindowsApp.lib). mingw-w64 has no C++/WinRT
# headers; generated ones can be supplied (cppwinrt -in <SDK winmd> -out DIR),
# otherwise the mingw build reports WGC unavailable (src/capture/wgc_capture.cpp).
set(RECON_CPPWINRT_INCLUDE "" CACHE PATH "Directory with generated C++/WinRT headers (mingw builds only)")
if(MSVC)
    target_link_libraries(recon-encoder PRIVATE windowsapp)
elseif(RECON_CPPWINRT_INCLUDE)
    target_include_directories(recon-encoder SYSTEM PRIVATE ${RECON_CPPWINRT_INCLUDE})
    target_link_libraries(recon-encoder PRIVATE runtimeobject ole32 oleaut32)
endif()
# One directory for every generator (no per-config subdirectory).
set_target_properties(recon-encoder PROPERTIES RUNTIME_OUTPUT_DIRECTORY $<1:${CMAKE_BINARY_DIR}/bin>)

if(MSVC)
    target_compile_options(recon-encoder PRIVATE /W4 /permissive- /utf-8 /EHsc /Zc:__cplusplus /bigobj)
else()
    target_compile_options(recon-encoder PRIVATE -Wall -Wextra)
    # C99 printf (%zu, %llu) instead of the old msvcrt one.
    target_compile_definitions(recon-encoder PRIVATE __USE_MINGW_ANSI_STDIO=1)
    # Self-contained .exe: no libstdc++ / libgcc / winpthread DLLs next to it.
    target_link_options(recon-encoder PRIVATE -static -static-libgcc -static-libstdc++)
endif()

# recon-fake-nvenc.dll: a test double of the NVIDIA driver's NVENC runtime
# (test/fake_nvenc.cpp) for `recon-encoder --self-test-nvenc=DLL`, which loads
# it by its path. Never shipped or installed: the helper itself loads
# nvEncodeAPI64.dll from System32 only, and releases bundle recon-encoder.exe alone.
add_library(recon-fake-nvenc SHARED test/fake_nvenc.cpp test/fake_nvenc.def)
target_include_directories(recon-fake-nvenc SYSTEM PRIVATE ${THIRD_PARTY}/nv-codec-headers/include)
target_compile_definitions(recon-fake-nvenc PRIVATE WIN32_LEAN_AND_MEAN NOMINMAX UNICODE _UNICODE _WIN32_WINNT=0x0A00)
set_target_properties(recon-fake-nvenc PROPERTIES PREFIX "" RUNTIME_OUTPUT_DIRECTORY $<1:${CMAKE_BINARY_DIR}/bin>)
if(MSVC)
    target_compile_options(recon-fake-nvenc PRIVATE /W4 /permissive- /utf-8 /EHsc)
else()
    target_compile_options(recon-fake-nvenc PRIVATE -Wall -Wextra)
    target_compile_definitions(recon-fake-nvenc PRIVATE __USE_MINGW_ANSI_STDIO=1)
    target_link_options(recon-fake-nvenc PRIVATE -static -static-libgcc -static-libstdc++)
endif()
