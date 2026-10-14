package gateway

import (
	"cmp"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// directCand is one address of the host's direct path the browser tries (the
// connect answer's direct.candidates): it starts them all, the first one in
// the list first, and keeps the first that answers.
type directCand struct {
	URL  string `json:"url"`
	Kind string `json:"kind,omitempty"` // proto.DirectAddr.Kind; "peer": the tunnel's peer address
	// Note says why the address is where it is in the list ("the browser's
	// network 192.168.5.0/24", "host config directAddr", ...): the client
	// reports it with its attempt's outcome, which the host logs.
	Note       string `json:"note,omitempty"`
	host, port string // the URL's, for the CSP
}

var (
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
	tsULAPrefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// maxDirectCands bounds the addresses a browser tries.
const maxDirectCands = 8

// directCandidates lists the addresses of the host's direct path in the order
// the browser at browser (invalid: unknown) should try them ("" when the host
// offers none).
func (hc *hostConn) directCandidates(browser netip.Addr) []directCand {
	hc.mu.Lock()
	d := hc.info.Direct
	hc.mu.Unlock()
	return orderDirect(d, hc.remoteIP, browser)
}

// orderDirect orders the direct endpoint's addresses for a browser at browser:
//
//  1. the PC's addresses on the browser's own network (its prefix contains the
//     browser's address; Tailscale addresses for a browser on the tailnet);
//  2. host config directAddr (Addr, then the Addrs of kind override): the
//     owner's choice, also a name or a port forward's address;
//  3. the PC's address towards the gateway (Route);
//  4. the address the gateway sees the PC's tunnel come from (peer): the PC
//     itself on a LAN, its router's public address behind NAT (a port
//     forward);
//  5. the PC's other addresses, LAN before IPv6 before Tailscale, VPN and
//     virtual switches (the host's order within each).
//
// A host before DirectInfo.Addrs offers Addr or the peer address alone, as
// before.
func orderDirect(d *proto.DirectInfo, peer string, browser netip.Addr) []directCand {
	if d == nil || d.Port == 0 || len(d.Hashes) == 0 {
		return nil
	}
	browser = browser.Unmap()
	port := strconv.Itoa(d.Port)
	var out []directCand
	seen := map[string]bool{}
	add := func(host, kind, note string) {
		host = strings.Trim(host, "[]")
		if ip, err := netip.ParseAddr(host); err == nil {
			host = ip.Unmap().String()
		}
		if host == "" || seen[host] || len(out) == maxDirectCands {
			return
		}
		seen[host] = true
		out = append(out, directCand{URL: "https://" + net.JoinHostPort(host, port) + "/wt", Kind: kind, Note: note, host: host, port: port})
	}
	own := slices.DeleteFunc(slices.Clone(d.Addrs), func(a proto.DirectAddr) bool { return a.Kind == proto.DirectKindOverride })
	if browser.IsValid() {
		for _, a := range own {
			ip, err := netip.ParseAddr(a.IP)
			if err != nil {
				continue
			}
			ip = ip.Unmap()
			if a.Bits > 0 && ip.BitLen() == browser.BitLen() && a.Bits < ip.BitLen() {
				if p := netip.PrefixFrom(ip, a.Bits).Masked(); p.Contains(browser) {
					add(a.IP, a.Kind, "the browser's network "+p.String())
					continue
				}
			}
			if a.Kind == proto.DirectKindTailscale && (cgnatPrefix.Contains(browser) || tsULAPrefix.Contains(browser)) &&
				(cgnatPrefix.Contains(ip) || tsULAPrefix.Contains(ip)) && ip.BitLen() == browser.BitLen() {
				add(a.IP, a.Kind, "the browser is on the tailnet too")
			}
		}
	}
	if d.Addr != "" {
		add(d.Addr, proto.DirectKindOverride, "host config directAddr")
	}
	for _, a := range d.Addrs {
		if a.Kind == proto.DirectKindOverride {
			add(a.IP, a.Kind, "host config directAddr")
		}
	}
	for _, a := range own {
		if a.Route {
			add(a.IP, a.Kind, "the PC's route to the gateway")
		}
	}
	add(peer, "peer", "the address the gateway sees the PC at")
	slices.SortStableFunc(own, func(x, y proto.DirectAddr) int { return cmp.Compare(kindRank(x.Kind), kindRank(y.Kind)) })
	for _, a := range own {
		add(a.IP, a.Kind, "")
	}
	return out
}

// kindRank orders the host's addresses of no better rank (as the host does).
func kindRank(kind string) int {
	switch kind {
	case proto.DirectKindLAN, proto.DirectKindPublic:
		return 0
	case proto.DirectKindIPv6:
		return 1
	case proto.DirectKindTailscale:
		return 2
	case proto.DirectKindVPN:
		return 3
	case proto.DirectKindVirtual:
		return 4
	}
	return 5
}

// browserAddr is the address of the browser that made r, as the gateway sees
// it (behind a trusted proxy: the forwarded one); invalid if unknown.
func (s *Server) browserAddr(r *http.Request) netip.Addr {
	ip, _ := netip.ParseAddr(s.clientIP(r))
	return ip.Unmap()
}

// directCandText is cands for a log line.
func directCandText(cands []directCand) string {
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		parts = append(parts, c.host+" ("+strings.Trim(c.Kind+", "+c.Note, ", ")+")")
	}
	return strings.Join(parts, "; ")
}
