// recon-encoder --self-test-convert: runs the colour conversion on a WARP
// device (no display or GPU needed) and checks it against a CPU reference of
// the same maths, plus absolute values, the orientation of a rotated display
// and the barcode blocks:
// - SDR: BGRA -> NV12 (BT.709 limited range), absolute BT.709 colour bars;
//   an FP16 scRGB source clipped to SDR;
// - HDR10 (step 3.9): FP16 scRGB -> P010 (BT.2020 primaries, SMPTE ST 2084
//   PQ, 10-bit limited range) and an 8-bit sRGB source in an HDR10 stream (at
//   203 cd/m2), against a double-precision reference of the PQ curve and the
//   BT.709 -> BT.2020 matrix, plus absolute code values of known luminances
//   (100 cd/m2 = PQ 0.508 = code 509, 1000 cd/m2 = 723, 10000 cd/m2 = 940).
#include <cmath>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "d3d/convert.hpp"
#include "d3d/device.hpp"
#include "selftest.hpp"

namespace recon {

namespace {

using d3d::ComPtr;
using Format = d3d::Nv12Converter::Format;

struct Rgb {
    double r = 0, g = 0, b = 0;
};

// A source image: the texture's bytes and the value the shader reads for every
// texel (UNORM8 / 255, or the FP16 value exactly as stored).
struct Image {
    uint32_t w = 0, h = 0;
    DXGI_FORMAT format = DXGI_FORMAT_B8G8R8A8_UNORM;
    bool linear = false;  // scRGB FP16
    std::vector<uint8_t> data;
    uint32_t pitch = 0;
    std::vector<Rgb> texels;
};

// The value of an IEEE 754 binary16 (what the shader reads from an FP16 texel).
double fromHalf(uint16_t h) {
    const uint32_t exp = (h >> 10) & 0x1f, mant = h & 0x3ffu;
    const double v = exp == 0 ? std::ldexp(double(mant), -24) : exp == 31 ? HUGE_VAL : std::ldexp(double(mant | 0x400u), int(exp) - 25);
    return (h & 0x8000u) ? -v : v;
}

// 100 % colour bars in the top half, a pseudo-random pattern in the bottom
// half so filtering errors show.
Image testPattern(uint32_t w, uint32_t h) {
    static const uint8_t bars[8][3] = {{255, 255, 255}, {255, 255, 0}, {0, 255, 255}, {0, 255, 0},
                                       {255, 0, 255},   {255, 0, 0},   {0, 0, 255},   {0, 0, 0}};  // RGB
    Image img;
    img.w = w;
    img.h = h;
    img.pitch = w * 4;
    img.data.resize(size_t(w) * h * 4);
    img.texels.resize(size_t(w) * h);
    uint32_t seed = 12345;
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            uint8_t* p = &img.data[(size_t(y) * w + x) * 4];
            if (y < h / 2) {
                const uint8_t* c = bars[x * 8 / w];
                p[0] = c[2], p[1] = c[1], p[2] = c[0];
            } else {
                seed = seed * 1664525u + 1013904223u;
                p[0] = uint8_t(seed >> 24), p[1] = uint8_t(seed >> 16), p[2] = uint8_t(seed >> 8);
            }
            p[3] = 255;
            img.texels[size_t(y) * w + x] = {p[2] / 255.0, p[1] / 255.0, p[0] / 255.0};
        }
    }
    return img;
}

// scRGB grey levels of the HDR pattern's top quarter (x 80 cd/m2): black,
// SDR white, 100, ~203, 1000, 4000, 10000 cd/m2 and beyond PQ's range.
const double kHdrGreys[8] = {0, 1.0, 1.25, 2.5375, 12.5, 50, 125, 250};
// Colour bars of its second quarter: BT.709 primaries at SDR white, a green
// outside sRGB (negative components) but inside BT.2020, a bright orange,
// grey, yellow, and a negative colour that clips to black.
const double kHdrColours[8][3] = {{1, 0, 0},     {0, 1, 0},       {0, 0, 1}, {-0.2, 1.1, -0.05},
                                  {10, 2, 0.5},  {0.5, 0.5, 0.5}, {1, 1, 0}, {-1, -1, -1}};

