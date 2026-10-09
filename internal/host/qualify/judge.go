package qualify

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
)

// The pass criteria of a cell (GUIDE 3.6), recorded in every results file.
const (
	// FollowFrames: after a rate change the P-frame sizes must be at the new
	// target within this many frames: some window of WindowFrames P frames
	// (rounded up to whole temporal-layer periods: 4 with two layers, whose
	// frames alternate in size) starting at most FollowFrames P frames after
	// the change has a mean size within SizeTolerance of the target.
	FollowFrames = 3
	WindowFrames = 3
	// SizeTolerance is the allowed relative deviation of a mean P-frame size
	// from the target's frame size (kbps * 1000 / 8 / fps), in the follow
	// window and in the steady second half of every phase.
	SizeTolerance = 0.25
	// KeyWithinFrames: with liveBitrate flush, the key frame of a change must
	// come within this many frames of it.
	KeyWithinFrames = 5
	// MaxUnreadablePct: barcodes that do not read (noise or artifacts in the
	// corner) allowed, in percent of the frames.
	MaxUnreadablePct = 1.0
	// MinRunPct: a run that delivered fewer frames than this share of the
	// planned ones ended early.
	MinRunPct = 95
)

// Verdicts of a cell.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
	// VerdictInconclusive: the source did not fill the high bitrate even
	// before the first change, so the sizes cannot show whether the encoder
	// follows (use a higher-motion source).
	VerdictInconclusive = "inconclusive"
	// VerdictError: the run did not happen (the helper or its encoder failed
	// to start). An encoder that refuses the live-bitrate mode itself, and a
	// helper that crashes or hangs after its stream started, fail instead.
	VerdictError = "error"
)

// Criteria are the pass criteria a results file was judged with.
type Criteria struct {
	FollowFrames     int     `json:"followFrames"`
	WindowFrames     int     `json:"windowFrames"`
	SizeTolerance    float64 `json:"sizeTolerance"`
	KeyWithinFrames  int     `json:"keyWithinFrames"`
	MaxUnreadablePct float64 `json:"maxUnreadablePct"`
}

// DefaultCriteria returns the constants above.
func DefaultCriteria() Criteria {
	return Criteria{FollowFrames: FollowFrames, WindowFrames: WindowFrames, SizeTolerance: SizeTolerance,
		KeyWithinFrames: KeyWithinFrames, MaxUnreadablePct: MaxUnreadablePct}
}

// KeyCheck: key frames against the rate changes, and the decoder's view.
type KeyCheck struct {
	// Unexpected key frames (frame ids): any after the first with seamless,
	// any not within KeyWithinFrames of a change with flush.
	Unexpected []uint64 `json:"unexpected,omitempty"`
	// MissingAfterChange (flush): changes (their first frame) with no key
	// frame within KeyWithinFrames.
	MissingAfterChange []uint64 `json:"missingAfterChange,omitempty"`
	// Mismatched: frames whose decoded type (intra or not) differs from the
	// encoder's key flag, e.g. an intra frame the helper reported as P. -1:
	// not decoded.
	Mismatched int `json:"mismatched"`
}

// FollowCheck: P-frame sizes against the target.
type FollowCheck struct {
	// MaxLagFrames: the largest number of P frames after a change before
	// the sizes were within tolerance of the new target; -1 if a change
	// never got there before the next one.
	MaxLagFrames int `json:"maxLagFrames"`
	// Late: changes (their first frame) that took more than FollowFrames.
	Late []uint64 `json:"late,omitempty"`
	// SteadyMin / SteadyMax: the lowest / highest ratio of a phase's
	// steady mean P-frame size (its second half) to the target.
	SteadyMin float64 `json:"steadyMin"`
	SteadyMax float64 `json:"steadyMax"`
	// Levels: per target (kbps) the median steady ratio over its phases.
	Levels map[string]float64 `json:"levels"`
	// FirstPhase: the steady ratio before the first change (at the start's
	// bitrate): below 1 - SizeTolerance the source cannot fill it.
	FirstPhase float64 `json:"firstPhase"`
}

// IDCheck: frame ids and drops.
type IDCheck struct {
	Gaps            int `json:"gaps"`            // frame ids missing between delivered frames
	DroppedBefore   int `json:"droppedBefore"`   // the ring's droppedBefore counts
	DroppedByHelper int `json:"droppedByHelper"` // frames the helper dropped (ring full, too large)
}

