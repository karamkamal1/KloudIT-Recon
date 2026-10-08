// Package host implements the recon-host agent that runs on the gaming PC.
package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// Config is persisted as JSON (see DefaultConfigPath).
type Config struct {
	Name       string `json:"name"`
	Gateway    string `json:"gateway"`    // host:port of the gateway (UDP)
	GatewayPin string `json:"gatewayPin"` // SPKI pin of the gateway tunnel identity
	HostID     string `json:"hostId"`
	Token      string `json:"token"`

	FFmpeg     string `json:"ffmpeg,omitempty"`
	Capture    string `json:"capture"` // auto | ddagrab | gfxcapture | x11grab | test | amf (AMD Direct Capture, experimental, opt-in)
	X11Display string `json:"x11Display,omitempty"`
	Encoder    string `json:"encoder,omitempty"` // force an encoder, e.g. hevc_nvenc
	DrawCursor bool   `json:"drawCursor"`        // bake the cursor into the video instead of local rendering
	// CaptureTimestamps "off" stops stamping frames with their capture time
	// (FFmpeg setpts=time(0)*1000000 + a µs encoder time base); default auto = on.
	// Capture "amf" keeps that wall-clock pts in the FFmpeg chain (vsrc_amf's
	// own pts are rounded to 1/fps); there "off" only stops sending the stamps.
	CaptureTimestamps string `json:"captureTimestamps,omitempty"`
	// GPUPriority is the GPU scheduling priority of the encoder process
	// (Windows): auto | high | realtime | off ("" = auto).
	GPUPriority string `json:"gpuPriority,omitempty"`
	// Pipeline picks the video pipeline: auto | helper | ffmpeg ("" = auto).
	// auto uses the native encoder helper (recon-encoder.exe next to
	// recon-host.exe, Windows) when it starts, can encode the negotiated codec
	// and the session needs nothing only FFmpeg offers; else FFmpeg.
	Pipeline string `json:"pipeline,omitempty"`
	// VirtualDisplay gives a session a virtual monitor matched to the client
	// (resolution and frame rate) through an installed IddCx driver
	// (internal/host/vdisplay): off | auto | on ("" = off).
	VirtualDisplay string `json:"virtualDisplay,omitempty"`
	// VirtualDisplayLayout places the virtual monitor: primary | extend | only
	// ("" = primary).
	VirtualDisplayLayout string `json:"virtualDisplayLayout,omitempty"`
	// AV1 is when the automatic codec choice uses AV1 (step 4.2): AV1Fallback
	// ("" = default) only where HEVC does not work end-to-end, AV1Faster also
	// instead of HEVC for clients that decode AV1 clearly faster (enable it
	// after measuring this host's AV1 encoder: Phase 0 latency and VMAF).
	AV1 string `json:"av1,omitempty"`

	// SVC is temporal SVC (Phase 5): "auto" ("" = auto) starts native-helper
	// streams with two temporal layers where the encoder has them (to
	// clients that can be thinned), and leaves out the frames no other frame
	// references (the enhancement layer; on the FFmpeg path non-reference
	// frames) under congestion: the frame rate halves for a moment, no
	// corruption, no key frame; "off": neither.
	SVC string `json:"svc,omitempty"`
	// StaticBitrate "auto" ("" = auto) lowers the encoder's bitrate while the
	// desktop is static (the native helper's dirty rects; a seamless live
	// bitrate only) to StaticKbps and raises it again with the first frame
	// that changes: to the full bitrate from 5 % of the picture changed,
	// linearly between 0.2 % and 5 %; "off": never.
	StaticBitrate string `json:"staticBitrate,omitempty"`
	// StaticKbps is a static desktop's bitrate (0 = a quarter of the rate
	// controller's target, at least 2000 kbit/s; never above the target).
	StaticKbps int `json:"staticKbps,omitempty"`
	// FPSFloor is the lowest frame rate the rate controller lowers to at its
	// bitrate floor before anything else (0 = 60, GUIDE 2.2). Where the
	// encoder changes its frame rate in place (the native helper) in fine
	// steps down to it, 2 s apart; elsewhere (FFmpeg, flushing encoders,
	// older helpers) the rungs 120 / 90 / 60 at or above it, nothing below
	// 60.
	FPSFloor int `json:"fpsFloor,omitempty"`

	DirectPort int    `json:"directPort"`           // UDP port for direct WebTransport (0 = off)
	DirectAddr string `json:"directAddr,omitempty"` // advertised address override
	Congestion string `json:"congestion,omitempty"` // QUIC congestion control of video connections: media | reno ("" = media)

	DefaultKbps int  `json:"defaultKbps"`
	MaxKbps     int  `json:"maxKbps"`
	DefaultFPS  int  `json:"defaultFps"`
	MaxFPS      int  `json:"maxFps"`
	Audio       bool `json:"audio"`
	AudioKbps   int  `json:"audioKbps"`
	Gamepad     bool `json:"gamepad"`

	TestWidth  int `json:"testWidth,omitempty"`
	TestHeight int `json:"testHeight,omitempty"`
	// TestPad: rows of padding below the test pattern that clients must crop
	// (VideoConfig cropBottom), as an encoder that pads the coded picture
	// produces; the browser E2E checks the client's crop with it.
	TestPad  int    `json:"testPad,omitempty"`
	LogLevel string `json:"logLevel,omitempty"`

	path string // file it was loaded from; the agent re-reads it to pick up a new pairing
}

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	if c.Capture == "" {
		if runtime.GOOS == "windows" {
			c.Capture = "auto"
		} else {
			c.Capture = "test"
		}
	}
	if c.X11Display == "" {
		c.X11Display = os.Getenv("DISPLAY")
		if c.X11Display == "" {
			c.X11Display = ":0.0"
		}
	}
	if c.DefaultKbps == 0 {
		c.DefaultKbps = 30000
	}
	if c.MaxKbps == 0 {
		c.MaxKbps = 250000
	}
	if c.DefaultFPS == 0 {
		c.DefaultFPS = 60
	}
	if c.MaxFPS == 0 {
		c.MaxFPS = 240
	}
	if c.AudioKbps == 0 {
		c.AudioKbps = 160
	}
	if c.TestWidth == 0 {
		c.TestWidth, c.TestHeight = 1280, 720
	}
}