// An FP16 scRGB desktop: grey and colour bars in the top half; in the bottom
// half pseudo-random values from 0.001 to 128 (0.08 to 10000 cd/m2, a few
// negative) when random, else a smooth field. Scaled cases use the smooth
// one: texture filtering weights have limited precision (D3D11: 8 fractional
// bits), and PQ magnifies a weight error between a near-black and a very
// bright neighbour into many code values.
Image hdrPattern(uint32_t w, uint32_t h, bool random) {
    Image img;
    img.w = w;
    img.h = h;
    img.format = DXGI_FORMAT_R16G16B16A16_FLOAT;
    img.linear = true;
    img.pitch = w * 8;
    img.data.resize(size_t(w) * h * 8);
    img.texels.resize(size_t(w) * h);
    uint32_t seed = 4242;
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            double v[3];
            if (y < h / 4) {
                v[0] = v[1] = v[2] = kHdrGreys[x * 8 / w];
            } else if (y < h / 2) {
                for (int i = 0; i < 3; ++i) v[i] = kHdrColours[x * 8 / w][i];
            } else if (random) {
                for (double& c : v) {
                    seed = seed * 1664525u + 1013904223u;
                    c = std::exp2(double(seed >> 8) / double(1u << 24) * 17.0 - 10.0);
                    if ((seed & 31) == 0) c = -c * 0.01;
                }
            } else {
                const double fx = double(x) / w, fy = double(y - h / 2) / (h / 2);
                v[0] = 0.01 + 50 * fx * fx * fx;
                v[1] = 0.02 + 20 * fy * fy;
                v[2] = 0.5 + 0.5 * std::sin(x * 0.05) + fx * fy;
            }
            uint16_t* p = reinterpret_cast<uint16_t*>(&img.data[(size_t(y) * w + x) * 8]);
            for (int i = 0; i < 3; ++i) p[i] = d3d::toHalf(float(v[i]));
            p[3] = d3d::toHalf(1.0f);
            img.texels[size_t(y) * w + x] = {fromHalf(p[0]), fromHalf(p[1]), fromHalf(p[2])};
        }
    }
    return img;
}

