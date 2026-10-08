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

func cell(codec, rc, mode, verdict string) Cell {
	return Cell{Codec: codec, RC: rc, LiveBitrate: mode, Verdict: verdict}
}

func TestChoose(t *testing.T) {
	amd := encoder.Caps{Backend: "amf", Vendor: "amd", AdapterName: "AMD Radeon RX 7900 XT"}
	type want struct {
		rc, mode string
		ok       bool
	}
	check := func(t *testing.T, r *Results, c encoder.Caps, codec string, adaptive bool, w want) {
		t.Helper()
		rc, mode, ok := r.Choose(c, codec, adaptive)
		if rc != w.rc || mode != w.mode || ok != w.ok {
			t.Fatalf("Choose(%s, adaptive %v) = %q %q %v, want %q %q %v", codec, adaptive, rc, mode, ok, w.rc, w.mode, w.ok)
		}
	}
	// Everything passes: CBR seamless for adaptive streams, vbr seamless
	// for fixed ones.
	all := results(cell("hevc", "cbr", "seamless", "pass"), cell("hevc", "cbr", "flush", "pass"),
		cell("hevc", "vbr", "seamless", "pass"), cell("hevc", "vbr", "flush", "pass"),
		cell("hevc", "vbr_peak", "seamless", "pass"), cell("hevc", "vbr_peak", "flush", "pass"))
	check(t, all, amd, "hevc", true, want{"cbr", "seamless", true})
	check(t, all, amd, "hevc", false, want{"vbr", "seamless", true})
	check(t, all, amd, "av1", true, want{"", "", false}) // not measured: the helper's defaults
	if ch := all.Choice["hevc"]; ch != (Choice{AdaptiveRC: "cbr", Adaptive: "seamless", Fixed: "seamless"}) {
		t.Fatalf("choice %+v", ch)
	}
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
	// Runs that did not happen or could not tell say nothing.
	r = results(cell("h264", "cbr", "seamless", "error"), cell("h264", "cbr", "flush", "inconclusive"))
	check(t, r, amd, "h264", true, want{"", "", false})
	r = results(cell("h264", "cbr", "seamless", "fail"), cell("h264", "cbr", "flush", "error"))
	check(t, r, amd, "h264", true, want{"", "", false})
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
	if err != nil || got == nil || len(got.Cells) != 2 || got.Cells[0].Follow.Levels["50000"] != 0.98 || got.Choice["hevc"].Adaptive != "seamless" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// A byte-order mark (PowerShell 5.1 edits) is fine; another version is not.
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, append([]byte("\xef\xbb\xbf"), b...), 0o600)
	if _, err := Load(path); err != nil {
		t.Fatalf("BOM: %v", err)
	}
	_ = os.WriteFile(path, bytes.Replace(b, []byte(`"version": 1`), []byte(`"version": 9`), 1), 0o600)
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
		cell("hevc", "vbr", "flush", "fail"))
	r.Cells[1].Failures = []string{"2 key frames after the first: 121, 241"}
	sortCells(r.Cells)
	if r.Cells[0].RC != "cbr" || r.Cells[1].RC != "vbr" || r.Cells[2].LiveBitrate != "flush" || r.Cells[3].Codec != "h264" {
		t.Fatalf("order: %+v", r.Cells)
	}
	var b bytes.Buffer
	r.Print(&b)
	out := b.String()
	for _, want := range []string{"hevc   vbr  seamless     FAIL", "2 key frames after the first",
		"hevc: adaptive bitrate -> rc cbr, live bitrate seamless; fixed bitrate (rc vbr) -> restart",
		"h264: adaptive bitrate -> rc cbr, live bitrate flush; fixed bitrate (rc vbr) -> helper default (not measured)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
