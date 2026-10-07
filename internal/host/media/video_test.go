package media

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
