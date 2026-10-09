package tlsutil

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// A CA made with name constraints vouches for the names under them and for
// no other website: Go's verifier (as browsers' and the OSes') refuses a
// certificate it issued for another name, and Permits agrees with it.
func TestCAConstraints(t *testing.T) {
	_, lan, _ := net.ParseCIDR("192.168.0.0/16")
	_, one, _ := net.ParseCIDR("203.0.113.7/32")
	ca, err := CreateCA(t.TempDir(), "test CA", &Constraints{Domains: []string{"local", "recon.example.com"}, Ranges: []*net.IPNet{lan, one}})
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Constrained() {
		t.Fatal("not constrained")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"gw.local", true},
		{"local", true},
		{"recon.example.com", true},
		{"a.recon.example.com", true},
		{"192.168.1.50", true},
		{"203.0.113.7", true},
		{"www.bankofamerica.com", false},
		{"mail.google.com", false},
		{"example.com", false},
		{"evilrecon.example.com", false},
		{"notlocal", false},
		{"203.0.113.8", false},
		{"8.8.8.8", false},
		{"2001:db8::1", false},
	} {
		if got := ca.Permits(tc.name); got != tc.ok {
			t.Errorf("Permits(%q) = %v, want %v", tc.name, got, tc.ok)
		}
		leaf, err := ca.Issue([]string{tc.name}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: tc.name})
		if (err == nil) != tc.ok {
			t.Errorf("verify a certificate for %q: %v, want ok=%v", tc.name, err, tc.ok)
		}
	}

	// Without constraints (as before them) it permits everything.
	old, err := CreateCA(t.TempDir(), "old CA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Constrained() || !old.Permits("www.bankofamerica.com") || !old.Permits("8.8.8.8") {
		t.Fatal("an unconstrained CA reports constraints")
	}
}
