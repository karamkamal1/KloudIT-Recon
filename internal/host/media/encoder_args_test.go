package media

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// ffmpeg81Caps returns Caps with the private encoder options of the FFmpeg 8.1
// Windows build the installer downloads: testdata/ffmpeg81-h-<encoder>.txt is
// its "ffmpeg -h encoder=<encoder>" output.
func ffmpeg81Caps(t *testing.T) *Caps {
	t.Helper()
	c := &Caps{Filters: map[string]bool{"ddagrab": true}, options: map[string]map[string]bool{},
		optValues: map[string]map[string]map[string]bool{}}
	for _, e := range []string{"av1_amf", "hevc_amf", "h264_amf", "av1_nvenc", "hevc_nvenc", "h264_nvenc"} {
		b, err := os.ReadFile(filepath.Join("testdata", "ffmpeg81-h-"+e+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		c.options[e], c.optValues[e] = parseEncoderHelp(string(b))
	}
	return c
}

func encoderNamed(name string) EncoderInfo {
	for _, e := range candidates {
		if e.Name == name {
			return e
		}
	}
	panic(name)
}

// usages returns the usages an encoder runs with: the default and, if it has
// one, the one it is retried with after a failure.
func usages(e EncoderInfo) []string {
	if u := RetryUsage(e); u != "" {
		return []string{"", u}
	}
	return []string{""}
}

// encoderArgMap returns the encoder arguments of a ddagrab command line (the
// ones after "-c:v <encoder>") as option -> value, failing on a repeated option.
func encoderArgMap(t *testing.T, c *Caps, p Params) map[string]string {
	t.Helper()
	p.Source = Source{Backend: "ddagrab"}
	args, err := c.BuildArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for i < len(args)-1 && !(args[i] == "-c:v" && args[i+1] == p.Encoder.Name) {
		i++
	}
	m := map[string]string{}
	for i += 2; i < len(args) && args[i] != "-fps_mode" && args[i] != "-enc_time_base"; i += 2 {
		name := strings.TrimPrefix(args[i], "-")
		if name == args[i] || i+1 >= len(args) {
			t.Fatalf("%s: malformed encoder arguments at %q: %q", p.Encoder.Name, args[i], args)
		}
		if _, dup := m[name]; dup {
			t.Fatalf("%s: -%s given twice: %q", p.Encoder.Name, name, args)
		}
		m[name] = args[i+1]
	}
	return m
}

func TestParseEncoderHelp(t *testing.T) {
	c := ffmpeg81Caps(t)
	for _, tc := range []struct {
		enc, opt string
		values   string // sorted named values, "" = none
	}{
		{"av1_amf", "header_insertion_mode", "frame gop none"},
		{"hevc_amf", "header_insertion_mode", "gop idr none"},
		{"av1_amf", "latency", "lowest_latency none power_saving_real_time real_time"},
		{"hevc_amf", "latency", ""}, // boolean
		{"av1_amf", "usage", "high_quality lowlatency lowlatency_high_quality transcoding ultralowlatency webcam"},
		{"h264_amf", "rc", "cbr cqp hqcbr hqvbr qvbr vbr_latency vbr_peak"},
		{"hevc_nvenc", "tune", "hq ll lossless uhq ull"},
	} {
		if !c.HasOption(tc.enc, tc.opt) {
			t.Errorf("%s: no -%s", tc.enc, tc.opt)
			continue
		}
		var names []string
		for n := range c.optValues[tc.enc][tc.opt] {
			names = append(names, n)
		}
		sort.Strings(names)
		if got := strings.Join(names, " "); got != tc.values {
			t.Errorf("%s -%s values %q, want %q", tc.enc, tc.opt, got, tc.values)
		}
	}
	for _, tc := range []struct {
		enc, opt string
		has      bool
	}{
		{"h264_amf", "header_insertion_mode", false},
		{"h264_amf", "frame_skipping", true},
		{"h264_amf", "skip_frame", false},
		{"hevc_amf", "skip_frame", true},
		{"av1_amf", "skip_frame", true},
		{"av1_amf", "vbaq", false},
		{"av1_amf", "aq_mode", true},
		{"av1_amf", "async_depth", true},
		{"hevc_nvenc", "intra-refresh", true},
	} {
		if c.HasOption(tc.enc, tc.opt) != tc.has {
			t.Errorf("%s -%s present %v, want %v", tc.enc, tc.opt, !tc.has, tc.has)
		}
	}
	// A3: the header insertion mode the AMD arguments used to pass to every
	// encoder is not a value of av1_amf's option (FFmpeg refuses to open it).
	if c.acceptsValue("av1_amf", "header_insertion_mode", "idr") || !c.acceptsValue("hevc_amf", "header_insertion_mode", "idr") {
		t.Error("header_insertion_mode idr: want rejected by av1_amf, accepted by hevc_amf")
	}
	if !c.acceptsValue("av1_amf", "latency", "3") || !c.acceptsValue("hevc_amf", "latency", "1") || c.acceptsValue("av1_amf", "latency", "fast") {
		t.Error("numbers must pass for enum and boolean options, unknown words not")
	}
}

// TestAMDEncoderArgs checks the AMF arguments (guide step 1.1, A1-A6) against
// the real option lists of FFmpeg 8.1, so the per-encoder filtering of option
// names and values is exercised.
func TestAMDEncoderArgs(t *testing.T) {
	c := ffmpeg81Caps(t)
	common := map[string]string{
		"usage": "ultralowlatency", "quality": "speed", "rc": "cbr", "enforce_hrd": "0", "filler_data": "0",
		"preanalysis": "0", "preencode": "0", "async_depth": "1", "flags": "+low_delay", "forced_idr": "1",
		"b:v": "20000k", "maxrate": "20000k", "bufsize": "500k", "g": "0", "bf": "0",
	}
	for _, tc := range []struct {
		enc    string
		extra  map[string]string // on top of common
		absent []string
	}{
		{"hevc_amf", map[string]string{"skip_frame": "0", "latency": "1", "header_insertion_mode": "idr", "vbaq": "1"},
			[]string{"frame_skipping", "align"}},
		{"av1_amf", map[string]string{"skip_frame": "0", "latency": "lowest_latency", "header_insertion_mode": "frame"},
			[]string{"frame_skipping", "vbaq", "align"}},
		// No header_insertion_mode on H.264 (its option is header_spacing).
		{"h264_amf", map[string]string{"frame_skipping": "0", "latency": "1", "vbaq": "1"},
			[]string{"skip_frame", "header_insertion_mode", "header_spacing"}},
	} {
		t.Run(tc.enc, func(t *testing.T) {
			p := Params{Encoder: encoderNamed(tc.enc), FPS: 60, BitrateKbps: 20000, Adaptive: true, Quality: ""}
			got := encoderArgMap(t, c, p)
			if args, err := c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: p.Encoder, FPS: 60, BitrateKbps: 20000, Adaptive: true}); err == nil {
				t.Logf("ffmpeg %s", strings.Join(args, " "))
			}
			want := map[string]string{}
			for k, v := range common {
				want[k] = v
			}
			for k, v := range tc.extra {
				want[k] = v
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("-%s %q, want %q", k, got[k], v)
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					t.Errorf("unexpected -%s %s", k, got[k])
				}
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("-%s must not be passed", k)
				}
			}

			// Fixed bitrate: latency-constrained VBR; explicit presets win.
			p.Adaptive, p.Quality = false, "balanced"
			got = encoderArgMap(t, c, p)
			if got["rc"] != "vbr_latency" || got["quality"] != "balanced" {
				t.Errorf("fixed bitrate: -rc %s -quality %s, want vbr_latency balanced", got["rc"], got["quality"])
			}
			p.Quality = "quality"
			if q := encoderArgMap(t, c, p)["quality"]; q != "quality" {
				t.Errorf("-quality %s, want quality", q)
			}
			// The usage retried after a failure.
			p.Usage = "lowlatency"
			if u := encoderArgMap(t, c, p)["usage"]; u != "lowlatency" {
				t.Errorf("-usage %s, want lowlatency", u)
			}
		})
	}
	if RetryUsage(encoderNamed("h264_amf")) != "lowlatency" || RetryUsage(encoderNamed("hevc_amf")) != "" ||
		RetryUsage(encoderNamed("h264_nvenc")) != "" {
		t.Error("RetryUsage: only h264_amf has a fallback usage")
	}
}

