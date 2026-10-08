//go:build linux

package gateway

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// An allocation socket bound to all addresses (the default -listen :8443)
// answers each peer from the address that peer sent to, even when the routing
// table would pick another one: here the browser (127.0.0.1) sends to the
// gateway's other address, whose answers would otherwise leave from 127.0.0.1.
func TestUDPRelayAnswersFromTheTargetedAddress(t *testing.T) {
	var other netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			other = netip.AddrFrom4([4]byte(n.IP.To4()))
			break
		}
	}
	if !other.IsValid() {
		t.Skip("no non-loopback IPv4 address")
	}
	r := newUDPRelay(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, freeUDPPorts(t, 1))
	t.Cleanup(r.closeAll)
	a, err := r.allocate(allocRequest{user: "u", clientIP: netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	if !a.pc.pktinfo {
		t.Fatal("no packet info on a socket bound to all addresses")
	}
	lo := netip.MustParseAddr("127.0.0.1")
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	read := func(c *net.UDPConn) ([]byte, netip.AddrPort) {
		t.Helper()
		buf := make([]byte, 2048)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	}
	port := uint16(a.port)
	host, browser := listen(), listen()
	host.WriteToUDPAddrPort(proto.RelayBindPacket(a.token), netip.AddrPortFrom(lo, port))
	if b, from := read(host); !proto.IsRelayBound(b) || from != netip.AddrPortFrom(lo, port) {
		t.Fatalf("bind answered with %x from %v", b, from)
	}
	initial := make([]byte, 1200)
	initial[0] = 0xc3
	copy(initial[1:], []byte{0, 0, 0, 1})
	browser.WriteToUDPAddrPort(initial, netip.AddrPortFrom(other, port))
	if b, from := read(host); len(b) != 1200 || from != netip.AddrPortFrom(lo, port) {
		t.Fatalf("host got %d bytes from %v", len(b), from)
	}
	answer := append([]byte{0x41}, make([]byte, 40)...)
	host.WriteToUDPAddrPort(answer, netip.AddrPortFrom(lo, port))
	if b, from := read(browser); len(b) != len(answer) || from != netip.AddrPortFrom(other, port) {
		t.Fatalf("browser got %d bytes from %v, want them from %v", len(b), from, netip.AddrPortFrom(other, port))
	}
}
