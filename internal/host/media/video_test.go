package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/nut"
)

func probeOrSkip(t *testing.T) *Caps {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	caps, err := Probe(context.Background(), ff, nil)
	if err != nil {
		t.Skipf("probe: %v", err)
	}
	return caps
}

func TestProbe(t *testing.T) {
	caps := probeOrSkip(t)
	t.Logf("%s", caps.Version)
	for _, e := range caps.Encoders {
		t.Logf("encoder %-12s family=%s vendor=%s", e.Name, e.Family, e.Vendor)
	}
	if !caps.HasOption("libx264", "preset") && caps.options["libx264"] != nil {
		t.Fatal("option parsing failed for libx264")
	}
}

func TestBuildArgsEscaping(t *testing.T) {
	caps := &Caps{Filters: map[string]bool{"gfxcapture": true, "ddagrab": true}, options: map[string]map[string]bool{
		"h264_nvenc": {"preset": true, "tune": true, "rc": true, "zerolatency": true, "delay": true, "forced-idr": true},
	}}
	enc := EncoderInfo{"h264_nvenc", "h264", "nvidia", true}
	args, err := caps.BuildArgs(Params{Source: Source{Backend: "gfxcapture", Window: "Game (DX12)"}, Encoder: enc, FPS: 120, BitrateKbps: 50000})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "window_title=") || !strings.Contains(joined, "-tune ull") || !strings.Contains(joined, "-f nut") {
		t.Fatalf("unexpected args: %s", joined)
	}
	if _, err := caps.BuildArgs(Params{Source: Source{Backend: "gfxcapture", Window: "x,movie=/etc/passwd"}, Encoder: enc}); err == nil {
		t.Fatal("filtergraph injection not rejected")
	}
	args, _ = caps.BuildArgs(Params{Source: Source{Backend: "ddagrab", Output: 1}, Encoder: enc, FPS: 144, BitrateKbps: 30000, Quality: "speed"})
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "ddagrab=output_idx=1:framerate=144:draw_mouse=0:dup_frames=0[v]") {
		t.Fatalf("ddagrab chain: %s", joined)
	}
	if strings.Contains(joined, "hwdownload") {
		t.Fatal("NVENC path must stay on the GPU")
	}
	if strings.Contains(joined, "setpts") || strings.Contains(joined, "enc_time_base") {
		t.Fatal("capture clock without CaptureClock")
	}
	args, _ = caps.BuildArgs(Params{Source: Source{Backend: "ddagrab", Output: 1}, Encoder: enc, FPS: 144, BitrateKbps: 30000, CaptureClock: true})
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "dup_frames=0,settb=AVTB,setpts=time(0)*1000000[v]") || !strings.Contains(joined, "-enc_time_base 1:1000000") {
		t.Fatalf("capture clock args: %s", joined)
	}
}

