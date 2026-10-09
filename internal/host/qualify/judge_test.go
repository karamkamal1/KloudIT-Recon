package qualify

import (
	"strings"
	"testing"
)

// run builds the frame log of a run: n frames at fps, the target stepping
// low / high every step frames from frame 1 + step on (as --rate-schedule
// does), every frame exactly at its target (key frames three times that).
func run(n, step, high, low, fps int) *RunLog {
	l := &RunLog{HasStarted: true, HasEnd: true}
	kbps := high
	for i := 1; i <= n; i++ {
		if i > 1 && (i-1)%step == 0 {
			if (i-1)/step%2 == 1 {
				kbps = low
			} else {
				kbps = high
			}
			l.End.RateChanges = append(l.End.RateChanges, RateChange{FrameID: uint64(i), Kbps: kbps})
		}
		f := Frame{ID: uint64(i), Kbps: kbps, Written: true, Bytes: kbps * 1000 / 8 / fps}
		if i == 1 {
			f.Key, f.SeqStart, f.Bytes = true, true, f.Bytes*3
		}
		l.Frames = append(l.Frames, f)
	}
	l.End.Frames, l.End.LastID, l.End.Written = n, uint64(n), n
	return l
}

// decoded is the decoder's view of a run: types from the key flags, barcode
// values the frames' sequence numbers.
func decoded(l *RunLog) *Decoded {
	d := &Decoded{}
	for _, f := range l.Frames {
		t := "P"
		if f.Key {
			t = "I"
		}
		d.Frames = append(d.Frames, DecodedFrame{Key: f.Key, Type: t, Barcode: int(f.ID - 1)})
	}
	return d
}

func judge(t *testing.T, mode string, l *RunLog, d *Decoded) Cell {
	t.Helper()
	c := Cell{Codec: "hevc", RC: "cbr", LiveBitrate: mode}
	in := Input{Mode: mode, FPS: 60, PlannedFrames: len(l.Frames), Log: l, Decoded: d, Barcode: d != nil,
		DecodeSkipped: "test", BarcodeSkipped: "test"}
	Judge(in, &c)
	return c
}

func wantVerdict(t *testing.T, c Cell, verdict string, failure string) {
	t.Helper()
	if c.Verdict != verdict {
		t.Fatalf("verdict %s, want %s; failures %q notes %q", c.Verdict, verdict, c.Failures, c.Notes)
	}
	if failure == "" {
		if len(c.Failures) > 0 && verdict == VerdictPass {
			t.Fatalf("failures on a pass: %q", c.Failures)
		}
		return
	}
	if !strings.Contains(strings.Join(c.Failures, "\n"), failure) {
		t.Fatalf("failures %q lack %q", c.Failures, failure)
	}
}

// flushKeys makes the first frame of every change a key frame (flush).
func flushKeys(l *RunLog, after int) {
	for i := 1; i < len(l.Frames); i++ {
		if l.Frames[i].Kbps != l.Frames[i-1].Kbps && i+after < len(l.Frames) {
			l.Frames[i+after].Key = true
			l.Frames[i+after].Bytes *= 3
		}
	}
}

func TestJudgeSeamlessPass(t *testing.T) {
	l := run(3600, 120, 50000, 20000, 60)
	c := judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictPass, "")
	if c.RateChanges != 29 || c.Frames != 3600 || c.Follow.MaxLagFrames != 0 || c.Barcode == nil || c.Barcode.Checked != 3600 ||
		c.KeyFrames.Mismatched != 0 || c.Decode == nil || c.Decode.Frames != 3600 {
		t.Fatalf("checks: %+v", c)
	}
	if c.Follow.Levels["50000"] != 1 || c.Follow.Levels["20000"] != 1 || c.Follow.FirstPhase != 1 {
		t.Fatalf("levels %v first %v", c.Follow.Levels, c.Follow.FirstPhase)
	}
}

func TestJudgeSeamlessIDR(t *testing.T) {
	// An encoder that makes an IDR at each change fails seamless (and the
	// same stream passes as flush).
	l := run(600, 120, 50000, 20000, 60)
	flushKeys(l, 1)
	c := judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "4 key frames after the first: 122 (1 after the change at 121)")
	if len(c.KeyFrames.Unexpected) != 4 {
		t.Fatalf("unexpected keys %v", c.KeyFrames.Unexpected)
	}
	c = judge(t, ModeFlush, l, decoded(l))
	wantVerdict(t, c, VerdictPass, "")
}

