// GPU colour conversion for the encoders (GUIDE 3.2): one pixel-shader pass
// per plane turns the captured BGRA image into NV12 (BT.709, limited range,
// 4:2:0 with chroma sited like H.264/HEVC chroma_sample_loc_type 0: co-sited
// horizontally with the even luma column, between the two luma rows), rotated
// for rotated displays, bilinearly scaled to the encoded size, with the
// optional in-band frame barcode (GUIDE 0.2: the frame's sequence number and its
// CRC-8 in 8 x 3 cells, the format of internal/proto/barcode.go) drawn in the
// same pass.
// HDR10 streams (GUIDE 3.9) get P010 instead: the FP16 scRGB desktop of
// Windows HDR converted to BT.2020 primaries and the SMPTE ST 2084 (PQ)
// transfer, 10-bit limited range, same siting, scaling and barcode.
// Sources: 8-bit BGRA / RGBA (UNORM, TYPELESS, or fully typed sRGB: a game's
// sRGB swap chain, which can only be viewed as sRGB, so the sampler decodes
// it), FP16 scRGB and 10-bit R10G10B10A2 (sRGB-coded, or BT.2020 PQ while the
// captured output is in HDR mode: setHdrDisplay), as AMD Direct Capture can
// hand them out.
//
// Output textures come from a small pool. A converted frame stays reserved
// while any copy of its `hold` exists, so the AMF / NVENC backends keep it
// until the encoder has read the texture and the capture thread never writes
// into a frame that is still being encoded.
//
// The device is shared with the capture and the encoder's threads (AMF, NVENC,
// AMD Direct Capture call into it from their own threads), so a conversion runs
// with the device's critical section held (ID3D10Multithread::Enter / Leave:
// "used ... when there is a series of graphics commands that must happen in
// order"), and it sets or clears every pipeline stage its draws depend on
// instead of trusting state left on the shared immediate context. A failure on
// a removed device is the fatal device_lost.
//
// Shaders are compiled at run time with D3DCompile from d3dcompiler_47.dll
// (System32 on every Windows 10/11; loaded dynamically) rather than embedded
// as bytecode: this sandbox has no fxc, and the same source then builds with
// MSVC and mingw-w64 without a build-time shader step. Compiling costs a few
// milliseconds once per stream start.
#pragma once

#include <d3d10.h>  // ID3D10Multithread
#include <d3d11.h>
#include <wrl/client.h>

#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "types.hpp"

namespace recon::d3d {

using Microsoft::WRL::ComPtr;

// Affine maps from output-normalised (u, v) to source-normalised (s, t):
// s = xu[0]*u + xu[1]*v + xu[2], t = xv[0]*u + xv[1]*v + xv[2]. rotation is
// the clockwise rotation from the source texture to the displayed image
// (DXGI_OUTDUPL_DESC::Rotation), as Sunshine's convert shaders apply it.
void rotationTransform(int rotation, float xu[3], float xv[3]);

// Y, Cb, Cr = dot(rgb, c.xyz) + c.w in code units / the code maximum:
// BT.709 8-bit limited range (/ 255) for NV12, BT.2020 non-constant luminance
// 10-bit limited range (/ 1023) for P010.
struct YuvCoefficients {
    float y[4], u[4], v[4];
};
YuvCoefficients bt709Limited();
YuvCoefficients bt2020Limited10();

// HDR10 conversion: cd/m2 of 1.0 in an scRGB (FP16) source, by its
// definition (IEC 61966-2-2: 80 cd/m2 white); and where an 8-bit sRGB image
// goes into an HDR10 stream (the desktop left HDR mode during the stream),
// its white is placed at the HDR reference white of ITU-R BT.2408, 203 cd/m2.
constexpr double kScrgbWhiteNits = 80.0;
constexpr double kSdrWhiteNits = 203.0;

// IEEE 754 binary16 of f, rounded to nearest even: what an FP16 texture
// stores (test images: the self-test and the synthetic-gpu source's HDR mode).
uint16_t toHalf(float f);

// The frame barcode's CRC-8 of a value (polynomial 0x07, init 0, xorout 0x55,
// over the high byte, then the low byte: proto.BarcodeCRC) and the 24-bit word
// drawn for it (value << 8 | crc: proto.BarcodeWord).
uint8_t barcodeCrc(uint16_t value);
uint32_t barcodeWord(uint16_t value);
// The barcode bit drawn at output luma pixel (x, y) for a value: -1 = no
// cell there.
int barcodeBit(const BarcodeLayout& b, uint16_t value, uint32_t x, uint32_t y);
// Checks a barcode layout against the output size (empty = fits).
std::string barcodeProblem(const BarcodeLayout& b, uint32_t width, uint32_t height);

// One converted frame.
struct ConvertedFrame {
    ID3D11Texture2D* nv12 = nullptr;   // Output::Nv12: the NV12 (Format::P010: P010) texture
    ID3D11Texture2D* y = nullptr;      // Output::Planar: R8 (R16) luma plane
    ID3D11Texture2D* uv = nullptr;     // Output::Planar: R8G8 (R16G16) chroma plane (half size)
    std::shared_ptr<void> hold;        // keeps the pool texture reserved
    int index = -1;                    // pool index (stable per texture)
};

class Nv12Converter {
public:
    // Nv12: the encoder format, render target views on the NV12 planes (D3D11.1
    // plane views by format: R8_UNORM = luma, R8G8_UNORM = chroma). Planar:
    // separate R8 + R8G8 textures, for devices without NV12 render targets
    // (Wine's wined3d); the self-test uses it there, encoders cannot.
    enum class Output { Nv12, Planar };
    // The pixel format: Nv12 = 8-bit BT.709 (SDR); P010 = 10-bit BT.2020 PQ
    // (HDR10), the 10-bit code in the high bits of each 16-bit sample, the
    // low 6 bits zero (Output::Nv12 then means a DXGI_FORMAT_P010 texture with
    // R16 / R16G16 plane views, Planar R16 + R16G16 textures).
    enum class Format { Nv12, P010 };

