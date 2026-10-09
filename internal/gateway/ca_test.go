package gateway

import (
	"bytes"
	"crypto/x509"
	"io"
	"log/slog"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/web"
)

// The private CA vouches for the gateway's own names and private addresses,
// not for other websites; a configured name it was not made for makes a new
// one (and an HTTPS certificate from it); a CA from before name constraints
// is kept.
func TestPrivateCAConstrained(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := func(names ...string) *Server {
		s, err := New(Config{Listen: "127.0.0.1:0", DataDir: dir, Web: web.FS(), Names: names}, log)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(s.ca.Cert)
		for _, n := range append([]string{"localhost", "127.0.0.1"}, names...) {
			if _, err := s.webCert.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: n}); err != nil {
				t.Errorf("the HTTPS certificate for %s: %v", n, err)
			}
		}
		return s
	}
	s := start("recon.example.com", "203.0.113.7")
	for n, want := range map[string]bool{
		"recon.example.com": true, "203.0.113.7": true, "gw.local": true, "pc.lan": true, "gw.tail1234.ts.net": true,
		"192.168.1.50": true, "10.1.2.3": true, "100.101.102.103": true, "fd7a:115c:a1e0::1": true,
		"www.bankofamerica.com": false, "mail.google.com": false, "example.com": false, "203.0.113.8": false, "1.1.1.1": false,
	} {
		if got := s.ca.Permits(n); got != want {
			t.Errorf("the CA permits %s: %v, want %v", n, got, want)
		}
	}
	first := s.ca.DER

	// Same names: the same CA.
	if s = start("recon.example.com", "203.0.113.7"); !bytes.Equal(s.ca.DER, first) {
		t.Fatal("the CA was made again for the same names")
	}
	// A name it was not made for: a new CA and certificate (start verifies).
	if s = start("recon.example.com", "203.0.113.7", "other.example.org"); bytes.Equal(s.ca.DER, first) || !s.ca.Permits("other.example.org") {
		t.Fatal("a configured name the CA was not made for kept the CA")
	}

	// A CA from before name constraints is kept as it is.
	old := t.TempDir()
	if _, err := tlsutil.CreateCA(old, caName, nil); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Listen: "127.0.0.1:0", DataDir: old, Web: web.FS(), Names: []string{"recon.example.com"}}, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.ca.Constrained() {
		t.Fatal("an older CA was replaced")
	}
}
