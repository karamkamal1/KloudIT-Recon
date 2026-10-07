// "synthetic-gpu": a test source that behaves like a present-driven GPU
// capture without a display. It renders a moving pattern into a D3D11 texture
// on the default adapter (WARP if there is none) whenever its simulated game
// "presents": at twice the stream's fps (at most 240 Hz) for 1 s, then nothing
// for 0.6 s, and again. So the shared PacedCapture logic (fps cap, newest
// wins, idle repeats), the NV12 conversion with its pool and the barcode run
// end to end in CI and under Wine. Not listed in caps; request it explicitly.
#include <algorithm>
#include <mutex>
#include <vector>

#include "capture/paced_capture.hpp"
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
    d3d::Device dev_;
    ComPtr<ID3D11Texture2D> slots_[2];
    int cur_ = 0;
    SourceInfo src_;
    std::vector<uint8_t> pixels_;
    int64_t start_ = 0, presentPeriod_ = 0, nextPresent_ = 0;
    uint64_t presents_ = 0;
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
    D3D11_TEXTURE2D_DESC td{};
    td.Width = src_.width;
    td.Height = src_.height;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_SHADER_RESOURCE;
    for (auto& t : slots_) {
        const HRESULT hr = dev_.device->CreateTexture2D(&td, nullptr, t.GetAddressOf());
        if (FAILED(hr)) return Status::Error("init_failed", "creating the test texture failed: " + d3d::hrText(hr));
    }
    pixels_.resize(size_t(td.Width) * td.Height * 4);
    const int64_t freq = qpcFrequency();
    presentPeriod_ = freq / std::min(240, 2 * p.fps);
    start_ = nextPresent_ = qpcNow();
    startPacing(p);
    return Status::Ok();
}

Next GpuTestCapture::acquire(int timeoutMs, Acquired& a, Status&) {
    const int64_t freq = qpcFrequency();
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq / 1000;
    for (;;) {
        // 1 s of presents, then 0.6 s without any.
        const int64_t cycle = freq * 16 / 10;
        const int64_t phase = (nextPresent_ - start_) % cycle;
        if (phase >= freq) nextPresent_ += cycle - phase;
        if (nextPresent_ > deadline) {
            sleepUntil(deadline);
            return stopping() ? Next::Stopped : Next::Timeout;
        }
        if (!sleepUntil(nextPresent_)) return Next::Stopped;
        const int64_t present = nextPresent_;
        nextPresent_ += presentPeriod_;
        if (qpcNow() - present > presentPeriod_ * 4) nextPresent_ = qpcNow();  // fell behind (debugger): resync
        // A colour that changes with every present and a bar that moves.
        ++presents_;
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
        dev_.context->UpdateSubresource(slots_[1 - cur_].Get(), 0, nullptr, pixels_.data(), w * 4, 0);
        a.presentQpc = present;
        a.captureQpc = qpcNow();
        a.dirtyPct = 100;
        return Next::Frame;
    }
}

}  // namespace

std::unique_ptr<Capture> createGpuTestCapture(Status&) { return std::make_unique<GpuTestCapture>(); }

}  // namespace recon
