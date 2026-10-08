// "synthetic-gpu": a test source that behaves like a present-driven GPU
// capture without a display. It renders a moving pattern into a D3D11 texture
// on the default adapter (WARP if there is none) whenever its simulated game
// "presents": at twice the stream's fps (at most 240 Hz) for 1 s, then nothing
// for 0.6 s, and again. So the shared PacedCapture logic (fps cap, newest
// wins, idle repeats), the NV12 conversion with its pool and the barcode run
// end to end in CI and under Wine. Not listed in caps; request it explicitly.
// With start's "motion" it is a high-motion source instead (the live-bitrate
// qualification of step 3.6): it presents without idle phases and every image
// is new: an 8 px checkerboard with a ramp scrolling 12 px right and 5 px down
// per present under full-frame noise (about +-40 per channel). Scaled to a
// 1080p stream that is far more than any bitrate the qualification asks for,
// so the encoder's rate control always decides the frame sizes.
//
// With start's hdr it plays an output in Windows HDR mode (GUIDE 3.9): FP16
// scRGB textures with values far above SDR white (up to 4000 cd/m2), a
// 1000 cd/m2 grey patch in the top-right 32x32 corner (PQ code 723 in the
// P010 output) and display metadata of a 1000 cd/m2 panel, so the HDR10
// conversion runs end to end too. The high-motion source has no HDR mode: a
// start with both motion and hdr is refused.
#include <algorithm>
#include <mutex>
#include <vector>

#include "capture/paced_capture.hpp"
#include "d3d/convert.hpp"
#include "d3d/device.hpp"
#include "probes.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

class GpuTestCapture : public PacedCapture {
public:
    const char* name() const override { return "synthetic-gpu"; }
    Status init(const StartParams& p) override;
    SourceInfo source() const override { return src_; }

protected:
    Next acquire(int timeoutMs, Acquired& a, Status& err) override;
    void promote() override { cur_ = 1 - cur_; }
    void describe(CapturedFrame& out) override {
        out.texture = slots_[cur_].Get();
        out.width = src_.width;
        out.height = src_.height;
    }

private:
    void drawPattern();
    void drawMotion();
    void drawHdr();

    d3d::Device dev_;
    ComPtr<ID3D11Texture2D> slots_[2];
    int cur_ = 0;
    SourceInfo src_;
    uint32_t bytesPerPixel_ = 4;
    std::vector<uint8_t> pixels_;
    std::vector<uint16_t> hdrRed_, hdrGreen_;  // HDR: FP16 gradients by column / row
    int64_t start_ = 0, presentPeriod_ = 0, nextPresent_ = 0;
    uint64_t presents_ = 0;
    bool motion_ = false;
    uint32_t noise_ = 0x9e3779b9u;  // xorshift32 state of the motion noise
};

Status GpuTestCapture::init(const StartParams& p) {
    Status s = d3d::createDevice(nullptr, dev_);
    if (!s.ok) s = d3d::createDevice(nullptr, dev_, true);
    if (!s.ok) return s;
    ComPtr<IDXGIDevice> dxgi;
    ComPtr<IDXGIAdapter> adapter;
    DXGI_ADAPTER_DESC ad{};
    if (SUCCEEDED(dev_.device.As(&dxgi)) && SUCCEEDED(dxgi->GetAdapter(adapter.GetAddressOf())) && SUCCEEDED(adapter->GetDesc(&ad))) {
        src_.adapter = describeAdapter(ad.VendorId, ad.AdapterLuid, ad.Description);
    }
    src_.device = dev_.device.Get();
    src_.width = 640;
    src_.height = 360;
    if (p.motion && p.hdr) return Status::Error("unsupported", "the high-motion test source (motion) has no HDR mode");
    if (p.hdr) {
        // A 1000 cd/m2 HDR panel with P3-like primaries.
        DisplayColor& d = src_.display;
        d.known = d.hdr = true;
        d.bitsPerColor = 10;
        d.red[0] = 0.680, d.red[1] = 0.320, d.green[0] = 0.265, d.green[1] = 0.690;
        d.blue[0] = 0.150, d.blue[1] = 0.060, d.white[0] = 0.3127, d.white[1] = 0.3290;
        d.minLuminance = 0.005, d.maxLuminance = 1000, d.maxFullFrameLuminance = 400;
        src_.hdr = true;
        bytesPerPixel_ = 8;
    }
    D3D11_TEXTURE2D_DESC td{};
    td.Width = src_.width;
    td.Height = src_.height;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = src_.hdr ? DXGI_FORMAT_R16G16B16A16_FLOAT : DXGI_FORMAT_B8G8R8A8_UNORM;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_SHADER_RESOURCE;
    for (auto& t : slots_) {
        const HRESULT hr = dev_.device->CreateTexture2D(&td, nullptr, t.GetAddressOf());
        if (FAILED(hr)) return Status::Error("init_failed", "creating the test texture failed: " + d3d::hrText(hr));
    }
    pixels_.resize(size_t(td.Width) * td.Height * bytesPerPixel_);
    if (src_.hdr) {
        for (uint32_t x = 0; x < src_.width; ++x) hdrRed_.push_back(d3d::toHalf(12.5f * float(x) / float(src_.width)));
        for (uint32_t y = 0; y < src_.height; ++y) hdrGreen_.push_back(d3d::toHalf(2.0f * float(y) / float(src_.height)));
    }
    motion_ = p.motion;
    const int64_t freq = qpcFrequency();
    presentPeriod_ = freq / std::min(240, 2 * p.fps);
    start_ = nextPresent_ = qpcNow();
    startPacing(p, dev_.device.Get());
    return Status::Ok();
}

