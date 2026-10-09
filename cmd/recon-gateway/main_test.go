package main

import (
	"slices"
	"testing"
)

// TestEnvList: the repeatable flags' environment variables (RECON_NAMES,
// RECON_TRUST_PROXY in gateway.env) are comma-separated lists.
func TestEnvList(t *testing.T) {
	t.Setenv("RECON_TRUST_PROXY", " 192.168.1.2/32, ,fd00::/8,")
	if got := envList("RECON_TRUST_PROXY"); !slices.Equal(got, []string{"192.168.1.2/32", "fd00::/8"}) {
		t.Fatalf("envList = %q", got)
	}
	t.Setenv("RECON_TRUST_PROXY", "")
	if got := envList("RECON_TRUST_PROXY"); got != nil {
		t.Fatalf("empty: %q", got)
	}
}
