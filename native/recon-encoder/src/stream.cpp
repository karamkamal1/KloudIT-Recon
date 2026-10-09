#include "stream.hpp"

#include "d3d/convert.hpp"
#include "nvenc/nvenc_runtime.hpp"
#include "platform/platform.hpp"

namespace recon {

Status startStream(const StartParams& p, BackendChoice& choice, RingWriter& ring, Reporter& rep, const std::string& dumpNv12,
                   StartResult& out, Started& st) {
    if (!choice.backend) return Status::Error("unavailable", "no usable encoder backend: " + joinUnavailable(choice.caps));
    std::string capName = p.capture;
    if (capName.empty()) {
        capName = (p.window || !p.windowTitle.empty()) ? "wgc" : choice.caps.capture.empty() ? "dda" : choice.caps.capture.front();
    }
    Status s;
    std::unique_ptr<Capture> capture = createCapture(capName, s);
    if (capture) s = capture->init(p);
    if (!capture || !s.ok) {
        if (capture) capture->shutdown();
        return s;
    }
    const SourceInfo src = capture->source();
    if (src.device) st.gpuPriority = applyGpuPriority(p.gpuPriority, src.adapter);

    InputSpec in;
    choice.backend->limitFrameSize(ring.payloadCapacity());
    s = choice.backend->init(p, src, in, st);
    const bool encoderReady = s.ok;
    std::unique_ptr<d3d::Nv12Converter> conv;
    bool lumaStandsIn = false;
    if (s.ok && (in.format == InputSpec::Format::Nv12 || in.format == InputSpec::Format::P010)) {
        using Converter = d3d::Nv12Converter;
        const auto format = in.format == InputSpec::Format::P010 ? Converter::Format::P010 : Converter::Format::Nv12;
        const char* formatName = format == Converter::Format::P010 ? "P010" : "NV12";
        if (!src.device) {
            s = Status::Error("unsupported", std::string("the encoder wants ") + formatName + " but the capture has no GPU device");
        } else {
            auto mode = Converter::Output::Nv12;
            if (!Converter::renderTargets(src.device, format)) {
                if (in.planarOk) {
                    // A backend reading the frames on the CPU takes the planes
                    // (EncoderFrame::y / uv).
                    mode = Converter::Output::Planar;
                } else if (choice.caps.backend == "mock" || (choice.caps.backend == "nvenc" && nvencRuntime().testDouble)) {
                    // The mock and the NVENC test double read nothing (Wine has no
                    // NV12 render targets): still exercise the shaders; the luma
                    // plane stands in for the NV12 texture (Pipeline::captureLoop).
                    mode = Converter::Output::Planar;
                    lumaStandsIn = true;
                }
            }
            conv = std::make_unique<Converter>();
            s = conv->init(src.device, in.width, in.height, p.barcode, mode, 6, in.contentWidth, in.contentHeight, format);
            if (s.ok) {
                conv->setHdrDisplay(src.display.hdr);  // later: the capture's events (Pipeline::captureLoop)
                std::string padded;
                if (conv->contentWidth() != in.width || conv->contentHeight() != in.height) {
                    padded = " padded to " + std::to_string(in.width) + "x" + std::to_string(in.height);
                }
                logf(LogLevel::Info, "converting %ux%u -> %ux%u%s %s%s%s", src.width, src.height, conv->contentWidth(),
                     conv->contentHeight(), padded.c_str(),
                     mode == Converter::Output::Nv12 ? formatName : "Y + CbCr planes (no NV12 / P010 render targets)",
                     format == Converter::Format::P010 ? " (HDR10: BT.2020 PQ, 10-bit)" : "",
                     p.barcode.enabled ? " with barcode" : "");
            }
        }
    } else if (s.ok && p.barcode.enabled) {
        s = Status::Error("unsupported", "the barcode needs the GPU colour conversion (a GPU capture)");
    }
    if (!s.ok) {
        // The encoder goes first: it may be initialized on the capture's
        // AMFContext, which the capture's destructor terminates.
        if (encoderReady) choice.backend->release();
        conv.reset();
        capture->shutdown();
        return s;
    }
    st.capture = capName;
    st.captureWidth = int(src.width);
    st.captureHeight = int(src.height);
    if (src.adapter.found) {
        st.adapterLuid = src.adapter.luid;
        st.adapterName = src.adapter.name;
        st.vendor = src.adapter.vendor;
        st.hagsEnabled = src.adapter.hags;
        st.idleRepeatMs = p.idleRepeatMs;
    }
    st.barcode = conv && p.barcode.enabled;
    st.cursorInVideo = src.cursorInVideo;
    if (p.hdr && !st.hdr) {
        // Not an error (Sunshine streams SDR from an SDR display too): started.hdr says so.
        const std::string why = src.hdr                                ? "the encoder made SDR"
                                : src.display.known && src.display.hdr ? "capture " + capName + " delivers no HDR frames"
                                : src.display.known                    ? "Windows HDR is off for this output"
                                                                       : "no HDR information for this source";
        logf(LogLevel::Info, "hdr was asked for, but the stream is SDR: %s", why.c_str());
    }

    RateParams rate;
    rate.kbps = p.kbps;
    rate.vbvFrames = p.vbvFrames;
    rate.fps = p.fps;
    PipelineOptions po;
    po.converter = std::move(conv);
    po.lumaStandsIn = lumaStandsIn;
    po.dumpPath = dumpNv12;
    out.pipeline = std::make_unique<Pipeline>(*choice.backend, *capture, ring, rep, rate, std::move(po));
    out.capture = std::move(capture);
    return Status::Ok();
}

std::string joinUnavailable(const Caps& caps) {
    std::string out;
    for (const auto& [name, why] : caps.unavailable) {
        if (!out.empty()) out += "; ";
        out += name + ": " + why;
    }
    return out;
}

}  // namespace recon
