//go:build windows

package qualify

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Integration tests of the whole qualification against the real
// recon-encoder.exe (make helper-test runs them under Wine): the mock backend
// (frame sizes padded to the target with --mock-follow-rate; an encoder that
// makes an IDR on every change, one that follows late) and the NVENC backend
// on its test double. RECON_HELPER_EXE points at the helper, RECON_FAKE_NVENC
// at recon-fake-nvenc.dll, RECON_FFMPEG (optional) at a Windows ffmpeg.exe
// for the decode checks.

func helperExe(t *testing.T) string {
	t.Helper()
	exe := os.Getenv("RECON_HELPER_EXE")
	if exe == "" {
		t.Skip("set RECON_HELPER_EXE to recon-encoder.exe to run the qualification integration tests")
	}
	return exe
}

func runMatrix(t *testing.T, o Options) *Results {
	t.Helper()
	var out bytes.Buffer
	o.Out, o.WorkDir = &out, t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r, err := Run(ctx, o)
	t.Logf("%s", out.String())
	if err != nil {
		t.Fatal(err)
	}
	var table bytes.Buffer
	r.Print(&table)
	t.Logf("%s", table.String())
	return r
}

func verdicts(r *Results) string {
	var v []string
	for _, c := range r.Cells {
		v = append(v, c.Codec+" "+c.Quality+" "+c.RC+" "+c.LiveBitrate+" "+c.Verdict)
	}
	return strings.Join(v, ", ")
}

func TestQualifyMock(t *testing.T) {
	o := Options{Helper: helperExe(t), Backend: "mock", FFmpeg: os.Getenv("RECON_FFMPEG")}
	r := runMatrix(t, o) // every quality preset
	if got := verdicts(r); got != "h264 speed cbr seamless pass, h264 speed cbr flush pass, h264 balanced cbr seamless pass, "+
		"h264 balanced cbr flush pass, h264 quality cbr seamless pass, h264 quality cbr flush pass" {
		t.Fatalf("verdicts: %s", got)
	}
	if r.Schedule.Frames != mockMaxFrames || r.Schedule.StepFrames != 10 || r.Backend != "mock" || r.Source.Capture != "synthetic" {
		t.Fatalf("schedule %+v source %+v", r.Schedule, r.Source)
	}
	for _, c := range r.Cells {
		// The mock's H.264 has no LTR recovery: no LTR slots, as in a session.
		if c.RateChanges != 5 || c.Frames != mockMaxFrames || c.Follow.MaxLagFrames != 0 || c.Follow.Levels["20000"] != 1 || c.LTRSlots != 0 {
			t.Fatalf("cell %+v", c)
		}
		if b, err := os.ReadFile(c.Log); err != nil || !strings.Contains(string(b), "--quality="+c.Quality) {
			t.Fatalf("log %s: %v", c.Log, err)
		}
		if (c.Decode == nil) != (o.FFmpeg == "") || c.Barcode != nil {
			t.Fatalf("decode %+v barcode %+v (ffmpeg %q)", c.Decode, c.Barcode, o.FFmpeg)
		}
		if c.Decode != nil && (c.Decode.Frames != mockMaxFrames || c.Decode.Errors != 0 || c.KeyFrames.Mismatched != 0) {
			t.Fatalf("decode %+v keys %+v", c.Decode, c.KeyFrames)
		}
	}
	if ch := r.Choice["h264"]["balanced"]; ch.AdaptiveRC != "cbr" || ch.Adaptive != ModeSeamless || len(r.Choice["h264"]) != 3 {
		t.Fatalf("choice %+v", r.Choice)
	}

	// An encoder that makes an IDR at every change: seamless fails, flush
	// passes, so sessions would use flush.
	o.HelperArgs, o.Qualities = []string{"--mock-idr-on-rate"}, []string{"speed"}
	r = runMatrix(t, o)
	if got := verdicts(r); got != "h264 speed cbr seamless fail, h264 speed cbr flush pass" {
		t.Fatalf("verdicts with an IDR per change: %s", got)
	}
	if c := r.Cells[0]; len(c.KeyFrames.Unexpected) != 5 || c.KeyFrames.Unexpected[0] != 11 {
		t.Fatalf("unexpected keys %v", c.KeyFrames.Unexpected)
	}
	if ch := r.Choice["h264"]["speed"]; ch.Adaptive != ModeFlush {
		t.Fatalf("choice %+v", ch)
	}

	// Sizes that follow 5 frames late fail both.
	o.HelperArgs = []string{"--mock-rate-lag=5"}
	r = runMatrix(t, o)
	if got := verdicts(r); got != "h264 speed cbr seamless fail, h264 speed cbr flush fail" {
		t.Fatalf("verdicts with a late encoder: %s", got)
	}
	if c := r.Cells[0]; c.Follow.MaxLagFrames != 5 || len(c.Follow.Late) != 5 {
		t.Fatalf("follow %+v", c.Follow)
	}
	if ch := r.Choice["h264"]["speed"]; ch.Adaptive != ModeRestart {
		t.Fatalf("choice %+v", ch)
	}
}

func TestQualifyNvencTestDouble(t *testing.T) {
	dll := os.Getenv("RECON_FAKE_NVENC")
	if dll == "" {
		t.Skip("set RECON_FAKE_NVENC to recon-fake-nvenc.dll")
	}
	// The NVENC backend through capture (the synthetic GPU source in its
	// high-motion mode), conversion, the test double and the ring; the test
	// double sizes frames like a CBR encoder that follows at once.
	r := runMatrix(t, Options{Helper: helperExe(t), Backend: "nvenc", NvencTestDLL: dll, FFmpeg: os.Getenv("RECON_FFMPEG"),
		Qualities: []string{"balanced"}, Width: 320, Height: 180, FPS: 60, Step: 500 * time.Millisecond, Duration: 3 * time.Second})
	if c := r.Cells[0]; c.Verdict == VerdictError && strings.Contains(strings.Join(c.Failures, " "), "D3D11") {
		t.Skipf("no D3D11 device (Wine needs an X display): %s", c.Failures[0])
	}
	if !r.TestDouble || r.Backend != "nvenc" || r.Source.Capture != "synthetic-gpu" || !r.Source.Motion {
		t.Fatalf("results %+v", r)
	}
	want := "hevc balanced cbr seamless pass, hevc balanced cbr flush pass, av1 balanced cbr seamless pass, av1 balanced cbr flush pass, " +
		"h264 balanced cbr seamless pass, h264 balanced cbr flush pass"
	if got := verdicts(r); got != want {
		t.Fatalf("verdicts: %s", got)
	}
	for _, c := range r.Cells {
		// NVENC recovers by reference invalidation: no LTR slots, as in a session.
		if c.Frames != 180 || c.RateChanges != 5 || c.RateControl != "cbr" || c.StartedLiveBitrate != c.LiveBitrate || c.Decode != nil ||
			c.LTRSlots != 0 {
			t.Fatalf("cell %+v", c)
		}
		if !strings.Contains(strings.Join(c.Notes, " "), "test double's bitstream does not decode") {
			t.Fatalf("notes %q", c.Notes)
		}
	}
	// Results of the test double are never used by sessions.
	if _, _, ok := r.Choose(encoder.Caps{Backend: r.Backend, AdapterName: r.AdapterName}, encoder.StartParams{Codec: "hevc", Quality: "balanced"}, true); ok {
		t.Fatal("test double results chosen")
	}
}