// TestNVIDIAEncoderArgs checks that the NVENC arguments survive the value
// filtering against FFmpeg 8.1's option lists unchanged.
func TestNVIDIAEncoderArgs(t *testing.T) {
	c := ffmpeg81Caps(t)
	for _, tc := range []struct {
		enc     string
		profile string
	}{{"hevc_nvenc", "main"}, {"h264_nvenc", "high"}, {"av1_nvenc", ""}} {
		got := encoderArgMap(t, c, Params{Encoder: encoderNamed(tc.enc), FPS: 60, BitrateKbps: 20000, Quality: "balanced"})
		want := map[string]string{
			"preset": "p3", "tune": "ull", "rc": "cbr", "multipass": "disabled", "zerolatency": "1", "delay": "0",
			"rc-lookahead": "0", "no-scenecut": "1", "forced-idr": "1", "strict_gop": "1", "spatial-aq": "1",
			"b:v": "20000k", "maxrate": "20000k", "bufsize": "500k", "g": "216000", "bf": "0",
		}
		if tc.profile != "" {
			want["profile"] = tc.profile
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: -%s %q, want %q", tc.enc, k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d arguments, want %d: %v", tc.enc, len(got), len(want), got)
		}
	}
}

// TestRecovery checks the recovery mode announced to the client (guide step
// 1.4): skip only with an intra refresh that heals within maxHealSeconds in
// the worst case (two refresh periods), read from the arguments the host
// really passes (FFmpeg 8.1 option lists).
func TestRecovery(t *testing.T) {
	c := ffmpeg81Caps(t)
	// The options Recovery reads are the real ones of these encoders.
	for enc, opt := range map[string]string{"hevc_nvenc": "intra-refresh", "h264_nvenc": "intra-refresh", "av1_nvenc": "intra-refresh",
		"h264_amf": "intra_refresh_mb"} {
		if !c.HasOption(enc, opt) {
			t.Errorf("%s has no -%s", enc, opt)
		}
	}
	for _, opt := range []string{"intra-refresh", "intra_refresh_mb"} {
		for _, enc := range []string{"hevc_amf", "av1_amf"} {
			if c.HasOption(enc, opt) {
				t.Errorf("%s has -%s: Recovery should read it", enc, opt)
			}
		}
	}
	// Today's arguments: no intra refresh anywhere, so every encoder needs key frames.
	for enc := range c.options {
		args, err := c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: encoderNamed(enc), FPS: 60, BitrateKbps: 20000})
		if err != nil {
			t.Fatal(err)
		}
		if r := Recovery(args, 1920, 1080, 60); r != proto.RecoveryKeyframe {
			t.Errorf("%s: recovery %q, want keyframe", enc, r)
		}
	}
	for _, tc := range []struct {
		args       string
		w, h, fps  int
		want, why  string
		withNVENCg bool
	}{
		{"-intra-refresh 1 -g 60", 1920, 1080, 60, proto.RecoverySkip, "1 s refresh period: heals within 2 s", false},
		{"-intra-refresh 1 -g 61", 1920, 1080, 60, proto.RecoveryKeyframe, "period over 1 s: two periods over 2 s", false},
		{"-intra-refresh 1 -g 120", 1920, 1080, 60, proto.RecoveryKeyframe, "2 s refresh period: up to 4 s damaged", false},
		{"-intra-refresh 1", 1920, 1080, 60, proto.RecoveryKeyframe, "NVENC with the default GOP (an hour): heals nothing", true},
		{"-intra-refresh true -g 30", 1280, 720, 30, proto.RecoverySkip, "boolean spelled out", false},
		{"-intra-refresh 0 -g 60", 1920, 1080, 60, proto.RecoveryKeyframe, "off", false},
		{"-g 30", 1920, 1080, 60, proto.RecoveryKeyframe, "no intra refresh", false},
		// 1920x1080 = 120 x 68 = 8160 macroblocks.
		{"-intra_refresh_mb 255", 1920, 1080, 60, proto.RecoverySkip, "32 frames", false},
		{"-intra_refresh_mb 136", 1920, 1080, 60, proto.RecoverySkip, "60 frames", false},
		{"-intra_refresh_mb 135", 1920, 1080, 60, proto.RecoveryKeyframe, "61 frames", false},
		{"-intra_refresh_mb 68", 1920, 1080, 60, proto.RecoveryKeyframe, "120 frames: up to 4 s damaged", false},
		{"-intra_refresh_mb -1", 1920, 1080, 60, proto.RecoveryKeyframe, "FFmpeg's default: off", false},
		{"-intra_refresh_mb 255", 0, 0, 60, proto.RecoveryKeyframe, "size unknown", false},
	} {
		args := strings.Fields(tc.args)
		if tc.withNVENCg {
			p := Params{Source: Source{Backend: "ddagrab"}, Encoder: encoderNamed("hevc_nvenc"), FPS: tc.fps, BitrateKbps: 20000}
			base, err := c.BuildArgs(p)
			if err != nil {
				t.Fatal(err)
			}
			args = append(base[:len(base):len(base)], args...)
		}
		if got := Recovery(args, tc.w, tc.h, tc.fps); got != tc.want {
			t.Errorf("%s (%s): %q, want %q", tc.args, tc.why, got, tc.want)
		}
	}
}

