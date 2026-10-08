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

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/nut"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
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
	Encoders []EncoderInfo     // usable encoders, best first
	Rejected map[string]string // encoder built into ffmpeg -> why its test encode failed
	options  map[string]map[string]bool

	// VersionInfo is "ffmpeg -version" without the configure line: version
	// (= Version), compiler and library versions.
	VersionInfo []string

	captureClock bool // CaptureClockFilter and a µs encoder time base work
	barcode      bool // BarcodeFilter draws readable frame barcodes

	align map[string]Alignment // encoders that pad the coded picture
}

// Alignment is the coded-size alignment of an encoder that pads pictures to
// a block size: the picture it codes is w x h rounded up to multiples of W x
// H (or larger), and the padding rows and columns reach the decoder's output
// unless the codec can signal a crop. AV1 cannot: RDNA3's AV1 encoder codes
// 1920x1080 as 1920x1082 (AMF docs: 64x16 alignment) and FFmpeg's av1_amf
// reports the crop only as stream side data (AV_PKT_DATA_FRAME_CROPPING),
// which NUT does not carry.
type Alignment struct {
	W, H int
	// ProbeW x ProbeH is the picture the probe encoded and CodedW x CodedH
	// the size the bitstream coded it at.
	ProbeW, ProbeH, CodedW, CodedH int
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
	c := &Caps{FFmpeg: ffmpeg, Filters: map[string]bool{}, Rejected: map[string]string{}, options: map[string]map[string]bool{},
		align: map[string]Alignment{}}
	out, err := quietCmd(ctx, ffmpeg, "-hide_banner", "-version").Output()
	if err != nil {
		return nil, fmt.Errorf("running ffmpeg: %w", err)
	}
	c.Version, c.VersionInfo = parseVersion(out)

	out, _ = quietCmd(ctx, ffmpeg, "-hide_banner", "-filters").Output()
	c.Filters = parseFilters(out)
	if c.Filters["vsrc_amf"] {
		help, _ := quietCmd(ctx, ffmpeg, "-hide_banner", "-h", "filter=vsrc_amf").Output()
		if err := checkAMFCapture(help, c.Filters); err != nil {
			c.Filters["vsrc_amf"] = false
			if log != nil {
				log.Info("AMD Direct Capture (vsrc_amf) unusable", "err", err)
			}
		}
	}
	if c.Filters["settb"] && c.Filters["setpts"] {
		if err := testCaptureClock(ctx, ffmpeg, CaptureClockFilter); err == nil {
			c.captureClock = true
		} else if log != nil {
			log.Info("capture timestamps unavailable", "err", err)
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
	if c.Filters["drawbox"] {
		wg.Add(1)
		go func() { // alongside the encoder tests: no extra start-up time
			defer wg.Done()
			err := testBarcode(ctx, ffmpeg)
			mu.Lock()
			c.barcode = err == nil
			mu.Unlock()
			if err != nil && log != nil {
				log.Info("test pattern frame barcode unavailable", "err", err)
			}
		}()
	}
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
			var al Alignment
			var alErr error
			if err == nil && e.HW && e.Family == "av1" {
				al, alErr = probeAlignment(ctx, ffmpeg, e)
				if alErr != nil && log != nil {
					log.Info("coded size probe failed, assuming no padding", "encoder", e.Name, "err", alErr)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			c.options[e.Name] = opts
			if al.W > 1 || al.H > 1 {
				c.align[e.Name] = al
				if log != nil {
					log.Info("encoder pads the coded picture", "encoder", e.Name,
						"probe", fmt.Sprintf("%dx%d", al.ProbeW, al.ProbeH), "coded", fmt.Sprintf("%dx%d", al.CodedW, al.CodedH),
						"alignment", fmt.Sprintf("%dx%d", al.W, al.H))
				}
			}
			if err == nil {
				ok[e.Name] = true
			} else {
				c.Rejected[e.Name] = err.Error()
				if log != nil {
					log.Debug("encoder unavailable", "encoder", e.Name, "err", err)
				}
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
	args := append(blackFramesArgs(e, 640, 360), "-f", "null", "-")
	var stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	return nil
}

// blackFramesArgs returns the arguments that encode three black w x h frames
// with e, without the output.
func blackFramesArgs(e EncoderInfo, w, h int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if e.Vendor == "vaapi" {
		args = append(args, "-vaapi_device", vaapiDevice())
	}
	args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=30", w, h), "-frames:v", "3")
	if e.Vendor == "vaapi" {
		args = append(args, "-vf", "format=nv12,hwupload")
	} else {
		args = append(args, "-pix_fmt", "yuv420p")
	}
	return append(args, "-c:v", e.Name)
}

// alignProbeW x alignProbeH is the picture probeAlignment encodes: the most
// common desktop size, and one that RDNA3's AV1 encoder pads.
const alignProbeW, alignProbeH = 1920, 1080

// probeAlignment encodes three black 1920x1080 frames with an AV1 encoder and
// reads the coded size from the sequence header: the NUT stream header only
// repeats the configured size. An encoder that codes the picture larger pads
// it; the result is then the documented RDNA3 alignment of 64x16, or a
// coarser one if the measured padding needs it. W, H = 1, 1: no padding.
func probeAlignment(ctx context.Context, ffmpeg string, e EncoderInfo) (Alignment, error) {
	cw, ch, err := codedSize(ctx, ffmpeg, blackFramesArgs(e, alignProbeW, alignProbeH))
	if err != nil {
		return Alignment{W: 1, H: 1}, err
	}
	a := Alignment{W: 1, H: 1, ProbeW: alignProbeW, ProbeH: alignProbeH, CodedW: cw, CodedH: ch}
	if cw != alignProbeW || ch != alignProbeH {
		a.W, a.H = alignmentFor(alignProbeW, cw, 64), alignmentFor(alignProbeH, ch, 16)
	}
	return a, nil
}

// alignmentFor returns the smallest power-of-two multiple of def whose
// rounding up of n reaches the measured coded size (RDNA3 special-cases
// 1080 rows: coded as 1082, not 1088, which def covers).
func alignmentFor(n, coded, def int) int {
	a := def
	for a < 1<<16 && (n+a-1)/a*a < coded {
		a *= 2
	}
	return a
}

// codedSize runs ffmpeg with args (input and encoder, see blackFramesArgs)
// into NUT and returns the coded frame size of the AV1 stream it writes.
func codedSize(ctx context.Context, ffmpeg string, args []string) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, append(args, "-f", "nut", "-write_index", "0", "pipe:1")...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, 0, fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	d := nut.NewDemuxer(&stdout, proto.MaxFrameSize)
	for {
		pkt, err := d.ReadPacket()
		if err != nil {
			break
		}
		st := d.Streams()[pkt.Stream]
		if st == nil || st.Class != nut.ClassVideo {
			continue
		}
		// The sequence header is in the extradata, the key frame, or both.
		if w, h, ok := codec.AV1FrameSize(st.Extradata); ok {
			return w, h, nil
		}
		if w, h, ok := codec.AV1FrameSize(pkt.Data); ok {
			return w, h, nil
		}
	}
	return 0, 0, errors.New("no AV1 sequence header in the encoder output")
}

// testCaptureClock runs the exact capture-clock filter and encoder time base
// BuildArgs uses, so a build that rejects them loses only the capture stamps.
func testCaptureClock(ctx context.Context, ffmpeg, filter string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:r=30", "-frames:v", "1",
		"-vf", filter, "-enc_time_base", "1:1000000", "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	return nil
}

// testBarcode draws BarcodeFilter on three frames, reads them back as raw luma
// and checks that they carry frame indexes 0, 1 and 2.
func testBarcode(ctx context.Context, ffmpeg string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	const w, h, frames = 160, 64, 3
	var stdout, stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=gray:s=%dx%d:r=30", w, h), "-frames:v", strconv.Itoa(frames),
		"-vf", BarcodeFilter(proto.BarcodeCell)+",format=gray", "-f", "rawvideo", "pipe:1")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	b := stdout.Bytes()
	if len(b) != w*h*frames {
		return fmt.Errorf("got %d bytes of raw video, want %d", len(b), w*h*frames)
	}
	for i := 0; i < frames; i++ {
		v, ok := proto.BarcodeReadLuma(b[i*w*h:(i+1)*w*h], w, proto.BarcodeCell)
		if !ok || v != uint16(i) {
			return fmt.Errorf("frame %d: barcode reads %d (valid %v)", i, v, ok)
		}
	}
	return nil
}

// probeFilters are the filters BuildArgs may use.
var probeFilters = []string{"ddagrab", "gfxcapture", "vsrc_amf", "hwmap", "hwdownload", "scale_vaapi", "vpp_qsv", "realtime",
	"testsrc2", "settb", "setpts", "drawbox", "select"}

// parseFilters returns which of probeFilters "ffmpeg -filters" lists.
func parseFilters(out []byte) map[string]bool {
	m := map[string]bool{}
	for _, f := range probeFilters {
		if regexp.MustCompile(`(?m)^\s*\S+\s+` + regexp.QuoteMeta(f) + `\s`).Match(out) {
			m[f] = true
		}
	}
	return m
}

// checkAMFCapture checks that BuildArgs can use vsrc_amf: "ffmpeg -h
// filter=vsrc_amf" (help) lists the options and the capture mode it sets, and
// the build has select for framePacer and settb/setpts for the capture clock,
// which every vsrc_amf chain carries.
func checkAMFCapture(help []byte, filters map[string]bool) error {
	for _, o := range []string{"monitor_index", "framerate", "duplicate_output", "capture_mode", "wait_for_present"} {
		if !regexp.MustCompile(`(?m)^\s+` + o + `\s`).Match(help) {
			return fmt.Errorf("vsrc_amf has no %s", o)
		}
	}
	for _, f := range []string{"select", "settb", "setpts"} {
		if !filters[f] {
			return fmt.Errorf("no %s filter", f)
		}
	}
	return nil
}

var optLine = regexp.MustCompile(`^\s{1,4}-([A-Za-z0-9_\-]+)\s+<`)

// parseVersion returns the first line of "ffmpeg -version" and its lines
// without the (very long) configure line, blank lines and the "Exiting with
// exit code" line FFmpeg 8 prints to stdout after -version.
func parseVersion(out []byte) (first string, info []string) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "configuration:") || strings.HasPrefix(line, "Exiting with exit code") {
			continue
		}
		info = append(info, line)
	}
	if len(info) > 0 {
		first = info[0]
	}
	return first, info
}

