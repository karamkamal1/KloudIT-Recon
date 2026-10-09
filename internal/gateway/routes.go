package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

const cookieName = "__Host-recon"

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

func (s *Server) routes() {
	m := http.NewServeMux()
	// Public.
	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("POST /api/setup", s.handleSetup)
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/login/totp", s.handleLoginTOTP)
	m.HandleFunc("GET /ca.crt", s.handleCA)
	// Authenticated.
	m.Handle("POST /api/logout", s.authed(s.handleLogout, false))
	m.Handle("GET /api/me", s.authed(s.handleMe, false))
	m.Handle("POST /api/me/password", s.authed(s.handlePassword, false))
	m.Handle("POST /api/me/totp/begin", s.authed(s.handleTOTPBegin, false))
	m.Handle("POST /api/me/totp/enable", s.authed(s.handleTOTPEnable, false))
	m.Handle("POST /api/me/totp/disable", s.authed(s.handleTOTPDisable, false))
	m.Handle("GET /api/hosts", s.authed(s.handleHosts, false))
	m.Handle("POST /api/hosts", s.authed(s.handleHostAdd, true))
	m.Handle("DELETE /api/hosts/{id}", s.authed(s.handleHostDelete, true))
	m.Handle("POST /api/hosts/{id}/rename", s.authed(s.handleHostRename, true))
	m.Handle("POST /api/hosts/{id}/repair", s.authed(s.handleHostRepair, true))
	m.Handle("POST /api/hosts/{id}/wake", s.authed(s.handleHostWake, false))
	m.Handle("POST /api/hosts/{id}/connect", s.authed(s.handleHostConnect, false))
	m.Handle("GET /api/users", s.authed(s.handleUsers, true))
	m.Handle("POST /api/users", s.authed(s.handleUserAdd, true))
	m.Handle("DELETE /api/users/{name}", s.authed(s.handleUserDelete, true))
	m.Handle("GET /api/audit", s.authed(s.handleAudit, true))
	// Media relay (authorised by single-use tickets).
	m.HandleFunc("POST /api/relay/udp", s.handleUDPRelay)
	m.HandleFunc("/wt/relay", s.handleWTRelay)
	m.HandleFunc("GET /ws/relay", s.handleWSRelay)
	// Static client.
	m.HandleFunc("/", s.handleStatic)
	s.mux = http.NewServeMux()
	s.mux.Handle("/", s.secure(m))
}

// secure adds security headers, request size limits and API rate limiting.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), gamepad=(self), fullscreen=(self)")
		h.Set("Content-Security-Policy", s.csp(r))
		if s.wtRot == nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if !s.apiIP.Allow(rateKey(s.clientIP(r))) {
				jsonError(w, http.StatusTooManyRequests, "too many requests")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		next.ServeHTTP(w, r)
	})
}

// csp allows connections to self, every host's direct endpoint and the UDP
// relay ports (under the name the page was loaded from).
func (s *Server) csp(r *http.Request) string {
	connect := []string{"'self'"}
	seen := map[string]bool{}
	add := func(src string) {
		if !seen[src] {
			seen[src] = true
			connect = append(connect, src)
		}
	}
	s.hosts.mu.Lock()
	for _, hc := range s.hosts.hosts {
		if addr, port := hc.directEndpoint(); addr != "" {
			add(cspSource(addr, strconv.Itoa(port)))
		}
	}
	s.hosts.mu.Unlock()
	if s.relay != nil {
		host := requestHostname(r)
		if len(s.relay.ports) > 32 {
			add(cspSource(host, "*"))
		} else {
			for _, p := range s.relay.ports {
				add(cspSource(host, strconv.Itoa(p)))
			}
		}
	}
	return "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self'; " +
		"connect-src " + strings.Join(connect, " ") + "; worker-src 'self'; manifest-src 'self'; media-src 'self' blob:; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// cspSource is the connect-src source for https://host:port (port "*": any
// port). CSP's host-source grammar has no IPv6 literals: browsers drop
// "https://[::1]:48100" as invalid and refuse the connection (the direct
// path of an agent whose tunnel reaches the gateway over IPv6, the relay
// ports of a page opened at an IPv6 address). An IPv6 host gets the wildcard
// host instead: any host on that port (with port "*", any https endpoint).
func cspSource(host, port string) string {
	host = strings.Trim(host, "[]")
	if strings.Contains(host, ":") {
		host = "*"
	}
	return "https://" + net.JoinHostPort(host, port)
}

// directEndpoint is the address and port of the host's direct path ("" when
// it offers none): the address it advertises, else its tunnel's peer address.
func (hc *hostConn) directEndpoint() (string, int) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	d := hc.info.Direct
	if d == nil || d.Port == 0 || len(d.Hashes) == 0 {
		return "", 0
	}
	addr := d.Addr
	if addr == "" {
		addr = hc.remoteIP
	}
	return addr, d.Port
}

