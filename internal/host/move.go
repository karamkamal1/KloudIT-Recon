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
	"slices"
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
// running session takes that connection over in two steps. Session.adopt
// answers Resumed on the new connection's control stream and starts reading
// it (after the current control stream) and its datagrams; frames,
// datagrams and control go on over the current connection. Once the client
// shows it moved (commitMove: its first datagram on the new connection, or
// its end of the current control stream, which it closes when it switches)
// they go out on the new one; a new connection that ends before that (the
// client gave up waiting for Resumed) is let go and the session stays where
// it was (abandonMove). So a host slower than the client's wait never moves
// a session the client did not. The pipeline, encoder, audio and state stay
// as they are, so the picture goes on without a restart or a key frame (a
// frame lost in the switch is a loss like any other: the loss-recovery
// ladder answers it). The connection before stays open for what is in flight
// on it until the client closes it, or the host moveGrace after the commit.
//
// The direct connection is authorised as any direct session: a ticket the
// gateway signed for the same user and page origin, single use, checked
// against the gateway's clock and its revocations; the resume token ties it
// to the session. The token travels on the session's control stream: end to
// end encrypted on the UDP relay (it ends at the client), seen by the
// gateway on the splice relay (it ends there), which is the party that
// signs the tickets anyway.

// moveGrace is how long the connection a session moved away from stays open
// for the frames and messages in flight on it.
const moveGrace = 2 * time.Second

// moveConfirmWait bounds how long a move waits for the client to show it
// moved (the client gives the host 5 s for Resumed, then closes the new
// connection, which abandons the move at once).
const moveConfirmWait = 10 * time.Second

// moveInputWait bounds how long input from the new connection waits for the
// input streams of the connections before it to end (the client closes them
// when it moves): their key and button events come first, so a key released
// right after the move is not pressed again by a late press.
const moveInputWait = time.Second

// sessionMove is a session's part in moving: the connection and control
// stream that can change, its resume token, and for a session that moved into
// another one (the connection it came on carries that one now), that one.
type sessionMove struct {
	conn *moveConn
	ctrl *moveStream // the control stream (set once the session has one)
	// token is Welcome.Resume: "" for a session that cannot move (one on
	// the direct path). Sessions on either relay get one; a WebSocket
	// client behind the splice never uses it (the host cannot tell it from
	// a WebTransport one there).
	token string
	path  atomic.Pointer[string]  // the session's path now, for its log lines
	into  atomic.Pointer[Session] // the session this one's connection moved into

	mu    sync.Mutex
	pend  *pendingMove  // a move under way (adopt until commitMove or abandonMove)
	live  int           // open input streams of the session's own (first) connection
	moved time.Time     // when it moved (adopt)
	ended chan struct{} // closed and replaced when one of them ends
}

