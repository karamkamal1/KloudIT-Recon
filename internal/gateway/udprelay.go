package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The UDP relay (guide step 2.6) carries one end-to-end QUIC connection
// between a browser and the host's WebTransport server, so the host's
// congestion controller is the only one on the path. The QUIC splice in
// relay.go terminates QUIC on both legs instead, and the two controllers fight
// (the gateway buffers whatever its browser leg cannot carry).
//
// Each relayed session gets an allocation: a UDP port of its own from the
// configured range. The browser's first packet (a QUIC Initial) carries nothing
// that names a session (a client-chosen random connection ID; the SNI is the
// gateway's name for every session), so sessions cannot share a port: the
// port is the routing key. The host binds its outbound relay socket to the
// allocation from the inside with a secret token (proto.RelayBind), like a TURN
// client, so it needs no inbound port; the browser is locked in by its first
// QUIC Initial from the IP address that requested the allocation. From then on
// datagrams are forwarded between exactly these two addresses, unmodified.

const (
	relayBindWait   = 5 * time.Second  // host bind after the allocation
	relayLockWait   = 20 * time.Second // browser's first Initial after the bind
	relayIdle       = 30 * time.Second // no datagram either way (QUIC idles out after 20 s)
	relayMaxPending = 4                // allocations per user the browser has not reached yet

	// Forwarding limits. Browser -> host carries ACKs, input and pings (well under
	// 1 Mbit/s); host -> browser the video (the host caps it at maxKbps).
	relayUpRate      = 32_000_000 / 8
	relayUpBurst     = 1 << 20
	relayDownRate    = 1_000_000_000 / 8
	relayDownBurst   = 8 << 20
	relayMaxDatagram = 64 << 10
)

// DefaultRelayPorts is the gateway's default UDP relay port range.
const DefaultRelayPorts = "8444-8459"

// parseRelayPorts parses "8444-8459", "40000,40002-40005" or "" / "off" (none).
func parseRelayPorts(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "off" || s == "0" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return nil, fmt.Errorf("relay ports %q: want ports or ranges like 8444-8459", s)
		}
		if len(out)+b-a+1 > 4096 {
			return nil, fmt.Errorf("relay ports %q: more than 4096 ports", s)
		}
		for p := a; p <= b; p++ {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// udpRelay hands out relay allocations.
type udpRelay struct {
	log   *slog.Logger
	ip    net.IP // address the allocation sockets bind to (the -listen host; nil = all)
	ports []int

	mu     sync.Mutex
	allocs map[int]*allocation // by port
	next   int                 // index into ports where the next search starts
	closed bool

	// Lifetimes (tests shorten them).
	bindWait, lockWait, idle time.Duration
}

func newUDPRelay(log *slog.Logger, ip net.IP, ports []int) *udpRelay {
	return &udpRelay{
		log: log, ip: ip, ports: ports, allocs: map[int]*allocation{},
		bindWait: relayBindWait, lockWait: relayLockWait, idle: relayIdle,
	}
}

var (
	errRelayFull    = errors.New("all relay ports are in use")
	errRelayPending = errors.New("too many relay allocations waiting for this user")
)

// allocRequest describes who an allocation is for.
type allocRequest struct {
	user, hostID string
	clientIP     netip.Addr                   // the browser must send from this IP
	onLock       func(browser netip.AddrPort) // the browser's first Initial arrived
	onEnd        func(*relayStats)            // the allocation closed after onLock
}

// allocation is one relayed session: a port, its host and its browser.
type allocation struct {
	r     *udpRelay
	req   allocRequest
	id    string
	port  int
	token []byte
	pc    *pktConn

	created time.Time
	bound   chan struct{} // closed when the host has bound
	locked  atomic.Bool   // the browser's address is known
	done    chan struct{} // closed when the allocation ends
	once    sync.Once

	stats relayStats
}

// relayStats counts what an allocation forwarded and dropped.
type relayStats struct {
	ToHost, ToBrowser       atomic.Int64 // bytes forwarded
	PktToHost, PktToBrowser atomic.Int64
	Dropped                 atomic.Int64 // datagrams from other addresses, over the rate limit, or unsendable
	Started, Last           time.Time    // browser locked in; last datagram forwarded (set when the allocation ends)
}

// allocate reserves a port and starts forwarding on it. The host still has to
// bind (allocation.bound) before the browser may connect.
func (r *udpRelay) allocate(req allocRequest) (*allocation, error) {
	token := make([]byte, proto.RelayTokenLen)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errRelayFull
	}
	pending := 0
	for _, a := range r.allocs {
		if a.req.user == req.user && !a.locked.Load() {
			pending++
		}
	}
	if pending >= relayMaxPending {
		return nil, errRelayPending
	}
	// Rotate through the range, so a port just released (and the packets still
	// in flight to it) is the last to be reused.
	for i := range r.ports {
		k := (r.next + i) % len(r.ports)
		port := r.ports[k]
		if r.allocs[port] != nil {
			continue
		}
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: r.ip, Port: port})
		if err != nil {
			r.log.Debug("udp relay: port unavailable", "port", port, "err", err)
			continue
		}
		_ = c.SetReadBuffer(8 << 20)
		_ = c.SetWriteBuffer(8 << 20)
		r.next = k + 1
		a := &allocation{
			r: r, req: req, id: auth.RandomToken(12), port: port, token: token, pc: newPktConn(c),
			created: time.Now(), bound: make(chan struct{}), done: make(chan struct{}),
		}
		r.allocs[port] = a
		go a.serve()
		return a, nil
	}
	return nil, errRelayFull
}