func encoderOptions(ctx context.Context, ffmpeg, enc string) map[string]bool {
	out, _ := quietCmd(ctx, ffmpeg, "-hide_banner", "-h", "encoder="+enc).Output()
	return parseEncoderOptions(out)
}

// parseEncoderOptions returns the private AVOptions listed by
// "ffmpeg -h encoder=<name>".
func parseEncoderOptions(out []byte) map[string]bool {
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

// Alignment returns the coded-size alignment the probe measured for an
// encoder (W, H = 1, 1: it codes any size as is).
func (c *Caps) Alignment(enc string) Alignment {
	if a, ok := c.align[enc]; ok {
		return a
	}
	return Alignment{W: 1, H: 1}
}

// SetAlignment records an encoder's coded-size alignment (the probe does;
// tests, and an encoder backend that knows its alignment factors).
func (c *Caps) SetAlignment(enc string, a Alignment) {
	if c.align == nil {
		c.align = map[string]Alignment{}
	}
	c.align[enc] = a
}

// Pads reports whether an encoder pads a w x h picture: w or h is not a
// multiple of its alignment. An unknown size (0) is not checked.
func (c *Caps) Pads(enc string, w, h int) bool {
	a := c.Alignment(enc)
	return w > 0 && h > 0 && (a.W > 1 && w%a.W != 0 || a.H > 1 && h%a.H != 0)
}

// ---------------------------------------------------------------------------
// Argument construction

// Source selects what is captured.
type Source struct {
	Backend  string // ddagrab | gfxcapture | amf | x11grab | test
	Output   int    // ddagrab output_idx / amf monitor_index: DXGI output index on adapter 0
	HMonitor uint64 // gfxcapture monitor handle
	Window   string // gfxcapture window title regex (optional)
	Display  string // x11grab display, e.g. ":0.0"
	X, Y     int    // x11grab offset
	NativeW  int    // native size of the captured surface (x11grab/test; ddagrab and gfxcapture: the monitor's, informational)
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
	// CaptureClock stamps every frame with its wall-clock capture time: pts
	// become the wall clock in µs right after the source (CaptureClockFilter)
	// and the encoder runs at a µs time base (it still gets the frame rate for
	// rate control). Video turns them into Frame.CaptureUs. Capture "amf"
	// gets that pts and time base also without it (BuildArgs), unreported.
	CaptureClock bool
	// Barcode draws the frame barcode of each frame's index (= Frame.Seq) into
	// the top-left corner (BarcodeFilter; test source only).
	Barcode bool
	// TestPad adds that many rows of white below the test pattern and
	// announces them as padding to crop (VideoConfig cropBottom), as for an
	// encoder that pads the coded picture (AV1 on RDNA3). Test source only:
	// tests of the client's crop path without such a GPU.
	TestPad int
}

// OutputSize returns the size of the picture BuildArgs hands the encoder
// for p, or 0, 0 when only the capture knows it (a window).
func (p Params) OutputSize() (w, h int) {
	switch p.Source.Backend {
	case "test":
		w, h = p.Source.NativeW, p.Source.NativeH
		if w == 0 {
			w, h = 1280, 720
		}
		return w, h + p.TestPad
	case "x11grab":
		w, h = p.Source.NativeW, p.Source.NativeH
		if p.Width > 0 && p.Height > 0 && w > 0 && h > 0 {
			// scale=W:H:force_original_aspect_ratio=decrease:force_divisible_by=2,
			// as libavfilter/scale_eval.c computes it (av_rescale rounds to
			// the nearest multiple of 2, then down).
			rescale := func(a, b, c int) int { return (a*b + c/2) / c }
			fw, fh := min(p.Width, rescale(p.Height, w, h*2)*2), min(p.Height, rescale(p.Width, h, w*2)*2)
			return fw &^ 1, fh &^ 1
		}
		return w, h
	case "gfxcapture":
		if p.Width > 0 && p.Height > 0 {
			return p.Width, p.Height // width/height force the frame size
		}
		if p.Source.Window != "" {
			return 0, 0
		}
	}
	return p.Source.NativeW, p.Source.NativeH
}

// CanCaptureAMF reports why AMD Direct Capture (Source.Backend "amf") cannot
// feed enc, or nil: this build needs a usable vsrc_amf (Probe checks its
// options), and its AMF surfaces only go to the AMF encoders.
func (c *Caps) CanCaptureAMF(enc EncoderInfo) error {
	if !c.Filters["vsrc_amf"] {
		return errors.New("this ffmpeg build has no usable vsrc_amf filter (AMD Direct Capture: FFmpeg >= 8.1 with AMF)")
	}
	if !strings.HasSuffix(enc.Name, "_amf") {
		return fmt.Errorf("AMD Direct Capture only feeds AMF encoders, not %s", enc.Name)
	}
	return nil
}

// CaptureClockFilter sets each frame's pts to the wall clock (av_gettime())
// in µs. time(0) replaces setpts' deprecated RTCTIME constant.
const CaptureClockFilter = "settb=AVTB,setpts=time(0)*1000000"

// CanStampCapture reports whether this FFmpeg build ran CaptureClock's filter
// and encoder time base in the probe.
func (c *Caps) CanStampCapture() bool { return c.captureClock }

// CanDrawBarcode reports whether BarcodeFilter produced readable barcodes of
// the frame index in the probe.
func (c *Caps) CanDrawBarcode() bool { return c.barcode }

// BarcodeFilter returns the filter chain that draws the frame barcode
// (proto.BarcodeWord) of the frame index n with cells of cell pixels into the
// top-left corner: a black box, then one white drawbox per cell, enabled while
// its bit is 1. drawbox's timeline expression sees n (the frame index from 0),
// which is the frame's Seq within a generation (one packet per frame, no
// frame drops with -fps_mode passthrough). Cheap per frame: 25 expression
// evaluations and at most 24 16x16 fills.
func BarcodeFilter(cell int) string {
	parts := []string{fmt.Sprintf("drawbox=x=0:y=0:w=%d:h=%d:color=black:t=fill", proto.BarcodeCols*cell, proto.BarcodeRows*cell)}
	crc0 := proto.BarcodeCRC(0)
	for k := 0; k < proto.BarcodeBits; k++ {
		bit := proto.BarcodeCellBit(k)
		var expr string
		if bit >= 8 {
			expr = fmt.Sprintf("bitand(n,%d)", 1<<(bit-8)) // value bit
		} else {
			// The CRC is affine in the value bits: crc(v) = crc(0) xor, over the
			// set bits i of v, crc(1<<i) xor crc(0). A sum mod 2 is the xor.
			terms := []string{strconv.Itoa(int(crc0 >> bit & 1))}
			for i := 0; i < 16; i++ {
				if (proto.BarcodeCRC(1<<i)^crc0)>>bit&1 == 1 {
					terms = append(terms, fmt.Sprintf("gt(bitand(n,%d),0)", 1<<i))
				}
			}
			expr = "mod(" + strings.Join(terms, "+") + ",2)"
		}
		// Quoted: the commas inside the expression must not split the graph.
		parts = append(parts, fmt.Sprintf("drawbox=x=%d:y=%d:w=%d:h=%d:color=white:t=fill:enable='%s'",
			k%proto.BarcodeCols*cell, k/proto.BarcodeCols*cell, cell, cell, expr))
	}
	return strings.Join(parts, ",")
}

// framePacer returns a select filter that passes at most fps frames a second
// on average from a source that delivers frames at its own pace: vsrc_amf in
// wait_for_present mode returns every present of DWM or a fullscreen game,
// whatever its framerate option says (the AMF Display Capture API defines
// that only for keep_framerate mode). The encoder's rate control assumes fps
// frames a second: a 144 Hz display streamed at 60 fps would get 2.4 times
// the bitrate.
//
// The clock is time(0), the wall clock in seconds when the frame arrives.
// ld(0) is the time the next frame is due and ld(1) the clock minus it. A
// frame passes when it is due, and the next one is due one interval later;
// a late frame keeps up to one interval of credit, so a source a little
// faster than fps still yields fps (after a late frame two may pass back to
// back), and one at fps loses no frame to jitter below half an interval. A
// clock that jumped back by more than two intervals (a wall-clock step) passes
// the frame and restarts the schedule instead of stalling the stream. FFmpeg
// evaluates both operands of a binary operator in order (libavutil/eval.c), so
// st(1) is stored before ld(1) reads it; the quotes keep the commas inside the
// filter's argument.
func framePacer(fps int) string {
	return fmt.Sprintf("select='if(gte(st(1,time(0)-ld(0)),0)+lt(ld(1),-2/%[1]d),1+0*st(0,ld(0)+ld(1)+1/%[1]d-clip(ld(1),0,1/%[1]d)),0)'", fps)
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
	gpuFrames := false      // source produces D3D11 frames (amf: AMF surfaces)
	clock := p.CaptureClock // pts = wall-clock capture time in µs
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
			return nil, errors.New("this ffmpeg build lacks the gfxcapture filter (need FFmpeg >= 8.1)")
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
	case "amf":
		// AMD Direct Capture (experimental): vsrc_amf returns AMF surfaces
		// (AV_PIX_FMT_AMF_SURFACE) on its own AMF device, and amfenc encodes
		// them in place on that device: zero copy, no hwmap. duplicate_output
		// hands out a copy of the captured surface, which may be
		// DCC-compressed and then cannot go to the encoder (AMF Display
		// Capture API). The filter has no cursor option.
		if err := c.CanCaptureAMF(e); err != nil {
			return nil, err
		}
		if p.DrawCursor {
			return nil, errors.New("AMD Direct Capture cannot draw the cursor")
		}
		if p.Source.Output < 0 || p.Source.Output > 8 {
			return nil, fmt.Errorf("AMD Direct Capture: monitor index %d out of range 0-8", p.Source.Output)
		}
		chain = fmt.Sprintf("vsrc_amf=monitor_index=%d:framerate=%d:capture_mode=wait_for_present:duplicate_output=1,%s",
			p.Source.Output, p.FPS, framePacer(p.FPS))
		gpuFrames = true
		// vsrc_amf rounds each frame's AMF capture time to its 1/framerate
		// time base, so two frames the pacer passes less than an interval
		// apart can share a pts (the muxer then shifts one with a
		// "Non-monotonic DTS" warning). The wall clock in µs keeps them
		// apart, also when the client gets no capture stamps.
		clock = true
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
	if clock {
		// Evaluated as the frame leaves the source (after realtime pacing for
		// the test source), before any conversion or encoding.
		chain += "," + CaptureClockFilter
	}
	if p.Barcode && p.Source.Backend == "test" {
		// The test pattern is generated at the output size: cells stay
		// BarcodeCell pixels in the encoded picture.
		chain += "," + BarcodeFilter(proto.BarcodeCell)
	}
	if p.TestPad > 0 && p.Source.Backend == "test" {
		chain += fmt.Sprintf(",pad=w=iw:h=ih+%d:x=0:y=0:color=white", p.TestPad)
	}

	// Convert into what the encoder accepts.
	switch {
	case gpuFrames && (e.Vendor == "nvidia" || e.Vendor == "amd"):
		// NVENC and AMF consume D3D11 textures directly, AMF also vsrc_amf's
		// AMF surfaces: zero copy.
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
	// Effectively infinite GOP: IDR only at start / on request. AMF rejects
	// out-of-range values silently (falling back to a ~1 s GOP), and QSV
	// stores the GOP in 16 bits, so clamp per vendor.
	gop := p.FPS * 3600
	switch e.Vendor {
	case "amd":
		gop = 1000
	case "intel":
		gop = min(gop, 65535)
	}
	args = append(args, "-c:v", e.Name)
	args = append(args, c.encoderArgs(p, bufKbits, gop)...)
	if clock {
		// Keep µs precision through the encoder; its frame rate still comes
		// from the source (checked: libx264/libsvtav1 bitrate and fps unchanged).
		args = append(args, "-enc_time_base", "1:1000000")
	}
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
			// rc=2: CBR. FFmpeg's wrapper asks for VBR unless -maxrate equals
			// -b:v; with low-delay prediction (pred-struct=1) SVT-AV1 1.7
			// forces CBR with a warning, but 4.x fails ("VBR Rate control is
			// currently not supported for LOW_DELAY, use CBR mode"). -maxrate
			// = -b:v fails on 1.7 ("Max Bitrate must be greater than Target
			// Bitrate"); rc=2 works on both.
			opt("svtav1-params", "pred-struct=1:lookahead=0:scd=0:rc=2")
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

// causeLines returns FFmpeg's first error lines: the encoder prints its reason
// (e.g. the minimum driver version) before a series of generic lines starting
// with "Error while opening encoder".
func causeLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")), "\n")
	for i, l := range lines {
		if i > 0 && strings.Contains(l, "Error while opening encoder") {
			lines = lines[:i]
			break
		}
	}
	if len(lines) > n {
		lines = lines[:n]
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
