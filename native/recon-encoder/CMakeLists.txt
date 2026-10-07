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
    src/control.cpp
    src/pipeline.cpp
    src/protocol.cpp
    src/registry.cpp
    src/ring.cpp
    src/platform/platform.cpp
    src/mock/replay_encoder.cpp
    src/mock/synthetic_capture.cpp
    src/amf/amf_backend.cpp
    src/nvenc/nvenc_backend.cpp
    src/capture/dda_capture.cpp
    src/capture/amd_direct_capture.cpp
    src/capture/wgc_capture.cpp
    ${CMAKE_CURRENT_BINARY_DIR}/generated/mock_clip_data.cpp
)
target_include_directories(recon-encoder PRIVATE src)
target_include_directories(recon-encoder SYSTEM PRIVATE
    ${THIRD_PARTY}/amf/include
    ${THIRD_PARTY}/nv-codec-headers/include
    ${THIRD_PARTY}/nlohmann/include
)
target_compile_definitions(recon-encoder PRIVATE
    WIN32_LEAN_AND_MEAN NOMINMAX UNICODE _UNICODE _WIN32_WINNT=0x0A00
    RECON_ENCODER_VERSION="${PROJECT_VERSION}"
)
target_link_libraries(recon-encoder PRIVATE dxgi)
# One directory for every generator (no per-config subdirectory).
set_target_properties(recon-encoder PROPERTIES RUNTIME_OUTPUT_DIRECTORY $<1:${CMAKE_BINARY_DIR}/bin>)

if(MSVC)
    target_compile_options(recon-encoder PRIVATE /W4 /permissive- /utf-8 /EHsc /Zc:__cplusplus)
else()
    target_compile_options(recon-encoder PRIVATE -Wall -Wextra)
    # C99 printf (%zu, %llu) instead of the old msvcrt one.
    target_compile_definitions(recon-encoder PRIVATE __USE_MINGW_ANSI_STDIO=1)
    # Self-contained .exe: no libstdc++ / libgcc / winpthread DLLs next to it.
    target_link_options(recon-encoder PRIVATE -static -static-libgcc -static-libstdc++)
endif()