// DefaultConfigPath returns the per-user config file location.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "KlouditRecon", "host.json")
		}
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "kloudit-recon", "host.json")
}

// LoadConfig reads the config file (a missing file yields defaults).
func LoadConfig(path string) (*Config, error) {
	c := &Config{Audio: true, Gamepad: true, DirectPort: 47998}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		// Windows PowerShell 5.1 and some editors save UTF-8 with a byte-order mark.
		b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	c.Defaults()
	if !transport.ValidCongestion(c.Congestion) {
		return nil, fmt.Errorf("%s: congestion must be %q or %q, not %q", path, transport.CongestionReno, transport.CongestionMedia, c.Congestion)
	}
	if !validAV1(c.AV1) {
		return nil, fmt.Errorf("%s: av1 must be %q or %q, not %q", path, AV1Fallback, AV1Faster, c.AV1)
	}
	if !media.ValidGPUPriority(c.GPUPriority) {
		return nil, fmt.Errorf("%s: gpuPriority must be %q, %q, %q or %q, not %q", path,
			media.GPUPriorityAuto, media.GPUPriorityHigh, media.GPUPriorityRealtime, media.GPUPriorityOff, c.GPUPriority)
	}
	switch c.Pipeline {
	case "", pipelineAuto, media.PipelineHelper, media.PipelineFFmpeg:
	default:
		return nil, fmt.Errorf("%s: pipeline must be %q, %q or %q, not %q", path, pipelineAuto, media.PipelineHelper,
			media.PipelineFFmpeg, c.Pipeline)
	}
	for _, v := range []struct{ key, val string }{{"svc", c.SVC}, {"staticBitrate", c.StaticBitrate}} {
		if v.val != "" && v.val != settingAuto && v.val != settingOff {
			return nil, fmt.Errorf("%s: %s must be %q or %q, not %q", path, v.key, settingAuto, settingOff, v.val)
		}
	}
	if c.StaticKbps < 0 {
		return nil, fmt.Errorf("%s: staticKbps must not be negative, not %d", path, c.StaticKbps)
	}
	if c.FPSFloor != 0 && (c.FPSFloor < 10 || c.FPSFloor > 240) {
		return nil, fmt.Errorf("%s: fpsFloor must be 0 (default) or 10..240, not %d", path, c.FPSFloor)
	}
	if !vdisplay.ValidPolicy(c.VirtualDisplay) {
		return nil, fmt.Errorf("%s: virtualDisplay must be %q, %q or %q, not %q", path,
			vdisplay.PolicyOff, vdisplay.PolicyAuto, vdisplay.PolicyOn, c.VirtualDisplay)
	}
	if !vdisplay.ValidLayout(c.VirtualDisplayLayout) {
		return nil, fmt.Errorf("%s: virtualDisplayLayout must be %q, %q or %q, not %q", path,
			vdisplay.LayoutPrimary, vdisplay.LayoutExtend, vdisplay.LayoutOnly, c.VirtualDisplayLayout)
	}
	c.path = path
	return c, nil
}

