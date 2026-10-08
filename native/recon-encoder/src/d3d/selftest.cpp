// recon-encoder --self-test-convert: runs the BGRA -> NV12 conversion on a
// WARP device (no display or GPU needed) and checks it against a CPU
// reference of the same maths, plus absolute BT.709 colour-bar values, the
// orientation of a rotated display and the barcode blocks.
#include <cmath>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <vector>

#include "d3d/convert.hpp"
#include "d3d/device.hpp"
#include "selftest.hpp"

namespace recon {

namespace {

using d3d::ComPtr;

struct Image {
    uint32_t w = 0, h = 0;
    std::vector<uint8_t> bgra;
};

struct Rgb {
    double r = 0, g = 0, b = 0;
};

// 100 % colour bars in the top half, a pseudo-random pattern in the bottom
// half so filtering errors show.
Image testPattern(uint32_t w, uint32_t h) {
    static const uint8_t bars[8][3] = {{255, 255, 255}, {255, 255, 0}, {0, 255, 255}, {0, 255, 0},
                                       {255, 0, 255},   {255, 0, 0},   {0, 0, 255},   {0, 0, 0}};  // RGB
    Image img;
    img.w = w;
    img.h = h;
    img.bgra.resize(size_t(w) * h * 4);
    uint32_t seed = 12345;
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            uint8_t* p = &img.bgra[(size_t(y) * w + x) * 4];
            if (y < h / 2) {
                const uint8_t* c = bars[x * 8 / w];
                p[0] = c[2], p[1] = c[1], p[2] = c[0];
            } else {
                seed = seed * 1664525u + 1013904223u;
                p[0] = uint8_t(seed >> 24), p[1] = uint8_t(seed >> 16), p[2] = uint8_t(seed >> 8);
            }
            p[3] = 255;
        }
    }
    return img;
}

Rgb texel(const Image& img, int x, int y) {
    x = x < 0 ? 0 : x >= int(img.w) ? int(img.w) - 1 : x;
    y = y < 0 ? 0 : y >= int(img.h) ? int(img.h) - 1 : y;
    const uint8_t* p = &img.bgra[(size_t(y) * img.w + size_t(x)) * 4];
    return {p[2] / 255.0, p[1] / 255.0, p[0] / 255.0};
}

// D3D11 bilinear sampling with clamp addressing at normalised (s, t).
Rgb sample(const Image& img, double s, double t) {
    const double x = s * img.w - 0.5, y = t * img.h - 0.5;
    const double x0 = std::floor(x), y0 = std::floor(y);
    const double ax = x - x0, ay = y - y0;
    const int ix = int(x0), iy = int(y0);
    const Rgb a = texel(img, ix, iy), b = texel(img, ix + 1, iy), c = texel(img, ix, iy + 1), d = texel(img, ix + 1, iy + 1);
    auto mix = [&](double pa, double pb, double pc, double pd) {
        return (pa * (1 - ax) + pb * ax) * (1 - ay) + (pc * (1 - ax) + pd * ax) * ay;
    };
    return {mix(a.r, b.r, c.r, d.r), mix(a.g, b.g, c.g, d.g), mix(a.b, b.b, c.b, d.b)};
}

// The shader's fetch(): an output luma position mapped through the rotation.
Rgb fetch(const Image& img, uint32_t outW, uint32_t outH, int rotation, double lx, double ly) {
    float xu[3], xv[3];
    d3d::rotationTransform(rotation, xu, xv);
    const double u = lx / outW, v = ly / outH;
    return sample(img, xu[0] * u + xu[1] * v + xu[2], xv[0] * u + xv[1] * v + xv[2]);
}

uint8_t unorm(double v) {
    v = v < 0 ? 0 : v > 1 ? 1 : v;
    return uint8_t(std::lround(v * 255.0));
}