// closeAll ends every allocation (gateway shutdown).
func (r *udpRelay) closeAll() {
	r.mu.Lock()
	r.closed = true
	all := make([]*allocation, 0, len(r.allocs))
	for _, a := range r.allocs {
		all = append(all, a)
	}
	r.mu.Unlock()
	for _, a := range all {
		a.close()
	}
}

// close ends the allocation; serve releases the port.
func (a *allocation) close() {
	a.once.Do(func() {
		close(a.done)
		a.pc.c.Close()
	})
}

// peer is one end of an allocation and the local address its datagrams
// arrive at (answers leave from it).
type peer struct {
	addr  netip.AddrPort
	local netip.Addr
	oob   []byte // source-address control message for answers
}

func (a *allocation) serve() {
	r := a.r
	var host, browser peer
	var boundAt time.Time
	last := time.Now()
	up := tokenBucket{rate: relayUpRate, burst: relayUpBurst, tokens: relayUpBurst, ts: last}
	down := tokenBucket{rate: relayDownRate, burst: relayDownBurst, tokens: relayDownBurst, ts: last}
	defer func() {
		a.close()
		r.mu.Lock()
		if r.allocs[a.port] == a {
			delete(r.allocs, a.port)
		}
		r.mu.Unlock()
		if a.locked.Load() {
			a.stats.Last = last
			r.log.Info("udp relay: session ended", "port", a.port, "host", a.req.hostID, "user", a.req.user,
				"to_browser_mb", fmt.Sprintf("%.1f", float64(a.stats.ToBrowser.Load())/1e6),
				"to_host_mb", fmt.Sprintf("%.1f", float64(a.stats.ToHost.Load())/1e6),
				"dropped", a.stats.Dropped.Load(), "after", last.Sub(a.stats.Started).Round(time.Second))
			if a.req.onEnd != nil {
				a.req.onEnd(&a.stats)
			}
		}
	}()
	send := func(b []byte, to peer) bool {
		if err := a.pc.write(b, to.addr, to.oob); err != nil {
			a.stats.Dropped.Add(1) // e.g. EMSGSIZE: DF is set, as on a router
			return false
		}
		return true
	}
	buf := make([]byte, relayMaxDatagram)
	deadline := time.Now().Add(time.Second)
	_ = a.pc.c.SetReadDeadline(deadline)
	for {
		n, from, local, err := a.pc.read(buf)
		now := time.Now()
		timeout := false
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return // closed
			}
			timeout = true
		}
		if timeout || !now.Before(deadline) { // lifetimes, checked about once a second
			switch {
			case !host.addr.IsValid() && now.Sub(a.created) > r.bindWait:
				r.log.Debug("udp relay: host did not bind", "port", a.port, "host", a.req.hostID)
				return
			case host.addr.IsValid() && !browser.addr.IsValid() && now.Sub(boundAt) > r.lockWait:
				r.log.Info("udp relay: the browser never arrived (is the relay port range open in the firewall?)",
					"port", a.port, "host", a.req.hostID, "user", a.req.user)
				return
			case browser.addr.IsValid() && now.Sub(last) > r.idle:
				return
			}
			deadline = now.Add(time.Second)
			_ = a.pc.c.SetReadDeadline(deadline)
		}
		if err != nil {
			continue
		}
		pkt := buf[:n]
		switch {
		case host.addr.IsValid() && from == host.addr:
			if typ, tok, ok := proto.ParseRelayToken(pkt); ok { // never forwarded
				switch {
				case subtle.ConstantTimeCompare(tok, a.token) != 1:
					a.stats.Dropped.Add(1) // e.g. a late release of the port's previous allocation
				case typ == proto.RelayRelease: // the relayed connection has ended
					return
				default:
					send(proto.RelayBoundPacket(), host) // a retransmitted bind
				}
				continue
			}
			if !browser.addr.IsValid() || !down.take(n, now) {
				a.stats.Dropped.Add(1)
				continue
			}
			last = now
			if send(pkt, browser) {
				a.stats.ToBrowser.Add(int64(n))
				a.stats.PktToBrowser.Add(1)
			}
		case browser.addr.IsValid() && from == browser.addr:
			if !up.take(n, now) {
				a.stats.Dropped.Add(1)
				continue
			}
			last = now
			if send(pkt, host) {
				a.stats.ToHost.Add(int64(n))
				a.stats.PktToHost.Add(1)
			}
		case !host.addr.IsValid():
			typ, tok, ok := proto.ParseRelayToken(pkt)
			if !ok || typ != proto.RelayBind || subtle.ConstantTimeCompare(tok, a.token) != 1 {
				a.stats.Dropped.Add(1)
				continue
			}
			host = peer{addr: from, local: local, oob: a.pc.sourceOOB(local)}
			boundAt, last = now, now
			close(a.bound)
			r.log.Debug("udp relay: host bound", "port", a.port, "host", a.req.hostID, "from", from)
			send(proto.RelayBoundPacket(), host)
		case !browser.addr.IsValid():
			// Only a QUIC Initial from the requesting IP locks the browser in:
			// nothing is ever sent to an address before that.
			if !isQUICInitial(pkt) || !sameClientIP(from.Addr(), a.req.clientIP) || !up.take(n, now) {
				a.stats.Dropped.Add(1)
				continue
			}
			browser = peer{addr: from, local: local, oob: a.pc.sourceOOB(local)}
			last = now
			a.stats.Started = now
			a.locked.Store(true)
			r.log.Info("udp relay: session started", "port", a.port, "host", a.req.hostID, "user", a.req.user, "browser", from)
			if a.req.onLock != nil {
				a.req.onLock(from)
			}
			if send(pkt, host) {
				a.stats.ToHost.Add(int64(n))
				a.stats.PktToHost.Add(1)
			}
		default:
			a.stats.Dropped.Add(1) // a stranger, or the browser from a new address (no migration)
		}
	}
}

