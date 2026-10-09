package host

import (
	"fmt"
	"strings"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Automatic codec choice (guide step 4.2): the host's encoders against the
// browser's decoders, decided per client by the decode times its browser
// measured.
//
// Host side, from what the host can encode, not from GPU names: a family is
// available when an encoder of it passed the probe's test encode and has not
// failed in this session (usableEncoder). A GPU without an AV1 encoder (AMD
// before RDNA3, NVIDIA before RTX 40) fails av1_amf's or av1_nvenc's test
// encode, so AV1 is simply not there. RDNA3's AV1 encoder pads pictures that
// are not 64x16-aligned (the step 1.7 probe, or the native helper's caps:
// Session.alignment); chooseEncoder then gives way to HEVC, and here such an
// encoder never replaces another family.
//
// Client side: the hello's decoders (isConfigSupported; hw: a hardware
// decoder that does not hold frames back) and their timing: a 1920x1080
// sample decoded one frame at a time with the decoder the stream would use
// (web/static/js/decoder-selftest.js).
//
// The rule: the first tier of autoTiers with a candidate; in it the first
// family in the tier's order (HEVC on hardware at both ends), unless a later
// one decodes clearly faster (fasterBy) at the stream's picture size; in the
// software encode tier always the first (its order is the host's encode
// cost, which the client's times do not tell). AV1 competes on speed only
// with Config.AV1 "faster". 4:4:4 is never chosen: the host encodes 4:2:0
// only (HEVC Range Extensions decode in Chrome exists on NVIDIA and Intel
// GPUs, not AMD; docs/VENDOR_NOTES.md 4.2).

// AV1 policies (Config.AV1).
const (
	// AV1Fallback: AV1 is chosen automatically only where HEVC does not work
	// end-to-end (then before H.264, as before step 4.2); it never replaces
	// HEVC.
	AV1Fallback = "fallback"
	// AV1Faster: AV1 also replaces HEVC (or H.264) for a client that decodes
	// it clearly faster.
	AV1Faster = "faster"
)

func validAV1(s string) bool { return s == "" || s == AV1Fallback || s == AV1Faster }

// autoTiers are the automatic choice's tiers, tried in turn until one has a
// candidate; each lists the families in its order of preference.
var autoTiers = []struct {
	name         string
	hwEnc, hwDec bool
	order        []string
}{
	// HEVC first on both vendors: AV1 becomes a host's default only after its
	// encoder is measured (Phase 0 latency, VMAF): Config.AV1.
	{"hardware encode and decode", true, true, []string{"hevc", "av1", "h264"}},
	// The browser decodes in software: H.264 is the cheapest to decode.
	{"hardware encode, software decode", true, false, []string{"h264", "hevc", "av1"}},
	// Software encoding: x264 is the cheapest to encode. The order is the
	// host's CPU cost, so decode times do not reorder it (negotiateEncoder).
	{"software encode", false, false, []string{"h264", "av1", "hevc"}},
}

// codecCandidate is a family both ends can use in a tier: the host's
// preferred usable encoder of it and the client's decoder.
type codecCandidate struct {
	enc  media.EncoderInfo
	dec  proto.DecoderInfo
	pads bool // enc pads the session's picture (Caps.Pads)
}

// "Clearly faster", per frame at the stream's picture size. A family that
// compresses at least as well as the pick must save more than the sample's
// noise: gainShare of the pick's time and gainMs. One that compresses worse
// (H.264 against HEVC or AV1: roughly a third more bits for the same picture)
// must save worseShare and worseMs, so that a decoder that is only a little
// quicker does not cost the stream its quality.
const (
	gainShare  = 0.10
	gainMs     = 0.5
	worseShare = 0.25
	worseMs    = 2.0
)

// efficiency ranks the families by compression at streaming bitrates. HEVC
// and AV1 rank alike: hardware AV1 encoders at low-latency settings gain
// little over HEVC, and whether they do on a host is what VMAF measures
// before Config.AV1 "faster".
var efficiency = map[string]int{"h264": 0, "hevc": 1, "av1": 1}

// chooseFamily picks among a tier's candidates, given in its order of
// preference (at least one). The first is the default; a later one replaces
// the pick when the client decodes it clearly faster (fasterBy), except AV1
// unless policy is AV1Faster, and an encoder that pads the w x h picture
// (chooseEncoder would give way again). It returns the pick and why.
func chooseFamily(cands []codecCandidate, policy string, w, h int) (codecCandidate, string) {
	pick, why := cands[0], "first choice"
	for _, c := range cands[1:] {
		if c.pads || (c.enc.Family == "av1" && policy != AV1Faster) {
			continue
		}
		if fasterBy(c, pick, w, h) {
			why = fmt.Sprintf("%s decodes clearly faster than %s (%.2f vs %.2f ms per %dx%d frame)", c.enc.Family, pick.enc.Family,
				c.dec.Timing.Ms, pick.dec.Timing.Ms, c.dec.Timing.W, c.dec.Timing.H)
			pick = c
		}
	}
	return pick, why
}

// fasterBy reports whether candidate c decodes clearly faster than pick at a
// w x h picture. Both must have been timed.
func fasterBy(c, pick codecCandidate, w, h int) bool {
	tc, tp := decodeEstimate(c.dec, w, h), decodeEstimate(pick.dec, w, h)
	if tc <= 0 || tp <= 0 {
		return false
	}
	share, ms := gainShare, gainMs
	if efficiency[c.enc.Family] < efficiency[pick.enc.Family] {
		share, ms = worseShare, worseMs
	}
	gain := tp - tc
	return gain >= ms && gain >= share*tp
}

// decodeEstimate returns the client's decode time of a family per frame of a
// w x h picture (0, 0: unknown) as the comparison uses it, or 0 when it was
// not timed: the timed sample's, scaled down by pixel count for a smaller
// picture, never up for a larger one. A frame's time is a fixed cost per call
// (a hardware decoder's round trip to the GPU process) plus work that grows
// with the picture, and one sample cannot tell them apart. A difference
// between two families' times shrinks with a smaller picture if it is all
// per pixel and stays as measured on a larger one if it is all fixed cost;
// the estimate takes the smaller of the two, so a family replaces another
// only for a gain it has at the stream's size whatever the split (the share
// of the pick's time is the same either way).
func decodeEstimate(d proto.DecoderInfo, w, h int) float64 {
	t := d.Timing
	if t == nil || t.Ms <= 0 {
		return 0
	}
	if w > 0 && h > 0 && t.W > 0 && t.H > 0 && w*h < t.W*t.H {
		return t.Ms * float64(w*h) / float64(t.W*t.H)
	}
	return t.Ms
}

// decoderSummary describes the hello's decoders for the log, e.g.
// "hevc:hw:2.10ms@1920x1080 av1:hw:1.85ms@1920x1080 h264:sw:-" (hw: a
// hardware decoder that does not hold frames back; then the timed decode).
func decoderSummary(ds []proto.DecoderInfo) string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteByte(' ')
		}
		kind := "sw"
		if d.HW {
			kind = "hw"
		}
		fmt.Fprintf(&b, "%s:%s:", d.Family, kind)
		if t := d.Timing; t != nil && t.Ms > 0 {
			fmt.Fprintf(&b, "%.2fms@%dx%d", t.Ms, t.W, t.H)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
