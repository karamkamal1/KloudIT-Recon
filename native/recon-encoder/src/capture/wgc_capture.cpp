// Windows.Graphics.Capture (GUIDE 3.2 (3)): the fallback for per-window
// capture and for outputs DDA cannot duplicate. Written against C++/WinRT,
// which the Windows SDK ships for MSVC (the CI / release build); a mingw-w64
// build without the C++/WinRT headers reports it unavailable (CMake option
// RECON_CPPWINRT_INCLUDE points a mingw build at generated headers).
//
// Frames come from a free-threaded frame pool (2 buffers). The FrameArrived
// handler only keeps the newest frame; the capture thread copies it into one
// of two textures of our own and closes it at once, so WGC always has a free
// buffer (the same pending / current scheme as DDA). SystemRelativeTime (100 ns
// units on the QPC time base) becomes presentQpc (VERIFY against
// QueryPerformanceCounter on hardware). The cursor is excluded
// (IsCursorCaptureEnabled, Windows 10 2004+; without it WGC is reported
// unavailable, since caps promise frames without the pointer, and a session
// that still cannot exclude it reports started.cursorInVideo), the yellow
// border is turned off where allowed (IsBorderRequired, Windows 11), and
// MinUpdateInterval lets WGC deliver faster than its 60 Hz default
// (Windows 11 24H2; Sunshine display_wgc.cpp sets 4 ms).
//
// The event handlers run on thread-pool threads and can still be running, or
// start, after they were revoked (revoking does not wait for a handler already
// invoked). So they never capture `this`: they hold the small shared state they
// use by shared_ptr ("Safely accessing the this pointer with an event-handling
// delegate", Strong and weak references in C++/WinRT), and the last owner
// closes its event.
#include "probes.hpp"

#if __has_include(<winrt/Windows.Graphics.Capture.h>) && __has_include(<winrt/Windows.Graphics.DirectX.Direct3D11.h>)
#define RECON_HAVE_WGC 1
#else
#define RECON_HAVE_WGC 0
#endif

#if RECON_HAVE_WGC

#include <unknwn.h>

#include <algorithm>
#include <cwctype>
#include <memory>
#include <mutex>

#include <winrt/Windows.Foundation.h>
#include <winrt/Windows.Foundation.Metadata.h>
#include <winrt/Windows.Graphics.Capture.h>
#include <winrt/Windows.Graphics.DirectX.Direct3D11.h>
#include <winrt/Windows.Graphics.DirectX.h>

#include "capture/paced_capture.hpp"
#include "d3d/device.hpp"

