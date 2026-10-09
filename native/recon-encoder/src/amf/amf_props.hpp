// Per-codec AMF encoder property names and values, so the AMF backend is
// written once for H.264 (VideoEncoderVCE.h), HEVC (VideoEncoderHEVC.h) and
// AV1 (VideoEncoderAV1.h). nullptr = the codec has no such property. Names and
// enum values are taken verbatim from the vendored AMF v1.5.3 headers; the
// meaning of each is in AMF_Video_Encode_API.md / _HEVC_API.md / _AV1_API.md.
#pragma once

#include <AMF/components/ColorSpace.h>
#include <AMF/components/VideoEncoderAV1.h>
#include <AMF/components/VideoEncoderHEVC.h>
#include <AMF/components/VideoEncoderVCE.h>

#include "codec/bitstream.hpp"

namespace recon {

struct AmfCodecProps {
    Codec codec = Codec::Hevc;
    const wchar_t* component = nullptr;

    // Static (before Init). USAGE first: it "fully configures parameter set".
    const wchar_t* usage = nullptr;
    amf_int64 usageUltraLowLatency = 0, usageLowLatency = 0;
    const wchar_t* instanceIndex = nullptr;
    const wchar_t* frameSize = nullptr;
    const wchar_t* profile = nullptr;
    amf_int64 profileValue = 0;
    const wchar_t* lowLatencyMode = nullptr;       // H.264, HEVC
    const wchar_t* encodingLatencyMode = nullptr;  // AV1
    amf_int64 lowestLatency = 0;
    const wchar_t* qualityPreset = nullptr;
    amf_int64 presetSpeed = 0, presetBalanced = 0, presetQuality = 0;
    const wchar_t* rateControl = nullptr;
    amf_int64 rcCbr = 0, rcLatencyVbr = 0, rcPeakVbr = 0;
    const wchar_t* preAnalysis = nullptr;
    const wchar_t* preEncode = nullptr;
    bool preEncodeIsInt = false;                   // H.264: AMF_VIDEO_ENCODER_PREENCODE_MODE_ENUM, else bool
    const wchar_t* vbaq = nullptr;                 // H.264, HEVC
    const wchar_t* aqMode = nullptr;               // AV1
    amf_int64 aqCaq = 0;
    const wchar_t* gopSize = nullptr;              // HEVC/AV1 GOP_SIZE, H.264 IDR_PERIOD
    const wchar_t* gopsPerIdr = nullptr;           // HEVC
    const wchar_t* headerInsertion = nullptr;      // HEVC, AV1
    amf_int64 headerKeyAligned = 0;
    const wchar_t* bPicPattern = nullptr;
    const wchar_t* maxRefs = nullptr;
    int maxRefsLimit = 16;
    const wchar_t* maxLtr = nullptr;
    const wchar_t* ltrMode = nullptr;
    amf_int64 ltrKeepUnused = 0;
    const wchar_t* maxTemporalLayers = nullptr;
    const wchar_t* numTemporalLayers = nullptr;
    const wchar_t* queryTimeout = nullptr;
    const wchar_t* inputQueueSize = nullptr;
    const wchar_t* alignmentMode = nullptr;        // AV1
    const wchar_t* switchFrameMode = nullptr;      // AV1 SWITCH_FRAME_INSERTION_MODE
    amf_int64 switchFrameNone = 0;
    const wchar_t* screenContentTools = nullptr;   // AV1
    const wchar_t* paletteMode = nullptr;          // AV1 (dynamic; needs screenContentTools)
    const wchar_t* colorBitDepth = nullptr;
    const wchar_t* inputColorProfile = nullptr;
    const wchar_t* inputTransfer = nullptr;
    const wchar_t* inputPrimaries = nullptr;
    const wchar_t* outputColorProfile = nullptr;
    const wchar_t* outputTransfer = nullptr;
    const wchar_t* outputPrimaries = nullptr;
    const wchar_t* inputFullRange = nullptr;
    const wchar_t* outputFullRange = nullptr;
    // HDR10 (step 3.9): AMFBuffer of AMFHDRMetadata (HEVC, AV1; the H.264
    // encoder has the property too, but this backend makes no 10-bit H.264),
    // and the profile of a 10-bit stream (HEVC Main10; AV1 Main covers it).
    const wchar_t* inputHdrMetadata = nullptr;
    amf_int64 profile10Value = -1;
    // Sub-frame output (Phase 5 experiment): OUTPUT_MODE SLICE (H.264 / HEVC)
    // / TILE (AV1), the slices / tiles per frame, AV1 one tile per tile group
    // OBU (so each tile is a unit of its own).
    const wchar_t* outputMode = nullptr;
    amf_int64 outputModeParts = 0;
    const wchar_t* slicesPerFrame = nullptr;
    const wchar_t* tileGroupObu = nullptr;  // AV1

