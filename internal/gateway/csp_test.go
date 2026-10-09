package gateway

import (
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// cspHostSource is CSP's host-source grammar for the https sources the
// gateway emits: a host name or IPv4 address (or "*", "*.name") and a port
// or "*". It has no IPv6 literals: Chromium drops "https://[::1]:48100" as an
// invalid source and refuses the connection.
var cspHostSource = regexp.MustCompile(`^https://(\*|(\*\.)?[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*)(:(\*|[0-9]+))?$`)

// connectSrc returns the connect-src sources of the page's CSP for a request
// to host (r.Host).
func connectSrc(s *Server, host string) []string {
	r := httptest.NewRequest("GET", "https://"+host+"/", nil)
	for _, d := range strings.Split(s.csp(r), ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "connect-src" {
			return f[1:]
		}
	}
	return nil
}

// The direct endpoints of hosts whose tunnel reaches the gateway over IPv6,
// and the relay ports of a page opened at an IPv6 address, are allowed by a
// source the grammar accepts (any host on that port); IPv4 addresses and
// names stay exact.
func TestCSPConnectSources(t *testing.T) {
	direct := func(remote, addr string, port int) *hostConn {
		return &hostConn{remoteIP: remote, info: proto.TunnelMsg{Direct: &proto.DirectInfo{Port: port, Addr: addr, Hashes: []string{"h"}}}}
	}
	s := &Server{hosts: newRegistry(), relay: &udpRelay{ports: []int{8444, 8445}}}
	s.hosts.hosts["v6"] = direct("2001:db8::5", "", 48100)
	s.hosts.hosts["v6b"] = direct("fd00::7", "", 48100)                 // the same source: listed once
	s.hosts.hosts["v6addr"] = direct("192.0.2.9", "2001:db8::9", 48102) // an advertised IPv6 address
	s.hosts.hosts["v4"] = direct("192.0.2.10", "", 48101)
	s.hosts.hosts["named"] = direct("192.0.2.11", "pc.lan", 48103)
	s.hosts.hosts["none"] = &hostConn{remoteIP: "2001:db8::6"}

	for _, tc := range []struct {
		host string
		want []string
	}{
		{"[fd00::1]:8443", []string{"'self'", "https://*:48100", "https://*:48102", "https://192.0.2.10:48101", "https://pc.lan:48103", "https://*:8444", "https://*:8445"}},
		{"gw.lan:8443", []string{"'self'", "https://*:48100", "https://*:48102", "https://192.0.2.10:48101", "https://pc.lan:48103", "https://gw.lan:8444", "https://gw.lan:8445"}},
		{"192.0.2.1", []string{"'self'", "https://*:48100", "https://*:48102", "https://192.0.2.10:48101", "https://pc.lan:48103", "https://192.0.2.1:8444", "https://192.0.2.1:8445"}},
	} {
		got := connectSrc(s, tc.host)
		for _, src := range got[1:] {
			if !cspHostSource.MatchString(src) {
				t.Errorf("page at %s: connect-src source %q is not valid CSP", tc.host, src)
			}
		}
		sorted := slices.Sorted(slices.Values(got))
		if want := slices.Sorted(slices.Values(tc.want)); !slices.Equal(sorted, want) {
			t.Errorf("page at %s: connect-src %v, want %v", tc.host, got, tc.want)
		}
	}

	// More than 32 relay ports: any port on the page's name; at an IPv6
	// address, any https endpoint (no host-source names an IPv6 host).
	s = &Server{hosts: newRegistry(), relay: &udpRelay{}}
	for p := 40000; p < 40040; p++ {
		s.relay.ports = append(s.relay.ports, p)
	}
	for host, want := range map[string]string{"gw.lan": "https://gw.lan:*", "[2001:db8::1]:8443": "https://*:*"} {
		if got := connectSrc(s, host); !slices.Equal(got, []string{"'self'", want}) {
			t.Errorf("page at %s with 40 relay ports: connect-src %v, want 'self' %s", host, got, want)
		}
	}

	// The direct URL handed to the browser keeps the address itself.
	if u := direct("2001:db8::5", "", 48100).directURL(); u != "https://[2001:db8::5]:48100/wt" {
		t.Errorf("directURL = %q", u)
	}
}
