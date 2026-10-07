package media

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
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
