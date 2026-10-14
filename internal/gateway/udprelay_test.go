package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

func TestParseRelayPorts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
		bad  bool
	}{
		{"", nil, false},
		{"off", nil, false},
		{"8444-8447", []int{8444, 8445, 8446, 8447}, false},
		{"9000, 8444-8445,9000", []int{8444, 8445, 9000}, false},
		{"8447-8444", nil, true},
		{"0-3", nil, true},
		{"65535-65536", nil, true},
		{"x", nil, true},
		{"1-5000", nil, true},
	} {
		got, err := parseRelayPorts(tc.in)
		if (err != nil) != tc.bad || !slices.Equal(got, tc.want) {
			t.Errorf("parseRelayPorts(%q) = %v, %v", tc.in, got, err)
		}
	}
}

func TestIsQUICInitial(t *testing.T) {
	p := make([]byte, 1200)
	p[0] = 0xc3 // long header, fixed bit, Initial, pn length 4
	copy(p[1:], []byte{0, 0, 0, 1})
	if !isQUICInitial(p) {
		t.Fatal("v1 Initial not recognised")
	}
	if isQUICInitial(p[:1199]) {
		t.Fatal("unpadded Initial accepted")
	}
	p[0] = 0xe3 // Handshake
	if isQUICInitial(p) {
		t.Fatal("Handshake packet accepted as Initial")
	}
	p[0] = 0x43 // short header
	if isQUICInitial(p) {
		t.Fatal("short header accepted")
	}
	p[0] = 0xd3 // v2 Initial is type 1
	copy(p[1:], []byte{0x6b, 0x33, 0x43, 0xcf})
	if !isQUICInitial(p) {
		t.Fatal("v2 Initial not recognised")
	}
	if !sameClientIP(netip.MustParseAddr("::ffff:192.0.2.1"), netip.MustParseAddr("192.0.2.1")) ||
		!sameClientIP(netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")) ||
		sameClientIP(netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")) ||
		sameClientIP(netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("sameClientIP")
	}
}

// ---------------------------------------------------------------------------
// Test rig: a relay on loopback, a "host" (quic-go server behind an outbound
// relay socket, like internal/host/relay.go) and "browsers" (quic-go clients).

const relayTestALPN = "relay-test"

func relayTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	cert, err := tlsutil.SelfSigned([]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	server = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{relayTestALPN}}
	client = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{relayTestALPN}}
	return server, client
}

func freeUDPPorts(t *testing.T, n int) []int {
	t.Helper()
	var out []int
	for len(out) < n {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		p := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func newTestRelay(t *testing.T, ports int) *udpRelay {
	t.Helper()
	r := newUDPRelay(slog.New(slog.NewTextHandler(io.Discard, nil)), net.IPv4(127, 0, 0, 1), freeUDPPorts(t, ports))
	t.Cleanup(r.closeAll)
	return r
}

func loopback(t *testing.T, ip string) *quic.Transport {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadBuffer(8 << 20)
	_ = c.SetWriteBuffer(8 << 20)
	tr := &quic.Transport{Conn: c}
	t.Cleanup(func() { tr.Close(); c.Close() })
	return tr
}

func (a *allocation) addr() *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.port}
}

// relayHost is the host end: a QUIC server on a socket that bound itself to
// an allocation.
type relayHost struct {
	tr *quic.Transport
	ln *quic.Listener
}

func newRelayHost(t *testing.T, serverTLS *tls.Config, conf *quic.Config) *relayHost {
	t.Helper()
	h := &relayHost{tr: loopback(t, "127.0.0.1")}
	var err error
	if h.ln, err = h.tr.Listen(serverTLS, conf); err != nil {
		t.Fatal(err)
	}
	return h
}

// bind sends the allocation's token until the gateway answers.
func (h *relayHost) bind(t *testing.T, a *allocation, token []byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			h.tr.WriteTo(proto.RelayBindPacket(token), a.addr())
			time.Sleep(50 * time.Millisecond)
		}
	}()
	buf := make([]byte, 64)
	for {
		n, from, err := h.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return err
		}
		if proto.IsRelayBound(buf[:n]) && from.(*net.UDPAddr).Port == a.port {
			return nil
		}
	}
}

