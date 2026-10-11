package media

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Screen capture without a GPU, and how FFmpeg's capture sources fail.
//
// ddagrab (DXGI Desktop Duplication) and gfxcapture (Windows.Graphics.Capture)
// capture on the GPU, and both open FFmpeg's D3D11VA hardware device first:
// D3D11CreateDevice on the default adapter with
// D3D11_CREATE_DEVICE_VIDEO_SUPPORT, then its ID3D11VideoDevice and
// ID3D11VideoContext. A PC or VM without a GPU has only the Microsoft Basic
// Display Adapter and the Basic Render Driver (WARP), which have no video
// support: FFmpeg logs "Failed to create D3D11VA device." (the device
// context's own failure is silent when only the video interfaces are
// missing) and every start fails the same way. gdigrab (GDI BitBlt into
// system memory) needs no GPU and works on any interactive desktop, at a
// higher CPU cost per frame: the whole picture is copied and converted on
// the CPU every frame interval, changed or not.

// CaptureFault is what kind of capture failure an FFmpeg generation's stderr
// shows (ClassifyCapture).
type CaptureFault int

const (
	// CaptureFaultNone: no capture failure recognised.
	CaptureFaultNone CaptureFault = iota
	// CaptureNoDevice: there is no Direct3D 11 video device, so neither
	// ddagrab nor gfxcapture can work on this PC (until it has a GPU
	// driver). gdigrab can.
	CaptureNoDevice
	// CaptureUnsupported: Desktop Duplication refuses this output or this
	// desktop (DXGI_ERROR_UNSUPPORTED: a remote desktop session, a display
	// mode it cannot duplicate; an output that is not one of the device's
	// adapter; E_INVALIDARG). A mode change can cause one for a moment, so
	// it takes two in a row before a session captures with GDI instead.
	CaptureUnsupported
	// CaptureTransient: an outage that ends by itself and that GDI would
	// not get around: the secure desktop (E_ACCESSDENIED: a UAC prompt, the
	// lock screen), a mode change or full-screen switch
	// (DXGI_ERROR_ACCESS_LOST), the output's size changed, too many
	// duplications at once, GDI's BitBlt failing without an interactive
	// desktop. The same capture is retried.
	CaptureTransient
)

// CaptureFailure is a capture failure read from FFmpeg's stderr.
type CaptureFailure struct {
	Fault CaptureFault
	// Text says what happened, for the user: lower case, no final stop, no
	// parentheses (notices put it in some; "" when nothing was
	// recognised). Line is FFmpeg's line it was read from, without its log
	// context.
	Text string
	Line string
}

// captureRule maps a line of a capture source to a CaptureFailure.
type captureRule struct {
	sources []string // the source's names in the line's log context ("[Parsed_ddagrab_0 @ ...]")
	sub     string
	fault   CaptureFault
	text    string
}

var (
	gpuSources = []string{"ddagrab", "gfxcapture"}
	ddaSource  = []string{"ddagrab"}
	// gdigrab logs as its input ("[in#0 @ ...]" while it opens, "[in#0/
	// gdigrab @ ...]" after); its messages are its own.
	gdiSource = []string{"gdigrab", "in#"}
)

// captureRules are the failures ClassifyCapture recognises, by FFmpeg 8.1's
// messages (libavfilter/vsrc_ddagrab.c, vsrc_gfxcapture_winrt.cpp,
// libavdevice/gdigrab.c).
var captureRules = []captureRule{
	{gpuSources, "Failed to create D3D11VA device", CaptureNoDevice,
		"no Direct3D 11 video device: a PC or VM without a GPU, or a graphics driver being installed or reset"},
	{ddaSource, "Selected output not supported", CaptureUnsupported,
		"Desktop Duplication cannot capture this display: a remote desktop session, or a display mode it does not support"},
	{ddaSource, "Failed to enumerate DXGI output", CaptureUnsupported,
		"Desktop Duplication cannot find this display on its GPU: a remote desktop session, or a display on another GPU"},
	{ddaSource, "Invalid output duplication argument", CaptureUnsupported,
		"Desktop Duplication cannot capture this display from the GPU it runs on"},
	{ddaSource, "Desktop duplication access denied", CaptureTransient,
		"Windows does not let programs capture the secure desktop: a UAC prompt or the lock screen"},
	{ddaSource, "Too many open duplication sessions", CaptureTransient,
		"too many programs capture the screen at once: Desktop Duplication allows four"},
	{ddaSource, "AcquireNextFrame failed", CaptureTransient,
		"Desktop Duplication lost the display: a display mode change, or a full-screen program took it over"},
	{ddaSource, "Output parameters changed", CaptureTransient,
		"the display's resolution or format changed"},
	{ddaSource, "Failed duplicating output", CaptureTransient,
		"Desktop Duplication could not start: is the session locked or disconnected?"},
	{gdiSource, "Failed to capture image", CaptureTransient,
		"GDI screen capture failed: is the session locked or disconnected?"},
	{gdiSource, "Couldn't get window device context", CaptureTransient,
		"GDI screen capture failed: is the session locked or disconnected?"},
	{gdiSource, "extends outside window area", CaptureFaultNone,
		"GDI cannot capture this display's area: a display left of or above the main one, with display scaling?"},
}

