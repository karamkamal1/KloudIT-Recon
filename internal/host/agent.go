package host

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// Version is set at build time.
var Version = "dev"

// Agent is the host process.
type Agent struct {
	cfg         *Config
	log         *slog.Logger
	caps        *media.Caps
	hostClock   func() uint64
	inj         *input.Injector
	audioSource media.AudioSource

	padsMu  sync.Mutex
	pads    *platform.Gamepads
	padsTry time.Time

	mu        sync.Mutex
	active    *Session
	directKey []byte
	nonces    map[string]int64
	tunnel    transport.BidiStream // control stream to the gateway
	tunnelMu  sync.Mutex
	directRot *tlsutil.Rotating

	pairMu   sync.RWMutex
	pairing  pairing
	reloadMu sync.Mutex // serialises reloadPairing
	cfgMod   time.Time  // config file mtime at the last (re)load, guarded by reloadMu
}

// errRepaired ends a gateway connection after the config file changed to a new pairing.
var errRepaired = errors.New("pairing changed")

// pairing is the gateway identity from the config file. `recon-host pair`
// rewrites the file, and the running agent adopts the change without a restart.
type pairing struct{ Gateway, Pin, HostID, Token, Name string }

func pairingOf(c *Config) pairing {
	return pairing{Gateway: c.Gateway, Pin: c.GatewayPin, HostID: c.HostID, Token: c.Token, Name: c.Name}
}

func (a *Agent) pair() pairing {
	a.pairMu.RLock()
	defer a.pairMu.RUnlock()
	return a.pairing
}

// reloadPairing re-reads the config file if it changed since the last check and
// reports whether the pairing changed.
func (a *Agent) reloadPairing() bool {
	if a.cfg.path == "" {
		return false
	}
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	fi, err := os.Stat(a.cfg.path)
	if err != nil || fi.ModTime().Equal(a.cfgMod) {
		return false
	}
	a.cfgMod = fi.ModTime()
	c, err := LoadConfig(a.cfg.path)
	if err != nil {
		a.log.Warn("config reload failed", "err", err)
		return false
	}
	p := pairingOf(c)
	a.pairMu.Lock()
	changed := p != a.pairing
	a.pairing = p
	a.pairMu.Unlock()
	if changed {
		a.log.Info("pairing changed", "gateway", p.Gateway, "host", p.Name)
	}
	return changed
}

// NewAgent probes ffmpeg and prepares the input backend.
func NewAgent(ctx context.Context, cfg *Config, log *slog.Logger) (*Agent, error) {
	platform.EnableDPIAwareness()
	platform.RaisePriority()
	if cfg.gpuPriority() != media.GPUPriorityOff {
		// REALTIME GPU priority for the encoder needs this privilege.
		if err := media.EnableGPUPriorityPrivilege(); err != nil {
			log.Info("SeIncreaseBasePriorityPrivilege not enabled, encoders get high instead of realtime GPU priority (run elevated)", "err", err)
		}
		media.LogGPUHost(log)
	}
	ff, err := media.FindFFmpeg(cfg.FFmpeg)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found (install FFmpeg 7.1+ or set \"ffmpeg\" in the config): %w", err)
	}
	log.Info("probing ffmpeg and GPU encoders", "ffmpeg", ff)
	caps, err := media.Probe(ctx, ff, log)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range caps.Encoders {
		names = append(names, e.Name)
	}
	log.Info("ffmpeg ready", "version", caps.Version, "encoders", strings.Join(names, ","),
		"ddagrab", caps.Filters["ddagrab"], "gfxcapture", caps.Filters["gfxcapture"])
	be, err := input.NewBackend()
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg: cfg, log: log, caps: caps, hostClock: media.NewClock(),
		inj:         input.NewInjector(be),
		audioSource: media.DefaultAudioSource(),
		nonces:      map[string]int64{},
		pairing:     pairingOf(cfg),
	}
	if cfg.path != "" {
		if fi, err := os.Stat(cfg.path); err == nil {
			a.cfgMod = fi.ModTime()
		}
	}
	return a, nil
}

// clock is the host clock (µs) shared by frame timestamps and pongs.
func (a *Agent) clock() uint64 { return a.hostClock() }

func (a *Agent) monitors() []platform.Monitor {
	mons, err := platform.Monitors()
	if err != nil || len(mons) == 0 {
		return []platform.Monitor{{W: a.cfg.TestWidth, H: a.cfg.TestHeight, Primary: true, Name: "Display", DXGIOutput: -1}}
	}
	if a.cfg.Capture == "test" {
		mons = []platform.Monitor{{W: a.cfg.TestWidth, H: a.cfg.TestHeight, Primary: true, Name: "Test pattern", Hz: 60, DXGIOutput: -1}}
	}
	return mons
}