func allocate(t *testing.T, r *udpRelay, user string) *allocation {
	t.Helper()
	a, err := r.allocate(allocRequest{user: user, hostID: "h", clientIP: netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// echo serves one connection: datagrams and bidirectional streams are echoed.
func echo(ctx context.Context, c *quic.Conn) {
	go func() {
		for {
			d, err := c.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			c.SendDatagram(d)
		}
	}()
	for {
		s, err := c.AcceptStream(ctx)
		if err != nil {
			return
		}
		go func() { io.Copy(s, s); s.Close() }()
	}
}

func TestUDPRelayForwardsQUIC(t *testing.T) {
	r := newTestRelay(t, 2)
	serverTLS, clientTLS := relayTLS(t)
	host := newRelayHost(t, serverTLS, transport.QUICConfig())
	var locked atomic.Value
	ended := make(chan *relayStats, 1)
	a, err := r.allocate(allocRequest{user: "u", hostID: "h", clientIP: netip.MustParseAddr("127.0.0.1"),
		onLock: func(b netip.AddrPort) { locked.Store(b) },
		onEnd:  func(st *relayStats) { ended <- st },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.bind(t, a, a.token); err != nil {
		t.Fatal("bind:", err)
	}
	select {
	case <-a.bound:
	default:
		t.Fatal("bound answered but the allocation is not bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for {
			c, err := host.ln.Accept(ctx)
			if err != nil {
				return
			}
			go echo(ctx, c)
		}
	}()
	browser := loopback(t, "127.0.0.1")
	c, err := browser.Dial(ctx, a.addr(), clientTLS, transport.QUICConfig())
	if err != nil {
		t.Fatal("dial through the relay:", err)
	}
	if b, _ := locked.Load().(netip.AddrPort); b.Port() != uint16(browser.Conn.LocalAddr().(*net.UDPAddr).Port) {
		t.Fatalf("locked to %v, want the browser's socket %v", b, browser.Conn.LocalAddr())
	}
	// 4 MiB through a stream and back.
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	go func() { s.Write(payload); s.Close() }()
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo: %d bytes, %v", len(got), err)
	}
	if err := c.SendDatagram([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if d, err := c.ReceiveDatagram(ctx); err != nil || string(d) != "ping" {
		t.Fatalf("datagram %q %v", d, err)
	}
	if a.stats.ToHost.Load() < 4<<20 || a.stats.ToBrowser.Load() < 4<<20 {
		t.Fatalf("stats: %d bytes to the host, %d to the browser", a.stats.ToHost.Load(), a.stats.ToBrowser.Load())
	}
	c.CloseWithError(0, "")

	// The host releases the allocation: the port is free at once.
	host.tr.WriteTo(proto.RelayReleasePacket(a.token), a.addr())
	select {
	case st := <-ended:
		if st.Last.Before(st.Started) {
			t.Fatal("stats without a duration")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release did not end the allocation")
	}
	r.mu.Lock()
	n := len(r.allocs)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d allocations after the release", n)
	}
}

// Nothing reaches the host or gets an answer before the host bound with the
// right token and the browser sent a QUIC Initial from the requesting IP; after
// that, only those two addresses are forwarded between.
func TestUDPRelayRefusesStrangers(t *testing.T) {
	r := newTestRelay(t, 1)
	r.bindWait = 10 * time.Second // the wrong-token bind below takes 2 s
	serverTLS, clientTLS := relayTLS(t)
	host := newRelayHost(t, serverTLS, transport.QUICConfig())
	a := allocate(t, r, "u")
	stranger, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	silent := func(what string) {
		t.Helper()
		stranger.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, _, err := stranger.ReadFrom(make([]byte, 2048)); err == nil {
			t.Fatalf("%s: the relay answered a stranger with %d bytes", what, n)
		}
	}
	initial := make([]byte, 1200)
	initial[0] = 0xc3
	copy(initial[1:], []byte{0, 0, 0, 1})

	// Before the bind: garbage, an Initial and a wrong token are all dropped.
	stranger.WriteTo(initial, a.addr())
	wrong := bytes.Repeat([]byte{7}, proto.RelayTokenLen)
	stranger.WriteTo(proto.RelayBindPacket(wrong), a.addr())
	silent("before the bind")
	if err := host.bind(t, a, wrong); err == nil {
		t.Fatal("a wrong token bound the allocation")
	}
	if err := host.bind(t, a, a.token); err != nil {
		t.Fatal("bind:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for {
			c, err := host.ln.Accept(ctx)
			if err != nil {
				return
			}
			go echo(ctx, c)
		}
	}()
	// A browser on another IP than the one that requested the allocation
	// cannot lock it.
	other := loopback(t, "127.0.0.2")
	dctx, dcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	if _, err := other.Dial(dctx, a.addr(), clientTLS, transport.QUICConfig()); err == nil {
		t.Fatal("a browser from another IP connected")
	}
	dcancel()
	if a.locked.Load() {
		t.Fatal("locked to the wrong IP")
	}
	// Neither can a non-Initial packet from the right IP.
	short := slices.Clone(initial)
	short[0] = 0x43
	stranger.WriteTo(short, a.addr())
	silent("short header before the lock")
	if a.locked.Load() {
		t.Fatal("locked by a short-header packet")
	}
	// The real browser.
	browser := loopback(t, "127.0.0.1")
	c, err := browser.Dial(ctx, a.addr(), clientTLS, transport.QUICConfig())
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer c.CloseWithError(0, "")
	// After the lock a stranger's datagrams go nowhere, even a valid bind.
	dropped := a.stats.Dropped.Load()
	stranger.WriteTo(initial, a.addr())
	stranger.WriteTo(proto.RelayBindPacket(a.token), a.addr())
	silent("after the lock")
	if a.stats.Dropped.Load() < dropped+2 {
		t.Fatalf("dropped %d -> %d", dropped, a.stats.Dropped.Load())
	}
	if err := c.SendDatagram([]byte("still works")); err != nil {
		t.Fatal(err)
	}
	if d, err := c.ReceiveDatagram(ctx); err != nil || string(d) != "still works" {
		t.Fatalf("datagram %q %v", d, err)
	}
}

// syncLog is a slog destination tests read while the relay writes.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestUDPRelayIPMismatchLogged: QUIC Initials from another IP than the
// browser's HTTPS request (a reverse proxy without -trust-proxy, IPv6 vs
// IPv4) are refused as before, but the gateway says so, once, with the
// source and the expected IP, and the allocation's end names that cause
// with the dropped count instead of blaming the firewall; without such
// Initials it still points at the relay ports.
func TestUDPRelayIPMismatchLogged(t *testing.T) {
	logs := &syncLog{}
	r := newUDPRelay(slog.New(slog.NewTextHandler(logs, nil)), net.IPv4(127, 0, 0, 1), freeUDPPorts(t, 2))
	t.Cleanup(r.closeAll)
	r.lockWait = 300 * time.Millisecond
	serverTLS, _ := relayTLS(t)
	host := newRelayHost(t, serverTLS, transport.QUICConfig())
	ended := func(a *allocation) {
		t.Helper()
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
			t.Fatal("allocation kept")
		}
	}
	a := allocate(t, r, "u") // requested from 127.0.0.1
	if err := host.bind(t, a, a.token); err != nil {
		t.Fatal(err)
	}
	other, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	initial := make([]byte, 1200)
	initial[0] = 0xc3
	copy(initial[1:], []byte{0, 0, 0, 1})
	for range 3 {
		other.WriteTo(initial, a.addr())
	}
	ended(a)
	if a.locked.Load() {
		t.Fatal("locked to the wrong IP")
	}
	from := other.LocalAddr().String()
	out := logs.String()
	if n := strings.Count(out, "udp relay: refused a QUIC Initial from another IP"); n != 1 ||
		!strings.Contains(out, "from="+from+" expected=127.0.0.1") {
		t.Fatalf("%d refusal lines:\n%s", n, out)
	}
	if !strings.Contains(out, "udp relay: the browser never arrived from its HTTPS request's IP") ||
		!strings.Contains(out, "-trust-proxy") || !strings.Contains(out, "expected=127.0.0.1 refused="+from+" dropped=3") ||
		strings.Contains(out, "is the relay port range open") {
		t.Fatalf("end of the allocation:\n%s", out)
	}
	// Nothing came: the relay ports.
	b := allocate(t, r, "u")
	if err := host.bind(t, b, b.token); err != nil {
		t.Fatal(err)
	}
	ended(b)
	if out := logs.String(); !strings.Contains(out, "udp relay: the browser never arrived (is the relay port range open in the firewall?)") ||
		!strings.Contains(out, fmt.Sprintf("port=%d host=h user=u dropped=0", b.port)) {
		t.Fatalf("no browser:\n%s", out)
	}
}

func TestUDPRelayLifetimes(t *testing.T) {
	r := newTestRelay(t, 3)
	r.bindWait, r.lockWait, r.idle = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	serverTLS, clientTLS := relayTLS(t)
	host := newRelayHost(t, serverTLS, transport.QUICConfig())
	ended := func(a *allocation, within time.Duration) bool {
		select {
		case <-a.done:
			return true
		case <-time.After(within):
			return false
		}
	}
	// Never bound.
	unbound := allocate(t, r, "u")
	if !ended(unbound, 3*time.Second) {
		t.Fatal("unbound allocation kept")
	}
	// Bound, the browser never came.
	nobrowser := allocate(t, r, "u")
	if err := host.bind(t, nobrowser, nobrowser.token); err != nil {
		t.Fatal(err)
	}
	if !ended(nobrowser, 3*time.Second) {
		t.Fatal("allocation without a browser kept")
	}
	// Used, then idle (the client vanishes without closing).
	idle := allocate(t, r, "u")
	if err := host.bind(t, idle, idle.token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		c, err := host.ln.Accept(ctx)
		if err == nil {
			echo(ctx, c)
		}
	}()
	conf := transport.QUICConfig()
	conf.KeepAlivePeriod = 0
	browser := loopback(t, "127.0.0.1")
	c, err := browser.Dial(ctx, idle.addr(), clientTLS, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseWithError(0, "")
	if ended(idle, 100*time.Millisecond) {
		t.Fatal("ended while in use")
	}
	browser.Conn.(*net.UDPConn).Close() // gone without a CONNECTION_CLOSE
	if !ended(idle, 3*time.Second) {
		t.Fatal("idle allocation kept")
	}
	r.mu.Lock()
	n := len(r.allocs)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d allocations left", n)
	}
}

// TestUDPRelayHostSilence: a locked allocation lives on what the host sends.
// Datagrams from the browser's address alone (an Initial-shaped packet locked
// it, the host has no connection for it) do not keep the port.
func TestUDPRelayHostSilence(t *testing.T) {
	r := newTestRelay(t, 1)
	r.idle = 500 * time.Millisecond
	a := allocate(t, r, "u")
	udp := func() *net.UDPConn {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	host, browser := udp(), udp()
	buf := make([]byte, 2048)
	for bound := false; !bound; {
		host.WriteTo(proto.RelayBindPacket(a.token), a.addr())
		host.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _, err := host.ReadFrom(buf)
		bound = err == nil && proto.IsRelayBound(buf[:n])
		if time.Since(a.created) > 2*time.Second {
			t.Fatal("no bind")
		}
	}
	initial := make([]byte, 1200)
	initial[0] = 0xc3
	copy(initial[1:], []byte{0, 0, 0, 1})
	browser.WriteTo(initial, a.addr())
	host.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := host.ReadFrom(buf); err != nil || !a.locked.Load() {
		t.Fatalf("the Initial was not forwarded (%v) or did not lock the allocation", err)
	}
	// keep sends from the browser (and from the host while hostToo) every
	// 50 ms for d and reports whether the allocation ended meanwhile.
	keep := func(d time.Duration, hostToo bool) bool {
		for end := time.Now().Add(d); time.Now().Before(end); {
			browser.WriteTo([]byte("browser"), a.addr())
			if hostToo {
				host.WriteTo([]byte("host"), a.addr())
			}
			select {
			case <-a.done:
				return true
			case <-time.After(50 * time.Millisecond):
			}
		}
		return false
	}
	if keep(2*time.Second, true) {
		t.Fatal("ended while the host was sending")
	}
	if !keep(3*time.Second, false) {
		t.Fatal("the browser's datagrams alone kept the allocation")
	}
}

func TestUDPRelayLimits(t *testing.T) {
	r := newTestRelay(t, relayMaxPerUser+2)
	for i := 0; i < relayMaxPerUser; i++ {
		a := allocate(t, r, "alice")
		a.locked.Store(i%2 == 0) // in use or not, each counts
	}
	if _, err := r.allocate(allocRequest{user: "alice"}); !errors.Is(err, errRelayPerUser) {
		t.Fatalf("allocations per user not limited: %v", err)
	}
	allocate(t, r, "bob")
	allocate(t, r, "bob")
	if _, err := r.allocate(allocRequest{user: "carol"}); !errors.Is(err, errRelayFull) {
		t.Fatalf("all ports in use: %v", err)
	}
	// A port someone else holds is skipped.
	busy := newTestRelay(t, 1)
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: busy.ip, Port: busy.ports[0]})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := busy.allocate(allocRequest{user: "dave"}); !errors.Is(err, errRelayFull) {
		t.Fatalf("allocated a port in use: %v", err)
	}
	// Browser -> host is rate limited.
	b := tokenBucket{rate: 1000, burst: 1500, tokens: 1500, ts: time.Now()}
	if !b.take(1200, b.ts) || b.take(1200, b.ts) || !b.take(1200, b.ts.Add(time.Second)) {
		t.Fatal("token bucket")
	}
}

// ---------------------------------------------------------------------------
// Measurement (guide step 2.6 acceptance): the relay adds < 2 ms to the round
// trip on loopback and, since the host's congestion controller runs end to
// end, the goodput over a lossy browser leg matches the direct path. The QUIC
// splice relay is measured alongside for comparison: its browser leg runs the
// gateway's own NewReno.

// lossyLink sits in front of server for one client: delay each way, random
// loss, and towards the client (newShapedLink) a bottleneck: a link of rate
// bit/s whose tail-drop queue holds queue of it.
type lossyLink struct {
	pc, up     *net.UDPConn
	client     atomic.Pointer[net.UDPAddr]
	dropped    atomic.Int64 // random losses
	overflowed atomic.Int64 // datagrams the bottleneck's full queue dropped
}

func newLossyLink(t *testing.T, server *net.UDPAddr, delay time.Duration, loss float64) *lossyLink {
	t.Helper()
	return newLink(t, server, delay, loss, 0, 0)
}

// newShapedLink is a link to the client of rate bit/s with a tail-drop queue
// of queue (a 100 Mbit/s Ethernet link and its switch port's buffer), no
// delay or loss of its own.
func newShapedLink(t *testing.T, server *net.UDPAddr, rate float64, queue time.Duration) *lossyLink {
	t.Helper()
	return newLink(t, server, 0, 0, rate, queue)
}

func newLink(t *testing.T, server *net.UDPAddr, delay time.Duration, loss, rate float64, queue time.Duration) *lossyLink {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	up, err := net.DialUDP("udp", nil, server)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*net.UDPConn{pc, up} {
		c.SetReadBuffer(8 << 20)
		c.SetWriteBuffer(8 << 20)
	}
	l := &lossyLink{pc: pc, up: up}
	// pipe forwards one way; rate > 0: through the bottleneck, whose queue
	// is the time its last datagram leaves (free) less now.
	pipe := func(seed uint64, rate float64, read func([]byte) (int, error), write func([]byte)) {
		type pkt struct {
			due  time.Time
			data []byte
		}
		q := make(chan pkt, 1<<15)
		go func() {
			for p := range q {
				time.Sleep(time.Until(p.due))
				write(p.data)
			}
		}()
		rng := rand.New(rand.NewPCG(seed, 7))
		defer close(q)
		var free time.Time
		for {
			b := make([]byte, 2048)
			n, err := read(b)
			if err != nil {
				return
			}
			if rng.Float64() < loss {
				l.dropped.Add(1)
				continue
			}
			due := time.Now()
			if rate > 0 {
				if free.Before(due) {
					free = due
				}
				if free.Sub(due) > queue {
					l.overflowed.Add(1)
					continue
				}
				free = free.Add(time.Duration(float64(n*8) / rate * float64(time.Second)))
				due = free
			}
			select {
			case q <- pkt{due.Add(delay), b[:n]}:
			default:
			}
		}
	}
	go pipe(1, 0, func(b []byte) (int, error) {
		n, a, err := pc.ReadFromUDP(b)
		if err == nil {
			l.client.Store(a)
		}
		return n, err
	}, func(b []byte) { up.Write(b) })
	go pipe(2, rate, up.Read, func(b []byte) {
		if a := l.client.Load(); a != nil {
			pc.WriteToUDP(b, a)
		}
	})
	t.Cleanup(func() { pc.Close(); up.Close() })
	return l
}

func (l *lossyLink) addr() *net.UDPAddr { return l.pc.LocalAddr().(*net.UDPAddr) }

// rttSamples measures datagram round trips on c (echoed by the far end).
func rttSamples(ctx context.Context, c *quic.Conn, n int) ([]time.Duration, error) {
	var out []time.Duration
	buf := make([]byte, 16)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		buf[0] = byte(i)
		if err := c.SendDatagram(buf); err != nil {
			return nil, err
		}
		rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		d, err := c.ReceiveDatagram(rctx)
		cancel()
		if err == nil && d[0] == byte(i) {
			out = append(out, time.Since(t0))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(out) < n/2 {
		return nil, errors.New("too many lost pings")
	}
	slices.Sort(out)
	return out, nil
}

func pct(s []time.Duration, p float64) time.Duration { return s[int(p*float64(len(s)-1))] }

// goodputSeries runs a bulk transfer from the host end (send) to the browser
// end (recv) for d and returns the received rate per 100 ms (bit/s), without
// the first second. A stream or connection that ends before d (the sender
// ends the stream at d of its own clock, which starts a little earlier) is
// an error, not a rate of 0 for the rest of the run: a quic-go receiver,
// for one, closes a connection whose stream holds more than 1000 gaps.
func goodputSeries(ctx context.Context, send, recv *quic.Conn, target int64, d time.Duration) ([]float64, error) {
	if m := transport.MediaControl(transport.FromQUIC(send)); m != nil && target > 0 {
		m.SetTarget(target, time.Second/60)
	}
	errc := make(chan error, 1)
	go func() {
		s, err := send.OpenUniStreamSync(ctx)
		if err != nil {
			errc <- err
			return
		}
		s.SetWriteDeadline(time.Now().Add(d))
		buf := make([]byte, 32<<10)
		for {
			if _, err := s.Write(buf); err != nil {
				break
			}
		}
		s.CancelWrite(0)
		errc <- nil
	}()
	s, err := recv.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	const bucket = 100 * time.Millisecond
	start := time.Now()
	counts := make([]int64, int(d/bucket)+1)
	buf := make([]byte, 64<<10)
	var early error
	for {
		n, err := s.Read(buf)
		at := time.Since(start)
		if k := int(at / bucket); k < len(counts) {
			counts[k] += int64(n)
		}
		if err != nil && at < d-bucket {
			early = fmt.Errorf("the stream ended after %v: %w", at.Round(time.Millisecond), err)
		}
		if err != nil || at > d {
			break
		}
	}
	s.CancelRead(0)
	<-errc
	if early != nil {
		return nil, early
	}
	var out []float64
	for _, c := range counts[int(time.Second/bucket) : len(counts)-1] {
		out = append(out, float64(c)*8/bucket.Seconds())
	}
	return out, nil
}

type seriesStats struct{ mean, cv, p10 float64 }

func summarize(s []float64) seriesStats {
	var sum, sq float64
	for _, v := range s {
		sum += v
	}
	mean := sum / float64(len(s))
	for _, v := range s {
		sq += (v - mean) * (v - mean)
	}
	sorted := slices.Sorted(slices.Values(s))
	return seriesStats{mean: mean, cv: math.Sqrt(sq/float64(len(s))) / mean, p10: sorted[len(sorted)/10]}
}

func TestUDPRelayLatencyAndThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("about 20 s of measurements")
	}
	serverTLS, clientTLS := relayTLS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The host: one QUIC server for every path (media congestion control), each
	// accepted connection handed to whoever waits for it.
	host := newRelayHost(t, serverTLS, transport.QUICConfig(transport.WithCongestion(transport.CongestionMedia)))
	accepted := make(chan *quic.Conn, 4)
	go func() {
		for {
			c, err := host.ln.Accept(ctx)
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	r := newTestRelay(t, 4)
	relay := func(t *testing.T) *allocation {
		a := allocate(t, r, "u")
		if err := host.bind(t, a, a.token); err != nil {
			t.Fatal(err)
		}
		return a
	}
	viaRelay := func(t *testing.T) *net.UDPAddr { return relay(t).addr() }
	direct := host.tr.Conn.LocalAddr().(*net.UDPAddr)
	// dial connects a browser to addr and returns both ends.
	dial := func(t *testing.T, addr *net.UDPAddr) (browser, hostEnd *quic.Conn) {
		t.Helper()
		c, err := loopback(t, "127.0.0.1").Dial(ctx, addr, clientTLS, transport.QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseWithError(0, "") })
		select {
		case h := <-accepted:
			return c, h
		case <-ctx.Done():
			t.Fatal("no connection at the host")
		}
		return nil, nil
	}

	// 1. Round trip on loopback, direct and relayed connections interleaved.
	dBrowser, dHost := dial(t, direct)
	rBrowser, rHost := dial(t, viaRelay(t))
	go echo(ctx, dHost)
	go echo(ctx, rHost)
	var dRTT, rRTT []time.Duration
	for range 3 {
		d, err := rttSamples(ctx, dBrowser, 100)
		if err != nil {
			t.Fatal(err)
		}
		rr, err := rttSamples(ctx, rBrowser, 100)
		if err != nil {
			t.Fatal(err)
		}
		dRTT, rRTT = append(dRTT, d...), append(rRTT, rr...)
	}
	slices.Sort(dRTT)
	slices.Sort(rRTT)
	added := pct(rRTT, 0.5) - pct(dRTT, 0.5)
	t.Logf("loopback RTT p50/p95/p99: direct %v/%v/%v, udp relay %v/%v/%v (relay adds %v at p50)",
		pct(dRTT, .5), pct(dRTT, .95), pct(dRTT, .99), pct(rRTT, .5), pct(rRTT, .95), pct(rRTT, .99), added)
	if added > 2*time.Millisecond {
		t.Errorf("the relay adds %v to the median round trip, want < 2 ms", added)
	}

	// 2. Bulk throughput on clean loopback: the relay carries a stream as
	// fast as the host sends one at a high setting, as the direct path does,
	// and drops none of it. The media controller paces a bulk sender at
	// 1.2 x its target, and nothing lowers the target here (no rate
	// controller): bulkTarget's 600 Mbit/s of pacing is twice that of the
	// host's highest default setting (maxKbps 250 Mbit/s) and under the
	// forwarder's own limit (relayDownRate). A target above that limit
	// measures the overrun, not the relay: the forwarder drops what exceeds
	// its limit (at 2 Gbit/s one datagram in two, finely interleaved), and
	// a quic-go receiver closes a connection whose one stream holds more
	// than 1000 gaps within a second (docs/VENDOR_NOTES.md "Merged second
	// hardware run: the UDP relay under overload"). TestUDPRelayOverrun has
	// the overrun.
	const bulkTarget = 500_000_000
	var bulk [2]float64
	for i, path := range []string{"direct", "udp relay"} {
		addr := direct
		var a *allocation
		if path != "direct" {
			a = relay(t)
			addr = a.addr()
		}
		b, h := dial(t, addr)
		s, err := goodputSeries(ctx, h, b, bulkTarget, 2*time.Second)
		if err != nil {
			t.Fatalf("loopback bulk, %s: %v", path, err)
		}
		bulk[i] = summarize(s).mean
		if a == nil {
			t.Logf("loopback bulk, %s: %.0f Mbit/s", path, bulk[i]/1e6)
			continue
		}
		t.Logf("loopback bulk, %s: %.0f Mbit/s (%d datagrams to the browser, %d dropped, %d dropped by the kernel)",
			path, bulk[i]/1e6, a.stats.PktToBrowser.Load(), a.stats.Dropped.Load(), a.stats.SocketDrops.Load())
		if n := a.stats.Dropped.Load(); n > 0 {
			t.Errorf("the relay dropped %d datagrams at %d Mbit/s of pacing, under its limit of %d", n, bulkTarget*6/5/1_000_000, relayDownRate*8/1_000_000)
		}
	}
	if bulk[1] < 0.8*bulk[0] || bulk[0] < 50e6 {
		t.Errorf("loopback bulk at a %d Mbit/s target: udp relay %.0f Mbit/s, direct %.0f Mbit/s: want the relay within 20 %% of the direct path",
			bulkTarget/1_000_000, bulk[1]/1e6, bulk[0]/1e6)
	}

	// 3. A lossy browser leg (20 ms each way, 1 % loss each way, like the netem
	// "wifi"/"wan" profiles) at a 40 Mbit/s video target: direct and relayed
	// connections run the host's media controller end to end; the splice relay
	// terminates QUIC at the gateway and its browser leg runs NewReno.
	const target = 40_000_000
	type res struct {
		name string
		st   seriesStats
	}
	var results []res
	for _, path := range []string{"direct", "udp relay", "quic splice"} {
		var addr *net.UDPAddr
		var hostEnd *quic.Conn
		switch path {
		case "direct":
			addr = direct
		case "udp relay":
			addr = viaRelay(t)
		case "quic splice":
			gwTLS, gwClientTLS := relayTLS(t)
			gw, err := quic.ListenAddr("127.0.0.1:0", gwTLS, transport.QUICConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer gw.Close()
			addr = gw.Addr().(*net.UDPAddr)
			gwOut := loopback(t, "127.0.0.1") // the gateway's data connection to the host
			go func() {
				bc, err := gw.Accept(ctx)
				if err != nil {
					return
				}
				hc, err := gwOut.Dial(ctx, direct, clientTLS, transport.QUICConfig())
				if err != nil {
					return
				}
				relayQUIC(transport.FromQUIC(bc), transport.FromQUIC(hc))
			}()
			link := newLossyLink(t, addr, 20*time.Millisecond, 0.01)
			b, err := loopback(t, "127.0.0.1").Dial(ctx, link.addr(), gwClientTLS, transport.QUICConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer b.CloseWithError(0, "")
			select {
			case hostEnd = <-accepted:
			case <-ctx.Done():
				t.Fatal("no spliced connection at the host")
			}
			// The browser leg's uni stream is opened by the gateway when the
			// host opens one: goodputSeries accepts it on the browser end.
			s, err := goodputSeries(ctx, hostEnd, b, target, 4*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, res{path, summarize(s)})
			continue
		}
		link := newLossyLink(t, addr, 20*time.Millisecond, 0.01)
		b, h := dial(t, link.addr())
		s, err := goodputSeries(ctx, h, b, target, 4*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if link.dropped.Load() == 0 {
			t.Fatal("the lossy link dropped nothing")
		}
		results = append(results, res{path, summarize(s)})
	}
	for _, x := range results {
		t.Logf("40 ms RTT, 1 %% loss, 40 Mbit/s target, %s: goodput %.1f Mbit/s, CV of 100 ms windows %.2f, p10 %.1f Mbit/s",
			x.name, x.st.mean/1e6, x.st.cv, x.st.p10/1e6)
	}
	d, u := results[0].st, results[1].st
	if u.mean < 0.85*d.mean || u.cv > d.cv+0.15 {
		t.Errorf("udp relay %.1f Mbit/s (CV %.2f) vs direct %.1f Mbit/s (CV %.2f): want the direct path's goodput and steadiness",
			u.mean/1e6, u.cv, d.mean/1e6, d.cv)
	}
}

// TestUDPRelayOverrun: a relayed session whose target is far above what its
// path carries, as when the second hardware run's sessions started at the
// 250 Mbit/s setting behind a link of ~100 Mbit/s. The media controller
// paces at 1.2 x the target whatever is lost (cc.Media leaves backing off to
// the session's rate controller), so the link drops two datagrams in three.
// What a session needs of the transport and the relay meanwhile: the
// connection lives and frames still arrive; and once its rate controller
// has brought the target under the link (within a second in the relay
// simulation, at 20a5043 as now: docs/VENDOR_NOTES.md "Merged second
// hardware run: the UDP relay under overload"), the frames are on time
// again within a second: nothing of the overrun (a backlog of
// retransmissions, probe timeouts backed off, the window's brake) outlives
// it. Frames go as the session sends them: one stream each, written in
// turn, cancelled when not sent by their deadline, the older left out when
// two wait.
func TestUDPRelayOverrun(t *testing.T) {
	if testing.Short() {
		t.Skip("about 6 s of streaming")
	}
	const (
		linkRate  = 100e6
		linkQueue = 50 * time.Millisecond
		over      = 250_000_000 // the target first: 300 Mbit/s of pacing
		under     = 60_000_000  // the rate controller's answer: 72 Mbit/s
		fps       = 60
		overFor   = 2 * time.Second
		recovery  = time.Second // after the target is lowered
		judged    = 2 * time.Second
		deadline  = 100 * time.Millisecond // a frame not sent by then is cancelled
		onTime    = 100 * time.Millisecond // made to complete at the browser
	)
	serverTLS, clientTLS := relayTLS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := newRelayHost(t, serverTLS, transport.QUICConfig(transport.WithCongestion(transport.CongestionMedia)))
	r := newTestRelay(t, 1)
	a := allocate(t, r, "u")
	if err := host.bind(t, a, a.token); err != nil {
		t.Fatal(err)
	}
	link := newShapedLink(t, a.addr(), linkRate, linkQueue)
	accepted := make(chan *quic.Conn, 1)
	go func() {
		if c, err := host.ln.Accept(ctx); err == nil {
			accepted <- c
		}
	}()
	b, err := loopback(t, "127.0.0.1").Dial(ctx, link.addr(), clientTLS, transport.QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseWithError(0, "")
	var h *quic.Conn
	select {
	case h = <-accepted:
	case <-ctx.Done():
		t.Fatal("no connection at the host")
	}
	m := transport.MediaControl(transport.FromQUIC(h))
	if m == nil {
		t.Fatal("no media controller")
	}

	// The browser reads each frame's stream to its end. A frame is its
	// number and when it was made (8 bytes each), then padding.
	var mu sync.Mutex
	done := map[int]time.Time{}
	go func() {
		for {
			s, err := b.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				p, err := io.ReadAll(s)
				if err != nil || len(p) < 16 {
					return // cancelled
				}
				mu.Lock()
				done[int(binary.BigEndian.Uint64(p))] = time.Now()
				mu.Unlock()
			}()
		}
	}()

	// The host: a frame of target / fps each frame interval into a queue
	// of two, written in turn.
	type frame struct {
		n    int
		made time.Time
		size int
	}
	start := time.Now()
	lowered := start.Add(overFor)
	end := lowered.Add(recovery + judged)
	queue := make(chan frame, 2)
	var made []time.Time // by frame number
	var cancelled, left atomic.Int64
	go func() {
		defer close(queue)
		tk := time.NewTicker(time.Second / fps)
		defer tk.Stop()
		target := int64(over)
		m.SetTarget(target, time.Second/fps)
		for n := 0; ; n++ {
			now := time.Now()
			if !now.Before(end) {
				return
			}
			if target == over && !now.Before(lowered) {
				target = under
				m.SetTarget(target, time.Second/fps)
			}
			mu.Lock()
			made = append(made, now)
			mu.Unlock()
			f := frame{n, now, int(target / 8 / fps)}
			select {
			case queue <- f:
			default:
				select {
				case <-queue:
					left.Add(1)
				default:
				}
				queue <- f
			}
			<-tk.C
		}
	}()
	buf := make([]byte, over/8/fps)
	for f := range queue {
		st, err := h.OpenUniStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := buf[:f.size]
		binary.BigEndian.PutUint64(p, uint64(f.n))
		binary.BigEndian.PutUint64(p[8:], uint64(f.made.UnixNano()))
		_ = st.SetWriteDeadline(f.made.Add(deadline))
		if _, err := st.Write(p); err != nil {
			st.CancelWrite(0)
			cancelled.Add(1)
			continue
		}
		st.Close()
	}
	time.Sleep(onTime) // the last frames' way
	if err := context.Cause(b.Context()); err != nil {
		t.Fatalf("the connection ended: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// span: frames made in [from, to) that completed, of how many, their
	// latencies (made to complete) and the longest wait between two
	// completions in order of making.
	type span struct {
		ok, of int
		lat    []time.Duration
	}
	judge := func(from, to time.Time) (s span) {
		for n, at := range made {
			if at.Before(from) || !at.Before(to) {
				continue
			}
			s.of++
			if d, ok := done[n]; ok {
				s.lat = append(s.lat, d.Sub(at))
				if d.Sub(at) <= onTime {
					s.ok++
				}
			}
		}
		slices.Sort(s.lat)
		return s
	}
	p := func(v []time.Duration, q float64) time.Duration {
		if len(v) == 0 {
			return -1
		}
		return v[min(len(v)-1, int(q*float64(len(v))))].Round(time.Millisecond)
	}
	// Recovered: the first frame made after the target was lowered that
	// completed on time with the half second of frames after it.
	recovered := time.Duration(-1)
	for n := range made {
		if made[n].Before(lowered) {
			continue
		}
		ok := n+fps/2 <= len(made)
		for k := n; ok && k < n+fps/2; k++ {
			d, in := done[k]
			ok = in && d.Sub(made[k]) <= onTime
		}
		if ok {
			recovered = made[n].Sub(lowered).Round(time.Millisecond)
			break
		}
	}
	ov, after := judge(start, lowered), judge(lowered.Add(recovery), end)
	st := m.Stats()
	t.Logf("overrun (%d Mbit/s into %.0f Mbit/s): %d of %d frames completed, latency p50/p95 %v/%v; target lowered to %d Mbit/s: on time again after %v; "+
		"from %v on %d of %d on time, latency p50/p95 %v/%v; %d frames cancelled, %d left out; the link's queue dropped %d datagrams; "+
		"the relay forwarded %d to the browser and dropped %d (%d by the kernel); the host lost %d of %d packets, %d collapses",
		over/1_000_000, linkRate/1e6, len(ov.lat), ov.of, p(ov.lat, .5), p(ov.lat, .95), under/1_000_000, recovered,
		recovery, after.ok, after.of, p(after.lat, .5), p(after.lat, .95), cancelled.Load(), left.Load(), link.overflowed.Load(),
		a.stats.PktToBrowser.Load(), a.stats.Dropped.Load(), a.stats.SocketDrops.Load(), st.LostPackets, st.LostPackets+st.AckedPackets, st.Collapses)
	if len(ov.lat) < ov.of/10 {
		t.Errorf("during the overrun %d of %d frames completed: want frames to keep arriving (a tenth at least)", len(ov.lat), ov.of)
	}
	if after.ok < after.of*9/10 {
		t.Errorf("%v after the target was lowered under the link %d of %d frames completed within %v: want 90 %%", recovery, after.ok, after.of, onTime)
	}
}

// TestUDPRelayFlood: a host that sends far past the forwarder's limit
// (relayDownRate, after the burst its token bucket allows) has the excess
// dropped, by the forwarder or by the kernel at the allocation's socket
// while the forwarder cannot keep up, and the rest forwarded; once the
// flood ends the forwarder is current again within the time it takes to
// read what its socket buffer held: nothing stalls or wedges
// (docs/VENDOR_NOTES.md "Merged second hardware run: the UDP relay under
// overload").
func TestUDPRelayFlood(t *testing.T) {
	r := newTestRelay(t, 1)
	a := allocate(t, r, "u")
	udp := func() *net.UDPConn {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadBuffer(8 << 20)
		t.Cleanup(func() { c.Close() })
		return c
	}
	host, browser := udp(), udp()
	buf := make([]byte, 2048)
	for bound := false; !bound; {
		host.WriteTo(proto.RelayBindPacket(a.token), a.addr())
		host.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _, err := host.ReadFrom(buf)
		bound = err == nil && proto.IsRelayBound(buf[:n])
		if time.Since(a.created) > 2*time.Second {
			t.Fatal("no bind")
		}
	}
	initial := make([]byte, 1200)
	initial[0] = 0xc3
	copy(initial[1:], []byte{0, 0, 0, 1})
	browser.WriteTo(initial, a.addr())
	host.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := host.ReadFrom(buf); err != nil || !a.locked.Load() {
		t.Fatalf("the Initial was not forwarded (%v) or did not lock the allocation", err)
	}

	// The browser counts the flood's datagrams and when the last came, and
	// notes the probes: datagrams that start with 'P', then their number
	// and when they were sent.
	const probes = 200
	var mu sync.Mutex
	var flooded int
	var lastFlood time.Time
	lat := map[int]time.Duration{}
	go func() {
		b := make([]byte, 2048)
		for {
			n, _, err := browser.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			mu.Lock()
			if n >= 17 && b[0] == 'P' {
				lat[int(binary.BigEndian.Uint64(b[1:]))] = time.Since(time.Unix(0, int64(binary.BigEndian.Uint64(b[9:]))))
			} else {
				flooded++
				lastFlood = time.Now()
			}
			mu.Unlock()
		}
	}()
	// The flood: 300 ms of datagrams as fast as the host's socket takes
	// them (1.7-2.2 Gbit/s on the sandbox's loopback: past the bucket's
	// 8 MB within 50 ms, then at about twice its rate).
	pkt := make([]byte, 1200)
	pkt[0] = 0x40
	sent := 0
	t0 := time.Now()
	for time.Since(t0) < 300*time.Millisecond {
		for range 64 {
			if _, err := host.WriteTo(pkt, a.addr()); err == nil {
				sent++
			}
		}
	}
	flood := time.Since(t0)
	// Probes from 100 ms after the flood, one a millisecond.
	time.Sleep(100 * time.Millisecond)
	for i := range probes {
		p := make([]byte, 1200)
		p[0] = 'P'
		binary.BigEndian.PutUint64(p[1:], uint64(i))
		binary.BigEndian.PutUint64(p[9:], uint64(time.Now().UnixNano()))
		host.WriteTo(p, a.addr())
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	var l []time.Duration
	for _, d := range lat {
		l = append(l, d)
	}
	through, last := flooded, lastFlood.Sub(t0)
	mu.Unlock()
	slices.Sort(l)
	p95 := time.Duration(-1)
	if len(l) > 0 {
		p95 = l[len(l)*95/100]
	}
	t.Logf("flood: %d datagrams in %v (%.0f Mbit/s); the relay forwarded %d of them in %v (%.0f Mbit/s) and dropped %d (%d more by the kernel); "+
		"%d of %d probes after it arrived, latency p95 %v",
		sent, flood.Round(time.Millisecond), float64(sent*len(pkt)*8)/flood.Seconds()/1e6, through, last.Round(time.Millisecond),
		float64(through*len(pkt)*8)/last.Seconds()/1e6, a.stats.Dropped.Load(), a.stats.SocketDrops.Load(), len(l), probes, p95)
	if limit := relayDownBurst + relayDownRate*last.Seconds() + 64<<10; float64(through*len(pkt)) > limit {
		t.Errorf("the relay forwarded %d bytes in %v: want no more than its limit, %.0f", through*len(pkt), last, limit)
	}
	if len(l) < probes*95/100 || p95 > 50*time.Millisecond {
		t.Errorf("after the flood %d of %d probes arrived, latency p95 %v: want 95 %% within 50 ms", len(l), probes, p95)
	}
}
