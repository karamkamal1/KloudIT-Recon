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

// TestTrustProxy: -trust-proxy takes CIDRs and a proxy's bare address (as the
// README words it), refuses what it cannot read instead of dropping it, and
// clientIP then reads the client from X-Forwarded-For only behind a trusted
// proxy (lockouts are keyed on user+IP).
func TestTrustProxy(t *testing.T) {
	for _, bad := range [][]string{{"cloudflared"}, {"127.0.0.1/33"}, {"10.0.0.0/8", ""}} {
		if _, err := parseTrustProxy(bad); err == nil {
			t.Errorf("parseTrustProxy(%q) accepted", bad)
		}
	}
	if _, err := New(Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), Web: web.FS(), TrustProxy: []string{"proxy.lan"}},
		slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("New accepted an unreadable -trust-proxy")
	}
	for _, tc := range []struct {
		proxies     []string
		remote, xff string
		want        string
	}{
		{nil, "127.0.0.1:5000", "203.0.113.9", "127.0.0.1"},
		{[]string{"127.0.0.1"}, "127.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{[]string{" 127.0.0.1 "}, "127.0.0.1:5000", "198.51.100.7, 203.0.113.9", "203.0.113.9"},
		{[]string{"127.0.0.1"}, "127.0.0.2:5000", "203.0.113.9", "127.0.0.2"},
		{[]string{"10.0.0.0/8"}, "10.1.2.3:5000", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{[]string{"::1"}, "[::1]:5000", "2001:db8::7", "2001:db8::7"},
		{[]string{"::1"}, "127.0.0.1:5000", "2001:db8::7", "127.0.0.1"},
	} {
		n, err := parseTrustProxy(tc.proxies)
		if err != nil {
			t.Fatalf("parseTrustProxy(%q): %v", tc.proxies, err)
		}
		s := &Server{proxies: n}
		r := httptest.NewRequest("POST", "https://gw.lan/api/login", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.xff)
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("proxies %q, from %s, X-Forwarded-For %q: client %s, want %s", tc.proxies, tc.remote, tc.xff, got, tc.want)
		}
	}
}
