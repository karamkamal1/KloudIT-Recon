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

// ResultsVersion is the results file format.
const ResultsVersion = 1

// PathFor returns the results file for a host config file.
func PathFor(configPath string) string { return filepath.Join(filepath.Dir(configPath), FileName) }

// Live-bitrate modes a session can use (Choose).
const (
	ModeSeamless = "seamless" // setRate changes the rate from the next frame, no IDR
	ModeFlush    = "flush"    // setRate makes an IDR (and an AMF Flush + ReInit)
	// ModeRestart: neither passed: every bitrate change starts a new helper
	// (as on the FFmpeg path).
	ModeRestart = "restart"
)

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

// Choice is what a session uses for one codec (Choose), written to the file
// for people (sessions recompute it from the cells).
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
	Backend       string    `json:"backend"` // amf | nvenc | mock
	Vendor        string    `json:"vendor"`
	AdapterName   string    `json:"adapterName"`
	AdapterLUID   string    `json:"adapterLuid,omitempty"` // changes with every boot: informational
	// TestDouble: measured against the NVENC test double, never used by
	// sessions.
	TestDouble bool              `json:"testDouble,omitempty"`
	Source     Source            `json:"source"`
	Schedule   Schedule          `json:"schedule"`
	Criteria   Criteria          `json:"criteria"`
	Notes      []string          `json:"notes,omitempty"`
	Cells      []Cell            `json:"cells"`
	Choice     map[string]Choice `json:"choice"`
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
// Results of the NVENC test double never match.
func (r *Results) Matches(c encoder.Caps) (bool, string) {
	switch {
	case r == nil:
		return false, "no results"
	case r.TestDouble:
		return false, "measured with the NVENC test double"
	case r.Backend != c.Backend:
		return false, fmt.Sprintf("measured on backend %s, the helper runs %s", r.Backend, c.Backend)
	case r.AdapterName != c.AdapterName:
		return false, fmt.Sprintf("measured on %q, the helper runs on %q", r.AdapterName, c.AdapterName)
	}
	return true, ""
}

func (r *Results) cell(codec, rc, mode string) *Cell {
	for i := range r.Cells {
		if c := &r.Cells[i]; c.Codec == codec && c.RC == rc && c.LiveBitrate == mode {
			return c
		}
	}
	return nil
}

func (r *Results) passed(codec, rc, mode string) bool {
	c := r.cell(codec, rc, mode)
	return c != nil && c.Passed()
}

// judged reports whether a cell ran and was judged pass or fail (not an
// error, not inconclusive: those say nothing about the encoder).
func (r *Results) judged(codec, rc, mode string) bool {
	c := r.cell(codec, rc, mode)
	return c != nil && (c.Verdict == VerdictPass || c.Verdict == VerdictFail)
}

// pick returns the live-bitrate mode for codec and rc: seamless where it
// passed, else flush where it passed, else restart where both were judged;
// ok false otherwise (not measured, or not conclusively).
func (r *Results) pick(codec, rc string) (string, bool) {
	switch {
	case r.passed(codec, rc, ModeSeamless):
		return ModeSeamless, true
	case r.passed(codec, rc, ModeFlush):
		return ModeFlush, true
	case r.judged(codec, rc, ModeSeamless) && r.judged(codec, rc, ModeFlush):
		return ModeRestart, true
	}
	return "", false
}

// Choose returns the rate-control mode (start's rc) and live-bitrate mode
// (seamless | flush | restart) a stream of codec should use on a helper with
// caps c, from these results; ok false where they say nothing about it (other
// GPU or backend, codec or mode not measured): then the helper's defaults
// apply. adaptive: the session's rate controller changes the bitrate; it
// runs CBR where CBR changes seamlessly, else the first of PEAK_CONSTRAINED_VBR
// and LATENCY_CONSTRAINED_VBR that does (GUIDE 10: adaptive = the 3.6
// winner), else CBR with flush (or restart). A fixed-bitrate stream runs rc
// vbr (the backend's low-latency VBR) with its own measured mode.
func (r *Results) Choose(c encoder.Caps, codec string, adaptive bool) (rc, mode string, ok bool) {
	if m, _ := r.Matches(c); !m {
		return "", "", false
	}
	rc = "vbr"
	if adaptive {
		if r.passed(codec, "cbr", ModeSeamless) {
			return "cbr", ModeSeamless, true
		}
		for _, alt := range []string{"vbr_peak", "vbr"} {
			if r.passed(codec, alt, ModeSeamless) {
				return alt, ModeSeamless, true
			}
		}
		rc = "cbr"
	}
	if mode, ok = r.pick(codec, rc); !ok {
		return "", "", false
	}
	return rc, mode, true
}

// Choices computes from the cells what a session on this GPU uses for every
// codec measured (Choose; the test double's results too, for the record).
func (r *Results) Choices() map[string]Choice {
	out := map[string]Choice{}
	c := encoder.Caps{Backend: r.Backend, AdapterName: r.AdapterName}
	rr := *r
	rr.TestDouble = false
	for _, cell := range r.Cells {
		if _, done := out[cell.Codec]; done {
			continue
		}
		var ch Choice
		if rc, mode, ok := rr.Choose(c, cell.Codec, true); ok {
			ch.AdaptiveRC, ch.Adaptive = rc, mode
		}
		if _, mode, ok := rr.Choose(c, cell.Codec, false); ok {
			ch.Fixed = mode
		}
		out[cell.Codec] = ch
	}
	return out
}

// fillChoice writes Choices into the file's Choice.
func (r *Results) fillChoice() { r.Choice = r.Choices() }

// Print writes a table of the cells and the choice.
func (r *Results) Print(w io.Writer) {
	fmt.Fprintf(w, "live-bitrate qualification: %s %s (%s), helper %s, %s %dx%d@%d, %d <-> %d kbps every %d ms for %d ms\n",
		r.Backend, r.AdapterName, r.Vendor, r.HelperVersion, r.Source.Capture, r.Source.Width, r.Source.Height, r.Source.FPS,
		r.Schedule.HighKbps, r.Schedule.LowKbps, r.Schedule.StepMs, r.Schedule.DurationMs)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "codec\trc\tliveBitrate\tverdict\tframes\tchanges\tkeys\tmax lag\tsteady %\tbarcode\twhy")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", c.Codec, c.RC, c.LiveBitrate, strings.ToUpper(c.Verdict),
			c.Frames, c.RateChanges, keys, lag, steady, bc, why)
	}
	tw.Flush()
	codecs := make([]string, 0, len(r.Choice))
	for k := range r.Choice {
		codecs = append(codecs, k)
	}
	sort.Strings(codecs)
	for _, k := range codecs {
		ch := r.Choice[k]
		fmt.Fprintf(w, "%s: adaptive bitrate -> rc %s, live bitrate %s; fixed bitrate (rc vbr) -> %s\n", k, orDash(ch.AdaptiveRC),
			orDash(ch.Adaptive), orDash(ch.Fixed))
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

// sortCells orders cells by codec (hevc, av1, h264), rc (cbr, vbr,
// vbr_peak) and mode (seamless, flush).
func sortCells(cells []Cell) {
	rank := func(list []string, v string) int {
		if i := slices.Index(list, v); i >= 0 {
			return i
		}
		return len(list)
	}
	sort.SliceStable(cells, func(i, j int) bool {
		a, b := cells[i], cells[j]
		if x, y := rank(codecOrder, a.Codec), rank(codecOrder, b.Codec); x != y {
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
