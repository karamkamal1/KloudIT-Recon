// Package auth implements password hashing (Argon2id), TOTP (RFC 6238), random
// tokens and HMAC-signed tickets.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. 46 MiB / t=1 / p=1 is one of the OWASP-recommended
// configurations; callers should bound concurrency (see gateway login limiter).
const (
	argonMemory  = 46 * 1024
	argonTime    = 1
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC Argon2id hash in constant time.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m > 1<<20 || t > 10 || p == 0 || p > 16 { // refuse absurd parameters from a tampered store
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a username does not exist, so that login
// timing does not reveal which accounts exist.
var dummyHash, _ = HashPassword("recon-dummy-password")

// BurnTime performs a password verification against a dummy hash.
func BurnTime(pw string) { VerifyPassword(dummyHash, pw) }

// RandomToken returns n random bytes, base64url encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash returns the SHA-256 of a token (what gets stored server-side).
func TokenHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// EqualHash compares two token hashes in constant time.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------------------------------------------------------------------------
// TOTP (RFC 6238, HMAC-SHA1, 30 s step, 6 digits)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random base32 secret (160 bits).
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPURI returns an otpauth:// URI for authenticator apps.
func TOTPURI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0xf
	code := (binary.BigEndian.Uint32(sum[off:]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// TOTPCode returns the code for time t.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(t.Unix()/30)), nil
}

// VerifyTOTP checks a code allowing ±1 step of clock skew. It rejects counters
// <= lastUsed (replay protection) and returns the matched counter.
func VerifyTOTP(secret, code string, t time.Time, lastUsed uint64) (uint64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil || len(code) != 6 {
		return 0, false
	}
	now := uint64(t.Unix() / 30)
	for _, c := range []uint64{now - 1, now, now + 1} {
		if c <= lastUsed {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, c)), []byte(code)) == 1 {
			return c, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Tickets: compact HMAC-SHA256 signed JSON with expiry.

var ErrTicket = errors.New("auth: invalid ticket")

// SignTicket serialises v (which must have an "exp" unix-seconds field honoured by
// VerifyTicket's caller via the returned expiry) and signs it.
func SignTicket(key []byte, v any) (string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// VerifyTicket checks the signature and unmarshals into v.
func VerifyTicket(key []byte, tok string, v any) error {
	i := strings.IndexByte(tok, '.')
	if i <= 0 || len(tok) > 4096 {
		return ErrTicket
	}
	body, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return ErrTicket
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil {
		return ErrTicket
	}
	m := hmac.New(sha256.New, key)
	m.Write(body)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return ErrTicket
	}
	if err := json.Unmarshal(body, v); err != nil {
		return ErrTicket
	}
	return nil
}
