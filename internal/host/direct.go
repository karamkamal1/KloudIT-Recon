package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// Direct path: the browser connects straight to the host over WebTransport,
// skipping the gateway hop. Authorised by gateway-signed one-time tickets;
// the certificate is pinned by hash (no CA needed).
//
// The endpoint listens on the direct port of every address of the PC a
// browser could reach (directAddrs: the LAN, IPv6, Tailscale, ...), one socket
// per address, and the gateway offers the browser all of them, the one on the
// browser's own network first (VENDOR_NOTES "Second hardware run: direct path
// on the LAN"). One socket per address, not one on the wildcard address: a
// socket bound to an address answers from it, out of the interface that owns
// it (Windows sends with the strong host model), while quic-go on Windows has
// no packet information to answer a wildcard socket's packet from the address
// it arrived at: the answer leaves from whatever address the route back to the
// browser picks. On a PC whose VPN or Tailscale routes cover the LAN (a subnet
// route the PC accepts), that is the tunnel's address, and the browser drops
// an answer from an address it did not send to: the handshake times out. Where
// no address can be bound (none found, or a program holds the port on all of
// them) the endpoint falls back to the wildcard socket of before.
//
// Every handshake that fails is logged (directHandshake), with what the
// failure says about the cause: none at all means the browser's packets never
// reached the PC.

// directRetry is how often the direct endpoint tries to bind its port again.
var directRetry = 30 * time.Second

// directRescan is how often the endpoint looks for addresses that came or
// went (a DHCP lease, a VPN or Tailscale going up or down).
var directRescan = 30 * time.Second

// maxDirectAddrs bounds the addresses announced (each is one handshake the
// browser starts) and bound.
const maxDirectAddrs = 8

// ifaceInfo is one network interface and its addresses.
type ifaceInfo struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []netip.Prefix
}

// listInterfaces returns the system's interfaces (tests replace it).
var listInterfaces = func() ([]ifaceInfo, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceInfo, 0, len(ifs))
	for _, ifc := range ifs {
		ii := ifaceInfo{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, Loopback: ifc.Flags&net.FlagLoopback != 0}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			bits, _ := ipn.Mask.Size()
			ii.Addrs = append(ii.Addrs, netip.PrefixFrom(ip.Unmap(), bits))
		}
		out = append(out, ii)
	}
	return out, nil
}

var (
	cgnatPrefix   = netip.MustParsePrefix("100.64.0.0/10")       // Tailscale's IPv4 range (RFC 6598 shared address space)
	tsULAPrefix   = netip.MustParsePrefix("fd7a:115c:a1e0::/48") // Tailscale's IPv6 range
	teredoPrefix  = netip.MustParsePrefix("2001::/32")           // Teredo: not reachable from a browser on the LAN
	tailscaleName = regexp.MustCompile(`(?i)tailscale`)
	vpnName       = regexp.MustCompile(`(?i)wireguard|^wg[0-9]|zerotier|^zt|openvpn|tap-windows|wintun|nordlynx|mullvad|proton|^tun[0-9]|^utun`)
	virtualName   = regexp.MustCompile(`(?i)vethernet|hyper-v|\bwsl|virtualbox|vboxnet|vmware|vmnet|docker|^br-|virbr|^veth|^lxc|^lxd`)
)

// directKind is how a browser could reach ip on the interface named iface
// (proto.DirectKind*), "" for an address it cannot: loopback, link-local (a
// URL cannot name its zone), multicast, Teredo.
func directKind(ip netip.Addr, iface string) string {
	switch {
	case !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast() || teredoPrefix.Contains(ip):
		return ""
	case cgnatPrefix.Contains(ip) || tsULAPrefix.Contains(ip) || tailscaleName.MatchString(iface):
		return proto.DirectKindTailscale
	case vpnName.MatchString(iface):
		return proto.DirectKindVPN
	case virtualName.MatchString(iface):
		return proto.DirectKindVirtual
	case ip.Is4() && ip.IsPrivate():
		return proto.DirectKindLAN
	case ip.Is4() && ip.IsGlobalUnicast():
		return proto.DirectKindPublic
	case ip.Is6() && ip.IsGlobalUnicast(): // also unique local (fc00::/7)
		return proto.DirectKindIPv6
	}
	return ""
}

