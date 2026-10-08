package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

func TestParseTestFaults(t *testing.T) {
	f, err := parseTestFaults("delay=every:97:200ms, drop=every:193,recovery=skip")
	if err != nil {
		t.Fatal(err)
	}
	if f != (testFaults{delayEvery: 97, delay: 200 * time.Millisecond, dropEvery: 193, recovery: proto.RecoverySkip}) || !f.active() {
		t.Fatalf("parsed %+v", f)
	}
	for n, want := range map[int]string{1: "", 97: "delay", 193: "drop", 194: "delay", 97 * 193: "drop"} {
		drop, delay := f.at(n)
		got := ""
		if drop {
			got = "drop"
		} else if delay == 200*time.Millisecond {
			got = "delay"
		} else if delay != 0 {
			got = delay.String()
		}
		if got != want {
			t.Errorf("frame %d: %q, want %q", n, got, want)
		}
	}
	if f, err := parseTestFaults(""); err != nil || f.active() {
		t.Fatalf("empty: %+v %v", f, err)
	}
	if f, err := parseTestFaults("drop=every:45, intra-refresh"); err != nil || f != (testFaults{dropEvery: 45, intraRefresh: true}) || !f.active() {
		t.Fatalf("intra-refresh: %+v %v", f, err)
	}
	if f, err := parseTestFaults("drop=every:45,ref-recovery"); err != nil || f != (testFaults{dropEvery: 45, refRecovery: true}) || !f.active() {
		t.Fatalf("ref-recovery: %+v %v", f, err)
	}
	if f, err := parseTestFaults("still=after:60"); err != nil || f != (testFaults{stillAfter: 60}) || !f.active() {
		t.Fatalf("still: %+v %v", f, err)
	}
	for _, bad := range []string{"delay=every:97", "delay=every:0:10ms", "delay=every:5:-1ms", "delay=every:5:1h",
		"drop=every:x", "drop=sometimes:3", "drop=every:3:4", "recovery=maybe", "loss=1%", "intra-refresh=1",
		"ref-recovery=1", "ref-recovery,intra-refresh", "ref-recovery,recovery=skip", "recovery=invalidate",
		"still", "still=60", "still=after:0", "still=after:x", "still=every:60",
		"rate-period=1s"} { // the 1.5 controller's hook, gone with it (GUIDE 2.2)
		if _, err := parseTestFaults(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeConn hands out recording frame streams; nothing else is used.
type fakeConn struct {
	transport.Conn
	mu      sync.Mutex
	streams []*fakeStream
}

func (c *fakeConn) OpenUniStreamSync(context.Context) (transport.SendStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &fakeStream{}
	c.streams = append(c.streams, st)
	return st, nil
}

func (c *fakeConn) snapshot() []streamState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]streamState, len(c.streams))
	for i, s := range c.streams {
		s.mu.Lock()
		out[i] = s.streamState
		out[i].data = append([]byte(nil), s.data...)
		s.mu.Unlock()
	}
	return out
}

type fakeStream struct {
	mu sync.Mutex
	streamState
}

type streamState struct {
	data              []byte
	closed, cancelled bool
	doneAt            time.Time // closed or cancelled
}

func (s *fakeStream) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, b...)
	return len(b), nil
}
func (s *fakeStream) Close() error {
	s.mu.Lock()
	s.closed, s.doneAt = true, time.Now()
	s.mu.Unlock()
	return nil
}
func (s *fakeStream) CancelWrite() {
	s.mu.Lock()
	s.cancelled, s.doneAt = true, time.Now()
	s.mu.Unlock()
}
func (s *fakeStream) SetWriteDeadline(time.Time) error { return nil }

// fakeCtrl collects the control messages the host sends.
type fakeCtrl struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *fakeCtrl) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(b)
}
func (c *fakeCtrl) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeCtrl) Close() error                     { return nil }
func (c *fakeCtrl) CancelRead()                      {}
func (c *fakeCtrl) CancelWrite()                     {}
func (c *fakeCtrl) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeCtrl) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeCtrl) dropped(t *testing.T) []proto.Dropped {
	t.Helper()
	c.mu.Lock()
	r := bytes.NewReader(c.buf.Bytes())
	c.mu.Unlock()
	var out []proto.Dropped
	for {
		b, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		var m proto.Dropped
		if err := json.Unmarshal(b, &m); err != nil || m.T != "dropped" {
			t.Fatalf("unexpected control message %s", b)
		}
		out = append(out, m)
	}
}