Next GpuTestCapture::acquire(int timeoutMs, Acquired& a, Status&) {
    const int64_t freq = qpcFrequency();
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq / 1000;
    for (;;) {
        // 1 s of presents, then 0.6 s without any (motion: no pause).
        const int64_t cycle = freq * 16 / 10;
        const int64_t phase = (nextPresent_ - start_) % cycle;
        if (phase >= freq && !motion_) nextPresent_ += cycle - phase;
        if (nextPresent_ > deadline) {
            sleepUntil(deadline);
            return stopping() ? Next::Stopped : Next::Timeout;
        }
        if (!sleepUntil(nextPresent_)) return Next::Stopped;
        const int64_t present = nextPresent_;
        nextPresent_ += presentPeriod_;
        if (qpcNow() - present > presentPeriod_ * 4) nextPresent_ = qpcNow();  // fell behind (debugger): resync
        ++presents_;
        if (src_.hdr) drawHdr();
        else if (motion_) drawMotion();
        else drawPattern();
        dev_.context->UpdateSubresource(slots_[1 - cur_].Get(), 0, nullptr, pixels_.data(), src_.width * bytesPerPixel_, 0);
        a.presentQpc = present;
        a.captureQpc = qpcNow();
        a.dirty = 1;  // the whole test image changes
        return Next::Frame;
    }
}

// A colour that changes with every present and a bar that moves.
void GpuTestCapture::drawPattern() {
    const uint32_t w = src_.width, h = src_.height, bar = uint32_t(presents_ * 7 % w);
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            uint8_t* px = &pixels_[(size_t(y) * w + x) * 4];
            const bool onBar = x >= bar && x < bar + 16;
            px[0] = onBar ? 255 : uint8_t(x * 255 / w);
            px[1] = onBar ? 255 : uint8_t(y * 255 / h);
            px[2] = onBar ? 255 : uint8_t(presents_ * 3);
            px[3] = 255;
        }
    }
}

// High motion: a scrolling checkerboard with a ramp under full-frame noise.
void GpuTestCapture::drawMotion() {
    const uint32_t w = src_.width, h = src_.height, dx = uint32_t(presents_ * 12), dy = uint32_t(presents_ * 5);
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            uint8_t* px = &pixels_[(size_t(y) * w + x) * 4];
            const uint32_t sx = x + dx, sy = y + dy;
            const int base = (((sx >> 3) ^ (sy >> 3)) & 1 ? 180 : 60) + int((sx * 7 + sy * 3) & 31);
            noise_ ^= noise_ << 13, noise_ ^= noise_ >> 17, noise_ ^= noise_ << 5;
            for (int c = 0; c < 3; ++c) {
                const int n = int((noise_ >> (8 * c)) & 0xff) - 128;
                px[c] = uint8_t(std::clamp(base + n * 5 / 16, 0, 255));
            }
            px[3] = 255;
        }
    }
}

// HDR: scRGB (1.0 = 80 cd/m2): gradients up to 12.5 (1000 cd/m2), a
// 4000 cd/m2 bar, the fixed 1000 cd/m2 patch top right.
void GpuTestCapture::drawHdr() {
    const uint32_t w = src_.width, h = src_.height, bar = uint32_t(presents_ * 7 % (w - 64));
    const uint16_t patch = d3d::toHalf(12.5f), bright = d3d::toHalf(50.0f), one = d3d::toHalf(1.0f);
    const uint16_t blue = d3d::toHalf(float(presents_ * 3 % 256) / 255.0f);
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            auto* px = reinterpret_cast<uint16_t*>(&pixels_[(size_t(y) * w + x) * 8]);
            if (x >= w - 32 && y < 32) {
                px[0] = px[1] = px[2] = patch;
            } else if (x >= bar && x < bar + 16) {
                px[0] = px[1] = px[2] = bright;
            } else {
                px[0] = hdrRed_[x], px[1] = hdrGreen_[y], px[2] = blue;
            }
            px[3] = one;
        }
    }
}

}  // namespace

std::unique_ptr<Capture> createGpuTestCapture(Status&) { return std::make_unique<GpuTestCapture>(); }

}  // namespace recon
