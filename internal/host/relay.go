package host

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// The host end of the gateway's UDP relay (guide step 2.6). One outbound UDP
// socket carries every relayed session: for each allocation the gateway
// announces over the tunnel ("relay" message: allocation ID, port, token), the
// host sends proto.RelayBind to the gateway's allocation port until the
// gateway answers. That opens the path through the host's NAT and firewall
// from the inside (no inbound port, like a TURN client), and the gateway then
// forwards the browser's QUIC datagrams unmodified. A WebTransport server on
// the socket terminates them: the browser runs one QUIC connection with the
// host, end to end, with the direct path's certificate (pinned by hash) and
// the host's congestion controller as the only one on the path.
//
// Only allocations the gateway announced are accepted: the quic.Transport
// refuses connections from any other address, one connection per allocation,
// and the session's hello must carry a gateway-signed ticket bound to the
// allocation (verifyTicket).

const (
	relayBindTimeout  = 2 * time.Second // the gateway's wait for the bind
	relayBindResend   = 200 * time.Millisecond
	relayUnusedTTL    = 30 * time.Second // the gateway gives the browser 20 s after the bind
	relayReleaseDelay = 2 * time.Second  // after the connection ended: about 3 PTO of draining
)

type relayServer struct {
	a    *Agent
	conn *net.UDPConn
	tr   *quic.Transport
	srv  *webtransport.Server

	mu     sync.Mutex
	allocs map[netip.AddrPort]*relayAlloc // by the gateway's allocation address
}

// relayAlloc is one gateway allocation.
type relayAlloc struct {
	id    string
	addr  netip.AddrPort
	token []byte
	bound chan struct{} // closed when the gateway confirmed the bind
	once  sync.Once
	used  bool // a connection arrived (guarded by relayServer.mu)
}

func (al *relayAlloc) isBound() bool {
	select {
	case <-al.bound:
		return true
	default:
		return false
	}
}

type relayAllocKey struct{}

var errNotAllocated = errors.New("not a relay allocation")

// relayInfo advertises the relay endpoint to the gateway.
func (a *Agent) relayInfo() *proto.RelayInfo {
	if a.directRot == nil {
		return nil
	}
	return &proto.RelayInfo{Hashes: a.directRot.HashesB64()}
}

// relayServer starts the relay socket and its WebTransport server on first use.
func (a *Agent) relayServer(ctx context.Context) (*relayServer, error) {
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	if a.relay != nil {
		return a.relay, nil
	}
	if a.directRot == nil {
		return nil, errors.New("no certificate")
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(8 << 20)
	_ = conn.SetWriteBuffer(8 << 20)
	rs := &relayServer{a: a, conn: conn, allocs: map[netip.AddrPort]*relayAlloc{}}
	rs.tr = &quic.Transport{Conn: conn, ConnContext: rs.connContext}
	quicConf := transport.QUICConfig(transport.WithCongestion(a.cfg.congestion()))
	ln, err := rs.tr.ListenEarly(http3.ConfigureTLSConfig(&tls.Config{GetCertificate: a.directRot.GetCertificate, MinVersion: tls.VersionTLS13}), quicConf)
	if err != nil {
		conn.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	rs.srv = &webtransport.Server{
		H3: &http3.Server{
			Handler:    mux,
			QUICConfig: quicConf,
			// Lets each session reach its connection's congestion controller.
			ConnContext: transport.WithQUICConn,
		},
		// The hello's ticket is bound to the page origin (as on the direct path).
		CheckOrigin: func(*http.Request) bool { return true },
	}
	sem := make(chan struct{}, 4) // bound concurrent unauthenticated handshakes
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		al, _ := r.Context().Value(relayAllocKey{}).(*relayAlloc)
		if al == nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		sess, err := rs.srv.Upgrade(w, r)
		<-sem
		if err != nil {
			a.log.Debug("relay upgrade failed", "err", err)
			return
		}
		c := transport.FromWebTransportOver(sess, transport.QUICConnFromContext(r.Context()))
		a.HandleConn(c, SessionMeta{Path: "relay", RequireTicket: true, Origin: r.Header.Get("Origin"), Relay: al.id})
	})
	go rs.accept(ctx, ln)
	go rs.readControl(ctx)
	go func() {
		<-ctx.Done()
		rs.srv.Close()
		ln.Close()
		rs.tr.Close()
		conn.Close()
		a.relayMu.Lock()
		a.relay = nil
		a.relayMu.Unlock()
	}()
	a.log.Info("relay socket ready", "local", conn.LocalAddr().String(), "congestion", a.cfg.congestion())
	a.relay = rs
	return rs, nil
}