// TestCaptureClock runs real encoders with CaptureClock and checks that every
// frame gets a plausible capture time (capture -> encoded a few ms) and that
// the frame rate and bitrate match a run without it.
func TestCaptureClock(t *testing.T) {
	caps := probeOrSkip(t)
	if !caps.CanStampCapture() {
		if caps.Filters["settb"] && caps.Filters["setpts"] {
			t.Fatalf("probe rejected %s: %v", CaptureClockFilter, testCaptureClock(context.Background(), caps.FFmpeg, CaptureClockFilter))
		}
		t.Skip("ffmpeg lacks settb/setpts")
	}
	// The probe must reject an expression this build cannot evaluate (e.g. a
	// constant a future FFmpeg drops), so only the stamps are lost.
	if err := testCaptureClock(context.Background(), caps.FFmpeg, "settb=AVTB,setpts=NO_SUCH_CONST"); err == nil {
		t.Fatal("probe accepted an invalid capture clock expression")
	}
	for _, name := range []string{"libx264", "libsvtav1"} {
		var enc EncoderInfo
		for _, e := range caps.Encoders {
			if e.Name == name {
				enc = e
			}
		}
		if enc.Name == "" {
			t.Logf("%s not available", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			type run struct {
				fps, mbps     float64
				p50, p95, max float64 // capture -> encoded, ms
			}
			measure := func(stamp bool) run {
				start := time.Now()
				clock := NewClock()
				v := NewVideo(caps, nil, clock)
				defer v.Stop()
				p := Params{Source: Source{Backend: "test", NativeW: 960, NativeH: 540}, Encoder: enc, FPS: 60, BitrateKbps: 4000, CaptureClock: stamp}
				if err := v.Start(p, false); err != nil {
					t.Fatal(err)
				}
				var lat []float64
				var bytes, n int
				var first time.Time
				deadline := time.After(30 * time.Second)
				for n < 300 {
					select {
					case ev := <-v.Events():
						if ev.Err != nil {
							t.Fatal(ev.Err)
						}
						f := ev.Frame
						if f == nil {
							continue
						}
						if n == 0 {
							first = time.Now()
						}
						n++
						if n > 30 { // past encoder start-up
							bytes += len(f.Data)
						}
						if stamp {
							if f.CaptureUs == 0 || f.CaptureUs > f.EncodeDoneUs {
								t.Fatalf("frame %d: capture %d encoded %d", f.Seq, f.CaptureUs, f.EncodeDoneUs)
							}
							if n > 30 {
								lat = append(lat, float64(f.EncodeDoneUs-f.CaptureUs)/1000)
							}
						} else if f.CaptureUs != 0 {
							t.Fatal("capture time without CaptureClock")
						}
					case <-deadline:
						t.Fatalf("timeout after %d frames (%v)", n, time.Since(start))
					}
				}
				el := time.Since(first).Seconds()
				r := run{fps: float64(n-1) / el, mbps: float64(bytes) * 8 / (float64(n-31) / 60) / 1e6}
				if stamp {
					sort.Float64s(lat)
					r.p50, r.p95, r.max = lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1]
				}
				return r
			}
			base := measure(false)
			got := measure(true)
			t.Logf("%s without: %.1f fps %.2f Mbit/s; with: %.1f fps %.2f Mbit/s, capture->encoded p50 %.2f p95 %.2f max %.2f ms",
				name, base.fps, base.mbps, got.fps, got.mbps, got.p50, got.p95, got.max)
			if got.fps < 40 || got.fps > 75 {
				t.Fatalf("frame rate %.1f with capture clock", got.fps)
			}
			if r := got.mbps / base.mbps; r < 0.8 || r > 1.25 {
				t.Fatalf("bitrate changed: %.2f vs %.2f Mbit/s", got.mbps, base.mbps)
			}
			if got.p50 <= 0 || got.p50 > 100 {
				t.Fatalf("implausible capture->encoded p50 %.2f ms", got.p50)
			}
		})
	}
}

// TestCaptureClockFollowsWallClock steps the host clock 500 ms ahead of the
// wall clock in the middle of a generation (as a W32Time step or long drift
// would) and checks that the capture stamps recover once the offset is
// re-measured instead of staying off by the step for the rest of the generation.
func TestCaptureClockFollowsWallClock(t *testing.T) {
	caps := probeOrSkip(t)
	enc, ok := caps.Best("h264")
	if !ok || !caps.CanStampCapture() {
		t.Skip("no h264 encoder or capture clock")
	}
	const step = 500_000 // µs
	base := NewClock()
	var jump atomic.Uint64
	v := NewVideo(caps, nil, func() uint64 { return base() + jump.Load() })
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: 320, NativeH: 180}, Encoder: enc, FPS: 60, BitrateKbps: 1000, CaptureClock: true}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	var stepped time.Time
	var after []float64 // capture -> encoded (ms), frames encoded >= 1.5 s after the step
	deadline := time.After(30 * time.Second)
	for len(after) < 60 {
		select {
		case ev := <-v.Events():
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			f := ev.Frame
			if f == nil {
				continue
			}
			if f.Seq == 30 {
				jump.Store(step)
				stepped = time.Now()
			}
			if !stepped.IsZero() && time.Since(stepped) >= 1500*time.Millisecond {
				if f.CaptureUs == 0 {
					t.Fatalf("frame %d: no capture stamp", f.Seq)
				}
				after = append(after, float64(f.EncodeDoneUs-f.CaptureUs)/1000)
			}
		case <-deadline:
			t.Fatalf("timeout (%d frames after the step)", len(after))
		}
	}
	sort.Float64s(after)
	t.Logf("capture->encoded after a %d ms host clock step: p50 %.2f max %.2f ms", step/1000, after[len(after)/2], after[len(after)-1])
	if after[len(after)/2] > step/1000/2 {
		t.Fatalf("capture stamps still off by the clock step: p50 %.2f ms", after[len(after)/2])
	}
}

