// DXGI Desktop Duplication capture (any vendor; the default, GUIDE 3.2).
//
// The D3D11 device is created on the adapter that owns the output (DDA
// requires it, and the encoder uses the same device). AcquireNextFrame follows
// the desktop's presents; LastPresentTime (QPC) is the frame's presentQpc. Each
// new image is copied on the GPU right away into one of two textures of our
// own (the pending image; the other holds the last delivered one, which idle
// repeats re-submit), so nothing downstream ever reads the duplication surface.
// The duplication frame itself is released just before the next
// AcquireNextFrame, as the ReleaseFrame documentation recommends (while we own
// it the OS only tracks dirty regions instead of copying every update) and as
// Sunshine does (display_base.cpp duplication_t::next_frame).
//
// DDA frames never contain the mouse pointer: frames where only the pointer
// moved (LastPresentTime 0) are skipped, and caps report cursorInVideo false
// (recon-host draws the cursor on the client; GUIDE 3.2).
//
// DXGI_ERROR_ACCESS_LOST (mode change, full-screen switch, desktop switch),
// E_ACCESSDENIED (secure desktop) and a stale DXGI factory recreate the
// duplication every 250 ms until it works again; meanwhile the last image is
// repeated, recon-host gets captureChanged "lost" / "restored", and a new size
// or rotation gives "resized" (the stream keeps its encoded size, scaled).
// A removed device (driver reset / TDR) is the fatal device_lost instead:
// nothing created on it works again (a TDR also changes the mode, so it
// usually shows up as DXGI_ERROR_ACCESS_LOST first, and then as a failing
// DuplicateOutput).
//
// AcquireNextFrame holds the device's lock while it waits (Sunshine
// display_base.cpp: "The D3D11 device is protected by an unfair lock that is
// held the entire time that IDXGIOutputDuplication::AcquireNextFrame() is
// running"), and the encoder shares this device (GUIDE 3.3/3.4; 3.4 warns not
// to run AcquireNextFrame and NVENC's Lock/UnlockBitstream from conflicting
// threads). So each wait is a short slice (kAcquireSliceMs) followed by a short
// sleep outside the call (kLockGapUs), in which the encoder's threads get the
// lock: they wait at most a slice instead of up to a frame interval (or 100 ms
// on an idle desktop), and a present is seen at most kLockGapUs late. Every
// IDXGIOutputDuplication call also holds d3d::dxgiGate(), which the NVENC
// output thread takes around NvEncLockBitstream / NvEncUnlockBitstream, so the
// two never overlap even where NVENC does not go through the device lock.
// docs/VENDOR_NOTES.md 3.2 has the VERIFY item (encoder output latency).
#include <dxgi1_5.h>

#include <algorithm>
#include <cmath>
#include <mutex>
#include <vector>

#include "capture/paced_capture.hpp"
#include "d3d/device.hpp"
#include "probes.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

constexpr int kRetryMs = 250;
constexpr UINT kAcquireSliceMs = 2;  // longest single AcquireNextFrame wait (holds the device lock)
constexpr int kLockGapUs = 500;      // pause between slices, outside the device lock

// Puts the calling thread on the desktop that currently receives input, as
// Sunshine misc.cpp syncThreadDesktop() does before duplicating (needed when
// running as SYSTEM across desktop switches; harmless otherwise). Sunshine
// closes the handle right after SetThreadDesktop, which fails ("The
// CloseDesktop function will fail if any thread in the calling process is
// using the specified desktop handle", CloseDesktop docs), so it leaks one
// handle per call (on every re-duplication: mode change, access lost, return
// from the lock screen). Here the handles stay listed while a thread is on
// them and are closed by a later call once no thread uses them any more (the
// calling thread's previous one): at most one open handle per thread that
// duplicated, however often the duplication is redone.
void syncThreadDesktop() {
    static std::mutex mu;
    static std::vector<HDESK> handles;
    HDESK input = OpenInputDesktop(0, FALSE, GENERIC_ALL);
    if (!input) return;
    if (!SetThreadDesktop(input)) {
        CloseDesktop(input);
        return;
    }
    std::lock_guard<std::mutex> lock(mu);
    // CloseDesktop fails, and the handle stays listed, while a thread uses it.
    std::erase_if(handles, [](HDESK h) { return CloseDesktop(h) != FALSE; });
    handles.push_back(input);
}