func (a *Agent) cursorSupported() bool { return runtime.GOOS == "windows" && a.cfg.Capture != "test" }

func (a *Agent) features() []string {
	f := []string{"text", "keyboard", "mouse"}
	if a.cfg.Audio {
		f = append(f, "audio")
	}
	if a.cursorSupported() && !a.cfg.DrawCursor {
		f = append(f, "cursor")
	}
	if a.gamepads() != nil {
		f = append(f, "gamepad")
	}
	return f
}

// gamepads lazily connects to ViGEmBus (retrying at most every 30 s).
func (a *Agent) gamepads() *platform.Gamepads {
	if !a.cfg.Gamepad {
		return nil
	}
	a.padsMu.Lock()
	defer a.padsMu.Unlock()
	if a.pads != nil || time.Since(a.padsTry) < 30*time.Second {
		return a.pads
	}
	a.padsTry = time.Now()
	p, err := platform.OpenGamepads()
	if err != nil {
		if !errors.Is(err, platform.ErrUnsupported) {
			a.log.Info("virtual gamepads unavailable", "err", err)
		}
		return nil
	}
	a.log.Info("ViGEmBus connected: virtual Xbox controllers available")
	a.pads = p
	return p
}

func (a *Agent) setActive(s *Session) {
	a.mu.Lock()
	old := a.active
	a.active = s
	a.mu.Unlock()
	if old != nil && old != s {
		old.log.Info("session replaced by a new connection")
		old.close("Another device connected to this host")
	}
	a.sendTunnel(proto.TunnelMsg{T: "status", Streaming: true, User: s.meta.User})
}

func (a *Agent) clearActive(s *Session) {
	a.mu.Lock()
	cleared := a.active == s
	if cleared {
		a.active = nil
	}
	a.mu.Unlock()
	if cleared {
		a.sendTunnel(proto.TunnelMsg{T: "status", Streaming: false})
	}
}

func (a *Agent) isActive(s *Session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active == s
}

// verifyTicket validates a direct-path ticket issued by the gateway. The ticket
// is bound to the browser origin that requested it.
func (a *Agent) verifyTicket(tok, origin string) (string, error) {
	a.mu.Lock()
	key := a.directKey
	a.mu.Unlock()
	if len(key) == 0 {
		return "", errors.New("not registered with a gateway")
	}
	var t proto.DirectTicket
	if err := auth.VerifyTicket(key, tok, &t); err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if t.HostID != a.pair().HostID || now > t.Exp || t.Nonce == "" {
		return "", errors.New("ticket expired or for another host")
	}
	if !strings.EqualFold(t.Origin, origin) {
		return "", errors.New("ticket was issued to a different origin")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for n, exp := range a.nonces {
		if exp < now {
			delete(a.nonces, n)
		}
	}
	if _, used := a.nonces[t.Nonce]; used {
		return "", errors.New("ticket already used")
	}
	a.nonces[t.Nonce] = t.Exp
	return t.User, nil
}

// Run serves until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.pair().Gateway == "" {
		// Wait instead of exiting: the logon task does not restart an agent that
		// exits, and `recon-host pair` may run after the agent has started.
		a.log.Warn("host is not paired yet: run `recon-host pair <code>`; waiting for the pairing", "config", a.cfg.path)
		for a.pair().Gateway == "" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				a.reloadPairing()
			}
		}
	}
	errc := make(chan error, 2)
	if a.cfg.DirectPort > 0 {
		// Created before the gateway tunnel starts, which advertises its hashes.
		rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
		if err != nil {
			return err
		}
		a.directRot = rot
		go func() { errc <- a.runDirect(ctx, rot) }()
	}
	go func() { errc <- a.runGateway(ctx) }()
	select {
	case <-ctx.Done():
		a.inj.Close()
		a.padsMu.Lock()
		if a.pads != nil {
			a.pads.Close()
		}
		a.padsMu.Unlock()
		return nil
	case err := <-errc:
		return err
	}
}

// ---------------------------------------------------------------------------
// Gateway tunnel

func (a *Agent) tlsFor(p pairing, alpn string) *tls.Config {
	host, _, _ := net.SplitHostPort(p.Gateway)
	return &tls.Config{
		ServerName:            host,
		InsecureSkipVerify:    true, // chain building skipped; the pin below is the verification
		VerifyPeerCertificate: tlsutil.PinVerifier(p.Pin),
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
	}
}

