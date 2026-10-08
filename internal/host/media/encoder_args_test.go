package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
// filtering against FFmpeg 8.1's option lists unchanged, without intra refresh
// (a GPU without it) and with each mode the probe can find (guide step 1.2):
// -intra-refresh 1 (and -single-slice-intra-refresh 1) with -g = the refresh
// period of half a second, forced-idr kept.
func TestNVIDIAEncoderArgs(t *testing.T) {
	for _, tc := range []struct {
		enc     string
		profile string
		ir      string // intra refresh mode found by the probe
		fps     int
		g       string
	}{
		{"hevc_nvenc", "main", "", 60, "216000"},
		{"h264_nvenc", "high", "", 60, "216000"},
		{"av1_nvenc", "", "", 60, "216000"},
		{"hevc_nvenc", "main", IntraRefreshSingleSlice, 60, "30"},
		{"h264_nvenc", "high", IntraRefreshSingleSlice, 120, "60"},
		{"hevc_nvenc", "main", IntraRefreshOn, 144, "72"},
		{"h264_nvenc", "high", IntraRefreshOn, 30, "15"},
	} {
		c := ffmpeg81Caps(t)
		if tc.ir != "" {
			c.intraRefresh = map[string]string{tc.enc: tc.ir}
		}
		got := encoderArgMap(t, c, Params{Encoder: encoderNamed(tc.enc), FPS: tc.fps, BitrateKbps: 20000, Quality: "balanced"})
		want := map[string]string{
			"preset": "p3", "tune": "ull", "rc": "cbr", "multipass": "disabled", "zerolatency": "1", "delay": "0",
			"rc-lookahead": "0", "no-scenecut": "1", "forced-idr": "1", "strict_gop": "1", "spatial-aq": "1",
			"b:v": "20000k", "maxrate": "20000k", "bufsize": strconv.Itoa(20000*3/2/tc.fps) + "k", "g": tc.g, "bf": "0",
		}
		if tc.profile != "" {
			want["profile"] = tc.profile
		}
		if tc.ir != "" {
			want["intra-refresh"] = "1"
		}
		if tc.ir == IntraRefreshSingleSlice {
			want["single-slice-intra-refresh"] = "1"
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s (intra refresh %q, %d fps): -%s %q, want %q", tc.enc, tc.ir, tc.fps, k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s (intra refresh %q): %d arguments, want %d: %v", tc.enc, tc.ir, len(got), len(want), got)
		}
	}
}

