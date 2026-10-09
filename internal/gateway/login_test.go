package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/web"
)

const testPassword = "correct horse battery"

// newLoginServer is a gateway with the user "owner" (password testPassword),
// with 2FA when totp is set; it returns the TOTP secret.
func newLoginServer(t *testing.T, totp bool) (*Server, string) {
	t.Helper()
	s, err := New(Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), Web: web.FS()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	secret := ""
	if totp {
		secret = auth.NewTOTPSecret()
	}
	if err := s.store.Update(func(st *state) error {
		st.Users["owner"] = &User{Username: "owner", PasswordHash: hash, Admin: true, TOTPSecret: secret, Created: time.Now().UTC()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, secret
}

// publicPost sends a login-page POST from the address from ("ip:port").
func publicPost(s *Server, path, from string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "https://gw.lan"+path, bytes.NewReader(b))
	r.RemoteAddr = from
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Recon-CSRF", "public")
	r.Header.Set("Origin", "https://gw.lan")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestTOTPGuessLimits: the second factor is bounded per password login and
// per account, not per address. Someone who has the password and fresh
// addresses for every request (a /64 each, so the per-client limits never
// trip) gets 3 codes per password login and 5 wrong codes on the account,
// after which 2FA is refused from every address, the right code too, until
// the lock is over.
func TestTOTPGuessLimits(t *testing.T) {
	s, secret := newLoginServer(t, true)
	n := 0
	addr := func() string { n++; return fmt.Sprintf("[2001:db8:%x::1]:5000", n) }
	code := func(off time.Duration) string {
		c, err := auth.TOTPCode(secret, time.Now().Add(off))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	wrong := func() string { // none of the codes VerifyTOTP accepts now
		for i := 0; ; i++ {
			w := fmt.Sprintf("%06d", (123456+i*7919)%1_000_000)
			if w != code(-30*time.Second) && w != code(0) && w != code(30*time.Second) && w != code(60*time.Second) {
				return w
			}
		}
	}
	login := func() string {
		st, out := publicPost(s, "/api/login", addr(), map[string]string{"username": "owner", "password": testPassword})
		p, _ := out["pending"].(string)
		if st != http.StatusOK || out["totpRequired"] != true || p == "" {
			t.Fatalf("password login: HTTP %d %v", st, out)
		}
		return p
	}
	totp := func(pending, c string) (int, string) {
		st, out := publicPost(s, "/api/login/totp", addr(), map[string]string{"pending": pending, "code": c})
		msg, _ := out["error"].(string)
		return st, msg
	}

	// One password login: three codes, then it is spent (the login page goes
	// back to the password on "expired").
	p := login()
	for i := range 3 {
		if st, msg := totp(p, wrong()); st != http.StatusUnauthorized || (i == 2) != strings.Contains(msg, "expired") {
			t.Fatalf("wrong code %d: HTTP %d %q", i+1, st, msg)
		}
	}
	if st, msg := totp(p, code(0)); st != http.StatusUnauthorized || !strings.Contains(msg, "expired") {
		t.Fatalf("right code on a spent login: HTTP %d %q, want 401 expired", st, msg)
	}

	// The account: the 5th wrong code from anywhere locks 2FA everywhere.
	p = login()
	for i := range 2 {
		if st, msg := totp(p, wrong()); st != http.StatusUnauthorized {
			t.Fatalf("wrong code %d on the second login: HTTP %d %q", i+4, st, msg)
		}
	}
	for _, c := range []string{wrong(), code(0)} {
		if st, msg := totp(p, c); st != http.StatusTooManyRequests {
			t.Fatalf("code %s after 5 wrong codes: HTTP %d %q, want 429", c, st, msg)
		}
	}
	p2 := login()
	if st, msg := totp(p2, code(0)); st != http.StatusTooManyRequests {
		t.Fatalf("right code on a fresh login while locked: HTTP %d %q, want 429", st, msg)
	}
	locked := 0
	for _, e := range s.audit.Recent(50) {
		if e.Event == "totp_locked" {
			locked++
		}
	}
	if locked != 1 {
		t.Errorf("%d totp_locked audit entries, want 1", locked)
	}

	// Once the lock is over the owner gets in; a refused check did not use
	// up a try.
	s.totpLocks.mu.Lock()
	s.totpLocks.fails["owner"].until = time.Now()
	s.totpLocks.mu.Unlock()
	if st, msg := totp(p2, code(0)); st != http.StatusOK {
		t.Fatalf("right code after the lock: HTTP %d %q", st, msg)
	}
	if _, ok := s.totpLocks.fails["owner"]; ok {
		t.Error("a right code left the account's 2FA failures counted")
	}
}

// TestTOTPConcurrentGuesses: guesses sent all at once cannot get past the
// account's lock before the first failures are counted.
func TestTOTPConcurrentGuesses(t *testing.T) {
	s, _ := newLoginServer(t, true)
	var pendings []string
	for i := range 4 {
		st, out := publicPost(s, "/api/login", fmt.Sprintf("[2001:db8:%x::1]:5000", i+1), map[string]string{"username": "owner", "password": testPassword})
		p, _ := out["pending"].(string)
		if st != http.StatusOK || p == "" {
			t.Fatalf("password login: HTTP %d %v", st, out)
		}
		pendings = append(pendings, p)
	}
	codes := make(chan int, 12)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			<-start
			// "abcdef" is never a valid code.
			st, _ := publicPost(s, "/api/login/totp", fmt.Sprintf("[2001:db8:1%x::1]:5000", i), map[string]string{"pending": pendings[i%4], "code": "abcdef"})
			codes <- st
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusUnauthorized] != 5 || got[http.StatusTooManyRequests] != 7 {
		t.Fatalf("12 concurrent wrong codes: %v, want 5 checked (401) and 7 refused (429)", got)
	}
}

// TestRateKey: the per-client limits key IPv6 clients on their /64, so the
// addresses of one subscriber share one bucket.
func TestRateKey(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"203.0.113.9", "203.0.113.9"},
		{"::ffff:203.0.113.9", "203.0.113.9"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::ffff", "2001:db8:1:2::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"not-an-ip", "not-an-ip"},
	} {
		if got := rateKey(tc.ip); got != tc.want {
			t.Errorf("rateKey(%q) = %q, want %q", tc.ip, got, tc.want)
		}
	}
	s, _ := newLoginServer(t, false)
	for i := 1; i <= 6; i++ {
		st, out := publicPost(s, "/api/login/totp", fmt.Sprintf("[2001:db8:7::%x]:5000", i), map[string]string{"pending": "x", "code": "000000"})
		if want := map[bool]int{true: http.StatusTooManyRequests, false: http.StatusUnauthorized}[i == 6]; st != want {
			t.Fatalf("request %d from one /64: HTTP %d %v, want %d", i, st, out, want)
		}
	}
}
