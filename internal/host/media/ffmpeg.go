// Package media runs the capture/encode pipeline (an ffmpeg child process with
// zero-copy GPU capture and hardware encoding) and the audio pipeline.
package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EncoderInfo describes a usable video encoder.
type EncoderInfo struct {
	Name   string `json:"name"`
	Family string `json:"family"` // h264 | hevc | av1
	Vendor string `json:"vendor"` // nvidia | amd | intel | vaapi | software
	HW     bool   `json:"hw"`
}

// Caps is the result of probing the local ffmpeg build and GPU.
type Caps struct {
	FFmpeg   string
	Version  string
	Filters  map[string]bool
	Encoders []EncoderInfo // usable encoders, best first
	options  map[string]map[string]bool
}

// candidate encoders in preference order within a family.
var candidates = []EncoderInfo{
	{"av1_nvenc", "av1", "nvidia", true},
	{"hevc_nvenc", "hevc", "nvidia", true},
	{"h264_nvenc", "h264", "nvidia", true},
	{"av1_amf", "av1", "amd", true},
	{"hevc_amf", "hevc", "amd", true},
	{"h264_amf", "h264", "amd", true},
	{"av1_qsv", "av1", "intel", true},
	{"hevc_qsv", "hevc", "intel", true},
	{"h264_qsv", "h264", "intel", true},
	{"h264_vaapi", "h264", "vaapi", true},
	{"hevc_vaapi", "hevc", "vaapi", true},
	{"av1_vaapi", "av1", "vaapi", true},
	{"libx264", "h264", "software", false},
	{"libsvtav1", "av1", "software", false},
	{"libaom-av1", "av1", "software", false},
}

// FindFFmpeg locates the ffmpeg binary: explicit path, next to the executable,
// ./ffmpeg/bin, then PATH.
func FindFFmpeg(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("ffmpeg not found at %s: %w", explicit, err)
		}
		return explicit, nil
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, p := range []string{filepath.Join(dir, name), filepath.Join(dir, "ffmpeg", "bin", name), filepath.Join(dir, "ffmpeg", name)} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return exec.LookPath(name)
}

func quietCmd(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	hideWindow(cmd)
	return cmd
}

// Probe inspects the ffmpeg build and test-encodes with every candidate encoder.
func Probe(ctx context.Context, ffmpeg string, log *slog.Logger) (*Caps, error) {
	c := &Caps{FFmpeg: ffmpeg, Filters: map[string]bool{}, options: map[string]map[string]bool{}}
	out, err := quietCmd(ctx, ffmpeg, "-hide_banner", "-version").Output()
	if err != nil {
		return nil, fmt.Errorf("running ffmpeg: %w", err)
	}
	c.Version = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])

	out, _ = quietCmd(ctx, ffmpeg, "-hide_banner", "-filters").Output()
	for _, f := range []string{"ddagrab", "gfxcapture", "hwmap", "hwdownload", "scale_vaapi", "vpp_qsv", "realtime", "testsrc2"} {
		if regexp.MustCompile(`(?m)^\s*\S+\s+` + regexp.QuoteMeta(f) + `\s`).Match(out) {
			c.Filters[f] = true
		}
	}
	out, _ = quietCmd(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	available := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 && f[0][0] == 'V' {
			available[f[1]] = true
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	ok := map[string]bool{}
	for _, cand := range candidates {
		if !available[cand.Name] {
			continue
		}
		if cand.Vendor == "vaapi" && runtime.GOOS != "linux" {
			continue
		}
		wg.Add(1)
		go func(e EncoderInfo) {
			defer wg.Done()
			opts := encoderOptions(ctx, ffmpeg, e.Name)
			err := testEncode(ctx, ffmpeg, e)
			mu.Lock()
			defer mu.Unlock()
			c.options[e.Name] = opts
			if err == nil {
				ok[e.Name] = true
			} else if log != nil {
				log.Debug("encoder unavailable", "encoder", e.Name, "err", err)
			}
		}(cand)
	}
	wg.Wait()
	for _, cand := range candidates {
		if ok[cand.Name] {
			c.Encoders = append(c.Encoders, cand)
		}
	}
	if len(c.Encoders) == 0 {
		return c, errors.New("no working video encoder found in ffmpeg")
	}
	return c, nil
}

func testEncode(ctx context.Context, ffmpeg string, e EncoderInfo) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if e.Vendor == "vaapi" {
		args = append(args, "-vaapi_device", vaapiDevice())
	}
	args = append(args, "-f", "lavfi", "-i", "color=c=black:s=640x360:r=30", "-frames:v", "3")
	if e.Vendor == "vaapi" {
		args = append(args, "-vf", "format=nv12,hwupload")
	} else {
		args = append(args, "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:v", e.Name, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, lastLines(stderr.String(), 3))
	}
	return nil
}