// cw x ch: the content rectangle the image is scaled into (the rest of
// w x h is padding: the same mapping continued, clamped to the edge).
std::vector<uint8_t> reference(const Image& img, uint32_t w, uint32_t h, uint32_t cw, uint32_t ch, int rotation,
                               const BarcodeLayout& bc, uint64_t value) {
    const d3d::YuvCoefficients k = d3d::bt709Limited();
    std::vector<uint8_t> out(size_t(w) * h * 3 / 2);
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            const int bit = d3d::barcodeBit(bc, value, x, y);
            if (bit >= 0) {
                out[size_t(y) * w + x] = bit ? 235 : 16;
                continue;
            }
            const Rgb c = fetch(img, cw, ch, rotation, x + 0.5, y + 0.5);
            out[size_t(y) * w + x] = unorm(k.y[0] * c.r + k.y[1] * c.g + k.y[2] * c.b + k.y[3]);
        }
    }
    uint8_t* uv = out.data() + size_t(w) * h;
    for (uint32_t j = 0; j < h / 2; ++j) {
        for (uint32_t i = 0; i < w / 2; ++i) {
            uint8_t* p = uv + (size_t(j) * (w / 2) + i) * 2;
            if (d3d::barcodeBit(bc, value, i * 2, j * 2) >= 0) {
                p[0] = p[1] = 128;
                continue;
            }
            const double lx = i * 2 + 0.5, ly = j * 2 + 0.5;
            const double wts[6][3] = {{-1, 0, 1}, {0, 0, 2}, {1, 0, 1}, {-1, 1, 1}, {0, 1, 2}, {1, 1, 1}};
            Rgb s;
            for (const auto& t : wts) {
                const Rgb c = fetch(img, cw, ch, rotation, lx + t[0], ly + t[1]);
                s.r += c.r * t[2], s.g += c.g * t[2], s.b += c.b * t[2];
            }
            s.r /= 8, s.g /= 8, s.b /= 8;
            p[0] = unorm(k.u[0] * s.r + k.u[1] * s.g + k.u[2] * s.b + k.u[3]);
            p[1] = unorm(k.v[0] * s.r + k.v[1] * s.g + k.v[2] * s.b + k.v[3]);
        }
    }
    return out;
}

struct Case {
    const char* name;
    uint32_t outW, outH;
    int rotation;
    BarcodeLayout barcode;
    uint64_t value;
    int tolerance;         // max |GPU - reference| per sample
    bool noShaderBinding;  // source texture without D3D11_BIND_SHADER_RESOURCE (converter copies it)
    uint32_t contentW = 0, contentH = 0;  // picture inside outW x outH, the rest padding (0 = no padding)
};

BarcodeLayout layout(int x, int y, int bw, int bh, int cols, int bits, bool msb) {
    BarcodeLayout b;
    b.enabled = true;
    b.x = x, b.y = y, b.blockW = bw, b.blockH = bh, b.cols = cols, b.bits = bits, b.msbFirst = msb;
    return b;
}

class Checker {
public:
    explicit Checker(const char* name) : name_(name) {}
    void fail(const char* fmt, ...)
#if defined(__MINGW32__) && !defined(__clang__)
        __attribute__((format(gnu_printf, 2, 3)))  // with __USE_MINGW_ANSI_STDIO (CMakeLists.txt)
#elif defined(__GNUC__)
        __attribute__((format(printf, 2, 3)))
#endif
    {
        if (++failures_ > 8) return;
        char buf[256];
        va_list ap;
        va_start(ap, fmt);
        std::vsnprintf(buf, sizeof(buf), fmt, ap);
        va_end(ap);
        std::printf("  %s: FAIL %s\n", name_, buf);
    }
    int failures() const { return failures_; }

private:
    const char* name_;
    int failures_ = 0;
};

// BT.709 limited-range 100 % colour bars (ITU-R BT.709-6 matrix, rounded).
const uint8_t kBarYuv[8][3] = {{235, 128, 128}, {219, 16, 138}, {188, 154, 16}, {173, 42, 26},
                               {78, 214, 230},  {63, 102, 240}, {32, 240, 118}, {16, 128, 128}};