class DdaCapture : public PacedCapture {
public:
    ~DdaCapture() override { releaseHeld(); }
    const char* name() const override { return "dda"; }
    Status init(const StartParams& p) override;
    SourceInfo source() const override {
        std::lock_guard<std::mutex> lock(srcMu_);
        return src_;
    }

protected:
    Next acquire(int timeoutMs, Acquired& a, Status& err) override;
    void promote() override { cur_ = 1 - cur_; }
    void describe(CapturedFrame& out) override {
        out.texture = slots_[cur_].Get();
        out.rotation = info_[cur_].rotation;
        out.width = info_[cur_].width;
        out.height = info_[cur_].height;
    }

private:
    struct SlotInfo {
        uint32_t width = 0, height = 0;  // as displayed
        int rotation = 0;
    };

    Status duplicate();
    Status reacquire();
    void releaseHeld();
    void lose(const std::string& why, bool report = true);
    Status copyIn(ID3D11Texture2D* tex);
    int dirtyPercent(const DXGI_OUTDUPL_FRAME_INFO& fi, uint32_t w, uint32_t h);

    d3d::OutputRef output_;
    d3d::Device dev_;
    ComPtr<IDXGIFactory1> factory_;
    ComPtr<IDXGIOutputDuplication> dup_;
    bool held_ = false;   // an acquired frame is not released yet
    bool first_ = true;   // first frame of a duplication: taken even without LastPresentTime
    bool lost_ = false;
    int64_t nextRetry_ = 0;
    int64_t lastProtectedWarn_ = 0;
    int rotation_ = 0;
    uint32_t dispW_ = 0, dispH_ = 0;
    ComPtr<ID3D11Texture2D> slots_[2];
    SlotInfo info_[2];
    int cur_ = 0;  // slot of the current (last delivered) image; the pending one is 1 - cur_
    std::vector<uint8_t> meta_;
    mutable std::mutex srcMu_;
    SourceInfo src_;
};

Status DdaCapture::init(const StartParams& p) {
    Status s = d3d::selectOutput(p, output_);
    if (!s.ok) return s;
    s = d3d::createDevice(output_.adapter.Get(), dev_);
    if (!s.ok) return s;
    CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory_.GetAddressOf()));
    s = duplicate();
    if (!s.ok) return s;
    {
        std::lock_guard<std::mutex> lock(srcMu_);
        src_.device = dev_.device.Get();
        src_.adapter = output_.adapterInfo;
        src_.width = dispW_;
        src_.height = dispH_;
        src_.rotation = rotation_;
    }
    logf(LogLevel::Info, "dda: %s on %s (%s), %ux%u rotation %d, feature level %x", output_.desc.name.c_str(),
         output_.adapterInfo.name.c_str(), output_.adapterInfo.luid.c_str(), dispW_, dispH_, rotation_, unsigned(dev_.level));
    startPacing(p, dev_.device.Get());
    return Status::Ok();
}