// isQUICInitial reports whether b is a QUIC v1 or v2 Initial packet of the
// minimum size a client must pad its Initials to (RFC 9000 14.1).
func isQUICInitial(b []byte) bool {
	if len(b) < 1200 || b[0]&0xc0 != 0xc0 {
		return false
	}
	switch binary.BigEndian.Uint32(b[1:5]) {
	case 0x00000001: // RFC 9000: Initial = type 0
		return b[0]&0x30 == 0x00
	case 0x6b3343cf: // RFC 9369: Initial = type 1
		return b[0]&0x30 == 0x10
	}
	return false
}

// sameClientIP compares the browser's UDP source with the IP its HTTPS request
// came from. ::1 and 127.0.0.1 match each other ("localhost" resolves to both).
func sameClientIP(udp, http netip.Addr) bool {
	udp, http = udp.Unmap(), http.Unmap()
	localhost := func(a netip.Addr) bool {
		return a == netip.IPv6Loopback() || a == netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	return udp == http || (localhost(udp) && localhost(http))
}

// tokenBucket limits a forwarding direction in bytes per second.
type tokenBucket struct {
	rate, burst, tokens float64
	ts                  time.Time
}

func (b *tokenBucket) take(n int, now time.Time) bool {
	b.tokens = min(b.burst, b.tokens+now.Sub(b.ts).Seconds()*b.rate)
	b.ts = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
