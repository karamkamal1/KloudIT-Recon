// Package encoder runs recon-encoder.exe, the native capture + encode helper
// (GUIDE Phase 3), and exposes its control protocol and shared-memory frame
// ring to Go. docs/HELPER_PROTOCOL.md specifies both; native/recon-encoder is
// the other side.
//
// recon-host starts one helper per streaming session (Launch), reads its
// capabilities (Caps), starts the stream (Start), steers it at run time
// (ForceIDR, Recover, SetRate, SetROI) and reads encoded frames (Frames).
// Restarting a helper that exited or reported a fatal error is the caller's
// job: Launch a new one and Start it (its first frame is an IDR).
package encoder

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the control protocol and ring layout version this package
// speaks; the helper reports its own in Caps.V.
const ProtocolVersion = 1

// MaxControlMsg bounds every control message in both directions.
const MaxControlMsg = 1 << 20

// StartParams configures the stream ("start" message).
type StartParams struct {
	Capture   string  `json:"capture,omitempty"` // dda | amd-direct | wgc | synthetic; "" = backend default (wgc with a window)
	Monitor   int     `json:"monitor"`           // DXGI output index (see HMonitor, AdapterLUID)
	Codec     string  `json:"codec"`             // h264 | hevc | av1
	Width     int     `json:"width,omitempty"`   // 0 = capture size
	Height    int     `json:"height,omitempty"`
	FPS       int     `json:"fps"`
	Kbps      int     `json:"kbps"`
	VBVFrames float64 `json:"vbvFrames,omitempty"` // VBV buffer in frame intervals (default 1)
	RC        string  `json:"rc,omitempty"`        // cbr (default) | vbr
	Quality   string  `json:"quality,omitempty"`   // speed (default) | balanced | quality
	// HDR asks for HDR10 (opt-in, GUIDE 3.9): when the captured output is in
	// Windows HDR mode (Output.HDR) the stream is 10-bit BT.2020 PQ with HDR
	// metadata (Started.HDR); an SDR output still gives an SDR stream. Only
	// with a codec whose CodecCaps.HDR10 is true (hevc, av1), else Start fails
	// with "unsupported".
	HDR       bool `json:"hdr,omitempty"`
	LTRSlots  int  `json:"ltrSlots,omitempty"`  // long-term reference slots (ACK-based recovery)
	SVCLayers int  `json:"svcLayers,omitempty"` // temporal layers (default 1)

	// Monitor selection, in this order: HMonitor; AdapterLUID + Monitor (output
	// index on that adapter); Monitor alone (output index on DXGI adapter 0,
	// like ddagrab's output_idx). Caps.Outputs lists the outputs.
	HMonitor    uint64 `json:"hmonitor,omitempty"`
	AdapterLUID string `json:"adapterLuid,omitempty"` // "%08x:%08x" as in Caps
	// Window capture (wgc only): a top-level window handle, or the first
	// visible window whose title contains WindowTitle (case-insensitive).
	Window       uint64   `json:"window,omitempty"`
	WindowTitle  string   `json:"windowTitle,omitempty"`
	GPUPriority  string   `json:"gpuPriority,omitempty"`  // auto (default) | high | realtime | off
	IdleRepeatMs int      `json:"idleRepeatMs,omitempty"` // repeat the last image after this long without a new one (default 100)
	Barcode      *Barcode `json:"barcode,omitempty"`      // in-band frame-id barcode, nil = off

	// Encoder knobs (optional; zero values keep the backend's defaults).
	LiveBitrate        string `json:"liveBitrate,omitempty"`        // "" = the codec's Caps LiveBitrate | seamless | flush
	EncoderInstance    *int   `json:"encoderInstance,omitempty"`    // hardware encoder engine (AMF INSTANCE_INDEX); nil = default
	LTRInterval        int    `json:"ltrInterval,omitempty"`        // frames between LTR marks; 0 = fps/10
	IntraRefreshFrames int    `json:"intraRefreshFrames,omitempty"` // intra refresh cycle in frames; 0 = off (not with LTRSlots)
	ZeroCopy           *bool  `json:"zeroCopy,omitempty"`           // AMD Direct Capture surfaces straight into AMF when possible; nil = true
}

// Barcode places the frame id as a block barcode into every encoded frame
// (GUIDE 0.2): block k shows bit k of the id (bits-1-k with MSBFirst), luma
// 235 for 1 and 16 for 0 with neutral chroma, Cols blocks per row, in output
// pixels. X, Y, BlockW and BlockH must be even.
type Barcode struct {
	X        int  `json:"x"`
	Y        int  `json:"y"`
	BlockW   int  `json:"blockW"`
	BlockH   int  `json:"blockH"`
	Cols     int  `json:"cols"`
	Bits     int  `json:"bits"` // low bits of the frame id, 1..64
	MSBFirst bool `json:"msbFirst"`
}

