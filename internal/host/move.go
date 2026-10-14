package host

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// Moving a live session onto the direct path (VENDOR_NOTES "Second hardware
// run: direct path on the LAN"). A client that streams through the relay
// goes on trying the direct path in the background; when one of the PC's
// addresses answers it opens a session there whose hello carries the
// session's resume token (Welcome.Resume) and a fresh direct ticket. The
// running session takes that connection over (Session.adopt): its frames,
// datagrams and control messages go out on it from then on, and its
// pipeline, encoder, audio and state stay as they are, so the picture goes
// on without a restart or a key frame (a frame lost in the switch is a loss
// like any other: the loss-recovery ladder answers it). The connection
// before stays open for what is in flight on it until the client closes it,
// or the host after moveGrace.
//
// The direct connection is authorised as any direct session: a ticket the
// gateway signed for the same user and page origin, single use, checked
// against the gateway's clock and its revocations; the resume token, sent
// only over the session's own end-to-end encrypted control stream, ties it
// to the session.

// moveGrace is how long the connection a session moved away from stays open
// for the frames and messages in flight on it.
const moveGrace = 2 * time.Second

// moveInputWait bounds how long input from the new connection waits for the
// input streams of the connections before it to end (the client closes them
// when it moves): their key and button events come first, so a key released
// right after the move is not pressed again by a late press.
const moveInputWait = time.Second

// sessionMove is a session's part in moving: the connection and control
// stream that can change, its resume token, and for a session that moved into
// another one (the connection it came on carries that one now), that one.
type sessionMove struct {
	conn  *moveConn
	ctrl  *moveStream             // the control stream (set once the session has one)
	token string                  // Welcome.Resume ("": this session cannot move: the splice, WebSocket)
	path  atomic.Pointer[string]  // the session's path now, for its log lines
	into  atomic.Pointer[Session] // the session this one's connection moved into

	mu    sync.Mutex
	live  int           // open input streams of the session's own (first) connection
	moved time.Time     // when it moved
	ended chan struct{} // closed and replaced when one of them ends
}

// newSessionMove prepares a session on c to move (and, as a connection on
// the direct path, to be moved into another session).
func newSessionMove(c transport.Conn, path string) *sessionMove {
	m := &sessionMove{conn: newMoveConn(c), ended: make(chan struct{})}
	// Sessions that end at the client can move: their client is a
	// WebTransport client that tries the direct path.
	if path == "relay" || path == "relay-splice" {
		m.token = auth.RandomToken(18)
	}
	m.path.Store(&path)
	return m
}

// resumeToken is the welcome's Resume: set while the session may move to
// the direct path (it is not on it, and the PC has a direct endpoint up).
func (s *Session) resumeToken() string {
	if s.move == nil || s.move.token == "" || !s.a.directUp.Load() {
		return ""
	}
	return s.move.token
}

// pathNow is the session's path now (direct after a move).
func (s *Session) pathNow() string {
	if s.move == nil {
		return s.meta.Path
	}
	return *s.move.path.Load()
}

// resumeInto runs a session whose hello asks to move another one onto its
// connection (Hello.Resume): it hands the connection to that session and
// keeps it open until that session ends or moves on. Refused, it tells the
// client why and closes the connection (the session goes on where it was).
func (s *Session) resumeInto() error {
	t, why := s.a.resumeTarget(s)
	if t != nil {
		if err := t.adopt(s); err != nil {
			why = err.Error()
			t = nil
		}
	}
	if t == nil {
		s.log.Info("direct path: the client's move of its session onto this connection refused", "why", why)
		if s.sendJSON(proto.Notice{T: "error", Level: "warn", Msg: "move refused: " + why}) == nil {
			select {
			case <-s.c.Context().Done():
			case <-time.After(byeGrace):
			}
		}
		s.c.Close(transport.CodeNone, "move refused")
		return errClosed
	}
	// The connection is t's now; this goroutine (its handler) holds it
	// until t ends, or the client closes it after t moved on.
	select {
	case <-t.ctx.Done():
	case <-s.c.Context().Done():
	}
	return errClosed
}

