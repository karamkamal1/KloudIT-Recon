package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// lockedBuffer is a log sink the agent's goroutines write concurrently.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// The owner's PC (second hardware run): "Ethernet 2" on the LAN with IPv6,
// Tailscale, and the virtual switches a gaming PC often has. The direct
// endpoint listens on (and the gateway offers) the LAN first, then IPv6,
// Tailscale, VPNs and virtual switches; never loopback, link-local or Teredo
// addresses, nor interfaces that are down.
func TestDirectAddrs(t *testing.T) {
	ifs := []ifaceInfo{
		{Name: "Loopback Pseudo-Interface 1", Up: true, Loopback: true, Addrs: []netip.Prefix{pfx("127.0.0.1/8"), pfx("::1/128")}},
		{Name: "vEthernet (WSL)", Up: true, Addrs: []netip.Prefix{pfx("172.20.16.1/20"), pfx("fe80::1/64")}},
		{Name: "Tailscale", Up: true, Addrs: []netip.Prefix{pfx("100.101.102.103/32"), pfx("fd7a:115c:a1e0::1/128")}},
		{Name: "Ethernet 2", Up: true, Addrs: []netip.Prefix{
			pfx("fe80::abcd/64"), pfx("2001:db8:5::10/64"), pfx("2001:db8:5::beef/64"), pfx("2001:db8:5::cafe/64"), pfx("192.168.5.10/24"),
			pfx("169.254.1.2/16"),
		}},
		{Name: "Teredo Tunneling Pseudo-Interface", Up: true, Addrs: []netip.Prefix{pfx("2001:0:1:2::3/64")}},
		{Name: "WireGuard Tunnel", Up: true, Addrs: []netip.Prefix{pfx("10.8.0.2/24")}},
		{Name: "Ethernet 3", Up: false, Addrs: []netip.Prefix{pfx("192.168.9.9/24")}},
	}
	gw := netip.MustParseAddr("192.168.5.202")
	all := directAddrs(ifs, netip.MustParseAddr("192.168.5.10"), gw)
	if len(all) != 8 || all[3].IP != "2001:db8:5::cafe" {
		t.Fatalf("every address the endpoint listens on: %+v", all)
	}
	got := directAnnounced(all)
	var s []string
	for _, a := range got {
		s = append(s, a.IP+"/"+a.Kind)
	}
	want := "192.168.5.10/lan 2001:db8:5::10/ipv6 2001:db8:5::beef/ipv6 100.101.102.103/tailscale fd7a:115c:a1e0::1/tailscale 10.8.0.2/vpn 172.20.16.1/virtual"
	if strings.Join(s, " ") != want {
		t.Fatalf("addresses %q, want %q", strings.Join(s, " "), want)
	}
	if !got[0].Route || got[0].Bits != 24 || got[0].Iface != "Ethernet 2" || got[1].Route {
		t.Fatalf("first addresses %+v", got[:2])
	}
	// The route to the gateway goes first among its kind: a PC whose
	// Tailscale subnet route covers the LAN reaches the gateway from its
	// tailnet address, which still comes after the LAN.
	got = directAnnounced(directAddrs(ifs, netip.MustParseAddr("100.101.102.103"), gw))
	if got[0].IP != "192.168.5.10" || got[0].Route || got[3].IP != "100.101.102.103" || !got[3].Route {
		t.Fatalf("with the route through Tailscale: %+v", got)
	}
	// That route is a tunnel although the gateway is on the LAN: warned.
	if w := tunnelledLAN(got, netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("192.168.5.202")); w !=
		"from 100.101.102.103 (Tailscale) instead of 192.168.5.10 on 192.168.5.0/24 (Ethernet 2)" {
		t.Fatalf("tunnelled route: %q", w)
	}
	if w := tunnelledLAN(got, netip.MustParseAddr("192.168.5.10"), netip.MustParseAddr("192.168.5.202")); w != "" {
		t.Fatalf("the LAN route warned: %q", w)
	}
	if w := tunnelledLAN(got, netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("203.0.113.7")); w != "" {
		t.Fatalf("a gateway away from the LAN warned: %q", w)
	}
	if txt := directAddrText(got[:1]); txt != "192.168.5.10 (Ethernet 2, lan)" {
		t.Fatalf("log text %q", txt)
	}
	// At most maxDirectAddrs.
	var many []netip.Prefix
	for i := 1; i <= 12; i++ {
		many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), 1}), 24))
	}
	if n := len(directAnnounced(directAddrs([]ifaceInfo{{Name: "eth0", Up: true, Addrs: many}}, netip.Addr{}, netip.Addr{}))); n != maxDirectAddrs {
		t.Fatalf("%d addresses, want %d", n, maxDirectAddrs)
	}
	if o := directOverrides(" pc.example.net, 192.168.5.10;[2001:db8::1]  pc.example.net "); strings.Join(o, "|") != "pc.example.net|192.168.5.10|2001:db8::1" {
		t.Fatalf("directOverrides %q", o)
	}
}

