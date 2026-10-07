package gateway

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// magicPacket builds a Wake-on-LAN packet: 6 x 0xFF then the MAC 16 times.
func magicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(mac))
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("invalid MAC address %q", mac)
	}
	p := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		p = append(p, 0xFF)
	}
	for i := 0; i < 16; i++ {
		p = append(p, hw...)
	}
	return p, nil
}

// broadcastAddrs returns the directed broadcast address of every local IPv4
// network plus the limited broadcast address.
func broadcastAddrs() []string {
	out := []string{"255.255.255.255"}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != 4 {
				continue
			}
			b := make(net.IP, 4)
			for i := range b {
				b[i] = ip4[i] | ^ipn.Mask[i]
			}
			out = append(out, b.String())
		}
	}
	return out
}

// Wake sends magic packets for every MAC to every broadcast address on UDP 7 and 9.
func Wake(macs []string, extraBroadcast string) error {
	if len(macs) == 0 {
		return errors.New("no MAC address known for this host yet (it must connect to the gateway once)")
	}
	targets := broadcastAddrs()
	if extraBroadcast != "" {
		targets = append(targets, extraBroadcast)
	}
	var sent int
	var lastErr error
	for _, mac := range macs {
		pkt, err := magicPacket(mac)
		if err != nil {
			lastErr = err
			continue
		}
		for _, t := range targets {
			for _, port := range []string{"9", "7"} {
				conn, err := net.Dial("udp4", net.JoinHostPort(t, port))
				if err != nil {
					lastErr = err
					continue
				}
				if _, err := conn.Write(pkt); err != nil {
					lastErr = err
				} else {
					sent++
				}
				conn.Close()
			}
		}
	}
	if sent == 0 {
		return fmt.Errorf("could not send wake packet: %v", lastErr)
	}
	return nil
}
