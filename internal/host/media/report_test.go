package media

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// caps81 returns Caps for the FFmpeg 8.1 Windows build (BtbN win64 GPL, the
// one the installer downloads) from its real "ffmpeg -version",
// "ffmpeg -filters", "ffmpeg -h filter=vsrc_amf" and "ffmpeg -h encoder=..."
// output in testdata, with every encoder usable.
func caps81(t *testing.T) *Caps {
	t.Helper()
	c := &Caps{
		FFmpeg:       `C:\Program Files\KlouditRecon\ffmpeg\bin\ffmpeg.exe`,
		Rejected:     map[string]string{"av1_qsv": "exit status 0xb1b4b1ab: Error creating a MFX session: -9."},
		options:      map[string]map[string]bool{},
		optValues:    map[string]map[string]map[string]bool{},
		captureClock: true,
	}
	out, err := os.ReadFile("testdata/ffmpeg81-version.txt")
	if err != nil {
		t.Fatal(err)
	}
	c.Version, c.VersionInfo = parseVersion(out)
	if out, err = os.ReadFile("testdata/ffmpeg81-filters.txt"); err != nil {
		t.Fatal(err)
	}
	c.Filters = parseFilters(out) // as Probe does
	if out, err = os.ReadFile("testdata/ffmpeg81-h-vsrc_amf.txt"); err != nil {
		t.Fatal(err)
	}
	if err := checkAMFCapture(out, c.Filters); err != nil {
		t.Fatal(err)
	}
	for _, e := range candidates {
		help, err := os.ReadFile(filepath.Join("testdata", "ffmpeg81-h-"+e.Name+".txt"))
		if err != nil {
			continue
		}
		c.options[e.Name], c.optValues[e.Name] = parseEncoderHelp(string(help))
		c.Encoders = append(c.Encoders, e)
	}
	return c
}

func TestParseFFmpegOutput(t *testing.T) {
	c := caps81(t)
	if c.Version != "ffmpeg version n8.1.3-14-g330caae0c1-20261006 Copyright (c) 2000-2026 the FFmpeg developers" {
		t.Fatalf("version %q", c.Version)
	}
	// Version, compiler, seven libraries; no configure line, no CR, no
	// "Exiting with exit code 0".
	if len(c.VersionInfo) != 9 || c.VersionInfo[0] != c.Version || !strings.HasPrefix(c.VersionInfo[1], "built with gcc") ||
		c.VersionInfo[3] != "libavcodec     62. 28.103 / 62. 28.103" {
		t.Fatalf("version info %q", c.VersionInfo)
	}
	for _, l := range c.VersionInfo {
		if strings.Contains(l, "configuration") || strings.Contains(l, "Exiting") || strings.ContainsAny(l, "\r\n") {
			t.Fatalf("version info line %q", l)
		}
	}
	want := map[string]int{"av1_amf": 47, "hevc_amf": 46, "h264_amf": 48, "av1_nvenc": 45, "hevc_nvenc": 54, "h264_nvenc": 52, "libx264": 48, "libsvtav1": 5}
	for name, n := range want {
		if len(c.options[name]) != n {
			t.Errorf("%s: %d options, want %d", name, len(c.options[name]), n)
		}
	}
	for _, o := range []struct {
		enc, opt string
		has      bool
	}{
		{"hevc_amf", "header_insertion_mode", true}, {"av1_amf", "async_depth", true}, {"h264_amf", "frame_skipping", true},
		{"h264_amf", "skip_frame", false}, {"hevc_nvenc", "intra-refresh", true}, {"libsvtav1", "svtav1-params", true},
		{"av1_amf", "lowest_latency", false}, // a value of -latency, not an option
		{"libsvtav1", "auto", false},
	} {
		if c.HasOption(o.enc, o.opt) != o.has {
			t.Errorf("%s -%s: listed %v, want %v", o.enc, o.opt, !o.has, o.has)
		}
	}
}

