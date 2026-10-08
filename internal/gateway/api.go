package gateway

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rsc.io/qr"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func validPassword(pw string) error {
	if utf8.RuneCountInString(pw) < 10 {
		return fmt.Errorf("password must be at least 10 characters")
	}
	if len(pw) > 256 {
		return fmt.Errorf("password too long")
	}
	return nil
}

// hashPassword bounds concurrent Argon2 work so a burst of logins cannot
// exhaust memory on a small server.
func (s *Server) hashPassword(pw string) (string, error) {
	s.hashSem <- struct{}{}
	defer func() { <-s.hashSem }()
	return auth.HashPassword(pw)
}

func (s *Server) verifyPassword(hash, pw string) bool {
	s.hashSem <- struct{}{}
	defer func() { <-s.hashSem }()
	if hash == "" {
		auth.BurnTime(pw)
		return false
	}
	return auth.VerifyPassword(hash, pw)
}

// ---------------------------------------------------------------------------
// Public

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"setupNeeded": s.store.UserCount() == 0,
		"version":     Version,
		"caInstalled": s.ca != nil,
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.checkPublicPost(w, r) {
		return
	}
	ip := s.clientIP(r)
	if !s.loginIP.Allow(ip) {
		jsonError(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var req struct {
		SetupToken string `json:"setupToken"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.store.UserCount() > 0 || s.setupTok == "" {
		jsonError(w, http.StatusConflict, "setup already completed")
		return
	}
	if !auth.EqualHash(strings.TrimSpace(req.SetupToken), s.setupTok) {
		s.audit.Log("setup_failed", req.Username, ip, "bad setup token")
		jsonError(w, http.StatusForbidden, "invalid setup token (see the gateway log or setup-token.txt in its data directory)")
		return
	}
	if !usernameRe.MatchString(req.Username) {
		jsonError(w, http.StatusBadRequest, "invalid username")
		return
	}
	if err := validPassword(req.Password); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := s.hashPassword(req.Password)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "hashing failed")
		return
	}
	err = s.store.Update(func(st *state) error {
		st.Users[req.Username] = &User{Username: req.Username, PasswordHash: hash, Admin: true, Created: time.Now().UTC()}
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setupTok = ""
	_ = os.Remove(filepath.Join(s.cfg.DataDir, "setup-token.txt"))
	s.audit.Log("setup", req.Username, ip, "admin account created")
	s.startSession(w, r, req.Username)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkPublicPost(w, r) {
		return
	}
	ip := s.clientIP(r)
	if !s.loginIP.Allow(ip) {
		s.audit.Log("login_ratelimited", "", ip, "")
		jsonError(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	lockKey := req.Username + "|" + ip // per user+IP, so a remote attacker cannot lock the owner out
	if locked, d := s.lockouts.Locked(lockKey); locked {
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("account temporarily locked, try again in %s", d.Round(time.Second)))
		return
	}
	u, ok := s.store.GetUser(req.Username)
	hash := ""
	if ok {
		hash = u.PasswordHash
	}
	if !s.verifyPassword(hash, req.Password) || !ok {
		s.lockouts.Fail(lockKey)
		s.audit.Log("login_failed", trunc(req.Username, 64), ip, "")
		time.Sleep(300 * time.Millisecond)
		jsonError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if u.TOTPSecret != "" {
		pending := auth.RandomToken(24)
		s.ticketMu.Lock()
		s.pending[auth.TokenHash(pending)] = &pendingLogin{user: u.Username, exp: time.Now().Add(5 * time.Minute), ip: ip}
		s.ticketMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"totpRequired": true, "pending": pending})
		return
	}
	s.lockouts.Success(lockKey)
	s.startSession(w, r, u.Username)
}

func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	if !s.checkPublicPost(w, r) {
		return
	}
	ip := s.clientIP(r)
	if !s.loginIP.Allow(ip) {
		jsonError(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var req struct {
		Pending string `json:"pending"`
		Code    string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.ticketMu.Lock()
	pl := s.pending[auth.TokenHash(req.Pending)]
	s.ticketMu.Unlock()
	if pl == nil || time.Now().After(pl.exp) {
		jsonError(w, http.StatusUnauthorized, "login expired, start again")
		return
	}
	if locked, d := s.lockouts.Locked(pl.user + "|" + ip); locked {
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("account temporarily locked, try again in %s", d.Round(time.Second)))
		return
	}
	ok := false
	_ = s.store.Update(func(st *state) error {
		u := st.Users[pl.user]
		if u == nil || u.TOTPSecret == "" {
			return nil
		}
		if c, valid := auth.VerifyTOTP(u.TOTPSecret, strings.TrimSpace(req.Code), time.Now(), u.TOTPLast); valid {
			u.TOTPLast = c
			ok = true
		}
		return nil
	})
	if !ok {
		s.lockouts.Fail(pl.user + "|" + ip)
		s.audit.Log("totp_failed", pl.user, ip, "")
		jsonError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	s.ticketMu.Lock()
	delete(s.pending, auth.TokenHash(req.Pending))
	s.ticketMu.Unlock()
	s.lockouts.Success(pl.user + "|" + ip)
	s.startSession(w, r, pl.user)
}

// ---------------------------------------------------------------------------
// Account

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	_ = s.store.Update(func(st *state) error {
		delete(st.Sessions, ls.TokenHash)
		return nil
	})
	clearCookie(w)
	s.audit.Log("logout", u.Username, s.clientIP(r), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "admin": u.Admin, "totp": u.TOTPSecret != "", "csrf": ls.CSRF, "version": Version,
	})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.verifyPassword(u.PasswordHash, req.Current) {
		s.audit.Log("password_change_failed", u.Username, s.clientIP(r), "")
		jsonError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	if err := validPassword(req.New); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := s.hashPassword(req.New)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "hashing failed")
		return
	}
	_ = s.store.Update(func(st *state) error {
		if p := st.Users[u.Username]; p != nil {
			p.PasswordHash = hash
		}
		// Log out every other session of this user.
		for k, x := range st.Sessions {
			if x.Username == u.Username && k != ls.TokenHash {
				delete(st.Sessions, k)
			}
		}
		return nil
	})
	s.audit.Log("password_changed", u.Username, s.clientIP(r), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	secret := auth.NewTOTPSecret()
	uri := auth.TOTPURI("KloudIT Recon", u.Username, secret)
	svg := ""
	if code, err := qr.Encode(uri, qr.M); err == nil {
		svg = qrSVG(code)
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": uri, "qr": "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))})
}

func qrSVG(c *qr.Code) string {
	var b strings.Builder
	n := c.Size
	q := 4 // quiet zone
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#fff"/><path fill="#000" d="`, n+2*q, n+2*q)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+q, y+q)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Secret string `json:"secret"`
		Code   string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, ok := auth.VerifyTOTP(req.Secret, strings.TrimSpace(req.Code), time.Now(), 0)
	if !ok || len(req.Secret) < 26 {
		jsonError(w, http.StatusBadRequest, "code does not match — check your authenticator app's clock")
		return
	}
	_ = s.store.Update(func(st *state) error {
		if p := st.Users[u.Username]; p != nil {
			p.TOTPSecret, p.TOTPLast = req.Secret, c
		}
		return nil
	})
	s.audit.Log("totp_enabled", u.Username, s.clientIP(r), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.verifyPassword(u.PasswordHash, req.Password) {
		jsonError(w, http.StatusForbidden, "password is wrong")
		return
	}
	_ = s.store.Update(func(st *state) error {
		if p := st.Users[u.Username]; p != nil {
			p.TOTPSecret, p.TOTPLast = "", 0
		}
		return nil
	})
	s.audit.Log("totp_disabled", u.Username, s.clientIP(r), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// Hosts

type hostView struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Online    bool                `json:"online"`
	Streaming bool                `json:"streaming"`
	User      string              `json:"user,omitempty"`
	LastSeen  time.Time           `json:"lastSeen,omitempty"`
	IP        string              `json:"ip,omitempty"`
	OS        string              `json:"os,omitempty"`
	Version   string              `json:"version,omitempty"`
	Encoders  []string            `json:"encoders,omitempty"`
	Monitors  []proto.MonitorInfo `json:"monitors,omitempty"`
	Direct    bool                `json:"direct"`
	CanWake   bool                `json:"canWake"`
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var out []hostView
	for _, h := range s.store.Hosts() {
		v := hostView{ID: h.ID, Name: h.Name, LastSeen: h.LastSeen, IP: h.LastIP, OS: h.OS, Version: h.Version, Encoders: h.Encoders, CanWake: len(h.MACs) > 0}
		if hc := s.hosts.get(h.ID); hc != nil {
			hc.mu.Lock()
			v.Online, v.Streaming, v.User = true, hc.streaming, hc.user
			v.Monitors = hc.info.Monitors
			v.Encoders = hc.info.Encoders
			hc.mu.Unlock()
			v.Direct = hc.directURL() != ""
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

func (s *Server) publicAddr(r *http.Request) string {
	if a := s.cfg.PublicAddr; a != "" {
		if _, _, err := net.SplitHostPort(a); err != nil { // no port given: use ours
			_, port, _ := net.SplitHostPort(s.cfg.Listen)
			a = net.JoinHostPort(strings.Trim(a, "[]"), port)
		}
		return a
	}
	host := r.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		_, port, _ := net.SplitHostPort(s.cfg.Listen)
		host = net.JoinHostPort(host, port)
	}
	return host
}

func (s *Server) pairingCode(r *http.Request, h *Host, token string) string {
	return proto.PairingCode{Gateway: s.publicAddr(r), HostID: h.ID, Token: token, Pin: s.TunnelPin, Name: h.Name}.Encode()
}

func cleanName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || utf8.RuneCountInString(n) > 64 {
		return "", fmt.Errorf("the name must be 1 to 64 characters")
	}
	if strings.ContainsAny(n, "<>\"`\x00") {
		return "", fmt.Errorf("the name can't contain < > \" or `")
	}
	return n, nil
}

func (s *Server) handleHostAdd(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := cleanName(req.Name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	token := auth.RandomToken(32)
	h := &Host{ID: auth.RandomToken(9), Name: name, TokenHash: auth.TokenHash(token), Created: time.Now().UTC()}
	_ = s.store.Update(func(st *state) error {
		st.Hosts[h.ID] = h
		return nil
	})
	s.audit.Log("host_added", u.Username, s.clientIP(r), name)
	writeJSON(w, http.StatusOK, map[string]any{"id": h.ID, "name": h.Name, "pairingCode": s.pairingCode(r, h, token)})
}

func (s *Server) handleHostRepair(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	id := r.PathValue("id")
	token := auth.RandomToken(32)
	var h Host
	err := s.store.Update(func(st *state) error {
		p := st.Hosts[id]
		if p == nil {
			return fmt.Errorf("no such host")
		}
		p.TokenHash = auth.TokenHash(token)
		h = *p
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	if hc := s.hosts.get(id); hc != nil {
		hc.conn.CloseWithError(4, "token revoked")
	}
	s.audit.Log("host_repaired", u.Username, s.clientIP(r), h.Name)
	writeJSON(w, http.StatusOK, map[string]any{"id": h.ID, "name": h.Name, "pairingCode": s.pairingCode(r, &h, token)})
}

func (s *Server) handleHostDelete(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	id := r.PathValue("id")
	var name string
	_ = s.store.Update(func(st *state) error {
		if p := st.Hosts[id]; p != nil {
			name = p.Name
		}
		delete(st.Hosts, id)
		return nil
	})
	if hc := s.hosts.get(id); hc != nil {
		hc.conn.CloseWithError(4, "host removed")
	}
	s.audit.Log("host_removed", u.Username, s.clientIP(r), name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleHostRename(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := cleanName(req.Name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	err = s.store.Update(func(st *state) error {
		p := st.Hosts[id]
		if p == nil {
			return fmt.Errorf("no such host")
		}
		p.Name = name
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleHostWake(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	h, ok := s.store.GetHost(r.PathValue("id"))
	if !ok {
		jsonError(w, http.StatusNotFound, "no such host")
		return
	}
	if err := Wake(h.MACs, h.WakeBroadcast); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit.Log("wake", u.Username, s.clientIP(r), h.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleHostConnect issues single-use tickets for the relay (UDP relay when
// the gateway has relay ports and the host supports it, QUIC splice over
// WebTransport, WebSocket) and, when the host offers one, for the direct path.
func (s *Server) handleHostConnect(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	id := r.PathValue("id")
	h, ok := s.store.GetHost(id)
	if !ok {
		jsonError(w, http.StatusNotFound, "no such host")
		return
	}
	hc := s.hosts.get(id)
	if hc == nil {
		jsonError(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	newTicket := func() string {
		t := auth.RandomToken(24)
		s.ticketMu.Lock()
		s.tickets[auth.TokenHash(t)] = &relayTicket{user: u.Username, hostID: id, exp: time.Now().Add(60 * time.Second)}
		s.ticketMu.Unlock()
		return t
	}
	resp := map[string]any{"host": h.Name}
	var hashes []string
	if s.wtRot != nil {
		hashes = s.wtRot.HashesB64()
	}
	relay := map[string]any{
		"wt":     "https://" + r.Host + "/wt/relay?t=" + newTicket(),
		"ws":     "wss://" + r.Host + "/ws/relay?t=" + newTicket(),
		"hashes": hashes,
	}
	if s.relay != nil && len(hc.relayHashes()) > 0 {
		// POST it to allocate a UDP relay port (handleUDPRelay).
		relay["udp"] = "https://" + r.Host + "/api/relay/udp?t=" + newTicket()
	}
	resp["relay"] = relay
	if u := hc.directURL(); u != "" {
		origin := "https://" + r.Host
		tok, err := auth.SignTicket(hc.directKey, proto.DirectTicket{
			HostID: id, User: ls.Username, Exp: time.Now().Add(60 * time.Second).Unix(), Nonce: auth.RandomToken(12), Origin: origin,
		})
		if err == nil {
			hc.mu.Lock()
			var dh []string
			if hc.info.Direct != nil {
				dh = append(dh, hc.info.Direct.Hashes...)
			}
			hc.mu.Unlock()
			if len(dh) > 0 {
				resp["direct"] = map[string]any{"url": u, "hashes": dh, "ticket": tok}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Users (admin)

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	type uv struct {
		Username  string    `json:"username"`
		Admin     bool      `json:"admin"`
		TOTP      bool      `json:"totp"`
		Created   time.Time `json:"created"`
		LastLogin time.Time `json:"lastLogin,omitempty"`
	}
	var out []uv
	for _, x := range s.store.Users() {
		out = append(out, uv{x.Username, x.Admin, x.TOTPSecret != "", x.Created, x.LastLogin})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) handleUserAdd(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !usernameRe.MatchString(req.Username) {
		jsonError(w, http.StatusBadRequest, "invalid username")
		return
	}
	if err := validPassword(req.Password); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := s.hashPassword(req.Password)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "hashing failed")
		return
	}
	err = s.store.Update(func(st *state) error {
		if st.Users[req.Username] != nil {
			return fmt.Errorf("user exists")
		}
		st.Users[req.Username] = &User{Username: req.Username, PasswordHash: hash, Admin: req.Admin, Created: time.Now().UTC()}
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	s.audit.Log("user_added", u.Username, s.clientIP(r), req.Username+" admin="+strconv.FormatBool(req.Admin))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	name := r.PathValue("name")
	if name == u.Username {
		jsonError(w, http.StatusBadRequest, "you cannot delete yourself")
		return
	}
	_ = s.store.Update(func(st *state) error {
		delete(st.Users, name)
		for k, x := range st.Sessions {
			if x.Username == name {
				delete(st.Sessions, k)
			}
		}
		return nil
	})
	s.audit.Log("user_removed", u.Username, s.clientIP(r), name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, u *User, ls *LoginSession) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.audit.Recent(200)})
}
