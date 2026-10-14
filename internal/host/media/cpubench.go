package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What the CPU of a PC without a GPU encoder sustains. There every frame is
// captured (GDI's BitBlt), converted and encoded (libx264) on the CPU, and a
// small VM (2-4 vCPUs) does not keep up with 1920x1080 at 60 fps: libx264
// "superfast" 1080p with a moving test source made 57 fps on two 2.8 GHz
// Xeon vCPUs here with nothing else running. Where the encoder falls behind,
// FFmpeg's queues fill (8 packets from the capture, 8 frames into the
// filters, 8 into the encoder) and every frame waits behind them: hundreds
// of ms of latency (gdigrab + libx264 at 1080p60 on two CPUs under Wine:
// 16 fps, grab->encoded p50 219 ms, p95 619 ms). Sessions pick a frame rate,
// size and preset the CPU sustains instead (host cpubudget.go), from this
// measurement at the agent's start and from the stream itself.

// SoftwareBench is BenchSoftware's result: libx264's frame rate at W x H
// with the session's arguments, at its "superfast" (Quality balanced) and
// "ultrafast" (Quality speed) presets.
type SoftwareBench struct {
	Encoder   string
	W, H      int
	Superfast float64 // frames per second
	Ultrafast float64
	Took      time.Duration
}

// benchFrames is how many frames each BenchSoftware run encodes at most,
// benchLimit how long it measures at most (wall clock, after its start-up),
// and benchStart how long its start-up may take.
const (
	benchFrames = 150
	benchLimit  = 2500 * time.Millisecond
	benchStart  = 10 * time.Second
)

// BenchSoftware measures how fast this PC encodes w x h with encoder e (a
// software encoder: libx264) as a session does (preset, zerolatency, CBR),
// from a moving source with as much detail as a busy desktop (testsrc2,
// generated as BGRA and converted as GDI's frames are): frames per second
// at each preset, all of the CPU's threads at work. A source of the
// session's own capture would measure a static desktop, which encodes far
// faster than the video or game the stream is for. Each run encodes
// benchFrames frames or for benchLimit, and the rate is taken after its
// first frames (start-up). It takes a few seconds of full CPU: the agent
// runs it once, in the background.
func BenchSoftware(ctx context.Context, ffmpeg string, e EncoderInfo, w, h int) (SoftwareBench, error) {
	b := SoftwareBench{Encoder: e.Name, W: w &^ 1, H: h &^ 1}
	if e.HW || e.Name != "libx264" {
		return b, fmt.Errorf("%s: only libx264 is measured", e.Name)
	}
	start := time.Now()
	for _, run := range []struct {
		preset string
		fps    *float64
	}{{"superfast", &b.Superfast}, {"ultrafast", &b.Ultrafast}} {
		fps, err := benchRun(ctx, ffmpeg, b.W, b.H, run.preset)
		if err != nil {
			return b, fmt.Errorf("%s %s: %w", e.Name, run.preset, err)
		}
		*run.fps = fps
	}
	b.Took = time.Since(start)
	return b, nil
}

// benchRun encodes one preset and returns its frame rate after start-up,
// read from FFmpeg's -progress lines as they come.
func benchRun(ctx context.Context, ffmpeg string, w, h int, preset string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, benchStart+benchLimit+5*time.Second)
	defer cancel()
	cmd := quietCmd(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-nostats",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=1000", w, h),
		"-frames:v", strconv.Itoa(benchFrames),
		"-vf", "format=bgra,format=yuv420p",
		"-c:v", "libx264", "-preset", preset, "-tune", "zerolatency", "-b:v", "20000k", "-maxrate", "20000k", "-bufsize", "667k",
		"-bf", "0", "-g", "100000", "-progress", "pipe:1", "-stats_period", "0.1", "-f", "null", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// The rate from the first progress line with a few frames done (the
	// encoder's start-up and first frames are behind it) to the last, for
	// benchFrames frames or benchLimit, whichever ends first: a slow CPU is
	// stopped there (cut).
	const warmup = 5
	started := time.Now()
	var t0, t1 time.Time
	var f0, f1 int
	cut := false
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "frame=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		now := time.Now()
		if t0.IsZero() && n >= warmup {
			t0, f0 = now, n
		}
		t1, f1 = now, n
		if !t0.IsZero() && now.Sub(t0) >= benchLimit || t0.IsZero() && now.Sub(started) >= benchStart {
			cut = true
			cancel()
			break
		}
	}
	if err := cmd.Wait(); err != nil && !cut {
		return 0, fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	if t0.IsZero() || f1-f0 < warmup || t1.Sub(t0) <= 0 {
		return 0, errors.New("too few frames to measure")
	}
	return float64(f1-f0) / t1.Sub(t0).Seconds(), nil
}