// MaxROIRects is the most regions of interest one SetROI may carry.
const MaxROIRects = 256

// ROIRect is one region of interest for SetROI.
type ROIRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	W      int `json:"w"`
	H      int `json:"h"`
	Weight int `json:"weight"` // -10..10, scaled by the backend
}

// Caps is the helper's first message (GUIDE Arch-2 plus diagnostics).
type Caps struct {
	V             int                  `json:"v"`
	HelperVersion string               `json:"helperVersion"`
	Backend       string               `json:"backend"` // amf | nvenc | mock | none
	Vendor        string               `json:"vendor"`  // amd | nvidia | intel | other | mock
	AdapterLUID   string               `json:"adapterLuid"`
	AdapterName   string               `json:"adapterName"`
	HAGSEnabled   *bool                `json:"hagsEnabled"` // hardware GPU scheduling on that adapter; nil = not detected
	Codecs        map[string]CodecCaps `json:"codecs"`
	Capture       []string             `json:"capture"`       // usable capture methods, default first
	CursorInVideo bool                 `json:"cursorInVideo"` // false: frames never contain the pointer (draw it on the client)
	Outputs       []Output             `json:"outputs"`
	Unavailable   map[string]string    `json:"unavailable"` // probed backend/capture -> why it is not usable
	QPCFrequency  int64                `json:"qpcFrequency"`
}

