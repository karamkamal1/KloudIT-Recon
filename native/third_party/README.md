# Third-party code used by native/recon-encoder

Vendored unmodified, headers only; each directory carries its license. All MIT-licensed except
FFmpeg's headers (LGPL 2.1 or later). Nothing here is linked at build time: the AMF and NVENC
runtimes are loaded dynamically from System32 (`amfrt64.dll`, `nvEncodeAPI64.dll`) and ship
with the GPU driver; FFmpeg's libraries (`avcodec-62.dll`, `avutil-60.dll`) are loaded
dynamically from the helper's `ffmpeg-lgpl\` directory or `--ffmpeg-dir` when present
(`install-host.ps1 -InstallLibavcodec` installs BtbN's LGPL shared build there).

| Directory | Upstream | Version (pin) | License | Used for |
|---|---|---|---|---|
| `amf/include/AMF/` | https://github.com/GPUOpen-LibrariesAndSDKs/AMF, `amf/public/include` (`core/`, `components/`) | tag `v1.5.3`, commit `8c648005e07d4309033282bfd9947df2c7e76104` (its `core/Version.h` says 1.5.2) | MIT, `amf/LICENSE.txt` | AMF encoder backend (3.3), AMD Direct Capture (3.2) |
| `nv-codec-headers/include/ffnvcodec/nvEncodeAPI.h` | https://github.com/FFmpeg/nv-codec-headers (FFmpeg's copy of the NVIDIA Video Codec SDK headers) | tag `n13.0.19.0`, commit `e844e5b26f46bb77479f063029595293aa8f812d` (Video Codec SDK 13.0.19, NVENC API 13.0) | MIT (notice in the header, copied to `nv-codec-headers/LICENSE`) | NVENC backend (3.4) |
| `ffmpeg/include/{libavcodec,libavutil}/*.h` | https://github.com/FFmpeg/FFmpeg (public headers, as installed by BtbN's `ffmpeg-n8.1-latest-win64-lgpl-shared-8.1` build) | FFmpeg `n8.1.3-14-g330caae0c1` (release/8.1; libavcodec 62.28.103, libavutil 60.26.103); only the 30 headers the helper includes (`avcodec.h`, `hwcontext.h`, `hwcontext_d3d11va.h`, `opt.h` and what they include; `avconfig.h` is the build's generated one) | LGPL 2.1 or later (notice in every header; `ffmpeg/COPYING.LGPLv2.1`) | libavcodec backend (3.8): struct layouts and declarations for the run-time loaded DLLs |
| `nlohmann/include/nlohmann/json.hpp` | https://github.com/nlohmann/json, `single_include/nlohmann/json.hpp` | tag `v3.12.0`, commit `55f93686c01528224f448c19128836e7df245f72` | MIT, `nlohmann/LICENSE.MIT` | control-message JSON |

Notes

- The AMF directory is laid out as `include/AMF/{core,components}` so sources include
  `<AMF/core/Factory.h>`, as FFmpeg does. `amf/LICENSE.txt` also contains AMD's notice
  about codec patents; the headers themselves are MIT.
- NVENC API 13.0 needs an NVIDIA driver 570 or newer. The NVENC backend asks
  `NvEncodeAPIGetMaxSupportedVersion` and reports an older driver in caps
  (`unavailable.nvenc`, naming the driver to install) rather than failing at session time
  (`src/nvenc/nvenc_runtime.cpp`).
- FFmpeg: the headers give the struct layouts (`AVFrame`, `AVCodecContext`, `AVPacket`,
  `AVD3D11VADeviceContext`) of libavcodec 62 / libavutil 60, so the helper requires those major
  versions at run time (any FFmpeg 8.x) and reports others as unavailable. Nothing of FFmpeg is
  compiled into the helper beyond these headers' declarations, macros and inline functions;
  users can replace the DLLs (LGPL). The GPL `ffmpeg.exe` of the FFmpeg command-line path is
  a separate download and never loaded by the helper.
- To update: replace the files from the new upstream tag, update this table, rebuild
  (`make helper`) and run the helper tests (`make helper-test`, CI job `helper-windows`).
