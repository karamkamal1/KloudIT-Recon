package host

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// movePipe is a control stream whose read side the test writes.
type movePipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newMovePipe() *movePipe {
	r, w := io.Pipe()
	return &movePipe{r: r, w: w}
}

func (p *movePipe) Read(b []byte) (int, error)       { return p.r.Read(b) }
func (p *movePipe) Write(b []byte) (int, error)      { return len(b), nil }
func (p *movePipe) Close() error                     { return nil }
func (p *movePipe) CancelRead()                      {}
func (p *movePipe) CancelWrite()                     {}
func (p *movePipe) SetReadDeadline(time.Time) error  { return nil }
func (p *movePipe) SetWriteDeadline(time.Time) error { return nil }

// A session's control stream across a move (moveStream): its messages are
// read whole from the stream before until that one ends, also the ones the
// client sent on it after the host moved (the client moves when "resumed"
// arrives), then from the new one; a message cut off by the old stream's
// end is dropped, never spliced with the new stream's bytes; the new
// stream's end is the session's.
func TestMoveStreamReadsWholeMessages(t *testing.T) {
	old, nw := newMovePipe(), newMovePipe()
	m := newMoveStream(old)
	got := make(chan string, 4)
	errc := make(chan error, 1)
	go func() {
		for {
			b, err := proto.ReadMsg(m, proto.MaxControlMsg)
			if err != nil {
				errc <- err
				return
			}
			got <- string(b)
		}
	}()
	go func() {
		proto.WriteMsg(old.w, []byte(`{"t":"a"}`))
		m.add(nw)
		m.commit(nw)
		go proto.WriteMsg(nw.w, []byte(`{"t":"c"}`)) // the new stream's first message comes first
		time.Sleep(50 * time.Millisecond)
		proto.WriteMsg(old.w, []byte(`{"t":"b"}`)) // sent before the client moved
		old.w.Write([]byte{9, 0})                  // half of a header
		old.w.Close()
	}()
	for _, want := range []string{`{"t":"a"}`, `{"t":"b"}`, `{"t":"c"}`} {
		select {
		case g := <-got:
			if g != want {
				t.Fatalf("read %s, want %s", g, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no message %s", want)
		}
	}
	nw.w.Close()
	select {
	case err := <-errc:
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			t.Fatalf("end of the new stream: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the end of the new stream did not end the reads")
	}

	// A move not taken (drop): the stream before is the only one again, and
	// writes never left it.
	a, b := newMovePipe(), newMovePipe()
	m = newMoveStream(a)
	m.add(b)
	if m.stream() != a {
		t.Fatal("writes went to a stream added but not committed")
	}
	if !m.drop(b) || m.stream() != a || len(m.streams) != 1 {
		t.Fatal("drop did not restore the stream before")
	}
	// Reading reached the added stream (the one before ended): too late.
	a, b = newMovePipe(), newMovePipe()
	m = newMoveStream(a)
	reached := m.add(b)
	go func() {
		a.w.Close()
		proto.WriteMsg(b.w, []byte(`{"t":"x"}`))
	}()
	if msg, err := proto.ReadMsg(m, proto.MaxControlMsg); err != nil || string(msg) != `{"t":"x"}` {
		t.Fatalf("read %q, %v", msg, err)
	}
	select {
	case <-reached:
	default:
		t.Fatal("reaching the added stream was not signalled")
	}
	if m.drop(b) {
		t.Fatal("dropped the stream being read")
	}
}

// moveFakeConn is a connection whose datagrams the test sends and whose end it
// controls.
type moveFakeConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	dgs    chan []byte
	closed atomic.Bool
}

func newMoveFakeConn() *moveFakeConn {
	c := &moveFakeConn{dgs: make(chan []byte, 8)}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c
}

func (c *moveFakeConn) AcceptStream(ctx context.Context) (transport.BidiStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *moveFakeConn) OpenStreamSync(ctx context.Context) (transport.BidiStream, error) {
	return nil, errors.New("no streams")
}
func (c *moveFakeConn) AcceptUniStream(ctx context.Context) (transport.RecvStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *moveFakeConn) OpenUniStreamSync(ctx context.Context) (transport.SendStream, error) {
	return nil, errors.New("no streams")
}
func (c *moveFakeConn) SendDatagram([]byte) error { return nil }
func (c *moveFakeConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-c.dgs:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}
func (c *moveFakeConn) Close(uint32, string) error { c.closed.Store(true); c.cancel(); return nil }
func (c *moveFakeConn) Context() context.Context   { return c.ctx }
func (c *moveFakeConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 168, 5, 20), Port: 50000}
}