namespace recon {

namespace {

namespace wgc = winrt::Windows::Graphics::Capture;
namespace wd3d = winrt::Windows::Graphics::DirectX::Direct3D11;
using d3d::ComPtr;

// From windows.graphics.capture.interop.h and
// windows.graphics.directx.direct3d11.interop.h (Windows SDK), declared here so
// the code does not depend on headers mingw-w64 lacks.
struct IGraphicsCaptureItemInterop : ::IUnknown {
    virtual HRESULT STDMETHODCALLTYPE CreateForWindow(HWND window, REFIID riid, void** result) = 0;
    virtual HRESULT STDMETHODCALLTYPE CreateForMonitor(HMONITOR monitor, REFIID riid, void** result) = 0;
};
struct IDirect3DDxgiInterfaceAccess : ::IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetInterface(REFIID iid, void** p) = 0;
};
constexpr GUID kIidGraphicsCaptureItemInterop = {0x3628e81b, 0x3cac, 0x4c60, {0xb7, 0xf4, 0x23, 0xce, 0x0e, 0x0c, 0x33, 0x56}};
constexpr GUID kIidDirect3DDxgiInterfaceAccess = {0xa9b3d012, 0x3df2, 0x4ee3, {0xb8, 0xd1, 0x86, 0x95, 0xf4, 0x57, 0xd3, 0xc1}};
using CreateDeviceFromDxgiFn = HRESULT(WINAPI*)(IDXGIDevice*, ::IUnknown**);

std::string hresultText(const winrt::hresult_error& e) {
    return d3d::hrText(HRESULT(e.code())) + " " + winrt::to_string(e.message());
}

bool propertyPresent(const wchar_t* type, const wchar_t* prop) {
    try {
        return winrt::Windows::Foundation::Metadata::ApiInformation::IsPropertyPresent(type, prop);
    } catch (const winrt::hresult_error&) {
        return false;
    }
}

// Whether the SDK the helper is built with projects IsCursorCaptureEnabled.
template <typename S>
constexpr bool kSdkCursorCapture = requires(S& s) { s.IsCursorCaptureEnabled(false); };

// Session properties newer than some Windows SDKs: set only when the SDK the
// helper is built with projects them (requires-expression on a dependent
// type), and only when the running Windows has them (propertyPresent).
template <typename S>
bool setCursorCapture(S& s, bool on) {
    if constexpr (requires { s.IsCursorCaptureEnabled(on); }) {
        s.IsCursorCaptureEnabled(on);
        return true;
    }
    return false;
}
template <typename S>
bool setBorderRequired(S& s, bool on) {
    if constexpr (requires { s.IsBorderRequired(on); }) {
        s.IsBorderRequired(on);
        return true;
    }
    return false;
}
template <typename S>
bool setMinUpdateInterval(S& s, winrt::Windows::Foundation::TimeSpan t) {
    if constexpr (requires { s.MinUpdateInterval(t); }) {
        s.MinUpdateInterval(t);
        return true;
    }
    return false;
}

const wchar_t kSessionType[] = L"Windows.Graphics.Capture.GraphicsCaptureSession";

// Shared between the capture and the WGC event handlers (see the top of the file).
struct WgcShared {
    std::mutex mu;
    wgc::Direct3D11CaptureFrame produced{nullptr};  // newest frame not taken by the capture thread yet
    bool closed = false;                            // the window or monitor went away
    HANDLE event = nullptr;                         // auto-reset: a frame arrived, the item closed, or shutdown

    WgcShared() { event = CreateEventW(nullptr, FALSE, FALSE, nullptr); }
    ~WgcShared() {
        try {
            if (produced) produced.Close();
        } catch (const winrt::hresult_error&) {
        }
        if (event) CloseHandle(event);
    }
    WgcShared(const WgcShared&) = delete;
    WgcShared& operator=(const WgcShared&) = delete;
};

// FrameArrived (thread pool): keep only the newest frame.
void onFrame(WgcShared& st, const wgc::Direct3D11CaptureFramePool& pool) {
    wgc::Direct3D11CaptureFrame frame{nullptr};
    try {
        frame = pool.TryGetNextFrame();
    } catch (const winrt::hresult_error&) {
        return;  // e.g. the pool was closed meanwhile
    }
    if (!frame) return;
    std::lock_guard<std::mutex> lock(st.mu);
    if (st.produced) {
        try {
            st.produced.Close();  // newest wins; the old buffer goes back to WGC
        } catch (const winrt::hresult_error&) {
        }
    }
    st.produced = frame;
    SetEvent(st.event);
}

struct TitleSearch {
    std::wstring needle;
    HWND found = nullptr;
};

BOOL CALLBACK findWindowByTitle(HWND hwnd, LPARAM param) {
    auto* s = reinterpret_cast<TitleSearch*>(param);
    if (!IsWindowVisible(hwnd) || GetWindow(hwnd, GW_OWNER)) return TRUE;
    wchar_t title[512];
    const int n = GetWindowTextW(hwnd, title, 512);
    if (n <= 0) return TRUE;
    std::wstring t(title, size_t(n));
    std::transform(t.begin(), t.end(), t.begin(), [](wchar_t c) { return wchar_t(std::towlower(c)); });
    if (t.find(s->needle) != std::wstring::npos) {
        s->found = hwnd;
        return FALSE;
    }
    return TRUE;
}

class WgcCapture : public PacedCapture {
public:
    ~WgcCapture() override;
    const char* name() const override { return "wgc"; }
    Status init(const StartParams& p) override;
    SourceInfo source() const override {
        std::lock_guard<std::mutex> lock(srcMu_);
        return src_;
    }
    void shutdown() override {
        PacedCapture::shutdown();
        if (shared_->event) SetEvent(shared_->event);
    }

protected:
    Next acquire(int timeoutMs, Acquired& a, Status& err) override;
    void promote() override { cur_ = 1 - cur_; }
    void describe(CapturedFrame& out) override {
        out.texture = slots_[cur_].Get();
        out.width = sizes_[cur_].first;
        out.height = sizes_[cur_].second;
    }

private:
    Status start(HWND window, HMONITOR monitor, int fps);

