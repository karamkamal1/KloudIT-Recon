//go:build !linux

package gateway

import (
	"net"
	"net/netip"
)

// pktConn is an allocation socket. Elsewhere than on Linux (the gateway's
// platform) answers leave from the address the routing table picks, which is
// right unless the gateway has several addresses towards a peer.
type pktConn struct{ c *net.UDPConn }

func newPktConn(c *net.UDPConn) *pktConn { return &pktConn{c: c} }

func (p *pktConn) read(b []byte) (int, netip.AddrPort, netip.Addr, error) {
	n, from, err := p.c.ReadFromUDPAddrPort(b)
	return n, netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), netip.Addr{}, err
}

func (p *pktConn) sourceOOB(netip.Addr) []byte { return nil }

func (p *pktConn) write(b []byte, to netip.AddrPort, _ []byte) error {
	_, err := p.c.WriteToUDPAddrPort(b, to)
	return err
}
