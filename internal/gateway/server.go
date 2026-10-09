// Package gateway is the web gateway: it serves the browser client, handles
// authentication, keeps the host agents' tunnels and relays media sessions.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// Version is set at build time.
var Version = "dev"

// Config configures the gateway.
type Config struct {
	Listen      string        // TCP (HTTPS/WSS) and UDP (HTTP/3, WebTransport, host tunnels) address
	DataDir     string        // state, certificates, audit log
	Names       []string      // extra DNS names / IPs for the generated certificate
	CertFile    string        // optional: use this certificate instead of the private CA
	KeyFile     string        //
	PublicAddr  string        // address host agents use to reach the gateway (pairing codes)
	SessionIdle time.Duration // log out after this much inactivity
	SessionMax  time.Duration // absolute session lifetime
	TrustProxy  []string      // CIDRs of reverse proxies whose X-Forwarded-For is trusted
	RelayPorts  string        // UDP relay ports, e.g. "8444-8459" ("" = no UDP relay, QUIC splice only)
	Web         fs.FS         // static web client
}

// Server is the gateway.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store *Store
	audit *Audit

	ca         *tlsutil.CA
	webCert    *tls.Certificate
	wtRot      *tlsutil.Rotating // nil when a user certificate is used
	tunnelCert tls.Certificate
	TunnelPin  string

	hosts   *registry
	relay   *udpRelay // nil: no UDP relay ports configured
	wt      *webtransport.Server
	mux     *http.ServeMux
	static  map[string]staticFile
	proxies []*net.IPNet

	ticketMu sync.Mutex
	tickets  map[string]*relayTicket
	pending  map[string]*pendingLogin
	streams  userStreams // users' streams through the gateway (revoke.go)

	loginIP   *limiter
	apiIP     *limiter
	limitLog  *limiter   // login_ratelimited audit entries per client
	lockouts  *lockout   // per user and client (rateKey)
	totpLocks *lockout   // per user, from every address: wrong 2FA codes
	totpMu    sync.Mutex // one 2FA code check at a time (checkTOTP)
	hashSem   chan struct{}
	setupTok  string
	setupMu   sync.Mutex
	listenErr chan error
}

type relayTicket struct {
	user   string
	hostID string
	issued time.Time
	exp    time.Time
}

type pendingLogin struct {
	user  string
	exp   time.Time
	tries int // 2FA codes checked against it (under ticketMu)
}

type staticFile struct {
	data  []byte
	etag  string
	ctype string
}

// New prepares a gateway (certificates, state) without listening yet.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = ":8443"
	}
	if cfg.SessionIdle == 0 {
		cfg.SessionIdle = 72 * time.Hour
	}
	if cfg.SessionMax == 0 {
		cfg.SessionMax = 30 * 24 * time.Hour
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	audit, err := OpenAudit(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	s := &Server{
		cfg: cfg, log: log, store: store, audit: audit,
		hosts:     newRegistry(),
		tickets:   map[string]*relayTicket{},
		pending:   map[string]*pendingLogin{},
		loginIP:   newLimiter(10, 5),
		apiIP:     newLimiter(600, 120),
		limitLog:  newLimiter(1, 1),
		lockouts:  newLockout(),
		totpLocks: newLockout(),
		hashSem:   make(chan struct{}, 2),
	}
	ports, err := parseRelayPorts(cfg.RelayPorts)
	if err != nil {
		return nil, err
	}
	if len(ports) > 0 {
		s.relay = newUDPRelay(log, nil, ports)
	}
	if s.proxies, err = parseTrustProxy(cfg.TrustProxy); err != nil {
		return nil, err
	}
	if err := s.setupTLS(); err != nil {
		return nil, err
	}
	if err := s.loadStatic(); err != nil {
		return nil, err
	}
	s.routes()
	if store.UserCount() == 0 {
		// Keep the token across restarts until it is used, so the one the
		// installer printed stays valid.
		tokPath := filepath.Join(cfg.DataDir, "setup-token.txt")
		if b, err := os.ReadFile(tokPath); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
			s.setupTok = strings.TrimSpace(string(b))
		} else {
			s.setupTok = auth.RandomToken(18)
			_ = os.WriteFile(tokPath, []byte(s.setupTok+"\n"), 0o600)
		}
		log.Warn("FIRST RUN: open the web UI and create the admin account with this setup token",
			"setup_token", s.setupTok, "also_saved_to", tokPath)
	}
	return s, nil
}