// The caps on what the gateway offers never drop the PC's route to the
// gateway (review of the second hardware run): Windows lists its current
// temporary IPv6 address, the one it connects from, after a stable and a
// deprecated one, and the PC's route may be on an interface the total cap
// cuts. A Hyper-V external switch ("vEthernet (...)") that carries the PC's
// LAN address is the LAN, not a virtual switch; WSL's is not.
func TestDirectAddrsRouteAndCaps(t *testing.T) {
	names := func(as []proto.DirectAddr) string {
		var s []string
		for _, a := range as {
			x := a.IP + "/" + a.Kind
			if a.Route {
				x += "/route"
			}
			s = append(s, x)
		}
		return strings.Join(s, " ")
	}
	eth := []ifaceInfo{{Name: "Ethernet 2", Up: true, Addrs: []netip.Prefix{
		pfx("192.168.5.10/24"), pfx("2001:db8:5::10/64"), pfx("2001:db8:5::aaaa/64"), pfx("2001:db8:5::bbbb/64"),
	}}}
	route := netip.MustParseAddr("2001:db8:5::bbbb")
	all := directAddrs(eth, route, netip.MustParseAddr("2001:db8:5::1"))
	if got, want := names(all), "192.168.5.10/lan 2001:db8:5::bbbb/ipv6/route 2001:db8:5::10/ipv6 2001:db8:5::aaaa/ipv6"; got != want {
		t.Fatalf("listened on %q, want %q", got, want)
	}
	if got, want := names(directAnnounced(all)), "192.168.5.10/lan 2001:db8:5::bbbb/ipv6/route 2001:db8:5::10/ipv6"; got != want {
		t.Fatalf("announced %q, want %q", got, want)
	}

	// The total cap: twelve LAN addresses and the route on a VPN.
	var many []netip.Prefix
	for i := 1; i <= 12; i++ {
		many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), 1}), 24))
	}
	ifs := []ifaceInfo{{Name: "eth0", Up: true, Addrs: many}, {Name: "wg0", Up: true, Addrs: []netip.Prefix{pfx("10.8.0.2/24")}}}
	all = directAddrs(ifs, netip.MustParseAddr("10.8.0.2"), netip.Addr{})
	got := directAnnounced(all)
	if len(all) != 13 || len(got) != maxDirectAddrs || got[len(got)-1].IP != "10.8.0.2" || !got[len(got)-1].Route {
		t.Fatalf("with the route beyond the cap: %d listened on, announced %q", len(all), names(got))
	}

	// Hyper-V's external switch carries the LAN: by the route, or by the
	// gateway's network.
	hv := []ifaceInfo{
		{Name: "vEthernet (External)", Up: true, Addrs: []netip.Prefix{pfx("192.168.5.10/24")}},
		{Name: "vEthernet (WSL)", Up: true, Addrs: []netip.Prefix{pfx("172.20.16.1/20")}},
	}
	if got, want := names(directAddrs(hv, netip.MustParseAddr("192.168.5.10"), netip.Addr{})), "192.168.5.10/lan/route 172.20.16.1/virtual"; got != want {
		t.Fatalf("Hyper-V external switch with the route: %q, want %q", got, want)
	}
	if got, want := names(directAddrs(hv, netip.Addr{}, netip.MustParseAddr("192.168.5.202"))), "192.168.5.10/lan 172.20.16.1/virtual"; got != want {
		t.Fatalf("Hyper-V external switch on the gateway's network: %q, want %q", got, want)
	}
}