// TestNoYUV444 keeps 4:4:4 off (step 4.2): Chrome's hardware decode of HEVC
// Range Extensions is reported on NVIDIA and Intel GPUs only, not AMD, so a
// 4:4:4 stream would play only on some clients. ddagrab hands the GPU encoders
// BGRA textures: FFmpeg 8.1's NVENC converts packed RGB to 4:2:0 by default
// (rgb_mode yuv420, which the host never changes) and the host pins its HEVC
// and H.264 profiles to Main and High; AMF encodes 4:2:0 only and has no such
// option; software encoders get yuv420p.
func TestNoYUV444(t *testing.T) {
	c := ffmpeg81Caps(t)
	for _, e := range []string{"av1_nvenc", "hevc_nvenc"} {
		b, err := os.ReadFile(filepath.Join("testdata", "ffmpeg81-h-"+e+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`(?m)^\s*-rgb_mode .*\(default yuv420\)`).Match(b) {
			t.Errorf("%s: FFmpeg 8.1 help shows no rgb_mode default yuv420", e)
		}
	}
	for _, name := range []string{"av1_amf", "hevc_amf", "h264_amf", "av1_nvenc", "hevc_nvenc", "h264_nvenc"} {
		for _, q := range []string{"speed", "balanced", "quality"} {
			for _, adaptive := range []bool{false, true} {
				got := encoderArgMap(t, c, Params{Encoder: encoderNamed(name), FPS: 60, BitrateKbps: 20000, Quality: q, Adaptive: adaptive})
				if _, ok := got["rgb_mode"]; ok {
					t.Errorf("%s: -rgb_mode %s", name, got["rgb_mode"])
				}
				if p, ok := got["profile"]; ok && p != "main" && p != "high" {
					t.Errorf("%s (%s): -profile %s", name, q, p)
				}
				for k, v := range got {
					if strings.Contains(v, "444") || k == "pix_fmt" {
						t.Errorf("%s (%s): -%s %s", name, q, k, v)
					}
				}
			}
		}
	}
	for _, sw := range []string{"libx264", "libsvtav1"} {
		args, err := c.BuildArgs(Params{Encoder: encoderNamed(sw), FPS: 60, BitrateKbps: 20000, Source: Source{Backend: "test", NativeW: 640, NativeH: 360}})
		if err != nil || !strings.Contains(strings.Join(args, " "), "format=yuv420p") {
			t.Errorf("%s: no yuv420p conversion (%v): %q", sw, err, args)
		}
	}
}

// TestIntraRefreshEncoders checks which encoders the probe tries intra refresh
// on and how it picks the mode: single slice where the encoder has the option
// and the test encode passes, else plain, else none (a GPU without
// NV_ENC_CAPS_SUPPORT_INTRA_REFRESH fails both test encodes). Each test encode
// runs the host's NVENC arguments in that mode over two refresh waves.
func TestIntraRefreshEncoders(t *testing.T) {
	c := ffmpeg81Caps(t)
	for enc, want := range map[string]bool{"h264_nvenc": true, "hevc_nvenc": true, "av1_nvenc": false,
		"h264_amf": false, "hevc_amf": false, "av1_amf": false, "libx264": false} {
		if intraRefreshEncoders[enc] != want {
			t.Errorf("probe tries intra refresh on %s: %v, want %v", enc, !want, want)
		}
	}
	// FFmpeg 8.1: single slice intra refresh only for H.264 and HEVC.
	for enc, want := range map[string]bool{"h264_nvenc": true, "hevc_nvenc": true, "av1_nvenc": false} {
		if !c.HasOption(enc, "intra-refresh") || c.HasOption(enc, "single-slice-intra-refresh") != want {
			t.Errorf("%s: -intra-refresh %v, -single-slice-intra-refresh %v", enc, c.HasOption(enc, "intra-refresh"),
				c.HasOption(enc, "single-slice-intra-refresh"))
		}
	}
	errNo := errors.New("Intra refresh not supported by the device")
	for _, tc := range []struct {
		enc          string
		single, mode error // test encode results with single slice / plain intra refresh
		want         string
		tries        int
	}{
		{"hevc_nvenc", nil, nil, IntraRefreshSingleSlice, 1},
		{"h264_nvenc", errNo, nil, IntraRefreshOn, 2},
		{"hevc_nvenc", errNo, errNo, "", 2},
		{"av1_nvenc", nil, nil, IntraRefreshOn, 1}, // no single slice option: not tried
		{"hevc_amf", nil, nil, "", 0},              // no -intra-refresh
	} {
		var tries [][]string
		got := probeIntraRefresh(encoderNamed(tc.enc), c.options[tc.enc], c.optValues[tc.enc], func(frames int, extra ...string) error {
			tries = append(tries, extra)
			// The host's own arguments in the mode, -g = the refresh period
			// at the test encode's 30 fps, over two refresh waves.
			m := map[string]string{}
			for i := 0; i+1 < len(extra); i += 2 {
				m[strings.TrimPrefix(extra[i], "-")] = extra[i+1]
			}
			single := m["single-slice-intra-refresh"] == "1"
			if m["intra-refresh"] != "1" || m["g"] != "15" || m["bf"] != "0" || m["tune"] != "ull" || m["forced-idr"] != "1" ||
				frames <= 2*15 || single != (len(tries) == 1 && c.HasOption(tc.enc, "single-slice-intra-refresh")) {
				t.Errorf("%s: test encode of %d frames with %q", tc.enc, frames, extra)
			}
			if single {
				return tc.single
			}
			return tc.mode
		})
		if got != tc.want || len(tries) != tc.tries {
			t.Errorf("%s: mode %q after %d test encodes %q, want %q after %d", tc.enc, got, len(tries), tries, tc.want, tc.tries)
		}
	}
	// The test hook: libx264 only where it has the option.
	c.options["libx264"] = map[string]bool{"intra-refresh": true}
	if c.UseIntraRefresh("libsvtav1") || !c.UseIntraRefresh("libx264") || c.IntraRefresh("libx264") != IntraRefreshOn {
		t.Error("UseIntraRefresh")
	}
}

// TestIntraRefreshPeriod checks the refresh period (half a second, -g with
// intra refresh) and that every frame rate the host streams at gets recovery
// skip from it: two periods within maxHealSeconds.
func TestIntraRefreshPeriod(t *testing.T) {
	for fps, want := range map[int]int{1: 2, 3: 2, 10: 5, 30: 15, 60: 30, 75: 38, 120: 60, 144: 72, 240: 120} {
		if got := IntraRefreshPeriod(fps); got != want {
			t.Errorf("IntraRefreshPeriod(%d) = %d, want %d", fps, got, want)
		}
	}
	c := ffmpeg81Caps(t)
	c.intraRefresh = map[string]string{"hevc_nvenc": IntraRefreshSingleSlice, "h264_nvenc": IntraRefreshOn}
	for _, enc := range []string{"hevc_nvenc", "h264_nvenc"} {
		for fps := 10; fps <= 240; fps++ {
			for _, size := range [][2]int{{1280, 720}, {1920, 1080}, {3840, 2160}} {
				args, err := c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: encoderNamed(enc), FPS: fps, BitrateKbps: 20000})
				if err != nil {
					t.Fatal(err)
				}
				if r := Recovery(args, size[0], size[1], fps); r != proto.RecoverySkip {
					t.Fatalf("%s at %d fps %dx%d: recovery %q, want skip", enc, fps, size[0], size[1], r)
				}
				if n := HealFrames(args, size[0], size[1]); n != 2*IntraRefreshPeriod(fps) {
					t.Fatalf("%s at %d fps: HealFrames %d, want two periods", enc, fps, n)
				}
			}
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
	// The host's arguments: skip where the probe found intra refresh (step
	// 1.2: h264_nvenc and hevc_nvenc), key frames everywhere else, also on a
	// GPU without intra refresh.
	for _, found := range []bool{false, true} {
		c.intraRefresh = nil
		if found {
			c.intraRefresh = map[string]string{"hevc_nvenc": IntraRefreshSingleSlice, "h264_nvenc": IntraRefreshOn}
		}
		for enc := range c.options {
			args, err := c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: encoderNamed(enc), FPS: 60, BitrateKbps: 20000})
			if err != nil {
				t.Fatal(err)
			}
			want := proto.RecoveryKeyframe
			if found && intraRefreshEncoders[enc] {
				want = proto.RecoverySkip
			}
			if r := Recovery(args, 1920, 1080, 60); r != want {
				t.Errorf("%s (intra refresh found %v): recovery %q, want %q", enc, found, r, want)
			}
		}
	}
	c.intraRefresh = nil
	for _, tc := range []struct {
		args       string
		w, h, fps  int
		want, why  string
		withNVENCg bool
		heal       int // HealFrames: two refresh periods
	}{
		{"-intra-refresh 1 -g 60", 1920, 1080, 60, proto.RecoverySkip, "1 s refresh period: heals within 2 s", false, 120},
		{"-intra-refresh 1 -g 61", 1920, 1080, 60, proto.RecoveryKeyframe, "period over 1 s: two periods over 2 s", false, 122},
		{"-intra-refresh 1 -g 120", 1920, 1080, 60, proto.RecoveryKeyframe, "2 s refresh period: up to 4 s damaged", false, 240},
		{"-intra-refresh 1", 1920, 1080, 60, proto.RecoveryKeyframe, "NVENC with the default GOP (an hour): heals nothing", true, 432000},
		{"-intra-refresh true -g 30", 1280, 720, 30, proto.RecoverySkip, "boolean spelled out", false, 60},
		{"-intra-refresh 0 -g 60", 1920, 1080, 60, proto.RecoveryKeyframe, "off", false, 0},
		{"-g 30", 1920, 1080, 60, proto.RecoveryKeyframe, "no intra refresh", false, 0},
		// 1920x1080 = 120 x 68 = 8160 macroblocks.
		{"-intra_refresh_mb 255", 1920, 1080, 60, proto.RecoverySkip, "32 frames", false, 64},
		{"-intra_refresh_mb 136", 1920, 1080, 60, proto.RecoverySkip, "60 frames", false, 120},
		{"-intra_refresh_mb 135", 1920, 1080, 60, proto.RecoveryKeyframe, "61 frames", false, 122},
		{"-intra_refresh_mb 68", 1920, 1080, 60, proto.RecoveryKeyframe, "120 frames: up to 4 s damaged", false, 240},
		{"-intra_refresh_mb -1", 1920, 1080, 60, proto.RecoveryKeyframe, "FFmpeg's default: off", false, 0},
		{"-intra_refresh_mb 255", 0, 0, 60, proto.RecoveryKeyframe, "size unknown", false, 0},
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
		if got := HealFrames(args, tc.w, tc.h); got != tc.heal {
			t.Errorf("%s (%s): HealFrames %d, want %d", tc.args, tc.why, got, tc.heal)
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
	// and with single-slice intra refresh (the probe tries it on h264/hevc_nvenc only)
	notOnSingleSlice := map[string]string{"av1_nvenc": "single-slice-intra-refresh"}
	for enc := range c.options {
		e := encoderNamed(enc)
		for _, ir := range []string{"", IntraRefreshOn, IntraRefreshSingleSlice} {
			c.intraRefresh = map[string]string{enc: ir}
			wide.intraRefresh = c.intraRefresh
			wantNot := notOn[enc]
			if ir == IntraRefreshSingleSlice {
				wantNot = strings.TrimSpace(wantNot + " " + notOnSingleSlice[enc])
			}
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
									t.Errorf("%s (quality %q adaptive %v usage %q intra refresh %q): -%s %s dropped: FFmpeg 8.1 does not take the value", enc, q, adaptive, usage, ir, k, v)
								}
							} else if g != v {
								t.Errorf("%s (quality %q adaptive %v usage %q intra refresh %q): -%s %s, want %s", enc, q, adaptive, usage, ir, k, g, v)
							}
						}
						for k := range got {
							if _, ok := want[k]; !ok {
								t.Errorf("%s (quality %q adaptive %v usage %q intra refresh %q): unexpected -%s", enc, q, adaptive, usage, ir, k)
							}
						}
						sort.Strings(dropped)
						if d := strings.Join(dropped, " "); d != wantNot {
							t.Errorf("%s (quality %q adaptive %v usage %q intra refresh %q): options not passed %q, want %q", enc, q, adaptive, usage, ir, d, wantNot)
						}
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

// runFFmpeg runs ffmpeg and returns its stderr.
func runFFmpeg(ff string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := quietCmd(ctx, ff, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.ReplaceAll(stderr.String(), "\r", ""), err
}

// limitFrames makes a command line stop after a few frames and discards its
// output.
func limitFrames(args []string) []string {
	out := append([]string{}, args[:len(args)-1]...)
	return append(out, "-frames:v", "5", "-")
}

// nvencRuntimeError matches the NVIDIA driver or GPU missing, which FFmpeg
// reports only after all options were accepted.
var nvencRuntimeError = regexp.MustCompile(`Cannot load (nvcuda\.dll|libcuda\.so|nvEncodeAPI)|No capable devices found|No NVENC capable devices found|Driver does not support the required nvenc API version`)

// TestNVENCArgsAccepted runs the NVENC command lines the host builds (guide
// step 1.2: with each intra refresh mode) with the local FFmpeg: in the sandbox
// FFmpeg 6.1 on Linux and the FFmpeg 8.1 Windows build under Wine (the
// cross-compiled test binary). With an NVIDIA GPU (the encoder passed the
// probe) the probe's own mode must encode; elsewhere every mode must get past
// option parsing to the missing driver.
func TestNVENCArgsAccepted(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	caps, err := Probe(context.Background(), ff, nil)
	if err != nil && caps == nil {
		t.Skipf("probe: %v", err)
	}
	tested := 0
	for _, name := range []string{"av1_nvenc", "hevc_nvenc", "h264_nvenc"} {
		if caps.options[name] == nil {
			t.Logf("%s: not in this ffmpeg build", name)
			continue
		}
		tested++
		works := false // passed the probe's test encode: an NVIDIA GPU
		for _, w := range caps.Encoders {
			works = works || w.Name == name
		}
		modes := []string{caps.IntraRefresh(name)}
		if !works && intraRefreshEncoders[name] {
			modes = []string{"", IntraRefreshOn, IntraRefreshSingleSlice}
		}
		for _, ir := range modes {
			c := *caps
			c.intraRefresh = map[string]string{name: ir}
			p := Params{Source: Source{Backend: "test", NativeW: 1280, NativeH: 720}, Encoder: encoderNamed(name), FPS: 60,
				BitrateKbps: 20000, CaptureClock: caps.CanStampCapture()}
			args, err := c.BuildArgs(p)
			if err != nil {
				t.Fatal(err)
			}
			if (ir != "") != strings.Contains(strings.Join(args, " "), "-intra-refresh 1 ") {
				t.Fatalf("%s (intra refresh %q): %q", name, ir, args)
			}
			stderr, err := runFFmpeg(ff, limitFrames(args))
			switch {
			case ffmpegOptionError.MatchString(stderr):
				t.Errorf("%s (intra refresh %q): ffmpeg refused an option: %s\nargs: %q", name, ir, causeLines(stderr, 4), args)
			case err == nil:
				t.Logf("%s (intra refresh %q): encoded", name, ir)
			case works:
				t.Errorf("%s (intra refresh %q) passed the probe but failed: %v: %s", name, ir, err, causeLines(stderr, 4))
			case nvencRuntimeError.MatchString(stderr):
				t.Logf("%s (intra refresh %q): options accepted, then: %s", name, ir, nvencRuntimeError.FindString(stderr))
			default:
				t.Errorf("%s (intra refresh %q): unexpected failure: %v: %s", name, ir, err, causeLines(stderr, 4))
			}
		}
	}
	if tested == 0 {
		t.Skip("ffmpeg has no NVENC encoders")
	}
	// Control: a value FFmpeg refuses is caught before the driver loads.
	if caps.options["hevc_nvenc"] != nil {
		bad := []string{"-hide_banner", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=640x360:r=60", "-frames:v", "5",
			"-pix_fmt", "yuv420p", "-c:v", "hevc_nvenc", "-intra-refresh", "2", "-g", "30", "-f", "null", "-"}
		if stderr, _ := runFFmpeg(ff, bad); !ffmpegOptionError.MatchString(stderr) {
			t.Errorf("-intra-refresh 2 not refused: %s", causeLines(stderr, 4))
		}
	}
}

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
	run := func(args []string) (string, error) { return runFFmpeg(ff, args) }
	limit := limitFrames
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