func (s *Server) setupTLS() error {
	dir := s.cfg.DataDir
	tc, err := tlsutil.LoadOrCreateKeyCert(filepath.Join(dir, "tunnel.crt"), filepath.Join(dir, "tunnel.key"), []string{"recon-gateway"}, 10*365*24*time.Hour)
	if err != nil {
		return fmt.Errorf("tunnel identity: %w", err)
	}
	s.tunnelCert = tc
	if s.TunnelPin, err = tlsutil.SPKIPin(tc.Certificate[0]); err != nil {
		return err
	}
	if s.cfg.CertFile != "" {
		c, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
		if err != nil {
			return fmt.Errorf("loading certificate: %w", err)
		}
		s.webCert = &c
		return nil
	}
	ca, err := tlsutil.LoadOrCreateCA(dir, "KloudIT Recon Local CA")
	if err != nil {
		return fmt.Errorf("CA: %w", err)
	}
	s.ca = ca
	names := s.certNames()
	c, err := s.loadOrIssueWebCert(names)
	if err != nil {
		return err
	}
	s.webCert = c
	// WebTransport over HTTP/3 uses short-lived certificates so browsers can
	// pin them by hash without trusting the CA.
	rot, err := tlsutil.NewRotating(names, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		return err
	}
	s.wtRot = rot
	return nil
}

func (s *Server) certNames() []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil {
		names = append(names, h, h+".local", h+".lan")
	}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				names = append(names, ipn.IP.String())
			}
		}
	}
	names = append(names, s.cfg.Names...)
	slices.Sort(names)
	return slices.Compact(names)
}

// loadOrIssueWebCert reuses the HTTPS certificate unless the set of names
// changed or it is close to expiry (so users only click through once).
func (s *Server) loadOrIssueWebCert(names []string) (*tls.Certificate, error) {
	certPath := filepath.Join(s.cfg.DataDir, "web.crt")
	keyPath := filepath.Join(s.cfg.DataDir, "web.key")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && c.Leaf != nil {
		have := append(append([]string{}, c.Leaf.DNSNames...), ipStrings(c.Leaf.IPAddresses)...)
		covered := true
		for _, n := range names {
			if !slices.Contains(have, n) && !slices.Contains(have, net.ParseIP(n).String()) {
				covered = false
				break
			}
		}
		if covered && time.Until(c.Leaf.NotAfter) > 30*24*time.Hour {
			return &c, nil
		}
	}
	c, err := s.ca.Issue(names, 825*24*time.Hour)
	if err != nil {
		return nil, err
	}
	if err := tlsutil.SaveKeyCert(certPath, keyPath, c); err != nil {
		return nil, err
	}
	return &c, nil
}

func ipStrings(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func (s *Server) loadStatic() error {
	s.static = map[string]staticFile{}
	if s.cfg.Web == nil {
		return errors.New("no web client files")
	}
	return fs.WalkDir(s.cfg.Web, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.cfg.Web, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		ct := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js", ".mjs":
			ct = "text/javascript; charset=utf-8"
		case ".webmanifest":
			ct = "application/manifest+json"
		case ".svg":
			ct = "image/svg+xml"
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		s.static["/"+p] = staticFile{data: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ct}
		return nil
	})
}

