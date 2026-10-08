package proto

// Control channel messages. Every message is a JSON object with a "t" field.

// Hello is the first control message sent by the client.
type Hello struct {
	T        string        `json:"t"` // "hello"
	V        int           `json:"v"`
	Ticket   string        `json:"ticket,omitempty"` // required on the direct path only
	Client   ClientInfo    `json:"client"`
	Decoders []DecoderInfo `json:"decoders"`
	Audio    AudioCaps     `json:"audio"`
	Prefs    Prefs         `json:"prefs"`
}

type ClientInfo struct {
	UA     string  `json:"ua"`
	Width  int     `json:"w"` // screen width in device pixels
	Height int     `json:"h"` // screen height in device pixels
	DPR    float64 `json:"dpr"`
	Hz     float64 `json:"hz"` // measured display refresh rate
}

// DecoderInfo reports what the browser can decode via WebCodecs.
type DecoderInfo struct {
	Family string `json:"family"` // h264 | hevc | av1
	// HW: a hardware decoder is available. Clients with the decoder
	// self-test (step 4.1) report false when the hardware decoder holds
	// frames back, so that a family decoded in hardware without delay wins.
	HW bool `json:"hw"`
	// Timing is the client's timed decode of a short sample of this family
	// (step 4.2), with the decoder its stream would use. Nil from clients
	// before it, and when the timed decode failed. The host picks the codec
	// family by it (see host.chooseFamily).
	Timing *DecodeTiming `json:"timing,omitempty"`
}

// DecodeTiming is a timed decode: a W x H key frame and N P frames (decode
// order = display order) fed one at a time, each after the previous one's
// output, as frames arrive on a stream.
type DecodeTiming struct {
	// Ms is the median time from decode() to the frame's output over the N P
	// frames: the decoder's latency per frame, not its throughput.
	Ms    float64 `json:"ms"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	N     int     `json:"n"`
	Accel string  `json:"accel"` // the WebCodecs hardwareAcceleration it decoded with
}

type AudioCaps struct {
	Opus bool `json:"opus"`
	PCM  bool `json:"pcm"`
}

// Prefs are the user's stream preferences. Zero values mean "host default".
type Prefs struct {
	Codec       string `json:"codec,omitempty"`   // auto | h264 | hevc | av1
	BitrateKbps int    `json:"bitrate,omitempty"` // target video bitrate
	FPS         int    `json:"fps,omitempty"`
	Width       int    `json:"width,omitempty"` // 0 = native
	Height      int    `json:"height,omitempty"`
	Monitor     int    `json:"monitor"`              // index into Welcome.Monitors
	Window      string `json:"window,omitempty"`     // capture a single window by title regex (gfxcapture)
	Audio       *bool  `json:"audio,omitempty"`      // default true
	AudioCodec  string `json:"audioCodec,omitempty"` // opus | pcm
	Cursor      string `json:"cursor,omitempty"`     // local | video
	Quality     string `json:"quality,omitempty"`    // speed | balanced | quality
	Adaptive    *bool  `json:"adaptive,omitempty"`   // adaptive bitrate on congestion, default true
}

func (p Prefs) AudioEnabled() bool { return p.Audio == nil || *p.Audio }

// AdaptiveBitrate reports whether the client lets the host change the bitrate
// during the session (clients before the adaptive field always did).
func (p Prefs) AdaptiveBitrate() bool { return p.Adaptive == nil || *p.Adaptive }

// Welcome is the host's reply to Hello.
type Welcome struct {
	T        string        `json:"t"` // "welcome"
	Session  string        `json:"session"`
	Host     string        `json:"host"`
	OS       string        `json:"os"`
	Version  string        `json:"version"`
	Monitors []MonitorInfo `json:"monitors"`
	Encoders []string      `json:"encoders"`
	Features []string      `json:"features"` // cursor, gamepad, audio, text
	MaxKbps  int           `json:"maxKbps"`
	MaxFPS   int           `json:"maxFps"`
	// WallOffsetUs is the host's wall clock (µs since the Unix epoch: what
	// Date.now() reads in a browser on the host) minus the host clock (µs).
	// The latency test page draws the wall clock into the picture; the client
	// converts it to the host clock with this. Refreshed by Clock messages.
	WallOffsetUs int64 `json:"wallOffsetUs,omitempty"`
}

// Clock refreshes Welcome.WallOffsetUs every few seconds (clients with hello
// v >= 2): the wall clock and the host clock drift apart (on Windows W32Time
// slews and steps the wall clock, QueryPerformanceCounter is not disciplined).
type Clock struct {
	T            string `json:"t"` // "clock"
	WallOffsetUs int64  `json:"wallOffsetUs"`
}

type MonitorInfo struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Width   int    `json:"w"`
	Height  int    `json:"h"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Primary bool   `json:"primary"`
	Hz      int    `json:"hz,omitempty"`
}

