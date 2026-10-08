#include "d3d/convert.hpp"

#include <d3dcompiler.h>

#include <algorithm>
#include <cstring>

#include "d3d/device.hpp"
#include "platform/platform.hpp"

namespace recon::d3d {

namespace {

// One vertex shader (a triangle covering the viewport) and one pixel shader
// per NV12 plane. Taps are computed in output luma pixels and mapped to the
// source through the rotation transform, so scaling and rotation share one
// code path. Chroma: Sunshine's LEFT_SUBSAMPLING_SCALE filter
// (convert_yuv420_packed_uv_ps_base.hlsl): luma-centred taps weighted
// [1 2 1] across the even luma column and its neighbours, [1 1] over the two
// luma rows, i.e. chroma sited at the even column, halfway down (type 0). At
// 1:1 every tap hits a texel centre, so the result is exact.
//
// HDR10 (P010, step 3.9): each tap is scRGB (linear BT.709 primaries, 1.0 =
// 80 cd/m2, the FP16 desktop of Windows HDR) turned into BT.2020 and PQ
// (Sunshine's convert_*_perceptual_quantizer shaders do the same maths), then
// the BT.2020 non-constant-luminance matrix gives 10-bit limited-range codes.
// Bilinear sampling interpolates the linear values. The chroma taps are
// averaged after PQ, like the SDR path averages gamma-encoded values (Sunshine
// averages linear light before PQ; the difference shows only at sharp
// high-contrast colour edges).
const char kShader[] = R"HLSL(
Texture2D<float4> src : register(t0);
SamplerState lin : register(s0);

cbuffer Params : register(b0) {
    float4 xformU;    // s = dot(xformU.xyz, float3(u, v, 1))
    float4 xformV;    // t = dot(xformV.xyz, float3(u, v, 1))
    float4 lumaSize;  // W, H, 1/W, 1/H of the output's content (the picture without padding)
    float4 coefY;     // dot(rgb, coef.xyz) + coef.w: the code value / its maximum (255, or 1023 for P010)
    float4 coefU;
    float4 coefV;
    uint4 bcRect;     // barcode x0, y0, x1, y1 (exclusive), output pixels: 8 x 3 cells
    uint4 bcGrid;     // x: cell size in output pixels
    uint4 bcValue;    // x: the 24-bit word (value << 8 | crc), w: enabled
    uint4 flags;      // x: source is linear (scRGB FP16); y: HDR10 output (BT.2020 PQ)
    float4 levels;    // barcode luma 0 / 1, neutral chroma (code / maximum); w: 1023 = 10-bit codes in UNORM16 (P010), 0 = UNORM8
    float4 nits;      // HDR10: cd/m2 of 1.0 in a linear (scRGB) source, of white in an sRGB source
};

struct VSOut { float4 pos : SV_Position; };

VSOut vs_main(uint id : SV_VertexID) {
    VSOut o;
    float2 p = float2((id << 1) & 2, id & 2);
    o.pos = float4(p.x * 2.0 - 1.0, 1.0 - p.y * 2.0, 0.0, 1.0);
    return o;
}

float3 linearToSrgb(float3 x) {
    return x <= 0.0031308 ? 12.92 * x : 1.055 * pow(x, 1.0 / 2.4) - 0.055;
}

float3 srgbToLinear(float3 x) {
    return x <= 0.04045 ? x / 12.92 : pow((x + 0.055) / 1.055, 2.4);
}

// SMPTE ST 2084 (PQ) inverse EOTF of absolute luminance in cd/m2.
float3 pq(float3 cdm2) {
    float3 y = pow(saturate(cdm2 / 10000.0), 0.1593017578125);
    return pow((0.8359375 + 18.8515625 * y) / (1.0 + 18.6875 * y), 78.84375);
}

float3 fetch(float2 lumaPos) {
    float3 uv1 = float3(lumaPos * lumaSize.zw, 1.0);
    float2 st = float2(dot(xformU.xyz, uv1), dot(xformV.xyz, uv1));
    float3 c = src.SampleLevel(lin, st, 0).rgb;
    if (flags.y != 0) {
        // HDR10: absolute linear light with BT.709 primaries (scRGB 1.0 =
        // 80 cd/m2; an sRGB image at SDR reference white), to BT.2020 (ITU-R
        // BT.2087), then PQ. Colours outside BT.2020 (negative) clip.
        float3 l = flags.x != 0 ? c * nits.x : srgbToLinear(saturate(c)) * nits.y;
        float3 wide = float3(dot(float3(0.6274040, 0.3292820, 0.0433136), l),
                             dot(float3(0.0690970, 0.9195400, 0.0113612), l),
                             dot(float3(0.0163916, 0.0880132, 0.8955950), l));
        return pq(max(wide, 0.0));
    }
    c = saturate(c);
    if (flags.x != 0) c = linearToSrgb(c);
    return c;
}

// P010: the 10-bit code in the high bits of the 16-bit word, the low 6 bits
// zero. UNORM8 (NV12): the target's conversion rounds.
float quantize(float v) {
    return levels.w > 0.0 ? round(saturate(v) * levels.w) * (64.0 / 65535.0) : v;
}

// The frame barcode (proto/barcode.go): cell k, row-major in 8 columns, shows
// bit 23 - k of the word.
int barcodeBit(uint2 p) {
    if (bcValue.w == 0 || p.x < bcRect.x || p.y < bcRect.y || p.x >= bcRect.z || p.y >= bcRect.w) return -1;
    uint k = ((p.y - bcRect.y) / bcGrid.x) * 8 + (p.x - bcRect.x) / bcGrid.x;
    return (int)((bcValue.x >> (23 - k)) & 1);
}

float ps_y(VSOut i) : SV_Target {
    int bit = barcodeBit(uint2(i.pos.xy));
    if (bit >= 0) return quantize(bit != 0 ? levels.y : levels.x);
    return quantize(dot(coefY.xyz, fetch(i.pos.xy)) + coefY.w);
}

float2 ps_uv(VSOut i) : SV_Target {
    uint2 c = uint2(i.pos.xy);
    if (barcodeBit(c * 2) >= 0) return float2(quantize(levels.z), quantize(levels.z));
    float2 l = float2(c * 2) + 0.5;
    float3 rgb = fetch(l + float2(-1.0, 0.0)) + 2.0 * fetch(l) + fetch(l + float2(1.0, 0.0)) +
                 fetch(l + float2(-1.0, 1.0)) + 2.0 * fetch(l + float2(0.0, 1.0)) + fetch(l + float2(1.0, 1.0));
    rgb *= 0.125;
    return float2(quantize(dot(coefU.xyz, rgb) + coefU.w), quantize(dot(coefV.xyz, rgb) + coefV.w));
}
)HLSL";

struct Constants {
    float xformU[4], xformV[4], lumaSize[4], coefY[4], coefU[4], coefV[4];
    uint32_t bcRect[4], bcGrid[4], bcValue[4], flags[4];
    float levels[4], nits[4];
};
static_assert(sizeof(Constants) == 192, "constant buffer layout");

Status compile(const char* entry, const char* target, ComPtr<ID3DBlob>& out) {
    static pD3DCompile fn = [] {
        std::string err;
        HMODULE m = loadSystemLibrary(L"d3dcompiler_47.dll", err);  // kept loaded
        if (!m) logf(LogLevel::Warn, "d3dcompiler_47.dll: %s", err.c_str());
        return m ? reinterpret_cast<pD3DCompile>(reinterpret_cast<void*>(GetProcAddress(m, "D3DCompile"))) : nullptr;
    }();
    if (!fn) return Status::Error("init_failed", "D3DCompile (d3dcompiler_47.dll) is not available");
    ComPtr<ID3DBlob> errors;
    const HRESULT hr = fn(kShader, sizeof(kShader) - 1, "recon-convert.hlsl", nullptr, nullptr, entry, target,
                          D3DCOMPILE_OPTIMIZATION_LEVEL3 | D3DCOMPILE_ENABLE_STRICTNESS, 0, out.ReleaseAndGetAddressOf(),
                          errors.GetAddressOf());
    if (FAILED(hr)) {
        std::string msg = errors ? std::string(static_cast<const char*>(errors->GetBufferPointer()), errors->GetBufferSize())
                                 : std::string();
        return Status::Error("init_failed", std::string("compiling the ") + entry + " shader failed: " + hrText(hr) + " " + msg);
    }
    return Status::Ok();
}

// Holds the device's critical section for a series of immediate-context calls
// (other threads' calls on the device wait meanwhile). Reentrant: every
// single call takes it again internally with multithread protection on.
class DeviceLock {
public:
    explicit DeviceLock(ID3D10Multithread* mt) : mt_(mt) {
        if (mt_) mt_->Enter();
    }
    ~DeviceLock() {
        if (mt_) mt_->Leave();
    }
    DeviceLock(const DeviceLock&) = delete;
    DeviceLock& operator=(const DeviceLock&) = delete;

private:
    ID3D10Multithread* mt_;
};

// A failed D3D11 call: device_lost (fatal) if the device was removed, else `code`.
Status failure(ID3D11Device* device, const char* code, const std::string& text) {
    Status s;
    if (deviceRemoved(device, text, s)) return s;
    return Status::Error(code, text);
}

// SRV format for a source texture format; linear = scRGB.
bool srvFormat(DXGI_FORMAT f, DXGI_FORMAT& out, bool& linear) {
    linear = false;
    switch (f) {
    case DXGI_FORMAT_B8G8R8A8_TYPELESS:
    case DXGI_FORMAT_B8G8R8A8_UNORM:
    case DXGI_FORMAT_B8G8R8A8_UNORM_SRGB: out = DXGI_FORMAT_B8G8R8A8_UNORM; return true;
    case DXGI_FORMAT_B8G8R8X8_TYPELESS:
    case DXGI_FORMAT_B8G8R8X8_UNORM:
    case DXGI_FORMAT_B8G8R8X8_UNORM_SRGB: out = DXGI_FORMAT_B8G8R8X8_UNORM; return true;
    case DXGI_FORMAT_R8G8B8A8_TYPELESS:
    case DXGI_FORMAT_R8G8B8A8_UNORM:
    case DXGI_FORMAT_R8G8B8A8_UNORM_SRGB: out = DXGI_FORMAT_R8G8B8A8_UNORM; return true;
    case DXGI_FORMAT_R16G16B16A16_TYPELESS:
    case DXGI_FORMAT_R16G16B16A16_FLOAT:
        out = DXGI_FORMAT_R16G16B16A16_FLOAT;
        linear = true;  // scRGB: PQ for HDR10 output, clipped to SDR for NV12
        return true;
    default: return false;
    }
}

}  // namespace

