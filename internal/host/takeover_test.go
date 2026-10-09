package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// stallCtrl is a control stream whose writes, while stalled, block until
// their deadline, like a QUIC stream on a path that died (nothing is
// acknowledged, the send buffer is full). SetWriteDeadline moves the
// deadline of a blocked write, as quic-go's does. A write that times out
// keeps half of its bytes, as a QUIC write that queued part of its data.
type stallCtrl struct {
	mu       sync.Mutex
	stalled  bool
	deadline time.Time
	wake     chan struct{} // closed when the deadline or stalled changes
	blocked  int           // writes waiting now
	buf      bytes.Buffer
	// onWrite sees each complete write (the client reading it), outside mu.
	onWrite func(p []byte)
}

func newStallCtrl(stalled bool) *stallCtrl {
	return &stallCtrl{stalled: stalled, wake: make(chan struct{})}
}

func (c *stallCtrl) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked++
	defer func() { c.blocked-- }() // runs before the Unlock above
	for c.stalled {
		dl, wake := c.deadline, c.wake
		if !dl.IsZero() && !time.Now().Before(dl) {
			c.buf.Write(p[:len(p)/2])
			return len(p) / 2, os.ErrDeadlineExceeded
		}
		c.mu.Unlock()
		var timeout <-chan time.Time
		if !dl.IsZero() {
			tm := time.NewTimer(time.Until(dl))
			timeout = tm.C
			defer tm.Stop()
		}
		select {
		case <-wake:
		case <-timeout:
		}
		c.mu.Lock()
	}
	c.buf.Write(p)
	if c.onWrite != nil {
		c.mu.Unlock()
		c.onWrite(p)
		c.mu.Lock()
	}
	return len(p), nil
}

func (c *stallCtrl) signal() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func (c *stallCtrl) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	c.signal()
	return nil
}

func (c *stallCtrl) unstall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = false
	c.signal()
}

func (c *stallCtrl) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocked
}

// messages returns the types of the complete messages written, and whether
// bytes follow that are no complete message.
func (c *stallCtrl) messages(t *testing.T) (types []string, torn bool) {
	t.Helper()
	c.mu.Lock()
	r := bytes.NewReader(append([]byte(nil), c.buf.Bytes()...))
	c.mu.Unlock()
	for r.Len() > 0 {
		b, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if err != nil {
			return types, true
		}
		var m struct{ T, Msg string }
		if err := json.Unmarshal(b, &m); err != nil {
			return types, true
		}
		types = append(types, m.T)
	}
	return types, false
}

func (*stallCtrl) Read([]byte) (int, error)        { return 0, io.EOF }
func (*stallCtrl) Close() error                    { return nil }
func (*stallCtrl) CancelRead()                     {}
func (*stallCtrl) CancelWrite()                    {}
func (*stallCtrl) SetReadDeadline(time.Time) error { return nil }

// closeConn records how the host closed the connection; peerClose ends it
// from the client's side.
type closeConn struct {
	transport.Conn
	ctx       context.Context
	peerClose context.CancelFunc
	mu        sync.Mutex
	code      uint32
	n         int
	at        time.Time
}

func (c *closeConn) Context() context.Context { return c.ctx }

func (c *closeConn) Close(code uint32, msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code, c.n, c.at = code, c.n+1, time.Now()
	c.peerClose()
	return nil
}

func takeoverSession(a *Agent, ctrl transport.BidiStream) (*Session, *closeConn) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &closeConn{}
	c.ctx, c.peerClose = context.WithCancel(context.Background())
	return &Session{a: a, c: c, ctrl: ctrl, ctx: ctx, cancel: cancel, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, c
}

// takeover makes old the active session and times setActive(new), as a new
// connection's session does before its welcome.
func takeover(t *testing.T, oldCtrl transport.BidiStream, before func(old *Session)) (time.Duration, *Session, *closeConn) {
	t.Helper()
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	old, oc := takeoverSession(a, oldCtrl)
	t.Cleanup(old.cancel)
	a.setActive(old)
	if before != nil {
		before(old)
	}
	nw, _ := takeoverSession(a, newStallCtrl(false))
	t.Cleanup(nw.cancel)
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		a.setActive(nw)
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		return d, old, oc
	case <-time.After(30 * time.Second):
		t.Fatal("setActive did not return")
	}
	return 0, nil, nil
}

// A takeover does not wait on the replaced session's control stream when the
// old client's path died: the write in progress and the bye get byeGrace,
// the writes queued behind it return at once, and the new session goes on to
// its welcome. Before, close() sent the bye through sendJSON behind every
// queued write, each holding the stream for its 5 s deadline.
func TestTakeoverDeadControlPath(t *testing.T) {
	ctrl := newStallCtrl(true)
	var queued sync.WaitGroup
	var errs []error
	var errsMu sync.Mutex
	d, old, oc := takeover(t, ctrl, func(old *Session) {
		for i := 0; i < 3; i++ { // one writing, two queued on ctrlMu (reportDropped, rate, cursor ...)
			queued.Add(1)
			go func() {
				defer queued.Done()
				err := old.sendJSON(proto.Dropped{T: "dropped", Gen: 1, FromSeq: uint32(i), Count: 1})
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}()
		}
		for ctrl.waiting() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // the other two are queued
	})
	if d > 2*byeGrace {
		t.Fatalf("takeover waited %v on the old session's dead control stream (bound %v)", d, 2*byeGrace)
	}
	waited := make(chan struct{})
	go func() { queued.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("control writes queued on the old session still block")
	}
	errsMu.Lock()
	for _, err := range errs {
		if err == nil {
			t.Errorf("a control write on the dead stream succeeded")
		}
	}
	errsMu.Unlock()
	if old.ctx.Err() == nil {
		t.Fatal("old session not cancelled")
	}
	if oc.n != 1 || oc.code != transport.CodeReplaced {
		t.Fatalf("old connection closed %d times, code %d", oc.n, oc.code)
	}
	// The timed-out write left part of a message: no bye after it (the
	// client could not parse it), and nothing else either.
	if types, torn := ctrl.messages(t); len(types) != 0 || !torn {
		t.Fatalf("old control stream holds %q (torn %v)", types, torn)
	}
	if err := old.sendJSON(proto.Notice{T: "notice", Msg: "late"}); !errors.Is(err, errClosed) {
		t.Fatalf("control write after close: %v", err)
	}
	t.Logf("takeover over a dead control path took %v", d)
}

