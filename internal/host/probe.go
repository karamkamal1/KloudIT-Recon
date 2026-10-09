package host

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
)

// The native encoder helper in "recon-host probe", which install-host.ps1
// prints as the detected capabilities: sessions on AMD and NVIDIA GPUs stream
// with it by default (host config "pipeline" auto), so the probe asks it what
// it can encode, launched as a session's first launch is (its own choice of
// backend, the libavcodec backend's libraries from "helperFFmpegDir").

// helperProbeWait bounds the helper's --print-caps (it opens every backend's
// runtime and the GPU).
const helperProbeWait = 30 * time.Second

// ProbeHelper writes the helper lines of "recon-host probe" for cfg to w:
// whether sessions use the helper next to this executable, and from its
// --print-caps the encoder backend it picks, its codecs and GPU, and why the
// other backends are unavailable. It reports whether sessions can stream
// with it.
func ProbeHelper(ctx context.Context, cfg *Config, w io.Writer) bool {
	switch {
	case cfg.pipeline() == media.PipelineFFmpeg:
		fmt.Fprintln(w, `helper:     not used: host config "pipeline" is "ffmpeg"`)
		return false
	case runtime.GOOS != "windows":
		fmt.Fprintln(w, "helper:     not used: the native encoder helper is Windows-only")
		return false
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(w, "helper:     not installed (%v): sessions stream with FFmpeg\n", err)
		return false
	}
	dir := filepath.Dir(self)
	return probeHelper(ctx, cfg, filepath.Join(dir, helperExeName), cfg.helperFFmpegDir(dir), w)
}

// probeHelper is ProbeHelper for the helper exe, whose libavcodec backend
// loads its libraries from ffDir.
func probeHelper(ctx context.Context, cfg *Config, exe, ffDir string, w io.Writer) bool {
	if _, err := os.Stat(exe); err != nil {
		fmt.Fprintf(w, "helper:     not installed (%v): sessions stream with FFmpeg\n", err)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, helperProbeWait)
	defer cancel()
	c, err := qualify.Caps(ctx, qualify.Options{Helper: exe, Backend: "auto", FFmpegDir: ffDir})
	if err != nil {
		fmt.Fprintf(w, "helper:     does not run (%v): sessions stream with FFmpeg\n", err)
		return false
	}
	usable := c.Usable()
	if usable {
		fmt.Fprintf(w, "helper:     %-6s %-14s %s (%s %s)\n", c.Backend, strings.Join(probeCodecs(c), ","), c.AdapterName, helperExeName, c.HelperVersion)
		for _, name := range probeCodecs(c) {
			cc := c.Codecs[name]
			extra := ""
			if cc.HDR10 {
				extra += " hdr10"
			}
			if cc.AlignW > 1 || cc.AlignH > 1 {
				extra += fmt.Sprintf(" align=%dx%d", cc.AlignW, cc.AlignH)
			}
			fmt.Fprintf(w, "            %-5s recovery=%s live-bitrate=%s max=%dx%d%s\n", name, cc.Recovery, cc.LiveBitrate, cc.MaxW, cc.MaxH, extra)
		}
		if c.Backend == backendLavc && !cfg.libavcodecOn() {
			usable = false
			fmt.Fprintln(w, `            not used: host config "helperLibavcodec" is "off": sessions stream with FFmpeg`)
		}
	} else {
		fmt.Fprintln(w, "helper:     no usable encoder: sessions stream with FFmpeg")
	}
	for _, k := range slices.Sorted(maps.Keys(c.Unavailable)) {
		fmt.Fprintf(w, "            unavailable: %s: %s\n", k, c.Unavailable[k])
	}
	return usable
}

// probeCodecs returns the helper's codecs in the order sessions prefer them
// (HEVC, AV1, H.264), then any others.
func probeCodecs(c encoder.Caps) []string {
	var out []string
	for _, name := range []string{"hevc", "av1", "h264"} {
		if _, ok := c.Codecs[name]; ok {
			out = append(out, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Codecs)) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