void rotationTransform(int rotation, float xu[3], float xv[3]) {
    // Sunshine base_vs.hlsl rotates the texture coordinates by
    // -90 * (DXGI_MODE_ROTATION - 1) degrees (display_vram.cpp); as maps:
    switch (rotation) {
    case 90:  // displayed = texture turned 90 degrees clockwise
        xu[0] = 0, xu[1] = 1, xu[2] = 0;
        xv[0] = -1, xv[1] = 0, xv[2] = 1;
        break;
    case 180:
        xu[0] = -1, xu[1] = 0, xu[2] = 1;
        xv[0] = 0, xv[1] = -1, xv[2] = 1;
        break;
    case 270:
        xu[0] = 0, xu[1] = -1, xu[2] = 1;
        xv[0] = 1, xv[1] = 0, xv[2] = 0;
        break;
    default:
        xu[0] = 1, xu[1] = 0, xu[2] = 0;
        xv[0] = 0, xv[1] = 1, xv[2] = 0;
        break;
    }
}

YuvCoefficients bt2020Limited10() {
    // ITU-R BT.2020 non-constant luminance: Kr 0.2627, Kb 0.0593; 10-bit
    // limited range: Y 64..940, Cb/Cr 64..960 around 512 (BT.2020 table 5,
    // BT.2100 table 9), in code / 1023.
    constexpr double kr = 0.2627, kb = 0.0593, kg = 1.0 - kr - kb;
    constexpr double ys = 876.0 / 1023.0, cs = 896.0 / 1023.0;
    YuvCoefficients c{};
    c.y[0] = float(ys * kr), c.y[1] = float(ys * kg), c.y[2] = float(ys * kb), c.y[3] = float(64.0 / 1023.0);
    c.u[0] = float(cs * -kr / (2 * (1 - kb))), c.u[1] = float(cs * -kg / (2 * (1 - kb))), c.u[2] = float(cs * 0.5);
    c.u[3] = float(512.0 / 1023.0);
    c.v[0] = float(cs * 0.5), c.v[1] = float(cs * -kg / (2 * (1 - kr))), c.v[2] = float(cs * -kb / (2 * (1 - kr)));
    c.v[3] = float(512.0 / 1023.0);
    return c;
}

