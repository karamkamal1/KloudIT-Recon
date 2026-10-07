# Cross-compile recon-encoder.exe on Linux with mingw-w64 (apt-get install mingw-w64):
#   cmake -S native/recon-encoder -B build -DCMAKE_TOOLCHAIN_FILE=native/recon-encoder/cmake/mingw-w64-x86_64.cmake
# The posix thread model is preferred: std::thread / std::mutex need it on
# older GCCs, and the build links statically, so no winpthread DLL is needed.
set(CMAKE_SYSTEM_NAME Windows)
set(CMAKE_SYSTEM_PROCESSOR x86_64)
set(MINGW_PREFIX x86_64-w64-mingw32)

find_program(MINGW_CXX NAMES ${MINGW_PREFIX}-g++-posix ${MINGW_PREFIX}-g++ REQUIRED)
find_program(MINGW_RC NAMES ${MINGW_PREFIX}-windres)
set(CMAKE_CXX_COMPILER ${MINGW_CXX})
if(MINGW_RC)
    set(CMAKE_RC_COMPILER ${MINGW_RC})
endif()

set(CMAKE_FIND_ROOT_PATH /usr/${MINGW_PREFIX})
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