// On a live path the bye arrives whole, after the write in progress, which
// close() does not cut short when it finishes within byeGrace. The client
// ends the session when it reads the bye; close() closes the connection only
// then: closing it resets the session's streams, and with them a bye still in
// flight, after which the client would reconnect and take the session back.
// A client that does not confirm (one from before this change) gets byeGrace,
// in which a live path delivers the bye.
func TestTakeoverLiveControlPath(t *testing.T) {
	for _, c := range []struct {
		name          string
		slow, confirm bool
	}{{"fast", false, true}, {"busy", true, true}, {"no confirmation", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			ctrl := newStallCtrl(c.slow)
			var byeAt time.Time
			d, _, oc := takeover(t, ctrl, func(old *Session) {
				ctrl.onWrite = func(p []byte) {
					if bytes.Contains(p, []byte(`"t":"bye"`)) {
						byeAt = time.Now()
						if c.confirm {
							time.AfterFunc(20*time.Millisecond, old.c.(*closeConn).peerClose) // a round trip later
						}
					}
				}
				if !c.slow {
					old.sendJSON(proto.Notice{T: "notice", Msg: "before"})
					return
				}
				go old.sendJSON(proto.Notice{T: "notice", Msg: "before"}) // blocked, as on a busy path
				for ctrl.waiting() == 0 {
					time.Sleep(time.Millisecond)
				}
				time.AfterFunc(100*time.Millisecond, ctrl.unstall)
			})
			types, torn := ctrl.messages(t)
			if torn || len(types) != 2 || types[0] != "notice" || types[1] != "bye" {
				t.Fatalf("old control stream holds %q (torn %v), want notice and bye", types, torn)
			}
			if oc.n != 1 || oc.code != transport.CodeReplaced {
				t.Fatalf("closed %d times with code %d", oc.n, oc.code)
			}
			wait := oc.at.Sub(byeAt)
			switch {
			case c.confirm && (wait < 15*time.Millisecond || d > byeGrace):
				t.Fatalf("closed %v after the bye (the client confirms after 20 ms), takeover took %v", wait, d)
			case !c.confirm && (wait < byeGrace-10*time.Millisecond || d > 2*byeGrace):
				t.Fatalf("closed %v after the bye without the client's confirmation (want byeGrace), takeover took %v", wait, d)
			}
		})
	}
}

// tearCtrl's write number tearAt (from 1) keeps keep/2 of its bytes and
// fails, as a quic-go write that reaches its deadline on a stalled path
// after it queued half of its message (keep 1) or none of it (keep 0); the
// stream stays open, as webtransport-go leaves it after a timeout.
type tearCtrl struct {
	*stallCtrl
	tearAt, keep, n int
}

func (c *tearCtrl) Write(p []byte) (int, error) {
	c.n++
	if c.n != c.tearAt {
		return c.stallCtrl.Write(p)
	}
	k := len(p) * c.keep / 2
	c.mu.Lock()
	c.buf.Write(p[:k])
	c.mu.Unlock()
	return k, os.ErrDeadlineExceeded
}

// A control write that fails ends the session: after a partial write the
// client cannot parse the stream, after a lost one it misses the message (a
// VideoConfig), and on a connection that stays up it would never notice.
// Before, the session went on writing after the torn message, and the
// client's control channel died without a reconnect.
func TestFailedControlWriteEndsSession(t *testing.T) {
	for _, keep := range []int{1, 0} {
		t.Run(map[int]string{1: "torn", 0: "lost"}[keep], func(t *testing.T) {
			ctrl := &tearCtrl{stallCtrl: newStallCtrl(false), tearAt: 2, keep: keep}
			s, c := takeoverSession(&Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}, ctrl)
			defer s.cancel()
			if err := s.sendJSON(proto.Notice{T: "notice", Msg: "first"}); err != nil {
				t.Fatal(err)
			}
			if err := s.sendJSON(proto.Notice{T: "notice", Msg: string(bytes.Repeat([]byte("x"), 3000))}); err == nil {
				t.Fatal("the failed write reported success")
			}
			for i := 0; i < 3; i++ {
				if err := s.sendJSON(proto.Notice{T: "notice", Msg: "after"}); !errors.Is(err, errClosed) {
					t.Fatalf("control write %d after the failed one: %v, want errClosed", i, err)
				}
			}
			if s.ctx.Err() == nil {
				t.Fatal("session not cancelled")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				c.mu.Lock()
				n, code := c.n, c.code
				c.mu.Unlock()
				if n == 1 && code == transport.CodeProtocol {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("connection closed %d times, code %d; want once with CodeProtocol", n, code)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if types, torn := ctrl.messages(t); len(types) != 1 || torn != (keep == 1) {
				t.Fatalf("control stream holds %q (torn %v)", types, torn)
			}
		})
	}
}
