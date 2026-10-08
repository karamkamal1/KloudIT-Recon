package qualify

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

func results(cells ...Cell) *Results {
	r := &Results{Version: ResultsVersion, Backend: "amf", Vendor: "amd", AdapterName: "AMD Radeon RX 7900 XT", Cells: cells}
	r.fillChoice()
	return r
}

// cell is a cell run as a session starts the codec on the RX 7900 XT: the
// helper's default preset, two LTR slots.
func cell(codec, rc, mode, verdict string) Cell {
	return Cell{Codec: codec, Quality: "speed", LTRSlots: 2, RC: rc, LiveBitrate: mode, Verdict: verdict}
}

func TestChoose(t *testing.T) {
	amd := encoder.Caps{Backend: "amf", Vendor: "amd", AdapterName: "AMD Radeon RX 7900 XT"}
	type want struct {
		rc, mode string
		ok       bool
	}
	// A session's start: no quality preset (the helper's default, speed),
	// two LTR slots (encoder.Caps.LTRSlots for recovery ltr).
	checkStart := func(t *testing.T, r *Results, c encoder.Caps, sp encoder.StartParams, adaptive bool, w want) {
		t.Helper()
		rc, mode, ok := r.Choose(c, sp, adaptive)
		if rc != w.rc || mode != w.mode || ok != w.ok {
			t.Fatalf("Choose(%+v, adaptive %v) = %q %q %v, want %q %q %v", sp, adaptive, rc, mode, ok, w.rc, w.mode, w.ok)
		}
	}
	check := func(t *testing.T, r *Results, c encoder.Caps, codec string, adaptive bool, w want) {
		t.Helper()
		checkStart(t, r, c, encoder.StartParams{Codec: codec, LTRSlots: 2}, adaptive, w)
	}
	// Everything passes: CBR seamless for adaptive streams, vbr seamless
	// for fixed ones.
	all := results(cell("hevc", "cbr", "seamless", "pass"), cell("hevc", "cbr", "flush", "pass"),
		cell("hevc", "vbr", "seamless", "pass"), cell("hevc", "vbr", "flush", "pass"),
		cell("hevc", "vbr_peak", "seamless", "pass"), cell("hevc", "vbr_peak", "flush", "pass"))
	check(t, all, amd, "hevc", true, want{"cbr", "seamless", true})
	check(t, all, amd, "hevc", false, want{"vbr", "seamless", true})
	check(t, all, amd, "av1", true, want{"", "", false}) // not measured: the helper's defaults
	if ch := all.Choice["hevc"]["speed"]; ch != (Choice{AdaptiveRC: "cbr", Adaptive: "seamless", Fixed: "seamless"}) || len(all.Choice["hevc"]) != 1 {
		t.Fatalf("choice %+v", all.Choice)
	}
	// Only cells run exactly as the session starts count: the same preset
	// (none = speed) and LTR slots.
	checkStart(t, all, amd, encoder.StartParams{Codec: "hevc", Quality: "speed", LTRSlots: 2}, true, want{"cbr", "seamless", true})
	checkStart(t, all, amd, encoder.StartParams{Codec: "hevc", Quality: "balanced", LTRSlots: 2}, true, want{"", "", false})
	checkStart(t, all, amd, encoder.StartParams{Codec: "hevc", LTRSlots: 0}, true, want{"", "", false})
	bal := results(cell("hevc", "cbr", "seamless", "fail"), cell("hevc", "cbr", "flush", "pass"))
	bal.Cells[0].Quality, bal.Cells[1].Quality = "balanced", "balanced"
	checkStart(t, bal, amd, encoder.StartParams{Codec: "hevc", Quality: "balanced", LTRSlots: 2}, true, want{"cbr", "flush", true})
	check(t, bal, amd, "hevc", true, want{"", "", false})
	// CBR does not change seamlessly, PEAK_CONSTRAINED_VBR does: adaptive
	// streams run that (GUIDE 10: the 3.6 winner).
	r := results(cell("hevc", "cbr", "seamless", "fail"), cell("hevc", "cbr", "flush", "pass"),
		cell("hevc", "vbr", "seamless", "fail"), cell("hevc", "vbr", "flush", "pass"),
		cell("hevc", "vbr_peak", "seamless", "pass"), cell("hevc", "vbr_peak", "flush", "pass"))
	check(t, r, amd, "hevc", true, want{"vbr_peak", "seamless", true})
	check(t, r, amd, "hevc", false, want{"vbr", "flush", true})
	// No seamless anywhere: CBR with flush.
	r = results(cell("av1", "cbr", "seamless", "fail"), cell("av1", "cbr", "flush", "pass"))
	check(t, r, amd, "av1", true, want{"cbr", "flush", true})
	// Neither passed: a new helper per change.
	r = results(cell("h264", "cbr", "seamless", "fail"), cell("h264", "cbr", "flush", "fail"))
	check(t, r, amd, "h264", true, want{"cbr", "restart", true})
	// Seamless failed and flush could not tell: never the helper's default
	// (seamless on AMF), a new helper per change.
	r = results(cell("h264", "cbr", "seamless", "fail"), cell("h264", "cbr", "flush", "error"))
	check(t, r, amd, "h264", true, want{"cbr", "restart", true})
	r = results(cell("h264", "cbr", "seamless", "fail"), cell("h264", "cbr", "flush", "inconclusive"))
	check(t, r, amd, "h264", true, want{"cbr", "restart", true})
	r = results(cell("h264", "cbr", "seamless", "fail"))
	check(t, r, amd, "h264", true, want{"cbr", "restart", true})
	// Seamless runs that did not happen or could not tell say nothing (with
	// flush not passed): the helper's defaults.
	r = results(cell("h264", "cbr", "seamless", "error"), cell("h264", "cbr", "flush", "inconclusive"))
	check(t, r, amd, "h264", true, want{"", "", false})
	r = results(cell("h264", "cbr", "seamless", "inconclusive"), cell("h264", "cbr", "flush", "fail"))
	check(t, r, amd, "h264", true, want{"", "", false})
	r = results(cell("h264", "cbr", "seamless", "error"), cell("h264", "cbr", "flush", "pass"))
	check(t, r, amd, "h264", true, want{"cbr", "flush", true})
	// Another GPU or backend, the test double, no results: nothing.
	check(t, all, encoder.Caps{Backend: "amf", AdapterName: "AMD Radeon RX 6800"}, "hevc", true, want{"", "", false})
	check(t, all, encoder.Caps{Backend: "nvenc", AdapterName: "AMD Radeon RX 7900 XT"}, "hevc", true, want{"", "", false})
	td := results(cell("hevc", "cbr", "seamless", "pass"))
	td.TestDouble = true
	check(t, td, amd, "hevc", true, want{"", "", false})
	var none *Results
	check(t, none, amd, "hevc", true, want{"", "", false})
}