// Output is one display output the helper can capture.
type Output struct {
	Index        int    `json:"index"`
	AdapterIndex int    `json:"adapterIndex"`
	OutputIndex  int    `json:"outputIndex"` // StartParams.Monitor together with AdapterLUID
	AdapterLUID  string `json:"adapterLuid"`
	AdapterName  string `json:"adapterName"`
	Vendor       string `json:"vendor"`
	Name         string `json:"name"` // GDI device name, \\.\DISPLAY1
	HMonitor     uint64 `json:"hmonitor"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Rotation     int    `json:"rotation"` // 0 | 90 | 180 | 270
	Attached     bool   `json:"attached"`
	// Colour (IDXGIOutput6::GetDesc1; older helpers and Wine: zero): HDR is
	// Windows HDR on for this output; the luminance values are the panel's, in
	// cd/m2 (EDID or the Windows HDR calibration).
	HDR                   bool    `json:"hdr"`
	BitsPerColor          int     `json:"bitsPerColor"`
	MinLuminance          float64 `json:"minLuminance"`
	MaxLuminance          float64 `json:"maxLuminance"`
	MaxFullFrameLuminance float64 `json:"maxFullFrameLuminance"`
}

// CodecCaps describes one codec of the selected backend.
type CodecCaps struct {
	MaxW              int    `json:"maxW"`
	MaxH              int    `json:"maxH"`
	TenBit            bool   `json:"tenBit"`
	YUV444            bool   `json:"yuv444"`
	ForceIDR          bool   `json:"forceIdr"`
	Recovery          string `json:"recovery"` // ltr | invalidate | none
	MaxLTR            int    `json:"maxLtr"`   // LTR slots Start's LTRSlots may ask for; 0 = no LTR recovery (NVENC invalidates)
	IntraRefresh      bool   `json:"intraRefresh"`
	LiveBitrate       string `json:"liveBitrate"` // seamless | flush | restart
	MaxTemporalLayers int    `json:"maxTemporalLayers"`
	ROI               string `json:"roi"` // importance (AMF) | emphasis (NVENC QP delta map) | none
	SliceOutput       bool   `json:"sliceOutput"`
	HWInstances       int    `json:"hwInstances"`
	QueryTimeout      bool   `json:"queryTimeout"`
	AlignW            int    `json:"alignW"`
	AlignH            int    `json:"alignH"`
	// DynamicResolution: the running encoder can change its coded size
	// without a new session (NVENC); no control message uses it yet.
	DynamicResolution bool `json:"dynamicResolution"`
	// HDR10: StartParams.HDR can produce HDR10 with this codec (10-bit P010
	// input, Main10 / AV1 10-bit, BT.2020 PQ and HDR metadata; step 3.9).
	HDR10 bool `json:"hdr10"`
	// Assumed names the fields above that are documented or default values,
	// not detected on this GPU (e.g. AMF AV1 "roi", "liveBitrate" until the
	// step 3.6 qualification); omitted when everything was detected.
	Assumed []string `json:"assumed,omitempty"`
}

// IsAssumed reports whether field (its JSON name) was assumed rather than detected.
func (c CodecCaps) IsAssumed(field string) bool {
	for _, f := range c.Assumed {
		if f == field {
			return true
		}
	}
	return false
}

// Usable reports whether the helper can encode anything.
func (c *Caps) Usable() bool { return c.Backend != "none" && c.Backend != "" && len(c.Codecs) > 0 }

// Started answers a successful Start with what the encoder actually does.
type Started struct {
	Backend       string `json:"backend"`
	Capture       string `json:"capture"`
	Codec         string `json:"codec"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	Kbps          int    `json:"kbps"`
	CaptureWidth  int    `json:"captureWidth"` // the source as displayed
	CaptureHeight int    `json:"captureHeight"`
	AdapterLUID   string `json:"adapterLuid"` // the capture / encode adapter ("" for synthetic)
	AdapterName   string `json:"adapterName"`
	Vendor        string `json:"vendor"`
	HAGSEnabled   *bool  `json:"hagsEnabled"`
	GPUPriority   string `json:"gpuPriority"` // realtime | high | failed | off | "" (no GPU)
	IdleRepeatMs  int    `json:"idleRepeatMs"`
	Barcode       bool   `json:"barcode"`
	// CursorInVideo: this stream's frames contain the mouse pointer (a WGC
	// session that could not exclude it), so the client must not draw its own.
	CursorInVideo bool `json:"cursorInVideo"`

	// The bitstream's frame size. AV1 on RDNA3 is coded in 64x16 multiples:
	// the picture is the top-left Width x Height, CropRight / CropBottom
	// columns / rows of padding follow (proto.VideoConfig gets the same, GUIDE
	// 1.7). Older helpers omit them: zero means Width / Height, no crop.
	CodedWidth  int `json:"codedWidth"`
	CodedHeight int `json:"codedHeight"`
	CropRight   int `json:"cropRight"`
	CropBottom  int `json:"cropBottom"`
	// What the encoder does (diagnostics and the recovery / rate logic).
	LiveBitrate        string `json:"liveBitrate"`        // seamless | flush: how SetRate is applied
	RateControl        string `json:"rateControl"`        // e.g. cbr | vbr_latency
	Usage              string `json:"usage"`              // e.g. ultra_low_latency (AMF H.264 may fall back to low_latency)
	LTRSlots           int    `json:"ltrSlots"`           // LTR slots in use; 0 = no LTR recovery (the codec's caps Recovery applies)
	LTRInterval        int    `json:"ltrInterval"`        // frames between LTR marks
	EncoderInstance    int    `json:"encoderInstance"`    // hardware engine used
	HWInstances        int    `json:"hwInstances"`        // engines the GPU has for this codec
	QueryTimeoutMs     int    `json:"queryTimeoutMs"`     // blocking output wait, 0 = polled
	ZeroCopy           bool   `json:"zeroCopy"`           // capture surfaces encoded without the NV12 conversion
	IntraRefreshFrames int    `json:"intraRefreshFrames"` // intra refresh cycle, 0 = off
	// NVENC (step 3.4; other backends leave them empty): the preset "p1".."p7",
	// whether the output comes by completion events (async) or by polling, and
	// the reference frames the encoder was configured to keep (the
	// invalidation window: 6, 5 at 4K H.264 / HEVC; the helper narrows it,
	// with a warning in its log, should the encoder's SPS say fewer).
	Preset      string `json:"preset"`
	AsyncEncode bool   `json:"asyncEncode"`
	RefFrames   int    `json:"refFrames"`
	// HDR10 (step 3.9): HDR is true when the stream is 10-bit BT.2020 PQ
	// (ColorSpace "bt2020-pq", BitDepth 10) with HDRMetadata; otherwise 8-bit
	// "bt709". Older helpers omit them (zero values: treat as 8-bit BT.709).
	HDR         bool         `json:"hdr"`
	BitDepth    int          `json:"bitDepth"`
	ColorSpace  string       `json:"colorSpace"`
	HDRMetadata *HDRMetadata `json:"hdrMetadata,omitempty"`
}

// HDRMetadata is an HDR10 stream's static metadata as the encoder writes it
// (HEVC mastering display colour volume and content light level SEI, AV1
// metadata OBUs): the mastering display's primaries and white point (CIE 1931
// xy; BT.2020 / D65) and luminance range, MaxCLL and MaxFALL. The helper takes
// the luminance values from the captured output (the host display's limits).
type HDRMetadata struct {
	DisplayPrimaries [3][2]float64 `json:"displayPrimaries"` // red, green, blue
	WhitePoint       [2]float64    `json:"whitePoint"`
	MaxLuminance     float64       `json:"maxLuminance"` // cd/m2
	MinLuminance     float64       `json:"minLuminance"`
	MaxCLL           int           `json:"maxCll"` // cd/m2, 0 = unknown
	MaxFALL          int           `json:"maxFall"`
}