YuvCoefficients bt709Limited() {
    // ITU-R BT.709: Kr 0.2126, Kb 0.0722; 8-bit limited range: Y 16..235,
    // Cb/Cr 16..240 around 128 (ITU-R BT.709-6 table 4).
    constexpr double kr = 0.2126, kb = 0.0722, kg = 1.0 - kr - kb;
    constexpr double ys = 219.0 / 255.0, cs = 224.0 / 255.0;
    YuvCoefficients c{};
    c.y[0] = float(ys * kr), c.y[1] = float(ys * kg), c.y[2] = float(ys * kb), c.y[3] = float(16.0 / 255.0);
    c.u[0] = float(cs * -kr / (2 * (1 - kb))), c.u[1] = float(cs * -kg / (2 * (1 - kb))), c.u[2] = float(cs * 0.5);
    c.u[3] = float(128.0 / 255.0);
    c.v[0] = float(cs * 0.5), c.v[1] = float(cs * -kg / (2 * (1 - kr))), c.v[2] = float(cs * -kb / (2 * (1 - kr)));
    c.v[3] = float(128.0 / 255.0);
    return c;
}

uint16_t toHalf(float f) {
    uint32_t x;
    std::memcpy(&x, &f, 4);
    const uint32_t sign = (x >> 16) & 0x8000u;
    const int32_t exp = int32_t((x >> 23) & 0xff) - 127 + 15;
    uint32_t mant = x & 0x7fffffu;
    if (exp >= 31) return uint16_t(sign | 0x7c00u);  // too large (or NaN): infinity
    if (exp <= 0) {                                   // subnormal or zero
        if (exp < -10) return uint16_t(sign);
        mant |= 0x800000u;
        const uint32_t shift = uint32_t(14 - exp), rem = mant & ((1u << shift) - 1), mid = 1u << (shift - 1);
        uint32_t h = mant >> shift;
        if (rem > mid || (rem == mid && (h & 1))) ++h;
        return uint16_t(sign | h);
    }
    uint32_t h = (uint32_t(exp) << 10) | (mant >> 13);
    const uint32_t rem = mant & 0x1fffu;
    if (rem > 0x1000u || (rem == 0x1000u && (h & 1))) ++h;  // a carry rounds up into the exponent
    return uint16_t(sign | h);
}

