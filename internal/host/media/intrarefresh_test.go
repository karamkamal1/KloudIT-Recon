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

// TestTestRecovery is the FFmpeg stand-in for reference recovery that the
// browser E2E uses (Caps.UseTestRecovery, host RECON_TEST_FAULTS ref-recovery,
// GUIDE 3.5): libx264 and libsvtav1 with a key frame every TestRecoveryGOP
// frames, only the generation's first frame flagged key, recovery
// "invalidate", and after a Recover the first key frame after the lost frame
// flagged as the recovery frame (refFloor = the frame before the loss),
// answered with a Recovered event. What a client under reference recovery
// decodes (the frames before the loss, then the recovery frame and everything
// after it) must equal the undamaged stream from the recovery frame on.
func TestTestRecovery(t *testing.T) {
	caps := probeOrSkip(t)
	for _, name := range []string{"libx264", "libsvtav1"} {
		t.Run(name, func(t *testing.T) {
			for _, e := range caps.Encoders {
				if e.Name == name {
					testRecoveryStandIn(t, caps, e)
					return
				}
			}
			t.Skip("no " + name)
		})
	}
}

func testRecoveryStandIn(t *testing.T, caps *Caps, enc EncoderInfo) {
	c := *caps
	c.Filters = map[string]bool{}
	for k, v := range caps.Filters {
		c.Filters[k] = v && k != "realtime" // encode as fast as possible
	}
	c.intraRefresh = nil
	c.UseTestRecovery()
	const w, h, fps, lost, total = 320, 180, 30, 7, 40
	gop := TestRecoveryGOP(fps)
	p := Params{Source: Source{Backend: "test", NativeW: w, NativeH: h}, Encoder: enc, FPS: fps, BitrateKbps: 800}
	args, err := c.BuildArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, fmt.Sprintf("-g %d ", gop)) {
		t.Fatalf("%s with a key frame every %d frames: %s", enc.Name, gop, joined)
	}
	v := NewVideo(&c, nil, NewClock())
	defer v.Stop()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	var frames []*Frame
	var rec *Recovered
	gen := uint8(0)
	deadline := time.After(60 * time.Second)
	for len(frames) < total || rec == nil {
		select {
		case ev := <-v.Events():
			switch {
			case ev.Err != nil:
				t.Fatal(ev.Err)
			case ev.Config != nil:
				if ev.Config.Recovery != proto.RecoveryInvalidate || ev.HealFrames != 0 {
					t.Fatalf("config recovery %q, heal frames %d", ev.Config.Recovery, ev.HealFrames)
				}
				gen = ev.Config.Gen
			case ev.Recovered != nil:
				rec = ev.Recovered
			case ev.Frame != nil:
				f := ev.Frame
				frames = append(frames, f)
				if f.Seq == 0 {
					if c := v.Capabilities(); c.Recovery != RecoveryInvalidate {
						t.Fatalf("capabilities %+v", c)
					}
					if err := v.Recover(gen, 0); err == nil {
						t.Fatal("Recover of the key frame")
					}
					if err := v.Recover(gen+1, 3); err == nil {
						t.Fatal("Recover of another generation")
					}
				}
				if f.Seq == lost {
					if err := v.Recover(gen, lost); err != nil {
						t.Fatal(err)
					}
					if err := v.Recover(gen, lost+1); err != nil { // a later loss: covered by the first
						t.Fatal(err)
					}
				}
			}
		case <-deadline:
			t.Fatalf("timeout after %d frames", len(frames))
		}
	}
	v.Stop()
	at := -1
	for i, f := range frames {
		if f.Key != (i == 0) {
			t.Fatalf("frame %d: key %v, want only the first", i, f.Key)
		}
		if f.Recovery {
			if at >= 0 {
				t.Fatalf("second recovery frame %d (first %d)", i, at)
			}
			at = i
		}
	}
	if at <= lost || frames[at].RefFloor != lost-1 || at > lost+3*gop {
		t.Fatalf("recovery frame %d (refFloor %d) for the loss at %d, key frames every %d", at, frames[max(at, 0)].RefFloor, lost, gop)
	}
	if enc.Family == "h264" {
		idr := false
		for _, nal := range codec.SplitAnnexB(frames[at].Data) {
			idr = idr || len(nal) > 0 && nal[0]&0x1f == 5
		}
		if !idr {
			t.Fatalf("recovery frame %d is no IDR", at)
		}
	}
	if rec.Gen != gen || rec.From != lost || rec.AtGen != gen || rec.AtSeq != uint32(at) || rec.Key {
		t.Fatalf("recovered %+v, want the frame %d", rec, at)
	}
	ref := decodeLuma(t, c.FFmpeg, enc.Family, frames, w, h)
	view := append(frames[:lost:lost], frames[at:]...)
	got := decodeLuma(t, c.FFmpeg, enc.Family, view, w, h)
	if len(ref) != len(frames) || len(got) != len(view) {
		t.Fatalf("decoded %d of %d frames, client view %d of %d", len(ref), len(frames), len(got), len(view))
	}
	for i := range got {
		want := ref[i]
		if i >= lost {
			want = ref[at+i-lost]
		}
		if !bytes.Equal(got[i], want) {
			t.Fatalf("client view frame %d differs from the undamaged stream", i)
		}
	}
	t.Logf("loss at %d recovered by frame %d (key frames every %d), %d frames discarded", lost, at, gop, at-lost)
}