int runCase(ID3D11Device* dev, d3d::Nv12Converter::Output mode, const Image& img, const Case& c) {
    Checker chk(c.name);
    d3d::Nv12Converter conv;
    Status s = conv.init(dev, c.outW, c.outH, c.barcode, mode, 2, c.contentW, c.contentH);
    if (!s.ok) {
        chk.fail("init: %s", s.text.c_str());
        return chk.failures();
    }
    D3D11_TEXTURE2D_DESC td{};
    td.Width = img.w;
    td.Height = img.h;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = c.noShaderBinding ? D3D11_BIND_RENDER_TARGET : D3D11_BIND_SHADER_RESOURCE;
    D3D11_SUBRESOURCE_DATA init{img.bgra.data(), img.w * 4, 0};
    ComPtr<ID3D11Texture2D> src;
    HRESULT hr = dev->CreateTexture2D(&td, &init, src.GetAddressOf());
    if (FAILED(hr)) {
        chk.fail("source texture: %s", d3d::hrText(hr).c_str());
        return chk.failures();
    }
    d3d::ConvertedFrame f;
    s = conv.convert(src.Get(), c.rotation, c.value, f);
    std::vector<uint8_t> got;
    if (s.ok) s = conv.readback(f, got);
    if (!s.ok) {
        chk.fail("%s", s.text.c_str());
        return chk.failures();
    }
    // The pool hands out the other texture while this one is held, and
    // refuses a third conversion while both are held.
    d3d::ConvertedFrame f2, f3;
    if (!conv.convert(src.Get(), c.rotation, c.value, f2).ok || f2.index == f.index) chk.fail("second pool texture");
    if (conv.convert(src.Get(), c.rotation, c.value, f3).code != "pool_exhausted") chk.fail("pool did not run out");
    f2 = {};
    if (!conv.convert(src.Get(), c.rotation, c.value, f3).ok) chk.fail("released texture not reused");

    const uint32_t w = c.outW, h = c.outH;
    const uint32_t cw = c.contentW ? c.contentW : w, ch = c.contentH ? c.contentH : h;
    const std::vector<uint8_t> ref = reference(img, w, h, cw, ch, c.rotation, c.barcode, c.value);
    int maxErr = 0;
    for (size_t i = 0; i < ref.size(); ++i) {
        const int e = std::abs(int(got[i]) - int(ref[i]));
        if (e > maxErr) maxErr = e;
        if (e > c.tolerance) {
            const bool luma = i < size_t(w) * h;
            const size_t j = luma ? i : (i - size_t(w) * h);
            chk.fail("%s at (%zu,%zu): got %d, want %d", luma ? "Y" : (j % 2 ? "Cr" : "Cb"),
                     luma ? j % w : (j / 2) % (w / 2), luma ? j / w : (j / 2) / (w / 2), got[i], ref[i]);
        }
    }

    // Absolute values: the middle of every colour bar (top half, unrotated cases).
    const uint8_t* uv = got.data() + size_t(w) * h;
    if (c.rotation == 0) {
        for (int b = 0; b < 8; ++b) {
            const uint32_t x = (uint32_t(b) * 2 + 1) * cw / 16 & ~1u, y = ch * 3 / 8 & ~1u;
            const uint8_t Y = got[size_t(y) * w + x], U = uv[(size_t(y / 2) * (w / 2) + x / 2) * 2],
                          V = uv[(size_t(y / 2) * (w / 2) + x / 2) * 2 + 1];
            if (std::abs(Y - kBarYuv[b][0]) > 1 || std::abs(U - kBarYuv[b][1]) > 1 || std::abs(V - kBarYuv[b][2]) > 1) {
                chk.fail("bar %d: YCbCr %d,%d,%d, want %d,%d,%d", b, Y, U, V, kBarYuv[b][0], kBarYuv[b][1], kBarYuv[b][2]);
            }
        }
    } else if (c.rotation == 90) {
        // Displayed = source turned 90 degrees clockwise: the source's top-left
        // (white bar) is at the top right, its top-right (black bar) at the bottom right.
        if (std::abs(got[size_t(4) * w + (w - 4)] - 235) > 1) chk.fail("rotation: top right is not white");
        if (std::abs(got[size_t(h - 4) * w + (w - 4)] - 16) > 1) chk.fail("rotation: bottom right is not black");
    }

    // The barcode: every block solid 16 / 235 with neutral chroma, and it decodes to the value.
    const BarcodeLayout& bc = c.barcode;
    if (bc.enabled) {
        uint64_t decoded = 0;
        for (int k = 0; k < bc.bits; ++k) {
            const uint32_t bx = uint32_t(bc.x + (k % bc.cols) * bc.blockW), by = uint32_t(bc.y + (k / bc.cols) * bc.blockH);
            int ones = 0;
            for (uint32_t y = by; y < by + uint32_t(bc.blockH); ++y) {
                for (uint32_t x = bx; x < bx + uint32_t(bc.blockW); ++x) {
                    const uint8_t v = got[size_t(y) * w + x];
                    if (v != 16 && v != 235) chk.fail("barcode block %d pixel (%u,%u) = %d", k, x, y, v);
                    ones += v == 235;
                    if (x % 2 == 0 && y % 2 == 0) {
                        const uint8_t* p = uv + (size_t(y / 2) * (w / 2) + x / 2) * 2;
                        if (p[0] != 128 || p[1] != 128) chk.fail("barcode block %d chroma %d,%d", k, p[0], p[1]);
                    }
                }
            }
            const int bit = ones * 2 > bc.blockW * bc.blockH;
            const int pos = bc.msbFirst ? bc.bits - 1 - k : k;
            decoded |= uint64_t(bit) << pos;
        }
        const uint64_t mask = bc.bits == 64 ? ~0ull : (1ull << bc.bits) - 1;
        if (decoded != (c.value & mask)) {
            chk.fail("barcode decodes to 0x%llx, want 0x%llx", static_cast<unsigned long long>(decoded),
                     static_cast<unsigned long long>(c.value & mask));
        }
    }
    // Padding: the last content column / row repeated (the clamp sampler), so
    // the padded area holds no new detail for the encoder to spend bits on.
    if (cw < w || ch < h) {
        for (uint32_t y = 0; y < h; ++y) {
            for (uint32_t x = cw + 2; x < w; ++x) {
                if (std::abs(got[size_t(y) * w + x] - got[size_t(y) * w + cw + 1]) > 1) {
                    chk.fail("padding column %u row %u is not the repeated edge", x, y);
                    x = w, y = h;
                }
            }
        }
    }
    std::printf("  %-28s %ux%u -> %ux%u rot %3d: %s (max error %d)\n", c.name, img.w, img.h, cw, ch, c.rotation,
                chk.failures() ? "FAIL" : "ok", maxErr);
    if (cw < w || ch < h) std::printf("  %-28s (padded to %ux%u)\n", "", w, h);
    return chk.failures();
}

}  // namespace