    // Dynamic (any time; applied before the next SubmitInput).
    const wchar_t* frameRate = nullptr;
    const wchar_t* targetBitrate = nullptr;
    const wchar_t* peakBitrate = nullptr;
    const wchar_t* vbvSize = nullptr;
    const wchar_t* enforceHrd = nullptr;
    const wchar_t* maxFrameSize = nullptr;  // in bits: H.264 / HEVC MAX_AU_SIZE, AV1 MAX_COMPRESSED_FRAME_SIZE
    const wchar_t* fillerData = nullptr;
    const wchar_t* skipFrame = nullptr;
    const wchar_t* intraRefreshPerSlot = nullptr;  // H.264 MBs / HEVC CTBs per slot
    uint32_t intraRefreshBlock = 0;                // their size: 16 / 64
    const wchar_t* intraRefreshMode = nullptr;     // AV1
    amf_int64 intraRefreshContinuous = 0, intraRefreshDisabled = 0;
    const wchar_t* intraRefreshStripes = nullptr;  // AV1

    // Per submission (properties of the input surface).
    const wchar_t* forcePictureType = nullptr;
    amf_int64 pictureNone = 0, pictureIdr = 0;
    const wchar_t* insertHeader = nullptr;         // HEVC INSERT_HEADER, AV1 FORCE_INSERT_SEQUENCE_HEADER
    const wchar_t* insertSps = nullptr;            // H.264
    const wchar_t* insertPps = nullptr;            // H.264
    const wchar_t* markLtr = nullptr;
    const wchar_t* forceLtrRef = nullptr;
    const wchar_t* roiData = nullptr;
    uint32_t roiBlock = 64;

    // Output buffer properties.
    const wchar_t* outputType = nullptr;
    amf_int64 outKey = 0, outIntra = 0;            // IDR / KEY; I / INTRA_ONLY
    amf_int64 outSwitch = -1;                      // AV1 SWITCH (clears the LTR slots); -1 = none (H.264 3 is B)
    const wchar_t* outputMarkedLtr = nullptr;
    const wchar_t* outputRefLtr = nullptr;
    const wchar_t* outputTemporalLayer = nullptr;  // H.264, HEVC
    const wchar_t* extradata = nullptr;
    const wchar_t* outputBufferType = nullptr;     // sub-frame output: FRAME / SLICE (TILE) / SLICE_LAST (TILE_LAST)
    amf_int64 bufferFrame = 0, bufferPart = 1, bufferLast = 2;

