//go:build linux

package gateway

import (
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// pktConn is an allocation socket. On a socket bound to all addresses it
// learns the local address each datagram arrived at (IP_PKTINFO /
// IPV6_PKTINFO) and answers from that address, as quic-go does on the main
// port: a peer drops answers from any other address of a multi-homed gateway
// (a second IPv4 address, several IPv6 addresses). DF is set like on quic-go's
// sockets, so a datagram that does not fit the next link is dropped instead of
// fragmented and both QUIC ends see the real path MTU.
type pktConn struct {
	c       *net.UDPConn
	pktinfo bool
	oob     []byte
}

func newPktConn(c *net.UDPConn) *pktConn {
	p := &pktConn{c: c}
	la, _ := c.LocalAddr().(*net.UDPAddr)
	want := la != nil && la.IP.IsUnspecified()
	if rc, err := c.SyscallConn(); err == nil {
		var err4, err6 error
		_ = rc.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE)
			if want {
				err4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1)
				err6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
			}
		})
		p.pktinfo = want && (err4 == nil || err6 == nil)
	}
	if p.pktinfo {
		p.oob = make([]byte, 128)
	}
	return p
}

// read returns a datagram, its sender and (with pktinfo) the local address it
// was sent to.
func (p *pktConn) read(b []byte) (int, netip.AddrPort, netip.Addr, error) {
	if !p.pktinfo {
		n, from, err := p.c.ReadFromUDPAddrPort(b)
		return n, unmapAddrPort(from), netip.Addr{}, err
	}
	n, oobn, _, from, err := p.c.ReadMsgUDPAddrPort(b, p.oob)
	var local netip.Addr
	for rest := p.oob[:oobn]; err == nil && len(rest) > 0; {
		h, data, r, perr := unix.ParseOneSocketControlMessage(rest)
		if perr != nil {
			break
		}
		switch {
		case h.Level == unix.IPPROTO_IP && h.Type == unix.IP_PKTINFO && len(data) >= 12:
			local = netip.AddrFrom4([4]byte(data[8:12])) // in_pktinfo.ipi_addr
		case h.Level == unix.IPPROTO_IPV6 && h.Type == unix.IPV6_PKTINFO && len(data) >= 16:
			local = netip.AddrFrom16([16]byte(data[:16])).Unmap()
		}
		rest = r
	}
	return n, unmapAddrPort(from), local, err
}

// sourceOOB returns the control message that sends from local ("" without pktinfo).
func (p *pktConn) sourceOOB(local netip.Addr) []byte {
	if !p.pktinfo || !local.IsValid() {
		return nil
	}
	if local.Is4() {
		return unix.PktInfo4(&unix.Inet4Pktinfo{Spec_dst: local.As4()})
	}
	return unix.PktInfo6(&unix.Inet6Pktinfo{Addr: local.As16()})
}

func (p *pktConn) write(b []byte, to netip.AddrPort, oob []byte) error {
	if len(oob) == 0 {
		_, err := p.c.WriteToUDPAddrPort(b, to)
		return err
	}
	_, _, err := p.c.WriteMsgUDPAddrPort(b, oob, to)
	return err
}

func unmapAddrPort(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}