func (a *Agent) runGateway(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.gatewayOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errRepaired) {
			a.log.Info("reconnecting with the new pairing")
			backoff = time.Second
			continue
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("gateway connection lost, reconnecting", "err", err, "in", backoff)
		if a.waitOrRepair(ctx, backoff) {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

// waitOrRepair sleeps for d, returning early (true) if the pairing changes, so a
// re-pair takes effect within seconds instead of after the backoff.
func (a *Agent) waitOrRepair(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return a.reloadPairing()
		case <-tick.C:
			if a.reloadPairing() {
				return true
			}
		}
	}
}

func (a *Agent) sendTunnel(m proto.TunnelMsg) {
	a.tunnelMu.Lock()
	defer a.tunnelMu.Unlock()
	if a.tunnel == nil {
		return
	}
	b, _ := json.Marshal(m)
	_ = a.tunnel.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = proto.WriteMsg(a.tunnel, b)
}

func (a *Agent) directInfo() *proto.DirectInfo {
	if a.cfg.DirectPort <= 0 || a.directRot == nil {
		return nil
	}
	return &proto.DirectInfo{Port: a.cfg.DirectPort, Addr: a.cfg.DirectAddr, Hashes: a.directRot.HashesB64()}
}

func (a *Agent) gatewayOnce(ctx context.Context) error {
	p := a.pair()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := quic.DialAddr(dctx, p.Gateway, a.tlsFor(p, proto.ALPNHostControl), transport.QUICConfig())
	cancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", p.Gateway, err)
	}
	defer conn.CloseWithError(0, "bye")
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	reg := proto.TunnelMsg{
		T: "register", HostID: p.HostID, Token: p.Token, Name: p.Name,
		OS: runtime.GOOS + "/" + runtime.GOARCH, Version: Version,
		MACs: localMACs(outboundLocalAddr(conn.RemoteAddr())), Direct: a.directInfo(),
	}
	for _, e := range a.caps.Encoders {
		reg.Encoders = append(reg.Encoders, e.Name)
	}
	for _, m := range a.monitors() {
		reg.Monitors = append(reg.Monitors, proto.MonitorInfo{Index: m.Index, Name: m.Name, Width: m.W, Height: m.H, Primary: m.Primary, Hz: m.Hz})
	}
	b, _ := json.Marshal(reg)
	if err := proto.WriteMsg(st, b); err != nil {
		return err
	}
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	b, err = proto.ReadMsg(st, proto.MaxControlMsg)
	if err != nil {
		return fmt.Errorf("registration: %w", err)
	}
	var resp proto.TunnelMsg
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	if resp.T != "registered" {
		return fmt.Errorf("gateway rejected registration: %s", resp.Error)
	}
	key, err := base64.StdEncoding.DecodeString(resp.DirectKey)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.directKey = key
	a.mu.Unlock()
	a.tunnelMu.Lock()
	a.tunnel = &quicStreamAdapter{st}
	a.tunnelMu.Unlock()
	defer func() {
		a.tunnelMu.Lock()
		a.tunnel = nil
		a.tunnelMu.Unlock()
	}()
	a.log.Info("connected to gateway", "gateway", p.Gateway, "host", p.Name)
	// `recon-host pair` may point this PC at another host entry or gateway
	// while connected: notice it and reconnect.
	var repaired atomic.Bool
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				if a.reloadPairing() {
					repaired.Store(true)
					conn.CloseWithError(0, "re-paired")
					return
				}
			}
		}
	}()
	a.mu.Lock()
	streaming := a.active != nil
	a.mu.Unlock()
	if streaming {
		a.sendTunnel(proto.TunnelMsg{T: "status", Streaming: true})
	}

	for {
		_ = st.SetReadDeadline(time.Now().Add(45 * time.Second))
		b, err := proto.ReadMsg(st, proto.MaxControlMsg)
		if err != nil {
			if repaired.Load() {
				return errRepaired
			}
			return err
		}
		var m proto.TunnelMsg
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		switch m.T {
		case "ping":
			a.sendTunnel(proto.TunnelMsg{T: "pong"})
		case "open":
			go a.openData(ctx, m)
		}
	}
}

type quicStreamAdapter struct{ s *quic.Stream }