// BarcodeCheck: the frame barcode (GUIDE 0.2) of every decoded frame.
type BarcodeCheck struct {
	Checked    int `json:"checked"`
	Unreadable int `json:"unreadable"`
	// Wrong: readable values that are not the frame's sequence number.
	Wrong int `json:"wrong"`
	// Gaps: jumps between readable values that the frames between them do
	// not explain (a frame missing from the decoded stream).
	Gaps int `json:"gaps"`
}

// DecodeCheck: the decoder's run over the written stream.
type DecodeCheck struct {
	Frames     int    `json:"frames"` // frames the decoder output
	Errors     int    `json:"errors"`
	FirstError string `json:"firstError,omitempty"`
	Warnings   int    `json:"warnings"`
}

// Cell is the result of one vendor x codec x rate-control x live-bitrate run.
type Cell struct {
	Codec       string `json:"codec"`
	RC          string `json:"rc"`          // cbr | vbr | vbr_peak (start's rc)
	LiveBitrate string `json:"liveBitrate"` // seamless | flush
	// Quality / LTRSlots / SVCLayers: the start's quality preset, LTR slots
	// and temporal layers (0: one), as a session starts this codec on this
	// encoder (Choose matches on all three).
	Quality   string `json:"quality"`
	LTRSlots  int    `json:"ltrSlots"`
	SVCLayers int    `json:"svcLayers,omitempty"`
	// IntraRefresh: the intra refresh cycle the stream started with, as a
	// session's (encoder.Caps.IntraRefreshFrames: the loss-recovery ladder's
	// safety net, GUIDE 2.3); 0 off. Not matched by Choose.
	IntraRefresh int    `json:"intraRefresh,omitempty"`
	Verdict      string `json:"verdict"`
	// Failures say why a cell failed (or is inconclusive / an error).
	Failures []string `json:"failures,omitempty"`
	// Notes: checks that were skipped and why, other observations.
	Notes []string `json:"notes,omitempty"`
	// RateControl / StartedLiveBitrate: what the helper's started message
	// says the encoder runs (e.g. vbr_latency for rc vbr on AMF).
	RateControl        string `json:"rateControl,omitempty"`
	StartedLiveBitrate string `json:"startedLiveBitrate,omitempty"`
	Width              int    `json:"width,omitempty"`
	Height             int    `json:"height,omitempty"`
	FPS                int    `json:"fps,omitempty"`

	Frames      int           `json:"frames"`
	RateChanges int           `json:"rateChanges"`
	KeyFrames   KeyCheck      `json:"keyFrames"`
	Follow      FollowCheck   `json:"follow"`
	FrameIDs    IDCheck       `json:"frameIds"`
	Barcode     *BarcodeCheck `json:"barcode,omitempty"`
	Decode      *DecodeCheck  `json:"decode,omitempty"`
	Seconds     float64       `json:"seconds"` // wall time of the run
	Log         string        `json:"log,omitempty"`
}

// Passed reports whether the cell passed.
func (c *Cell) Passed() bool { return c.Verdict == VerdictPass }

// Input is what Judge needs about one run.
type Input struct {
	Mode          string // seamless | flush: how the run applied setRate
	FPS           int    // frames per second the targets are divided by
	SVCLayers     int    // temporal layers of the stream (0, 1: one)
	PlannedFrames int
	Log           *RunLog
	// Decoded is the decoder's view of the written frames; nil: not
	// decoded (DecodeSkipped says why).
	Decoded       *Decoded
	DecodeSkipped string
	// Barcode: Decoded carries barcode values to check; else
	// BarcodeSkipped says why not.
	Barcode        bool
	BarcodeSkipped string
}