var optLine = regexp.MustCompile(`^\s{1,4}-([A-Za-z0-9_\-]+)\s+<`)

func encoderOptions(ctx context.Context, ffmpeg, enc string) map[string]bool {
	out, _ := quietCmd(ctx, ffmpeg, "-hide_banner", "-h", "encoder="+enc).Output()
	m := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if s := optLine.FindStringSubmatch(line); s != nil {
			m[s[1]] = true
		}
	}
	return m
}

func vaapiDevice() string {
	if d := os.Getenv("RECON_VAAPI_DEVICE"); d != "" {
		return d
	}
	return "/dev/dri/renderD128"
}

// Best returns the preferred encoder for a family (hardware first).
func (c *Caps) Best(family string) (EncoderInfo, bool) {
	for _, e := range c.Encoders {
		if e.Family == family {
			return e, true
		}
	}
	return EncoderInfo{}, false
}

// Families returns the families with at least one working encoder.
func (c *Caps) Families() map[string]EncoderInfo {
	m := map[string]EncoderInfo{}
	for _, e := range c.Encoders {
		if _, ok := m[e.Family]; !ok {
			m[e.Family] = e
		}
	}
	return m
}

// HasOption reports whether an encoder exposes a private AVOption.
func (c *Caps) HasOption(enc, opt string) bool {
	return c.options[enc][opt]
}

// ---------------------------------------------------------------------------
// Argument construction

// Source selects what is captured.
type Source struct {
	Backend  string // ddagrab | gfxcapture | x11grab | test
	Output   int    // ddagrab output index (adapter 0)
	HMonitor uint64 // gfxcapture monitor handle
	Window   string // gfxcapture window title regex (optional)
	Display  string // x11grab display, e.g. ":0.0"
	X, Y     int    // x11grab offset
	NativeW  int    // native size of the captured surface (x11grab/test)
	NativeH  int
}

// Params fully describes one encoder generation.
type Params struct {
	Source      Source
	Encoder     EncoderInfo
	Width       int // output size, 0 = native
	Height      int
	FPS         int
	BitrateKbps int
	Quality     string // speed | balanced | quality
	DrawCursor  bool
}

var safeRegex = regexp.MustCompile(`^[A-Za-z0-9 _.\-()*+?^$|\[\]]{1,128}$`)

// escapeFilterValue quotes a value for use inside a filtergraph option.
func escapeFilterValue(s string) string {
	// Level 1 (option value) escaping with single quotes, then level 2 (graph).
	q := "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `[`, `\[`, `]`, `\]`, `,`, `\,`, `;`, `\;`)
	return r.Replace(q)
}