// congestion returns the congestion controller for the direct server, the UDP
// relay and the splice relay's data connections: media by default since the rate controller (GUIDE
// 2.2) backs off for it; reno stays selectable. The default stays empty in the
// file, so a later release can change it.
func (c *Config) congestion() string {
	if c.Congestion == "" {
		return transport.CongestionMedia
	}
	return c.Congestion
}

// pipelineAuto is the default of host config "pipeline": the native helper
// where it can serve the session, else FFmpeg.
const pipelineAuto = "auto"

// pipeline returns the video pipeline mode: auto, helper or ffmpeg.
func (c *Config) pipeline() string {
	if c.Pipeline == "" {
		return pipelineAuto
	}
	return c.Pipeline
}

// Values of the auto | off settings (svc, staticBitrate).
const (
	settingAuto = "auto"
	settingOff  = "off"
)

// svc reports whether temporal SVC thinning is on (Phase 5): "svc" auto.
func (c *Config) svc() bool { return c.SVC != settingOff }

// staticBitrate reports whether a static desktop lowers the bitrate:
// "staticBitrate" auto.
func (c *Config) staticBitrate() bool { return c.StaticBitrate != settingOff }

// av1 returns the AV1 policy of the automatic codec choice.
func (c *Config) av1() string {
	if c.AV1 == "" {
		return AV1Fallback
	}
	return c.AV1
}

// gpuPriority returns the encoder's GPU scheduling priority mode.
func (c *Config) gpuPriority() string {
	if c.GPUPriority == "" {
		return media.GPUPriorityAuto
	}
	return c.GPUPriority
}

// virtualDisplayOptions are the vdisplay.Options of this config: policy and
// layout, the restore journal next to the config file, and the host id as the
// virtual monitor's identity. The caller adds RenderAdapter, Linger and Log.
func (c *Config) virtualDisplayOptions() vdisplay.Options {
	o := vdisplay.Options{Policy: c.VirtualDisplay, Layout: c.VirtualDisplayLayout, MonitorID: c.HostID}
	if o.Policy == "" {
		o.Policy = vdisplay.PolicyOff
	}
	if o.Layout == "" {
		o.Layout = vdisplay.LayoutPrimary
	}
	if o.MonitorID == "" {
		o.MonitorID = c.Name
	}
	if c.path != "" {
		o.StateDir = filepath.Dir(c.path)
	}
	return o
}

// Save writes the config with owner-only permissions (it contains the host token).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