// VideoConfig announces a (new) encoder generation. The client must (re)configure
// its decoder and wait for a key frame of this generation.
type VideoConfig struct {
	T           string `json:"t"` // "video"
	Gen         uint8  `json:"gen"`
	Family      string `json:"family"`
	Codec       string `json:"codec"` // WebCodecs codec string, e.g. avc1.640033
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrate"`
	// MaxBitrateKbps is the bitrate the host goes back to after a congestion
	// back-off (the settings' bitrate or the host default): BitrateKbps is
	// below it while a back-off lasts. 0 from hosts before this field.
	MaxBitrateKbps int    `json:"maxBitrate,omitempty"`
	Encoder        string `json:"encoder"`
	Capture        string `json:"capture"`
	// Recovery is how the client recovers from a confirmed frame loss (a frame
	// the host reported dropped, or a gap that outlasted the late-frame
	// timeout): RecoverySkip when the encoder heals the picture by itself
	// (intra refresh), RecoveryKeyframe (also when empty: hosts before this
	// field) when the decoder needs a key frame.
	Recovery string `json:"recovery,omitempty"`
	// Crop, set when the coded picture is larger than Width x Height
	// (omitted otherwise): the decoder outputs CodedWidth x CodedHeight, of
	// which the right CropRight columns and bottom CropBottom rows are
	// padding, and the client shows only the top-left Width x Height. An
	// encoder that codes in blocks pads (AV1 on RDNA3 codes 1920x1080 as
	// 1920x1082) and AV1 has no cropping window. Clients that ignore these
	// fields show the padding; Width and Height keep their meaning.
	CodedWidth  int `json:"codedWidth,omitempty"`
	CodedHeight int `json:"codedHeight,omitempty"`
	CropRight   int `json:"cropRight,omitempty"`
	CropBottom  int `json:"cropBottom,omitempty"`
}

// SetCrop announces the padding of a coded picture of codedW x codedH whose
// visible part is w x h (no crop fields when it has none).
func (c *VideoConfig) SetCrop(w, h, codedW, codedH int) {
	c.Width, c.Height = w, h
	c.CodedWidth, c.CodedHeight, c.CropRight, c.CropBottom = 0, 0, 0, 0
	if codedW > w || codedH > h {
		c.CodedWidth, c.CodedHeight = max(codedW, w), max(codedH, h)
		c.CropRight, c.CropBottom = c.CodedWidth-w, c.CodedHeight-h
	}
}

// VideoConfig.Recovery values.
const (
	RecoverySkip     = "skip"     // skip the lost frame and keep decoding
	RecoveryKeyframe = "keyframe" // request a key frame (FFmpeg path: a new encoder generation)
)

// Dropped tells the client that the host discarded Count frames of
// generation Gen from FromSeq on, which it will never send (frame queue
// overflow, or a frame stream that failed or was cancelled). The client
// treats them as lost at once instead of waiting out its gap timeout: frames
// travel on reliable streams, so a gap without this message is a late frame.
type Dropped struct {
	T       string `json:"t"` // "dropped"
	Gen     uint8  `json:"gen"`
	FromSeq uint32 `json:"fromSeq"`
	Count   int    `json:"count"`
}

type AudioConfig struct {
	T          string `json:"t"` // "audio"
	Codec      string `json:"codec"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
	FrameMs    int    `json:"frameMs"`
	Enabled    bool   `json:"enabled"`
	// SameStream marks a config that changes the running stream (its Opus
	// frame duration, step 4.6): its sequence numbers go on. Any other
	// config announces a new stream, numbered from 0.
	SameStream bool `json:"sameStream,omitempty"`
}

// CursorShape carries a cursor image (PNG, base64) for local cursor rendering.
type CursorShape struct {
	T      string `json:"t"` // "cursor"
	ID     uint64 `json:"id"`
	PNG    string `json:"png,omitempty"`
	HotX   int    `json:"hotX"`
	HotY   int    `json:"hotY"`
	Width  int    `json:"w"`
	Height int    `json:"h"`
	Hidden bool   `json:"hidden,omitempty"`
}

type Notice struct {
	T     string `json:"t"` // "notice"
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// ClientMsg is the union of client->host control messages after Hello.
type ClientMsg struct {
	T       string      `json:"t"` // settings | keyframe | congestion | stages | pause | resume | bye
	Prefs   *Prefs      `json:"prefs,omitempty"`
	DelayMs int         `json:"delayMs,omitempty"`
	Reason  string      `json:"reason,omitempty"` // "congestion": CongestionDecoder, or "" (one-way delay grew)
	Stages  []StageStat `json:"stages,omitempty"` // "stages": the client's latency summary
	// Renderer ("stages"): the presentation path that drew the frames
	// (canvas2d | webgl2 | webgpu, or bakeoff for a window with several;
	// step 4.3): the draw and display rows depend on it. Empty from clients
	// before it.
	Renderer string `json:"renderer,omitempty"`
	// Pacing ("stages"): the client's frame pacing mode for the window's
	// frames (latency | smooth, or mixed when it changed; step 4.4): the hold
	// and display rows depend on it. Empty from clients before it.
	Pacing string `json:"pacing,omitempty"`
}

// CongestionDecoder is the reason of a "congestion" message from a client
// whose decoder fell behind: it flushed the decoder and discards the current
// generation's frames until a new key frame, so the host restarts at once
// instead of overlapped.
const CongestionDecoder = "decoder"

// StageStat is one row of the per-stage latency summary a v2 client sends
// every ~10 s ({"t":"stages"}): percentiles in ms over its last ~10 s window.
// Names: capture, queue, network, transfer, wait, decode, hold (only to hosts
// that announce FeatureStageHold), draw, display, e2e.
// FeatureStageHold is the Welcome.Features entry announcing that the host
// takes the "hold" row (decoder output -> draw start: the frame pacing wait,
// step 4.4). Hosts before it accept at most nine rows; clients report hold
// and draw to them as one draw row (decoder output -> drawn).
const FeatureStageHold = "stage-hold"

type StageStat struct {
	Name string  `json:"name"`
	From string  `json:"from,omitempty"` // e2e only: capture | send (where end-to-end starts)
	N    int     `json:"n"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
}