func TestResultsFile(t *testing.T) {
	dir := t.TempDir()
	if got := PathFor(filepath.Join(dir, "host.json")); got != filepath.Join(dir, "live-bitrate.json") {
		t.Fatalf("PathFor %s", got)
	}
	path := PathFor(filepath.Join(dir, "host.json"))
	if r, err := Load(path); r != nil || err != nil {
		t.Fatalf("missing file: %v %v", r, err)
	}
	r := results(cell("hevc", "cbr", "seamless", "pass"), cell("hevc", "cbr", "flush", "pass"))
	r.Cells[0].Follow.Levels = map[string]float64{"50000": 0.98}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got == nil || len(got.Cells) != 2 || got.Cells[0].Follow.Levels["50000"] != 0.98 || got.Cells[0].Quality != "speed" ||
		got.Cells[0].LTRSlots != 2 || got.Choice["hevc"]["speed"].Adaptive != "seamless" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// A byte-order mark (PowerShell 5.1 edits) is fine; another version is not.
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, append([]byte("\xef\xbb\xbf"), b...), 0o600)
	if _, err := Load(path); err != nil {
		t.Fatalf("BOM: %v", err)
	}
	_ = os.WriteFile(path, bytes.Replace(b, []byte(`"version": 2`), []byte(`"version": 9`), 1), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "results version 9") {
		t.Fatalf("version: %v", err)
	}
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestResultsPrint(t *testing.T) {
	r := results(cell("h264", "cbr", "flush", "pass"), cell("hevc", "vbr", "seamless", "fail"), cell("hevc", "cbr", "seamless", "pass"),
		cell("hevc", "vbr", "flush", "fail"), cell("hevc", "cbr", "seamless", "fail"))
	r.Cells[1].Failures = []string{"2 key frames after the first: 121, 241"}
	r.Cells[4].Quality = "balanced"
	r.fillChoice()
	sortCells(r.Cells)
	if r.Cells[0].RC != "cbr" || r.Cells[1].RC != "vbr" || r.Cells[2].LiveBitrate != "flush" || r.Cells[3].Quality != "balanced" ||
		r.Cells[4].Codec != "h264" {
		t.Fatalf("order: %+v", r.Cells)
	}
	var b bytes.Buffer
	r.Print(&b)
	out := b.String()
	for _, want := range []string{"hevc   speed     2    vbr  seamless     FAIL", "2 key frames after the first",
		"hevc, quality speed: adaptive bitrate -> rc cbr, live bitrate seamless; fixed bitrate (rc vbr) -> restart",
		"hevc, quality balanced: adaptive bitrate -> rc cbr, live bitrate restart; fixed bitrate (rc vbr) -> helper default (not measured)",
		"h264, quality speed: adaptive bitrate -> rc cbr, live bitrate flush; fixed bitrate (rc vbr) -> helper default (not measured)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "hevc, quality speed") || strings.Index(out, "hevc, quality speed") > strings.Index(out, "hevc, quality balanced") ||
		strings.Index(out, "hevc, quality balanced") > strings.Index(out, "h264, quality speed") {
		t.Errorf("choice order:\n%s", out)
	}
	if got := strings.Join(r.ChoiceLines(), "; "); got != "hevc speed: adaptive cbr/seamless, fixed vbr/restart; "+
		"hevc balanced: adaptive cbr/restart, fixed vbr/-; h264 speed: adaptive cbr/flush, fixed vbr/-" {
		t.Errorf("choice lines %q", got)
	}
}