    Nv12Converter() = default;
    Nv12Converter(const Nv12Converter&) = delete;
    Nv12Converter& operator=(const Nv12Converter&) = delete;
    ~Nv12Converter();

    static bool renderTargets(ID3D11Device* device, Format format);
    static bool nv12RenderTargets(ID3D11Device* device) { return renderTargets(device, Format::Nv12); }

    // width/height: output size, even. poolSize: textures at most (created on
    // demand). contentWidth/contentHeight (even, 0 = width/height): the image
    // is scaled into the top-left content rectangle only and its edge pixels
    // are repeated into the rest (padding to a coded size the encoder needs,
    // e.g. AV1 on RDNA3; the clamp sampler does the repeating). The barcode
    // must fit the content.
    Status init(ID3D11Device* device, uint32_t width, uint32_t height, const BarcodeLayout& barcode, Output output,
                int poolSize = 6, uint32_t contentWidth = 0, uint32_t contentHeight = 0, Format format = Format::Nv12);
    // Converts src (8-bit BGRA/RGBA or 10-bit RGB, sRGB; FP16 scRGB: clipped
    // to SDR for NV12, PQ for P010; an sRGB source in P010 is SDR at
    // kSdrWhiteNits; 10-bit BT.2020 PQ: as it is for P010, for NV12 with
    // kSdrWhiteNits as white) into a free pool texture, with the barcode of
    // barcodeValue (the frame's sequence number) when enabled. Error
    // "pool_exhausted" (non-fatal) when every pool texture is still reserved
    // by the encoder; "unsupported" for a texture format it cannot read.
    Status convert(ID3D11Texture2D* src, int rotation, uint16_t barcodeValue, ConvertedFrame& out);
    // Whether the captured output is in Windows HDR mode, which decides what a
    // 10-bit (R10G10B10A2) source holds: BT.2020 PQ (an HDR10 swap chain the
    // display scans out) when it is, else sRGB-coded R'G'B' like 8-bit
    // (VERIFY on hardware, docs/VENDOR_NOTES.md). Set before convert, on its
    // thread (the pipeline: at the start and on every capture event).
    void setHdrDisplay(bool hdr) { hdrDisplay_ = hdr; }
    // Copies a converted frame to the CPU as tightly packed NV12 (Y plane, then
    // interleaved CbCr), or P010 (the same with 16-bit little-endian samples).
    // Stalls until the GPU is done: tests and dumps only.
    Status readback(const ConvertedFrame& f, std::vector<uint8_t>& out);

    uint32_t width() const { return width_; }
    uint32_t height() const { return height_; }
    uint32_t contentWidth() const { return contentW_; }
    uint32_t contentHeight() const { return contentH_; }
    Output output() const { return output_; }
    Format format() const { return format_; }

private:
    struct Slot {
        ComPtr<ID3D11Texture2D> nv12, y, uv;
        ComPtr<ID3D11RenderTargetView> rtvY, rtvUV;
    };
    struct PoolState {
        std::mutex mu;
        std::vector<bool> busy;
    };
    struct SrvEntry {
        ComPtr<ID3D11Texture2D> texture;
        ComPtr<ID3D11ShaderResourceView> srv;
        ComPtr<ID3D11Texture2D> copy;  // when the source cannot be bound as a shader resource
        uint32_t kind = 0;             // how the shader reads it (convert.cpp SourceKind)
        float linearWhite = 0;         // a linear source: cd/m2 of 1.0
        bool tenBit = false;           // 10-bit RGB: BT.2020 PQ while hdrDisplay_
    };

    Status createSlot(Slot& s);
    Status sourceView(ID3D11Texture2D* src, SrvEntry*& out);

    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> ctx_;
    ComPtr<ID3D10Multithread> mt_;  // the device's critical section (null if not exposed)
    ComPtr<ID3D11VertexShader> vs_;
    ComPtr<ID3D11PixelShader> psY_, psUV_;
    ComPtr<ID3D11SamplerState> sampler_;
    ComPtr<ID3D11RasterizerState> rasterizer_;
    ComPtr<ID3D11Buffer> cb_;
    ComPtr<ID3D11Texture2D> stagingA_, stagingB_;
    std::vector<Slot> slots_;
    std::shared_ptr<PoolState> pool_;
    std::vector<SrvEntry> srvCache_;  // most recent first
    size_t poolSize_ = 0;
    uint32_t width_ = 0, height_ = 0;
    uint32_t contentW_ = 0, contentH_ = 0;
    BarcodeLayout barcode_;
    Output output_ = Output::Nv12;
    Format format_ = Format::Nv12;
    bool hdrDisplay_ = false;
    DXGI_FORMAT loggedFormat_ = DXGI_FORMAT_UNKNOWN;  // the source format last logged
};

}  // namespace recon::d3d
