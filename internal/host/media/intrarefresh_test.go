package media

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestIntraRefreshHeals is the recovery "skip" path with the software encoder
// standing in for NVENC (guide step 1.2): libx264 with periodic intra refresh
// (Caps.UseIntraRefresh, the same -intra-refresh 1 and -g = IntraRefreshPeriod
// the host passes to NVENC) on the test pattern, frames exactly as Video
// delivers them to the session (parameter sets in front of the first frame).
// A client that skips a lost frame and decodes on must get a picture that
// differs from the undamaged one after the loss and is identical again within
// two refresh periods (HealFrames), the bound Recovery assumes, without an IDR
// after the first frame. libx264 flags the first frame of every refresh wave
// a key frame (its recovery point; Video then puts the parameter sets in front
// of it); NVENC flags only IDRs.
func TestIntraRefreshHeals(t *testing.T) {
	caps := probeOrSkip(t)
	enc, ok := EncoderInfo{}, false
	for _, e := range caps.Encoders {
		if e.Name == "libx264" {
			enc, ok = e, true
		}
	}
	if !ok {
		t.Skip("no libx264")
	}
	c := *caps
	c.Filters = map[string]bool{}
	for k, v := range caps.Filters {
		c.Filters[k] = v && k != "realtime" // encode as fast as possible
	}
	c.intraRefresh = nil
	if !c.UseIntraRefresh("libx264") {
		t.Fatal("libx264 has no -intra-refresh")
	}
	const w, h, fps = 640, 360, 30
	period := IntraRefreshPeriod(fps)
	total := 7 * period
	p := Params{Source: Source{Backend: "test", NativeW: w, NativeH: h}, Encoder: enc, FPS: fps, BitrateKbps: 1500}
	args, err := c.BuildArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-intra-refresh 1") || !strings.Contains(joined, fmt.Sprintf("-g %d ", period)) {
		t.Fatalf("libx264 with intra refresh: %s", joined)
	}
	if r := Recovery(args, w, h, fps); r != proto.RecoverySkip {
		t.Fatalf("recovery %q, want skip", r)
	}

	v := NewVideo(&c, nil, NewClock())
	defer v.Stop()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	var frames []*Frame
	deadline := time.After(60 * time.Second)
	for len(frames) < total {
		select {
		case ev := <-v.Events():
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			if ev.Config != nil && (ev.Config.Recovery != proto.RecoverySkip || ev.HealFrames != 2*period) {
				t.Fatalf("generation announces recovery %q, heals in %d frames", ev.Config.Recovery, ev.HealFrames)
			}
			if ev.Frame != nil {
				frames = append(frames, ev.Frame)
			}
		case <-deadline:
			t.Fatalf("timeout after %d frames", len(frames))
		}
	}
	v.Stop()
	if !frames[0].Key {
		t.Fatal("the first frame is not a key frame")
	}
	without := func(lost int) []*Frame {
		return append(frames[:lost:lost], frames[lost+1:]...)
	}
	// Every frame the decoder makes, damaged or not: FFmpeg's H.264 decoder
	// otherwise withholds the frames after a gap in frame_num until a
	// recovery point has healed them.
	showAll := []string{"-flags2", "+showall"}
	ref := decodeLuma(t, c.FFmpeg, "h264", frames, w, h, showAll...)
	if len(ref) != total {
		t.Fatalf("undamaged stream: decoded %d of %d frames", len(ref), total)
	}
	keys := 0
	for i, f := range frames {
		if f.Key {
			keys++
		}
		idr := false
		for _, nal := range codec.SplitAnnexB(f.Data) {
			idr = idr || len(nal) > 0 && nal[0]&0x1f == 5
		}
		if idr != (i == 0) {
			t.Fatalf("frame %d (key %v): IDR %v, want an IDR in the first frame only", i, f.Key, idr)
		}
	}
	worst, withheldMax := 0, 0
	// A loss at every point of a refresh wave (x264 marks a wave's first
	// frame a key frame, with a recovery point), and a frame_num wrap.
	for lost := 2 * period; lost <= 3*period; lost++ {
		got := decodeLuma(t, c.FFmpeg, "h264", without(lost), w, h, showAll...)
		// FFmpeg's decoder can withhold the frames right after the loss: when
		// the lost frame is where frame_num wraps, it takes the following
		// frames (POC type 2) for out of order until the next wrap. So match
		// the decoded frames from the end: the picture has healed from the
		// first frame on which every later frame comes out identical to the
		// undamaged decode (a withheld frame leaves the old one on screen,
		// which counts as damage).
		withheld := total - 1 - len(got)
		if withheld < 0 || withheld > 2*period || len(got) < lost {
			t.Fatalf("frame %d skipped: decoded %d of %d frames", lost, len(got), total-1)
		}
		for i := 0; i < lost; i++ {
			if !bytes.Equal(got[i], ref[i]) {
				t.Fatalf("frame %d skipped: frame %d before it differs", lost, i)
			}
		}
		healed := total // first frame of the identical tail, in the undamaged stream
		for k := 1; k <= len(got)-lost && bytes.Equal(got[len(got)-k], ref[total-k]); k++ {
			healed = total - k
		}
		if healed <= lost+1 {
			t.Logf("frame %d skipped: no visible damage", lost)
			continue
		}
		heal := healed - lost // frames until the picture is right again
		worst, withheldMax = max(worst, heal), max(withheldMax, withheld)
		t.Logf("frame %d skipped (key %v): right again from frame %d, after %d frames (%d withheld by the decoder)",
			lost, frames[lost].Key, healed, heal, withheld)
		if heal > 2*period {
			t.Errorf("frame %d skipped: the picture healed only after %d frames, more than two refresh periods (%d)", lost, heal, 2*period)
		}
	}
	t.Logf("refresh period %d frames at %d fps, %d key frames in %d (one IDR); worst heal %d frames (%.2f s), at most %d frames withheld",
		period, fps, keys, total, worst, float64(worst)/fps, withheldMax)
	if worst == 0 {
		t.Fatal("no skipped frame damaged the picture: the test shows nothing")
	}
}