// directKindRank orders the PC's addresses (and, at the gateway, the ones
// that have no better rank): the LAN first, Tailscale and VPNs after IPv6
// (their own address is reachable only from their network), virtual switches
// last.
func directKindRank(kind string) int {
	switch kind {
	case proto.DirectKindLAN, proto.DirectKindPublic:
		return 0
	case proto.DirectKindIPv6:
		return 1
	case proto.DirectKindTailscale:
		return 2
	case proto.DirectKindVPN:
		return 3
	case proto.DirectKindVirtual:
		return 4
	}
	return 5
}

// directAddrs picks the PC's addresses the direct endpoint listens on and the
// gateway offers: up, not loopback, an address a browser could reach
// (directKind), at most two IPv6 addresses per interface (Windows lists a
// temporary address next to each stable one), ordered by kind (directKindRank)
// with route (the address the PC reaches the gateway from) first among its
// kind.
func directAddrs(ifs []ifaceInfo, route netip.Addr) []proto.DirectAddr {
	type cand struct {
		a    proto.DirectAddr
		rank int
		n    int
	}
	var cs []cand
	seen := map[netip.Addr]bool{}
	for _, ifc := range ifs {
		if !ifc.Up || ifc.Loopback {
			continue
		}
		v6 := 0
		for _, p := range ifc.Addrs {
			ip := p.Addr().Unmap()
			kind := directKind(ip, ifc.Name)
			if kind == "" || seen[ip] {
				continue
			}
			if ip.Is6() {
				if v6 >= 2 {
					continue
				}
				v6++
			}
			seen[ip] = true
			a := proto.DirectAddr{IP: ip.String(), Bits: p.Bits(), Kind: kind, Iface: ifc.Name, Route: route.IsValid() && ip == route.Unmap()}
			rank := directKindRank(kind) * 2
			if !a.Route {
				rank++
			}
			cs = append(cs, cand{a, rank, len(cs)})
		}
	}
	slices.SortStableFunc(cs, func(x, y cand) int { return x.rank - y.rank })
	out := make([]proto.DirectAddr, 0, len(cs))
	for _, c := range cs {
		if len(out) == maxDirectAddrs {
			break
		}
		out = append(out, c.a)
	}
	return out
}

