package gateway

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

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

	// For TLS servers only: a code-signing or e-mail certificate made with
	// the CA's key is refused by a verifier that applies the CA's EKU to the
	// chain (Go here; Windows' certificate store too).
	if eku := s.ca.Cert.ExtKeyUsage; len(eku) != 1 || eku[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("the CA's extended key usage %v, want server authentication only", eku)
	}
	roots := x509.NewCertPool()
	roots.AddCert(s.ca.Cert)
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning, x509.ExtKeyUsageEmailProtection, x509.ExtKeyUsageServerAuth} {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "recon.example.com"}, DNSNames: []string{"recon.example.com"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{u}}
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca.Cert, &key.PublicKey, s.ca.Key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(der)
		_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{u}})
		if (err == nil) != (u == x509.ExtKeyUsageServerAuth) {
			t.Errorf("a certificate for extended key usage %d made with the CA's key: verify error %v", u, err)
		}
	}

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

// TestCATailnet: a configured name in a tailnet narrows the CA's ts.net to
// that tailnet, so it cannot vouch for another tailnet's names or Tailscale
// Funnel's public hosts; without one the CA keeps ts.net (a tailnet name
// configured later needs no new CA).
func TestCATailnet(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  map[string]bool
	}{
		{[]string{"gw.tail1234.ts.net", "recon.example.com"}, map[string]bool{
			"gw.tail1234.ts.net": true, "pc.tail1234.ts.net": true, "recon.example.com": true, "gw.lan": true, "100.101.102.103": true,
			"gw.tail9999.ts.net": false, "funnel-host.ts.net": false, "ts.net": false}},
		{[]string{"*.Tail1234.ts.net", "gw.tail5678.ts.net"}, map[string]bool{
			"gw.tail1234.ts.net": true, "gw.tail5678.ts.net": true, "gw.tail9999.ts.net": false}},
		{[]string{"recon.example.com", "ts.net.example.org"}, map[string]bool{
			"gw.tail1234.ts.net": true, "gw.tail9999.ts.net": true, "ts.net.example.org": true}},
	} {
		cs := caConstraints(c.names)
		for n, want := range c.want {
			if got := tlsutil.Permits(cs.Domains, cs.Ranges, n); got != want {
				t.Errorf("names %v: the CA permits %s: %v, want %v (domains %v)", c.names, n, got, want, cs.Domains)
			}
		}
	}
}