// pendingMove is a move adopt started: Resumed is on its way to the client
// over the new connection, the session still sends over the current one.
type pendingMove struct {
	n    *Session             // the session that came on the new connection
	raw  transport.Conn       // the new connection
	st   transport.BidiStream // its control stream
	from string               // the path before
	at   time.Time

	reached  <-chan struct{} // reading reached st: the current control stream ended (the client moved)
	heard    <-chan struct{} // the new connection's first datagram (the client moved)
	curEnded <-chan struct{} // the current connection ended meanwhile
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

// adopt starts moving the session onto the connection of n (a session on
// the direct path whose hello carries this session's resume token): Resumed
// first on n's control stream, which from then on is read after the current
// one, and the new connection's datagrams read with the current one's; the
// rest waits for the client to take the move (awaitMove). It needs no
// ctrlMu (nothing else writes to n's control stream yet), so a control
// write stuck on the current path does not hold Resumed up.
func (s *Session) adopt(n *Session) error {
	raw, _ := n.move.conn.current()
	s.ctrlDL.Lock()
	closing := s.closing
	s.ctrlDL.Unlock()
	if closing || s.ctx.Err() != nil || s.move.ctrl == nil || n.move.ctrl == nil {
		return errClosed
	}
	st := n.move.ctrl.stream()
	p := &pendingMove{n: n, raw: raw, st: st, from: s.pathNow(), at: time.Now()}
	s.move.mu.Lock()
	switch {
	case s.move.pend != nil:
		s.move.mu.Unlock()
		return errors.New("a move of the session is under way")
	case p.from == "direct":
		s.move.mu.Unlock()
		return errors.New("the session is on the direct path already")
	}
	s.move.pend = p
	s.move.moved = p.at
	s.move.mu.Unlock()
	// In place before Resumed: the client ends the current control stream
	// and opens input streams on the new connection as soon as it has it.
	p.reached = s.move.ctrl.add(st)
	p.heard, p.curEnded = s.move.conn.add(raw)
	n.move.into.Store(s)
	b, _ := json.Marshal(proto.Resumed{T: "resumed", Session: s.id, Path: n.meta.Path})
	_ = st.SetWriteDeadline(time.Now().Add(byeGrace))
	err := proto.WriteMsg(st, b)
	_ = st.SetWriteDeadline(time.Time{})
	if err != nil {
		s.abandonMove(p, "")
		return fmt.Errorf("the new connection's control stream: %w", err)
	}
	go s.awaitMove(p)
	return nil
}

// awaitMove waits for the client to take the move p (or for its end).
func (s *Session) awaitMove(p *pendingMove) {
	t := time.NewTimer(moveConfirmWait)
	defer t.Stop()
	why := ""
	select {
	case <-p.heard: // its first datagram on the new connection
	case <-p.reached: // it ended the current control stream (or that stream ended)
	case <-p.curEnded: // the current connection ended: the new one is all there is
	case <-p.raw.Context().Done():
	case <-t.C:
		why = fmt.Sprintf("the client did not take it in %s", moveConfirmWait)
	case <-s.ctx.Done():
		return
	}
	if why == "" && p.raw.Context().Err() != nil {
		why = "the client closed the new connection before it took the move (it waited for Resumed in vain)"
	}
	if why != "" {
		s.abandonMove(p, why)
		_ = transport.CloseConnection(p.raw, transport.CodeNone, "move not taken")
		return
	}
	s.commitMove(p)
}

// commitMove moves the session onto p's connection: frames, datagrams and
// control go out on it from now on. The connection before is closed after
// moveGrace.
func (s *Session) commitMove(p *pendingMove) {
	s.ctrlMu.Lock() // a control message is written whole to one stream
	defer s.ctrlMu.Unlock()
	s.move.mu.Lock()
	if s.move.pend != p {
		s.move.mu.Unlock()
		return
	}
	s.move.pend = nil
	s.move.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	s.move.ctrl.commit(p.st)
	old := s.move.conn.commit(p.raw)
	path := p.n.meta.Path
	s.move.path.Store(&path)
	s.rate.setPath(true) // the direct path ends at the client
	s.log.Info("session moved to the direct path: the stream goes on over it, without a restart", "from", p.from,
		"remote", p.raw.RemoteAddr().String(), "after_s", time.Since(s.start()).Round(time.Second).Seconds(),
		"taken_ms", time.Since(p.at).Milliseconds())
	time.AfterFunc(moveGrace, func() {
		_ = transport.CloseConnection(old, transport.CodeReplaced, "the session moved to the direct path")
	})
}

// abandonMove lets the move p go (why: for the log, "" for none): the
// session stays on its connection. One whose current control stream or
// connection ended meanwhile cannot: it ends.
func (s *Session) abandonMove(p *pendingMove, why string) {
	s.move.mu.Lock()
	if s.move.pend != p {
		s.move.mu.Unlock()
		return
	}
	s.move.pend = nil
	s.move.mu.Unlock()
	p.n.move.into.Store(nil)
	kept := s.move.ctrl.drop(p.st)
	s.move.conn.drop(p.raw)
	if why != "" {
		s.log.Info("direct path: the move to the direct path was not taken, the session stays where it was", "why", why, "on", p.from)
	}
	if !kept {
		s.end("the control stream ended during a move to the direct path")
	}
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

// inputTarget returns the session input read from an input stream of s's
// connection is for (inputSession: after a move, the session that took the
// connection over; s itself then runs nothing but resumeInto, so any
// per-session state input touches, acknowledgements, ROI and the like, must
// go to the target, never to s) and whether it may be injected now: for the
// active session, once the input streams of that session's first connection
// have ended (the client closes them when it moves; moveInputWait at most),
// so their events come first.
func (s *Session) inputTarget() (*Session, bool) {
	t := s.inputSession()
	if t != s && t.move != nil {
		t.move.waitOldInput()
	}
	return t, t.inputAllowed()
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
		append([]any{"why", r.Why, "tried", r.Summary()}, s.a.clientClockAttr(r)...)...)
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
	pending *pendingConn     // a connection a move is waiting to make current (add)
	moved   context.Context  // the current connection's: cancelled when a swap replaces it
	unmoved context.CancelFunc
	pumping bool // datagrams are being read (ReceiveDatagram was called)
	started time.Time

	dgs    chan []byte
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// pendingConn is a connection a move waits to make current.
type pendingConn struct {
	c        transport.Conn
	heard    chan struct{} // closed at its first datagram
	curEnded chan struct{} // closed when the current connection ended while this one waited
	hOnce    sync.Once
	eOnce    sync.Once
}

func newMoveConn(c transport.Conn) *moveConn {
	m := &moveConn{cur: c, conns: []transport.Conn{c}, dgs: make(chan []byte, 256), started: time.Now()}
	m.ctx, m.cancel = context.WithCancelCause(context.Background())
	m.moved, m.unmoved = context.WithCancel(context.Background())
	m.watch(c)
	return m
}

// watch ends m's context when c ends while it is the current connection,
// unless a move waits with another (that one goes ahead: curEnded).
func (m *moveConn) watch(c transport.Conn) {
	context.AfterFunc(c.Context(), func() {
		m.mu.Lock()
		cur := m.cur == c
		p := m.pending
		m.mu.Unlock()
		switch {
		case cur && p != nil:
			p.eOnce.Do(func() { close(p.curEnded) })
		case cur:
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

// add makes c the connection a move waits to make current: its datagrams
// are read with the current one's (heard is closed at the first), and the
// current connection's end no longer ends m (curEnded is closed instead).
func (m *moveConn) add(c transport.Conn) (heard, curEnded <-chan struct{}) {
	p := &pendingConn{c: c, heard: make(chan struct{}), curEnded: make(chan struct{})}
	m.mu.Lock()
	m.pending = p
	curDone := m.cur.Context().Err() != nil
	m.mu.Unlock()
	if curDone {
		close(p.curEnded)
	}
	m.watch(c)
	go m.pump(c, func() { p.hOnce.Do(func() { close(p.heard) }) })
	return p.heard, p.curEnded
}

// commit makes c, the connection add waits with, the current connection
// and returns the one before.
func (m *moveConn) commit(c transport.Conn) transport.Conn {
	m.mu.Lock()
	old := m.cur
	m.cur = c
	m.conns = append(m.conns, c)
	m.pending = nil
	m.unmoved()
	m.moved, m.unmoved = context.WithCancel(context.Background())
	m.mu.Unlock()
	if c.Context().Err() != nil { // it ended before it became current (watch let it be)
		m.cancel(context.Cause(c.Context()))
	}
	return old
}

// drop lets c, the connection add waits with, go: the current one stays,
// and ends m if it ended meanwhile.
func (m *moveConn) drop(c transport.Conn) {
	m.mu.Lock()
	if m.pending != nil && m.pending.c == c {
		m.pending = nil
	}
	cur := m.cur
	m.mu.Unlock()
	if cur.Context().Err() != nil {
		m.cancel(context.Cause(cur.Context()))
	}
}

// pump feeds c's datagrams to ReceiveDatagram until c ends (first, if set,
// at each one).
func (m *moveConn) pump(c transport.Conn, first func()) {
	for {
		d, err := c.ReceiveDatagram(m.ctx)
		if err != nil {
			return
		}
		if first != nil {
			first()
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
		go m.pump(m.cur, nil)
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
// their grace, one a move waits with) and ends m's context.
func (m *moveConn) Close(code uint32, msg string) error {
	m.mu.Lock()
	cur, conns := m.cur, m.conns
	if m.pending != nil {
		conns = append(slices.Clone(conns), m.pending.c)
	}
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
// are read whole, from each stream until it ends (the client closes the one
// before when it moves; moveGrace after the commit at most), then from the
// next, so a message the client sent before it moved is still read and a
// message never spans two streams; writes go to the current one (the
// session writes one message per Write, under its ctrlMu, which a commit
// holds).
type moveStream struct {
	mu      sync.Mutex
	streams []transport.BidiStream // read in this order, each until it ends (the first is being read)
	w       transport.BidiStream   // the current one: writes go to it
	reached map[transport.BidiStream]chan struct{}

	rmu  sync.Mutex // one reader
	rbuf []byte
}

func newMoveStream(st transport.BidiStream) *moveStream {
	return &moveStream{streams: []transport.BidiStream{st}, w: st, reached: map[transport.BidiStream]chan struct{}{}}
}

// stream returns the current stream.
func (m *moveStream) stream() transport.BidiStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.w
}

// add has st read once the streams before it end; the channel is closed
// when reading reaches it.
func (m *moveStream) add(st transport.BidiStream) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streams = append(m.streams, st)
	ch := make(chan struct{})
	m.reached[st] = ch
	return ch
}

// commit makes st (added) the current stream. The ones before it are read
// on until they end, moveGrace at most.
func (m *moveStream) commit(st transport.BidiStream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.w = st
	for _, x := range m.streams {
		if x == st {
			break
		}
		_ = x.SetReadDeadline(time.Now().Add(moveGrace))
	}
}

// drop takes back an add of st (the move was not taken). It reports false
// when reading has reached st already: the streams before it ended.
func (m *moveStream) drop(st transport.BidiStream) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.Index(m.streams, st)
	if i == 0 {
		return false
	}
	if i > 0 {
		m.streams = slices.Delete(m.streams, i, i+1)
	}
	delete(m.reached, st)
	return true
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
			var reached chan struct{}
			if older {
				m.streams = m.streams[1:] // that stream ended after a move: read on from the next one
				reached = m.reached[m.streams[0]]
				delete(m.reached, m.streams[0])
			}
			m.mu.Unlock()
			if reached != nil {
				close(reached)
			}
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