// BuildArgs returns the ffmpeg command line for one encoder generation.
// The output is a NUT stream on stdout.
func (c *Caps) BuildArgs(p Params) ([]string, error) {
	if p.FPS <= 0 {
		p.FPS = 60
	}
	if p.BitrateKbps <= 0 {
		p.BitrateKbps = 20000
	}
	e := p.Encoder
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	gpuFrames := false // source produces D3D11 frames
	var chain string
	cursor := "0"
	if p.DrawCursor {
		cursor = "1"
	}
	switch p.Source.Backend {
	case "ddagrab":
		if !c.Filters["ddagrab"] {
			return nil, errors.New("this ffmpeg build lacks the ddagrab filter (need FFmpeg >= 6.1 for Windows)")
		}
		chain = fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=%s:dup_frames=0", p.Source.Output, p.FPS, cursor)
		gpuFrames = true
	case "gfxcapture":
		if !c.Filters["gfxcapture"] {
			return nil, errors.New("this ffmpeg build lacks the gfxcapture filter (need FFmpeg >= 8.0)")
		}
		opts := []string{fmt.Sprintf("max_framerate=%d", p.FPS), "capture_cursor=" + cursor}
		if p.Source.Window != "" {
			if !safeRegex.MatchString(p.Source.Window) {
				return nil, errors.New("invalid window pattern")
			}
			opts = append(opts, "window_title="+escapeFilterValue(p.Source.Window))
		} else {
			opts = append(opts, fmt.Sprintf("hmonitor=%d", p.Source.HMonitor))
		}
		if p.Width > 0 && p.Height > 0 {
			opts = append(opts, fmt.Sprintf("width=%d", p.Width), fmt.Sprintf("height=%d", p.Height), "resize_mode=scale_aspect", "scale_mode=bilinear")
		}
		chain = "gfxcapture=" + strings.Join(opts, ":")
		gpuFrames = true
	case "x11grab":
		args = append(args, "-f", "x11grab", "-framerate", strconv.Itoa(p.FPS), "-draw_mouse", cursor)
		if p.Source.NativeW > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", p.Source.NativeW, p.Source.NativeH))
		}
		args = append(args, "-i", fmt.Sprintf("%s+%d,%d", p.Source.Display, p.Source.X, p.Source.Y))
		chain = "[0:v]null"
	case "test":
		w, h := p.Source.NativeW, p.Source.NativeH
		if w == 0 {
			w, h = 1280, 720
		}
		chain = fmt.Sprintf("testsrc2=s=%dx%d:r=%d", w, h, p.FPS)
		if c.Filters["realtime"] {
			chain += ",realtime"
		}
	default:
		return nil, fmt.Errorf("unknown capture backend %q", p.Source.Backend)
	}

	// Convert into what the encoder accepts.
	switch {
	case gpuFrames && (e.Vendor == "nvidia" || e.Vendor == "amd"):
		// NVENC and AMF consume D3D11 textures directly: zero copy.
	case gpuFrames && e.Vendor == "intel" && c.Filters["vpp_qsv"]:
		// QSV turns BGRA input into 4:4:4 HEVC, which browsers cannot decode:
		// convert to NV12 on the GPU first.
		chain += ",hwmap=derive_device=qsv,format=qsv,vpp_qsv=format=nv12"
	case gpuFrames:
		chain += ",hwdownload,format=bgra,format=yuv420p"
	case e.Vendor == "vaapi":
		args = append([]string{"-vaapi_device", vaapiDevice()}, args...)
		chain += ",format=nv12,hwupload"
	default:
		if p.Width > 0 && p.Height > 0 && !gpuFrames {
			chain += fmt.Sprintf(",scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", p.Width, p.Height)
		}
		chain += ",format=yuv420p"
	}
	if p.Width > 0 && p.Height > 0 && e.Vendor == "vaapi" {
		chain += fmt.Sprintf(",scale_vaapi=w=%d:h=%d", p.Width, p.Height)
	}
	args = append(args, "-filter_complex", chain+"[v]", "-map", "[v]")

	// Rate control: CBR with a VBV of ~1-3 frames so no frame takes much longer
	// than one frame interval to transmit.
	vbvFrames := 1.5
	switch p.Quality {
	case "speed":
		vbvFrames = 1
	case "quality":
		vbvFrames = 3
	}
	bufKbits := int(float64(p.BitrateKbps) * vbvFrames / float64(p.FPS))
	if bufKbits < 64 {
		bufKbits = 64
	}
	gop := p.FPS * 3600 // effectively infinite: IDR only at start / on request
	args = append(args, "-c:v", e.Name)
	args = append(args, c.encoderArgs(p, bufKbits, gop)...)
	args = append(args,
		"-fps_mode", "passthrough",
		"-an", "-sn", "-dn",
		"-flush_packets", "1",
		"-f", "nut", "-write_index", "0", "pipe:1")
	return args, nil
}