Status DdaCapture::duplicate() {
    releaseHeld();
    dup_.Reset();
    syncThreadDesktop();
    HRESULT hr;
    {
        std::lock_guard<std::mutex> gate(d3d::dxgiGate());  // not while NVENC locks a bitstream (d3d/device.hpp)
        ComPtr<IDXGIOutput5> o5;
        if (SUCCEEDED(output_.output.As(&o5))) {
            // B8G8R8A8 only for now: an HDR (FP16) desktop is converted to it by
            // DXGI. Step 3.9 adds DXGI_FORMAT_R16G16B16A16_FLOAT for HDR streams.
            const DXGI_FORMAT formats[] = {DXGI_FORMAT_B8G8R8A8_UNORM};
            hr = o5->DuplicateOutput1(dev_.device.Get(), 0, 1, formats, dup_.GetAddressOf());
        } else {
            ComPtr<IDXGIOutput1> o1;  // before Windows 10 1703
            hr = output_.output.As(&o1);
            if (SUCCEEDED(hr)) hr = o1->DuplicateOutput(dev_.device.Get(), dup_.GetAddressOf());
        }
    }
    if (FAILED(hr)) {
        dup_.Reset();
        return Status::Error("init_failed", "DuplicateOutput on " + output_.desc.name + " failed: " + d3d::hrText(hr) +
                                                (hr == E_ACCESSDENIED ? " (secure desktop or no access to it)" : ""));
    }
    DXGI_OUTDUPL_DESC dd{};
    dup_->GetDesc(&dd);
    DXGI_OUTPUT_DESC od{};
    output_.output->GetDesc(&od);
    rotation_ = d3d::rotationDegrees(dd.Rotation);
    dispW_ = uint32_t(od.DesktopCoordinates.right - od.DesktopCoordinates.left);
    dispH_ = uint32_t(od.DesktopCoordinates.bottom - od.DesktopCoordinates.top);
    first_ = true;
    return Status::Ok();
}

void DdaCapture::releaseHeld() {
    if (held_ && dup_) {
        std::lock_guard<std::mutex> gate(d3d::dxgiGate());
        dup_->ReleaseFrame();
    }
    held_ = false;
}

void DdaCapture::lose(const std::string& why, bool report) {
    releaseHeld();
    dup_.Reset();
    nextRetry_ = qpcNow();  // retry at once, then every kRetryMs
    if (report && !lost_) {
        lost_ = true;
        CaptureEvent ev;
        ev.reason = "lost";
        ev.width = int(dispW_), ev.height = int(dispH_), ev.rotation = rotation_;
        ev.text = why;
        postEvent(ev);
    }
}

Status DdaCapture::reacquire() {
    if (!factory_ || !factory_->IsCurrent()) {
        factory_.Reset();
        CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory_.GetAddressOf()));
    }
    // The output object can be stale after a mode change: look it up again by
    // name, on the adapter our device lives on.
    d3d::OutputRef ref;
    Status s = d3d::findOutput(output_.adapterInfo.luidValue, output_.deviceName, ref);
    if (!s.ok) return s;
    output_.output = ref.output;
    output_.desc = ref.desc;
    const uint32_t oldW = dispW_, oldH = dispH_;
    const int oldRot = rotation_;
    s = duplicate();
    if (!s.ok) return s;
    CaptureEvent ev;
    ev.width = int(dispW_), ev.height = int(dispH_), ev.rotation = rotation_;
    if (lost_) {
        lost_ = false;
        ev.reason = "restored";
        postEvent(ev);
    }
    if (dispW_ != oldW || dispH_ != oldH || rotation_ != oldRot) {
        ev.reason = "resized";
        ev.text = "was " + std::to_string(oldW) + "x" + std::to_string(oldH) + " rotation " + std::to_string(oldRot);
        postEvent(ev);
        std::lock_guard<std::mutex> lock(srcMu_);
        src_.width = dispW_;
        src_.height = dispH_;
        src_.rotation = rotation_;
    }
    return Status::Ok();
}