    std::shared_ptr<WgcShared> shared_ = std::make_shared<WgcShared>();
    d3d::OutputRef output_;
    d3d::Device dev_;
    wd3d::IDirect3DDevice rtDevice_{nullptr};
    wgc::GraphicsCaptureItem item_{nullptr};
    wgc::Direct3D11CaptureFramePool pool_{nullptr};
    wgc::GraphicsCaptureSession session_{nullptr};
    winrt::event_token frameToken_{}, closedToken_{};
    winrt::Windows::Graphics::SizeInt32 poolSize_{};
    bool cursorInVideo_ = false;  // the session could not exclude the pointer
    bool lostPosted_ = false;
    ComPtr<ID3D11Texture2D> slots_[2];
    std::pair<uint32_t, uint32_t> sizes_[2];
    int cur_ = 0;
    mutable std::mutex srcMu_;
    SourceInfo src_;
};

WgcCapture::~WgcCapture() {
    try {
        if (pool_) pool_.FrameArrived(frameToken_);
        if (item_) item_.Closed(closedToken_);
        if (session_) session_.Close();
        if (pool_) pool_.Close();
    } catch (const winrt::hresult_error&) {
    }
    // A handler still running keeps shared_ alive; the frame it may store is
    // closed with it (WgcShared's destructor).
    std::lock_guard<std::mutex> lock(shared_->mu);
    try {
        if (shared_->produced) shared_->produced.Close();
    } catch (const winrt::hresult_error&) {
    }
    shared_->produced = nullptr;
}

Status WgcCapture::init(const StartParams& p) {
    try {
        winrt::init_apartment(winrt::apartment_type::multi_threaded);
    } catch (const winrt::hresult_error&) {
        // RPC_E_CHANGED_MODE: COM is already initialized on this thread; fine.
    }
    HWND window = nullptr;
    if (p.window) {
        window = reinterpret_cast<HWND>(static_cast<uintptr_t>(p.window));
        if (!IsWindow(window)) return Status::Error("no_output", "window handle " + std::to_string(p.window) + " does not exist");
    } else if (!p.windowTitle.empty()) {
        TitleSearch s;
        s.needle = fromUtf8(p.windowTitle);
        std::transform(s.needle.begin(), s.needle.end(), s.needle.begin(), [](wchar_t c) { return wchar_t(std::towlower(c)); });
        EnumWindows(findWindowByTitle, reinterpret_cast<LPARAM>(&s));
        if (!s.found) return Status::Error("no_output", "no visible window title contains \"" + p.windowTitle + "\"");
        window = s.found;
    }
    // The device lives on the adapter of the monitor showing the window (or the requested output).
    Status st = window ? d3d::outputForMonitor(reinterpret_cast<uint64_t>(MonitorFromWindow(window, MONITOR_DEFAULTTONEAREST)), output_)
                       : d3d::selectOutput(p, output_);
    if (!st.ok) return st;
    st = d3d::createDevice(output_.adapter.Get(), dev_);
    if (!st.ok) return st;
    if (!shared_->event) return Status::Error("init_failed", "CreateEvent failed");
    st = start(window, reinterpret_cast<HMONITOR>(static_cast<uintptr_t>(output_.desc.hmonitor)), p.fps);
    if (!st.ok) return st;
    {
        std::lock_guard<std::mutex> lock(srcMu_);
        src_.device = dev_.device.Get();
        src_.adapter = output_.adapterInfo;
        src_.width = uint32_t(poolSize_.Width);
        src_.height = uint32_t(poolSize_.Height);
        src_.cursorInVideo = cursorInVideo_;
    }
    logf(LogLevel::Info, "wgc: capturing %s %dx%d on %s", window ? "a window" : output_.desc.name.c_str(), poolSize_.Width,
         poolSize_.Height, output_.adapterInfo.name.c_str());
    startPacing(p, dev_.device.Get());
    return Status::Ok();
}

Status WgcCapture::start(HWND window, HMONITOR monitor, int fps) {
    try {
        if (!wgc::GraphicsCaptureSession::IsSupported()) {
            return Status::Error("unavailable", "Windows.Graphics.Capture is not supported on this system");
        }
        static const auto createDevice = [] {
            HMODULE d3d11 = GetModuleHandleW(L"d3d11.dll");
            return procAddress<CreateDeviceFromDxgiFn>(d3d11, "CreateDirect3D11DeviceFromDXGIDevice");
        }();
        ComPtr<IDXGIDevice> dxgi;
        winrt::com_ptr<::IUnknown> inspectable;
        if (!createDevice || FAILED(dev_.device.As(&dxgi)) || FAILED(createDevice(dxgi.Get(), inspectable.put()))) {
            return Status::Error("init_failed", "CreateDirect3D11DeviceFromDXGIDevice failed");
        }
        rtDevice_ = inspectable.as<wd3d::IDirect3DDevice>();

        auto factory = winrt::get_activation_factory<wgc::GraphicsCaptureItem>();
        ComPtr<IGraphicsCaptureItemInterop> interop;
        HRESULT hr = static_cast<::IUnknown*>(winrt::get_abi(factory))
                         ->QueryInterface(kIidGraphicsCaptureItemInterop, reinterpret_cast<void**>(interop.GetAddressOf()));
        if (FAILED(hr)) return Status::Error("unavailable", "IGraphicsCaptureItemInterop: " + d3d::hrText(hr));
        const winrt::guid itemIid = winrt::guid_of<wgc::IGraphicsCaptureItem>();
        hr = window ? interop->CreateForWindow(window, reinterpret_cast<const GUID&>(itemIid), winrt::put_abi(item_))
                    : interop->CreateForMonitor(monitor, reinterpret_cast<const GUID&>(itemIid), winrt::put_abi(item_));
        if (FAILED(hr)) return Status::Error("init_failed", "creating the capture item failed: " + d3d::hrText(hr));

        poolSize_ = item_.Size();
        pool_ = wgc::Direct3D11CaptureFramePool::CreateFreeThreaded(
            rtDevice_, winrt::Windows::Graphics::DirectX::DirectXPixelFormat::B8G8R8A8UIntNormalized, 2, poolSize_);
        session_ = pool_.CreateCaptureSession(item_);
        frameToken_ = pool_.FrameArrived(
            [st = shared_](const wgc::Direct3D11CaptureFramePool& sender, const winrt::Windows::Foundation::IInspectable&) {
                onFrame(*st, sender);
            });
        closedToken_ = item_.Closed([st = shared_](const wgc::GraphicsCaptureItem&, const winrt::Windows::Foundation::IInspectable&) {
            std::lock_guard<std::mutex> lock(st->mu);
            st->closed = true;
            SetEvent(st->event);
        });
        try {
            // The probe only lists WGC where this exists; should it still
            // fail, started.cursorInVideo tells recon-host.
            if (!propertyPresent(kSessionType, L"IsCursorCaptureEnabled") || !setCursorCapture(session_, false)) {
                cursorInVideo_ = true;
                logf(LogLevel::Warn, "wgc: this Windows cannot exclude the cursor from the capture");
            }
        } catch (const winrt::hresult_error& e) {
            cursorInVideo_ = true;
            logf(LogLevel::Warn, "wgc: cannot exclude the cursor: %s", hresultText(e).c_str());
        }
        try {
            if (propertyPresent(kSessionType, L"IsBorderRequired")) setBorderRequired(session_, false);
        } catch (const winrt::hresult_error& e) {
            logf(LogLevel::Warn, "wgc: cannot turn the capture border off: %s", hresultText(e).c_str());
        }
        try {
            const int64_t ticks = std::min<int64_t>(40000, 10000000 / (2 * int64_t(fps)));  // <= 4 ms, < half a frame
            if (!propertyPresent(kSessionType, L"MinUpdateInterval") ||
                !setMinUpdateInterval(session_, winrt::Windows::Foundation::TimeSpan(ticks))) {
                logf(LogLevel::Info, "wgc: MinUpdateInterval not available (Windows 11 24H2+): capture may be limited to 60 Hz");
            }
        } catch (const winrt::hresult_error& e) {
            logf(LogLevel::Warn, "wgc: MinUpdateInterval: %s (capture may be limited to 60 Hz)", hresultText(e).c_str());
        }
        session_.StartCapture();
    } catch (const winrt::hresult_error& e) {
        return Status::Error("init_failed", "Windows.Graphics.Capture: " + hresultText(e));
    }
    return Status::Ok();
}

Next WgcCapture::acquire(int timeoutMs, Acquired& a, Status& err) {
    const HANDLE handles[] = {stopEvent(), shared_->event};
    const DWORD w = WaitForMultipleObjects(2, handles, FALSE, DWORD(timeoutMs));
    if (stopping()) return Next::Stopped;
    if (w == WAIT_TIMEOUT) return Next::Timeout;  // PacedCapture checks for a removed device meanwhile
    wgc::Direct3D11CaptureFrame frame{nullptr};
    {
        std::lock_guard<std::mutex> lock(shared_->mu);
        frame = shared_->produced;
        shared_->produced = nullptr;
        if (shared_->closed && !lostPosted_) {
            lostPosted_ = true;
            CaptureEvent ev;
            ev.reason = "lost";
            ev.width = poolSize_.Width, ev.height = poolSize_.Height;
            ev.text = "the captured window or monitor is gone";
            postEvent(ev);
        }
    }
    if (!frame) return Next::Timeout;
    const int64_t now = qpcNow();
    try {
        const auto content = frame.ContentSize();
        ComPtr<IDirect3DDxgiInterfaceAccess> access;
        ComPtr<ID3D11Texture2D> tex;
        auto* surface = static_cast<::IUnknown*>(winrt::get_abi(frame.Surface()));
        HRESULT hr = surface->QueryInterface(kIidDirect3DDxgiInterfaceAccess, reinterpret_cast<void**>(access.GetAddressOf()));
        if (SUCCEEDED(hr)) hr = access->GetInterface(__uuidof(ID3D11Texture2D), reinterpret_cast<void**>(tex.GetAddressOf()));
        if (FAILED(hr)) {
            frame.Close();
            if (d3d::deviceRemoved(dev_.device.Get(), "wgc: frame texture", err)) return Next::Error;
            err = Status::Error("capture_failed", "WGC frame without a D3D11 texture: " + d3d::hrText(hr), true);
            return Next::Error;
        }
        D3D11_TEXTURE2D_DESC td{};
        tex->GetDesc(&td);
        // A window can be smaller than the pool's buffers: copy only its content.
        const uint32_t cw = std::min<uint32_t>(td.Width, uint32_t(std::max(content.Width, 1)));
        const uint32_t ch = std::min<uint32_t>(td.Height, uint32_t(std::max(content.Height, 1)));
        const int slot = 1 - cur_;
        D3D11_TEXTURE2D_DESC have{};
        if (slots_[slot]) slots_[slot]->GetDesc(&have);
        if (!slots_[slot] || have.Width != cw || have.Height != ch || have.Format != td.Format) {
            D3D11_TEXTURE2D_DESC nd{};
            nd.Width = cw;
            nd.Height = ch;
            nd.MipLevels = 1;
            nd.ArraySize = 1;
            nd.Format = td.Format;
            nd.SampleDesc.Count = 1;
            nd.Usage = D3D11_USAGE_DEFAULT;
            nd.BindFlags = D3D11_BIND_SHADER_RESOURCE;
            hr = dev_.device->CreateTexture2D(&nd, nullptr, slots_[slot].ReleaseAndGetAddressOf());
            if (FAILED(hr)) {
                frame.Close();
                if (d3d::deviceRemoved(dev_.device.Get(), "wgc: creating a capture texture", err)) return Next::Error;
                err = Status::Error("capture_failed", "creating a capture texture failed: " + d3d::hrText(hr), true);
                return Next::Error;
            }
        }
        const D3D11_BOX box{0, 0, 0, cw, ch, 1};
        dev_.context->CopySubresourceRegion(slots_[slot].Get(), 0, 0, 0, 0, tex.Get(), 0, &box);
        sizes_[slot] = {cw, ch};
        // SystemRelativeTime: 100 ns units on the QPC time base.
        const int64_t t100ns = frame.SystemRelativeTime().count();
        a.presentQpc = t100ns > 0 ? int64_t(double(t100ns) * double(qpcFrequency()) / 1e7) : 0;
        a.captureQpc = now;
        a.dirty = -1;  // WGC reports no dirty regions (Windows 11 24H2's DirtyRegions not used yet)
        frame.Close();
        if (content.Width != poolSize_.Width || content.Height != poolSize_.Height) {
            // Recreate the pool at the new size (WGC sample guidance); the
            // stream keeps its encoded size and scales.
            poolSize_ = content;
            pool_.Recreate(rtDevice_, winrt::Windows::Graphics::DirectX::DirectXPixelFormat::B8G8R8A8UIntNormalized, 2, poolSize_);
            CaptureEvent ev;
            ev.reason = "resized";
            ev.width = content.Width, ev.height = content.Height;
            postEvent(ev);
            std::lock_guard<std::mutex> lock(srcMu_);
            src_.width = uint32_t(content.Width);
            src_.height = uint32_t(content.Height);
        }
    } catch (const winrt::hresult_error& e) {
        if (d3d::deviceRemoved(dev_.device.Get(), "Windows.Graphics.Capture: " + hresultText(e), err)) return Next::Error;
        err = Status::Error("capture_failed", "Windows.Graphics.Capture: " + hresultText(e), true);
        return Next::Error;
    }
    return Next::Frame;
}

}  // namespace