func testSession(t *testing.T, faults testFaults) (*Session, *fakeConn, *fakeCtrl) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, ctrl := &fakeConn{}, &fakeCtrl{}
	s := &Session{
		a: &Agent{hostClock: media.NewHostClock(), faults: faults}, c: c, ctrl: ctrl, ctx: ctx, cancel: cancel,
		frameQ: make(chan *media.Frame, 6), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s, c, ctrl
}

// TestFrameSenderFaults runs frameSender with the test hook: every 3rd frame
// goes out 80 ms late while the ones after it do not wait, every 5th fails
// mid-stream and is reported dropped (and only those).
func TestFrameSenderFaults(t *testing.T) {
	s, c, ctrl := testSession(t, testFaults{delayEvery: 3, delay: 80 * time.Millisecond, dropEvery: 5})
	go s.frameSender()
	start := time.Now()
	for seq := uint32(0); seq < 10; seq++ {
		s.frameQ <- &media.Frame{Gen: 4, Seq: seq, Data: bytes.Repeat([]byte{byte(seq)}, 100)}
	}
	deadline := time.Now().Add(5 * time.Second)
	var st []streamState
	for {
		st = c.snapshot()
		done := len(st) == 10
		for _, x := range st {
			done = done && (x.closed || x.cancelled)
		}
		if done && len(ctrl.dropped(t)) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames not finished: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, x := range st {
		n := i + 1 // frame count from 1; seq = i
		h, _, payload, err := proto.ParseFrame(x.data)
		switch {
		case n%5 == 0:
			if !x.cancelled || x.closed || len(payload) >= 100 {
				t.Errorf("seq %d: want a stream reset after part of the frame, got cancelled %v closed %v (%d bytes)", i, x.cancelled, x.closed, len(x.data))
			}
		case err != nil || !x.closed || x.cancelled || h.Seq != uint32(i) || len(payload) != 100:
			t.Errorf("seq %d: closed %v cancelled %v, parse %v", i, x.closed, x.cancelled, err)
		case n%3 == 0:
			if late := x.doneAt.Sub(start); late < 80*time.Millisecond {
				t.Errorf("seq %d: sent after %v, want >= 80 ms", i, late)
			}
		default:
			if x.doneAt.Sub(start) >= 80*time.Millisecond {
				t.Errorf("seq %d: held up behind a delayed frame (%v)", i, x.doneAt.Sub(start))
			}
		}
	}
	// Each report goes out on its own goroutine: the two may arrive in
	// either order (the client does not depend on it).
	want := []proto.Dropped{{T: "dropped", Gen: 4, FromSeq: 4, Count: 1}, {T: "dropped", Gen: 4, FromSeq: 9, Count: 1}}
	got := ctrl.dropped(t)
	if len(got) == 2 && got[0].FromSeq > got[1].FromSeq {
		got[0], got[1] = got[1], got[0]
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dropped reports %+v, want %+v", got, want)
	}
}

// TestReportDropped: one message per run of consecutive frames of a
// generation, as a queue overflow drains them.
func TestReportDropped(t *testing.T) {
	s, _, ctrl := testSession(t, testFaults{})
	var fs []*media.Frame
	for _, x := range [][2]int{{7, 250}, {7, 251}, {7, 252}, {7, 254}, {8, 0}, {8, 1}} {
		fs = append(fs, &media.Frame{Gen: uint8(x[0]), Seq: uint32(x[1])})
	}
	s.reportDropped(fs, "queue overflow")
	s.reportDropped(nil, "nothing")
	want := []proto.Dropped{
		{T: "dropped", Gen: 7, FromSeq: 250, Count: 3},
		{T: "dropped", Gen: 7, FromSeq: 254, Count: 1},
		{T: "dropped", Gen: 8, FromSeq: 0, Count: 2},
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(ctrl.dropped(t)) < len(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := ctrl.dropped(t)
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := s.stats.dropped.Load(); n != 6 {
		t.Fatalf("dropped counter %d, want 6", n)
	}
}