Status DdaCapture::copyIn(ID3D11Texture2D* tex) {
    D3D11_TEXTURE2D_DESC td{};
    tex->GetDesc(&td);
    const int slot = 1 - cur_;
    D3D11_TEXTURE2D_DESC have{};
    if (slots_[slot]) slots_[slot]->GetDesc(&have);
    if (!slots_[slot] || have.Width != td.Width || have.Height != td.Height || have.Format != td.Format) {
        D3D11_TEXTURE2D_DESC nd{};
        nd.Width = td.Width;
        nd.Height = td.Height;
        nd.MipLevels = 1;
        nd.ArraySize = 1;
        nd.Format = td.Format;
        nd.SampleDesc.Count = 1;
        nd.Usage = D3D11_USAGE_DEFAULT;
        nd.BindFlags = D3D11_BIND_SHADER_RESOURCE;
        const HRESULT hr = dev_.device->CreateTexture2D(&nd, nullptr, slots_[slot].ReleaseAndGetAddressOf());
        if (FAILED(hr)) {
            Status lost;
            if (d3d::deviceRemoved(dev_.device.Get(), "creating a capture texture", lost)) return lost;
            return Status::Error("capture_failed", "creating a capture texture failed: " + d3d::hrText(hr), true);
        }
    }
    dev_.context->CopyResource(slots_[slot].Get(), tex);
    info_[slot].rotation = rotation_;
    const bool swap = rotation_ == 90 || rotation_ == 270;
    info_[slot].width = swap ? td.Height : td.Width;
    info_[slot].height = swap ? td.Width : td.Height;
    return Status::Ok();
}

int DdaCapture::dirtyPercent(const DXGI_OUTDUPL_FRAME_INFO& fi, uint32_t w, uint32_t h) {
    if (!fi.TotalMetadataBufferSize || !w || !h) return fi.TotalMetadataBufferSize ? -1 : 0;
    meta_.resize(fi.TotalMetadataBufferSize);
    UINT used = 0;
    double area = 0;
    std::lock_guard<std::mutex> gate(d3d::dxgiGate());
    // Move rects first (their destinations changed), then dirty rects.
    if (FAILED(dup_->GetFrameMoveRects(UINT(meta_.size()), reinterpret_cast<DXGI_OUTDUPL_MOVE_RECT*>(meta_.data()), &used))) {
        return -1;
    }
    const auto* moves = reinterpret_cast<const DXGI_OUTDUPL_MOVE_RECT*>(meta_.data());
    for (UINT i = 0; i < used / sizeof(DXGI_OUTDUPL_MOVE_RECT); ++i) {
        area += double(moves[i].DestinationRect.right - moves[i].DestinationRect.left) *
                double(moves[i].DestinationRect.bottom - moves[i].DestinationRect.top);
    }
    if (FAILED(dup_->GetFrameDirtyRects(UINT(meta_.size()), reinterpret_cast<RECT*>(meta_.data()), &used))) return -1;
    const auto* rects = reinterpret_cast<const RECT*>(meta_.data());
    for (UINT i = 0; i < used / sizeof(RECT); ++i) {
        area += double(rects[i].right - rects[i].left) * double(rects[i].bottom - rects[i].top);
    }
    // Overlaps are counted twice: an upper bound, capped at 100.
    return int(std::min(100.0, std::ceil(area * 100.0 / (double(w) * h))));
}