Probe probeWgcCapture() {
    try {
        winrt::init_apartment(winrt::apartment_type::multi_threaded);
    } catch (const winrt::hresult_error&) {
    }
    try {
        if (!wgc::GraphicsCaptureSession::IsSupported()) return {false, "Windows.Graphics.Capture is not supported on this system"};
    } catch (const winrt::hresult_error& e) {
        return {false, "Windows.Graphics.Capture: " + hresultText(e)};
    }
    // caps promise frames without the pointer (cursorInVideo false): without
    // IsCursorCaptureEnabled WGC frames contain it, and the client would draw
    // a second one.
    if constexpr (!kSdkCursorCapture<wgc::GraphicsCaptureSession>) {
        return {false, "this build's Windows SDK has no GraphicsCaptureSession.IsCursorCaptureEnabled (frames would contain the pointer)"};
    } else {
        if (!propertyPresent(kSessionType, L"IsCursorCaptureEnabled")) {
            return {false, "this Windows cannot keep the mouse pointer out of Windows.Graphics.Capture frames (needs Windows 10 2004 or later)"};
        }
        return {true, "monitors and windows (capture \"wgc\")"};
    }
}

std::unique_ptr<Capture> createWgcCapture(Status& err) {
    Probe p = probeWgcCapture();
    if (!p.available) {
        err = Status::Error("unavailable", p.reason);
        return nullptr;
    }
    return std::make_unique<WgcCapture>();
}

}  // namespace recon

#else  // !RECON_HAVE_WGC

namespace recon {

Probe probeWgcCapture() {
    return {false, "this build has no C++/WinRT headers (mingw-w64 build): use the MSVC build for Windows.Graphics.Capture"};
}

std::unique_ptr<Capture> createWgcCapture(Status& err) {
    err = Status::Error("unavailable", probeWgcCapture().reason);
    return nullptr;
}

}  // namespace recon

#endif