func TestVideoGenerations(t *testing.T) {
	caps := probeOrSkip(t)
	enc, ok := caps.Best("h264")
	if !ok {
		t.Skip("no h264 encoder")
	}
	start := time.Now()
	clock := func() uint64 { return uint64(time.Since(start).Microseconds()) }
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	v := NewVideo(caps, log, clock)
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: 640, NativeH: 360}, Encoder: enc, FPS: 60, BitrateKbps: 4000}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	var gens []uint8
	frames := map[uint8]int{}
	restarted := false
	firstFrameAt := time.Time{}
	for {
		select {
		case ev := <-v.Events():
			switch {
			case ev.Err != nil:
				t.Fatal(ev.Err)
			case ev.Config != nil:
				gens = append(gens, ev.Config.Gen)
				if !strings.HasPrefix(ev.Config.Codec, "avc1.") || ev.Config.Width != 640 {
					t.Fatalf("bad config %+v", ev.Config)
				}
			case ev.Frame != nil:
				f := ev.Frame
				if frames[f.Gen] == 0 && !f.Key {
					t.Fatalf("generation %d did not start with a key frame", f.Gen)
				}
				frames[f.Gen]++
				if firstFrameAt.IsZero() {
					firstFrameAt = time.Now()
				}
				if !restarted && frames[f.Gen] == 60 {
					restarted = true
					p.BitrateKbps = 2000
					if err := v.Start(p, false); err != nil {
						t.Fatal(err)
					}
				}
				if len(gens) == 2 && frames[gens[1]] == 60 {
					rate := float64(frames[gens[0]]+frames[gens[1]]) / time.Since(firstFrameAt).Seconds()
					t.Logf("gens=%v frames=%v rate=%.1f fps", gens, frames, rate)
					if rate < 40 || rate > 75 {
						t.Fatalf("frame rate %.1f not near 60", rate)
					}
					return
				}
			}
		case <-deadline:
			t.Fatalf("timeout: gens=%v frames=%v", gens, frames)
		}
	}
}