// Judge applies the pass criteria to a run and fills the checks and the
// verdict of c (Codec, RC and LiveBitrate are the caller's).
func Judge(in Input, c *Cell) {
	fail := func(format string, a ...any) { c.Failures = append(c.Failures, fmt.Sprintf(format, a...)) }
	note := func(format string, a ...any) { c.Notes = append(c.Notes, fmt.Sprintf(format, a...)) }
	log := in.Log
	frames := log.Frames
	c.Frames = len(frames)
	c.KeyFrames.Mismatched = -1
	if len(frames) == 0 {
		c.Verdict = VerdictError
		fail("no frames came out of the encoder")
		return
	}
	fps := max(in.FPS, 1)

	// The run itself.
	switch {
	case !log.HasEnd:
		fail("the helper's frame log has no end line (the helper crashed)")
	case log.End.Fatal:
		fail("the helper reported a fatal error (see the log)")
	case log.End.TimedOut:
		fail("the run timed out")
	}
	if in.PlannedFrames > 0 && len(frames)*100 < in.PlannedFrames*MinRunPct {
		fail("the run ended early: %d of %d frames", len(frames), in.PlannedFrames)
	}
	if log.HasEnd && log.End.Errors > 0 {
		note("the helper reported %d non-fatal errors (see the log)", log.End.Errors)
	}

	// Frame ids: consecutive from 1, nothing dropped.
	prev := uint64(0)
	for _, f := range frames {
		if f.ID > prev+1 {
			c.FrameIDs.Gaps += int(f.ID - prev - 1)
		}
		prev = max(prev, f.ID)
		c.FrameIDs.DroppedBefore += f.DroppedBefore
	}
	if log.HasEnd {
		c.FrameIDs.DroppedByHelper = log.End.DroppedByHelper
	}
	if n := c.FrameIDs.Gaps + c.FrameIDs.DroppedBefore + c.FrameIDs.DroppedByHelper; n > 0 {
		fail("frame ids have gaps: %d missing, %d flagged droppedBefore, %d dropped by the helper", c.FrameIDs.Gaps,
			c.FrameIDs.DroppedBefore, c.FrameIDs.DroppedByHelper)
	}

	// Rate changes: the first frame of each new target.
	var changes []int // indexes into frames
	for i := 1; i < len(frames); i++ {
		if frames[i].Kbps != frames[i-1].Kbps {
			changes = append(changes, i)
		}
	}
	c.RateChanges = len(changes)
	if len(changes) == 0 {
		fail("no rate change happened")
	}

	// Key frames.
	if !frames[0].Key {
		fail("the stream does not start with a key frame")
	}
	nearChange := func(i int) (int, bool) { // the change whose key frame frame i may be (flush)
		for _, ch := range changes {
			if i >= ch && i < ch+KeyWithinFrames {
				return ch, true
			}
		}
		return 0, false
	}
	for i := 1; i < len(frames); i++ {
		if !frames[i].Key {
			continue
		}
		if _, ok := nearChange(i); ok && in.Mode == "flush" {
			continue
		}
		c.KeyFrames.Unexpected = append(c.KeyFrames.Unexpected, frames[i].ID)
	}
	if in.Mode == "flush" {
		for _, ch := range changes {
			found := false
			for i := ch; i < min(ch+KeyWithinFrames, len(frames)); i++ {
				found = found || frames[i].Key
			}
			if !found && ch+KeyWithinFrames <= len(frames) {
				c.KeyFrames.MissingAfterChange = append(c.KeyFrames.MissingAfterChange, frames[ch].ID)
			}
		}
	}
	if n := len(c.KeyFrames.Unexpected); n > 0 {
		what := "after the first"
		if in.Mode == "flush" {
			what = fmt.Sprintf("not within %d frames of a rate change", KeyWithinFrames)
		}
		var near []string
		for _, id := range c.KeyFrames.Unexpected {
			if ch, ok := nearChangeID(frames, changes, id); ok {
				near = append(near, fmt.Sprintf("%d (%d after the change at %d)", id, id-ch, ch))
			} else {
				near = append(near, strconv.FormatUint(id, 10))
			}
			if len(near) == 5 {
				break
			}
		}
		fail("%d key frames %s: %s", n, what, joinMore(near, n))
	}
	if n := len(c.KeyFrames.MissingAfterChange); n > 0 {
		fail("%d rate changes without a key frame within %d frames (flush)", n, KeyWithinFrames)
	}

	// The frames that went into the stream, in order (all of them: the
	// qualification simulates no loss).
	var written []Frame
	for _, f := range frames {
		if f.Written {
			written = append(written, f)
		}
	}

	// Decoder: frame count, errors, frame types.
	if in.Decoded == nil {
		note("decode checks skipped: %s", in.DecodeSkipped)
	} else {
		d := in.Decoded
		c.Decode = &DecodeCheck{Frames: len(d.Frames), Errors: len(d.Errors), Warnings: d.Warnings}
		if len(d.Errors) > 0 {
			c.Decode.FirstError = d.Errors[0]
			fail("the decoder reported %d errors, first: %s", len(d.Errors), d.Errors[0])
		}
		if len(d.Frames) != len(written) {
			fail("the decoder output %d frames of the %d written", len(d.Frames), len(written))
		}
		c.KeyFrames.Mismatched = 0
		for i := 0; i < min(len(d.Frames), len(written)); i++ {
			if d.Frames[i].Intra() != written[i].Key {
				c.KeyFrames.Mismatched++
			}
		}
		if c.KeyFrames.Mismatched > 0 {
			fail("%d frames are intra in the bitstream but not flagged key by the encoder, or the other way round", c.KeyFrames.Mismatched)
		}
	}

	// Barcodes: each decoded frame shows its sequence number.
	switch {
	case !in.Barcode:
		note("barcode check skipped: %s", in.BarcodeSkipped)
	case in.Decoded == nil:
		note("barcode check skipped: not decoded")
	default:
		c.Barcode = checkBarcodes(written, in.Decoded.Frames)
		b := c.Barcode
		if b.Wrong > 0 || b.Gaps > 0 {
			fail("barcodes: %d frames show another frame's number, %d gaps", b.Wrong, b.Gaps)
		}
		if float64(b.Unreadable)*100 > MaxUnreadablePct*float64(max(b.Checked, 1)) {
			fail("barcodes: %d of %d unreadable (artifacts in the corner?)", b.Unreadable, b.Checked)
		} else if b.Unreadable > 0 {
			note("barcodes: %d of %d unreadable", b.Unreadable, b.Checked)
		}
	}

	// Sizes: every phase (the frames between two changes) is judged on its
	// P frames (no key frames, no idle repeats).
	sizeFails := followCheck(frames, changes, fps, max(in.SVCLayers, 1), &c.Follow)
	inconclusive := false
	if c.Follow.FirstPhase > 0 && c.Follow.FirstPhase < 1-SizeTolerance && len(sizeFails) > 0 {
		inconclusive = true
		note("P frames used %.0f %% of the start bitrate before any change: the source does not need it (use a higher-motion source)",
			c.Follow.FirstPhase*100)
	}

	switch {
	case len(c.Failures) > 0:
		c.Failures = append(c.Failures, sizeFails...)
		c.Verdict = VerdictFail
	case inconclusive:
		c.Failures = sizeFails
		c.Verdict = VerdictInconclusive
	case len(sizeFails) > 0:
		c.Failures = sizeFails
		c.Verdict = VerdictFail
	default:
		c.Verdict = VerdictPass
	}
}