int runConvertSelfTest(bool hardware) {
    d3d::Device dev;
    const char* kind = hardware ? "default hardware" : "WARP";
    Status s = d3d::createDevice(nullptr, dev, !hardware);
    if (!s.ok && !hardware) {
        kind = "default hardware";
        Status hw = d3d::createDevice(nullptr, dev, false);
        if (!hw.ok) {
            std::printf("self-test-convert: SKIP: no D3D11 device (WARP: %s; hardware: %s)\n", s.text.c_str(), hw.text.c_str());
            return kSelfTestSkip;
        }
    } else if (!s.ok) {
        std::printf("self-test-convert: SKIP: no hardware D3D11 device: %s\n", s.text.c_str());
        return kSelfTestSkip;
    }
    {
        ComPtr<IDXGIDevice> dxgi;
        ComPtr<IDXGIAdapter> adapter;
        DXGI_ADAPTER_DESC ad{};
        if (SUCCEEDED(dev.device.As(&dxgi)) && SUCCEEDED(dxgi->GetAdapter(adapter.GetAddressOf())) &&
            SUCCEEDED(adapter->GetDesc(&ad))) {
            std::printf("self-test-convert: adapter %s (vendor 0x%04x)\n", toUtf8(ad.Description).c_str(), unsigned(ad.VendorId));
        }
    }
    const bool nv12 = d3d::Nv12Converter::nv12RenderTargets(dev.device.Get());
    const auto mode = nv12 ? d3d::Nv12Converter::Output::Nv12 : d3d::Nv12Converter::Output::Planar;
    std::printf("self-test-convert: %s device, feature level %x, %s\n", kind, unsigned(dev.level),
                nv12 ? "NV12 render targets (mode nv12)"
                     : "no NV12 render targets: testing the shaders on separate R8/R8G8 planes (mode planar)");

    const Image img = testPattern(256, 128);
    const Case cases[] = {
        {"1:1 + barcode", 256, 128, 0, layout(8, 96, 8, 8, 16, 32, true), 0xA5C30F1Eull, 1, false},
        {"2:1 downscale + barcode", 128, 64, 0, layout(4, 48, 4, 4, 16, 24, true), 0x00C0FFEEull, 1, true},
        {"4:3 downscale, lsb first", 192, 96, 0, layout(0, 0, 8, 8, 8, 16, false), 0x1234ull, 2, false},
        {"1:1 rotated 90", 128, 256, 90, layout(16, 16, 8, 8, 8, 64, true), 0x0123456789ABCDEFull, 1, false},
        {"1:1 rotated 180", 256, 128, 180, BarcodeLayout{}, 0, 1, false},
        {"1:1 rotated 270", 128, 256, 270, BarcodeLayout{}, 0, 1, false},
        {"2:1 upscale", 512, 256, 0, BarcodeLayout{}, 0, 2, false},
        // AV1 on RDNA3: 200x90 coded as 256x96 (multiples of 64x16).
        {"scaled into 64x16 padding", 256, 96, 0, layout(8, 64, 4, 4, 16, 16, true), 0xBEEFull, 2, false, 200, 90},
    };
    int failures = 0;
    for (const Case& c : cases) failures += runCase(dev.device.Get(), mode, img, c);
    std::printf("self-test-convert: %s (mode %s)\n", failures ? "FAIL" : "ok", nv12 ? "nv12" : "planar");
    return failures ? 1 : 0;
}

}  // namespace recon
