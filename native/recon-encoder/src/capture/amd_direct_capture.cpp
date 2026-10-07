// AMD Direct Capture (AMF "AMFDisplayCapture" component; AMD only, opt-in
// until it beats DDA in the Phase 0 measurements; GUIDE 3.2 (2)).
//
// Set-up follows AMF_Display_Capture_API.md and the AMD Streaming SDK sample
// (samples/RemoteDesktopServer: RemoteDesktopServer.cpp InitVideoCapture,
// RemoteDesktopServerWin.cpp, AVStreamer.cpp CaptureVideo):
//   context->InitDX11(our device on the output's adapter);
//   factory->CreateComponent(context, AMFDisplayCapture);
//   MONITOR_INDEX, MODE = WAIT_FOR_PRESENT, FRAMERATE = (0, 1) (follow the
//   flips of the game or DWM), ENABLE_DIRTY_RECTS = true, DUPLICATEOUTPUT = true;
//   Init(AMF_SURFACE_UNKNOWN, 0, 0); poll QueryOutput, sleeping >= 1 ms on AMF_REPEAT.
// Per surface: FRAME_FLIP_TIMESTAMP (QPC) is presentQpc, DIRTY_RECTS (an
// AMFBuffer of AMFRect) gives dirtyPct, DisplayCaptureDCC says whether the
// surface is DCC compressed.
//
// DUPLICATEOUTPUT: the pipeline keeps a pending and a current surface (newest
// wins, idle repeats re-submit the current one), and the capture component
// may otherwise overwrite a surface it handed out while it is still in use
// (the Streaming SDK enables it whenever a surface goes to the encoder
// directly, AVStreamer.cpp). The copy costs the component < 1 ms per frame.
//
// Surfaces reach the encoder through the NV12 converter (it samples the
// surface's D3D11 texture; DCC is resolved by the shader read). When the AMF
// encoder (3.3) runs on the same AMFContext (SourceInfo::amfContext), it can
// take CapturedFrame::amfSurface directly instead, unless amfDcc (DCC surfaces
// cannot be submitted to the encoder as they are: AMF_Display_Capture_API.md).
//
// VERIFY on hardware (docs/VENDOR_NOTES.md): MONITOR_INDEX = the output's
// index on its adapter (the doc says "determined by EnumAdapters"), AMF
// rotation values vs DXGI, exclusive full screen, HDR desktops (FP16 surfaces),
// IddCx virtual displays (probably unsupported: use DDA).
#include <algorithm>
#include <cmath>
#include <mutex>

#include <AMF/components/DisplayCapture.h>
#include <AMF/core/Buffer.h>
#include <AMF/core/Context.h>
#include <AMF/core/Surface.h>

#include "amf/amf_runtime.hpp"
#include "capture/paced_capture.hpp"
#include "d3d/device.hpp"
#include "probes.hpp"

namespace recon {

namespace {

constexpr int kRetryMs = 250;

std::string amfError(const char* what, AMF_RESULT r) { return std::string(what) + " failed (AMF_RESULT " + std::to_string(int(r)) + ")"; }

int rotationFromAmf(amf_int64 r) {
    switch (r) {
    case amf::AMF_ROTATION_90: return 90;
    case amf::AMF_ROTATION_180: return 180;
    case amf::AMF_ROTATION_270: return 270;
    default: return 0;
    }
}

class AmdDirectCapture : public PacedCapture {
public:
    ~AmdDirectCapture() override {
        pending_ = nullptr;
        current_ = nullptr;
        if (comp_) comp_->Terminate();
        comp_ = nullptr;
        if (ctx_) ctx_->Terminate();
        ctx_ = nullptr;
    }
    const char* name() const override { return "amd-direct"; }
    Status init(const StartParams& p) override;
    SourceInfo source() const override {
        std::lock_guard<std::mutex> lock(srcMu_);
        return src_;
    }

protected:
    Next acquire(int timeoutMs, Acquired& a, Status& err) override;
    void promote() override {
        current_ = pending_;
        pending_ = nullptr;
    }
    void describe(CapturedFrame& out) override;

private:
    Status initComponent();
    int dirtyPercent(amf::AMFSurface* s, amf_int32 w, amf_int32 h);