// A connection a move waits with (moveConn.add, review of the second
// hardware run): its datagrams are read and the first one is signalled, it
// is not current until commit, and dropping it (the client gave the move
// up) leaves the session on its connection; the current connection's end
// while the move waits does not end the session but lets the move go ahead.
func TestMoveConnPending(t *testing.T) {
	cur, nw := newMoveFakeConn(), newMoveFakeConn()
	m := newMoveConn(cur)
	heard, curEnded := m.add(nw)
	if c, _ := m.current(); c != cur {
		t.Fatal("the pending connection became current at add")
	}
	nw.dgs <- []byte{1}
	select {
	case <-heard:
	case <-time.After(2 * time.Second):
		t.Fatal("the pending connection's first datagram was not signalled")
	}
	if d, err := m.ReceiveDatagram(context.Background()); err != nil || d[0] != 1 {
		t.Fatalf("the pending connection's datagram: %v %v", d, err)
	}
	// Dropped: the session stays on cur; nw's end means nothing to it.
	m.drop(nw)
	nw.Close(0, "")
	time.Sleep(50 * time.Millisecond)
	if m.Context().Err() != nil {
		t.Fatal("a dropped connection's end ended the session")
	}
	select {
	case <-curEnded:
		t.Fatal("curEnded without an end")
	default:
	}

	// The current connection ends while a move waits: the move goes ahead.
	nw2 := newMoveFakeConn()
	_, curEnded = m.add(nw2)
	cur.Close(0, "")
	select {
	case <-curEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("the current connection's end was not signalled")
	}
	if m.Context().Err() != nil {
		t.Fatal("the current connection's end ended the session while a move waited")
	}
	if old := m.commit(nw2); old != cur {
		t.Fatal("commit returned another connection")
	}
	if c, _ := m.current(); c != nw2 || m.Context().Err() != nil {
		t.Fatal("commit did not make the new connection current")
	}
	nw2.Close(0, "")
	select {
	case <-m.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the current connection's end after the commit did not end the session")
	}

	// Dropped after the current one ended: the session ends.
	a, b := newMoveFakeConn(), newMoveFakeConn()
	m = newMoveConn(a)
	m.add(b)
	a.Close(0, "")
	time.Sleep(20 * time.Millisecond)
	m.drop(b)
	if m.Context().Err() == nil {
		t.Fatal("a session whose connection ended stays after its move was dropped")
	}
	// Close closes a pending connection too.
	c, d := newMoveFakeConn(), newMoveFakeConn()
	m = newMoveConn(c)
	m.add(d)
	m.Close(0, "")
	if !c.closed.Load() || !d.closed.Load() {
		t.Fatal("Close left a connection open")
	}
}

// A move from the relay to the direct path: the reports' delay drops by the
// relay's hop (here a tunnelled detour of 20 ms), with frames still on their
// way over the old path in the first reports after it. The rate controller
// takes the lower delay as its new base and lowers nothing (the move leaves
// it as it is).
func TestRateAcrossMove(t *testing.T) {
	h := newCtl(t, 20000, 60, seamless)
	h.run(4*time.Second, flat(20*time.Millisecond)) // on the relay
	n := len(h.changes)
	moved := h.elapsed()
	h.run(4*time.Second, func(at time.Duration) time.Duration {
		if at-moved < 150*time.Millisecond && (at/(25*time.Millisecond))%2 == 0 {
			return 20 * time.Millisecond // a report of frames still on the old path
		}
		return time.Millisecond
	})
	if d := h.decreases(n); len(d) > 0 {
		t.Fatalf("decreases after the move: %+v", d)
	}
	if h.cur() < 20000 {
		t.Fatalf("bitrate %d after the move, want the setting 20000", h.cur())
	}
}