// TestWriteReport checks the probe report: version lines, and under each
// usable encoder the exact command line BuildArgs builds for the sample.
func TestWriteReport(t *testing.T) {
	c := caps81(t)
	// What the probe measures on RDNA3 (step 1.7) and on an NVIDIA GPU with
	// single slice intra refresh (step 1.2).
	c.SetAlignment("av1_amf", Alignment{W: 64, H: 16, ProbeW: 1920, ProbeH: 1080, CodedW: 1920, CodedH: 1082})
	c.intraRefresh = map[string]string{"hevc_nvenc": IntraRefreshSingleSlice}
	sample := Params{Source: Source{Backend: "ddagrab"}, FPS: 60, BitrateKbps: 30000, Quality: "balanced", Adaptive: true, CaptureClock: true}
	each := func(e EncoderInfo) Params {
		p := sample
		p.Encoder = e
		return p
	}
	var buf bytes.Buffer
	c.WriteReport(&buf, each)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	t.Logf("report:\n%s", buf.String())

	want := []string{`ffmpeg:     C:\Program Files\KlouditRecon\ffmpeg\bin\ffmpeg.exe`}
	for _, l := range c.VersionInfo {
		want = append(want, "            "+l)
	}
	want = append(want,
		"capture:    ddagrab=true gfxcapture=true vsrc_amf=true",
		"session:    ddagrab output 0 at its native size, 60 fps, 30 Mbit/s, quality balanced, adaptive bitrate, capture timestamps (command lines below)")
	if len(lines) < len(want) || !slices.Equal(lines[:len(want)], want) {
		t.Fatalf("report head:\n%s\nwant:\n%s", strings.Join(lines[:min(len(lines), len(want))], "\n"), strings.Join(want, "\n"))
	}
	lines = lines[len(want):]
	for _, e := range c.Encoders {
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "encoder:    "+e.Name+" ") {
			t.Fatalf("expected %s, got %q", e.Name, lines)
		}
		args, err := c.BuildArgs(each(e))
		if err != nil {
			t.Fatal(err)
		}
		cmd, ok := strings.CutPrefix(lines[1], "            ffmpeg ")
		if got := splitCommand(cmd); !ok || !slices.Equal(got, args) {
			t.Fatalf("%s: command line %q\nparses as %q\nwant %q", e.Name, lines[1], got, args)
		}
		lines = lines[2:]
		if e.Name == "av1_amf" {
			if len(lines) == 0 || lines[0] != "            pads: coded 1920x1080 as 1920x1082; sessions at sizes that are not multiples of 64x16 use HEVC or H.264" {
				t.Fatalf("av1_amf: no padding line: %q", lines)
			}
			lines = lines[1:]
		}
	}
	if !slices.Equal(lines, []string{"unusable:   av1_qsv      exit status 0xb1b4b1ab: Error creating a MFX session: -9."}) {
		t.Fatalf("report tail %q", lines)
	}

	out := buf.String()
	for _, s := range []string{
		// Zero-copy D3D11 input for AMF and NVENC, filter graph quoted.
		` -filter_complex "ddagrab=output_idx=0:framerate=60:draw_mouse=0:dup_frames=0,settb=AVTB,setpts=time(0)*1000000[v]" -map "[v]" -c:v hevc_amf `,
		// Software encoders download the frames.
		`dup_frames=0,settb=AVTB,setpts=time(0)*1000000,hwdownload,format=bgra,format=yuv420p[v]" -map "[v]" -c:v libsvtav1 `,
		// Explicit CBR for SVT-AV1 with low-delay prediction.
		` -c:v libsvtav1 -preset 12 -svtav1-params pred-struct=1:lookahead=0:scd=0:rc=2 -b:v 30000k `,
		` -c:v hevc_nvenc -preset p3 -tune ull -rc cbr `,
		// Intra refresh with its period as -g (half a second at 60 fps).
		` -intra-refresh 1 -single-slice-intra-refresh 1 `,
		"encoder:    hevc_nvenc   hevc  nvidia intra-refresh=single-slice\n",
		// AMF: CBR while the bitrate adapts (step 1.1).
		` -c:v hevc_amf -usage ultralowlatency -quality balanced -rc cbr -enforce_hrd 0 `,
		` -enc_time_base 1:1000000 -fps_mode passthrough -an -sn -dn -flush_packets 1 -f nut -write_index 0 pipe:1`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q", s)
		}
	}

	// A sample whose source differs between encoders (capture "amf": AMD
	// Direct Capture for the AMF encoders) gets a session line per group.
	buf.Reset()
	c.WriteReport(&buf, func(e EncoderInfo) Params {
		p := each(e)
		if c.CanCaptureAMF(e) == nil {
			p.Source.Backend = "amf"
		}
		return p
	})
	var groups []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if s, ok := strings.CutPrefix(l, "session:    "); ok {
			groups = append(groups, s[:strings.Index(s, ",")])
		} else if s, ok := strings.CutPrefix(l, "encoder:    "); ok {
			groups = append(groups, strings.Fields(s)[0])
		}
	}
	if want := []string{"ddagrab output 0 at its native size", "av1_nvenc", "hevc_nvenc", "h264_nvenc",
		"AMD Direct Capture (vsrc_amf) of output 0 at its native size", "av1_amf", "hevc_amf", "h264_amf",
		"ddagrab output 0 at its native size", "libx264", "libsvtav1"}; !slices.Equal(groups, want) {
		t.Fatalf("session lines and encoders %q, want %q", groups, want)
	}
	if !strings.Contains(buf.String(), "encoder:    hevc_amf     hevc  amd\n            ffmpeg -hide_banner") || !strings.Contains(buf.String(), "vsrc_amf=") {
		t.Fatalf("no AMD Direct Capture command line:\n%s", buf.String())
	}

	// A build without the sample's capture filter says why instead.
	c.Filters["ddagrab"] = false
	buf.Reset()
	c.WriteReport(&buf, each)
	if !strings.Contains(buf.String(), "encoder:    av1_amf      av1   amd\n            no command line: this ffmpeg build lacks the ddagrab filter") {
		t.Fatalf("report without ddagrab:\n%s", buf.String())
	}

	// gfxcapture is new in FFmpeg 8.1 (8.0's libavfilter/allfilters.c lacks it).
	c.Filters["gfxcapture"] = false
	if _, err := c.BuildArgs(Params{Source: Source{Backend: "gfxcapture"}, Encoder: c.Encoders[0]}); err == nil || !strings.Contains(err.Error(), "need FFmpeg >= 8.1") {
		t.Fatalf("gfxcapture missing: %v", err)
	}

	// The Linux sample: test pattern, no capture stamps, cursor drawn.
	if s := describeSample(Params{Source: Source{Backend: "test", NativeW: 1280, NativeH: 720}, FPS: 60, BitrateKbps: 2500, DrawCursor: true}); s != "test pattern 1280x720, 60 fps, 2.5 Mbit/s, quality default, cursor drawn" {
		t.Fatalf("describeSample: %q", s)
	}
	if s := describeSample(Params{Source: Source{Backend: "gfxcapture", NativeW: 2560, NativeH: 1440}, FPS: 60, BitrateKbps: 30000, Quality: "balanced"}); s != "gfxcapture of a 2560x1440 monitor, 60 fps, 30 Mbit/s, quality balanced" {
		t.Fatalf("describeSample: %q", s)
	}
}