    d3d::OutputRef output_;
    d3d::Device dev_;
    amf::AMFContextPtr ctx_;
    amf::AMFComponentPtr comp_;
    amf::AMFSurfacePtr pending_, current_;
    int rotation_ = 0;
    amf_int32 lastW_ = 0, lastH_ = 0;
    bool broken_ = false;
    int64_t nextRetry_ = 0;
    mutable std::mutex srcMu_;
    SourceInfo src_;
};

Status AmdDirectCapture::init(const StartParams& p) {
    const AmfRuntime& rt = amfRuntime();
    if (!rt.factory) return Status::Error("unavailable", rt.error);
    Status s = d3d::selectOutput(p, output_);
    if (!s.ok) return s;
    if (output_.adapterInfo.vendor != "amd") {
        return Status::Error("unavailable", "AMD Direct Capture needs an AMD adapter; " + output_.desc.name + " is on " +
                                                output_.adapterInfo.name + " (" + output_.adapterInfo.vendor + ")");
    }
    s = d3d::createDevice(output_.adapter.Get(), dev_);
    if (!s.ok) return s;
    AMF_RESULT r = rt.factory->CreateContext(&ctx_);
    if (r != AMF_OK) return Status::Error("init_failed", amfError("AMFFactory::CreateContext", r));
    // OBS texture-amf.cpp asks for AMF_DX11_1 on its device; 11_0 otherwise.
    r = ctx_->InitDX11(dev_.device.Get(), dev_.level >= D3D_FEATURE_LEVEL_11_1 ? amf::AMF_DX11_1 : amf::AMF_DX11_0);
    if (r != AMF_OK) return Status::Error("init_failed", amfError("AMFContext::InitDX11", r));
    r = rt.factory->CreateComponent(ctx_, AMFDisplayCapture, &comp_);
    if (r != AMF_OK) {
        return Status::Error("unavailable", amfError("creating AMFDisplayCapture (driver without AMD Direct Capture?)", r));
    }
    s = initComponent();
    if (!s.ok) return s;
    {
        std::lock_guard<std::mutex> lock(srcMu_);
        src_.device = dev_.device.Get();
        src_.adapter = output_.adapterInfo;
        src_.amfContext = ctx_.GetPtr();
    }
    logf(LogLevel::Info, "amd-direct: %s (monitor index %d) on %s, AMF runtime %s", output_.desc.name.c_str(),
         output_.desc.outputIndex, output_.adapterInfo.name.c_str(), rt.versionText.c_str());
    startPacing(p);
    return Status::Ok();
}

Status AmdDirectCapture::initComponent() {
    // MONITOR_INDEX: "determined by EnumAdapters in DXGI" (DisplayCapture.h);
    // we pass the output's index on its adapter. VERIFY with several monitors.
    comp_->SetProperty(AMF_DISPLAYCAPTURE_MONITOR_INDEX, amf_int64(output_.desc.outputIndex));
    AMF_RESULT r = comp_->SetProperty(AMF_DISPLAYCAPTURE_MODE, amf_int64(AMF_DISPLAYCAPTURE_MODE_WAIT_FOR_PRESENT));
    if (r != AMF_OK) return Status::Error("init_failed", amfError("AMF_DISPLAYCAPTURE_MODE = WAIT_FOR_PRESENT", r));
    comp_->SetProperty(AMF_DISPLAYCAPTURE_FRAMERATE, AMFConstructRate(0, 1));  // driven by flips, not a rate
    comp_->SetProperty(AMF_DISPLAYCAPTURE_ENABLE_DIRTY_RECTS, true);
    if (comp_->SetProperty(AMF_DISPLAYCAPTURE_DUPLICATEOUTPUT, true) != AMF_OK) {
        logf(LogLevel::Warn, "amd-direct: AMF_DISPLAYCAPTURE_DUPLICATEOUTPUT not supported");
    }
    r = comp_->Init(amf::AMF_SURFACE_UNKNOWN, 0, 0);
    if (r != AMF_OK) return Status::Error("init_failed", amfError("AMFDisplayCapture::Init", r));
    AMFSize size{};
    amf_int64 rot = 0, format = 0;
    comp_->GetProperty(AMF_DISPLAYCAPTURE_RESOLUTION, &size);
    comp_->GetProperty(AMF_DISPLAYCAPTURE_ROTATION, &rot);
    comp_->GetProperty(AMF_DISPLAYCAPTURE_FORMAT, &format);
    rotation_ = rotationFromAmf(rot);
    std::lock_guard<std::mutex> lock(srcMu_);
    // RESOLUTION is the screen size; rotated displays report it as displayed (VERIFY).
    src_.width = uint32_t(size.width > 0 ? size.width : output_.desc.width);
    src_.height = uint32_t(size.height > 0 ? size.height : output_.desc.height);
    src_.rotation = rotation_;
    logf(LogLevel::Debug, "amd-direct: resolution %dx%d, rotation %d, surface format %lld", size.width, size.height, rotation_,
         static_cast<long long>(format));
    return Status::Ok();
}

int AmdDirectCapture::dirtyPercent(amf::AMFSurface* s, amf_int32 w, amf_int32 h) {
    amf::AMFInterfacePtr iface;
    if (s->GetProperty(AMF_DISPLAYCAPTURE_DIRTY_RECTS, &iface) != AMF_OK || !iface) return -1;
    amf::AMFBufferPtr buf(iface);
    if (!buf || w <= 0 || h <= 0) return -1;
    const auto* rects = static_cast<const AMFRect*>(buf->GetNative());
    const size_t n = buf->GetSize() / sizeof(AMFRect);
    if (!rects && n) return -1;
    double area = 0;
    for (size_t i = 0; i < n; ++i) area += double(rects[i].right - rects[i].left) * double(rects[i].bottom - rects[i].top);
    return int(std::min(100.0, std::ceil(area * 100.0 / (double(w) * h))));
}

Next AmdDirectCapture::acquire(int timeoutMs, Acquired& a, Status& err) {
    const int64_t freq = qpcFrequency();
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq / 1000;
    for (;;) {
        if (stopping()) return Next::Stopped;
        int64_t now = qpcNow();
        if (broken_) {
            // The component failed (mode change, display gone): re-initialize
            // it every kRetryMs; the last image is repeated meanwhile.
            if (now < nextRetry_) {
                if (now >= deadline) return Next::Timeout;
                sleepUntil(std::min(nextRetry_, deadline));
                continue;
            }
            comp_->Terminate();
            Status s = initComponent();
            if (!s.ok) {
                nextRetry_ = qpcNow() + int64_t(kRetryMs) * freq / 1000;
                continue;
            }
            broken_ = false;
            CaptureEvent ev;
            ev.reason = "restored";
            ev.width = int(src_.width), ev.height = int(src_.height), ev.rotation = rotation_;
            postEvent(ev);
        }
        amf::AMFDataPtr data;
        const AMF_RESULT r = comp_->QueryOutput(&data);
        now = qpcNow();
        if (r == AMF_REPEAT || (r == AMF_OK && !data)) {
            if (now >= deadline) return Next::Timeout;
            Sleep(1);  // "sleep for at least 1 ms" on AMF_REPEAT (AMF_Display_Capture_API.md 2.3)
            continue;
        }
        if (r != AMF_OK) {
            if (r == AMF_EOF) return Next::Stopped;
            broken_ = true;
            nextRetry_ = now + int64_t(kRetryMs) * freq / 1000;
            CaptureEvent ev;
            ev.reason = "lost";
            ev.width = int(src_.width), ev.height = int(src_.height), ev.rotation = rotation_;
            ev.text = amfError("QueryOutput", r);
            postEvent(ev);
            continue;
        }
        amf::AMFSurfacePtr surface(data);
        // More presents may be queued: keep only the newest (newest wins),
        // adding up the dirty areas of the ones skipped.
        int dirty = surface ? dirtyPercent(surface, lastW_, lastH_) : -1;
        for (;;) {
            amf::AMFDataPtr more;
            if (comp_->QueryOutput(&more) != AMF_OK || !more) break;
            amf::AMFSurfacePtr newer(more);
            if (!newer) break;
            const int d = dirtyPercent(newer, lastW_, lastH_);
            dirty = dirty < 0 || d < 0 ? -1 : std::min(100, dirty + d);
            surface = newer;
        }
        amf::AMFPlane* plane = surface ? surface->GetPlaneAt(0) : nullptr;
        if (!plane || !plane->GetNative()) {
            err = Status::Error("capture_failed", "AMD Direct Capture returned a surface without a D3D11 texture");
            return Next::Error;
        }
        const amf_int32 w = plane->GetWidth(), h = plane->GetHeight();
        if ((lastW_ || lastH_) && (w != lastW_ || h != lastH_)) {
            CaptureEvent ev;
            ev.reason = "resized";
            ev.width = int(w), ev.height = int(h), ev.rotation = rotation_;
            ev.text = "was " + std::to_string(lastW_) + "x" + std::to_string(lastH_);
            postEvent(ev);
            std::lock_guard<std::mutex> lock(srcMu_);
            src_.width = uint32_t(w);
            src_.height = uint32_t(h);
            dirty = 100;
        }
        if (!lastW_) dirty = dirtyPercent(surface, w, h);
        lastW_ = w, lastH_ = h;
        amf_int64 flip = 0;
        surface->GetProperty(AMF_DISPLAYCAPTURE_FRAME_FLIP_TIMESTAMP, &flip);
        a.presentQpc = flip;  // QueryPerformanceCounter ticks (AMF_Display_Capture_API.md 2.5)
        a.captureQpc = now;
        a.dirtyPct = dirty;
        pending_ = surface;  // an older pending surface is dropped here (newest wins)
        return Next::Frame;
    }
}

void AmdDirectCapture::describe(CapturedFrame& out) {
    amf::AMFPlane* plane = current_->GetPlaneAt(0);
    out.texture = static_cast<ID3D11Texture2D*>(plane->GetNative());
    amf_int64 rot = 0;
    current_->GetProperty(AMF_SURFACE_ROTATION, &rot);
    out.rotation = rot ? rotationFromAmf(rot) : rotation_;
    const bool swap = out.rotation == 90 || out.rotation == 270;
    out.width = uint32_t(swap ? plane->GetHeight() : plane->GetWidth());
    out.height = uint32_t(swap ? plane->GetWidth() : plane->GetHeight());
    out.amfSurface = current_.GetPtr();
    bool dcc = false;
    current_->GetProperty(AMF_DISPLAY_CAPTURE_DCC, &dcc);
    out.amfDcc = dcc;
}

}  // namespace

Probe probeAmdDirectCapture() {
    bool amd = false;
    for (const OutputDesc& o : d3d::enumerateOutputs()) amd = amd || (o.attached && o.vendor == "amd");
    if (!amd) return {false, "no display output on an AMD adapter"};
    const AmfRuntime& rt = amfRuntime();
    if (!rt.factory) return {false, rt.error};
    return {true, "AMF runtime " + rt.versionText + " (opt-in: capture \"amd-direct\")"};
}

std::unique_ptr<Capture> createAmdDirectCapture(Status& err) {
    Probe p = probeAmdDirectCapture();
    if (!p.available) {
        err = Status::Error("unavailable", "AMD Direct Capture: " + p.reason);
        return nullptr;
    }
    return std::make_unique<AmdDirectCapture>();
}

}  // namespace recon