// A rescan keeps the socket of every address the PC still has, also one
// the caps leave out of what it announces (a new temporary IPv6 address
// listed ahead of it): a live direct session may be on it. Only an address
// gone from the interfaces loses its socket.
func TestDirectScanKeepsPresentSockets(t *testing.T) {
	ifc := ifaceInfo{Name: "Ethernet 2", Up: true, Addrs: []netip.Prefix{
		pfx("192.168.5.10/24"), pfx("2001:db8:5::10/64"), pfx("2001:db8:5::aaaa/64"), pfx("2001:db8:5::bbbb/64"),
	}}
	defer func(f func() ([]ifaceInfo, error)) { listInterfaces = f }(listInterfaces)
	listInterfaces = func() ([]ifaceInfo, error) { return []ifaceInfo{ifc}, nil }
	// The sockets: real ones on loopback, one per address asked for.
	defer func(f func(*directEndpoint, context.Context, netip.AddrPort, *tls.Config) (*directSock, error)) {
		directListen = f
	}(directListen)
	directListen = func(d *directEndpoint, ctx context.Context, ap netip.AddrPort, c *tls.Config) (*directSock, error) {
		return d.listen(ctx, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0), c)
	}
	rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: &Config{DirectPort: 48100}, log: slog.New(slog.NewTextHandler(&lockedBuffer{}, nil))}
	d := &directEndpoint{a: a, srv: &webtransport.Server{H3: &http3.Server{}}, socks: map[netip.AddrPort]*directSock{}}
	defer d.closeAll()
	tlsConf := http3.ConfigureTLSConfig(&tls.Config{GetCertificate: rot.GetCertificate, MinVersion: tls.VersionTLS13})
	ap := func(ip string) netip.AddrPort { return netip.AddrPortFrom(netip.MustParseAddr(ip), 48100) }
	ctx := context.Background()

	if held, _, err := d.scan(ctx, tlsConf); held != 4 || err != nil {
		t.Fatalf("first scan: %d sockets (%v), want one per address", held, err)
	}
	if txt := directAddrText(d.announced()); txt != "192.168.5.10 (Ethernet 2, lan); 2001:db8:5::10 (Ethernet 2, ipv6); 2001:db8:5::aaaa (Ethernet 2, ipv6)" {
		t.Fatalf("announced %q", txt)
	}
	kept := d.socks[ap("2001:db8:5::aaaa")]

	// A new temporary address listed first pushes ::aaaa out of the two
	// IPv6 addresses announced: its socket stays.
	ifc.Addrs = []netip.Prefix{pfx("192.168.5.10/24"), pfx("2001:db8:5::cccc/64"), pfx("2001:db8:5::10/64"), pfx("2001:db8:5::aaaa/64"), pfx("2001:db8:5::bbbb/64")}
	held, changed, _ := d.scan(ctx, tlsConf)
	if held != 5 || !changed || d.socks[ap("2001:db8:5::aaaa")] != kept || slices.ContainsFunc(d.announced(), func(x proto.DirectAddr) bool { return x.IP == "2001:db8:5::aaaa" }) {
		t.Fatalf("after a new address: %d sockets, changed %v, ::aaaa's socket kept %v, announced %q", held, changed,
			d.socks[ap("2001:db8:5::aaaa")] == kept, directAddrText(d.announced()))
	}
	select {
	case <-kept.done:
		t.Fatal("the socket of an address still present was closed")
	default:
	}

	// ::aaaa gone: its socket goes.
	ifc.Addrs = []netip.Prefix{pfx("192.168.5.10/24"), pfx("2001:db8:5::cccc/64"), pfx("2001:db8:5::10/64"), pfx("2001:db8:5::bbbb/64")}
	if held, _, _ := d.scan(ctx, tlsConf); held != 4 || d.socks[ap("2001:db8:5::aaaa")] != nil {
		t.Fatalf("after ::aaaa went: %d sockets", held)
	}
	select {
	case <-kept.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the socket of an address that went is still open")
	}
}

// directInfo announces host config directAddr first (its first entry as
// Addr, for gateways before Addrs) and then the addresses the endpoint
// listens on.
func TestDirectInfoAddrs(t *testing.T) {
	rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: &Config{DirectPort: 48100, DirectAddr: "pc.example.net, 192.168.5.10"}, directRot: rot,
		direct: &directEndpoint{addrs: []proto.DirectAddr{{IP: "192.168.5.10", Kind: proto.DirectKindLAN}, {IP: "100.101.102.103", Kind: proto.DirectKindTailscale}}}}
	a.directUp.Store(true)
	d := a.directInfo()
	// 192.168.5.10 once, as directAddr.
	if d.Addr != "pc.example.net" || len(d.Addrs) != 2 || d.Addrs[0].IP != "192.168.5.10" || d.Addrs[0].Kind != proto.DirectKindOverride ||
		d.Addrs[1].IP != "100.101.102.103" || d.Addrs[1].Kind != proto.DirectKindTailscale || d.Port != 48100 || len(d.Hashes) == 0 {
		t.Fatalf("direct info %+v", d)
	}
}