func (q *quicStreamAdapter) Read(p []byte) (int, error)         { return q.s.Read(p) }
func (q *quicStreamAdapter) Write(p []byte) (int, error)        { return q.s.Write(p) }
func (q *quicStreamAdapter) Close() error                       { return q.s.Close() }
func (q *quicStreamAdapter) CancelRead()                        { q.s.CancelRead(0) }
func (q *quicStreamAdapter) CancelWrite()                       { q.s.CancelWrite(0) }
func (q *quicStreamAdapter) SetReadDeadline(t time.Time) error  { return q.s.SetReadDeadline(t) }
func (q *quicStreamAdapter) SetWriteDeadline(t time.Time) error { return q.s.SetWriteDeadline(t) }

// openData dials a dedicated QUIC connection for one relayed session.
func (a *Agent) openData(ctx context.Context, m proto.TunnelMsg) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p := a.pair()
	conn, err := quic.DialAddr(dctx, p.Gateway, a.tlsFor(p, proto.ALPNHostData), transport.QUICConfig(transport.WithCongestion(a.cfg.congestion())))
	if err != nil {
		a.log.Warn("data connection failed", "err", err)
		return
	}
	st, err := conn.OpenStreamSync(dctx)
	if err != nil {
		conn.CloseWithError(1, "")
		return
	}
	b, _ := json.Marshal(proto.DataHello{HostID: p.HostID, SID: m.SID, Nonce: m.Nonce})
	if err := proto.WriteMsg(st, b); err != nil {
		conn.CloseWithError(1, "")
		return
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := proto.ReadMsg(st, 1024); err != nil {
		conn.CloseWithError(1, "")
		return
	}
	st.Close()
	a.HandleConn(transport.FromQUIC(conn), SessionMeta{Path: "relay", User: m.User})
}

// outboundLocalAddr returns the source address the OS routes from to reach
// remote. (quic.DialAddr binds 0.0.0.0, so the connection's LocalAddr is
// unspecified.) Connecting a UDP socket sends nothing.
func outboundLocalAddr(remote net.Addr) net.Addr {
	c, err := net.Dial("udp", remote.String())
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr()
}

// localMACs returns MAC addresses for Wake-on-LAN, the interface used to reach
// the gateway first.
func localMACs(local net.Addr) []string {
	var localIP net.IP
	if ua, ok := local.(*net.UDPAddr); ok {
		localIP = ua.IP
	}
	ifaces, _ := net.Interfaces()
	var first, rest []string
	for _, ifc := range ifaces {
		if len(ifc.HardwareAddr) != 6 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		mac := ifc.HardwareAddr.String()
		addrs, _ := ifc.Addrs()
		match := false
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && localIP != nil && ipn.IP.Equal(localIP) {
				match = true
			}
		}
		if match {
			first = append(first, mac)
		} else if ifc.Flags&net.FlagUp != 0 {
			rest = append(rest, mac)
		}
	}
	out := append(first, rest...)
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// ---------------------------------------------------------------------------
// Direct path: the browser connects straight to the host over WebTransport,
// skipping the gateway hop. Authorised by gateway-signed one-time tickets;
// the certificate is pinned by hash (no CA needed).

func (a *Agent) runDirect(ctx context.Context, rot *tlsutil.Rotating) error {
	rot.OnRotate(func() { a.sendTunnel(proto.TunnelMsg{T: "direct", Direct: a.directInfo()}) })
	go rot.Run(ctx.Done())

	sem := make(chan struct{}, 4) // bound concurrent unauthenticated handshakes
	mux := http.NewServeMux()
	srv := &webtransport.Server{
		H3: &http3.Server{
			Addr:       fmt.Sprintf(":%d", a.cfg.DirectPort),
			TLSConfig:  http3.ConfigureTLSConfig(&tls.Config{GetCertificate: rot.GetCertificate, MinVersion: tls.VersionTLS13}),
			Handler:    mux,
			QUICConfig: transport.QUICConfig(transport.WithCongestion(a.cfg.congestion())),
			// Lets each session reach its connection's congestion controller.
			ConnContext: transport.WithQUICConn,
		},
		// Any origin may open the session, but the first control message must
		// carry a gateway-signed ticket bound to that same origin.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		sess, err := srv.Upgrade(w, r)
		<-sem
		if err != nil {
			a.log.Debug("direct upgrade failed", "err", err)
			return
		}
		c := transport.FromWebTransportOver(sess, transport.QUICConnFromContext(r.Context()))
		a.HandleConn(c, SessionMeta{Path: "direct", RequireTicket: true, Origin: r.Header.Get("Origin")})
	})
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	a.log.Info("direct WebTransport endpoint listening", "port", a.cfg.DirectPort, "congestion", a.cfg.congestion())
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		a.log.Warn("direct endpoint stopped (relay still works)", "err", err)
	}
	<-ctx.Done()
	return nil
}