// resumeTarget is the session a hello with Resume moves onto its connection,
// or why none.
func (a *Agent) resumeTarget(n *Session) (*Session, string) {
	a.mu.Lock()
	t := a.active
	ended, revoked := a.ended[n.meta.User]
	a.mu.Unlock()
	switch {
	case n.meta.Path != "direct":
		return nil, "only a connection on the direct path takes a session over"
	case t == nil || t.move == nil || t.move.token == "":
		return nil, "no session that can move is running"
	case subtle.ConstantTimeCompare([]byte(t.move.token), []byte(n.hello.Resume)) != 1:
		return nil, "the resume token is not the running session's"
	case t.meta.User != n.meta.User:
		return nil, "the running session is another user's"
	case t.meta.Origin != "" && !strings.EqualFold(t.meta.Origin, n.meta.Origin):
		return nil, "the running session came from another page origin"
	case revoked && ended.gen > n.meta.Gen:
		return nil, ended.reason
	}
	return t, ""
}

// adopt moves the session onto the connection of n (a session on the direct
// path whose hello carries this session's resume token): its control stream
// first, whose first message is Resumed, then its frames and datagrams. The
// connection before is closed after moveGrace.
func (s *Session) adopt(n *Session) error {
	raw, _ := n.move.conn.current()
	s.ctrlMu.Lock()
	defer s.ctrlMu.Unlock()
	s.ctrlDL.Lock()
	closing := s.closing
	s.ctrlDL.Unlock()
	if closing || s.ctrlTorn || s.ctx.Err() != nil || s.move.ctrl == nil || n.move.ctrl == nil {
		return errClosed
	}
	from := s.pathNow()
	if from == "direct" {
		return errors.New("the session is on the direct path already")
	}
	st := n.move.ctrl.stream()
	s.move.ctrl.swap(st)
	b, _ := json.Marshal(proto.Resumed{T: "resumed", Session: s.id, Path: n.meta.Path})
	_ = s.ctrl.SetWriteDeadline(time.Now().Add(byeGrace))
	if err := s.writeCtrl(b); err != nil {
		// The session stays where it was: its control stream back (not
		// torn: only the new one's write failed), with no read deadline.
		s.move.ctrl.undo(st)
		s.ctrlTorn = false
		return fmt.Errorf("the new connection's control stream: %w", err)
	}
	s.move.mu.Lock()
	s.move.moved = time.Now()
	s.move.mu.Unlock()
	old := s.move.conn.swap(raw)
	n.move.into.Store(s)
	path := n.meta.Path
	s.move.path.Store(&path)
	s.rate.setPath(true) // the direct path ends at the client
	s.log.Info("session moved to the direct path: the stream goes on over it, without a restart", "from", from,
		"remote", raw.RemoteAddr().String(), "after_s", time.Since(s.start()).Round(time.Second).Seconds())
	time.AfterFunc(moveGrace, func() {
		_ = transport.CloseConnection(old, transport.CodeReplaced, "the session moved to the direct path")
	})
	return nil
}

// start is when the session started (its connection's first moment is
// close enough: the session started on it).
func (s *Session) start() time.Time {
	if s.move == nil {
		return time.Now()
	}
	return s.move.conn.started
}

// inputSession is the session input on s's streams is for: the one s's
// connection moved into, if it did.
func (s *Session) inputSession() *Session {
	if s.move != nil {
		if t := s.move.into.Load(); t != nil {
			return t
		}
	}
	return s
}

// moveCtrl wraps the session's control stream so that a move can replace it
// (the stream itself for a session that cannot take part in one).
func (s *Session) moveCtrl(st transport.BidiStream) transport.BidiStream {
	if s.move == nil {
		return st
	}
	s.move.ctrl = newMoveStream(st)
	return s.move.ctrl
}

// inputReady reports whether input read from an input stream of s's
// connection may be injected: for the session s's connection belongs to
// now (inputSession), the active one, once the input streams of that
// session's first connection have ended (the client closes them when it
// moves; moveInputWait at most), so their events come first.
func (s *Session) inputReady() bool {
	t := s.inputSession()
	if t != s && t.move != nil {
		t.move.waitOldInput()
	}
	return t.inputAllowed()
}

// inputStream counts an open input stream of s's own connection, for the
// order across a move (waitOldInput), and returns its end.
func (s *Session) inputStream() (end func()) {
	m := s.move
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	m.live++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.live--
		close(m.ended)
		m.ended = make(chan struct{})
		m.mu.Unlock()
	}
}

// waitOldInput waits until no input stream of the session's first
// connection is open, moveInputWait after the move at most.
func (m *sessionMove) waitOldInput() {
	for {
		m.mu.Lock()
		live, left, ended := m.live, moveInputWait-time.Since(m.moved), m.ended
		m.mu.Unlock()
		if live <= 0 || left <= 0 {
			return
		}
		select {
		case <-ended:
		case <-time.After(left):
		}
	}
}