func (rs *relayServer) accept(ctx context.Context, ln *quic.EarlyListener) {
	for {
		c, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		if al, _ := c.Context().Value(relayAllocKey{}).(*relayAlloc); al != nil {
			context.AfterFunc(c.Context(), func() {
				rs.remove(al)
				// Release the gateway's port once the closing connection had time to
				// deliver its CONNECTION_CLOSE (twice: a lost release only costs the
				// gateway's idle timeout).
				release, dst := proto.RelayReleasePacket(al.token), net.UDPAddrFromAddrPort(al.addr)
				for _, d := range []time.Duration{relayReleaseDelay, relayReleaseDelay + 100*time.Millisecond} {
					time.AfterFunc(d, func() { _, _ = rs.tr.WriteTo(release, dst) })
				}
			})
		}
		go func() {
			if err := rs.srv.ServeQUICConn(c); err != nil {
				rs.a.log.Debug("relay connection", "err", err)
			}
		}()
	}
}

// connContext admits one connection per allocation and refuses every other
// sender (quic.Transport.ConnContext). The gateway forwards nothing before the
// bind, so a QUIC packet from an allocation port also confirms the bind (the
// "bound" answer may have been lost).
func (rs *relayServer) connContext(ctx context.Context, ci *quic.ClientInfo) (context.Context, error) {
	ap := udpAddrPort(ci.RemoteAddr)
	rs.mu.Lock()
	al := rs.allocs[ap]
	ok := al != nil && !al.used
	if ok {
		al.used = true
	}
	rs.mu.Unlock()
	if !ok {
		rs.a.log.Debug("relay: refused a connection", "from", ap)
		return nil, errNotAllocated
	}
	al.once.Do(func() { close(al.bound) })
	return context.WithValue(ctx, relayAllocKey{}, al), nil
}

// readControl receives the gateway's bind confirmations.
func (rs *relayServer) readControl(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		n, from, err := rs.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if !proto.IsRelayBound(buf[:n]) {
			continue
		}
		rs.mu.Lock()
		al := rs.allocs[udpAddrPort(from)]
		rs.mu.Unlock()
		if al != nil {
			al.once.Do(func() { close(al.bound) })
		}
	}
}

func (rs *relayServer) remove(al *relayAlloc) {
	rs.mu.Lock()
	if rs.allocs[al.addr] == al {
		delete(rs.allocs, al.addr)
	}
	rs.mu.Unlock()
}

// openRelay binds the relay socket to a gateway allocation (tunnel message
// "relay"). gw is the gateway address the tunnel reached: the allocation port
// is on the same address.
func (a *Agent) openRelay(ctx context.Context, m proto.TunnelMsg, gw netip.Addr) {
	token, err := base64.StdEncoding.DecodeString(m.Nonce)
	if err != nil || len(token) != proto.RelayTokenLen || m.Port <= 0 || m.Port > 65535 || m.SID == "" || !gw.IsValid() {
		a.log.Warn("relay: invalid allocation from the gateway")
		return
	}
	rs, err := a.relayServer(ctx)
	if err != nil {
		a.log.Warn("relay socket failed", "err", err)
		return
	}
	al := &relayAlloc{id: m.SID, addr: netip.AddrPortFrom(gw.Unmap(), uint16(m.Port)), token: token, bound: make(chan struct{})}
	rs.mu.Lock()
	rs.allocs[al.addr] = al // a port the gateway reuses replaces its old allocation
	rs.mu.Unlock()
	bind := proto.RelayBindPacket(token)
	dst := net.UDPAddrFromAddrPort(al.addr)
	tick := time.NewTicker(relayBindResend)
	defer tick.Stop()
	timeout := time.NewTimer(relayBindTimeout)
	defer timeout.Stop()
	for !al.isBound() {
		if _, err := rs.tr.WriteTo(bind, dst); err != nil {
			a.log.Debug("relay bind", "err", err)
		}
		select {
		case <-al.bound:
		case <-tick.C:
		case <-timeout.C:
			a.log.Warn("relay: the gateway's relay port did not answer (UDP to it blocked?)", "gateway", al.addr.String())
			rs.remove(al)
			return
		case <-ctx.Done():
			return
		}
	}
	a.log.Debug("relay allocation bound", "gateway", al.addr.String(), "user", m.User)
	time.AfterFunc(relayUnusedTTL, func() {
		rs.mu.Lock()
		if !al.used && rs.allocs[al.addr] == al {
			delete(rs.allocs, al.addr)
		}
		rs.mu.Unlock()
	})
}

func udpAddrPort(a net.Addr) netip.AddrPort {
	if ua, ok := a.(*net.UDPAddr); ok {
		ap := ua.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}