// TestVideoFailureEvent: a failing generation's error event carries its
// parameters (the session decides the encoder fallback and the fallback from
// capture "amf" from them, after the generation is gone from Current),
// whether it had gone live and whether the encoder itself failed.
func TestVideoFailureEvent(t *testing.T) {
	caps := probeOrSkip(t)
	enc, ok := caps.Best("h264")
	if !ok {
		t.Skip("no h264 encoder")
	}
	start := time.Now()
	v := NewVideo(caps, nil, func() uint64 { return uint64(time.Since(start).Microseconds()) })
	defer v.Stop()
	failure := func(what string) VideoEvent {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case ev := <-v.Events():
				if ev.Err != nil {
					return ev
				}
			case <-deadline:
				t.Fatalf("%s: no failure event", what)
			}
		}
	}

	// Fails to start: FFmpeg does not know the encoder.
	bad := EncoderInfo{Name: "recon_no_such_encoder", Family: "h264", Vendor: "software"}
	p := Params{Source: Source{Backend: "test", NativeW: 320, NativeH: 180}, Encoder: bad, FPS: 30, BitrateKbps: 1000}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	ev := failure("start failure")
	if ev.Failed == nil || ev.Failed.Encoder.Name != bad.Name || ev.Failed.Source.Backend != "test" || ev.Live || !ev.EncoderFault {
		t.Fatalf("start failure: %v: failed %+v live %v fault %v, want %s, not live, encoder fault", ev.Err, ev.Failed, ev.Live, ev.EncoderFault, bad.Name)
	}
	if _, ok := v.Current(); ok {
		t.Fatal("the failed generation is still current")
	}

	// Fails to start in its source (the filter graph cannot be configured,
	// as when ddagrab cannot duplicate the desktop): not the encoder's fault.
	p.Encoder = enc
	p.Source.NativeW, p.Source.NativeH = 40000, 40000
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	ev = failure("source failure")
	if ev.Failed == nil || ev.Failed.Encoder.Name != enc.Name || ev.Live || ev.EncoderFault {
		t.Fatalf("source failure: %v: failed %+v live %v fault %v, want %s, not live, no encoder fault", ev.Err, ev.Failed, ev.Live, ev.EncoderFault, enc.Name)
	}
	p.Source.NativeW, p.Source.NativeH = 320, 180

	// Fails while live: the encoder process dies after its first key frame.
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	for live := false; !live; {
		select {
		case ev := <-v.Events():
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			live = ev.Config != nil
		case <-time.After(20 * time.Second):
			t.Fatal("encoder did not start")
		}
	}
	v.mu.Lock()
	proc := v.active.cmd.Process
	v.mu.Unlock()
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	ev = failure("live failure")
	if ev.Failed == nil || ev.Failed.Encoder.Name != enc.Name || !ev.Live || ev.EncoderFault {
		t.Fatalf("live failure: %v: failed %+v live %v fault %v, want %s, live, no encoder fault", ev.Err, ev.Failed, ev.Live, ev.EncoderFault, enc.Name)
	}

	// Never sends a key frame (a wrapper drops them from FFmpeg's output):
	// the generation cannot go live and must fail instead of hanging, as an
	// encoder fault (the session excludes an encoder after two).
	if runtime.GOOS == "windows" {
		return
	}
	wrapper := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n# all arguments but the last (pipe:1), then drop the key frames\n" +
		"n=$#; i=0; for a in \"$@\"; do i=$((i+1)); [ $i -lt $n ] && set -- \"$@\" \"$a\"; done; shift $n\n" +
		"exec " + caps.FFmpeg + " \"$@\" -bsf:v noise=drop=key pipe:1\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	noKeys := *caps
	noKeys.FFmpeg = wrapper
	v2 := NewVideo(&noKeys, nil, func() uint64 { return uint64(time.Since(start).Microseconds()) })
	defer v2.Stop()
	if err := v2.Start(p, false); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-v2.Events():
		if ev.Err == nil || !strings.Contains(ev.Err.Error(), "without a key frame") || ev.Failed == nil || ev.Live || !ev.EncoderFault {
			t.Fatalf("no key frames: event %+v", ev)
		}
		t.Logf("no key frames: %v", ev.Err)
	case <-time.After(20 * time.Second):
		t.Fatal("no key frames: no failure event")
	}
	if _, ok := v2.Current(); ok {
		t.Fatal("the generation without key frames is still current")
	}
}

