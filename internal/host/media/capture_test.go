package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// userReport is the notice of the first hardware report (a Proxmox VM
// without a GPU) as the session sent it: "encoder <name> exited: " and the
// stderr ring, cut at 160 characters.
const userReport = "encoder libx264 exited: [Parsed_ddagrab_0 @ 000002bda68effc0] Failed to create D3D11VA device. | " +
	"[Parsed_ddagrab_0 @ 000002bda68effc0] Failed to configure outpu"

// TestClassifyCapture reads capture failures from real FFmpeg 8.1 stderr
// (BtbN win64, recorded under Wine, which like a GPU-less VM has no
// Direct3D 11 video device), the user's report, and vsrc_ddagrab.c's and
// gdigrab.c's other messages as they appear in a session's stderr ring.
func TestClassifyCapture(t *testing.T) {
	ring := func(file string) string {
		f, err := os.Open(filepath.Join("testdata", "ffmpeg81-stderr-"+file+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := &stderrRing{}
		r.consume(bufio.NewReader(f))
		return r.String()
	}
	// FFmpeg's line says these best (failureNotice): no sentence.
	noText := map[string]bool{"DXGI_ERROR_INVALID_CALL": true, "E_OUTOFMEMORY": true}
	for _, c := range []struct {
		name, stderr string
		fault        CaptureFault
		line         string
	}{
		// The user's case, reproduced under Wine with the user's encoder.
		{"ddagrab libx264 (Wine)", ring("ddagrab-libx264"), CaptureNoDevice, "Failed to create D3D11VA device."},
		// With D3D11CreateDevice's own line first (887a0004: DXGI_ERROR_UNSUPPORTED).
		{"ddagrab hevc_amf (Wine)", ring("ddagrab-hevc_amf"), CaptureNoDevice, "Failed to create D3D11VA device."},
		{"the user's notice, cut", userReport, CaptureNoDevice, "Failed to create D3D11VA device."},
		{"gfxcapture", "[Parsed_gfxcapture_0 @ 000001c4] Failed to create D3D11VA device.", CaptureNoDevice, "Failed to create D3D11VA device."},
		// Desktop Duplication refusing the display.
		{"DXGI_ERROR_UNSUPPORTED", "[Parsed_ddagrab_0 @ 0000020a] Selected output not supported | [Parsed_ddagrab_0 @ 0000020a] Failed to configure output pad on Parsed_ddagrab_0",
			CaptureUnsupported, "Selected output not supported"},
		{"no such output", "[Parsed_ddagrab_0 @ 0000020a] Failed to enumerate DXGI output 1", CaptureUnsupported, "Failed to enumerate DXGI output 1"},
		{"E_INVALIDARG", "[Parsed_ddagrab_0 @ 0000020a] Invalid output duplication argument", CaptureUnsupported, "Invalid output duplication argument"},
		// What a session gets past: the secure desktop, a mode change, a
		// full-screen switch, a resolution change, a busy DDA, GDI without
		// a desktop.
		{"E_ACCESSDENIED (UAC, lock screen)", "[Parsed_ddagrab_0 @ 0000020a] Desktop duplication access denied | [Parsed_ddagrab_0 @ 0000020a] Failed to configure output pad on Parsed_ddagrab_0",
			CaptureTransient, "Desktop duplication access denied"},
		{"DXGI_ERROR_ACCESS_LOST", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0026 | [fc#0 @ 0000021b] Error while filtering: External library error",
			CaptureTransient, "AcquireNextFrame failed: 887a0026"},
		// A driver reset, and AcquireNextFrame codes without a sentence.
		{"DXGI_ERROR_DEVICE_REMOVED", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0005", CaptureTransient, "AcquireNextFrame failed: 887a0005"},
		{"DXGI_ERROR_DEVICE_HUNG", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0006", CaptureTransient, "AcquireNextFrame failed: 887a0006"},
		{"DXGI_ERROR_DEVICE_RESET", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0007", CaptureTransient, "AcquireNextFrame failed: 887a0007"},
		{"DXGI_ERROR_INVALID_CALL", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 887a0001", CaptureTransient, "AcquireNextFrame failed: 887a0001"},
		{"E_OUTOFMEMORY", "[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: 8007000e", CaptureTransient, "AcquireNextFrame failed: 8007000e"},
		{"mode change", "[Parsed_ddagrab_0 @ 0000020a] Output parameters changed!", CaptureTransient, "Output parameters changed!"},
		{"four duplications", "[Parsed_ddagrab_0 @ 0000020a] Too many open duplication sessions", CaptureTransient, "Too many open duplication sessions"},
		{"session disconnected", "[Parsed_ddagrab_0 @ 0000020a] Failed duplicating output", CaptureTransient, "Failed duplicating output"},
		{"GDI without a desktop", "[in#0/gdigrab @ 0000020a] Failed to capture image (error 6)", CaptureTransient, "Failed to capture image (error 6)"},
		// gdigrab refusing an area outside the desktop it measured (real,
		// Wine: its message has no line end).
		{"gdigrab area", ring("gdigrab-outside"), CaptureFaultNone,
			"Capture area (1280,0),(2560,720) extends outside window area (0,0),(1280,720)Error opening input: I/O error"},
		// Not a capture source's line.
		{"encoder", ring("hevc_amf-open"), CaptureFaultNone, ""},
		{"test source", ring("source-libx264"), CaptureFaultNone, ""},
		{"another filter", "[hwupload @ 0000020a] Failed to create D3D11VA device.", CaptureFaultNone, ""},
		{"no log context", "Failed to create D3D11VA device.", CaptureFaultNone, ""},
		{"empty", "", CaptureFaultNone, ""},
	} {
		got := ClassifyCapture(c.stderr)
		if got.Fault != c.fault || got.Line != c.line || (got.Line != "" && !noText[c.name]) != (got.Text != "") {
			t.Errorf("%s: %+v, want fault %d line %q", c.name, got, c.fault, c.line)
		}
		if strings.ContainsAny(got.Text, "()") || strings.HasSuffix(got.Text, ".") {
			t.Errorf("%s: text %q has parentheses or a final stop (notices embed it)", c.name, got.Text)
		}
	}
	if got := ClassifyCapture(userReport).Text; got != "no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset" {
		t.Errorf("the user's case reads %q", got)
	}
	// Only ACCESS_LOST is a mode change; a driver reset reads as one.
	for hr, want := range map[string]string{
		"887a0026": "Desktop Duplication lost the display: a display mode change, or a full-screen program took it over",
		"887a0005": "the graphics driver was reset or updated, or stopped responding",
		"887a0007": "the graphics driver was reset or updated, or stopped responding",
	} {
		if got := ClassifyCapture("[Parsed_ddagrab_0 @ 0000020a] AcquireNextFrame failed: " + hr).Text; got != want {
			t.Errorf("AcquireNextFrame %s reads %q, want %q", hr, got, want)
		}
	}
}

// TestPlainFFmpegError drops FFmpeg's log contexts, nested ones too.
func TestPlainFFmpegError(t *testing.T) {
	for in, want := range map[string]string{
		userReport: "encoder libx264 exited: Failed to create D3D11VA device. | Failed to configure outpu",
		"[vost#0:0/libx264 @ 00007fffff36e2c0] [enc:libx264 @ 00007fffff36e9c0] Could not open encoder before EOF": "Could not open encoder before EOF",
		"[out#0/nut @ 0x55d0a1] Nothing was written":                                                               "Nothing was written",
		"Error opening input files: I/O error":                                                                     "Error opening input files: I/O error",
		"[a b] not a context":                                                                                      "[a b] not a context",
	} {
		if got := PlainFFmpegError(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestParseDevices reads "ffmpeg -devices" of FFmpeg 8.1's Windows build
// (real output, CRLF) and a Linux build's.
func TestParseDevices(t *testing.T) {
	out, err := os.ReadFile("testdata/ffmpeg81-devices.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d := parseDevices(out); !d["gdigrab"] {
		t.Fatalf("Windows build: %v", d)
	}
	linux := "Devices:\n D. = Demuxing supported\n .E = Muxing supported\n ---\n DE alsa            ALSA audio output\n" +
		" D  lavfi           Libavfilter virtual input device\n D  x11grab         X11 screen capture, using XCB\n"
	if d := parseDevices([]byte(linux)); d["gdigrab"] {
		t.Fatalf("Linux build: %v", d)
	}
	// An output-only device of that name would not do.
	if d := parseDevices([]byte(" .E gdigrab  output\n")); d["gdigrab"] {
		t.Fatal("muxing-only device taken for an input")
	}
}

// gdiCaps is the Windows FFmpeg 8.1 build's caps with every encoder (the
// probe's real option lists), as on a PC whose GPU capture failed.
func gdiCaps(t *testing.T) *Caps {
	c := caps81(t)
	c.SetGPUCaptureBlocked("no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset")
	return c
}

// TestGDIGrabArgs checks gdigrab's command line: exactly the monitor's area
// (offsets, also negative, and size), the stream's frame rate, the cursor,
// no fps probing; the capture stamp right after the source (Phase 0); CPU
// scaling to the stream's size in the conversion's swscale pass; yuv420p for
// the software encoders, NV12 (system memory) for NVENC, AMF and QSV, never
// a hwdownload or a hwmap; and refusals without gdigrab or an area.
func TestGDIGrabArgs(t *testing.T) {
	c := gdiCaps(t)
	enc := map[string]EncoderInfo{}
	for _, e := range c.Encoders {
		enc[e.Name] = e
	}
	enc["h264_qsv"] = EncoderInfo{Name: "h264_qsv", Family: "h264", Vendor: "intel", HW: true}
	base := Params{Source: Source{Backend: "gdigrab", X: -1920, Y: 120, NativeW: 1920, NativeH: 1080}, FPS: 60, BitrateKbps: 8000,
		Quality: "balanced", CaptureClock: true}
	for _, tc := range []struct {
		enc     string
		w, h    int
		cursor  bool
		convert string // the chain after the capture stamp
	}{
		{"libx264", 0, 0, false, ",format=yuv420p"},
		{"libsvtav1", 1280, 720, true, ",scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p"},
		{"hevc_nvenc", 0, 0, false, ",format=nv12"},
		{"av1_amf", 1280, 720, false, ",scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2,format=nv12"},
		{"h264_qsv", 0, 0, true, ",format=nv12"},
	} {
		p := base
		p.Encoder, p.Width, p.Height, p.DrawCursor = enc[tc.enc], tc.w, tc.h, tc.cursor
		args, err := c.BuildArgs(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.enc, err)
		}
		cursor := map[bool]string{true: "1", false: "0"}[tc.cursor]
		// The capture stamps are gdigrab's own (its pts: the wall clock in
		// µs at the grab), kept with -copyts: they count FFmpeg's queues.
		in := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-copyts", "-fpsprobesize", "0", "-f", "gdigrab", "-framerate", "60",
			"-draw_mouse", cursor, "-offset_x", "-1920", "-offset_y", "120", "-video_size", "1920x1080", "-i", "desktop",
			"-filter_complex", "[0:v]settb=AVTB" + tc.convert + "[v]", "-map", "[v]", "-c:v", tc.enc}
		if !slices.Equal(args[:len(in)], in) {
			t.Errorf("%s:\n got %q\nwant %q", tc.enc, args[:min(len(args), len(in))], in)
		}
		j := strings.Join(args, " ")
		if strings.Contains(j, "hwdownload") || strings.Contains(j, "hwmap") || !strings.Contains(j, "-enc_time_base 1:1000000") {
			t.Errorf("%s: %s", tc.enc, j)
		}
		if w, h := p.OutputSize(); tc.w == 0 && (w != 1920 || h != 1080) || tc.w > 0 && (w != 1280 || h != 720) {
			t.Errorf("%s: OutputSize %dx%d", tc.enc, w, h)
		}
	}
	// Without capture stamps (captureTimestamps "off"): no wall-clock pts.
	p := base
	p.Encoder, p.CaptureClock = enc["libx264"], false
	if args, err := c.BuildArgs(p); err != nil || !strings.Contains(strings.Join(args, " "), `[0:v]null,format=yuv420p[v]`) ||
		strings.Contains(strings.Join(args, " "), "enc_time_base") || slices.Contains(args, "-copyts") {
		t.Fatalf("no capture stamps: %q %v", args, err)
	}
	// A build without gdigrab, or no area to capture.
	c.Devices["gdigrab"] = false
	if _, err := c.BuildArgs(base); err == nil || !strings.Contains(err.Error(), "lacks the gdigrab input device") {
		t.Fatalf("no gdigrab: %v", err)
	}
	c.Devices["gdigrab"] = true
	p = base
	p.Encoder, p.Source.NativeW = enc["libx264"], 0
	if _, err := c.BuildArgs(p); err == nil {
		t.Fatal("gdigrab of no area accepted")
	}
	if s := describeSample(base); s != "gdigrab (GDI) of the 1920x1080 monitor at (-1920,120), 60 fps, 8 Mbit/s, quality balanced, capture timestamps" {
		t.Fatalf("describeSample: %q", s)
	}
}

// TestGDIGrabChainRuns runs gdigrab's filter chain and encoder arguments
// through this machine's FFmpeg (Linux: no gdigrab, so a testsrc2 of the
// monitor's size in bgra, gdigrab's 32-bit desktop format, stands in for
// it): the stream comes out at the size OutputSize says, in the pixel format
// the encoder was given, with wall-clock µs timestamps.
func TestGDIGrabChainRuns(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	caps, err := Probe(context.Background(), ff, nil)
	if err != nil {
		t.Skipf("probe: %v", err)
	}
	enc, ok := caps.Best("h264")
	if !ok || enc.HW {
		t.Skip("no software h264 encoder")
	}
	caps.Devices = map[string]bool{"gdigrab": true}
	for _, sz := range [][4]int{{1366, 768, 0, 0}, {2560, 1440, 1280, 720}, {1920, 1200, 1280, 720}} {
		p := Params{Source: Source{Backend: "gdigrab", X: 0, Y: 0, NativeW: sz[0], NativeH: sz[1]}, Width: sz[2], Height: sz[3],
			Encoder: enc, FPS: 30, BitrateKbps: 4000, CaptureClock: caps.CanStampCapture()}
		args, err := caps.BuildArgs(p)
		if err != nil {
			t.Fatal(err)
		}
		i, j := slices.Index(args, "-fpsprobesize"), slices.Index(args, "desktop")
		if i < 0 || j < i {
			t.Fatalf("no gdigrab input in %q", args)
		}
		// gdigrab's pts are the wall clock in µs at each grab: the stand-in's
		// too (its own time base and stamps), which -copyts keeps.
		src := []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30,format=bgra,%s", sz[0], sz[1], CaptureClockFilter), "-frames:v", "5"}
		args = append(append(slices.Clone(args[:i]), src...), args[j+1:]...)
		out := filepath.Join(t.TempDir(), "gdi.nut")
		args[len(args)-1] = out
		if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, b)
		}
		b, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "stream=width,height,pix_fmt:packet=pts",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatal(err)
		}
		w, h := p.OutputSize()
		lines := strings.Fields(string(b))
		if !slices.Contains(lines, fmt.Sprintf("%d,%d,yuv420p", w, h)) {
			t.Errorf("%dx%d into %dx%d: ffprobe %q, want %dx%d yuv420p", sz[0], sz[1], sz[2], sz[3], lines, w, h)
		}
		if p.CaptureClock {
			now := time.Now().UnixMicro()
			var pts int64
			if _, err := fmt.Sscan(lines[0], &pts); err != nil || pts < now-60_000_000 || pts > now {
				t.Errorf("first pts %q is not the wall clock in µs (now %d)", lines[0], now)
			}
		}
	}
}

// TestCheckGPUCapture runs the GPU capture check against stand-ins for
// ffmpeg: Wine's real output (no Direct3D 11 video device) is a finding, a
// secure desktop or a timeout is not, nor is a capture that opens.
func TestCheckGPUCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts")
	}
	dir := t.TempDir()
	wine, err := filepath.Abs("testdata/ffmpeg81-stderr-ddagrab-libx264.txt")
	if err != nil {
		t.Fatal(err)
	}
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	args := filepath.Join(dir, "args")
	for _, c := range []struct {
		name, body      string
		blocked, detail string
	}{
		{"no device", "echo \"$@\" > " + args + "; cat " + wine + " >&2; exit 171", "no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset",
			"Failed to create D3D11VA device."},
		{"secure desktop", "echo '[Parsed_ddagrab_0 @ 0000020a] Desktop duplication access denied' >&2; exit 1", "",
			"Desktop duplication access denied"},
		{"other", "echo 'something else' >&2; exit 1", "", "exit status 1: something else"},
		{"opened", "exit 0", "", ""},
	} {
		blocked, detail := CheckGPUCapture(context.Background(), script(c.name, c.body))
		if blocked != c.blocked || detail != c.detail {
			t.Errorf("%s: %q, %q; want %q, %q", c.name, blocked, detail, c.blocked, c.detail)
		}
	}
	b, err := os.ReadFile(args)
	if err != nil || strings.TrimSpace(string(b)) != "-hide_banner -loglevel error -nostdin -filter_complex ddagrab=output_idx=0:framerate=10:draw_mouse=0 -frames:v 1 -f null -" {
		t.Fatalf("check command %q %v", b, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if blocked, detail := CheckGPUCapture(ctx, script("hangs", "exec sleep 30")); blocked != "" || !strings.HasPrefix(detail, "no frame within") ||
		time.Since(start) > 5*time.Second { // the caller's deadline, not gpuCaptureTimeout
		t.Fatalf("a check that hangs: %q, %q after %v", blocked, detail, time.Since(start))
	}
}

// TestCheckDDA opens ddagrab on the output asked for (a session's display):
// it opens, or Desktop Duplication refuses it with the failure classified.
func TestCheckDDA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts")
	}
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"$@\" > "+args+"\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok, f, detail := CheckDDA(context.Background(), script("opens", "exit 0"), 2)
	b, _ := os.ReadFile(args)
	if !ok || f.Fault != CaptureFaultNone || detail != "" || !strings.Contains(string(b), "-filter_complex ddagrab=output_idx=2:framerate=10:draw_mouse=0 -frames:v 1") {
		t.Fatalf("opens: %v %+v %q, command %q", ok, f, detail, b)
	}
	ok, f, detail = CheckDDA(context.Background(), script("refused", "echo '[Parsed_ddagrab_0 @ 0000020a] Selected output not supported' >&2; exit 1"), 1)
	if ok || f.Fault != CaptureUnsupported || detail != "Selected output not supported" {
		t.Fatalf("refused: %v %+v %q", ok, f, detail)
	}
}

// TestProbeGPUCaptureInBackground: the probe does not wait for its GPU
// capture check beyond its encoder tests (a ddagrab that waits for a first
// frame must not delay the agent's start); the finding follows, and
// GPUCaptureDone says when. The report waits for it.
func TestProbeGPUCaptureInBackground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	wine, err := filepath.Abs("testdata/ffmpeg81-stderr-ddagrab-libx264.txt")
	if err != nil {
		t.Fatal(err)
	}
	ff := filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
case "$*" in
*ddagrab=output_idx*) sleep 1.5; cat ` + wine + ` >&2; exit 171 ;;
*-version*) echo "ffmpeg version n8.1.3 Copyright (c) 2000-2026" ;;
*-filters*) echo " ... ddagrab           |->V       Grab Windows Desktop images using Desktop Duplication API" ;;
*-devices*) echo " D  gdigrab         GDI API Windows frame grabber" ;;
*-encoders*) echo " V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)" ;;
esac
exit 0
`
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c, err := Probe(context.Background(), ff, nil)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.GPUCaptureDone():
		t.Fatalf("the probe waited %v for its GPU capture check", took)
	default:
	}
	if took > 1200*time.Millisecond || c.GPUCaptureBlocked() != "" || !c.CanGDIGrab() || len(c.Encoders) != 1 {
		t.Fatalf("probe after %v: finding %q, gdigrab %v, encoders %v", took, c.GPUCaptureBlocked(), c.CanGDIGrab(), c.Encoders)
	}
	var out bytes.Buffer
	c.WriteReport(&out, func(e EncoderInfo) Params {
		return Params{Encoder: e, FPS: 60, BitrateKbps: 30000, Source: Source{Backend: "gdigrab", NativeW: 1280, NativeH: 720}}
	})
	if c.GPUCaptureBlocked() != "no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset" ||
		!strings.Contains(out.String(), "capture:    ddagrab unavailable (no Direct3D 11 video device") {
		t.Fatalf("after the check: %q; report:\n%s", c.GPUCaptureBlocked(), out.String())
	}
	t.Logf("probe %v, check %v", took.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
}

// TestWriteReportGDI: on a PC without GPU capture, "recon-host probe" (and
// the installer's "Detected capabilities") says so and that sessions use
// gdigrab, and prints gdigrab's command line under each encoder.
func TestWriteReportGDI(t *testing.T) {
	c := gdiCaps(t)
	sample := func(e EncoderInfo) Params {
		return Params{Source: Source{Backend: "gdigrab", NativeW: 1280, NativeH: 800}, Encoder: e, FPS: 60, BitrateKbps: 30000,
			Quality: "balanced", Adaptive: true, CaptureClock: true}
	}
	var buf bytes.Buffer
	c.WriteReport(&buf, sample)
	out := buf.String()
	t.Logf("report:\n%s", out)
	for _, s := range []string{
		"capture:    ddagrab=true gfxcapture=true vsrc_amf=true gdigrab=true\n",
		"capture:    ddagrab unavailable (no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset): using gdigrab (GDI: more CPU and latency)\n",
		"session:    gdigrab (GDI) of the 1280x800 monitor at (0,0), 60 fps, 30 Mbit/s, quality balanced, adaptive bitrate, capture timestamps (command lines below)\n",
		"encoder:    libx264      h264  software\n            ffmpeg -hide_banner -loglevel warning -nostdin -copyts -fpsprobesize 0 -f gdigrab -framerate 60 -draw_mouse 0 -offset_x 0 -offset_y 0 -video_size 1280x800 -i desktop -filter_complex \"[0:v]settb=AVTB,format=yuv420p[v]\" -map \"[v]\" -c:v libx264 ",
		"encoder:    hevc_nvenc   hevc  nvidia\n            ffmpeg -hide_banner -loglevel warning -nostdin -copyts -fpsprobesize 0 -f gdigrab ",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q", s)
		}
	}
	c.Devices["gdigrab"] = false
	buf.Reset()
	c.WriteReport(&buf, sample)
	if !strings.Contains(buf.String(), "capture:    ddagrab unavailable (no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset): no gdigrab in this ffmpeg either: sessions cannot capture the screen\n") ||
		!strings.Contains(buf.String(), "no command line: this ffmpeg build lacks the gdigrab input device") {
		t.Fatalf("report without gdigrab:\n%s", buf.String())
	}
}

// TestGPUCaptureVideoDevice: where the Direct3D 11 video device FFmpeg's GPU
// capture opens first cannot be opened (a PC or VM without a GPU), the
// probe has its finding when it returns (the first session captures with
// GDI from its start, not after a failed ddagrab generation), and the
// re-check finds it without starting FFmpeg's capture; where it opens, the
// ddagrab test decides as before.
func TestGPUCaptureVideoDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ddagrab-ran")
	ff := filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
case "$*" in
*ddagrab=output_idx*) touch ` + marker + `; sleep 1.5; exit 0 ;;
*-version*) echo "ffmpeg version n8.1.3 Copyright (c) 2000-2026" ;;
*-filters*) echo " ... ddagrab           |->V       Grab Windows Desktop images using Desktop Duplication API" ;;
*-devices*) echo " D  gdigrab         GDI API Windows frame grabber" ;;
*-encoders*) echo " V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)" ;;
esac
exit 0
`
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(f func() error) { videoDevice = f }(videoDevice)
	videoDevice = func() error { return fmt.Errorf("ID3D11VideoDevice: HRESULT 0x80004002") }
	c, err := Probe(context.Background(), ff, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.GPUCaptureDone():
	default:
		t.Fatal("no finding when the probe returned")
	}
	if c.GPUCaptureBlocked() != NoVideoDevice {
		t.Fatalf("finding %q", c.GPUCaptureBlocked())
	}
	if blocked, detail := CheckGPUCapture(context.Background(), ff); blocked != NoVideoDevice || detail != "ID3D11VideoDevice: HRESULT 0x80004002" {
		t.Fatalf("re-check: %q, %q", blocked, detail)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("FFmpeg's ddagrab test ran")
	}
	videoDevice = func() error { return nil } // a GPU: FFmpeg's test decides, in the background
	if c, err = Probe(context.Background(), ff, nil); err != nil {
		t.Fatal(err)
	}
	<-c.GPUCaptureDone()
	if _, err := os.Stat(marker); err != nil || c.GPUCaptureBlocked() != "" {
		t.Fatalf("with a video device: ddagrab test %v, finding %q", err, c.GPUCaptureBlocked())
	}
}

// TestBenchSoftware measures libx264 as the agent does on a PC without a
// GPU encoder (this machine's FFmpeg; under taskset to two CPUs it is the
// 2-vCPU VM of the second hardware report's question): a frame rate at each
// preset, ultrafast not slower than superfast by much.
func TestBenchSoftware(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := BenchSoftware(context.Background(), ff, EncoderInfo{Name: "h264_amf", Family: "h264", HW: true}, 64, 64); err == nil {
		t.Fatal("a hardware encoder was measured")
	}
	b, err := BenchSoftware(context.Background(), ff, EncoderInfo{Name: "libx264", Family: "h264", Vendor: "software"}, 1280, 720)
	if err != nil {
		t.Skipf("libx264: %v", err)
	}
	t.Logf("libx264 1280x720: superfast %.0f fps, ultrafast %.0f fps (%v)", b.Superfast, b.Ultrafast, b.Took.Round(time.Millisecond))
	if b.Superfast <= 0 || b.Ultrafast <= 0 || b.Took > 2*(benchLimit+time.Second) {
		t.Fatalf("bench %+v", b)
	}
}
