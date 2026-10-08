package media

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestParseFilters reads the capture and helper filters from the real
// "ffmpeg -filters" of the FFmpeg 8.1 Windows build, and checks
// "ffmpeg -h filter=vsrc_amf" for what BuildArgs sets.
func TestParseFilters(t *testing.T) {
	out, err := os.ReadFile("testdata/ffmpeg81-filters.txt")
	if err != nil {
		t.Fatal(err)
	}
	f := parseFilters(out)
	for _, name := range probeFilters {
		if !f[name] {
			t.Errorf("%s not found", name)
		}
	}
	// "pad" is only the tail of "tpad"; names that are not probed stay out.
	if f["pad"] || len(f) != len(probeFilters) {
		t.Fatalf("filters %v", f)
	}
	if f = parseFilters([]byte(" .. tpad              V->V       Temporarily pad video frames.\r\n")); len(f) != 0 {
		t.Fatalf("partial name matched: %v", f)
	}

	help, err := os.ReadFile("testdata/ffmpeg81-h-vsrc_amf.txt")
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]bool{"select": true, "settb": true, "setpts": true}
	if err := checkAMFCapture(help, all); err != nil {
		t.Fatal(err)
	}
	for f := range all {
		without := maps.Clone(all)
		delete(without, f)
		if err := checkAMFCapture(help, without); err == nil || !strings.Contains(err.Error(), f) {
			t.Fatalf("without %s: %v", f, err)
		}
	}
	// A vsrc_amf without capture modes (or another option BuildArgs sets).
	var noMode []string
	for _, l := range strings.Split(string(help), "\n") {
		if !strings.Contains(l, "wait_for_present") {
			noMode = append(noMode, l)
		}
	}
	if err := checkAMFCapture([]byte(strings.Join(noMode, "\n")), all); err == nil || !strings.Contains(err.Error(), "wait_for_present") {
		t.Fatalf("missing capture mode: %v", err)
	}
	if err := checkAMFCapture(bytes.ReplaceAll(help, []byte("monitor_index"), []byte("display_index")), all); err == nil {
		t.Fatal("missing monitor_index accepted")
	}
}