// CaptureChanged reports a change of the capture source. Reason "resized":
// new size or rotation (the stream keeps its encoded size, scaled; restart the
// helper to follow); "lost": capture unavailable for now (secure desktop,
// output gone, mode switch), the last image is repeated; "restored"; "hdr":
// Windows HDR was turned on or off for the output (HDR says which; the stream
// keeps its format, restart the helper to follow).
type CaptureChanged struct {
	Reason   string `json:"reason"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Rotation int    `json:"rotation"`
	HDR      bool   `json:"hdr"` // the output is in HDR mode now (dda; amd-direct: as at the start)
	Text     string `json:"text"`
}

// Stats is the helper's per-frame report, also sent for frames it dropped
// (Dropped, Reason "ringFull" or "tooLarge"). Timestamps are QPC ticks
// (Caps.QPCFrequency per second).
type Stats struct {
	FrameID       uint64  `json:"frameId"`
	Gen           uint32  `json:"gen"`
	Dropped       bool    `json:"dropped"`
	Reason        string  `json:"reason,omitempty"`
	Key           bool    `json:"key"`
	Recovery      bool    `json:"recovery"`
	Repeat        bool    `json:"repeat"`   // idle re-submit of the previous image
	DirtyPct      int     `json:"dirtyPct"` // share of the image that changed, -1 = unknown
	Bytes         uint64  `json:"bytes"`
	PresentQPC    int64   `json:"presentQpc"`
	CaptureQPC    int64   `json:"captureQpc"`
	SubmitQPC     int64   `json:"submitQpc"`
	OutputQPC     int64   `json:"outputQpc"`
	RefFloor      uint64  `json:"refFloor,omitempty"`
	LTRSlot       int32   `json:"ltrSlot"`
	TemporalLayer uint32  `json:"temporalLayer"`
	RefLTRMask    uint32  `json:"refLtrMask"`
	Kbps          int     `json:"kbps"` // current target, as last set
	VBVFrames     float64 `json:"vbvFrames"`
	FPS           int     `json:"fps"`
	RingDropped   uint64  `json:"ringDropped"` // frames dropped by the helper so far
}

// HelperError is an "error" message from the helper. A fatal error ends the
// helper; the caller restarts it.
type HelperError struct {
	Code  string `json:"code"`
	Text  string `json:"text"`
	Fatal bool   `json:"fatal"`
	Re    string `json:"re,omitempty"` // request type that caused it ("start", "setRate", ...)
}

func (e *HelperError) Error() string {
	kind := "error"
	if e.Fatal {
		kind = "fatal error"
	}
	if e.Re != "" {
		return fmt.Sprintf("encoder helper %s (%s, in reply to %s): %s", kind, e.Code, e.Re, e.Text)
	}
	return fmt.Sprintf("encoder helper %s (%s): %s", kind, e.Code, e.Text)
}

// Go -> helper messages.

type startMsg struct {
	T string `json:"t"` // "start"
	StartParams
}

type simpleMsg struct {
	T string `json:"t"` // "forceIdr" | "shutdown"
}

type recoverMsg struct {
	T               string  `json:"t"` // "recover"
	LostFromFrameID uint64  `json:"lostFromFrameId"`
	AckedLTRFrameID *uint64 `json:"ackedLtrFrameId,omitempty"`
}

type setRateMsg struct {
	T         string  `json:"t"` // "setRate"
	Kbps      int     `json:"kbps"`
	VBVFrames float64 `json:"vbvFrames,omitempty"`
	FPS       int     `json:"fps,omitempty"`
}

type ackMsg struct {
	T       string `json:"t"` // "ack"
	FrameID uint64 `json:"frameId"`
}

type setROIMsg struct {
	T     string    `json:"t"` // "setRoi"
	Rects []ROIRect `json:"rects"`
}

var errUnknownMessage = errors.New("unknown message type")

// decodeMessage parses one helper -> Go message into *Caps, *Started, *Stats,
// *CaptureChanged or *HelperError. Unknown types return errUnknownMessage (ignored by the
// reader, for forward compatibility).
func decodeMessage(b []byte) (any, error) {
	var env struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("encoder helper: bad control message: %w", err)
	}
	var v any
	switch env.T {
	case "caps":
		v = &Caps{}
	case "started":
		v = &Started{}
	case "stats":
		v = &Stats{}
	case "captureChanged":
		v = &CaptureChanged{}
	case "error":
		v = &HelperError{}
	default:
		return nil, fmt.Errorf("%w %q", errUnknownMessage, env.T)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("encoder helper: bad %s message: %w", env.T, err)
	}
	return v, nil
}