// directOverrides splits host config directAddr: one address or name, or
// several separated by commas or spaces, offered first in that order.
func directOverrides(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\t' })
	out := f[:0]
	for _, x := range f {
		x = strings.Trim(x, "[]")
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// directAddrText is addrs for a log line.
func directAddrText(addrs []proto.DirectAddr) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		note := strings.Trim(a.Iface+", "+a.Kind, ", ")
		if a.Route {
			note += ", route to the gateway"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", a.IP, note))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// directEndpoint is the direct path's WebTransport server and its sockets.
type directEndpoint struct {
	a   *Agent
	srv *webtransport.Server

	mu    sync.Mutex
	socks map[netip.AddrPort]*directSock // by bound address (the zero AddrPort: the wildcard socket)
	addrs []proto.DirectAddr             // the PC's addresses being announced (bound)
	route netip.Addr                     // the PC's address towards the gateway at the last scan
	state string                         // the sockets for the log ("*": the wildcard socket alone)

	hsLog directHandshakeLog
}

// directSock is one bound socket and its QUIC listener.
type directSock struct {
	conn *net.UDPConn
	tr   *quic.Transport
	ln   *quic.EarlyListener
	done chan struct{} // closed when its accept loop ended
}

// runDirect runs the direct endpoint until ctx ends: it binds the direct port
// on the PC's addresses, again every directRescan when they change (and every
// directRetry while it holds none), and tells the gateway what it holds (a
// "direct" tunnel message when that changes).
func (a *Agent) runDirect(ctx context.Context, rot *tlsutil.Rotating) error {
	sem := make(chan struct{}, 4) // bound concurrent unauthenticated handshakes
	mux := http.NewServeMux()
	d := &directEndpoint{a: a, socks: map[netip.AddrPort]*directSock{}}
	d.srv = &webtransport.Server{
		H3: &http3.Server{
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
		sess, err := d.srv.Upgrade(w, r)
		<-sem
		if err != nil {
			a.log.Debug("direct upgrade failed", "err", err)
			return
		}
		c := transport.FromWebTransportOver(sess, transport.QUICConnFromContext(r.Context()))
		a.HandleConn(c, SessionMeta{Path: "direct", RequireTicket: true, Origin: r.Header.Get("Origin")})
	})
	a.mu.Lock()
	a.direct = d
	a.mu.Unlock()
	tlsConf := http3.ConfigureTLSConfig(&tls.Config{GetCertificate: rot.GetCertificate, MinVersion: tls.VersionTLS13})
	failing := false
	for {
		held, changed, err := d.scan(ctx, tlsConf)
		switch {
		case held == 0 && !failing:
			// Sunshine and Apollo use UDP 47998-48000 and 48010 while they stream.
			a.log.Warn("direct endpoint unavailable: cannot bind its UDP port, retrying (streams go through the gateway; "+
				"if another program holds the port, set another directPort in host.json)", "port", a.cfg.DirectPort, "err", err)
			failing = true
		case held > 0 && (failing || changed):
			if failing {
				a.log.Info("direct endpoint port is free again", "port", a.cfg.DirectPort)
				failing = false
			}
			d.mu.Lock()
			addrs := d.state
			d.mu.Unlock()
			if addrs == "*" {
				addrs = "every address (none of its own found to listen on: the gateway offers its tunnel's peer address)"
			}
			// The addresses the gateway offers browsers, in this order after
			// the one on the browser's own network and host config directAddr.
			a.log.Info("direct WebTransport endpoint listening", "port", a.cfg.DirectPort, "congestion", a.cfg.congestion(), "addrs", addrs)
		}
		// The gateway learns of the endpoint coming up or going, and of
		// the addresses it offers browsers changing.
		if up := held > 0; a.directUp.Swap(up) != up || (up && changed) {
			a.sendTunnel(proto.TunnelMsg{T: "direct", Direct: a.directInfo(), Relay: a.relayInfo()})
		}
		wait := directRescan
		if held == 0 {
			wait = directRetry
		}
		select {
		case <-ctx.Done():
			d.closeAll()
			a.setDirectUp(false)
			return nil
		case <-time.After(wait):
		}
	}
}

// scan binds the direct port on the PC's addresses that have no socket yet
// and closes the sockets of addresses that are gone. It returns how many
// sockets it holds, whether what it announces changed, and the last bind
// error.
func (d *directEndpoint) scan(ctx context.Context, tlsConf *tls.Config) (held int, changed bool, lastErr error) {
	a := d.a
	var route netip.Addr
	if gw := a.pair().Gateway; gw != "" {
		if ua, err := net.ResolveUDPAddr("udp", gw); err == nil {
			if la, ok := outboundLocalAddr(ua).(*net.UDPAddr); ok {
				route, _ = netip.AddrFromSlice(la.IP)
				route = route.Unmap()
			}
		}
	}
	ifs, err := listInterfaces()
	if err != nil {
		a.log.Debug("direct path: listing the network interfaces", "err", err)
	}
	want := directAddrs(ifs, route)
	// Host config directAddr entries that are IP addresses of this PC are
	// listened on too (a loopback address in tests, an address the
	// interfaces did not list); the others (a name, a port forward's
	// public address) are only offered, and their bind fails quietly.
	var extra []netip.Addr
	for _, o := range directOverrides(a.cfg.DirectAddr) {
		if ip, err := netip.ParseAddr(o); err == nil && !slices.ContainsFunc(want, func(w proto.DirectAddr) bool { return w.IP == ip.Unmap().String() }) {
			extra = append(extra, ip.Unmap())
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	wild := netip.AddrPort{}
	if s := d.socks[wild]; s != nil && len(want)+len(extra) > 0 {
		// Addresses of its own to listen on (again): the wildcard socket
		// holds the port on all of them, so it goes first.
		s.close()
		delete(d.socks, wild)
	}
	keep := map[netip.AddrPort]bool{}
	bind := func(ap netip.AddrPort) bool {
		if d.socks[ap] != nil {
			keep[ap] = true
			return true
		}
		s, err := d.listen(ctx, ap, tlsConf)
		if err != nil {
			lastErr = err
			a.log.Debug("direct path: cannot listen on this address", "addr", ap.String(), "err", err)
			return false
		}
		d.socks[ap] = s
		keep[ap] = true
		return true
	}
	port := uint16(a.cfg.DirectPort)
	var bound []proto.DirectAddr
	for _, w := range want {
		if ip, err := netip.ParseAddr(w.IP); err == nil && bind(netip.AddrPortFrom(ip, port)) {
			bound = append(bound, w)
		}
	}
	for _, ip := range extra {
		bind(netip.AddrPortFrom(ip, port))
	}
	if len(keep) == 0 {
		// No address of its own to listen on (or none free): the wildcard
		// socket, as before the per-address sockets. The gateway then
		// offers its tunnel's peer address (and directAddr).
		bind(wild)
	}
	for ap, s := range d.socks {
		if !keep[ap] {
			s.close()
			delete(d.socks, ap)
		}
	}
	state := directAddrText(bound)
	if d.socks[wild] != nil {
		state = "*"
	}
	changed = state != d.state || route != d.route
	d.addrs, d.route, d.state = bound, route, state
	return len(d.socks), changed, lastErr
}

// listen binds one socket (ap: the zero AddrPort for the wildcard address on
// the direct port) and serves WebTransport on it.
func (d *directEndpoint) listen(ctx context.Context, ap netip.AddrPort, tlsConf *tls.Config) (*directSock, error) {
	la := &net.UDPAddr{Port: d.a.cfg.DirectPort}
	if ap.IsValid() {
		la = net.UDPAddrFromAddrPort(ap)
	}
	conn, err := net.ListenUDP("udp", la)
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(8 << 20)
	_ = conn.SetWriteBuffer(8 << 20)
	s := &directSock{conn: conn, done: make(chan struct{})}
	local := ap.Addr().String()
	if !ap.IsValid() {
		local = "*"
	}
	s.tr = &quic.Transport{Conn: conn, ConnContext: func(cctx context.Context, ci *quic.ClientInfo) (context.Context, error) {
		return d.hsLog.started(cctx, d.a, ci.RemoteAddr, local), nil
	}}
	quicConf := transport.QUICConfig(transport.WithCongestion(d.a.cfg.congestion()))
	ln, err := s.tr.ListenEarly(tlsConf, quicConf)
	if err != nil {
		s.tr.Close()
		conn.Close()
		return nil, err
	}
	s.ln = ln
	go func() {
		defer close(s.done)
		for {
			c, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			d.hsLog.accepted(c)
			go func() {
				if err := d.srv.ServeQUICConn(c); err != nil {
					d.a.log.Debug("direct connection", "err", err)
				}
			}()
		}
	}()
	return s, nil
}

func (s *directSock) close() {
	s.ln.Close()
	s.tr.Close()
	s.conn.Close()
}

func (d *directEndpoint) closeAll() {
	d.srv.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	for ap, s := range d.socks {
		s.close()
		delete(d.socks, ap)
	}
	d.addrs, d.state = nil, ""
}

// announced returns the PC's addresses the endpoint listens on (nil without
// sockets of its own).
func (d *directEndpoint) announced() []proto.DirectAddr {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

func (a *Agent) directInfo() *proto.DirectInfo {
	if a.cfg.DirectPort <= 0 || a.directRot == nil || !a.directUp.Load() {
		return nil
	}
	info := &proto.DirectInfo{Port: a.cfg.DirectPort, Hashes: a.directRot.HashesB64()}
	ov := directOverrides(a.cfg.DirectAddr)
	if len(ov) > 0 {
		// The first one for gateways before DirectInfo.Addrs (they offer it alone).
		info.Addr = ov[0]
		for _, o := range ov[1:] {
			info.Addrs = append(info.Addrs, proto.DirectAddr{IP: o, Kind: proto.DirectKindOverride})
		}
	}
	a.mu.Lock()
	d := a.direct
	a.mu.Unlock()
	for _, x := range d.announced() {
		if !slices.Contains(ov, x.IP) {
			info.Addrs = append(info.Addrs, x)
		}
	}
	return info
}

// setDirectUp records whether the direct endpoint holds its port and tells
// the gateway when that changes, so browsers are not sent to a closed port.
func (a *Agent) setDirectUp(up bool) {
	if a.directUp.Swap(up) != up {
		a.sendTunnel(proto.TunnelMsg{T: "direct", Direct: a.directInfo(), Relay: a.relayInfo()})
	}
}

// ---------------------------------------------------------------------------
// Handshake diagnostics

// directHandshake is one browser connection to the direct endpoint, from its
// first packet on.
type directHandshake struct {
	remote, local string
	start         time.Time
	done          atomic.Bool // the handshake completed
}

type directHandshakeKey struct{}

// directHandshakeLog logs the direct endpoint's handshakes that fail: the
// first failure of a kind from an address in directHandshakeQuiet at Info,
// the rest at Debug.
type directHandshakeLog struct {
	mu   sync.Mutex
	last map[string]time.Time // remote IP and why -> when it was last logged at Info
}

const directHandshakeQuiet = 5 * time.Minute

// started is the quic.Transport ConnContext of a direct socket: the first
// packet of a connection from remote arrived on local.
func (l *directHandshakeLog) started(ctx context.Context, a *Agent, remote net.Addr, local string) context.Context {
	hs := &directHandshake{remote: remote.String(), local: local, start: time.Now()}
	context.AfterFunc(ctx, func() {
		if !hs.done.Load() {
			l.failed(a, hs, context.Cause(ctx))
		}
	})
	return context.WithValue(ctx, directHandshakeKey{}, hs)
}

// accepted watches an accepted connection's handshake (an early connection:
// the PC has sent its part, the browser has yet to accept the certificate).
func (l *directHandshakeLog) accepted(c *quic.Conn) {
	hs, _ := c.Context().Value(directHandshakeKey{}).(*directHandshake)
	if hs == nil {
		return
	}
	go func() {
		select {
		case <-c.HandshakeComplete():
			hs.done.Store(true)
		case <-c.Context().Done():
		}
	}()
}

// failed logs a handshake that did not complete, with what its end says
// about the cause.
func (l *directHandshakeLog) failed(a *Agent, hs *directHandshake, cause error) {
	why, expected := directHandshakeWhy(cause, time.Since(hs.start))
	ip, _, _ := net.SplitHostPort(hs.remote)
	level := a.log.Debug
	if !expected {
		l.mu.Lock()
		if l.last == nil {
			l.last = map[string]time.Time{}
		}
		if key := ip + " " + why; time.Since(l.last[key]) > directHandshakeQuiet {
			l.last[key] = time.Now()
			level = a.log.Info
		}
		if len(l.last) > 256 {
			clear(l.last)
		}
		l.mu.Unlock()
	}
	level("direct path: a browser's connection failed in its handshake", "from", hs.remote, "to", hs.local,
		"after_ms", time.Since(hs.start).Milliseconds(), "why", why, "err", cause)
}

// directAnswersLost explains a handshake whose answers never reached the
// browser (its packets reached the PC: the connection exists).
const directAnswersLost = "the PC's answers did not reach it. They leave from the address the browser sent to, so a route " +
	"back that does not lead to the browser (a VPN, or a Tailscale subnet route the PC accepts that covers the LAN) or an " +
	"outbound filter loses them"

// directHandshakeWhy says what a handshake's end means: expected is true for
// an end that is no fault (the browser gave up a parallel attempt another of
// the PC's addresses won).
func directHandshakeWhy(err error, after time.Duration) (why string, expected bool) {
	var te *quic.TransportError
	var ae *quic.ApplicationError
	var hto *quic.HandshakeTimeoutError
	var ito *quic.IdleTimeoutError
	switch {
	case errors.As(err, &hto) || errors.As(err, &ito):
		return "the browser never completed the handshake: " + directAnswersLost, false
	case errors.As(err, &te) && te.Remote && te.ErrorCode.IsCryptoError():
		return "the browser refused the PC's certificate: the hash the gateway gave it does not match (a gateway or agent " +
			"older than the other) or its clock is outside the certificate's validity", false
	case errors.As(err, &te) && te.Remote && te.ErrorCode == quic.NoError, errors.As(err, &ae) && ae.Remote:
		if after >= 2*time.Second {
			// The client gives an address 2.5 s (DIRECT_TIMEOUT_MS);
			// Chrome ends a handshake without answers after 4 s ("No
			// recent network activity").
			return "the browser gave up the handshake: " + directAnswersLost, false
		}
		return "the browser ended it (another of the PC's addresses answered first)", true
	case errors.As(err, &te) && te.Remote:
		return "the browser closed it: " + te.Error(), false
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The client's report

// logDirectReport logs the client's account of its direct path attempts
// (Hello.Direct at the session's start): the host log alone then says why a
// session streams through the gateway, and which of the PC's addresses
// answered. Clients before it send none.
func (s *Session) logDirectReport(r *proto.DirectReport) {
	if r == nil {
		return
	}
	if s.meta.Path == "direct" {
		s.log.Info("direct path: connected", "tried", r.Summary())
		return
	}
	why := r.Why
	if why == "" {
		why = "the client gave no reason"
	}
	s.log.Info("direct path: not used by this session, it streams through the gateway", "why", why, "tried", r.Summary())
}