// logContext matches FFmpeg's log context in front of a message ("[Parsed_
// ddagrab_0 @ 000002bda68effc0] ", "[vost#0:0/libx264 @ 0x55d0] [enc:libx264
// @ 0x55d1] ").
var logContext = regexp.MustCompile(`\[([^\[\]]+) @ (?:0x)?[0-9A-Fa-f]+\]\s*`)

// ffmpegLines splits stderr as the encoder failure carries it: lines, or
// the stderr ring's " | "-joined last lines.
func ffmpegLines(stderr string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(stderr, "\r", ""), "\n") {
		out = append(out, strings.Split(l, " | ")...)
	}
	return out
}

// ClassifyCapture reads the first capture failure it recognises from an
// FFmpeg generation's stderr. Only lines a capture source logged count (its
// name in the line's log context): ddagrab, gfxcapture, gdigrab.
func ClassifyCapture(stderr string) CaptureFailure {
	for _, line := range ffmpegLines(stderr) {
		ctx, end := "", 0
		for _, m := range logContext.FindAllStringSubmatchIndex(line, -1) {
			ctx += line[m[2]:m[3]] + " "
			end = m[1]
		}
		if ctx == "" {
			continue
		}
		msg := strings.TrimSpace(line[end:]) // the message after its contexts
		for _, r := range captureRules {
			if !strings.Contains(msg, r.sub) {
				continue
			}
			for _, src := range r.sources {
				if strings.Contains(ctx, src) {
					return CaptureFailure{Fault: r.fault, Text: r.text, Line: msg}
				}
			}
		}
	}
	return CaptureFailure{}
}

// PlainFFmpegError returns an FFmpeg error text without the log contexts in
// front of its lines ("[Parsed_ddagrab_0 @ 000002bda68effc0] "), which say
// nothing to a user and take most of a short notice.
func PlainFFmpegError(s string) string {
	return logContext.ReplaceAllString(s, "")
}

// gpuCaptureTimeout bounds the GPU capture test. Desktop Duplication hands
// out the desktop's current image at once, but ddagrab waits for its first
// frame (a display that is off, a desktop that does not draw): a test that
// times out is no finding. Generous: the probe runs it next to a dozen
// encoder tests, which on a small VM at logon can delay FFmpeg's start by
// seconds (under Wine 5 s were not always enough).
const gpuCaptureTimeout = 10 * time.Second

// CheckGPUCapture opens ddagrab on output 0 for one frame, as a session's
// capture does, and returns why GPU capture (ddagrab, gfxcapture: the same
// D3D11VA device) cannot work on this PC: no Direct3D 11 video device
// (CaptureNoDevice; detail is FFmpeg's line). It returns "" when the capture
// opened (detail ""), and when it failed for another reason (the secure
// desktop at logon, a mode change) or timed out, which a session can get
// past; detail then says what happened. The test takes a fraction of a
// second.
func CheckGPUCapture(ctx context.Context, ffmpeg string) (blocked, detail string) {
	ctx, cancel := context.WithTimeout(ctx, gpuCaptureTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-filter_complex", "ddagrab=output_idx=0:framerate=10:draw_mouse=0", "-frames:v", "1", "-f", "null", "-")
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return "", ""
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", "no frame within " + gpuCaptureTimeout.String()
	}
	if f := ClassifyCapture(stderr.String()); f.Fault == CaptureNoDevice {
		return f.Text, f.Line
	} else if f.Line != "" {
		return "", f.Line
	}
	return "", err.Error() + ": " + PlainFFmpegError(causeLines(stderr.String(), 3))
}

// GPUCaptureBlocked returns why FFmpeg's GPU capture (ddagrab, gfxcapture)
// cannot work on this PC, from the probe's CheckGPUCapture, or "" when it can
// or the probe could not tell.
func (c *Caps) GPUCaptureBlocked() string { return c.gpuCapture }

// SetGPUCaptureBlocked records why GPU capture cannot work ("" = it can), as
// the probe does (tests).
func (c *Caps) SetGPUCaptureBlocked(why string) { c.gpuCapture = why }

// CanGDIGrab reports whether this FFmpeg build has the gdigrab input device
// (Windows builds).
func (c *Caps) CanGDIGrab() bool { return c.Devices["gdigrab"] }
