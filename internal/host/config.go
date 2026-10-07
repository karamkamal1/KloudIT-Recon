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
	Capture    string `json:"capture"` // auto | ddagrab | gfxcapture | x11grab | test
	X11Display string `json:"x11Display,omitempty"`
	Encoder    string `json:"encoder,omitempty"` // force an encoder, e.g. hevc_nvenc
	DrawCursor bool   `json:"drawCursor"`        // bake the cursor into the video instead of local rendering
	// CaptureTimestamps "off" stops stamping frames with their capture time
	// (FFmpeg setpts=time(0)*1000000 + a µs encoder time base); default auto = on.
	CaptureTimestamps string `json:"captureTimestamps,omitempty"`

	DirectPort int    `json:"directPort"`           // UDP port for direct WebTransport (0 = off)
	DirectAddr string `json:"directAddr,omitempty"` // advertised address override
	Congestion string `json:"congestion,omitempty"` // QUIC congestion control of video connections: reno | media ("" = default)

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
	c.path = path
	return c, nil
}

// congestion returns the congestion controller for the direct server and the
// relay data connections. The default stays empty in the file, so a later
// release can change it.
func (c *Config) congestion() string {
	if c.Congestion == "" {
		return transport.CongestionReno
	}
	return c.Congestion
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
