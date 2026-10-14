package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// cspHostSource is CSP's host-source grammar for the https sources the
// gateway emits: a host name or IPv4 address (or "*", "*.name") and a port
// or "*". It has no IPv6 literals: Chromium drops "https://[::1]:48100" as an
// invalid source and refuses the connection.
var cspHostSource = regexp.MustCompile(`^https://(\*|(\*\.)?[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*)(:(\*|[0-9]+))?$`)

// connectSrc returns the connect-src sources of the CSP the gateway sends
// for a GET of path at host (r.Host), with the session cookie token ("" for
// none).
func connectSrc(s *Server, host, path, token string) []string {
	r := httptest.NewRequest("GET", "https://"+host+path, nil)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	for _, d := range strings.Split(w.Header().Get("Content-Security-Policy"), ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "connect-src" {
			return f[1:]
		}
	}
	return nil
}

// signIn adds a live login session of user to s and returns its cookie
// token.
func signIn(t *testing.T, s *Server, user string) string {
	t.Helper()
	tok := auth.RandomToken(32)
	ls := &LoginSession{TokenHash: auth.TokenHash(tok), Username: user, CSRF: auth.RandomToken(18), Created: time.Now(), LastSeen: time.Now()}
	if err := s.store.Update(func(st *state) error { st.Sessions[ls.TokenHash] = ls; return nil }); err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestCSPConnectSources: only the stream page and its worker, naming a
// host in a live login session, allow more than the gateway: that host's
// direct endpoint and the relay ports. Every other page (login, the
// dashboard), and the stream page without a session, allow 'self' only.
// The direct endpoints of hosts whose tunnel reaches the gateway over IPv6,
// and the relay ports of a page opened at an IPv6 address, are allowed by a
// source the grammar accepts (any host on that port); IPv4 addresses and
// names stay exact. Before, every page listed every online host's direct
// endpoint, so with one host on IPv6 the login page allowed https://*:48100.
func TestCSPConnectSources(t *testing.T) {
	direct := func(remote, addr string, port int) *hostConn {
		return &hostConn{remoteIP: remote, info: proto.TunnelMsg{Direct: &proto.DirectInfo{Port: port, Addr: addr, Hashes: []string{"h"}}}}
	}
	s, _ := newLoginServer(t, false)
	s.relay = &udpRelay{ports: []int{8444, 8445}}
	s.hosts.hosts["v6"] = direct("2001:db8::5", "", 48100)
	s.hosts.hosts["v6addr"] = direct("192.0.2.9", "2001:db8::9", 48102) // an advertised IPv6 address
	s.hosts.hosts["v4"] = direct("192.0.2.10", "", 48101)
	s.hosts.hosts["named"] = direct("192.0.2.11", "pc.lan", 48103)
	s.hosts.hosts["none"] = &hostConn{remoteIP: "2001:db8::6"}
	tok := signIn(t, s, "owner")
	expired := auth.RandomToken(32)
	if err := s.store.Update(func(st *state) error {
		old := time.Now().Add(-2 * s.cfg.SessionMax)
		st.Sessions[auth.TokenHash(expired)] = &LoginSession{TokenHash: auth.TokenHash(expired), Username: "owner", Created: old, LastSeen: old}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		host, path, token string
		want              []string
	}{
		{"[fd00::1]:8443", "/stream?host=v6", tok, []string{"'self'", "https://*:48100", "https://*:8444", "https://*:8445"}},
		{"gw.lan:8443", "/js/stream-worker.js?host=v6", tok, []string{"'self'", "https://*:48100", "https://gw.lan:8444", "https://gw.lan:8445"}},
		// The advertised address first, then the tunnel's peer address (orderDirect).
		{"gw.lan:8443", "/stream?host=v6addr", tok, []string{"'self'", "https://*:48102", "https://192.0.2.9:48102", "https://gw.lan:8444", "https://gw.lan:8445"}},
		{"192.0.2.1", "/js/stream-worker.js?host=v4", tok, []string{"'self'", "https://192.0.2.10:48101", "https://192.0.2.1:8444", "https://192.0.2.1:8445"}},
		{"gw.lan", "/stream.html?host=named", tok, []string{"'self'", "https://pc.lan:48103", "https://192.0.2.11:48103", "https://gw.lan:8444", "https://gw.lan:8445"}},
		{"gw.lan", "/stream?host=none", tok, []string{"'self'", "https://gw.lan:8444", "https://gw.lan:8445"}},    // no direct path
		{"gw.lan", "/stream?host=offline", tok, []string{"'self'", "https://gw.lan:8444", "https://gw.lan:8445"}}, // not connected
		{"gw.lan", "/", tok, []string{"'self'"}},                                        // the dashboard
		{"gw.lan", "/login?next=/stream%3Fhost%3Dv6", "", []string{"'self'"}},           // login
		{"gw.lan", "/js/app.js?host=v6", tok, []string{"'self'"}},                       // another script
		{"gw.lan", "/api/hosts?host=v6", tok, []string{"'self'"}},                       // the API
		{"gw.lan", "/stream", tok, []string{"'self'"}},                                  // no host named
		{"gw.lan", "/stream?host=v6", "", []string{"'self'"}},                           // not signed in
		{"gw.lan", "/js/stream-worker.js?host=v6", expired, []string{"'self'"}},         // an expired session
		{"gw.lan", "/js/stream-worker.js?host=v6", "not-a-session", []string{"'self'"}}, // an unknown cookie
	} {
		got := connectSrc(s, tc.host, tc.path, tc.token)
		for _, src := range got[1:] {
			if !cspHostSource.MatchString(src) {
				t.Errorf("%s at %s: connect-src source %q is not valid CSP", tc.path, tc.host, src)
			}
		}
		sorted := slices.Sorted(slices.Values(got))
		if want := slices.Sorted(slices.Values(tc.want)); !slices.Equal(sorted, want) {
			t.Errorf("%s at %s (session %v): connect-src %v, want %v", tc.path, tc.host, tc.token == tok, got, tc.want)
		}
	}

	// More than 32 relay ports: any port on the page's name; at an IPv6
	// address, any https endpoint (no host-source names an IPv6 host). Only
	// on the stream page: the dashboard keeps 'self'.
	s.relay = &udpRelay{}
	for p := 40000; p < 40040; p++ {
		s.relay.ports = append(s.relay.ports, p)
	}
	for host, want := range map[string]string{"gw.lan": "https://gw.lan:*", "[2001:db8::1]:8443": "https://*:*"} {
		if got := connectSrc(s, host, "/stream?host=none", tok); !slices.Equal(got, []string{"'self'", want}) {
			t.Errorf("stream page at %s with 40 relay ports: connect-src %v, want 'self' %s", host, got, want)
		}
		if got := connectSrc(s, host, "/", tok); !slices.Equal(got, []string{"'self'"}) {
			t.Errorf("dashboard at %s with 40 relay ports: connect-src %v, want 'self'", host, got)
		}
	}

	// The direct URL handed to the browser keeps the address itself.
	if c := direct("2001:db8::5", "", 48100).directCandidates(netip.Addr{}); len(c) != 1 || c[0].URL != "https://[2001:db8::5]:48100/wt" {
		t.Errorf("direct candidates %+v", c)
	}
}

// The stream page's CSP allows every address any browser may be offered,
// not only the first maxDirectCands of a list for no browser in particular
// (review of the second hardware run): with eight addresses of the PC and a
// peer address that is none of them, the PC's address on a virtual switch
// ranks last, and only a browser on that switch's network gets it within
// its cap, first.
func TestCSPCoversEveryCandidate(t *testing.T) {
	s, _ := newLoginServer(t, false)
	info := &proto.DirectInfo{Port: 48100, Hashes: []string{"h"}, Addrs: []proto.DirectAddr{
		{IP: "192.168.5.10", Bits: 24, Kind: proto.DirectKindLAN, Iface: "Ethernet 2", Route: true},
		{IP: "2001:db8:5::10", Bits: 64, Kind: proto.DirectKindIPv6, Iface: "Ethernet 2"},
		{IP: "2001:db8:5::11", Bits: 64, Kind: proto.DirectKindIPv6, Iface: "Ethernet 2"},
		{IP: "100.101.102.103", Bits: 32, Kind: proto.DirectKindTailscale, Iface: "Tailscale"},
		{IP: "fd7a:115c:a1e0::1", Bits: 128, Kind: proto.DirectKindTailscale, Iface: "Tailscale"},
		{IP: "172.20.0.1", Bits: 20, Kind: proto.DirectKindVirtual, Iface: "vEthernet (WSL)"},
		{IP: "172.30.0.1", Bits: 20, Kind: proto.DirectKindVirtual, Iface: "vEthernet (Default Switch)"},
		{IP: "192.168.56.1", Bits: 24, Kind: proto.DirectKindVirtual, Iface: "VirtualBox Host-Only Network"},
	}}
	hc := &hostConn{remoteIP: "192.168.5.2", info: proto.TunnelMsg{Direct: info}}
	s.hosts.hosts["pc"] = hc
	tok := signIn(t, s, "owner")
	src := connectSrc(s, "gw.lan", "/js/stream-worker.js?host=pc", tok)
	for _, browser := range []string{"192.168.56.101", "192.168.5.20", "100.64.7.8", "172.20.3.4", "203.0.113.9"} {
		cands := hc.directCandidates(netip.MustParseAddr(browser))
		if len(cands) == 0 || len(cands) > maxDirectCands {
			t.Fatalf("browser %s: %d candidates", browser, len(cands))
		}
		for _, c := range cands {
			if !slices.Contains(src, cspSource(c.host, c.port)) {
				t.Errorf("candidate %s (%s) offered to a browser at %s is not in the worker's CSP %v", c.host, c.Note, browser, src)
			}
		}
	}
	if c := hc.directCandidates(netip.MustParseAddr("192.168.56.101")); c[0].host != "192.168.56.1" {
		t.Fatalf("a browser on the host-only network gets %q first", c[0].host)
	}
}
