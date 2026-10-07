// GPU colour conversion for the encoders (GUIDE 3.2): one pixel-shader pass
// per plane turns the captured BGRA image into NV12 (BT.709, limited range,
// 4:2:0 with chroma sited like H.264/HEVC chroma_sample_loc_type 0: co-sited
// horizontally with the even luma column, between the two luma rows), rotated
// for rotated displays, bilinearly scaled to the encoded size, with the
// optional in-band frame-id barcode (GUIDE 0.2) drawn in the same pass.
//
// Output textures come from a small pool. A converted frame stays reserved
// while any copy of its `hold` exists, so the AMF / NVENC backends keep it
// until the encoder has read the texture and the capture thread never writes
// into a frame that is still being encoded.
//
// Shaders are compiled at run time with D3DCompile from d3dcompiler_47.dll
// (System32 on every Windows 10/11; loaded dynamically) rather than embedded
// as bytecode: this sandbox has no fxc, and the same source then builds with
// MSVC and mingw-w64 without a build-time shader step. Compiling costs a few
// milliseconds once per stream start.
#pragma once

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

// Y, Cb, Cr = dot(rgb, c.xyz) + c.w in UNORM8 units / 255 (BT.709 limited range).
struct YuvCoefficients {
    float y[4], u[4], v[4];
};
YuvCoefficients bt709Limited();

// The barcode bit drawn at output luma pixel (x, y): -1 = no block there.
int barcodeBit(const BarcodeLayout& b, uint64_t value, uint32_t x, uint32_t y);
// Checks a barcode layout against the output size (empty = fits).
std::string barcodeProblem(const BarcodeLayout& b, uint32_t width, uint32_t height);

// One converted frame.
struct ConvertedFrame {
    ID3D11Texture2D* nv12 = nullptr;   // Output::Nv12
    ID3D11Texture2D* y = nullptr;      // Output::Planar: R8 luma plane
    ID3D11Texture2D* uv = nullptr;     // Output::Planar: R8G8 chroma plane (half size)
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

    Nv12Converter() = default;
    Nv12Converter(const Nv12Converter&) = delete;
    Nv12Converter& operator=(const Nv12Converter&) = delete;
    ~Nv12Converter();

    static bool nv12RenderTargets(ID3D11Device* device);

    // width/height: output size, even. poolSize: textures at most (created on demand).
    Status init(ID3D11Device* device, uint32_t width, uint32_t height, const BarcodeLayout& barcode, Output output,
                int poolSize = 6);
    // Converts src (8-bit BGRA/RGBA, or FP16 scRGB which is clipped to SDR)
    // into a free pool texture. Error "pool_exhausted" (non-fatal) when every
    // pool texture is still reserved by the encoder.
    Status convert(ID3D11Texture2D* src, int rotation, uint64_t barcodeValue, ConvertedFrame& out);
    // Copies a converted frame to the CPU as tightly packed NV12 (Y plane, then
    // interleaved CbCr). Stalls until the GPU is done: tests and dumps only.
    Status readback(const ConvertedFrame& f, std::vector<uint8_t>& out);

    uint32_t width() const { return width_; }
    uint32_t height() const { return height_; }
    Output output() const { return output_; }

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
        bool linear = false;
    };

    Status createSlot(Slot& s);
    Status sourceView(ID3D11Texture2D* src, SrvEntry*& out);

    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> ctx_;
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
    BarcodeLayout barcode_;
    Output output_ = Output::Nv12;
};

}  // namespace recon::d3d