uint8_t barcodeCrc(uint16_t value) {
    uint8_t crc = 0;
    for (const uint8_t byte : {uint8_t(value >> 8), uint8_t(value)}) {
        crc ^= byte;
        for (int i = 0; i < 8; ++i) crc = (crc & 0x80) ? uint8_t((crc << 1) ^ 0x07) : uint8_t(crc << 1);
    }
    return uint8_t(crc ^ 0x55);
}

uint32_t barcodeWord(uint16_t value) { return uint32_t(value) << 8 | barcodeCrc(value); }

int barcodeBit(const BarcodeLayout& b, uint16_t value, uint32_t x, uint32_t y) {
    if (!b.enabled) return -1;
    const uint32_t x0 = uint32_t(b.x), y0 = uint32_t(b.y), cell = uint32_t(b.cell);
    if (x < x0 || y < y0 || x >= x0 + uint32_t(b.width()) || y >= y0 + uint32_t(b.height())) return -1;
    const uint32_t k = (y - y0) / cell * uint32_t(kBarcodeCols) + (x - x0) / cell;
    return int((barcodeWord(value) >> (uint32_t(kBarcodeBits) - 1 - k)) & 1);
}

std::string barcodeProblem(const BarcodeLayout& b, uint32_t width, uint32_t height) {
    if (!b.enabled) return {};
    if (int64_t(b.x) + b.width() > int64_t(width) || int64_t(b.y) + b.height() > int64_t(height)) {
        return "the barcode (" + std::to_string(b.width()) + "x" + std::to_string(b.height()) + " at " + std::to_string(b.x) +
               "," + std::to_string(b.y) + ") does not fit the " + std::to_string(width) + "x" + std::to_string(height) +
               " output";
    }
    return {};
}

Nv12Converter::~Nv12Converter() = default;

bool Nv12Converter::renderTargets(ID3D11Device* device, Format format) {
    UINT support = 0;
    return SUCCEEDED(device->CheckFormatSupport(format == Format::P010 ? DXGI_FORMAT_P010 : DXGI_FORMAT_NV12, &support)) &&
           (support & D3D11_FORMAT_SUPPORT_RENDER_TARGET) && (support & D3D11_FORMAT_SUPPORT_TEXTURE2D);
}

