// Self-tests that run without a display or GPU (CI on windows-latest, Wine):
// recon-encoder --self-test-convert | --self-test-pacer | --self-test-encoder.
#pragma once

namespace recon {

// Exit code of a self-test that could not run here (no D3D11 device).
constexpr int kSelfTestSkip = 77;

// BGRA -> NV12 conversion on a WARP device (hardware: the default adapter)
// against a CPU reference (d3d/selftest.cpp).
int runConvertSelfTest(bool hardware = false);
// Frame pacing policy against simulated present patterns (capture/pacer.cpp).
int runPacerSelfTest();
// Encoder-independent logic: the LTR recovery policy, parameter sets on key
// frames, ROI importance maps (codec/selftest.cpp).
int runEncoderSelfTest();

}  // namespace recon
