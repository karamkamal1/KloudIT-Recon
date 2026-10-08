package qualify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// FileName is the results file, next to the host config (host.json).
const FileName = "live-bitrate.json"

// ResultsVersion is the results file format (2: cells carry the quality
// preset and LTR slots they ran with; version 1 measured neither).
const ResultsVersion = 2

// PathFor returns the results file for a host config file.
func PathFor(configPath string) string { return filepath.Join(filepath.Dir(configPath), FileName) }

// Live-bitrate modes a session can use (Choose).
const (
	ModeSeamless = "seamless" // setRate changes the rate from the next frame, no IDR
	ModeFlush    = "flush"    // setRate makes an IDR (and an AMF Flush + ReInit)
	// ModeRestart: seamless failed and flush did not pass: every bitrate
	// change starts a new helper (as on the FFmpeg path).
	ModeRestart = "restart"
)

// Qualities are the start's quality presets sessions may ask for (the
// client's encoder preset setting), each qualified on its own.
var Qualities = []string{"speed", "balanced", "quality"}

// DefaultQuality is the helper's quality preset when start has none.
const DefaultQuality = "speed"

// Source describes what the runs encoded.
type Source struct {
	Capture string `json:"capture"` // synthetic-gpu | dda | amd-direct | synthetic (mock)
	Motion  bool   `json:"motion"`  // the synthetic-gpu high-motion source
	Monitor int    `json:"monitor,omitempty"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	Barcode bool   `json:"barcode"`
}

// Schedule is the rate schedule of every run.
type Schedule struct {
	HighKbps   int `json:"highKbps"` // the start rate, and every second step
	LowKbps    int `json:"lowKbps"`
	StepMs     int `json:"stepMs"`
	DurationMs int `json:"durationMs"`
	StepFrames int `json:"stepFrames"` // StepMs in frames: the first frame at a new rate is 1 + k * StepFrames
	Frames     int `json:"frames"`     // planned frames per run
}

// Choice is what a session uses for one codec and quality preset (Choose),
// written to the file for people (sessions recompute it from the cells).
type Choice struct {
	// AdaptiveRC / Adaptive: the rate-control mode and live-bitrate mode
	// of a session whose rate controller changes the bitrate (GUIDE 10:
	// adaptive = the 3.6 winner).
	AdaptiveRC string `json:"adaptiveRc,omitempty"`
	Adaptive   string `json:"adaptive,omitempty"`
	// Fixed: the live-bitrate mode of a session at a fixed bitrate (rc vbr):
	// user changes of the bitrate setting.
	Fixed string `json:"fixed,omitempty"`
}

// Results is the results file (live-bitrate.json next to host.json).
type Results struct {
	Version       int       `json:"version"`
	Time          time.Time `json:"time"`
	Host          string    `json:"host,omitempty"`
	HelperVersion string    `json:"helperVersion"`
	Backend       string    `json:"backend"` // amf | nvenc | lavc | mock
	Vendor        string    `json:"vendor"`
	AdapterName   string    `json:"adapterName"`
	AdapterLUID   string    `json:"adapterLuid,omitempty"` // changes with every boot: informational
	// TestDouble: measured against the NVENC test double; TestEncoder:
	// with the libavcodec backend's test encoders (Options.LavcTestEncoder,
	// software). Neither is ever used by sessions.
	TestDouble  bool     `json:"testDouble,omitempty"`
	TestEncoder string   `json:"testEncoder,omitempty"`
	Source      Source   `json:"source"`
	Schedule    Schedule `json:"schedule"`
	Criteria    Criteria `json:"criteria"`
	Notes       []string `json:"notes,omitempty"`
	Cells       []Cell   `json:"cells"`
	// Choice: per codec and quality preset.
	Choice map[string]map[string]Choice `json:"choice"`
}

// Save writes the results file (indented JSON).
func (r *Results) Save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a results file; a missing file is (nil, nil).
func Load(path string) (*Results, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Results
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if r.Version != ResultsVersion {
		return nil, fmt.Errorf("%s: results version %d, this recon-host reads %d (run recon-host qualify again)", path, r.Version, ResultsVersion)
	}
	return &r, nil
}

// Matches reports whether the results were measured on the encoder of a
// helper with these caps (same backend and adapter; why not otherwise).
// Results of the NVENC test double or of the libavcodec backend's test
// encoders never match.
func (r *Results) Matches(c encoder.Caps) (bool, string) {
	switch {
	case r == nil:
		return false, "no results"
	case r.TestDouble:
		return false, "measured with the NVENC test double"
	case r.TestEncoder != "":
		return false, "measured with the libavcodec backend's test encoder " + r.TestEncoder
	case r.Backend != c.Backend:
		return false, fmt.Sprintf("measured on backend %s, the helper runs %s", r.Backend, c.Backend)
	case r.AdapterName != c.AdapterName:
		return false, fmt.Sprintf("measured on %q, the helper runs on %q", r.AdapterName, c.AdapterName)
	}
	return true, ""
}

// stream is what a cell ran besides rc and live-bitrate mode: Choose uses
// only cells that ran a session's stream exactly so.
type stream struct {
	codec, quality string
	ltrSlots       int
}

// streamOf returns the stream of a session's start (no quality: the
// helper's default preset).
func streamOf(sp encoder.StartParams) stream {
	q := sp.Quality
	if q == "" {
		q = DefaultQuality
	}
	return stream{sp.Codec, q, sp.LTRSlots}
}

func (r *Results) cell(s stream, rc, mode string) *Cell {
	for i := range r.Cells {
		if c := &r.Cells[i]; c.Codec == s.codec && c.Quality == s.quality && c.LTRSlots == s.ltrSlots && c.RC == rc &&
			c.LiveBitrate == mode {
			return c
		}
	}
	return nil
}

func (r *Results) verdict(s stream, rc, mode string) string {
	if c := r.cell(s, rc, mode); c != nil {
		return c.Verdict
	}
	return ""
}

// pick returns the live-bitrate mode for a stream and rc: seamless where it
// passed, else flush where it passed, else restart where seamless was judged
// and failed (never the helper's default then, which may be seamless); ok
// false otherwise (seamless not measured or not conclusively, flush not
// passed).
func (r *Results) pick(s stream, rc string) (string, bool) {
	switch {
	case r.verdict(s, rc, ModeSeamless) == VerdictPass:
		return ModeSeamless, true
	case r.verdict(s, rc, ModeFlush) == VerdictPass:
		return ModeFlush, true
	case r.verdict(s, rc, ModeSeamless) == VerdictFail:
		return ModeRestart, true
	}
	return "", false
}

// Choose returns the rate-control mode (start's rc) and live-bitrate mode
// (seamless | flush | restart) a stream should use on a helper with caps c,
// from these results; sp is its start (codec, quality preset, LTR slots: only
// cells that ran exactly that count). ok false where they say nothing about
// it (other GPU or backend; codec, preset, LTR slots or mode not measured):
// then the helper's defaults apply. adaptive: the session's rate controller
// changes the bitrate; it runs CBR where CBR changes seamlessly, else the
// first of PEAK_CONSTRAINED_VBR and LATENCY_CONSTRAINED_VBR that does (GUIDE
// 10: adaptive = the 3.6 winner), else CBR with flush (or restart). A
// fixed-bitrate stream runs rc vbr (the backend's low-latency VBR) with its
// own measured mode.
func (r *Results) Choose(c encoder.Caps, sp encoder.StartParams, adaptive bool) (rc, mode string, ok bool) {
	if m, _ := r.Matches(c); !m {
		return "", "", false
	}
	s := streamOf(sp)
	rc = "vbr"
	if adaptive {
		for _, alt := range []string{"cbr", "vbr_peak", "vbr"} {
			if r.verdict(s, alt, ModeSeamless) == VerdictPass {
				return alt, ModeSeamless, true
			}
		}
		rc = "cbr"
	}
	if mode, ok = r.pick(s, rc); !ok {
		return "", "", false
	}
	return rc, mode, true
}

// Choices computes from the cells what a session on this GPU uses for every
// codec and quality preset measured (Choose; test runs' results too, for the
// record).
func (r *Results) Choices() map[string]map[string]Choice {
	out := map[string]map[string]Choice{}
	c := encoder.Caps{Backend: r.Backend, AdapterName: r.AdapterName}
	rr := *r
	rr.TestDouble, rr.TestEncoder = false, ""
	for _, cell := range r.Cells {
		if _, done := out[cell.Codec][cell.Quality]; done {
			continue
		}
		sp := encoder.StartParams{Codec: cell.Codec, Quality: cell.Quality, LTRSlots: cell.LTRSlots}
		var ch Choice
		if rc, mode, ok := rr.Choose(c, sp, true); ok {
			ch.AdaptiveRC, ch.Adaptive = rc, mode
		}
		if _, mode, ok := rr.Choose(c, sp, false); ok {
			ch.Fixed = mode
		}
		if out[cell.Codec] == nil {
			out[cell.Codec] = map[string]Choice{}
		}
		out[cell.Codec][cell.Quality] = ch
	}
	return out
}

// ChoiceLines describes Choices, one line per codec and quality preset
// ("hevc speed: adaptive cbr/seamless, fixed vbr/flush"; "-": the helper's
// default), codecs and presets in their usual order.
func (r *Results) ChoiceLines() []string {
	var out []string
	choices := r.Choices()
	for _, codec := range sortedBy(codecOrder, choices) {
		for _, q := range sortedBy(Qualities, choices[codec]) {
			ch := choices[codec][q]
			out = append(out, fmt.Sprintf("%s %s: adaptive %s/%s, fixed vbr/%s", codec, q, dash(ch.AdaptiveRC), dash(ch.Adaptive), dash(ch.Fixed)))
		}
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sortedBy returns the keys of m, those in order first (in that order), then
// the others sorted.
func sortedBy[V any](order []string, m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if x, y := rank(order, keys[i]), rank(order, keys[j]); x != y {
			return x < y
		}
		return keys[i] < keys[j]
	})
	return keys
}

// fillChoice writes Choices into the file's Choice.
func (r *Results) fillChoice() { r.Choice = r.Choices() }

// Print writes a table of the cells and the choice.
func (r *Results) Print(w io.Writer) {
	fmt.Fprintf(w, "live-bitrate qualification: %s %s (%s), helper %s, %s %dx%d@%d, %d <-> %d kbps every %d ms for %d ms\n",
		r.Backend, r.AdapterName, r.Vendor, r.HelperVersion, r.Source.Capture, r.Source.Width, r.Source.Height, r.Source.FPS,
		r.Schedule.HighKbps, r.Schedule.LowKbps, r.Schedule.StepMs, r.Schedule.DurationMs)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "codec\tquality\tltr\trc\tliveBitrate\tverdict\tframes\tchanges\tkeys\tmax lag\tsteady %\tbarcode\twhy")
	for _, c := range r.Cells {
		keys := fmt.Sprintf("%d unexpected", len(c.KeyFrames.Unexpected))
		if c.LiveBitrate == ModeFlush {
			keys += fmt.Sprintf(", %d missing", len(c.KeyFrames.MissingAfterChange))
		}
		lag := "-"
		if c.RateChanges > 0 {
			lag = fmt.Sprint(c.Follow.MaxLagFrames)
		}
		steady := fmt.Sprintf("%.0f-%.0f", c.Follow.SteadyMin*100, c.Follow.SteadyMax*100)
		bc := "skipped"
		if c.Barcode != nil {
			bc = fmt.Sprintf("%d/%d ok", c.Barcode.Checked-c.Barcode.Unreadable-c.Barcode.Wrong, c.Barcode.Checked)
		}
		why := strings.Join(c.Failures, "; ")
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", c.Codec, c.Quality, c.LTRSlots, c.RC, c.LiveBitrate,
			strings.ToUpper(c.Verdict), c.Frames, c.RateChanges, keys, lag, steady, bc, why)
	}
	tw.Flush()
	for _, codec := range sortedBy(codecOrder, r.Choice) {
		for _, q := range sortedBy(Qualities, r.Choice[codec]) {
			ch := r.Choice[codec][q]
			fmt.Fprintf(w, "%s, quality %s: adaptive bitrate -> rc %s, live bitrate %s; fixed bitrate (rc vbr) -> %s\n", codec, q,
				orDash(ch.AdaptiveRC), orDash(ch.Adaptive), orDash(ch.Fixed))
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "note:", n)
	}
}

func orDash(s string) string {
	if s == "" {
		return "helper default (not measured)"
	}
	return s
}

func rank(list []string, v string) int {
	if i := slices.Index(list, v); i >= 0 {
		return i
	}
	return len(list)
}

// sortCells orders cells by codec (hevc, av1, h264), quality preset (speed,
// balanced, quality), rc (cbr, vbr, vbr_peak) and mode (seamless, flush).
func sortCells(cells []Cell) {
	sort.SliceStable(cells, func(i, j int) bool {
		a, b := cells[i], cells[j]
		if x, y := rank(codecOrder, a.Codec), rank(codecOrder, b.Codec); x != y {
			return x < y
		}
		if x, y := rank(Qualities, a.Quality), rank(Qualities, b.Quality); x != y {
			return x < y
		}
		if x, y := rank(rcOrder, a.RC), rank(rcOrder, b.RC); x != y {
			return x < y
		}
		return rank(modeOrder, a.LiveBitrate) < rank(modeOrder, b.LiveBitrate)
	})
}

var (
	codecOrder = []string{"hevc", "av1", "h264"}
	rcOrder    = []string{"cbr", "vbr", "vbr_peak"}
	modeOrder  = []string{ModeSeamless, ModeFlush}
)
