package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiagnosticsNoSecrets: "recon-host report" has the config without its
// secrets, the end of host.log (and of host.log.old before it), and none of
// the config's token or gateway pin, pairing codes, secret-named fields or
// long opaque strings anywhere, also where host.log would carry them.
func TestDiagnosticsNoSecrets(t *testing.T) {
	dir := t.TempDir()
	const token = "tok_9f8e7d6c5b4a39281706f5e4d3c2b1a0ZYXWVU"
	const pin = "sha256/Q2hhbmdlTWVQbGVhc2VUaGlzSXNBUGluVmFsdWUxMjM0NQ=="
	cfgPath := filepath.Join(dir, "host.json")
	if err := os.WriteFile(cfgPath, []byte(`{"name":"vm","gateway":"192.168.5.202:8443","gatewayPin":"`+pin+`","hostId":"h-123",`+
		`"token":"`+token+`","capture":"auto","consoleSession":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "host.log")
	old := "time=1 level=INFO msg=\"old line\"\n"
	cur := "time=2 level=INFO msg=\"agent started\"\n" +
		"time=3 level=WARN msg=\"pairing\" code=recon1:eyJnIjoiMTkyLjE2OC41LjIwMjo4NDQzIiwidCI6InNlY3JldCJ9 token=" + token + "\n" +
		"time=4 level=INFO msg=x ticket=\"abc.def.ghi\" password=hunter2 key=9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928\n" +
		"time=5 level=INFO msg=\"stream stats\" fps=30 path=github.com/karamkamal1/kloudit-recon/internal/host/media.(*Video).Start\n"
	if err := os.WriteFile(logPath+".old", []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(cur), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteDiagnostics(context.Background(), DiagnosticsOptions{Config: cfg, ConfigPath: cfgPath, LogPath: logPath, LogLines: 10}, &out); err != nil {
		t.Fatal(err)
	}
	r := out.String()
	t.Logf("report:\n%s", r)
	for _, secret := range []string{token, pin, "Q2hhbmdlTWVQbGVhc2VUaGlzSXNBUGluVmFsdWUxMjM0NQ", "eyJnIjoi", "hunter2", "abc.def.ghi",
		"9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928"} {
		if strings.Contains(r, secret) {
			t.Errorf("the report has %q", secret)
		}
	}
	for _, want := range []string{"KloudIT Recon diagnostics report", "== Session and desktop", "== Graphics", "== Sound",
		`"token": "[redacted]"`, `"gatewayPin": "[redacted]"`, `"gateway": "192.168.5.202:8443"`, `"hostId": "h-123"`,
		`"consoleSession": "off"`, "(last 1 lines of " + logPath + ".old)", "old line", "(last 4 lines of " + logPath + ")",
		"code=recon1:[redacted]", "password=[redacted]", `ticket=[redacted]`, "fps=30",
		"github.com/karamkamal1/kloudit-recon/internal/host/media.(*Video).Start"} {
		if !strings.Contains(r, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
}