// directEndpointForTest runs the direct endpoint on 127.0.0.1 (host config
// directAddr; no interfaces) and returns its port and the agent's log.
func directEndpointForTest(t *testing.T) (*Agent, int, *lockedBuffer) {
	t.Helper()
	defer func(f func() ([]ifaceInfo, error)) { listInterfaces = f }(listInterfaces)
	listInterfaces = func() ([]ifaceInfo, error) { return nil, nil }
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	a := &Agent{cfg: &Config{DirectPort: port, DirectAddr: "127.0.0.1"}, log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		directRot: rot, tunnel: &ctrlRecorder{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.runDirect(ctx, rot) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); a.directInfo() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the direct endpoint did not come up")
		}
	}
	return a, port, logs
}

// waitLog waits for a log line with every one of subs.
func waitLog(t *testing.T, logs *lockedBuffer, d time.Duration, subs ...string) string {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		for _, l := range strings.Split(logs.String(), "\n") {
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(l, s)
			}
			if ok {
				return l
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q in:\n%s", subs, logs.String())
		}
	}
}

// The host log says why a browser's handshake on the direct path failed,
// which the browser itself cannot see: a certificate the browser refuses (a
// hash that does not match), or answers that never reach it (they leave by
// another route). A browser that drops a parallel attempt another address
// won is no fault (Debug).
func TestDirectHandshakeDiagnostics(t *testing.T) {
	_, port, logs := directEndpointForTest(t)
	if l := waitLog(t, logs, 5*time.Second, `msg="direct WebTransport endpoint listening"`); !strings.Contains(l, "addrs=") {
		t.Fatalf("listening line %q", l)
	}
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}

	// A certificate the client refuses.
	refuse := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3},
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { return errors.New("hash mismatch") }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := quic.DialAddr(ctx, target.String(), refuse, transport.QUICConfig())
	cancel()
	if err == nil {
		t.Fatal("the handshake with a refused certificate succeeded")
	}
	l := waitLog(t, logs, 5*time.Second, "direct path: a browser's connection failed in its handshake", "refused the PC's certificate")
	if !strings.Contains(l, "level=INFO") || !strings.Contains(l, "to=127.0.0.1") {
		t.Fatalf("certificate refusal logged as %q", l)
	}

	// Answers that never reach the client: a forwarder passes its packets to
	// the PC and drops the PC's (an asymmetric route).
	fwd, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	up, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, _, err := fwd.ReadFrom(b)
			if err != nil {
				return
			}
			up.Write(b[:n])
		}
	}()
	go func() { // the PC's answers: dropped
		b := make([]byte, 2048)
		for {
			if _, err := up.Read(b); errors.Is(err, net.ErrClosed) {
				return
			}
		}
	}()
	accept := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}}
	ctx, cancel = context.WithTimeout(context.Background(), 2500*time.Millisecond)
	_, err = quic.DialAddr(ctx, fwd.LocalAddr().String(), accept, transport.QUICConfig())
	cancel()
	if err == nil {
		t.Fatal("a handshake without answers succeeded")
	}
	l = waitLog(t, logs, 10*time.Second, "direct path: a browser's connection failed in its handshake", "the PC's answers did not reach it")
	if !strings.Contains(l, "level=INFO") {
		t.Fatalf("lost answers logged as %q", l)
	}

	// A connection the client completes and closes is no failure (at most a
	// Debug line where its close overtakes the handshake's completion).
	infoFailures := func() int {
		n := 0
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "level=INFO") && strings.Contains(l, "failed in its handshake") {
				n++
			}
		}
		return n
	}
	before := infoFailures()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	c, err := quic.DialAddr(ctx, target.String(), accept, transport.QUICConfig())
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	c.CloseWithError(0, "")
	time.Sleep(300 * time.Millisecond)
	if n := infoFailures(); n != before {
		t.Fatalf("a completed handshake was logged as failed:\n%s", logs.String())
	}
}

func TestDirectHandshakeWhy(t *testing.T) {
	for _, tc := range []struct {
		err      error
		after    time.Duration
		want     string
		expected bool
	}{
		{&quic.HandshakeTimeoutError{}, 5 * time.Second, "never completed the handshake", false},
		{&quic.IdleTimeoutError{}, 5 * time.Second, "never completed the handshake", false},
		{&quic.TransportError{Remote: true, ErrorCode: quic.TransportErrorCode(0x100 + 42)}, 20 * time.Millisecond, "refused the PC's certificate", false},
		{&quic.ApplicationError{Remote: true}, 30 * time.Millisecond, "another of the PC's addresses answered first", true},
		// An application close in a Handshake packet (RFC 9000 10.2.3).
		{&quic.TransportError{Remote: true, ErrorCode: quic.ApplicationErrorErrorCode}, 10 * time.Millisecond, "another of the PC's addresses answered first", true},
		{&quic.TransportError{Remote: true, ErrorCode: quic.ApplicationErrorErrorCode}, 4 * time.Second, "gave up the handshake", false},
		{&quic.TransportError{Remote: true, ErrorCode: quic.NoError}, 2600 * time.Millisecond, "gave up the handshake: the PC's answers did not reach it", false},
		{&quic.TransportError{Remote: true, ErrorCode: quic.ProtocolViolation}, time.Second, "the browser closed it", false},
	} {
		why, expected := directHandshakeWhy(tc.err, tc.after)
		if !strings.Contains(why, tc.want) || expected != tc.expected {
			t.Errorf("%v after %s: %q (expected %v), want %q (%v)", tc.err, tc.after, why, expected, tc.want, tc.expected)
		}
	}
}