// followCheck measures how P-frame sizes follow the targets of a stream with
// layers temporal layers and returns the failures.
func followCheck(frames []Frame, changes []int, fps, layers int, fc *FollowCheck) []string {
	var fails []string
	window := (WindowFrames + layers - 1) / layers * layers
	target := func(kbps int) float64 { return float64(kbps) * 1000 / 8 / float64(fps) }
	within := func(mean, t float64) bool { return mean >= t*(1-SizeTolerance) && mean <= t*(1+SizeTolerance) }
	bounds := append([]int{0}, changes...)
	bounds = append(bounds, len(frames))
	levels := map[int][]float64{}
	fc.SteadyMin, fc.SteadyMax = 0, 0
	first := true
	for p := 0; p+1 < len(bounds); p++ {
		from, to := bounds[p], bounds[p+1]
		var sizes []float64 // P frames of the phase
		for _, f := range frames[from:to] {
			if !f.Key && !f.Repeat {
				sizes = append(sizes, float64(f.Bytes))
			}
		}
		t := target(frames[from].Kbps)
		// Follow: only phases that start with a change.
		if p > 0 {
			lag := -1
			for k := 0; k+window <= len(sizes); k++ {
				if within(mean(sizes[k:k+window]), t) {
					lag = k
					break
				}
			}
			if len(sizes) < window {
				lag = 0 // too short to tell (a phase of a key frame and a frame or two)
			}
			switch {
			case lag < 0:
				fc.MaxLagFrames = -1
				fc.Late = append(fc.Late, frames[from].ID)
			case lag > FollowFrames:
				fc.Late = append(fc.Late, frames[from].ID)
				if fc.MaxLagFrames >= 0 {
					fc.MaxLagFrames = max(fc.MaxLagFrames, lag)
				}
			default:
				if fc.MaxLagFrames >= 0 {
					fc.MaxLagFrames = max(fc.MaxLagFrames, lag)
				}
			}
		}
		// Steady: the second half of the phase.
		if len(sizes) >= 2 {
			r := mean(sizes[len(sizes)/2:]) / t
			if first {
				fc.SteadyMin, fc.SteadyMax = r, r
				first = false
			}
			fc.SteadyMin, fc.SteadyMax = min(fc.SteadyMin, r), max(fc.SteadyMax, r)
			levels[frames[from].Kbps] = append(levels[frames[from].Kbps], r)
			if p == 0 {
				fc.FirstPhase = r
			}
		}
	}
	fc.Levels = map[string]float64{}
	for k, rs := range levels {
		fc.Levels[strconv.Itoa(k)] = round3(median(rs))
	}
	fc.SteadyMin, fc.SteadyMax, fc.FirstPhase = round3(fc.SteadyMin), round3(fc.SteadyMax), round3(fc.FirstPhase)
	if n := len(fc.Late); n > 0 {
		ids := make([]string, 0, 5)
		for _, id := range fc.Late[:min(n, 5)] {
			ids = append(ids, strconv.FormatUint(id, 10))
		}
		lag := "never within the phase"
		if fc.MaxLagFrames >= 0 {
			lag = fmt.Sprintf("up to %d frames", fc.MaxLagFrames)
		}
		fails = append(fails, fmt.Sprintf("P-frame sizes reached the new target later than %d frames after %d of the changes (%s; at %s)",
			FollowFrames, n, lag, joinMore(ids, n)))
	}
	if !first && (fc.SteadyMin < 1-SizeTolerance || fc.SteadyMax > 1+SizeTolerance) {
		fails = append(fails, fmt.Sprintf("steady P-frame sizes between %.0f %% and %.0f %% of the target (allowed %.0f-%.0f %%)",
			fc.SteadyMin*100, fc.SteadyMax*100, (1-SizeTolerance)*100, (1+SizeTolerance)*100))
	}
	return fails
}