    // AMFCaps.
    const wchar_t* capMaxBitrate = nullptr;
    const wchar_t* capHwInstances = nullptr;
    const wchar_t* capMaxTemporalLayers = nullptr;
    const wchar_t* capRoi = nullptr;               // none for AV1 (OBS: "should always be supported")
    const wchar_t* capQueryTimeout = nullptr;      // none for AV1 (FFmpeg: set and read back)
    const wchar_t* capSliceOutput = nullptr;       // AV1: tile output
    const wchar_t* capMaxRefs = nullptr;
    const wchar_t* capMaxProfile = nullptr;
    amf_int64 profileMain10 = -1;                  // HEVC: a max profile >= this means 10-bit
    const wchar_t* capMaxLtr = nullptr;            // AV1
    const wchar_t* capAlignW = nullptr;            // AV1
    const wchar_t* capAlignH = nullptr;            // AV1
    int docMaxLtr = 0;                             // MAX_LTR_FRAMES range in the docs
};

inline const AmfCodecProps& amfH264Props() {
    static const AmfCodecProps p = [] {
        AmfCodecProps c;
        c.codec = Codec::H264;
        c.component = AMFVideoEncoderVCE_AVC;
        c.usage = AMF_VIDEO_ENCODER_USAGE;
        c.usageUltraLowLatency = AMF_VIDEO_ENCODER_USAGE_ULTRA_LOW_LATENCY;
        c.usageLowLatency = AMF_VIDEO_ENCODER_USAGE_LOW_LATENCY;
        c.instanceIndex = AMF_VIDEO_ENCODER_INSTANCE_INDEX;
        c.frameSize = AMF_VIDEO_ENCODER_FRAMESIZE;
        c.profile = AMF_VIDEO_ENCODER_PROFILE;
        c.profileValue = AMF_VIDEO_ENCODER_PROFILE_HIGH;
        c.lowLatencyMode = AMF_VIDEO_ENCODER_LOWLATENCY_MODE;
        c.qualityPreset = AMF_VIDEO_ENCODER_QUALITY_PRESET;
        c.presetSpeed = AMF_VIDEO_ENCODER_QUALITY_PRESET_SPEED;
        c.presetBalanced = AMF_VIDEO_ENCODER_QUALITY_PRESET_BALANCED;
        c.presetQuality = AMF_VIDEO_ENCODER_QUALITY_PRESET_QUALITY;
        c.rateControl = AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD;
        c.rcCbr = AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_CBR;
        c.rcLatencyVbr = AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_LATENCY_CONSTRAINED_VBR;
        c.rcPeakVbr = AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_PEAK_CONSTRAINED_VBR;
        c.preAnalysis = AMF_VIDEO_ENCODER_PRE_ANALYSIS_ENABLE;
        c.preEncode = AMF_VIDEO_ENCODER_PREENCODE_ENABLE;
        c.preEncodeIsInt = true;
        c.vbaq = AMF_VIDEO_ENCODER_ENABLE_VBAQ;
        c.gopSize = AMF_VIDEO_ENCODER_IDR_PERIOD;
        c.bPicPattern = AMF_VIDEO_ENCODER_B_PIC_PATTERN;
        c.maxRefs = AMF_VIDEO_ENCODER_MAX_NUM_REFRAMES;
        c.maxRefsLimit = 16;
        c.maxLtr = AMF_VIDEO_ENCODER_MAX_LTR_FRAMES;
        c.ltrMode = AMF_VIDEO_ENCODER_LTR_MODE;
        c.ltrKeepUnused = AMF_VIDEO_ENCODER_LTR_MODE_KEEP_UNUSED;
        c.maxTemporalLayers = AMF_VIDEO_ENCODER_MAX_NUM_TEMPORAL_LAYERS;
        c.numTemporalLayers = AMF_VIDEO_ENCODER_NUM_TEMPORAL_ENHANCMENT_LAYERS;
        c.queryTimeout = AMF_VIDEO_ENCODER_QUERY_TIMEOUT;
        c.inputQueueSize = AMF_VIDEO_ENCODER_INPUT_QUEUE_SIZE;
        c.colorBitDepth = AMF_VIDEO_ENCODER_COLOR_BIT_DEPTH;
        c.inputColorProfile = AMF_VIDEO_ENCODER_INPUT_COLOR_PROFILE;
        c.inputTransfer = AMF_VIDEO_ENCODER_INPUT_TRANSFER_CHARACTERISTIC;
        c.inputPrimaries = AMF_VIDEO_ENCODER_INPUT_COLOR_PRIMARIES;
        c.outputColorProfile = AMF_VIDEO_ENCODER_OUTPUT_COLOR_PROFILE;
        c.outputTransfer = AMF_VIDEO_ENCODER_OUTPUT_TRANSFER_CHARACTERISTIC;
        c.outputPrimaries = AMF_VIDEO_ENCODER_OUTPUT_COLOR_PRIMARIES;
        c.inputFullRange = AMF_VIDEO_ENCODER_INPUT_FULL_RANGE_COLOR;
        c.outputFullRange = AMF_VIDEO_ENCODER_OUTPUT_FULL_RANGE_COLOR;
        c.frameRate = AMF_VIDEO_ENCODER_FRAMERATE;
        c.targetBitrate = AMF_VIDEO_ENCODER_TARGET_BITRATE;
        c.peakBitrate = AMF_VIDEO_ENCODER_PEAK_BITRATE;
        c.vbvSize = AMF_VIDEO_ENCODER_VBV_BUFFER_SIZE;
        c.enforceHrd = AMF_VIDEO_ENCODER_ENFORCE_HRD;
        c.maxFrameSize = AMF_VIDEO_ENCODER_MAX_AU_SIZE;
        c.fillerData = AMF_VIDEO_ENCODER_FILLER_DATA_ENABLE;
        c.skipFrame = AMF_VIDEO_ENCODER_RATE_CONTROL_SKIP_FRAME_ENABLE;
        c.intraRefreshPerSlot = AMF_VIDEO_ENCODER_INTRA_REFRESH_NUM_MBS_PER_SLOT;
        c.intraRefreshBlock = 16;
        c.forcePictureType = AMF_VIDEO_ENCODER_FORCE_PICTURE_TYPE;
        c.pictureNone = AMF_VIDEO_ENCODER_PICTURE_TYPE_NONE;
        c.pictureIdr = AMF_VIDEO_ENCODER_PICTURE_TYPE_IDR;
        c.insertSps = AMF_VIDEO_ENCODER_INSERT_SPS;
        c.insertPps = AMF_VIDEO_ENCODER_INSERT_PPS;
        c.markLtr = AMF_VIDEO_ENCODER_MARK_CURRENT_WITH_LTR_INDEX;
        c.forceLtrRef = AMF_VIDEO_ENCODER_FORCE_LTR_REFERENCE_BITFIELD;
        c.roiData = AMF_VIDEO_ENCODER_ROI_DATA;
        c.roiBlock = 16;  // "Importance value for each 16x16 macro block"
        c.outputType = AMF_VIDEO_ENCODER_OUTPUT_DATA_TYPE;
        c.outKey = AMF_VIDEO_ENCODER_OUTPUT_DATA_TYPE_IDR;
        c.outIntra = AMF_VIDEO_ENCODER_OUTPUT_DATA_TYPE_I;
        c.outputMarkedLtr = AMF_VIDEO_ENCODER_OUTPUT_MARKED_LTR_INDEX;
        c.outputRefLtr = AMF_VIDEO_ENCODER_OUTPUT_REFERENCED_LTR_INDEX_BITFIELD;
        c.outputTemporalLayer = AMF_VIDEO_ENCODER_OUTPUT_TEMPORAL_LAYER;
        c.extradata = AMF_VIDEO_ENCODER_EXTRADATA;
        c.outputMode = AMF_VIDEO_ENCODER_OUTPUT_MODE;
        c.outputModeParts = AMF_VIDEO_ENCODER_OUTPUT_MODE_SLICE;
        c.slicesPerFrame = AMF_VIDEO_ENCODER_SLICES_PER_FRAME;
        c.outputBufferType = AMF_VIDEO_ENCODER_OUTPUT_BUFFER_TYPE;
        c.bufferFrame = AMF_VIDEO_ENCODER_OUTPUT_BUFFER_TYPE_FRAME;
        c.bufferPart = AMF_VIDEO_ENCODER_OUTPUT_BUFFER_TYPE_SLICE;
        c.bufferLast = AMF_VIDEO_ENCODER_OUTPUT_BUFFER_TYPE_SLICE_LAST;
        c.capMaxBitrate = AMF_VIDEO_ENCODER_CAP_MAX_BITRATE;
        c.capHwInstances = AMF_VIDEO_ENCODER_CAP_NUM_OF_HW_INSTANCES;
        c.capMaxTemporalLayers = AMF_VIDEO_ENCODER_CAP_MAX_TEMPORAL_LAYERS;
        c.capRoi = AMF_VIDEO_ENCODER_CAP_ROI;
        c.capQueryTimeout = AMF_VIDEO_ENCODER_CAP_QUERY_TIMEOUT_SUPPORT;
        c.capSliceOutput = AMF_VIDEO_ENCODER_CAP_SUPPORT_SLICE_OUTPUT;
        c.capMaxRefs = AMF_VIDEO_ENCODER_CAP_MAX_REFERENCE_FRAMES;
        c.capMaxProfile = AMF_VIDEO_ENCODER_CAP_MAX_PROFILE;
        c.docMaxLtr = 2;  // MAX_LTR_FRAMES "0 … 2"
        return c;
    }();
    return p;
}

inline const AmfCodecProps& amfHevcProps() {
    static const AmfCodecProps p = [] {
        AmfCodecProps c;
        c.codec = Codec::Hevc;
        c.component = AMFVideoEncoder_HEVC;
        c.usage = AMF_VIDEO_ENCODER_HEVC_USAGE;
        c.usageUltraLowLatency = AMF_VIDEO_ENCODER_HEVC_USAGE_ULTRA_LOW_LATENCY;
        c.usageLowLatency = AMF_VIDEO_ENCODER_HEVC_USAGE_LOW_LATENCY;
        c.instanceIndex = AMF_VIDEO_ENCODER_HEVC_INSTANCE_INDEX;
        c.frameSize = AMF_VIDEO_ENCODER_HEVC_FRAMESIZE;
        c.profile = AMF_VIDEO_ENCODER_HEVC_PROFILE;
        c.profileValue = AMF_VIDEO_ENCODER_HEVC_PROFILE_MAIN;
        c.lowLatencyMode = AMF_VIDEO_ENCODER_HEVC_LOWLATENCY_MODE;
        c.qualityPreset = AMF_VIDEO_ENCODER_HEVC_QUALITY_PRESET;
        c.presetSpeed = AMF_VIDEO_ENCODER_HEVC_QUALITY_PRESET_SPEED;
        c.presetBalanced = AMF_VIDEO_ENCODER_HEVC_QUALITY_PRESET_BALANCED;
        c.presetQuality = AMF_VIDEO_ENCODER_HEVC_QUALITY_PRESET_QUALITY;
        c.rateControl = AMF_VIDEO_ENCODER_HEVC_RATE_CONTROL_METHOD;
        c.rcCbr = AMF_VIDEO_ENCODER_HEVC_RATE_CONTROL_METHOD_CBR;
        c.rcLatencyVbr = AMF_VIDEO_ENCODER_HEVC_RATE_CONTROL_METHOD_LATENCY_CONSTRAINED_VBR;
        c.rcPeakVbr = AMF_VIDEO_ENCODER_HEVC_RATE_CONTROL_METHOD_PEAK_CONSTRAINED_VBR;
        c.preAnalysis = AMF_VIDEO_ENCODER_HEVC_PRE_ANALYSIS_ENABLE;
        c.preEncode = AMF_VIDEO_ENCODER_HEVC_PREENCODE_ENABLE;
        c.vbaq = AMF_VIDEO_ENCODER_HEVC_ENABLE_VBAQ;
        c.gopSize = AMF_VIDEO_ENCODER_HEVC_GOP_SIZE;
        c.gopsPerIdr = AMF_VIDEO_ENCODER_HEVC_NUM_GOPS_PER_IDR;
        c.headerInsertion = AMF_VIDEO_ENCODER_HEVC_HEADER_INSERTION_MODE;
        c.headerKeyAligned = AMF_VIDEO_ENCODER_HEVC_HEADER_INSERTION_MODE_IDR_ALIGNED;
        c.maxRefs = AMF_VIDEO_ENCODER_HEVC_MAX_NUM_REFRAMES;
        c.maxRefsLimit = 16;
        c.maxLtr = AMF_VIDEO_ENCODER_HEVC_MAX_LTR_FRAMES;
        c.ltrMode = AMF_VIDEO_ENCODER_HEVC_LTR_MODE;
        c.ltrKeepUnused = AMF_VIDEO_ENCODER_HEVC_LTR_MODE_KEEP_UNUSED;
        c.maxTemporalLayers = AMF_VIDEO_ENCODER_HEVC_MAX_NUM_TEMPORAL_LAYERS;
        c.numTemporalLayers = AMF_VIDEO_ENCODER_HEVC_NUM_TEMPORAL_LAYERS;
        c.queryTimeout = AMF_VIDEO_ENCODER_HEVC_QUERY_TIMEOUT;
        c.inputQueueSize = AMF_VIDEO_ENCODER_HEVC_INPUT_QUEUE_SIZE;
        c.colorBitDepth = AMF_VIDEO_ENCODER_HEVC_COLOR_BIT_DEPTH;
        c.inputColorProfile = AMF_VIDEO_ENCODER_HEVC_INPUT_COLOR_PROFILE;
        c.inputTransfer = AMF_VIDEO_ENCODER_HEVC_INPUT_TRANSFER_CHARACTERISTIC;
        c.inputPrimaries = AMF_VIDEO_ENCODER_HEVC_INPUT_COLOR_PRIMARIES;
        c.outputColorProfile = AMF_VIDEO_ENCODER_HEVC_OUTPUT_COLOR_PROFILE;
        c.outputTransfer = AMF_VIDEO_ENCODER_HEVC_OUTPUT_TRANSFER_CHARACTERISTIC;
        c.outputPrimaries = AMF_VIDEO_ENCODER_HEVC_OUTPUT_COLOR_PRIMARIES;
        c.inputFullRange = AMF_VIDEO_ENCODER_HEVC_INPUT_FULL_RANGE_COLOR;
        c.outputFullRange = AMF_VIDEO_ENCODER_HEVC_OUTPUT_FULL_RANGE_COLOR;
        c.inputHdrMetadata = AMF_VIDEO_ENCODER_HEVC_INPUT_HDR_METADATA;
        c.profile10Value = AMF_VIDEO_ENCODER_HEVC_PROFILE_MAIN_10;
        c.frameRate = AMF_VIDEO_ENCODER_HEVC_FRAMERATE;
        c.targetBitrate = AMF_VIDEO_ENCODER_HEVC_TARGET_BITRATE;
        c.peakBitrate = AMF_VIDEO_ENCODER_HEVC_PEAK_BITRATE;
        c.vbvSize = AMF_VIDEO_ENCODER_HEVC_VBV_BUFFER_SIZE;
        c.enforceHrd = AMF_VIDEO_ENCODER_HEVC_ENFORCE_HRD;
        c.maxFrameSize = AMF_VIDEO_ENCODER_HEVC_MAX_AU_SIZE;
        c.fillerData = AMF_VIDEO_ENCODER_HEVC_FILLER_DATA_ENABLE;
        c.skipFrame = AMF_VIDEO_ENCODER_HEVC_RATE_CONTROL_SKIP_FRAME_ENABLE;
        c.intraRefreshPerSlot = AMF_VIDEO_ENCODER_HEVC_INTRA_REFRESH_NUM_CTBS_PER_SLOT;
        c.intraRefreshBlock = 64;
        c.forcePictureType = AMF_VIDEO_ENCODER_HEVC_FORCE_PICTURE_TYPE;
        c.pictureNone = AMF_VIDEO_ENCODER_HEVC_PICTURE_TYPE_NONE;
        c.pictureIdr = AMF_VIDEO_ENCODER_HEVC_PICTURE_TYPE_IDR;
        c.insertHeader = AMF_VIDEO_ENCODER_HEVC_INSERT_HEADER;
        c.markLtr = AMF_VIDEO_ENCODER_HEVC_MARK_CURRENT_WITH_LTR_INDEX;
        c.forceLtrRef = AMF_VIDEO_ENCODER_HEVC_FORCE_LTR_REFERENCE_BITFIELD;
        c.roiData = AMF_VIDEO_ENCODER_HEVC_ROI_DATA;
        c.roiBlock = 64;  // "each 64x64 CTB"
        c.outputType = AMF_VIDEO_ENCODER_HEVC_OUTPUT_DATA_TYPE;
        c.outKey = AMF_VIDEO_ENCODER_HEVC_OUTPUT_DATA_TYPE_IDR;
        c.outIntra = AMF_VIDEO_ENCODER_HEVC_OUTPUT_DATA_TYPE_I;
        c.outputMarkedLtr = AMF_VIDEO_ENCODER_HEVC_OUTPUT_MARKED_LTR_INDEX;
        c.outputRefLtr = AMF_VIDEO_ENCODER_HEVC_OUTPUT_REFERENCED_LTR_INDEX_BITFIELD;
        c.outputTemporalLayer = AMF_VIDEO_ENCODER_HEVC_OUTPUT_TEMPORAL_LAYER;
        c.extradata = AMF_VIDEO_ENCODER_HEVC_EXTRADATA;
        c.outputMode = AMF_VIDEO_ENCODER_HEVC_OUTPUT_MODE;
        c.outputModeParts = AMF_VIDEO_ENCODER_HEVC_OUTPUT_MODE_SLICE;
        c.slicesPerFrame = AMF_VIDEO_ENCODER_HEVC_SLICES_PER_FRAME;
        c.outputBufferType = AMF_VIDEO_ENCODER_HEVC_OUTPUT_BUFFER_TYPE;
        c.bufferFrame = AMF_VIDEO_ENCODER_HEVC_OUTPUT_BUFFER_TYPE_FRAME;
        c.bufferPart = AMF_VIDEO_ENCODER_HEVC_OUTPUT_BUFFER_TYPE_SLICE;
        c.bufferLast = AMF_VIDEO_ENCODER_HEVC_OUTPUT_BUFFER_TYPE_SLICE_LAST;
        c.capMaxBitrate = AMF_VIDEO_ENCODER_HEVC_CAP_MAX_BITRATE;
        c.capHwInstances = AMF_VIDEO_ENCODER_HEVC_CAP_NUM_OF_HW_INSTANCES;
        c.capMaxTemporalLayers = AMF_VIDEO_ENCODER_HEVC_CAP_MAX_TEMPORAL_LAYERS;
        c.capRoi = AMF_VIDEO_ENCODER_HEVC_CAP_ROI;
        c.capQueryTimeout = AMF_VIDEO_ENCODER_HEVC_CAP_QUERY_TIMEOUT_SUPPORT;
        c.capSliceOutput = AMF_VIDEO_ENCODER_HEVC_CAP_SUPPORT_SLICE_OUTPUT;
        c.capMaxRefs = AMF_VIDEO_ENCODER_HEVC_CAP_MAX_REFERENCE_FRAMES;
        c.capMaxProfile = AMF_VIDEO_ENCODER_HEVC_CAP_MAX_PROFILE;
        c.profileMain10 = AMF_VIDEO_ENCODER_HEVC_PROFILE_MAIN_10;
        c.docMaxLtr = 16;  // MAX_LTR_FRAMES "0 … 16" (shared with SVC; level / DPB limits apply)
        return c;
    }();
    return p;
}

inline const AmfCodecProps& amfAv1Props() {
    static const AmfCodecProps p = [] {
        AmfCodecProps c;
        c.codec = Codec::Av1;
        c.component = AMFVideoEncoder_AV1;
        c.usage = AMF_VIDEO_ENCODER_AV1_USAGE;
        c.usageUltraLowLatency = AMF_VIDEO_ENCODER_AV1_USAGE_ULTRA_LOW_LATENCY;
        c.usageLowLatency = AMF_VIDEO_ENCODER_AV1_USAGE_LOW_LATENCY;
        c.instanceIndex = AMF_VIDEO_ENCODER_AV1_ENCODER_INSTANCE_INDEX;
        c.frameSize = AMF_VIDEO_ENCODER_AV1_FRAMESIZE;
        c.profile = AMF_VIDEO_ENCODER_AV1_PROFILE;
        c.profileValue = AMF_VIDEO_ENCODER_AV1_PROFILE_MAIN;
        c.encodingLatencyMode = AMF_VIDEO_ENCODER_AV1_ENCODING_LATENCY_MODE;
        c.lowestLatency = AMF_VIDEO_ENCODER_AV1_ENCODING_LATENCY_MODE_LOWEST_LATENCY;
        c.qualityPreset = AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET;
        c.presetSpeed = AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET_SPEED;
        c.presetBalanced = AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET_BALANCED;
        c.presetQuality = AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET_QUALITY;
        c.rateControl = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD;
        c.rcCbr = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD_CBR;
        c.rcLatencyVbr = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD_LATENCY_CONSTRAINED_VBR;
        c.rcPeakVbr = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD_PEAK_CONSTRAINED_VBR;
        c.preAnalysis = AMF_VIDEO_ENCODER_AV1_PRE_ANALYSIS_ENABLE;
        c.preEncode = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_PREENCODE;
        c.aqMode = AMF_VIDEO_ENCODER_AV1_AQ_MODE;
        c.aqCaq = AMF_VIDEO_ENCODER_AV1_AQ_MODE_CAQ;
        c.gopSize = AMF_VIDEO_ENCODER_AV1_GOP_SIZE;
        c.headerInsertion = AMF_VIDEO_ENCODER_AV1_HEADER_INSERTION_MODE;
        c.headerKeyAligned = AMF_VIDEO_ENCODER_AV1_HEADER_INSERTION_MODE_KEY_FRAME_ALIGNED;
        c.bPicPattern = AMF_VIDEO_ENCODER_AV1_B_PIC_PATTERN;
        c.maxRefs = AMF_VIDEO_ENCODER_AV1_MAX_NUM_REFRAMES;
        c.maxRefsLimit = 8;  // "In AV1, maximum of 8 reference frames are supported"
        c.maxLtr = AMF_VIDEO_ENCODER_AV1_MAX_LTR_FRAMES;
        c.ltrMode = AMF_VIDEO_ENCODER_AV1_LTR_MODE;
        c.ltrKeepUnused = AMF_VIDEO_ENCODER_AV1_LTR_MODE_KEEP_UNUSED;
        c.maxTemporalLayers = AMF_VIDEO_ENCODER_AV1_MAX_NUM_TEMPORAL_LAYERS;
        c.numTemporalLayers = AMF_VIDEO_ENCODER_AV1_NUM_TEMPORAL_LAYERS;
        c.queryTimeout = AMF_VIDEO_ENCODER_AV1_QUERY_TIMEOUT;
        c.inputQueueSize = AMF_VIDEO_ENCODER_AV1_INPUT_QUEUE_SIZE;
        c.alignmentMode = AMF_VIDEO_ENCODER_AV1_ALIGNMENT_MODE;
        c.switchFrameMode = AMF_VIDEO_ENCODER_AV1_SWITCH_FRAME_INSERTION_MODE;
        c.switchFrameNone = AMF_VIDEO_ENCODER_AV1_SWITCH_FRAME_INSERTION_MODE_NONE;
        c.screenContentTools = AMF_VIDEO_ENCODER_AV1_SCREEN_CONTENT_TOOLS;
        c.paletteMode = AMF_VIDEO_ENCODER_AV1_PALETTE_MODE;
        c.colorBitDepth = AMF_VIDEO_ENCODER_AV1_COLOR_BIT_DEPTH;
        c.inputColorProfile = AMF_VIDEO_ENCODER_AV1_INPUT_COLOR_PROFILE;
        c.inputTransfer = AMF_VIDEO_ENCODER_AV1_INPUT_TRANSFER_CHARACTERISTIC;
        c.inputPrimaries = AMF_VIDEO_ENCODER_AV1_INPUT_COLOR_PRIMARIES;
        c.outputColorProfile = AMF_VIDEO_ENCODER_AV1_OUTPUT_COLOR_PROFILE;
        c.outputTransfer = AMF_VIDEO_ENCODER_AV1_OUTPUT_TRANSFER_CHARACTERISTIC;
        c.outputPrimaries = AMF_VIDEO_ENCODER_AV1_OUTPUT_COLOR_PRIMARIES;
        c.inputFullRange = AMF_VIDEO_ENCODER_AV1_INPUT_FULL_RANGE_COLOR;
        c.outputFullRange = AMF_VIDEO_ENCODER_AV1_OUTPUT_FULL_RANGE_COLOR;
        c.inputHdrMetadata = AMF_VIDEO_ENCODER_AV1_INPUT_HDR_METADATA;
        c.profile10Value = AMF_VIDEO_ENCODER_AV1_PROFILE_MAIN;  // "Main": 8 and 10 bit 4:2:0
        c.frameRate = AMF_VIDEO_ENCODER_AV1_FRAMERATE;
        c.targetBitrate = AMF_VIDEO_ENCODER_AV1_TARGET_BITRATE;
        c.peakBitrate = AMF_VIDEO_ENCODER_AV1_PEAK_BITRATE;
        c.vbvSize = AMF_VIDEO_ENCODER_AV1_VBV_BUFFER_SIZE;
        c.enforceHrd = AMF_VIDEO_ENCODER_AV1_ENFORCE_HRD;
        c.maxFrameSize = AMF_VIDEO_ENCODER_AV1_MAX_COMPRESSED_FRAME_SIZE;
        c.fillerData = AMF_VIDEO_ENCODER_AV1_FILLER_DATA;
        c.skipFrame = AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_SKIP_FRAME;
        c.intraRefreshMode = AMF_VIDEO_ENCODER_AV1_INTRA_REFRESH_MODE;
        c.intraRefreshContinuous = AMF_VIDEO_ENCODER_AV1_INTRA_REFRESH_MODE__CONTINUOUS;
        c.intraRefreshDisabled = AMF_VIDEO_ENCODER_AV1_INTRA_REFRESH_MODE__DISABLED;
        c.intraRefreshStripes = AMF_VIDEO_ENCODER_AV1_INTRAREFRESH_STRIPES;
        c.forcePictureType = AMF_VIDEO_ENCODER_AV1_FORCE_FRAME_TYPE;
        c.pictureNone = AMF_VIDEO_ENCODER_AV1_FORCE_FRAME_TYPE_NONE;
        c.pictureIdr = AMF_VIDEO_ENCODER_AV1_FORCE_FRAME_TYPE_KEY;
        c.insertHeader = AMF_VIDEO_ENCODER_AV1_FORCE_INSERT_SEQUENCE_HEADER;
        c.markLtr = AMF_VIDEO_ENCODER_AV1_MARK_CURRENT_WITH_LTR_INDEX;
        c.forceLtrRef = AMF_VIDEO_ENCODER_AV1_FORCE_LTR_REFERENCE_BITFIELD;
        c.roiData = AMF_VIDEO_ENCODER_AV1_ROI_DATA;
        c.roiBlock = 64;  // "each 64x64 SB"
        c.outputType = AMF_VIDEO_ENCODER_AV1_OUTPUT_FRAME_TYPE;
        c.outKey = AMF_VIDEO_ENCODER_AV1_OUTPUT_FRAME_TYPE_KEY;
        c.outIntra = AMF_VIDEO_ENCODER_AV1_OUTPUT_FRAME_TYPE_INTRA_ONLY;
        c.outSwitch = AMF_VIDEO_ENCODER_AV1_OUTPUT_FRAME_TYPE_SWITCH;
        c.outputMarkedLtr = AMF_VIDEO_ENCODER_AV1_OUTPUT_MARKED_LTR_INDEX;
        c.outputRefLtr = AMF_VIDEO_ENCODER_AV1_OUTPUT_REFERENCED_LTR_INDEX_BITFIELD;
        c.extradata = AMF_VIDEO_ENCODER_AV1_EXTRA_DATA;
        c.outputMode = AMF_VIDEO_ENCODER_AV1_OUTPUT_MODE;
        c.outputModeParts = AMF_VIDEO_ENCODER_AV1_OUTPUT_MODE_TILE;
        c.slicesPerFrame = AMF_VIDEO_ENCODER_AV1_TILES_PER_FRAME;
        c.tileGroupObu = AMF_VIDEO_ENCODER_AV1_TILE_GROUP_OBU;
        c.outputBufferType = AMF_VIDEO_ENCODER_AV1_OUTPUT_BUFFER_TYPE;
        c.bufferFrame = AMF_VIDEO_ENCODER_AV1_OUTPUT_BUFFER_TYPE_FRAME;
        c.bufferPart = AMF_VIDEO_ENCODER_AV1_OUTPUT_BUFFER_TYPE_TILE;
        c.bufferLast = AMF_VIDEO_ENCODER_AV1_OUTPUT_BUFFER_TYPE_TILE_LAST;
        c.capMaxBitrate = AMF_VIDEO_ENCODER_AV1_CAP_MAX_BITRATE;
        c.capHwInstances = AMF_VIDEO_ENCODER_AV1_CAP_NUM_OF_HW_INSTANCES;
        c.capMaxTemporalLayers = AMF_VIDEO_ENCODER_AV1_CAP_MAX_NUM_TEMPORAL_LAYERS;
        c.capSliceOutput = AMF_VIDEO_ENCODER_AV1_CAP_SUPPORT_TILE_OUTPUT;
        c.capMaxProfile = AMF_VIDEO_ENCODER_AV1_CAP_MAX_PROFILE;
        c.capMaxLtr = AMF_VIDEO_ENCODER_AV1_CAP_MAX_NUM_LTR_FRAMES;
        c.capAlignW = AMF_VIDEO_ENCODER_AV1_CAP_WIDTH_ALIGNMENT_FACTOR;
        c.capAlignH = AMF_VIDEO_ENCODER_AV1_CAP_HEIGHT_ALIGNMENT_FACTOR;
        c.docMaxLtr = 8;  // MAX_LTR_FRAMES "0 … 8"
        return c;
    }();
    return p;
}

inline const AmfCodecProps& amfProps(Codec c) {
    switch (c) {
    case Codec::H264: return amfH264Props();
    case Codec::Av1: return amfAv1Props();
    case Codec::Hevc: break;
    }
    return amfHevcProps();
}

}  // namespace recon