func (hc *hostConn) directURL() string {
	addr, port := hc.directEndpoint()
	if addr == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/wt"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("expected application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ---------------------------------------------------------------------------
// Sessions

// authed wraps a handler with session validation, CSRF checks for unsafe
// methods, and an optional admin requirement.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *User, *LoginSession), admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" {
			jsonError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		th := auth.TokenHash(c.Value)
		var ls LoginSession
		var u User
		ok := false
		now := time.Now()
		s.store.View(func(st *state) {
			p := st.Sessions[th]
			if p == nil || now.Sub(p.LastSeen) > s.cfg.SessionIdle || now.Sub(p.Created) > s.cfg.SessionMax {
				return
			}
			usr := st.Users[p.Username]
			if usr == nil {
				return
			}
			ls, u, ok = *p, *usr, true
		})
		if !ok {
			clearCookie(w)
			jsonError(w, http.StatusUnauthorized, "session expired")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !auth.EqualHash(r.Header.Get("X-Recon-CSRF"), ls.CSRF) || (r.Header.Get("Origin") != "" && !s.sameOrigin(r)) {
				jsonError(w, http.StatusForbidden, "CSRF check failed")
				return
			}
		}
		if admin && !u.Admin {
			jsonError(w, http.StatusForbidden, "admin only")
			return
		}
		if now.Sub(ls.LastSeen) > time.Minute {
			s.store.UpdateLazy(func(st *state) {
				if p := st.Sessions[th]; p != nil {
					p.LastSeen = now
				}
			})
		}
		h(w, r, &u, &ls)
	})
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, username string) {
	tok := auth.RandomToken(32)
	ls := &LoginSession{
		TokenHash: auth.TokenHash(tok), Username: username, CSRF: auth.RandomToken(18),
		Created: time.Now(), LastSeen: time.Now(), IP: s.clientIP(r), UA: trunc(r.UserAgent(), 200),
	}
	_ = s.store.Update(func(st *state) error {
		st.Sessions[ls.TokenHash] = ls
		if u := st.Users[username]; u != nil {
			u.LastLogin = time.Now().UTC()
		}
		// Cap concurrent sessions per user.
		var mine []*LoginSession
		for _, x := range st.Sessions {
			if x.Username == username {
				mine = append(mine, x)
			}
		}
		if len(mine) > 20 {
			oldest := mine[0]
			for _, x := range mine {
				if x.LastSeen.Before(oldest.LastSeen) {
					oldest = x
				}
			}
			delete(st.Sessions, oldest.TokenHash)
		}
		return nil
	})
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(s.cfg.SessionMax.Seconds()),
	})
	s.audit.Log("login", username, s.clientIP(r), trunc(r.UserAgent(), 120))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": ls.CSRF})
}

// unauthenticated POSTs must still come from our own pages.
func (s *Server) checkPublicPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Recon-CSRF") != "public" || !s.sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "CSRF check failed")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Relay endpoints

// takeTicket validates and consumes a single-use relay ticket.
func (s *Server) takeTicket(r *http.Request) (*relayTicket, error) {
	t := r.URL.Query().Get("t")
	if t == "" {
		return nil, errTicket
	}
	s.ticketMu.Lock()
	defer s.ticketMu.Unlock()
	rt := s.tickets[auth.TokenHash(t)]
	if rt == nil || time.Now().After(rt.exp) {
		return nil, errTicket
	}
	delete(s.tickets, auth.TokenHash(t))
	return rt, nil
}

// requestHostname is the host part of the request's Host header.
func requestHostname(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return strings.Trim(r.Host, "[]")
}

