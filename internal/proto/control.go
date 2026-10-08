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
	HW     bool   `json:"hw"`     // hardware decoder available
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
	Encoder     string `json:"encoder"`
	Capture     string `json:"capture"`
	// Recovery is how the client recovers from a confirmed frame loss (a frame
	// the host reported dropped, or a gap that outlasted the late-frame
	// timeout): RecoverySkip when the encoder heals the picture by itself
	// (intra refresh), RecoveryKeyframe (also when empty: hosts before this
	// field) when the decoder needs a key frame.
	Recovery string `json:"recovery,omitempty"`
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
}

// CongestionDecoder is the reason of a "congestion" message from a client
// whose decoder fell behind: it flushed the decoder and discards the current
// generation's frames until a new key frame, so the host restarts at once
// instead of overlapped.
const CongestionDecoder = "decoder"

// StageStat is one row of the per-stage latency summary a v2 client sends
// every ~10 s ({"t":"stages"}): percentiles in ms over its last ~10 s window.
// Names: capture, queue, network, transfer, wait, decode, draw, display, e2e.
type StageStat struct {
	Name string  `json:"name"`
	From string  `json:"from,omitempty"` // e2e only: capture | send (where end-to-end starts)
	N    int     `json:"n"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
}