// logBackgroundDirect logs the client's direct path attempts while the
// session streams through the relay ("direct" control message, sent when
// their outcome changes).
func (s *Session) logBackgroundDirect(r *proto.DirectReport) {
	if r == nil {
		return
	}
	s.log.Info("direct path: the client's attempt in the background failed, the session stays on the relay",
		"why", r.Why, "tried", r.Summary())
}

// pathHandler adds the session's path now to each of its log lines (after
// a move: direct), where a.log.With would fix the path it started on.
type pathHandler struct {
	slog.Handler
	path *atomic.Pointer[string]
}

func (h pathHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r2.AddAttrs(slog.String("path", *h.path.Load()))
	r.Attrs(func(a slog.Attr) bool {
		r2.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, r2)
}

func (h pathHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return pathHandler{h.Handler.WithAttrs(as), h.path}
}

func (h pathHandler) WithGroup(name string) slog.Handler {
	return pathHandler{h.Handler.WithGroup(name), h.path}
}

// ---------------------------------------------------------------------------
// The connection and control stream that move

// moveConn is a session's connection, which a move replaces (swap): every
// call goes to the connection current at that moment; datagrams come from
// all of them (the ones in flight on the connection before still count:
// acknowledgements, reports); its context ends with the current connection
// (not with the ones it moved away from) or Close.
type moveConn struct {
	mu      sync.Mutex
	cur     transport.Conn
	conns   []transport.Conn // every connection, the current one last
	moved   context.Context  // the current connection's: cancelled when a swap replaces it
	unmoved context.CancelFunc
	pumping bool // datagrams are being read (ReceiveDatagram was called)
	started time.Time

	dgs    chan []byte
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func newMoveConn(c transport.Conn) *moveConn {
	m := &moveConn{cur: c, conns: []transport.Conn{c}, dgs: make(chan []byte, 256), started: time.Now()}
	m.ctx, m.cancel = context.WithCancelCause(context.Background())
	m.moved, m.unmoved = context.WithCancel(context.Background())
	m.watch(c)
	return m
}

// watch ends m's context when c ends while it is the current connection.
func (m *moveConn) watch(c transport.Conn) {
	context.AfterFunc(c.Context(), func() {
		m.mu.Lock()
		cur := m.cur == c
		m.mu.Unlock()
		if cur {
			m.cancel(context.Cause(c.Context()))
		}
	})
}

// current returns the current connection and a context cancelled when a
// swap replaces it.
func (m *moveConn) current() (transport.Conn, context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur, m.moved
}

// swap makes c the current connection and returns the one before.
func (m *moveConn) swap(c transport.Conn) transport.Conn {
	m.mu.Lock()
	old := m.cur
	m.cur = c
	m.conns = append(m.conns, c)
	m.unmoved()
	m.moved, m.unmoved = context.WithCancel(context.Background())
	pump := m.pumping
	m.mu.Unlock()
	m.watch(c)
	if pump {
		go m.pump(c)
	}
	return old
}

// pump feeds c's datagrams to ReceiveDatagram until c ends.
func (m *moveConn) pump(c transport.Conn) {
	for {
		d, err := c.ReceiveDatagram(m.ctx)
		if err != nil {
			return
		}
		select {
		case m.dgs <- d:
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *moveConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	if !m.pumping {
		// The first reader starts reading the current connection (and
		// each later one): a session on a connection that moves into
		// another never reads its datagrams, which are that one's.
		m.pumping = true
		go m.pump(m.cur)
	}
	m.mu.Unlock()
	select {
	case d := <-m.dgs:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, context.Cause(m.ctx)
	}
}

func (m *moveConn) AcceptStream(ctx context.Context) (transport.BidiStream, error) {
	c, _ := m.current()
	return c.AcceptStream(ctx)
}

func (m *moveConn) OpenStreamSync(ctx context.Context) (transport.BidiStream, error) {
	c, _ := m.current()
	return c.OpenStreamSync(ctx)
}

func (m *moveConn) AcceptUniStream(ctx context.Context) (transport.RecvStream, error) {
	c, _ := m.current()
	return c.AcceptUniStream(ctx)
}

// OpenUniStreamSync opens a stream on the current connection; one that
// waited for the connection a move replaced opens on the new one.
func (m *moveConn) OpenUniStreamSync(ctx context.Context) (transport.SendStream, error) {
	for {
		c, moved := m.current()
		octx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(moved, cancel)
		st, err := c.OpenUniStreamSync(octx)
		stop()
		cancel()
		if err == nil || ctx.Err() != nil || moved.Err() == nil {
			return st, err
		}
	}
}

func (m *moveConn) SendDatagram(b []byte) error {
	c, _ := m.current()
	return c.SendDatagram(b)
}

// Close closes every connection (the ones moved away from may still be in
// their grace) and ends m's context.
func (m *moveConn) Close(code uint32, msg string) error {
	m.mu.Lock()
	cur, conns := m.cur, m.conns
	m.mu.Unlock()
	for _, c := range conns {
		if c != cur {
			_ = c.Close(code, msg)
		}
	}
	err := cur.Close(code, msg)
	m.cancel(errors.New("session closed"))
	return err
}

func (m *moveConn) Context() context.Context { return m.ctx }

func (m *moveConn) RemoteAddr() net.Addr {
	c, _ := m.current()
	return c.RemoteAddr()
}

// PartialDelivery is transport.PartialDelivery of the current connection.
func (m *moveConn) PartialDelivery() bool {
	c, _ := m.current()
	return transport.PartialDelivery(c)
}

// MediaControl is transport.MediaControl of the current connection.
func (m *moveConn) MediaControl() *cc.Media {
	c, _ := m.current()
	return transport.MediaControl(c)
}

// moveStream is a session's control stream, which a move replaces: messages
// are read whole, from each stream before until it ends (the client closes
// it when it moves; moveGrace at most), then from the new one, so a message
// the client sent before it moved is still read and a message never spans
// two streams; writes go to the current one (the session writes one message
// per Write, under its ctrlMu, which a move holds).
type moveStream struct {
	mu      sync.Mutex
	streams []transport.BidiStream // the current one last; the ones before it until read to their end

	rmu  sync.Mutex // one reader
	rbuf []byte
}

func newMoveStream(st transport.BidiStream) *moveStream {
	return &moveStream{streams: []transport.BidiStream{st}}
}

// stream returns the current stream.
func (m *moveStream) stream() transport.BidiStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[len(m.streams)-1]
}

// swap makes st the current stream. The one before is read on until it
// ends, moveGrace at most.
func (m *moveStream) swap(st transport.BidiStream) {
	m.mu.Lock()
	prev := m.streams[len(m.streams)-1]
	m.streams = append(m.streams, st)
	m.mu.Unlock()
	_ = prev.SetReadDeadline(time.Now().Add(moveGrace))
}

// undo takes back a swap to st (the move failed): the stream before is the
// current one again, with no read deadline.
func (m *moveStream) undo(st transport.BidiStream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.streams); n > 1 && m.streams[n-1] == st {
		m.streams = m.streams[:n-1]
		_ = m.streams[n-2].SetReadDeadline(time.Time{})
	}
}

func (m *moveStream) Read(p []byte) (int, error) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	for len(m.rbuf) == 0 {
		m.mu.Lock()
		st := m.streams[0]
		m.mu.Unlock()
		b, err := proto.ReadMsg(st, proto.MaxControlMsg)
		if err != nil {
			m.mu.Lock()
			older := len(m.streams) > 1 && m.streams[0] == st
			if older {
				m.streams = m.streams[1:] // that stream ended after a move: read on from the next one
			}
			m.mu.Unlock()
			if older {
				continue
			}
			return 0, err
		}
		// proto.WriteMsg's framing, which the session's reader parses.
		m.rbuf = binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b)))
		m.rbuf = append(m.rbuf, b...)
	}
	n := copy(p, m.rbuf)
	m.rbuf = m.rbuf[n:]
	return n, nil
}

func (m *moveStream) Write(p []byte) (int, error) {
	return m.stream().Write(p)
}

func (m *moveStream) Close() error {
	return m.stream().Close()
}

func (m *moveStream) CancelRead() {
	m.stream().CancelRead()
}

func (m *moveStream) CancelWrite() {
	m.stream().CancelWrite()
}

func (m *moveStream) SetReadDeadline(t time.Time) error {
	return m.stream().SetReadDeadline(t)
}

func (m *moveStream) SetWriteDeadline(t time.Time) error {
	return m.stream().SetWriteDeadline(t)
}

var _ transport.BidiStream = (*moveStream)(nil)
var _ transport.Conn = (*moveConn)(nil)
