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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

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
	got := directAddrs(ifs, netip.MustParseAddr("192.168.5.10"))
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
	got = directAddrs(ifs, netip.MustParseAddr("100.101.102.103"))
	if got[0].IP != "192.168.5.10" || got[0].Route || got[3].IP != "100.101.102.103" || !got[3].Route {
		t.Fatalf("with the route through Tailscale: %+v", got)
	}
	if txt := directAddrText(got[:1]); txt != "192.168.5.10 (Ethernet 2, lan)" {
		t.Fatalf("log text %q", txt)
	}
	// At most maxDirectAddrs.
	var many []netip.Prefix
	for i := 1; i <= 12; i++ {
		many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), 1}), 24))
	}
	if n := len(directAddrs([]ifaceInfo{{Name: "eth0", Up: true, Addrs: many}}, netip.Addr{})); n != maxDirectAddrs {
		t.Fatalf("%d addresses, want %d", n, maxDirectAddrs)
	}
	if o := directOverrides(" pc.example.net, 192.168.5.10;[2001:db8::1]  pc.example.net "); strings.Join(o, "|") != "pc.example.net|192.168.5.10|2001:db8::1" {
		t.Fatalf("directOverrides %q", o)
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

	// A connection the client completes and closes is no failure.
	before := strings.Count(logs.String(), "failed in its handshake")
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	c, err := quic.DialAddr(ctx, target.String(), accept, transport.QUICConfig())
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	c.CloseWithError(0, "")
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(logs.String(), "failed in its handshake"); n != before {
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
		{&quic.TransportError{Remote: true, ErrorCode: quic.NoError}, 2600 * time.Millisecond, "gave up waiting", false},
		{&quic.TransportError{Remote: true, ErrorCode: quic.ProtocolViolation}, time.Second, "the browser closed it", false},
	} {
		why, expected := directHandshakeWhy(tc.err, tc.after)
		if !strings.Contains(why, tc.want) || expected != tc.expected {
			t.Errorf("%v after %s: %q (expected %v), want %q (%v)", tc.err, tc.after, why, expected, tc.want, tc.expected)
		}
	}
}