// TestEncoderFault classifies the stderr of real FFmpeg 8.1 (BtbN win64)
// failures, recorded under Wine (no GPU) in testdata/ffmpeg81-stderr-*.txt,
// as the encoder process's stderr ring holds it.
func TestEncoderFault(t *testing.T) {
	for _, tc := range []struct {
		file, enc string
		want      bool
	}{
		{"hevc_amf-open", "hevc_amf", true},     // AMF runtime missing: the encoder cannot open
		{"hevc_nvenc-open", "hevc_nvenc", true}, // CUDA driver missing
		{"av1_amf-option", "av1_amf", true},     // an option value refused (the pre-1.1 av1 arguments)
		{"unknown-encoder", "recon_no_such_encoder", true},
		{"ddagrab-hevc_amf", "hevc_amf", false}, // ddagrab cannot create its device
		{"source-libx264", "libx264", false},    // the source filter fails to configure
	} {
		f, err := os.Open(filepath.Join("testdata", "ffmpeg81-stderr-"+tc.file+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		r := &stderrRing{}
		r.consume(bufio.NewReader(f))
		f.Close()
		if got := encoderFault(r.String(), tc.enc); got != tc.want {
			t.Errorf("%s: encoder fault %v, want %v: %s", tc.file, got, tc.want, r.String())
		}
	}
	if encoderFault("EOF", "hevc_amf") || encoderFault("", "hevc_amf") {
		t.Error("a process that died without a message counted as an encoder fault")
	}
}

// TestAlignment checks the alignment derived from a probe measurement and
// which sizes pad: RDNA3's AV1 alignment of 64x16 keeps 2560x1440, 3840x2160,
// 1280x720 and 2560x1600, and pads 1920x1080 (height) and 3440x1440 (width).
func TestAlignment(t *testing.T) {
	for _, c := range []struct{ n, coded, def, want int }{
		{1920, 1920, 64, 64}, {1080, 1082, 16, 16}, {1080, 1088, 16, 16}, {3440, 3456, 64, 64},
		{1920, 2048, 64, 256}, {1080, 1152, 16, 128},
	} {
		if got := alignmentFor(c.n, c.coded, c.def); got != c.want {
			t.Errorf("alignmentFor(%d, %d, %d) = %d, want %d", c.n, c.coded, c.def, got, c.want)
		}
	}
	c := &Caps{}
	if a := c.Alignment("av1_amf"); a.W != 1 || a.H != 1 || c.Pads("av1_amf", 1920, 1080) {
		t.Fatalf("unprobed encoder: %+v", a)
	}
	c.SetAlignment("av1_amf", Alignment{W: 64, H: 16})
	for _, s := range []struct {
		w, h int
		pads bool
	}{
		{2560, 1440, false}, {3840, 2160, false}, {1280, 720, false}, {2560, 1600, false},
		{1920, 1080, true}, {3440, 1440, true}, {0, 0, false},
	} {
		if c.Pads("av1_amf", s.w, s.h) != s.pads {
			t.Errorf("%dx%d: pads %v, want %v", s.w, s.h, !s.pads, s.pads)
		}
	}
	if c.Pads("hevc_amf", 1920, 1080) {
		t.Fatal("hevc_amf has no alignment")
	}
}

// TestOutputSize compares Params.OutputSize with the size FFmpeg actually
// encodes: the x11grab scale (aspect ratio kept, even sizes) runs through a
// real ffmpeg on a source of the native size.
func TestOutputSize(t *testing.T) {
	for _, c := range []struct {
		p    Params
		w, h int
	}{
		{Params{Source: Source{Backend: "test"}}, 1280, 720},
		{Params{Source: Source{Backend: "test", NativeW: 960, NativeH: 540}, TestPad: 16}, 960, 556},
		{Params{Source: Source{Backend: "ddagrab", NativeW: 1920, NativeH: 1080}}, 1920, 1080},
		{Params{Source: Source{Backend: "gfxcapture", NativeW: 3440, NativeH: 1440}}, 3440, 1440},
		{Params{Source: Source{Backend: "gfxcapture", NativeW: 3440, NativeH: 1440}, Width: 2560, Height: 1440}, 2560, 1440},
		{Params{Source: Source{Backend: "gfxcapture", Window: "Game", NativeW: 3440, NativeH: 1440}}, 0, 0},
		{Params{Source: Source{Backend: "x11grab", NativeW: 1920, NativeH: 1080}}, 1920, 1080},
	} {
		if w, h := c.p.OutputSize(); w != c.w || h != c.h {
			t.Errorf("%+v: %dx%d, want %dx%d", c.p, w, h, c.w, c.h)
		}
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	for _, s := range [][4]int{{3440, 1440, 1920, 1080}, {1920, 1080, 1280, 720}, {1366, 768, 1280, 720}, {2560, 1080, 1920, 1080},
		{1280, 1024, 1920, 1080}, {1920, 1200, 1600, 900}, {1001, 777, 640, 480}, {1001, 777, 1920, 1080}} {
		p := Params{Source: Source{Backend: "x11grab", NativeW: s[0], NativeH: s[1]}, Width: s[2], Height: s[3]}
		w, h := p.OutputSize()
		// The chain BuildArgs uses for x11grab with a software encoder.
		cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", fmt.Sprintf("color=s=%dx%d", s[0], s[1]),
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p", s[2], s[3]),
			"-c:v", "rawvideo", "-f", "nut", "pipe:1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		d := nut.NewDemuxer(bytes.NewReader(out), 64<<20)
		if _, err := d.ReadPacket(); err != nil {
			t.Fatal(err)
		}
		if st := d.Streams()[0]; st.Width != w || st.Height != h {
			t.Errorf("x11grab %dx%d scaled into %dx%d: ffmpeg %dx%d, OutputSize %dx%d", s[0], s[1], s[2], s[3], st.Width, st.Height, w, h)
		}
	}
}

// TestProbeAlignment runs the coded-size probe on a real AV1 encoder
// (SVT-AV1, which codes any size as is), then on the same encoder with its
// output padded to 1920x1082 the way RDNA3 codes 1080p: the sequence header
// shows the padding although the probe asked for 1920x1080.
func TestProbeAlignment(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	svt := EncoderInfo{"libsvtav1", "av1", "software", false}
	if err := testEncode(context.Background(), ff, svt); err != nil {
		t.Skipf("libsvtav1: %v", err)
	}
	a, err := probeAlignment(context.Background(), ff, svt)
	if err != nil || a != (Alignment{W: 1, H: 1, ProbeW: 1920, ProbeH: 1080, CodedW: 1920, CodedH: 1080}) {
		t.Fatalf("libsvtav1: %+v %v", a, err)
	}
	w, h, err := codedSize(context.Background(), ff, append(blackFramesArgs(svt, 1920, 1080, 3), "-vf", "pad=iw:ih+2"))
	if err != nil || w != 1920 || h != 1082 {
		t.Fatalf("padded: %dx%d %v", w, h, err)
	}
	if aw, ah := alignmentFor(1920, w, 64), alignmentFor(1080, h, 16); aw != 64 || ah != 16 {
		t.Fatalf("padded: alignment %dx%d", aw, ah)
	}
	// Not AV1: no sequence header to read.
	if _, _, err := codedSize(context.Background(), ff, blackFramesArgs(EncoderInfo{"libx264", "h264", "software", false}, 64, 64, 3)); err == nil {
		t.Fatal("coded size of an H.264 stream")
	}
}

// TestVideoCrop runs the test source with TestPad through the Video manager:
// the coded picture is 16 rows taller, VideoConfig announces the crop and
// keeps the visible size as Width x Height (H.264 and AV1).
func TestVideoCrop(t *testing.T) {
	caps := probeOrSkip(t)
	for _, fam := range []string{"h264", "av1"} {
		enc, ok := caps.Best(fam)
		if !ok || enc.HW {
			continue
		}
		t.Run(enc.Name, func(t *testing.T) {
			start := time.Now()
			v := NewVideo(caps, nil, func() uint64 { return uint64(time.Since(start).Microseconds()) })
			defer v.Stop()
			if err := v.Start(Params{Source: Source{Backend: "test", NativeW: 640, NativeH: 360}, Encoder: enc, FPS: 30, BitrateKbps: 2000, TestPad: 16}, false); err != nil {
				t.Fatal(err)
			}
			timeout := time.After(20 * time.Second)
			for {
				select {
				case ev := <-v.Events():
					if ev.Err != nil {
						t.Fatal(ev.Err)
					}
					if c := ev.Config; c != nil {
						if c.Width != 640 || c.Height != 360 || c.CodedWidth != 640 || c.CodedHeight != 376 || c.CropRight != 0 || c.CropBottom != 16 {
							t.Fatalf("config %+v", c)
						}
						return
					}
				case <-timeout:
					t.Fatal("no video config")
				}
			}
		})
	}
}
