// Package tlsutil generates and manages the certificates used by Recon.
//
//   - A long-lived private CA (gateway) that signs the HTTPS certificate, so a
//     user can optionally install one root and get a trusted padlock.
//   - Short-lived (< 14 day) ECDSA P-256 certificates for WebTransport endpoints,
//     which browsers accept via `serverCertificateHashes` without any trust
//     store changes. They rotate automatically and both the current and the
//     previous hash are published so connections never race a rotation.
//   - A stable tunnel identity for the gateway<->host QUIC link, pinned by the
//     host through its SPKI hash.
package tlsutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

// CA is a private certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	DER  []byte
}

// LoadOrCreateCA loads ca.crt/ca.key from dir or creates them.
func LoadOrCreateCA(dir, name string) (*CA, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if ca, err := loadCA(certPath, keyPath); err == nil {
		return ca, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"KloudIT Recon"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", kb, 0o600); err != nil {
		return nil, err
	}
	return loadCA(certPath, keyPath)
}

func loadCA(certPath, keyPath string) (*CA, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return nil, errors.New("tlsutil: bad CA PEM")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, DER: cblk.Bytes}, nil
}

// PEM returns the CA certificate in PEM form.
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.DER})
}

// Issue creates a leaf certificate for the given DNS names / IPs.
func (ca *CA) Issue(names []string, validity time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: firstOr(names, "recon")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	addSANs(tmpl, names)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, ca.DER}, PrivateKey: key, Leaf: leaf}, nil
}

func firstOr(s []string, d string) string {
	if len(s) > 0 {
		return s[0]
	}
	return d
}

func addSANs(tmpl *x509.Certificate, names []string) {
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
}

// SelfSigned creates a self-signed ECDSA P-256 certificate. With validity under
// 14 days it satisfies the WebTransport serverCertificateHashes requirements.
func SelfSigned(names []string, validity time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: firstOr(names, "recon")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity - time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	addSANs(tmpl, names)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// CertHash is the SHA-256 of the leaf certificate's DER encoding, the format
// used by WebTransport serverCertificateHashes.
func CertHash(c *tls.Certificate) [32]byte {
	return sha256.Sum256(c.Certificate[0])
}

// SPKIPin returns base64(SHA-256(SubjectPublicKeyInfo)) of a certificate.
func SPKIPin(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// LoadOrCreateKeyCert loads a persisted leaf (used for the tunnel identity) or
// creates a self-signed one with the given validity.
func LoadOrCreateKeyCert(certPath, keyPath string, names []string, validity time.Duration) (tls.Certificate, error) {
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if c.Leaf == nil {
			c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
		}
		if c.Leaf != nil && time.Until(c.Leaf.NotAfter) > 30*24*time.Hour {
			return c, nil
		}
	}
	c, err := SelfSigned(names, validity)
	if err != nil {
		return c, err
	}
	if err := SaveKeyCert(certPath, keyPath, c); err != nil {
		return c, err
	}
	return c, nil
}

// SaveKeyCert writes a certificate chain and its private key as PEM.
func SaveKeyCert(certPath, keyPath string, c tls.Certificate) error {
	var certPEM []byte
	for _, d := range c.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey.(crypto.Signer))
	if err != nil {
		return err
	}
	return writePEM(keyPath, "PRIVATE KEY", kb, 0o600)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Rotating serves a short-lived self-signed certificate for a WebTransport
// endpoint and replaces it well before expiry.
type Rotating struct {
	names    []string
	validity time.Duration
	every    time.Duration

	mu       sync.RWMutex
	cur      *tls.Certificate
	prev     *tls.Certificate
	rotated  time.Time
	onRotate func()
}

// NewRotating creates a rotating certificate source. validity must be < 14 days.
func NewRotating(names []string, validity, every time.Duration) (*Rotating, error) {
	r := &Rotating{names: names, validity: validity, every: every}
	if err := r.rotate(); err != nil {
		return nil, err
	}
	return r, nil
}

// OnRotate registers a callback invoked after every rotation.
func (r *Rotating) OnRotate(f func()) {
	r.mu.Lock()
	r.onRotate = f
	r.mu.Unlock()
}

func (r *Rotating) rotate() error {
	c, err := SelfSigned(r.names, r.validity)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.prev = r.cur
	r.cur = &c
	r.rotated = time.Now()
	cb := r.onRotate
	r.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

// Run rotates the certificate until stop is closed.
func (r *Rotating) Run(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.mu.RLock()
			due := time.Since(r.rotated) > r.every
			r.mu.RUnlock()
			if due {
				_ = r.rotate()
			}
		}
	}
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *Rotating) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur, nil
}

// Hashes returns hex-free raw SHA-256 hashes (current first) for serverCertificateHashes.
func (r *Rotating) Hashes() [][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out [][]byte
	for _, c := range []*tls.Certificate{r.cur, r.prev} {
		if c == nil || time.Now().After(c.Leaf.NotAfter) {
			continue
		}
		h := CertHash(c)
		out = append(out, h[:])
	}
	return out
}

// HashesB64 returns the hashes base64-encoded (for JSON).
func (r *Rotating) HashesB64() []string {
	var out []string
	for _, h := range r.Hashes() {
		out = append(out, base64.StdEncoding.EncodeToString(h))
	}
	return out
}

// PinVerifier returns a VerifyPeerCertificate func that accepts only a leaf whose
// SPKI hash equals pin (base64url). Used with InsecureSkipVerify=true, which here
// means "skip PKIX chain building", not "skip verification".
func PinVerifier(pin string) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("tlsutil: no peer certificate")
		}
		got, err := SPKIPin(raw[0])
		if err != nil {
			return err
		}
		if got != pin {
			return fmt.Errorf("tlsutil: certificate pin mismatch")
		}
		cert, _ := x509.ParseCertificate(raw[0])
		if cert != nil && time.Now().After(cert.NotAfter) {
			return errors.New("tlsutil: pinned certificate expired")
		}
		return nil
	}
}
