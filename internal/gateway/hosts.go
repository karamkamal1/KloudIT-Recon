package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// hostConn is a connected host agent.
type hostConn struct {
	id        string
	conn      *quic.Conn
	ctrl      *quic.Stream
	writeMu   sync.Mutex
	directKey []byte
	remoteIP  string
	connected time.Time

	mu        sync.Mutex
	info      proto.TunnelMsg
	streaming bool
	user      string
}

func (h *hostConn) send(m proto.TunnelMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_ = h.ctrl.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return proto.WriteMsg(h.ctrl, b)
}

type pendingData struct {
	hostID string
	nonce  string
	ch     chan *quic.Conn
}

// registry tracks online hosts and data connections being set up.
type registry struct {
	mu      sync.Mutex
	hosts   map[string]*hostConn
	pending map[string]*pendingData
}

func newRegistry() *registry {
	return &registry{hosts: map[string]*hostConn{}, pending: map[string]*pendingData{}}
}

func (r *registry) get(id string) *hostConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hosts[id]
}

// handleHostControl authenticates a host agent and keeps its control stream.
func (s *Server) handleHostControl(conn *quic.Conn) {
	ctx := conn.Context()
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, err := conn.AcceptStream(actx)
	cancel()
	if err != nil {
		conn.CloseWithError(1, "no control stream")
		return
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := proto.ReadMsg(st, proto.MaxControlMsg)
	if err != nil {
		conn.CloseWithError(1, "no registration")
		return
	}
	var reg proto.TunnelMsg
	if json.Unmarshal(b, &reg) != nil || reg.T != "register" {
		conn.CloseWithError(1, "bad registration")
		return
	}
	ip := hostIP(conn.RemoteAddr())
	h, ok := s.store.GetHost(reg.HostID)
	if !ok || reg.Token == "" || !auth.EqualHash(h.TokenHash, auth.TokenHash(reg.Token)) {
		s.audit.Log("host_auth_failed", "", ip, "host id "+trunc(reg.HostID, 40))
		s.log.Warn("host authentication failed", "ip", ip)
		resp, _ := json.Marshal(proto.TunnelMsg{T: "error", Error: "unknown host or invalid token (re-pair the host)"})
		_ = proto.WriteMsg(st, resp)
		time.Sleep(200 * time.Millisecond)
		conn.CloseWithError(4, "unauthorized")
		return
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	hc := &hostConn{id: h.ID, conn: conn, ctrl: st, directKey: key, remoteIP: ip, connected: time.Now(), info: reg}
	if err := hc.send(proto.TunnelMsg{T: "registered", DirectKey: base64.StdEncoding.EncodeToString(key)}); err != nil {
		conn.CloseWithError(1, "")
		return
	}
	_ = s.store.Update(func(st *state) error {
		if p := st.Hosts[h.ID]; p != nil {
			p.LastSeen = time.Now().UTC()
			p.LastIP = ip
			if len(reg.MACs) > 0 {
				p.MACs = reg.MACs
			}
			p.OS, p.Version, p.Encoders = reg.OS, reg.Version, reg.Encoders
		}
		return nil
	})
	s.hosts.mu.Lock()
	old := s.hosts.hosts[h.ID]
	s.hosts.hosts[h.ID] = hc
	s.hosts.mu.Unlock()
	if old != nil {
		old.conn.CloseWithError(3, "replaced by a newer connection")
	}
	s.log.Info("host online", "host", h.Name, "ip", ip, "os", reg.OS, "encoders", reg.Encoders)
	s.audit.Log("host_online", "", ip, h.Name)

	defer func() {
		s.hosts.mu.Lock()
		if s.hosts.hosts[h.ID] == hc {
			delete(s.hosts.hosts, h.ID)
		}
		s.hosts.mu.Unlock()
		s.store.UpdateLazy(func(st *state) {
			if p := st.Hosts[h.ID]; p != nil {
				p.LastSeen = time.Now().UTC()
			}
		})
		s.log.Info("host offline", "host", h.Name)
	}()

	// Keep-alive pings and message loop.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if hc.send(proto.TunnelMsg{T: "ping"}) != nil {
					conn.CloseWithError(1, "")
					return
				}
			}
		}
	}()
	for {
		_ = st.SetReadDeadline(time.Now().Add(60 * time.Second))
		b, err := proto.ReadMsg(st, proto.MaxControlMsg)
		if err != nil {
			conn.CloseWithError(0, "")
			return
		}
		var m proto.TunnelMsg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.T {
		case "status":
			hc.mu.Lock()
			hc.streaming, hc.user = m.Streaming, m.User
			hc.mu.Unlock()
		case "direct":
			hc.mu.Lock()
			hc.info.Direct = m.Direct
			hc.mu.Unlock()
		}
	}
}

// handleHostData matches a per-session data connection to a waiting relay.
func (s *Server) handleHostData(conn *quic.Conn) {
	actx, cancel := context.WithTimeout(conn.Context(), 10*time.Second)
	st, err := conn.AcceptStream(actx)
	cancel()
	if err != nil {
		conn.CloseWithError(1, "")
		return
	}
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := proto.ReadMsg(st, 4096)
	if err != nil {
		conn.CloseWithError(1, "")
		return
	}
	var dh proto.DataHello
	if json.Unmarshal(b, &dh) != nil {
		conn.CloseWithError(1, "")
		return
	}
	s.hosts.mu.Lock()
	p := s.hosts.pending[dh.SID]
	if p != nil && p.hostID == dh.HostID && auth.EqualHash(p.nonce, dh.Nonce) {
		delete(s.hosts.pending, dh.SID)
	} else {
		p = nil
	}
	s.hosts.mu.Unlock()
	if p == nil {
		conn.CloseWithError(4, "unknown session")
		return
	}
	_ = proto.WriteMsg(st, []byte(`{"ok":true}`))
	st.Close()
	select {
	case p.ch <- conn:
	default:
		conn.CloseWithError(1, "")
	}
}

var errHostOffline = errors.New("host is offline")

// openHostSession asks a host for a new data connection and waits for it.
func (s *Server) openHostSession(ctx context.Context, hostID, user string) (*quic.Conn, error) {
	hc := s.hosts.get(hostID)
	if hc == nil {
		return nil, errHostOffline
	}
	sid := auth.RandomToken(12)
	nonce := auth.RandomToken(24)
	p := &pendingData{hostID: hostID, nonce: nonce, ch: make(chan *quic.Conn, 1)}
	s.hosts.mu.Lock()
	s.hosts.pending[sid] = p
	s.hosts.mu.Unlock()
	defer func() {
		s.hosts.mu.Lock()
		delete(s.hosts.pending, sid)
		s.hosts.mu.Unlock()
	}()
	if err := hc.send(proto.TunnelMsg{T: "open", SID: sid, Nonce: nonce, User: user}); err != nil {
		return nil, err
	}
	select {
	case c := <-p.ch:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("host did not open a media connection")
	}
}

func hostIP(a net.Addr) string {
	if ua, ok := a.(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	h, _, _ := net.SplitHostPort(a.String())
	return h
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