func TestJudgeFlushMissingKey(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	flushKeys(l, 0)
	l.Frames[240].Key = false // the change at 241 brought no key frame
	l.Frames[240].Bytes /= 3
	c := judge(t, ModeFlush, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "1 rate changes without a key frame within 5 frames")
	// A key frame far from any change is unexpected with flush too.
	l = run(600, 120, 50000, 20000, 60)
	flushKeys(l, 0)
	l.Frames[300].Key = true
	c = judge(t, ModeFlush, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "key frames not within 5 frames of a rate change: 301")
}

// lag makes the sizes follow each change n frames late.
func lag(l *RunLog, n int) {
	prev := l.Frames[0].Kbps
	for i := 1; i < len(l.Frames); i++ {
		if l.Frames[i].Kbps != l.Frames[i-1].Kbps {
			for k := i; k < i+n && k < len(l.Frames); k++ {
				l.Frames[k].Bytes = prev * 1000 / 8 / 60
			}
		}
		prev = l.Frames[i].Kbps
	}
}

func TestJudgeFollow(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	lag(l, 3)
	c := judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictPass, "")
	if c.Follow.MaxLagFrames != 3 {
		t.Fatalf("max lag %d, want 3", c.Follow.MaxLagFrames)
	}
	l = run(600, 120, 50000, 20000, 60)
	lag(l, 5)
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "later than 3 frames after 4 of the changes (up to 5 frames; at 121, 241, 361, 481)")
	// Sizes that overshoot the target in a steady phase.
	l = run(600, 120, 50000, 20000, 60)
	for i := 300; i < 360; i++ {
		l.Frames[i].Bytes = l.Frames[i].Bytes * 3 / 2
	}
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "steady P-frame sizes between 100 % and 150 % of the target")
	// A change that never reaches its target before the next one.
	l = run(600, 120, 50000, 20000, 60)
	for i := 120; i < 240; i++ {
		l.Frames[i].Bytes = 50000 * 1000 / 8 / 60
	}
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "never within the phase")
	if c.Follow.MaxLagFrames != -1 {
		t.Fatalf("max lag %d, want -1", c.Follow.MaxLagFrames)
	}
}

// TestJudgeFollowSVC: with two temporal layers the frames alternate in size
// (a base-layer frame, then a smaller enhancement-layer one), so the follow
// window spans whole layer periods (4 frames): a stream at its target on
// average passes. With one layer the same sizes never settle within 3 frames.
func TestJudgeFollowSVC(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	for i := 1; i < len(l.Frames); i++ {
		if i%2 == 1 {
			l.Frames[i].Bytes = l.Frames[i].Bytes * 9 / 5 // base layer
		} else {
			l.Frames[i].Bytes = l.Frames[i].Bytes / 5 // enhancement layer
		}
	}
	c := Cell{Codec: "hevc", RC: "cbr", LiveBitrate: ModeSeamless}
	Judge(Input{Mode: ModeSeamless, FPS: 60, SVCLayers: 2, PlannedFrames: len(l.Frames), Log: l, Decoded: decoded(l), Barcode: true}, &c)
	wantVerdict(t, c, VerdictPass, "")
	if c.Follow.MaxLagFrames != 0 {
		t.Fatalf("max lag %d, want 0", c.Follow.MaxLagFrames)
	}
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "never within the phase")
}

func TestJudgeInconclusive(t *testing.T) {
	// A source that does not fill 50 Mbit/s before any change: the high
	// phases stay at 30 Mbit/s, the low ones follow.
	l := run(600, 120, 50000, 20000, 60)
	for i := range l.Frames {
		if l.Frames[i].Kbps == 50000 && !l.Frames[i].Key {
			l.Frames[i].Bytes = 30000 * 1000 / 8 / 60
		}
	}
	c := judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictInconclusive, "steady P-frame sizes between 60 %")
	if !strings.Contains(strings.Join(c.Notes, "\n"), "use a higher-motion source") {
		t.Fatalf("notes %q", c.Notes)
	}
	// The same after a key frame on a change: a failure, not inconclusive.
	l.Frames[121].Key = true
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "key frames after the first")
	// An encoder that does not come back up after the first change fails.
	l = run(600, 120, 50000, 20000, 60)
	for i := 240; i < 600; i++ {
		l.Frames[i].Bytes = 20000 * 1000 / 8 / 60
	}
	c = judge(t, ModeSeamless, l, decoded(l))
	wantVerdict(t, c, VerdictFail, "reached the new target later than 3 frames")
}

