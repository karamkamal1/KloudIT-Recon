package host

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigAcceptsBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.json")
	// Windows PowerShell 5.1's Set-Content -Encoding UTF8 writes a byte-order mark.
	if err := os.WriteFile(p, []byte("\xef\xbb\xbf{\"name\":\"PC\",\"directPort\":1234}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "PC" || c.DirectPort != 1234 {
		t.Fatalf("got name %q port %d", c.Name, c.DirectPort)
	}
}

func TestReloadPairing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.json")
	c, err := LoadConfig(p) // missing file: unpaired defaults
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: c, log: slog.New(slog.NewTextHandler(io.Discard, nil)), pairing: pairingOf(c)}
	if a.reloadPairing() {
		t.Fatal("no file yet, but reported a change")
	}
	c2 := *c
	c2.Gateway, c2.GatewayPin, c2.HostID, c2.Token = "192.0.2.1:8443", "pin", "h1", "tok"
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	if !a.reloadPairing() || a.pair().Token != "tok" || a.pair().Gateway != "192.0.2.1:8443" {
		t.Fatalf("pairing not picked up: %+v", a.pair())
	}
	if a.reloadPairing() {
		t.Fatal("unchanged file reported as a change")
	}
	c2.Token = "tok2"
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	// Make sure the mtime differs even on coarse-grained filesystems.
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(p, later, later)
	if !a.reloadPairing() || a.pair().Token != "tok2" {
		t.Fatalf("re-pair not picked up: %+v", a.pair())
	}
}

func TestConfigCongestion(t *testing.T) {
	dir := t.TempDir()
	load := func(json string) (*Config, error) {
		p := filepath.Join(dir, "host.json")
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	// media by default since the rate controller (GUIDE 2.2).
	c, err := load(`{}`)
	if err != nil || c.congestion() != "media" {
		t.Fatalf("default: %v %q", err, c.congestion())
	}
	// The default is not written back, so a later release can change it.
	if err := c.Save(filepath.Join(dir, "saved.json")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "saved.json")); strings.Contains(string(b), "congestion") {
		t.Fatalf("default congestion saved: %s", b)
	}
	if c, err := load(`{"congestion":"media"}`); err != nil || c.congestion() != "media" {
		t.Fatalf("media: %v", err)
	}
	if c, err := load(`{"congestion":"reno"}`); err != nil || c.congestion() != "reno" {
		t.Fatalf("reno: %v", err)
	}
	if _, err := load(`{"congestion":"bbr"}`); err == nil {
		t.Fatal("unknown congestion controller accepted")
	}
}