Status Nv12Converter::init(ID3D11Device* device, uint32_t width, uint32_t height, const BarcodeLayout& barcode,
                           Output output, int poolSize, uint32_t contentWidth, uint32_t contentHeight, Format format) {
    if (!device || width < 2 || height < 2 || (width & 1) || (height & 1) || width > 16384 || height > 16384) {
        return Status::Error("init_failed", "bad conversion size " + std::to_string(width) + "x" + std::to_string(height));
    }
    if (!contentWidth) contentWidth = width;
    if (!contentHeight) contentHeight = height;
    if (contentWidth < 2 || contentHeight < 2 || (contentWidth & 1) || (contentHeight & 1) || contentWidth > width ||
        contentHeight > height) {
        return Status::Error("init_failed", "bad content size " + std::to_string(contentWidth) + "x" +
                                                std::to_string(contentHeight) + " in " + std::to_string(width) + "x" +
                                                std::to_string(height));
    }
    if (std::string p = barcodeProblem(barcode, contentWidth, contentHeight); !p.empty()) return Status::Error("bad_message", p);
    if (output == Output::Nv12 && !renderTargets(device, format)) {
        return Status::Error("unsupported", std::string("this D3D11 device cannot render to ") +
                                                (format == Format::P010 ? "P010" : "NV12") + " textures");
    }
    device_ = device;
    device_->GetImmediateContext(ctx_.ReleaseAndGetAddressOf());
    if (FAILED(device_->QueryInterface(__uuidof(ID3D10Multithread), reinterpret_cast<void**>(mt_.ReleaseAndGetAddressOf())))) {
        mt_.Reset();
        logf(LogLevel::Warn, "the D3D11 device exposes no ID3D10Multithread: conversions run without the device lock");
    }
    width_ = width;
    height_ = height;
    contentW_ = contentWidth;
    contentH_ = contentHeight;
    barcode_ = barcode;
    output_ = output;
    format_ = format;
    poolSize_ = size_t(poolSize < 1 ? 1 : poolSize);
    slots_.clear();
    slots_.reserve(poolSize_);
    pool_ = std::make_shared<PoolState>();

    ComPtr<ID3DBlob> vs, psY, psUV;
    Status s = compile("vs_main", "vs_4_0", vs);
    if (s.ok) s = compile("ps_y", "ps_4_0", psY);
    if (s.ok) s = compile("ps_uv", "ps_4_0", psUV);
    if (!s.ok) return s;
    HRESULT hr = device_->CreateVertexShader(vs->GetBufferPointer(), vs->GetBufferSize(), nullptr, vs_.ReleaseAndGetAddressOf());
    if (SUCCEEDED(hr)) hr = device_->CreatePixelShader(psY->GetBufferPointer(), psY->GetBufferSize(), nullptr, psY_.ReleaseAndGetAddressOf());
    if (SUCCEEDED(hr)) hr = device_->CreatePixelShader(psUV->GetBufferPointer(), psUV->GetBufferSize(), nullptr, psUV_.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return Status::Error("init_failed", "creating the conversion shaders failed: " + hrText(hr));

    D3D11_SAMPLER_DESC sd{};
    sd.Filter = D3D11_FILTER_MIN_MAG_MIP_LINEAR;
    sd.AddressU = sd.AddressV = sd.AddressW = D3D11_TEXTURE_ADDRESS_CLAMP;
    sd.ComparisonFunc = D3D11_COMPARISON_NEVER;
    sd.MaxLOD = D3D11_FLOAT32_MAX;
    hr = device_->CreateSamplerState(&sd, sampler_.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return Status::Error("init_failed", "CreateSamplerState failed: " + hrText(hr));

    D3D11_RASTERIZER_DESC rs{};
    rs.FillMode = D3D11_FILL_SOLID;
    rs.CullMode = D3D11_CULL_NONE;
    rs.DepthClipEnable = TRUE;
    hr = device_->CreateRasterizerState(&rs, rasterizer_.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return Status::Error("init_failed", "CreateRasterizerState failed: " + hrText(hr));

    D3D11_BUFFER_DESC bd{};
    bd.ByteWidth = sizeof(Constants);
    bd.Usage = D3D11_USAGE_DYNAMIC;
    bd.BindFlags = D3D11_BIND_CONSTANT_BUFFER;
    bd.CPUAccessFlags = D3D11_CPU_ACCESS_WRITE;
    hr = device_->CreateBuffer(&bd, nullptr, cb_.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return Status::Error("init_failed", "creating the constant buffer failed: " + hrText(hr));
    stagingA_.Reset();
    stagingB_.Reset();
    srvCache_.clear();
    return Status::Ok();
}

Status Nv12Converter::createSlot(Slot& s) {
    D3D11_TEXTURE2D_DESC td{};
    td.Width = width_;
    td.Height = height_;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    HRESULT hr;
    D3D11_RENDER_TARGET_VIEW_DESC rd{};
    rd.ViewDimension = D3D11_RTV_DIMENSION_TEXTURE2D;
    const bool p010 = format_ == Format::P010;
    const DXGI_FORMAT lumaFormat = p010 ? DXGI_FORMAT_R16_UNORM : DXGI_FORMAT_R8_UNORM;
    const DXGI_FORMAT chromaFormat = p010 ? DXGI_FORMAT_R16G16_UNORM : DXGI_FORMAT_R8G8_UNORM;
    const char* name = p010 ? "P010" : "NV12";
    if (output_ == Output::Nv12) {
        td.Format = p010 ? DXGI_FORMAT_P010 : DXGI_FORMAT_NV12;
        // Shader-resource too, as OBS's texture-amf.cpp creates its AMF input
        // textures; without it if the driver refuses that combination.
        td.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
        hr = device_->CreateTexture2D(&td, nullptr, s.nv12.ReleaseAndGetAddressOf());
        if (FAILED(hr)) {
            td.BindFlags = D3D11_BIND_RENDER_TARGET;
            hr = device_->CreateTexture2D(&td, nullptr, s.nv12.ReleaseAndGetAddressOf());
        }
        if (FAILED(hr)) return failure(device_.Get(), "init_failed", std::string("creating a ") + name + " texture failed: " + hrText(hr));
        // The plane is picked by the view format (luma R8 / R16, chroma R8G8 /
        // R16G16), as Sunshine's display_vram.cpp does for its NV12 and P010
        // encoder textures.
        rd.Format = lumaFormat;
        hr = device_->CreateRenderTargetView(s.nv12.Get(), &rd, s.rtvY.ReleaseAndGetAddressOf());
        if (SUCCEEDED(hr)) {
            rd.Format = chromaFormat;
            hr = device_->CreateRenderTargetView(s.nv12.Get(), &rd, s.rtvUV.ReleaseAndGetAddressOf());
        }
        if (FAILED(hr)) return failure(device_.Get(), "init_failed", std::string(name) + " plane render target views failed: " + hrText(hr));
        return Status::Ok();
    }
    td.BindFlags = D3D11_BIND_RENDER_TARGET;
    td.Format = lumaFormat;
    hr = device_->CreateTexture2D(&td, nullptr, s.y.ReleaseAndGetAddressOf());
    if (SUCCEEDED(hr)) {
        td.Width /= 2;
        td.Height /= 2;
        td.Format = chromaFormat;
        hr = device_->CreateTexture2D(&td, nullptr, s.uv.ReleaseAndGetAddressOf());
    }
    if (SUCCEEDED(hr)) hr = device_->CreateRenderTargetView(s.y.Get(), nullptr, s.rtvY.ReleaseAndGetAddressOf());
    if (SUCCEEDED(hr)) hr = device_->CreateRenderTargetView(s.uv.Get(), nullptr, s.rtvUV.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return failure(device_.Get(), "init_failed", "creating the plane textures failed: " + hrText(hr));
    return Status::Ok();
}

Status Nv12Converter::sourceView(ID3D11Texture2D* src, SrvEntry*& out) {
    // Drop cached views of textures nobody else holds any more (old capture
    // textures after a mode change).
    for (size_t i = 0; i < srvCache_.size();) {
        ID3D11Texture2D* t = srvCache_[i].texture.Get();
        t->AddRef();
        if (t != src && t->Release() == 1) srvCache_.erase(srvCache_.begin() + ptrdiff_t(i));
        else ++i;
    }
    for (size_t i = 0; i < srvCache_.size(); ++i) {
        if (srvCache_[i].texture.Get() == src) {
            if (i) std::rotate(srvCache_.begin(), srvCache_.begin() + ptrdiff_t(i), srvCache_.begin() + ptrdiff_t(i) + 1);
            out = &srvCache_.front();
            return Status::Ok();
        }
    }
    D3D11_TEXTURE2D_DESC td{};
    src->GetDesc(&td);
    SrvEntry e;
    e.texture = src;
    DXGI_FORMAT f;
    if (!srvFormat(td.Format, f, e.linear)) {
        return Status::Error("unsupported", "cannot convert capture format " + std::to_string(int(td.Format)));
    }
    ID3D11Texture2D* viewed = src;
    if (td.ArraySize != 1) {
        // A texture array (AMF can hand out array slices): only slice 0 is
        // converted. VERIFY with AMD Direct Capture surfaces (docs/VENDOR_NOTES.md).
        logf(LogLevel::Warn, "capture texture is an array of %u: converting slice 0", td.ArraySize);
    }
    if (!(td.BindFlags & D3D11_BIND_SHADER_RESOURCE) || td.ArraySize != 1 || td.SampleDesc.Count != 1 || td.MipLevels != 1) {
        D3D11_TEXTURE2D_DESC cd{};
        cd.Width = td.Width;
        cd.Height = td.Height;
        cd.MipLevels = 1;
        cd.ArraySize = 1;
        cd.Format = td.Format;
        cd.SampleDesc.Count = 1;
        cd.Usage = D3D11_USAGE_DEFAULT;
        cd.BindFlags = D3D11_BIND_SHADER_RESOURCE;
        const HRESULT hr = device_->CreateTexture2D(&cd, nullptr, e.copy.ReleaseAndGetAddressOf());
        if (FAILED(hr)) return failure(device_.Get(), "init_failed", "creating a copy of the capture texture failed: " + hrText(hr));
        viewed = e.copy.Get();
    }
    D3D11_SHADER_RESOURCE_VIEW_DESC sd{};
    sd.Format = f;
    sd.ViewDimension = D3D11_SRV_DIMENSION_TEXTURE2D;
    sd.Texture2D.MipLevels = 1;
    const HRESULT hr = device_->CreateShaderResourceView(viewed, &sd, e.srv.ReleaseAndGetAddressOf());
    if (FAILED(hr)) return failure(device_.Get(), "init_failed", "CreateShaderResourceView on the capture texture failed: " + hrText(hr));
    srvCache_.insert(srvCache_.begin(), std::move(e));
    if (srvCache_.size() > 8) srvCache_.pop_back();
    out = &srvCache_.front();
    return Status::Ok();
}

Status Nv12Converter::convert(ID3D11Texture2D* src, int rotation, uint16_t barcodeValue, ConvertedFrame& out) {
    if (!cb_) return Status::Error("init_failed", "converter not initialized");
    // A free pool texture (the encoder may still hold some).
    int index = -1;
    {
        std::lock_guard<std::mutex> lock(pool_->mu);
        for (size_t i = 0; i < slots_.size(); ++i) {
            if (!pool_->busy[i]) {
                index = int(i);
                break;
            }
        }
        if (index < 0 && slots_.size() < poolSize_) {
            index = int(slots_.size());
            pool_->busy.push_back(false);
        }
        if (index < 0) return Status::Error("pool_exhausted", "every converted frame is still held by the encoder");
        pool_->busy[size_t(index)] = true;
    }
    auto release = [pool = pool_, index] {
        std::lock_guard<std::mutex> lock(pool->mu);
        pool->busy[size_t(index)] = false;
    };
    // From here on the device lock is held (lock order: device, then pool:
    // an encoder thread may release a frame while it holds the device lock).
    const DeviceLock deviceLock(mt_.Get());
    if (size_t(index) == slots_.size()) {
        slots_.emplace_back();
        Status s = createSlot(slots_.back());
        if (!s.ok) {
            slots_.pop_back();
            std::lock_guard<std::mutex> lock(pool_->mu);
            pool_->busy.pop_back();
            return s;
        }
    }
    Slot& slot = slots_[size_t(index)];

    SrvEntry* srv = nullptr;
    Status s = sourceView(src, srv);
    if (!s.ok) {
        release();
        return s;
    }
    if (srv->copy) ctx_->CopySubresourceRegion(srv->copy.Get(), 0, 0, 0, 0, src, 0, nullptr);

    D3D11_MAPPED_SUBRESOURCE m{};
    HRESULT hr = ctx_->Map(cb_.Get(), 0, D3D11_MAP_WRITE_DISCARD, 0, &m);
    if (FAILED(hr)) {
        release();
        return failure(device_.Get(), "encode_failed", "mapping the constant buffer failed: " + hrText(hr));
    }
    Constants c{};
    rotationTransform(rotation, c.xformU, c.xformV);
    // Output pixels map to the source through the content size: beyond it
    // (padding) the coordinates pass 1.0 and the clamp sampler repeats the edge.
    c.lumaSize[0] = float(contentW_), c.lumaSize[1] = float(contentH_);
    c.lumaSize[2] = 1.0f / float(contentW_), c.lumaSize[3] = 1.0f / float(contentH_);
    const bool hdr10 = format_ == Format::P010;
    const YuvCoefficients k = hdr10 ? bt2020Limited10() : bt709Limited();
    std::memcpy(c.coefY, k.y, sizeof(c.coefY));
    std::memcpy(c.coefU, k.u, sizeof(c.coefU));
    std::memcpy(c.coefV, k.v, sizeof(c.coefV));
    if (barcode_.enabled) {
        c.bcRect[0] = uint32_t(barcode_.x), c.bcRect[1] = uint32_t(barcode_.y);
        c.bcRect[2] = uint32_t(barcode_.x + barcode_.width()), c.bcRect[3] = uint32_t(barcode_.y + barcode_.height());
        c.bcGrid[0] = uint32_t(barcode_.cell);
        c.bcValue[0] = barcodeWord(barcodeValue), c.bcValue[3] = 1;
    }
    c.flags[0] = srv->linear ? 1 : 0;
    c.flags[1] = hdr10 ? 1 : 0;
    // Barcode cells: limited-range black and white, neutral chroma (16 / 235
    // / 128 in 8 bits, 64 / 940 / 512 in 10 bits).
    const float codeMax = hdr10 ? 1023.0f : 255.0f;
    c.levels[0] = (hdr10 ? 64.0f : 16.0f) / codeMax;
    c.levels[1] = (hdr10 ? 940.0f : 235.0f) / codeMax;
    c.levels[2] = (hdr10 ? 512.0f : 128.0f) / codeMax;
    c.levels[3] = hdr10 ? 1023.0f : 0.0f;
    c.nits[0] = float(kScrgbWhiteNits);
    c.nits[1] = float(kSdrWhiteNits);
    std::memcpy(m.pData, &c, sizeof(c));
    ctx_->Unmap(cb_.Get(), 0);

    // Every stage the draws depend on, set or cleared: the immediate context
    // is shared, so other components may have left anything bound (a hull
    // shader would make the TRIANGLELIST draws invalid, a predicate could skip
    // them, stream-output targets would capture them).
    ctx_->SetPredication(nullptr, FALSE);
    ctx_->IASetInputLayout(nullptr);
    ctx_->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLELIST);
    ctx_->VSSetShader(vs_.Get(), nullptr, 0);
    ctx_->HSSetShader(nullptr, nullptr, 0);
    ctx_->DSSetShader(nullptr, nullptr, 0);
    ctx_->GSSetShader(nullptr, nullptr, 0);
    ctx_->SOSetTargets(0, nullptr, nullptr);
    ctx_->RSSetState(rasterizer_.Get());  // no scissor test, no culling
    ctx_->OMSetBlendState(nullptr, nullptr, 0xffffffff);
    ctx_->OMSetDepthStencilState(nullptr, 0);
    ID3D11Buffer* cbs[] = {cb_.Get()};
    ctx_->PSSetConstantBuffers(0, 1, cbs);
    ID3D11SamplerState* samplers[] = {sampler_.Get()};
    ctx_->PSSetSamplers(0, 1, samplers);
    ID3D11ShaderResourceView* srvs[] = {srv->srv.Get()};
    ctx_->PSSetShaderResources(0, 1, srvs);

    D3D11_VIEWPORT vp{};
    vp.Width = float(width_);
    vp.Height = float(height_);
    vp.MaxDepth = 1.0f;
    ID3D11RenderTargetView* rtv = slot.rtvY.Get();
    ctx_->OMSetRenderTargets(1, &rtv, nullptr);
    ctx_->RSSetViewports(1, &vp);
    ctx_->PSSetShader(psY_.Get(), nullptr, 0);
    ctx_->Draw(3, 0);

    vp.Width = float(width_ / 2);
    vp.Height = float(height_ / 2);
    rtv = slot.rtvUV.Get();
    ctx_->OMSetRenderTargets(1, &rtv, nullptr);
    ctx_->RSSetViewports(1, &vp);
    ctx_->PSSetShader(psUV_.Get(), nullptr, 0);
    ctx_->Draw(3, 0);

    // Unbind, so neither the capture's next copy into the source nor the
    // encoder's read of the target finds them still bound.
    ID3D11ShaderResourceView* noSrv[] = {nullptr};
    ctx_->PSSetShaderResources(0, 1, noSrv);
    ctx_->OMSetRenderTargets(0, nullptr, nullptr);
    ctx_->Flush();  // start the GPU work now (OBS texture-amf.cpp flushes before handing a texture to AMF)

    out = ConvertedFrame{};
    out.nv12 = slot.nv12.Get();
    out.y = slot.y.Get();
    out.uv = slot.uv.Get();
    out.index = index;
    out.hold = std::shared_ptr<void>(new int(index), [release](void* p) {
        delete static_cast<int*>(p);
        release();
    });
    return Status::Ok();
}

Status Nv12Converter::readback(const ConvertedFrame& f, std::vector<uint8_t>& out) {
    const DeviceLock deviceLock(mt_.Get());
    const uint32_t w = width_, h = height_;
    const uint32_t bps = format_ == Format::P010 ? 2 : 1;  // bytes per sample
    out.assign(size_t(w) * h * 3 / 2 * bps, 0);
    auto staging = [&](ID3D11Texture2D* like, ComPtr<ID3D11Texture2D>& st) -> HRESULT {
        if (st) return S_OK;
        D3D11_TEXTURE2D_DESC td{};
        like->GetDesc(&td);
        td.Usage = D3D11_USAGE_STAGING;
        td.BindFlags = 0;
        td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        td.MiscFlags = 0;
        return device_->CreateTexture2D(&td, nullptr, st.GetAddressOf());
    };
    auto copyRows = [](const D3D11_MAPPED_SUBRESOURCE& m, size_t offset, uint32_t rowBytes, uint32_t rows, uint8_t* dst) {
        const auto* p = static_cast<const uint8_t*>(m.pData) + offset;
        for (uint32_t r = 0; r < rows; ++r) std::memcpy(dst + size_t(r) * rowBytes, p + size_t(r) * m.RowPitch, rowBytes);
    };
    D3D11_MAPPED_SUBRESOURCE m{};
    if (output_ == Output::Nv12) {
        HRESULT hr = staging(f.nv12, stagingA_);
        if (FAILED(hr)) return Status::Error("readback", "NV12 staging texture: " + hrText(hr));
        ctx_->CopyResource(stagingA_.Get(), f.nv12);
        hr = ctx_->Map(stagingA_.Get(), 0, D3D11_MAP_READ, 0, &m);
        if (FAILED(hr)) return Status::Error("readback", "Map: " + hrText(hr));
        copyRows(m, 0, w * bps, h, out.data());
        copyRows(m, size_t(m.RowPitch) * h, w * bps, h / 2, out.data() + size_t(w) * h * bps);  // chroma follows the luma plane
        ctx_->Unmap(stagingA_.Get(), 0);
        return Status::Ok();
    }
    HRESULT hr = staging(f.y, stagingA_);
    if (SUCCEEDED(hr)) hr = staging(f.uv, stagingB_);
    if (FAILED(hr)) return Status::Error("readback", "plane staging textures: " + hrText(hr));
    ctx_->CopyResource(stagingA_.Get(), f.y);
    ctx_->CopyResource(stagingB_.Get(), f.uv);
    hr = ctx_->Map(stagingA_.Get(), 0, D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) return Status::Error("readback", "Map: " + hrText(hr));
    copyRows(m, 0, w * bps, h, out.data());
    ctx_->Unmap(stagingA_.Get(), 0);
    hr = ctx_->Map(stagingB_.Get(), 0, D3D11_MAP_READ, 0, &m);
    if (FAILED(hr)) return Status::Error("readback", "Map: " + hrText(hr));
    copyRows(m, 0, w * bps, h / 2, out.data() + size_t(w) * h * bps);
    ctx_->Unmap(stagingB_.Get(), 0);
    return Status::Ok();
}

}  // namespace recon::d3d