func (c *Caps) encoderArgs(p Params, bufKbits, gop int) []string {
	e := p.Encoder
	var a []string
	opt := func(name string, vals ...string) {
		if c.HasOption(e.Name, name) {
			a = append(a, "-"+name)
			a = append(a, vals...)
		}
	}
	br := strconv.Itoa(p.BitrateKbps) + "k"
	common := []string{"-b:v", br, "-maxrate", br, "-bufsize", strconv.Itoa(bufKbits) + "k", "-g", strconv.Itoa(gop), "-bf", "0"}
	switch e.Vendor {
	case "nvidia":
		preset := map[string]string{"speed": "p1", "balanced": "p3", "quality": "p5"}[p.Quality]
		if preset == "" {
			preset = "p3"
		}
		opt("preset", preset)
		opt("tune", "ull")
		opt("rc", "cbr")
		opt("multipass", "disabled")
		opt("zerolatency", "1")
		opt("delay", "0")
		opt("rc-lookahead", "0")
		opt("no-scenecut", "1")
		opt("forced-idr", "1")
		opt("strict_gop", "1")
		if p.Quality != "speed" {
			opt("spatial-aq", "1")
		}
		if e.Family == "h264" {
			opt("profile", "high")
		} else if e.Family == "hevc" {
			opt("profile", "main")
		}
		a = append(a, common...)
	case "amd":
		opt("usage", "ultralowlatency")
		q := map[string]string{"speed": "speed", "balanced": "balanced", "quality": "quality"}[p.Quality]
		if q == "" {
			q = "balanced"
		}
		opt("quality", q)
		opt("rc", "cbr")
		opt("enforce_hrd", "1")
		opt("filler_data", "0")
		opt("preanalysis", "0")
		opt("latency", "1")
		opt("header_insertion_mode", "idr")
		a = append(a, common...)
	case "intel":
		preset := map[string]string{"speed": "veryfast", "balanced": "faster", "quality": "medium"}[p.Quality]
		if preset == "" {
			preset = "faster"
		}
		opt("preset", preset)
		opt("low_delay_brc", "1")
		opt("async_depth", "1")
		opt("look_ahead", "0")
		opt("forced_idr", "1")
		a = append(a, common...)
	case "vaapi":
		opt("rc_mode", "CBR")
		a = append(a, common...)
	default:
		switch e.Name {
		case "libx264":
			preset := map[string]string{"speed": "ultrafast", "balanced": "superfast", "quality": "veryfast"}[p.Quality]
			if preset == "" {
				preset = "superfast"
			}
			opt("preset", preset)
			opt("tune", "zerolatency")
			a = append(a, common...)
		case "libsvtav1":
			opt("preset", "12")
			opt("svtav1-params", "pred-struct=1:lookahead=0:scd=0")
			a = append(a, "-b:v", br, "-g", strconv.Itoa(gop))
		case "libaom-av1":
			opt("usage", "realtime")
			opt("cpu-used", "8")
			opt("lag-in-frames", "0")
			opt("row-mt", "1")
			opt("aq-mode", "0")
			a = append(a, "-b:v", br, "-g", strconv.Itoa(gop))
		default:
			a = append(a, common...)
		}
	}
	return a
}

// ---------------------------------------------------------------------------

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// stderrRing keeps the last lines of a child's stderr for diagnostics.
type stderrRing struct {
	mu    sync.Mutex
	lines []string
	log   *slog.Logger
}

func (r *stderrRing) consume(rd *bufio.Reader) {
	for {
		line, err := rd.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			r.mu.Lock()
			r.lines = append(r.lines, line)
			if len(r.lines) > 20 {
				r.lines = r.lines[len(r.lines)-20:]
			}
			r.mu.Unlock()
			if r.log != nil {
				r.log.Debug("ffmpeg", "line", line)
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *stderrRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, " | ")
}

// SortFamilies orders families by preference for "auto" codec selection.
func SortFamilies(fams []string, order []string) []string {
	rank := map[string]int{}
	for i, f := range order {
		rank[f] = i
	}
	sort.SliceStable(fams, func(i, j int) bool { return rank[fams[i]] < rank[fams[j]] })
	return fams
}
