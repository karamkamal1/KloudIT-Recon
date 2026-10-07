package gateway

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/web"
)

func TestPublicAddr(t *testing.T) {
	for _, tc := range []struct{ public, host, want string }{
		{"", "192.168.1.50:8443", "192.168.1.50:8443"},
		{"", "recon.lan", "recon.lan:9443"},
		{"203.0.113.7:443", "x", "203.0.113.7:443"},
		{"203.0.113.7", "x", "203.0.113.7:9443"},
		{"home.example.com", "x", "home.example.com:9443"},
		{"2001:db8::1", "x", "[2001:db8::1]:9443"},
		{"[2001:db8::1]", "x", "[2001:db8::1]:9443"},
	} {
		s := &Server{cfg: Config{PublicAddr: tc.public, Listen: ":9443"}}
		r := httptest.NewRequest("GET", "https://"+tc.host+"/", nil)
		if got := s.publicAddr(r); got != tc.want {
			t.Errorf("publicAddr(%q, host %q) = %q, want %q", tc.public, tc.host, got, tc.want)
		}
	}
}

func TestSetupTokenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := func() string {
		s, err := New(Config{Listen: "127.0.0.1:0", DataDir: dir, Web: web.FS()}, log)
		if err != nil {
			t.Fatal(err)
		}
		return s.setupTok
	}
	first := start()
	if len(first) < 16 {
		t.Fatalf("setup token %q too short", first)
	}
	b, err := os.ReadFile(filepath.Join(dir, "setup-token.txt"))
	if err != nil || strings.TrimSpace(string(b)) != first {
		t.Fatalf("token file %q, err %v", b, err)
	}
	if again := start(); again != first {
		t.Fatalf("token changed across restart: %q -> %q", first, again)
	}
}