// TestEncoderArgsNotDropped checks that the option filtering (opt in
// encoderArgs) drops no option the host means to pass to an AMD or NVIDIA
// encoder for a reason other than the encoder lacking it: for every preset,
// adaptive on/off and usage it builds the arguments with FFmpeg 8.1's option
// lists and with lists that take every option of the vendor's encoders with
// any value. An option missing from the first must be one the encoder does
// not have, and exactly the ones listed below: a value FFmpeg 8.1 does not
// name (a typo, or a value the build lacks) would be dropped silently.
func TestEncoderArgsNotDropped(t *testing.T) {
	c := ffmpeg81Caps(t)
	wide := &Caps{Filters: c.Filters, options: map[string]map[string]bool{}}
	for enc := range c.options {
		wide.options[enc] = map[string]bool{}
		for other, opts := range c.options {
			if encoderNamed(other).Vendor == encoderNamed(enc).Vendor {
				for o := range opts {
					wide.options[enc][o] = true
				}
			}
		}
	}
	notOn := map[string]string{ // options the host asks for that the encoder does not have
		"hevc_amf": "frame_skipping", "av1_amf": "frame_skipping", "h264_amf": "header_insertion_mode skip_frame",
		"hevc_nvenc": "", "h264_nvenc": "", "av1_nvenc": "",
	}
	for enc := range c.options {
		e := encoderNamed(enc)
		for _, q := range []string{"", "speed", "balanced", "quality"} {
			for _, adaptive := range []bool{false, true} {
				for _, usage := range usages(e) {
					p := Params{Encoder: e, FPS: 120, BitrateKbps: 50000, Quality: q, Adaptive: adaptive, Usage: usage}
					got, want := encoderArgMap(t, c, p), encoderArgMap(t, wide, p)
					var dropped []string
					for k, v := range want {
						if g, ok := got[k]; !ok {
							dropped = append(dropped, k)
							if c.HasOption(enc, k) {
								t.Errorf("%s (quality %q adaptive %v usage %q): -%s %s dropped: FFmpeg 8.1 does not take the value", enc, q, adaptive, usage, k, v)
							}
						} else if g != v {
							t.Errorf("%s (quality %q adaptive %v usage %q): -%s %s, want %s", enc, q, adaptive, usage, k, g, v)
						}
					}
					for k := range got {
						if _, ok := want[k]; !ok {
							t.Errorf("%s (quality %q adaptive %v usage %q): unexpected -%s", enc, q, adaptive, usage, k)
						}
					}
					sort.Strings(dropped)
					if d := strings.Join(dropped, " "); d != notOn[enc] {
						t.Errorf("%s (quality %q adaptive %v usage %q): options not passed %q, want %q", enc, q, adaptive, usage, d, notOn[enc])
					}
				}
			}
		}
	}
}

