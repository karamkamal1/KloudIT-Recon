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
	HDR       bool    `json:"hdr,omitempty"`
	LTRSlots  int     `json:"ltrSlots,omitempty"`  // long-term reference slots (ACK-based recovery)
	SVCLayers int     `json:"svcLayers,omitempty"` // temporal layers (default 1)

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
}

// CodecCaps describes one codec of the selected backend.
type CodecCaps struct {
	MaxW              int    `json:"maxW"`
	MaxH              int    `json:"maxH"`
	TenBit            bool   `json:"tenBit"`
	YUV444            bool   `json:"yuv444"`
	ForceIDR          bool   `json:"forceIdr"`
	Recovery          string `json:"recovery"` // ltr | invalidate | none
	MaxLTR            int    `json:"maxLtr"`
	IntraRefresh      bool   `json:"intraRefresh"`
	LiveBitrate       string `json:"liveBitrate"` // seamless | flush | restart
	MaxTemporalLayers int    `json:"maxTemporalLayers"`
	ROI               string `json:"roi"` // importance | emphasis | none
	SliceOutput       bool   `json:"sliceOutput"`
	HWInstances       int    `json:"hwInstances"`
	QueryTimeout      bool   `json:"queryTimeout"`
	AlignW            int    `json:"alignW"`
	AlignH            int    `json:"alignH"`
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
}

// CaptureChanged reports a change of the capture source. Reason "resized":
// new size or rotation (the stream keeps its encoded size, scaled; restart the
// helper to follow); "lost": capture unavailable for now (secure desktop,
// output gone, mode switch), the last image is repeated; "restored".
type CaptureChanged struct {
	Reason   string `json:"reason"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Rotation int    `json:"rotation"`
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