Rgb texel(const Image& img, int x, int y) {
    x = x < 0 ? 0 : x >= int(img.w) ? int(img.w) - 1 : x;
    y = y < 0 ? 0 : y >= int(img.h) ? int(img.h) - 1 : y;
    return img.texels[size_t(y) * img.w + size_t(x)];
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

double clamp01(double v) { return v < 0 ? 0 : v > 1 ? 1 : v; }
double linearToSrgb(double x) { return x <= 0.0031308 ? 12.92 * x : 1.055 * std::pow(x, 1.0 / 2.4) - 0.055; }
double srgbToLinear(double x) { return x <= 0.04045 ? x / 12.92 : std::pow((x + 0.055) / 1.055, 2.4); }

// SMPTE ST 2084 inverse EOTF (ITU-R BT.2100 table 4): absolute luminance in
// cd/m2 to the PQ signal 0..1.
double pqEncode(double cdm2) {
    const double m1 = 2610.0 / 16384, m2 = 2523.0 / 4096 * 128, c1 = 3424.0 / 4096, c2 = 2413.0 / 4096 * 32,
                 c3 = 2392.0 / 4096 * 32;
    const double y = std::pow(clamp01(cdm2 / 10000.0), m1);
    return std::pow((c1 + c2 * y) / (1 + c3 * y), m2);
}

// The non-linear R'G'B' the encoder's Y'CbCr is made of, from one sample.
Rgb transfer(Rgb c, bool linearSource, bool hdr10) {
    if (hdr10) {
        // Linear light in cd/m2 with BT.709 primaries, to BT.2020 (ITU-R
        // BT.2087 matrix), clipped at 0, PQ.
        const double scale = linearSource ? d3d::kScrgbWhiteNits : d3d::kSdrWhiteNits;
        Rgb l = linearSource ? c : Rgb{srgbToLinear(clamp01(c.r)), srgbToLinear(clamp01(c.g)), srgbToLinear(clamp01(c.b))};
        l.r *= scale, l.g *= scale, l.b *= scale;
        const double r = 0.6274040 * l.r + 0.3292820 * l.g + 0.0433136 * l.b;
        const double g = 0.0690970 * l.r + 0.9195400 * l.g + 0.0113612 * l.b;
        const double b = 0.0163916 * l.r + 0.0880132 * l.g + 0.8955950 * l.b;
        return {pqEncode(r > 0 ? r : 0), pqEncode(g > 0 ? g : 0), pqEncode(b > 0 ? b : 0)};
    }
    c = {clamp01(c.r), clamp01(c.g), clamp01(c.b)};
    if (linearSource) c = {linearToSrgb(c.r), linearToSrgb(c.g), linearToSrgb(c.b)};
    return c;
}

// The shader's fetch(): an output luma position mapped through the rotation.
Rgb fetch(const Image& img, uint32_t outW, uint32_t outH, int rotation, double lx, double ly, bool hdr10) {
    float xu[3], xv[3];
    d3d::rotationTransform(rotation, xu, xv);
    const double u = lx / outW, v = ly / outH;
    return transfer(sample(img, xu[0] * u + xu[1] * v + xu[2], xv[0] * u + xv[1] * v + xv[2]), img.linear, hdr10);
}

struct Levels {
    int lo, hi, mid, max;  // barcode 0 / 1 luma, neutral chroma, code maximum
};
Levels levelsFor(Format f) { return f == Format::P010 ? Levels{64, 940, 512, 1023} : Levels{16, 235, 128, 255}; }

int quantize(double v, int max) { return int(std::lround(clamp01(v) * max)); }

// Code values (8-bit NV12 or 10-bit P010 samples: Y plane, then CbCr pairs).
// cw x ch: the content rectangle the image is scaled into (the rest of
// w x h is padding: the same mapping continued, clamped to the edge).
std::vector<int> reference(const Image& img, uint32_t w, uint32_t h, uint32_t cw, uint32_t ch, int rotation,
                           const BarcodeLayout& bc, uint64_t value, Format format) {
    const bool hdr10 = format == Format::P010;
    const d3d::YuvCoefficients k = hdr10 ? d3d::bt2020Limited10() : d3d::bt709Limited();
    const Levels lv = levelsFor(format);
    std::vector<int> out(size_t(w) * h * 3 / 2);
    for (uint32_t y = 0; y < h; ++y) {
        for (uint32_t x = 0; x < w; ++x) {
            const int bit = d3d::barcodeBit(bc, value, x, y);
            if (bit >= 0) {
                out[size_t(y) * w + x] = bit ? lv.hi : lv.lo;
                continue;
            }
            const Rgb c = fetch(img, cw, ch, rotation, x + 0.5, y + 0.5, hdr10);
            out[size_t(y) * w + x] = quantize(k.y[0] * c.r + k.y[1] * c.g + k.y[2] * c.b + k.y[3], lv.max);
        }
    }
    int* uv = out.data() + size_t(w) * h;
    for (uint32_t j = 0; j < h / 2; ++j) {
        for (uint32_t i = 0; i < w / 2; ++i) {
            int* p = uv + (size_t(j) * (w / 2) + i) * 2;
            if (d3d::barcodeBit(bc, value, i * 2, j * 2) >= 0) {
                p[0] = p[1] = lv.mid;
                continue;
            }
            const double lx = i * 2 + 0.5, ly = j * 2 + 0.5;
            const double wts[6][3] = {{-1, 0, 1}, {0, 0, 2}, {1, 0, 1}, {-1, 1, 1}, {0, 1, 2}, {1, 1, 1}};
            Rgb s;
            for (const auto& t : wts) {
                const Rgb c = fetch(img, cw, ch, rotation, lx + t[0], ly + t[1], hdr10);
                s.r += c.r * t[2], s.g += c.g * t[2], s.b += c.b * t[2];
            }
            s.r /= 8, s.g /= 8, s.b /= 8;
            p[0] = quantize(k.u[0] * s.r + k.u[1] * s.g + k.u[2] * s.b + k.u[3], lv.max);
            p[1] = quantize(k.v[0] * s.r + k.v[1] * s.g + k.v[2] * s.b + k.v[3], lv.max);
        }
    }
    return out;
}

enum class Source { Bars, Hdr, HdrSmooth };

struct Case {
    const char* name;
    uint32_t outW, outH;
    int rotation;
    BarcodeLayout barcode;
    uint64_t value;
    int tolerance;         // max |GPU - reference| per sample, in code values
    bool noShaderBinding;  // source texture without D3D11_BIND_SHADER_RESOURCE (converter copies it)
    uint32_t contentW = 0, contentH = 0;  // picture inside outW x outH, the rest padding (0 = no padding)
    Source source = Source::Bars;
    Format format = Format::Nv12;
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
const int kBarYuv[8][3] = {{235, 128, 128}, {219, 16, 138}, {188, 154, 16}, {173, 42, 26},
                           {78, 214, 230},  {63, 102, 240}, {32, 240, 118}, {16, 128, 128}};
// The HDR pattern's grey bars in 10-bit HDR10 (PQ of 0, 80, 100, 203, 1000,
// 4000, 10000 and 20000 cd/m2: 64 + 876 x PQ), chroma 512.
const int kHdrGreyY[8] = {64, 490, 509, 573, 723, 855, 940, 940};
// Its first three colour bars (scRGB red, green, blue at 80 cd/m2) as BT.2020
// PQ Y'CbCr, computed in double precision from BT.2087, ST 2084 and BT.2020.
const int kHdrColourYuv[3][3] = {{325, 448, 598}, {450, 432, 476}, {226, 650, 535}};

// Checks the code value at output (x, y) of the top-left content against want (±1).
void expectAt(Checker& chk, const std::vector<int>& got, uint32_t w, uint32_t h, uint32_t x, uint32_t y, const int want[3],
              const char* what, int index) {
    x &= ~1u, y &= ~1u;
    const int* uv = got.data() + size_t(w) * h;
    const int Y = got[size_t(y) * w + x], U = uv[(size_t(y / 2) * (w / 2) + x / 2) * 2], V = uv[(size_t(y / 2) * (w / 2) + x / 2) * 2 + 1];
    if (std::abs(Y - want[0]) > 1 || std::abs(U - want[1]) > 1 || std::abs(V - want[2]) > 1) {
        chk.fail("%s %d: YCbCr %d,%d,%d, want %d,%d,%d", what, index, Y, U, V, want[0], want[1], want[2]);
    }
}

int runCase(ID3D11Device* dev, d3d::Nv12Converter::Output mode, const Image& img, const Case& c) {
    Checker chk(c.name);
    d3d::Nv12Converter conv;
    Status s = conv.init(dev, c.outW, c.outH, c.barcode, mode, 2, c.contentW, c.contentH, c.format);
    if (!s.ok) {
        chk.fail("init: %s", s.text.c_str());
        return chk.failures();
    }
    D3D11_TEXTURE2D_DESC td{};
    td.Width = img.w;
    td.Height = img.h;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = img.format;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = c.noShaderBinding ? D3D11_BIND_RENDER_TARGET : D3D11_BIND_SHADER_RESOURCE;
    D3D11_SUBRESOURCE_DATA init{img.data.data(), img.pitch, 0};
    ComPtr<ID3D11Texture2D> src;
    HRESULT hr = dev->CreateTexture2D(&td, &init, src.GetAddressOf());
    if (FAILED(hr)) {
        chk.fail("source texture: %s", d3d::hrText(hr).c_str());
        return chk.failures();
    }
    d3d::ConvertedFrame f;
    s = conv.convert(src.Get(), c.rotation, c.value, f);
    std::vector<uint8_t> bytes;
    if (s.ok) s = conv.readback(f, bytes);
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
    const bool p010 = c.format == Format::P010;
    // Code values: P010 samples carry the 10-bit code in their high bits, the low 6 bits zero.
    std::vector<int> got(size_t(w) * h * 3 / 2);
    int lowBits = 0;
    for (size_t i = 0; i < got.size(); ++i) {
        if (!p010) {
            got[i] = bytes[i];
            continue;
        }
        const int word = bytes[i * 2] | bytes[i * 2 + 1] << 8;
        got[i] = word >> 6;
        if ((word & 63) && ++lowBits == 1) chk.fail("P010 sample %zu = 0x%04x: low 6 bits not zero", i, word);
    }
    const uint32_t cw = c.contentW ? c.contentW : w, ch = c.contentH ? c.contentH : h;
    const std::vector<int> ref = reference(img, w, h, cw, ch, c.rotation, c.barcode, c.value, c.format);
    int maxErr = 0;
    for (size_t i = 0; i < ref.size(); ++i) {
        const int e = std::abs(got[i] - ref[i]);
        if (e > maxErr) maxErr = e;
        if (e > c.tolerance) {
            const bool luma = i < size_t(w) * h;
            const size_t j = luma ? i : (i - size_t(w) * h);
            chk.fail("%s at (%zu,%zu): got %d, want %d", luma ? "Y" : (j % 2 ? "Cr" : "Cb"), luma ? j % w : (j / 2) % (w / 2),
                     luma ? j / w : (j / 2) / (w / 2), got[i], ref[i]);
        }
    }

    // Absolute values in the middle of the bars (unrotated cases).
    const Levels lv = levelsFor(c.format);
    auto barX = [&](int b) { return (uint32_t(b) * 2 + 1) * cw / 16; };
    if (c.rotation == 0 && c.source == Source::Bars && !p010) {
        for (int b = 0; b < 8; ++b) expectAt(chk, got, w, h, barX(b), ch * 3 / 8, kBarYuv[b], "bar", b);
    } else if (c.rotation == 0 && c.source == Source::Bars) {
        // sRGB in an HDR10 stream: white at 203 cd/m2 (PQ 0.581, code 573), black 64.
        const int white[3] = {573, 512, 512}, black[3] = {64, 512, 512};
        expectAt(chk, got, w, h, barX(0), ch * 3 / 8, white, "sRGB white bar at 203 cd/m2", 0);
        expectAt(chk, got, w, h, barX(7), ch * 3 / 8, black, "sRGB black bar", 7);
    } else if (c.rotation == 0) {
        for (int b = 0; b < 8; ++b) {
            // NV12 clips scRGB to SDR: 0 is black, 1.0 and above white.
            const int want[3] = {p010 ? kHdrGreyY[b] : b == 0 ? 16 : 235, lv.mid, lv.mid};
            expectAt(chk, got, w, h, barX(b), ch / 8, want, "grey bar", b);
        }
        for (int b = 0; b < 3 && p010; ++b) expectAt(chk, got, w, h, barX(b), ch * 3 / 8, kHdrColourYuv[b], "colour bar", b);
        if (p010) {
            const int black[3] = {64, 512, 512};  // a negative scRGB colour clips to black
            expectAt(chk, got, w, h, barX(7), ch * 3 / 8, black, "negative colour bar", 7);
        } else {
            expectAt(chk, got, w, h, barX(0), ch * 3 / 8, kBarYuv[5], "clipped red bar", 0);  // scRGB red = sRGB red
        }
    } else if (c.rotation == 90 && c.source == Source::Bars) {
        // Displayed = source turned 90 degrees clockwise: the source's top-left
        // (white bar) is at the top right, its top-right (black bar) at the bottom right.
        if (std::abs(got[size_t(4) * w + (w - 4)] - 235) > 1) chk.fail("rotation: top right is not white");
        if (std::abs(got[size_t(h - 4) * w + (w - 4)] - 16) > 1) chk.fail("rotation: bottom right is not black");
    } else if (c.rotation == 90) {
        // The source's top-left grey bar (black) at the top right, its
        // top-right one (beyond 10000 cd/m2) at the bottom right.
        if (std::abs(got[size_t(4) * w + (w - 4)] - lv.lo) > 1) chk.fail("rotation: top right is not black");
        if (std::abs(got[size_t(h - 4) * w + (w - 4)] - lv.hi) > 1) chk.fail("rotation: bottom right is not peak white");
    }

    // The barcode: every block solid black / white with neutral chroma, and it decodes to the value.
    const BarcodeLayout& bc = c.barcode;
    const int* uv = got.data() + size_t(w) * h;
    if (bc.enabled) {
        uint64_t decoded = 0;
        for (int k = 0; k < bc.bits; ++k) {
            const uint32_t bx = uint32_t(bc.x + (k % bc.cols) * bc.blockW), by = uint32_t(bc.y + (k / bc.cols) * bc.blockH);
            int ones = 0;
            for (uint32_t y = by; y < by + uint32_t(bc.blockH); ++y) {
                for (uint32_t x = bx; x < bx + uint32_t(bc.blockW); ++x) {
                    const int v = got[size_t(y) * w + x];
                    if (v != lv.lo && v != lv.hi) chk.fail("barcode block %d pixel (%u,%u) = %d", k, x, y, v);
                    ones += v == lv.hi;
                    if (x % 2 == 0 && y % 2 == 0) {
                        const int* p = uv + (size_t(y / 2) * (w / 2) + x / 2) * 2;
                        if (p[0] != lv.mid || p[1] != lv.mid) chk.fail("barcode block %d chroma %d,%d", k, p[0], p[1]);
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
    std::printf("  %-32s %ux%u -> %ux%u rot %3d: %s (max error %d)\n", c.name, img.w, img.h, cw, ch, c.rotation,
                chk.failures() ? "FAIL" : "ok", maxErr);
    if (cw < w || ch < h) std::printf("  %-32s (padded to %ux%u)\n", "", w, h);
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
    using Converter = d3d::Nv12Converter;
    const bool nv12 = Converter::renderTargets(dev.device.Get(), Format::Nv12);
    const bool p010 = Converter::renderTargets(dev.device.Get(), Format::P010);
    std::printf("self-test-convert: %s device, feature level %x, %s; %s\n", kind, unsigned(dev.level),
                nv12 ? "NV12 render targets (mode nv12)"
                     : "no NV12 render targets: testing the shaders on separate R8/R8G8 planes (mode planar)",
                p010 ? "P010 render targets (HDR10 mode p010)"
                     : "no P010 render targets: HDR10 on separate R16/R16G16 planes (HDR10 mode planar)");

    const Image bars = testPattern(256, 128), hdr = hdrPattern(256, 128, true), smooth = hdrPattern(256, 128, false);
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
        // An FP16 (Windows HDR) desktop in an SDR stream: clipped to SDR.
        {"FP16 scRGB clipped to SDR", 256, 128, 0, BarcodeLayout{}, 0, 1, false, 0, 0, Source::Hdr},
        // HDR10 (step 3.9): P010, BT.2020 PQ.
        {"HDR10 1:1 + barcode", 256, 128, 0, layout(8, 96, 8, 8, 16, 32, true), 0xA5C30F1Eull, 1, false, 0, 0, Source::Hdr, Format::P010},
        {"HDR10 2:1 downscale + barcode", 128, 64, 0, layout(4, 48, 4, 4, 16, 24, true), 0x00C0FFEEull, 2, true, 0, 0, Source::HdrSmooth,
         Format::P010},
        {"HDR10 1:1 rotated 90", 128, 256, 90, BarcodeLayout{}, 0, 1, false, 0, 0, Source::Hdr, Format::P010},
        {"HDR10 from an sRGB source", 256, 128, 0, BarcodeLayout{}, 0, 1, false, 0, 0, Source::Bars, Format::P010},
        {"HDR10 scaled into 64x16 padding", 256, 96, 0, layout(8, 64, 4, 4, 16, 16, true), 0xBEEFull, 2, false, 200, 90, Source::HdrSmooth,
         Format::P010},
    };
    int failures = 0;
    for (const Case& c : cases) {
        const Image& img = c.source == Source::Hdr ? hdr : c.source == Source::HdrSmooth ? smooth : bars;
        const bool rt = c.format == Format::P010 ? p010 : nv12;
        failures += runCase(dev.device.Get(), rt ? Converter::Output::Nv12 : Converter::Output::Planar, img, c);
    }
    std::printf("self-test-convert: %s (mode %s; HDR10 mode %s)\n", failures ? "FAIL" : "ok", nv12 ? "nv12" : "planar",
                p010 ? "p010" : "planar");
    return failures ? 1 : 0;
}

}  // namespace recon
