package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse battery staple") {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword(h, "Correct horse battery staple") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("$argon2id$v=19$m=999999999,t=1,p=1$AAAA$AAAA", "x") {
		t.Fatal("absurd params accepted")
	}
}

// RFC 6238 Appendix B test vectors (SHA-1, 8 digits there; we use the last 6).
func TestTOTPVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	vectors := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for ts, want := range vectors {
		got, err := TOTPCode(secret, time.Unix(ts, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("t=%d got %s want %s", ts, got, want)
		}
	}
}

func TestTOTPReplay(t *testing.T) {
	s := NewTOTPSecret()
	now := time.Now()
	code, _ := TOTPCode(s, now)
	c, ok := VerifyTOTP(s, code, now, 0)
	if !ok {
		t.Fatal("valid code rejected")
	}
	if _, ok := VerifyTOTP(s, code, now, c); ok {
		t.Fatal("replayed code accepted")
	}
	if _, ok := VerifyTOTP(s, "000000", now, 0); ok && code != "000000" {
		t.Fatal("wrong code accepted")
	}
}

func TestTicket(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	type T struct {
		Host string `json:"h"`
		Exp  int64  `json:"exp"`
	}
	tok, err := SignTicket(key, T{"pc", 123})
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := VerifyTicket(key, tok, &out); err != nil || out.Host != "pc" || out.Exp != 123 {
		t.Fatalf("verify: %v %+v", err, out)
	}
	bad := tok[:len(tok)-2] + "AA"
	if VerifyTicket(key, bad, &out) == nil {
		t.Fatal("tampered ticket accepted")
	}
	if VerifyTicket([]byte("other-key-other-key-other-key-xx"), tok, &out) == nil {
		t.Fatal("ticket accepted with wrong key")
	}
}