func TestConfigGPUPriority(t *testing.T) {
	dir := t.TempDir()
	load := func(json string) (*Config, error) {
		p := filepath.Join(dir, "host.json")
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(`{}`)
	if err != nil || c.gpuPriority() != "auto" {
		t.Fatalf("default: %v %q", err, c.gpuPriority())
	}
	for _, m := range []string{"auto", "high", "realtime", "off"} {
		if c, err := load(`{"gpuPriority":"` + m + `"}`); err != nil || c.gpuPriority() != m {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if _, err := load(`{"gpuPriority":"normal"}`); err == nil || !strings.Contains(err.Error(), "gpuPriority") {
		t.Fatalf("unknown gpuPriority: %v", err)
	}
}

func TestConfigPipeline(t *testing.T) {
	dir := t.TempDir()
	load := func(json string) (*Config, error) {
		p := filepath.Join(dir, "host.json")
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(`{}`)
	if err != nil || c.pipeline() != "auto" {
		t.Fatalf("default: %v %q", err, c.pipeline())
	}
	for _, m := range []string{"auto", "helper", "ffmpeg"} {
		if c, err := load(`{"pipeline":"` + m + `"}`); err != nil || c.pipeline() != m {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if _, err := load(`{"pipeline":"native"}`); err == nil || !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("unknown pipeline: %v", err)
	}
}

func TestConfigVirtualDisplay(t *testing.T) {
	dir := t.TempDir()
	load := func(json string) (*Config, error) {
		p := filepath.Join(dir, "host.json")
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(`{"hostId":"h1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if o := c.virtualDisplayOptions(); o.Policy != "off" || o.Layout != "primary" || o.StateDir != dir || o.MonitorID != "h1" {
		t.Fatalf("defaults: %+v", o)
	}
	for _, p := range []string{"off", "auto", "on"} {
		for _, l := range []string{"primary", "extend", "only"} {
			c, err := load(`{"virtualDisplay":"` + p + `","virtualDisplayLayout":"` + l + `"}`)
			if err != nil || c.virtualDisplayOptions().Policy != p || c.virtualDisplayOptions().Layout != l {
				t.Fatalf("%s/%s: %v", p, l, err)
			}
		}
	}
	if _, err := load(`{"virtualDisplay":"yes"}`); err == nil || !strings.Contains(err.Error(), "virtualDisplay") {
		t.Fatalf("unknown virtualDisplay: %v", err)
	}
	if _, err := load(`{"virtualDisplayLayout":"mirror"}`); err == nil || !strings.Contains(err.Error(), "virtualDisplayLayout") {
		t.Fatalf("unknown virtualDisplayLayout: %v", err)
	}
}

// TestConfigPhase5Rate: svc and staticBitrate default to auto, take auto or
// off; staticKbps is not negative; fpsFloor is 0 or 10..240.
func TestConfigPhase5Rate(t *testing.T) {
	dir := t.TempDir()
	load := func(json string) (*Config, error) {
		p := filepath.Join(dir, "host.json")
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(`{}`)
	if err != nil || !c.svc() || !c.staticBitrate() || c.StaticKbps != 0 || c.FPSFloor != 0 {
		t.Fatalf("defaults: %v svc %v static %v", err, c.svc(), c.staticBitrate())
	}
	if c, err := load(`{"svc":"off","staticBitrate":"off","staticKbps":1500,"fpsFloor":45}`); err != nil || c.svc() || c.staticBitrate() ||
		c.StaticKbps != 1500 || c.FPSFloor != 45 {
		t.Fatalf("off: %v", err)
	}
	if c, err := load(`{"svc":"auto","staticBitrate":"auto"}`); err != nil || !c.svc() || !c.staticBitrate() {
		t.Fatalf("auto: %v", err)
	}
	for _, bad := range []string{`{"svc":"on"}`, `{"staticBitrate":"2000"}`, `{"staticKbps":-1}`, `{"fpsFloor":5}`, `{"fpsFloor":480}`} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestConfigPhase5Options: host config roi, encoderInstance (a string or a
// JSON number), reencodeOversized and sliceOutput (Phase 5 wiring B): the
// defaults (roi auto, the default engine, both experiments off), accepted
// values, the ones refused, and a saved config that loads again.
func TestConfigPhase5Options(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "host.json")
	load := func(json string) (*Config, error) {
		if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := load(`{}`)
	if err != nil || c.roi() != "auto" || c.EncoderInstance != "" || c.ReencodeOversized != 0 || c.SliceOutput != 0 {
		t.Fatalf("defaults: %v roi %q instance %q reencode %v slices %d", err, c.roi(), c.EncoderInstance, c.ReencodeOversized, c.SliceOutput)
	}
	for _, ok := range []struct {
		json, roi, instance string
		reencode            float64
		slices              int
	}{
		{`{"roi":"off"}`, "off", "", 0, 0},
		{`{"roi":"cursor","encoderInstance":"dedicated"}`, "cursor", "dedicated", 0, 0},
		{`{"roi":"center","encoderInstance":1}`, "center", "1", 0, 0},
		{`{"roi":"auto","encoderInstance":"auto","reencodeOversized":2.5,"sliceOutput":4}`, "auto", "auto", 2.5, 4},
		{`{"encoderInstance":"0","reencodeOversized":100,"sliceOutput":64}`, "auto", "0", 100, 64},
	} {
		c, err := load(ok.json)
		if err != nil || c.roi() != ok.roi || string(c.EncoderInstance) != ok.instance || c.ReencodeOversized != ok.reencode || c.SliceOutput != ok.slices {
			t.Fatalf("%s: %v roi %q instance %q reencode %v slices %d", ok.json, err, c.roi(), c.EncoderInstance, c.ReencodeOversized, c.SliceOutput)
		}
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
		if again, err := LoadConfig(p); err != nil || again.EncoderInstance != c.EncoderInstance || again.ROI != c.ROI {
			t.Fatalf("%s saved and loaded: %v %+v", ok.json, err, again)
		}
	}
	for _, bad := range []string{`{"roi":"on"}`, `{"roi":"pointer"}`, `{"encoderInstance":"fast"}`, `{"encoderInstance":-1}`,
		`{"encoderInstance":16}`, `{"encoderInstance":true}`, `{"reencodeOversized":1.2}`, `{"reencodeOversized":101}`,
		`{"reencodeOversized":-2}`, `{"sliceOutput":-1}`, `{"sliceOutput":65}`} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
