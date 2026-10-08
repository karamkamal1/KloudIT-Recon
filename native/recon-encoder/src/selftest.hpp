// Self-tests that run without a display or GPU (CI on windows-latest, Wine):
// recon-encoder --self-test-convert | --self-test-pacer | --self-test-encoder |
// --self-test-nvenc=DLL (with the NVENC test double; without DLL it needs an
// NVIDIA GPU).
#pragma once

#include <string>

namespace recon {

// Exit code of a self-test that could not run here (no D3D11 device).
constexpr int kSelfTestSkip = 77;

// BGRA -> NV12 conversion on a WARP device (hardware: the default adapter)
// against a CPU reference (d3d/selftest.cpp).
int runConvertSelfTest(bool hardware = false);
// Frame pacing policy against simulated present patterns (capture/pacer.cpp).
int runPacerSelfTest();
// Encoder-independent logic: the LTR and invalidation recovery policies,
// parameter sets on key frames, ROI maps, NVENC settings (codec/selftest.cpp).
int runEncoderSelfTest();
// The NVENC backend against the test double testDouble (a DLL path: no GPU
// needed) or, with an empty path, the NVIDIA driver (nvenc/selftest.cpp).
int runNvencSelfTest(const std::wstring& testDouble);

}  // namespace recon