// Failed handshakes cannot fill host.log (review of the second hardware
// run): Info lines only for browsers on the PC's own networks, at most
// directHandshakeBudget per window in all; the rest Debug, counted, and one
// Info line after the window says how many. A burst of connection attempts
// beyond directHandshakeRate a second must prove its source (Retry).
func TestDirectHandshakeLogBudget(t *testing.T) {
	logs := &lockedBuffer{}
	a := &Agent{log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		direct: &directEndpoint{addrs: []proto.DirectAddr{{IP: "2001:db8:5::10", Bits: 64, Kind: proto.DirectKindIPv6}}}}
	var l directHandshakeLog
	fail := func(remote string) {
		l.failed(a, &directHandshake{remote: remote, local: "192.168.5.10", start: time.Now().Add(-5 * time.Second)}, &quic.HandshakeTimeoutError{})
	}
	count := func(level string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "level="+level) && strings.Contains(line, "failed in its handshake") {
				n++
			}
		}
		return n
	}
	// A scan from the internet: Debug only.
	for i := range 50 {
		fail(netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), 5000).String())
	}
	if info := count("INFO"); info != 0 {
		t.Fatalf("%d Info lines for handshakes from away", info)
	}
	// Browsers on the PC's networks: the LAN, the tailnet, the LAN's IPv6
	// prefix. Each once per address and reason, 20 in all.
	for _, r := range []string{"192.168.5.20:5000", "192.168.5.20:5001", "100.64.7.8:5000", "[2001:db8:5::77]:5000", "[fd00::5]:5000"} {
		fail(r)
	}
	if info := count("INFO"); info != 4 {
		t.Fatalf("%d Info lines for four local browsers (one twice):\n%s", info, logs.String())
	}
	for i := range 30 {
		fail(netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, 1, byte(i)}), 5000).String())
	}
	if info := count("INFO"); info != directHandshakeBudget {
		t.Fatalf("%d Info lines, want the budget %d", info, directHandshakeBudget)
	}
	// After the window: one line with the count, then Info lines again.
	l.mu.Lock()
	l.window = l.window.Add(-directHandshakeWindow)
	l.mu.Unlock()
	fail("192.168.5.30:5000")
	// 50 from away, 14 beyond the budget.
	if !strings.Contains(logs.String(), "count=64") || count("INFO") != directHandshakeBudget+1 {
		t.Fatalf("after the window:\n%s", logs.String())
	}

	for i := range directHandshakeRate {
		if l.verifySource(nil) {
			t.Fatalf("attempt %d in a second had to prove its address", i+1)
		}
	}
	if !l.verifySource(nil) {
		t.Fatal("an attempt beyond the rate did not have to prove its address")
	}
}

// A browser refuses the PC's certificate when its own clock is outside the
// certificate's validity, whatever the hash (the gateway passes the agent's
// hashes on as they are): the direct path lines say where the client's clock
// (DirectReport.Clock) stands.
func TestClientClockAttr(t *testing.T) {
	rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{directRot: rot}
	at := func(off time.Duration) string {
		kv := a.clientClockAttr(&proto.DirectReport{Clock: time.Now().Add(off).UnixMilli()})
		if len(kv) == 0 {
			return ""
		}
		return kv[1].(string)
	}
	for _, tc := range []struct {
		off  time.Duration
		want string
	}{
		{-2 * time.Hour, "behind the PC's: before the PC's certificate is valid"},
		{20 * 24 * time.Hour, "ahead of the PC's: after the PC's certificate expires"},
		{-5 * time.Minute, "5m0s behind the PC's"},
		{30 * time.Second, ""},
	} {
		if got := at(tc.off); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("client clock %v off: %q, want %q", tc.off, got, tc.want)
		}
	}
	if kv := a.clientClockAttr(&proto.DirectReport{}); kv != nil {
		t.Errorf("a report without a clock: %v", kv)
	}
}