// TestBuildArgsAMF checks the AMD Direct Capture command line on the FFmpeg
// 8.1 Windows build's real filter list and encoder options: vsrc_amf's AMF
// surfaces go to the AMF encoder without any conversion, paced by framePacer,
// with the same encoder arguments as ddagrab; anything else is refused.
func TestBuildArgsAMF(t *testing.T) {
	c := caps81(t)
	pacer := "select='if(gte(st(1,time(0)-ld(0)),0)+lt(ld(1),-2/144),1+0*st(0,ld(0)+ld(1)+1/144-clip(ld(1),0,1/144)),0)'"
	if framePacer(144) != pacer {
		t.Fatalf("framePacer(144) = %s", framePacer(144))
	}
	for _, e := range c.Encoders {
		amf := Params{Source: Source{Backend: "amf", Output: 1, NativeW: 2560, NativeH: 1440}, Encoder: e, FPS: 144, BitrateKbps: 50000,
			Quality: "speed", CaptureClock: true}
		args, err := c.BuildArgs(amf)
		if e.Vendor != "amd" {
			if err == nil || !strings.Contains(err.Error(), "only feeds AMF encoders") {
				t.Errorf("%s: %v, args %q", e.Name, err, args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		i := slices.Index(args, "-filter_complex")
		if i < 0 || args[i+1] != "vsrc_amf=monitor_index=1:framerate=144:capture_mode=wait_for_present:duplicate_output=1,"+
			pacer+",settb=AVTB,setpts=time(0)*1000000[v]" {
			t.Fatalf("%s: filter graph %q", e.Name, args)
		}
		// The encoder arguments are those of a ddagrab session.
		dda := amf
		dda.Source.Backend = "ddagrab"
		ddaArgs, err := c.BuildArgs(dda)
		if err != nil {
			t.Fatal(err)
		}
		if j := slices.Index(ddaArgs, "-map"); j < 0 || !slices.Equal(args[i+2:], ddaArgs[j:]) {
			t.Fatalf("%s: encoder arguments differ from ddagrab:\n%q\n%q", e.Name, args, ddaArgs)
		}
		if !strings.Contains(strings.Join(args, " "), "-map [v] -c:v "+e.Name+" -usage ultralowlatency ") {
			t.Fatalf("%s: %q", e.Name, args)
		}
		if w, h := amf.OutputSize(); w != 2560 || h != 1440 {
			t.Fatalf("output size %dx%d", w, h)
		}
	}

	// Without capture stamps (CaptureClock false) the pts still come from the
	// wall clock in µs: vsrc_amf's own are rounded to 1/framerate and paced
	// frames could share one.
	hevc := EncoderInfo{"hevc_amf", "hevc", "amd", true}
	args, err := c.BuildArgs(Params{Source: Source{Backend: "amf"}, Encoder: hevc, FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(args, " "); !strings.Contains(j, "vsrc_amf=monitor_index=0:framerate=60:capture_mode=wait_for_present:duplicate_output=1,"+
		framePacer(60)+",settb=AVTB,setpts=time(0)*1000000[v] ") || !strings.Contains(j, " -enc_time_base 1:1000000 ") ||
		strings.Contains(j, "hwmap") || strings.Contains(j, "hwdownload") || strings.Contains(j, "format=") {
		t.Fatalf("args %s", j)
	}
	// ddagrab's frames are on its 1/framerate grid already: no retiming there.
	args, err = c.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: hevc, FPS: 60})
	if j := strings.Join(args, " "); err != nil || strings.Contains(j, "setpts") || strings.Contains(j, "enc_time_base") {
		t.Fatalf("ddagrab args %s: %v", j, err)
	}
	for _, bad := range []struct {
		p    Params
		want string
	}{
		{Params{Source: Source{Backend: "amf"}, Encoder: hevc, DrawCursor: true}, "cursor"},
		{Params{Source: Source{Backend: "amf", Output: 9}, Encoder: hevc}, "out of range"},
		{Params{Source: Source{Backend: "amf", Output: -1}, Encoder: hevc}, "out of range"},
		{Params{Source: Source{Backend: "amf"}, Encoder: EncoderInfo{"libx264", "h264", "software", false}}, "only feeds AMF encoders"},
	} {
		if _, err := c.BuildArgs(bad.p); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: %v, want %q", bad.p, err, bad.want)
		}
	}
	c.Filters["vsrc_amf"] = false
	if err := c.CanCaptureAMF(hevc); err == nil || !strings.Contains(err.Error(), "vsrc_amf") {
		t.Fatal(err)
	}
	if _, err := c.BuildArgs(Params{Source: Source{Backend: "amf"}, Encoder: hevc}); err == nil {
		t.Fatal("amf capture without vsrc_amf")
	}
}

// TestFramePacer runs framePacer's expression in a real ffmpeg on frames with
// made-up arrival times: its clock time(0) becomes t, the timestamps set by
// setpts. Then one real-time run with time(0).
func TestFramePacer(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	// count passes frames whose arrival time (s) at index N is arrival
	// through the pacer for fps and returns how many it lets through. The
	// clock starts at 1000 s: like the wall clock, far from the pacer's
	// initial due time 0.
	count := func(frames int, arrival string, fps int) int {
		t.Helper()
		pacer := strings.Replace(framePacer(fps), "time(0)", "t", 1)
		if pacer == framePacer(fps) {
			t.Fatal("no time(0) in framePacer")
		}
		graph := fmt.Sprintf("color=s=16x16:r=1000,trim=end_frame=%d,settb=AVTB,setpts='(1000+%s)/TB',%s,format=gray[v]", frames, arrival, pacer)
		cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-filter_complex", graph, "-map", "[v]",
			"-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %s", err, stderr.String())
		}
		return len(out) / (16 * 16)
	}
	for _, c := range []struct {
		name           string
		frames         int
		arrival        string
		fps            int
		min, max       int
		wantExactInput bool
	}{
		// 5 s of presents each.
		{"144 Hz display at 60 fps", 720, "N/144", 60, 300, 302, false},
		{"240 Hz at 60 fps", 1200, "N/240", 60, 300, 302, false},
		{"75 Hz at 60 fps", 375, "N/75", 60, 300, 302, false},
		{"61 Hz at 60 fps", 305, "N/61", 60, 300, 302, false},
		{"144 Hz at 144 fps", 720, "N/144", 144, 720, 720, true},
		{"60 Hz with 7 ms jitter at 60 fps", 300, "N/60+0.007*sin(N*2.7)", 60, 300, 300, true},
		{"59.94 Hz at 60 fps", 300, "N*1001/60000", 60, 300, 300, true},
		{"30 fps game at 60 fps", 150, "N/30", 60, 150, 150, true},
		{"irregular game frames at 144 fps", 500, "N/100+0.004*sin(N*1.3)", 144, 500, 500, true},
		// Wall-clock steps after 2 s: back by 10 s must not stall the stream
		// for 10 s, forward by 10 s must not lose frames.
		{"clock steps back", 720, "N/144-10*gte(N,288)", 60, 299, 303, false},
		{"clock steps forward", 720, "N/144+10*gte(N,288)", 60, 299, 303, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := count(c.frames, c.arrival, c.fps)
			t.Logf("%d of %d frames passed", n, c.frames)
			if n < c.min || n > c.max || c.wantExactInput && n != c.frames {
				t.Fatalf("%d of %d frames passed, want %d-%d", n, c.frames, c.min, c.max)
			}
		})
	}

	t.Run("real time", func(t *testing.T) {
		// 2 s of a 144 fps source in real time through the pacer for 60 fps.
		const seconds = 2
		cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin",
			"-filter_complex", fmt.Sprintf("color=s=16x16:r=144,trim=end_frame=%d,realtime,%s,format=gray[v]", 144*seconds, framePacer(60)),
			"-map", "[v]", "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		n := len(out) / (16 * 16)
		t.Logf("%d frames in %d s", n, seconds)
		if n < 60*seconds*9/10 || n > 60*seconds+3 {
			t.Fatalf("%d frames in %d s, want about %d", n, seconds, 60*seconds)
		}
	})
}