// handleUDPRelay allocates a UDP relay port for a browser holding a relay
// ticket (guide step 2.6), has the host bind its relay socket to it, and tells
// the browser where to connect: the allocation port on this gateway's name,
// the host's certificate hashes and a host ticket bound to the allocation. The
// browser then runs one QUIC connection end to end with the host, and the
// gateway only forwards its datagrams (udprelay.go).
func (s *Server) handleUDPRelay(w http.ResponseWriter, r *http.Request) {
	if s.relay == nil {
		jsonError(w, http.StatusNotFound, "the UDP relay is off")
		return
	}
	if !s.sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	rt, err := s.takeTicket(r)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	hc := s.hosts.get(rt.hostID)
	if hc == nil {
		jsonError(w, http.StatusServiceUnavailable, errHostOffline.Error())
		return
	}
	hashes := hc.relayHashes()
	if len(hashes) == 0 {
		jsonError(w, http.StatusNotImplemented, "the host agent predates the UDP relay")
		return
	}
	ip := s.clientIP(r)
	clientIP, err := netip.ParseAddr(ip)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "unknown client address")
		return
	}
	name := s.hostName(rt.hostID)
	a, err := s.relay.allocate(allocRequest{
		user: rt.user, hostID: rt.hostID, clientIP: clientIP,
		onLock: func(netip.AddrPort) {
			s.audit.Log("stream_start", rt.user, ip, "host "+name+" via udp relay")
		},
		onEnd: func(st *relayStats) {
			s.audit.Log("stream_end", rt.user, ip, fmt.Sprintf("host %s after %s", name, st.Last.Sub(st.Started).Round(time.Second)))
		},
	})
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	remove, ok := s.streams.add(rt.user, rt.issued, a.close)
	if !ok {
		a.close()
		jsonError(w, http.StatusUnauthorized, errRevoked.Error())
		return
	}
	go func() { <-a.done; remove() }()
	if err := hc.send(proto.TunnelMsg{T: "relay", SID: a.id, Nonce: base64.StdEncoding.EncodeToString(a.token), User: rt.user, Port: a.port}); err != nil {
		a.close()
		jsonError(w, http.StatusServiceUnavailable, errHostOffline.Error())
		return
	}
	select {
	case <-a.bound:
	case <-a.done:
		// The host's bind did not arrive in time, typically because a firewall in
		// front of the gateway lets only 8443 through. 504 tells the client to
		// skip the UDP relay for a while, as when its own datagrams get no answer.
		jsonError(w, http.StatusGatewayTimeout, "the host did not reach the relay port")
		return
	case <-r.Context().Done():
		a.close()
		return
	}
	tok, err := hc.signHostTicket(proto.DirectTicket{
		HostID: rt.hostID, User: rt.user, Exp: time.Now().Add(60 * time.Second).Unix(), Nonce: auth.RandomToken(12),
		Origin: "https://" + r.Host, Relay: a.id,
	})
	if err != nil {
		a.close()
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":    "https://" + net.JoinHostPort(requestHostname(r), strconv.Itoa(a.port)) + "/wt",
		"hashes": hashes,
		"ticket": tok,
	})
}

func (s *Server) handleWTRelay(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rt, err := s.takeTicket(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	hostConn, err := s.openHostSession(r.Context(), rt.hostID, rt.user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	remove, ok := s.streams.add(rt.user, rt.issued, func() { hostConn.CloseWithError(0, errRevoked.Error()) })
	if !ok {
		hostConn.CloseWithError(0, errRevoked.Error())
		http.Error(w, errRevoked.Error(), http.StatusUnauthorized)
		return
	}
	defer remove()
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		hostConn.CloseWithError(1, "browser upgrade failed")
		s.log.Warn("webtransport upgrade failed", "err", err)
		return
	}
	s.audit.Log("stream_start", rt.user, s.clientIP(r), "host "+s.hostName(rt.hostID)+" via webtransport relay")
	start := time.Now()
	relayQUIC(transport.FromWebTransport(sess), transport.FromQUIC(hostConn))
	s.audit.Log("stream_end", rt.user, s.clientIP(r), fmt.Sprintf("host %s after %s", s.hostName(rt.hostID), time.Since(start).Round(time.Second)))
}

func (s *Server) handleWSRelay(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rt, err := s.takeTicket(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	hostConn, err := s.openHostSession(r.Context(), rt.hostID, rt.user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	remove, ok := s.streams.add(rt.user, rt.issued, func() { hostConn.CloseWithError(0, errRevoked.Error()) })
	if !ok {
		hostConn.CloseWithError(0, errRevoked.Error())
		http.Error(w, errRevoked.Error(), http.StatusUnauthorized)
		return
	}
	defer remove()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		hostConn.CloseWithError(1, "")
		return
	}
	s.audit.Log("stream_start", rt.user, s.clientIP(r), "host "+s.hostName(rt.hostID)+" via websocket relay")
	start := time.Now()
	relayWS(context.Background(), ws, transport.FromQUIC(hostConn))
	s.audit.Log("stream_end", rt.user, s.clientIP(r), fmt.Sprintf("host %s after %s", s.hostName(rt.hostID), time.Since(start).Round(time.Second)))
}

func (s *Server) hostName(id string) string {
	if h, ok := s.store.GetHost(id); ok {
		return h.Name
	}
	return id
}

// ---------------------------------------------------------------------------
// Static files

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	switch p {
	case "/":
		p = "/index.html"
	case "/login", "/stream":
		p += ".html"
	}
	f, ok := s.static[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("ETag", f.etag)
	if strings.HasSuffix(p, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "no-cache, max-age=0")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.data)
}

func (s *Server) handleCA(w http.ResponseWriter, r *http.Request) {
	if s.ca == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="kloudit-recon-ca.crt"`)
	_, _ = w.Write(s.ca.PEM())
}
