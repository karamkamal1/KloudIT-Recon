package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

func TestUDPRelayLimits(t *testing.T) {
	r := newTestRelay(t, relayMaxPending+2)
	for i := 0; i < relayMaxPending; i++ {
		allocate(t, r, "alice")
	}
	if _, err := r.allocate(allocRequest{user: "alice"}); !errors.Is(err, errRelayPending) {
		t.Fatalf("pending allocations per user not limited: %v", err)
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

// lossyLink sits in front of server for one client: delay each way, random loss.
type lossyLink struct {
	pc, up  *net.UDPConn
	client  atomic.Pointer[net.UDPAddr]
	dropped atomic.Int64
}

func newLossyLink(t *testing.T, server *net.UDPAddr, delay time.Duration, loss float64) *lossyLink {
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
	pipe := func(seed uint64, read func([]byte) (int, error), write func([]byte)) {
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
			select {
			case q <- pkt{time.Now().Add(delay), b[:n]}:
			default:
			}
		}
	}
	go pipe(1, func(b []byte) (int, error) {
		n, a, err := pc.ReadFromUDP(b)
		if err == nil {
			l.client.Store(a)
		}
		return n, err
	}, func(b []byte) { up.Write(b) })
	go pipe(2, up.Read, func(b []byte) {
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
// the first second.
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
	for {
		n, err := s.Read(buf)
		if k := int(time.Since(start) / bucket); k < len(counts) {
			counts[k] += int64(n)
		}
		if err != nil || time.Since(start) > d {
			break
		}
	}
	s.CancelRead(0)
	<-errc
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
	viaRelay := func(t *testing.T) *net.UDPAddr {
		a := allocate(t, r, "u")
		if err := host.bind(t, a, a.token); err != nil {
			t.Fatal(err)
		}
		return a.addr()
	}
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

	// 2. Bulk throughput on clean loopback (NewReno both ways is not involved:
	// the host runs media at a target far above what loopback carries).
	const fast = 2_000_000_000
	for _, path := range []string{"direct", "udp relay"} {
		addr := direct
		if path != "direct" {
			addr = viaRelay(t)
		}
		b, h := dial(t, addr)
		s, err := goodputSeries(ctx, h, b, fast, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		st := summarize(s)
		t.Logf("loopback bulk, %s: %.0f Mbit/s", path, st.mean/1e6)
		if st.mean < 50e6 {
			t.Errorf("%s: %.0f Mbit/s on loopback", path, st.mean/1e6)
		}
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