// TestAMFCapture runs the AMD Direct Capture command line BuildArgs builds for
// every AMF encoder with the ffmpeg on PATH when it has vsrc_amf (the FFmpeg
// 8.1 Windows build; skipped elsewhere): on an AMD GPU with Direct Capture it
// must stream (or wait for a present on a still desktop); without one (Wine,
// other GPUs) FFmpeg must accept every option and value and stop only when it
// opens the AMF runtime or the capture.
func TestAMFCapture(t *testing.T) {
	caps := probeOrSkip(t)
	if !caps.Filters["vsrc_amf"] {
		t.Skip("ffmpeg has no usable vsrc_amf filter")
	}
	runtimeErrors := []string{
		"DLL amfrt64.dll failed to open",                  // no AMD driver
		"Failed to create  hardware device context (AMF)", // (two spaces: FFmpeg's text)
		"CreateComponent(AMFDisplayCapture) failed",       // driver without Direct Capture
		"Failed to initialize capture component",          // e.g. the monitor index
		"Failed to get capture resolution from AMF",       // capture without a display
		"CreateComponent(AMFVideoEncoder",                 // GPU without this codec
		"Failed to initialize hardware frames context",    // AMF frames
	}
	for _, e := range candidates {
		if e.Vendor != "amd" {
			continue
		}
		// The capture clock is in the chain with and without capture stamps.
		for _, stamps := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stamps=%v", e.Name, stamps), func(t *testing.T) {
				p := Params{Source: Source{Backend: "amf"}, Encoder: e, FPS: 60, BitrateKbps: 20000, Quality: "speed", CaptureClock: stamps}
				args, err := caps.BuildArgs(p)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, caps.FFmpeg, args...)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err = cmd.Run()
				msg := strings.ReplaceAll(stderr.String(), "\r", "")
				switch {
				case ctx.Err() != nil && stdout.Len() > 0:
					t.Logf("captured: %d bytes of NUT in 10 s", stdout.Len())
				case ctx.Err() != nil:
					t.Logf("no frame in 10 s (no present on a still desktop?): %s", msg)
				case err == nil:
					t.Fatalf("ffmpeg ended without an error: %s", msg)
				case e.Name == "av1_amf" && strings.Contains(msg, `"header_insertion_mode" option value "idr"`):
					// A3: av1_amf only takes none|gop|frame; step 1.1 replaces the
					// AMD encoder arguments.
					t.Skip("av1_amf rejects -header_insertion_mode idr (A3, fixed by the 1.1 AMD arguments)")
				default:
					for _, r := range runtimeErrors {
						if strings.Contains(msg, r) {
							t.Logf("stopped at the AMF runtime: %s", r)
							return
						}
					}
					t.Fatalf("%v: %s", err, msg)
				}
			})
		}
	}
}

// TestVideoFailureParams checks that an encoder failure event carries the
// parameters of the generation that failed (the session falls back from
// capture "amf" on them).
func TestVideoFailureParams(t *testing.T) {
	caps := probeOrSkip(t)
	v := NewVideo(caps, nil, func() uint64 { return 0 })
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: 64, NativeH: 64}, Encoder: EncoderInfo{Name: "no_such_encoder", Family: "h264"}, FPS: 30}
	if err := v.Start(p, true); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-v.Events():
		if ev.Err == nil || ev.Failed.Encoder.Name != "no_such_encoder" || ev.Failed.Source.Backend != "test" {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no failure event")
	}
}
