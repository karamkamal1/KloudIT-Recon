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