// splitCommand splits a command line the way a POSIX shell does for the
// quoting commandLine uses (bare words, "..." without escapes, '...').
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// TestCommandLineShells pastes command lines into real shells (sh, and
// PowerShell when installed) and checks the arguments they pass on.
func TestCommandLineShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("no printf")
	}
	c := caps81(t)
	var sets [][]string
	for _, e := range c.Encoders {
		args, err := c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: e, FPS: 60, BitrateKbps: 30000, CaptureClock: true})
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, args)
	}
	// The other sources the probe's sample can have.
	for _, p := range []Params{
		{Source: Source{Backend: "test", NativeW: 1280, NativeH: 720}, Encoder: c.Encoders[6], Barcode: true, TestPad: 8},
		{Source: Source{Backend: "gfxcapture", HMonitor: 65537, NativeW: 2560, NativeH: 1440}, Encoder: c.Encoders[1]},
		{Source: Source{Backend: "amf", Output: 1}, Encoder: c.Encoders[4]},
	} {
		p.FPS, p.BitrateKbps, p.CaptureClock = 60, 30000, true
		args, err := c.BuildArgs(p)
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, args)
	}
	sets = append(sets, []string{"a b", "x,y;z", "it's", "time(0)*2", "[v]", "a=b:c=d", "-c:v", "1:1000000", ""})
	posixOnly := []string{"$HOME", "`id`", `C:\dir`, "!!", "-b:v", "pipe:1"}
	for i, args := range append(sets, posixOnly) {
		line := commandLine(printf+` '%s\n'`, args)
		check := func(shell string, argv ...string) {
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v: %s", shell, err, out)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			if !slices.Equal(got, args) {
				t.Fatalf("%s: %s\npassed %q\nwant   %q", shell, line, got, args)
			}
		}
		check("sh", "sh", "-c", line)
		if i == len(sets) { // single-quoted fallback: POSIX shells only
			continue
		}
		if pwsh, err := exec.LookPath("pwsh"); err == nil {
			check("pwsh", pwsh, "-NoProfile", "-NonInteractive", "-Command", "& "+line)
		}
	}
}