Next DdaCapture::acquire(int timeoutMs, Acquired& a, Status& err) {
    const int64_t freq = qpcFrequency();
    const int64_t deadline = qpcNow() + int64_t(timeoutMs) * freq / 1000;
    for (;;) {
        if (stopping()) return Next::Stopped;
        int64_t now = qpcNow();
        const int left = int(std::max<int64_t>(0, (deadline - now) * 1000 / freq));
        if (factory_ && !factory_->IsCurrent() && dup_) {
            // Display configuration changed (mode, HDR, outputs): re-find the
            // output and duplicate it again (DXGI recommends checking every
            // frame). Reported as lost only if that does not work at once.
            lose("display configuration changed", false);
        }
        if (!dup_) {
            if (now < nextRetry_) {
                if (now >= deadline) return Next::Timeout;
                sleepUntil(std::min(nextRetry_, deadline));
                continue;
            }
            Status s = reacquire();
            if (!s.ok) {
                // A removed device cannot duplicate anything again: fatal now
                // rather than retrying every kRetryMs for good.
                if (d3d::deviceRemoved(dev_.device.Get(), "dda: " + s.text, err)) return Next::Error;
                if (!lost_) lose(s.text);
                nextRetry_ = qpcNow() + int64_t(kRetryMs) * freq / 1000;
                logf(LogLevel::Debug, "dda: %s", s.text.c_str());
                continue;
            }
            logf(LogLevel::Info, "dda: duplicating %s again (%ux%u rotation %d)", output_.desc.name.c_str(), dispW_, dispH_, rotation_);
        }
        releaseHeld();
        DXGI_OUTDUPL_FRAME_INFO fi{};
        ComPtr<IDXGIResource> res;
        // A short slice: AcquireNextFrame holds the device lock while it waits,
        // and NVENC must not lock a bitstream meanwhile (top of file).
        HRESULT hr;
        {
            std::lock_guard<std::mutex> gate(d3d::dxgiGate());
            hr = dup_->AcquireNextFrame(std::min(UINT(left), kAcquireSliceMs), &fi, res.GetAddressOf());
        }
        now = qpcNow();
        if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
            if (now >= deadline) return Next::Timeout;
            // Outside the call the lock is free: the encoder's threads, woken
            // when it was released, take it now (an unfair lock would let an
            // immediate re-entry starve them, as Sunshine observed).
            if (!sleepUntil(now + int64_t(kLockGapUs) * freq / 1000000)) return Next::Stopped;
            continue;
        }
        if (hr == DXGI_ERROR_ACCESS_LOST || hr == DXGI_ERROR_ACCESS_DENIED || hr == E_ACCESSDENIED ||
            hr == static_cast<HRESULT>(WAIT_ABANDONED) || hr == DXGI_ERROR_INVALID_CALL || hr == DXGI_ERROR_SESSION_DISCONNECTED) {
            if (d3d::deviceRemoved(dev_.device.Get(), "AcquireNextFrame: " + d3d::hrText(hr), err)) return Next::Error;
            lose("AcquireNextFrame: " + d3d::hrText(hr));
            continue;
        }
        if (FAILED(hr)) {
            if (d3d::deviceRemoved(dev_.device.Get(), "AcquireNextFrame failed: " + d3d::hrText(hr), err)) return Next::Error;
            err = Status::Error("capture_failed", "AcquireNextFrame failed: " + d3d::hrText(hr), true);
            return Next::Error;
        }
        held_ = true;
        if (fi.ProtectedContentMaskedOut && now - lastProtectedWarn_ > 10 * freq) {
            logf(LogLevel::Warn, "dda: Windows blanks DRM-protected content in the capture");
            lastProtectedWarn_ = now;
        }
        // LastPresentTime 0: the image did not change (only the pointer moved).
        // The first frame of a duplication is taken anyway, so a static
        // desktop still produces a first image.
        if (fi.LastPresentTime.QuadPart == 0 && !first_) {
            if (now >= deadline) return Next::Timeout;
            continue;
        }
        first_ = false;
        ComPtr<ID3D11Texture2D> tex;
        if (FAILED(res.As(&tex))) {
            err = Status::Error("capture_failed", "the duplicated frame is not a D3D11 texture", true);
            return Next::Error;
        }
        Status s = copyIn(tex.Get());
        if (!s.ok) {
            err = s;
            return Next::Error;
        }
        D3D11_TEXTURE2D_DESC td{};
        tex->GetDesc(&td);
        a.presentQpc = fi.LastPresentTime.QuadPart;
        a.captureQpc = now;
        a.dirtyPct = dirtyPercent(fi, td.Width, td.Height);
        return Next::Frame;
    }
}

}  // namespace

Probe probeDdaCapture() {
    int attached = 0;
    std::string why = "no DXGI output is attached to the desktop (no display, or a session without one)";
    for (const OutputDesc& o : d3d::enumerateOutputs()) attached += o.attached;
    if (attached) return {true, std::to_string(attached) + " output(s)"};
    return {false, why};
}

std::unique_ptr<Capture> createDdaCapture(Status& err) {
    Probe p = probeDdaCapture();
    if (!p.available) {
        err = Status::Error("unavailable", "desktop duplication: " + p.reason);
        return nullptr;
    }
    return std::make_unique<DdaCapture>();
}

}  // namespace recon
