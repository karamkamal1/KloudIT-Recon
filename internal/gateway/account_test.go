package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
)

// signedIn adds a login session of user to s and returns a function that
// POSTs to the API from it (cookie, CSRF token, same origin).
func signedIn(t *testing.T, s *Server, user string) func(path string, body any) (int, map[string]any) {
	t.Helper()
	tok := auth.RandomToken(32)
	ls := &LoginSession{TokenHash: auth.TokenHash(tok), Username: user, CSRF: auth.RandomToken(18), Created: time.Now(), LastSeen: time.Now()}
	if err := s.store.Update(func(st *state) error { st.Sessions[ls.TokenHash] = ls; return nil }); err != nil {
		t.Fatal(err)
	}
	return func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "https://gw.lan"+path, bytes.NewReader(b))
		r.RemoteAddr = "192.0.2.10:5000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Recon-CSRF", ls.CSRF)
		r.Header.Set("Origin", "https://gw.lan")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
}

// TestTOTPEnableNeedsPassword (final review): turning 2FA on needs the
// password, as turning it off does, and never replaces a secret that is on.
// A session left signed in (or a stolen cookie) could otherwise swap the
// account's second factor for its own and lock the owner out of 2FA logins.
func TestTOTPEnableNeedsPassword(t *testing.T) {
	for _, on := range []bool{false, true} {
		s, old := newLoginServer(t, on)
		post := signedIn(t, s, "owner")
		secret := auth.NewTOTPSecret()
		code, err := auth.TOTPCode(secret, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		secretNow := func() string {
			u, _ := s.store.GetUser("owner")
			return u.TOTPSecret
		}
		if !on {
			// A request without the password (what the page sent before).
			if st, out := post("/api/me/totp/enable", map[string]string{"secret": secret, "code": code}); st != http.StatusForbidden {
				t.Fatalf("enable without a password: HTTP %d %v, want 403", st, out)
			}
			for _, pw := range []string{"", "wrong password 123"} {
				if st, out := post("/api/me/totp/enable", map[string]string{"secret": secret, "code": code, "password": pw}); st != http.StatusForbidden {
					t.Fatalf("enable with password %q: HTTP %d %v, want 403", pw, st, out)
				}
			}
			if secretNow() != "" {
				t.Fatal("2FA turned on without the password")
			}
			if st, out := post("/api/me/totp/enable", map[string]string{"secret": secret, "code": code, "password": testPassword}); st != http.StatusOK {
				t.Fatalf("enable with the password: HTTP %d %v", st, out)
			}
			if secretNow() != secret {
				t.Fatal("2FA not on after enabling it with the password")
			}
			continue
		}
		// 2FA is on: no request replaces it, the password does not either.
		if st, out := post("/api/me/totp/enable", map[string]string{"secret": secret, "code": code, "password": testPassword}); st != http.StatusConflict {
			t.Fatalf("enable while 2FA is on: HTTP %d %v, want 409", st, out)
		}
		if secretNow() != old {
			t.Fatal("the account's 2FA secret was replaced")
		}
		// Off (with the password), then on again with the new secret.
		if st, out := post("/api/me/totp/disable", map[string]string{"password": testPassword}); st != http.StatusOK {
			t.Fatalf("disable: HTTP %d %v", st, out)
		}
		if st, out := post("/api/me/totp/enable", map[string]string{"secret": secret, "code": code, "password": testPassword}); st != http.StatusOK {
			t.Fatalf("enable after disabling: HTTP %d %v", st, out)
		}
		if secretNow() != secret {
			t.Fatal("the new secret is not on")
		}
	}
}
