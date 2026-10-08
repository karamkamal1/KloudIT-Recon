package qualify

import (
	"slices"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Each cell's stream starts as a session's: its quality preset, two LTR
// slots where the codec recovers from LTR frames (encoder.Caps.LTRSlots, as
// media.HelperVideo), none otherwise, and intra refresh where the encoder has
// it and runs no LTR slots (encoder.Caps.IntraRefreshFrames: GUIDE 2.3).
func TestCellArgs(t *testing.T) {
	caps := encoder.Caps{Backend: "amf", Codecs: map[string]encoder.CodecCaps{
		"hevc": {Recovery: "ltr", MaxLTR: 2, IntraRefresh: true}, "h264": {Recovery: "none", IntraRefresh: true}}}
	o := Options{}
	o.defaults()
	cr := cellRun{backend: "amf", capture: "synthetic-gpu", motion: true, frames: 3600, step: 120}
	c := &Cell{Codec: "hevc", Quality: "balanced", LTRSlots: caps.LTRSlots("hevc"), RC: "cbr", LiveBitrate: ModeSeamless}
	args := cellArgs(o, cr, c, "s.hevc", "s.jsonl")
	for _, want := range []string{"--codec=hevc", "--quality=balanced", "--ltr-slots=2", "--rc=cbr", "--live-bitrate=seamless",
		"--rate-schedule=20000,50000:120", "--kbps=50000", "--motion=1"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %q", want, args)
		}
	}
	if c.IntraRefresh = caps.IntraRefreshFrames("hevc", o.FPS); c.IntraRefresh != 0 || strings.Contains(strings.Join(args, " "), "--intra-refresh") {
		t.Errorf("hevc with LTR slots got intra refresh: %d %q", c.IntraRefresh, args)
	}
	c = &Cell{Codec: "h264", Quality: "speed", LTRSlots: caps.LTRSlots("h264"), RC: "cbr", LiveBitrate: ModeFlush,
		IntraRefresh: caps.IntraRefreshFrames("h264", 60)}
	if args := strings.Join(cellArgs(o, cr, c, "s.h264", "s.jsonl"), " "); strings.Contains(args, "--ltr-slots") ||
		!strings.Contains(args, "--quality=speed") || !strings.Contains(args, "--intra-refresh=30") {
		t.Errorf("h264 (no LTR recovery, intra refresh at 60 fps): %s", args)
	}
}

// A start the encoder refuses for its live-bitrate mode is a fail of that
// mode; other refusals are errors. A helper that leaves no frame log after
// its stream started crashed or hung in that mode: a fail; before that an
// error.
func TestRunFailures(t *testing.T) {
	var c Cell
	startFailed("encode-test: start failed: unsupported: liveBitrate seamless: this encoder cannot change the bitrate of a running "+
		"session (NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE 0; caps liveBitrate restart)", &c)
	if c.Verdict != VerdictFail || !strings.HasPrefix(c.Failures[0], "the encoder refuses the mode: liveBitrate seamless: this encoder") {
		t.Fatalf("refused mode: %+v", c)
	}
	for _, line := range []string{"encode-test: start failed: unsupported: rc vbr_peak is not supported",
		"encode-test: start failed: init_failed: liveBitrate flush", "encode-test: cannot write x.hevc"} {
		c = Cell{}
		startFailed(line, &c)
		if c.Verdict != VerdictError || c.Failures[0] != "the stream did not start: "+line {
			t.Fatalf("%q: %+v", line, c)
		}
	}
	c = Cell{}
	noFrameLog([]byte("encode-test: backend amf (AMD Radeon RX 7900 XT), {}\nencode-test: {\"t\":\"started\",\"backend\":\"amf\"}\n"),
		"exit status 0xc0000005: encode-test: {...}", &c)
	if c.Verdict != VerdictFail || !strings.Contains(c.Failures[0], "crashed or hung during the run") {
		t.Fatalf("crash after the start: %+v", c)
	}
	c = Cell{}
	noFrameLog([]byte("encode-test: backend amf (AMD Radeon RX 7900 XT), {}\n"), "signal: killed", &c)
	if c.Verdict != VerdictError || c.Failures[0] != "no frame log: signal: killed" {
		t.Fatalf("crash before the start: %+v", c)
	}
}

// The libavcodec backend (GUIDE 3.8) runs as sessions run it: with the host
// config's FFmpeg library directory; its streams start without LTR slots or
// intra refresh (caps recovery none, no intra refresh).
func TestCellArgsLavc(t *testing.T) {
	caps := encoder.Caps{Backend: "lavc", Codecs: map[string]encoder.CodecCaps{"hevc": {Recovery: "none", ForceIDR: true, LiveBitrate: "flush"}}}
	o := Options{FFmpegDir: `C:\Program Files\KlouditRecon\ffmpeg-lgpl`}
	o.defaults()
	cr := cellRun{backend: "lavc", capture: "synthetic-gpu", motion: true, frames: 3600, step: 120}
	c := &Cell{Codec: "hevc", Quality: "speed", LTRSlots: caps.LTRSlots("hevc"), RC: DefaultRCModes("lavc")[0], LiveBitrate: ModeSeamless,
		IntraRefresh: caps.IntraRefreshFrames("hevc", 60)}
	args := strings.Join(cellArgs(o, cr, c, "s.hevc", "s.jsonl"), " ")
	for _, want := range []string{"--backend=lavc", `--ffmpeg-dir=C:\Program Files\KlouditRecon\ffmpeg-lgpl`, "--rc=cbr"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %s: %s", want, args)
		}
	}
	for _, bad := range []string{"--ltr-slots", "--intra-refresh", "--lavc-test-encoder", "--nvenc-test-dll"} {
		if strings.Contains(args, bad) {
			t.Errorf("args with %s: %s", bad, args)
		}
	}
	o.LavcTestEncoder = "libx264"
	if args := o.backendArgs("lavc"); !slices.Contains(args, "--lavc-test-encoder=libx264") || args[0] != "--backend=lavc" {
		t.Errorf("test encoder args %q", args)
	}
}