// Run listens on TCP and UDP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	tcpLn, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.Listen)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	_ = udpConn.SetReadBuffer(8 << 20)
	_ = udpConn.SetWriteBuffer(8 << 20)
	if s.relay != nil {
		s.relay.ip = udpAddr.IP // allocations listen where the main port does
		defer s.relay.closeAll()
	}

	httpSrv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{*s.webCert},
			NextProtos:   []string{"h2", "http/1.1"},
		},
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}

	h3 := &http3.Server{Handler: s.mux, QUICConfig: transport.QUICConfig()}
	s.wt = &webtransport.Server{H3: h3, CheckOrigin: s.sameOrigin}

	h3TLS := http3.ConfigureTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13})
	if s.wtRot != nil {
		h3TLS.GetCertificate = s.wtRot.GetCertificate
		stop := make(chan struct{})
		defer close(stop)
		go s.wtRot.Run(stop)
	} else {
		h3TLS.Certificates = []tls.Certificate{*s.webCert}
	}
	tunnelTLS := func(alpn string) *tls.Config {
		return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.tunnelCert}, NextProtos: []string{alpn}}
	}
	quicTLS := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, p := range chi.SupportedProtos {
				if p == proto.ALPNHostControl || p == proto.ALPNHostData {
					return tunnelTLS(p), nil
				}
			}
			return h3TLS, nil
		},
	}
	ln, err := quic.ListenEarly(udpConn, quicTLS, transport.QUICConfig())
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	go func() { errc <- httpSrv.ServeTLS(tcpLn, "", "") }()
	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				errc <- err
				return
			}
			switch conn.ConnectionState().TLS.NegotiatedProtocol {
			case proto.ALPNHostControl:
				go s.handleHostControl(conn)
			case proto.ALPNHostData:
				go s.handleHostData(conn)
			default:
				go func() { _ = s.wt.ServeQUICConn(conn) }()
			}
		}
	}()
	go s.janitor(ctx)
	relayPorts := "off"
	if s.relay != nil {
		relayPorts = s.cfg.RelayPorts
	}
	s.log.Info("gateway listening", "addr", s.cfg.Listen, "tcp", "https/wss", "udp", "http3/webtransport/host-tunnel",
		"udp_relay_ports", relayPorts, "tunnel_pin", s.TunnelPin)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
	_ = s.wt.Close()
	_ = ln.Close()
	_ = udpConn.Close()
	s.store.Flush()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// janitor expires sessions, tickets and pending logins.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		s.ticketMu.Lock()
		for k, v := range s.tickets {
			if now.After(v.exp) {
				delete(s.tickets, k)
			}
		}
		for k, v := range s.pending {
			if now.After(v.exp) {
				delete(s.pending, k)
			}
		}
		s.ticketMu.Unlock()
		s.store.UpdateLazy(func(st *state) {
			for k, ls := range st.Sessions {
				if now.Sub(ls.LastSeen) > s.cfg.SessionIdle || now.Sub(ls.Created) > s.cfg.SessionMax {
					delete(st.Sessions, k)
				}
			}
		})
	}
}

// sameOrigin accepts requests whose Origin matches the Host they were sent to.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	return strings.EqualFold(o, "https://"+r.Host)
}

// parseTrustProxy parses -trust-proxy values: CIDRs, or a proxy's own
// address (as /32 or /128). An entry it cannot read stops the gateway: one
// silently dropped would key rate limits and lockouts on the proxy's address,
// shared by every client behind it.
func parseTrustProxy(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range list {
		c = strings.TrimSpace(c)
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(c)
		if ip == nil {
			return nil, fmt.Errorf("trust-proxy %q: want a proxy's address or a CIDR like 10.0.0.0/8", c)
		}
		bits := 128
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 32
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// clientIP returns the caller's IP, honouring X-Forwarded-For only from trusted proxies.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || len(s.proxies) == 0 {
		return host
	}
	trusted := func(ip net.IP) bool {
		for _, n := range s.proxies {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(ip) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := net.ParseIP(strings.TrimSpace(parts[i]))
		if p != nil && !trusted(p) {
			return p.String()
		}
	}
	return host
}
