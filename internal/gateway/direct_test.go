package gateway

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The connect answer's direct candidates (VENDOR_NOTES "Second hardware run:
// direct path on the LAN"): the PC's address on the browser's own network
// first, then directAddr, the PC's route to the gateway, the tunnel's peer
// address and the rest by kind. The owner's PC: the LAN on "Ethernet 2", an
// IPv6 address, Tailscale; the browser on the LAN, on the tailnet, or away.
func TestOrderDirect(t *testing.T) {
	owner := &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}, Addrs: []proto.DirectAddr{
		{IP: "192.168.5.10", Bits: 24, Kind: proto.DirectKindLAN, Iface: "Ethernet 2", Route: true},
		{IP: "2001:db8:5::10", Bits: 64, Kind: proto.DirectKindIPv6, Iface: "Ethernet 2"},
		{IP: "100.101.102.103", Bits: 32, Kind: proto.DirectKindTailscale, Iface: "Tailscale"},
		{IP: "fd7a:115c:a1e0::1", Bits: 128, Kind: proto.DirectKindTailscale, Iface: "Tailscale"},
		{IP: "172.20.0.1", Bits: 20, Kind: proto.DirectKindVirtual, Iface: "vEthernet (WSL)"},
	}}
	hosts := func(c []directCand) string {
		var s []string
		for _, x := range c {
			s = append(s, x.host)
		}
		return strings.Join(s, " ")
	}
	for _, tc := range []struct {
		name    string
		info    *proto.DirectInfo
		peer    string
		browser string
		want    string
		note    string // the first candidate's note
	}{
		{"browser on the LAN", owner, "192.168.5.10", "192.168.5.20",
			"192.168.5.10 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the browser's network 192.168.5.0/24"},
		{"browser on the LAN over IPv6", owner, "192.168.5.10", "2001:db8:5::77",
			"2001:db8:5::10 192.168.5.10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the browser's network 2001:db8:5::/64"},
		{"browser on the tailnet", owner, "192.168.5.10", "100.64.7.8",
			"100.101.102.103 192.168.5.10 2001:db8:5::10 fd7a:115c:a1e0::1 172.20.0.1", "the browser is on the tailnet too"},
		// The PC reaches the gateway through a Tailscale subnet router: the
		// gateway sees the router's address, which comes after the PC's own.
		{"peer is a subnet router", owner, "192.168.5.2", "192.168.5.20",
			"192.168.5.10 192.168.5.2 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the browser's network 192.168.5.0/24"},
		{"browser away (unknown network)", owner, "203.0.113.9", "198.51.100.4",
			"192.168.5.10 203.0.113.9 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the PC's route to the gateway"},
		{"browser address unknown", owner, "192.168.5.10", "",
			"192.168.5.10 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the PC's route to the gateway"},
		// directAddr: first unless the browser is on one of the PC's networks.
		{"directAddr list", &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}, Addr: "pc.example.net", Addrs: append([]proto.DirectAddr{
			{IP: "198.51.100.200", Kind: proto.DirectKindOverride}}, owner.Addrs...)}, "198.51.100.200", "203.0.113.50",
			"pc.example.net 198.51.100.200 192.168.5.10 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "host config directAddr"},
		{"directAddr, browser on the LAN", &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}, Addr: "pc.example.net", Addrs: owner.Addrs}, "198.51.100.200", "192.168.5.20",
			"192.168.5.10 pc.example.net 198.51.100.200 2001:db8:5::10 100.101.102.103 fd7a:115c:a1e0::1 172.20.0.1", "the browser's network 192.168.5.0/24"},
		// A host before DirectInfo.Addrs: Addr, else the tunnel's peer address, alone.
		{"old host", &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}}, "192.168.5.10", "192.168.5.20", "192.168.5.10", "the address the gateway sees the PC at"},
		{"old host with directAddr", &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}, Addr: "100.101.102.103"}, "192.168.5.10", "192.168.5.20",
			"100.101.102.103 192.168.5.10", "host config directAddr"},
		{"no endpoint", nil, "192.168.5.10", "192.168.5.20", "", ""},
		{"no hashes", &proto.DirectInfo{Port: 48100}, "192.168.5.10", "192.168.5.20", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := netip.ParseAddr(tc.browser)
			got := orderDirect(tc.info, tc.peer, b)
			if hosts(got) != tc.want {
				t.Fatalf("candidates %q, want %q", hosts(got), tc.want)
			}
			if len(got) > 0 && got[0].Note != tc.note {
				t.Fatalf("first candidate's note %q, want %q", got[0].Note, tc.note)
			}
			for _, c := range got {
				if !strings.HasPrefix(c.URL, "https://") || !strings.HasSuffix(c.URL, ":48100/wt") {
					t.Fatalf("candidate URL %q", c.URL)
				}
			}
			if len(got) > maxDirectCands {
				t.Fatalf("%d candidates", len(got))
			}
		})
	}
	if c := orderDirect(owner, "192.168.5.10", netip.MustParseAddr("192.168.5.20")); c[1].URL != "https://[2001:db8:5::10]:48100/wt" {
		t.Fatalf("IPv6 candidate URL %q", c[1].URL)
	}
}