// checkBarcodes compares the decoded barcodes with the written frames'
// sequence numbers (frame id minus the id of the latest sequence start).
// Gaps are jumps between consecutive readable values that the frames between
// them do not explain: by frame ids when the decoder output every written
// frame (a frame-id gap is reported as such, not again here), by position
// when it did not.
func checkBarcodes(written []Frame, decoded []DecodedFrame) *BarcodeCheck {
	b := &BarcodeCheck{}
	aligned := len(decoded) == len(written)
	seqBase := uint64(0)
	lastIdx, lastVal := -1, 0
	for i, d := range decoded {
		b.Checked++
		seqStart := false
		if i < len(written) {
			seqStart = written[i].SeqStart
			if seqStart || seqBase == 0 {
				seqBase = written[i].ID
			}
		}
		if d.Barcode < 0 {
			b.Unreadable++
			continue
		}
		if i < len(written) && uint16(written[i].ID-seqBase) != uint16(d.Barcode) {
			b.Wrong++
		}
		if lastIdx >= 0 && !seqStart {
			step := uint16(i - lastIdx)
			if aligned {
				step = uint16(written[i].ID - written[lastIdx].ID)
			}
			if uint16(d.Barcode-lastVal) != step {
				b.Gaps++
			}
		}
		lastIdx, lastVal = i, d.Barcode
	}
	return b
}

func nearChangeID(frames []Frame, changes []int, id uint64) (uint64, bool) {
	best, ok := uint64(0), false
	for _, ch := range changes {
		if c := frames[ch].ID; c <= id && id-c < 30 {
			best, ok = c, true
		}
	}
	return best, ok
}

func joinMore(items []string, total int) string {
	s := ""
	for i, it := range items {
		if i > 0 {
			s += ", "
		}
		s += it
	}
	if total > len(items) {
		s += fmt.Sprintf(" and %d more", total-len(items))
	}
	return s
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	return s[len(s)/2]
}