func TestJudgeFrameIDs(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	l.Frames = append(l.Frames[:100], l.Frames[102:]...) // ids 101, 102 missing
	l.Frames[200].DroppedBefore = 1
	l.End.DroppedByHelper = 3
	c := judge(t, ModeSeamless, l, nil)
	wantVerdict(t, c, VerdictFail, "frame ids have gaps: 2 missing, 1 flagged droppedBefore, 3 dropped by the helper")
	if c.Barcode != nil || c.Decode != nil || c.KeyFrames.Mismatched != -1 {
		t.Fatalf("checks without a decoder: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "decode checks skipped: test") {
		t.Fatalf("notes %q", c.Notes)
	}
}

func TestJudgeRun(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	l.HasEnd = false
	wantVerdict(t, judge(t, ModeSeamless, l, nil), VerdictFail, "no end line")
	l = run(600, 120, 50000, 20000, 60)
	l.End.Fatal = true
	wantVerdict(t, judge(t, ModeSeamless, l, nil), VerdictFail, "fatal error")
	l = run(500, 120, 50000, 20000, 60)
	c := Cell{}
	Judge(Input{Mode: ModeSeamless, FPS: 60, PlannedFrames: 600, Log: l}, &c)
	wantVerdict(t, c, VerdictFail, "the run ended early: 500 of 600 frames")
	l = run(100, 120, 50000, 20000, 60)
	wantVerdict(t, judge(t, ModeSeamless, l, nil), VerdictFail, "no rate change happened")
	c = Cell{}
	Judge(Input{Mode: ModeSeamless, FPS: 60, Log: &RunLog{HasEnd: true}}, &c)
	wantVerdict(t, c, VerdictError, "no frames came out")
	l = run(600, 120, 50000, 20000, 60)
	l.Frames[0].Key = false
	wantVerdict(t, judge(t, ModeSeamless, l, nil), VerdictFail, "does not start with a key frame")
}

func TestJudgeDecoded(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	d := decoded(l)
	d.Frames[121].Type = "I" // an intra frame the encoder reported as P
	wantVerdict(t, judge(t, ModeSeamless, l, d), VerdictFail, "1 frames are intra in the bitstream but not flagged key")
	d = decoded(l)
	d.Errors = []string{"[hevc @ 0x1] [error] Could not find ref with POC 7"}
	c := judge(t, ModeSeamless, l, d)
	wantVerdict(t, c, VerdictFail, "the decoder reported 1 errors, first: [hevc @ 0x1] [error] Could not find ref")
	if c.Decode.FirstError == "" {
		t.Fatal("first error not recorded")
	}
	d = decoded(l)
	d.Frames = d.Frames[:599]
	wantVerdict(t, judge(t, ModeSeamless, l, d), VerdictFail, "the decoder output 599 frames of the 600 written")
}

func TestJudgeBarcodes(t *testing.T) {
	l := run(600, 120, 50000, 20000, 60)
	d := decoded(l)
	for _, i := range []int{10, 20, 30, 40, 50, 60, 70} { // more than 1 %
		d.Frames[i].Barcode = -1
	}
	c := judge(t, ModeSeamless, l, d)
	wantVerdict(t, c, VerdictFail, "barcodes: 7 of 600 unreadable")
	d = decoded(l)
	for _, i := range []int{10, 20, 30} {
		d.Frames[i].Barcode = -1
	}
	c = judge(t, ModeSeamless, l, d)
	wantVerdict(t, c, VerdictPass, "")
	if c.Barcode.Unreadable != 3 || c.Barcode.Wrong != 0 || c.Barcode.Gaps != 0 {
		t.Fatalf("barcode %+v", c.Barcode)
	}
	// A frame missing from the stream: everything after it is shifted.
	d = decoded(l)
	d.Frames = append(d.Frames[:300], d.Frames[301:]...)
	d.Frames = append(d.Frames, DecodedFrame{Type: "P", Barcode: 600})
	c = judge(t, ModeSeamless, l, d)
	wantVerdict(t, c, VerdictFail, "barcodes: 300 frames show another frame's number, 1 gaps")
	// The barcode counts from the latest sequence start.
	l = run(600, 120, 50000, 20000, 60)
	d = decoded(l)
	l.Frames[400].SeqStart, l.Frames[400].Key, d.Frames[400].Key = true, true, true
	for i := 400; i < 600; i++ {
		d.Frames[i].Barcode = i - 400
	}
	c = judge(t, ModeSeamless, l, d)
	if c.Barcode.Wrong != 0 || c.Barcode.Gaps != 0 {
		t.Fatalf("barcode after a sequence start: %+v", c.Barcode)
	}
}

func TestJudgeRepeatsIgnored(t *testing.T) {
	// Idle repeats (a desktop that stopped changing) are tiny and are not
	// judged; enough real frames remain.
	l := run(600, 120, 50000, 20000, 60)
	for i := 150; i < 600; i += 7 {
		l.Frames[i].Repeat, l.Frames[i].Bytes = true, 200
	}
	wantVerdict(t, judge(t, ModeSeamless, l, decoded(l)), VerdictPass, "")
}
