package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/gateway"
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

// TestUserRecoverySignsOut (final review): the README's account recovery
// (`user passwd`, `user reset-2fa`, run with the gateway stopped) signs the
// account out everywhere: its login sessions are gone from the state the
// gateway reads at its next start, other accounts keep theirs, and the
// recovery is recorded for the hosts (gateway.User.Recovered).
func TestUserRecoverySignsOut(t *testing.T) {
	dir := t.TempDir()
	state := `{"users":{"owner":{"username":"owner","passwordHash":"x","totpSecret":"SECRET"},"guest":{"username":"guest","passwordHash":"y"}},
"sessions":{"a":{"tokenHash":"a","username":"owner"},"b":{"tokenHash":"b","username":"guest"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := func() []string {
		var st struct {
			Sessions map[string]struct{ Username string } `json:"sessions"`
		}
		b, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		var out []string
		for k := range st.Sessions {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin })
	w.WriteString("a new password 123\n")
	w.Close()
	userCmd(dir, []string{"passwd", "owner"})
	if got := sessions(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("sessions after user passwd: %q, want only the guest's", got)
	}
	store, err := gateway.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := store.GetUser("owner")
	if !auth.VerifyPassword(u.PasswordHash, "a new password 123") || u.Recovered.IsZero() || u.TOTPSecret != "SECRET" {
		t.Fatalf("owner after user passwd: %+v", u)
	}
	if g, _ := store.GetUser("guest"); !g.Recovered.IsZero() {
		t.Error("user passwd recorded a recovery of another account")
	}

	// reset-2fa signs out too (the session the owner signed in with since).
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	userCmd(dir, []string{"reset-2fa", "owner"})
	if got := sessions(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("sessions after user reset-2fa: %q, want only the guest's", got)
	}
	store, err = gateway.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := store.GetUser("owner"); u.TOTPSecret != "" || u.Recovered.IsZero() {
		t.Fatalf("owner after user reset-2fa: %+v", u)
	}
}