// ffmpegOptionError matches FFmpeg's refusal of an option or option value,
// which it reports before it opens the encoder.
var ffmpegOptionError = regexp.MustCompile(`Error applying encoder options|Error setting option|Unable to parse "[^"]*" option value|Option \S+ not found`)

// amfRuntimeError matches the AMF runtime or device failing to load, which
// comes only after all options were accepted.
var amfRuntimeError = regexp.MustCompile(`amfrt64\S* failed to open|hardware device context \(AMF\)`)

// TestAMFArgsAccepted runs the AMF command lines the host builds with an FFmpeg
// that has the AMF encoders (Windows builds; in the sandbox the FFmpeg 8.1
// build under Wine, via the cross-compiled test binary). On an AMD GPU every
// run must encode; elsewhere it must get past option parsing to the AMF
// runtime error. The pre-1.1 AV1 arguments must be refused (A3).
func TestAMFArgsAccepted(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	caps, err := Probe(context.Background(), ff, nil)
	if err != nil && caps == nil {
		t.Skipf("probe: %v", err)
	}
	run := func(args []string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var stderr bytes.Buffer
		cmd := quietCmd(ctx, ff, args...)
		cmd.Stderr = &stderr
		err := cmd.Run()
		return strings.ReplaceAll(stderr.String(), "\r", ""), err
	}
	// limit makes a command line stop after a few frames and discards its output.
	limit := func(args []string) []string {
		out := append([]string{}, args[:len(args)-1]...)
		return append(out, "-frames:v", "5", "-")
	}
	tested := 0
	for _, name := range []string{"av1_amf", "hevc_amf", "h264_amf"} {
		if caps.options[name] == nil {
			t.Logf("%s: not in this ffmpeg build", name)
			continue
		}
		tested++
		e := encoderNamed(name)
		works := false // passed the probe's test encode: an AMD GPU
		for _, w := range caps.Encoders {
			works = works || w.Name == name
		}
		for _, usage := range usages(e) {
			for _, adaptive := range []bool{true, false} {
				p := Params{Source: Source{Backend: "test", NativeW: 1280, NativeH: 720}, Encoder: e, FPS: 60, BitrateKbps: 20000,
					Adaptive: adaptive, Usage: usage, CaptureClock: caps.CanStampCapture()}
				args, err := caps.BuildArgs(p)
				if err != nil {
					t.Fatal(err)
				}
				stderr, err := run(limit(args))
				switch {
				case ffmpegOptionError.MatchString(stderr):
					t.Errorf("%s (adaptive %v usage %q): ffmpeg refused an option: %s\nargs: %q", name, adaptive, usage, causeLines(stderr, 4), args)
				case err == nil:
					t.Logf("%s (adaptive %v usage %q): encoded", name, adaptive, usage)
				case works:
					t.Errorf("%s (adaptive %v usage %q) passed the probe but failed: %v: %s", name, adaptive, usage, err, causeLines(stderr, 4))
				case amfRuntimeError.MatchString(stderr):
					t.Logf("%s (adaptive %v usage %q): options accepted, then: %s", name, adaptive, usage, causeLines(stderr, 2))
				default:
					t.Errorf("%s (adaptive %v usage %q): unexpected failure: %v: %s", name, adaptive, usage, err, causeLines(stderr, 4))
				}
			}
		}
	}
	if tested == 0 {
		t.Skip("ffmpeg has no AMF encoders")
	}
	if caps.options["av1_amf"] == nil {
		return
	}
	// The AV1 arguments before guide step 1.1.
	old := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=1280x720:r=60", "-frames:v", "5",
		"-pix_fmt", "yuv420p", "-c:v", "av1_amf", "-usage", "ultralowlatency", "-quality", "balanced", "-rc", "cbr",
		"-enforce_hrd", "1", "-filler_data", "0", "-preanalysis", "0", "-latency", "1", "-header_insertion_mode", "idr",
		"-b:v", "20000k", "-maxrate", "20000k", "-bufsize", "500k", "-g", "1000", "-bf", "0", "-f", "null", "-"}
	stderr, err := run(old)
	if err == nil || !ffmpegOptionError.MatchString(stderr) || !strings.Contains(stderr, "header_insertion_mode") {
		t.Errorf("old av1_amf arguments: want header_insertion_mode refused, got err %v: %s", err, causeLines(stderr, 4))
	} else {
		t.Logf("old av1_amf arguments refused: %s", causeLines(stderr, 3))
	}
}
