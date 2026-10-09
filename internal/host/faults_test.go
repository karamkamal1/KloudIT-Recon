package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if f, err := parseTestFaults("pre-stage-hold"); err != nil || f != (testFaults{preStageHold: true}) || !f.active() {
		t.Fatalf("pre-stage-hold: %+v %v", f, err)
	}
	if f, err := parseTestFaults("rumble-echo"); err != nil || f != (testFaults{rumbleEcho: true}) || !f.active() {
		t.Fatalf("rumble-echo: %+v %v", f, err)
	}
	if f, err := parseTestFaults("no-window"); err != nil || f != (testFaults{noWindow: true}) || !f.active() {
		t.Fatalf("no-window: %+v %v", f, err)
	}
	f, err = parseTestFaults("thin=every:10:for:3")
	if err != nil || f != (testFaults{thinEvery: 10, thinFor: 3}) || !f.active() {
		t.Fatalf("thin: %+v %v", f, err)
	}
	var under []uint64
	for n := uint64(1); n <= 20; n++ {
		if f.thinAt(n) {
			under = append(under, n)
		}
	}
	if fmt.Sprint(under) != "[7 8 9 17 18 19]" {
		t.Fatalf("thin: frames under pressure %v, want the last 3 of every 10", under)
	}
	if f, err := parseTestFaults("fec-loss=0.03"); err != nil || f != (testFaults{fecLoss: 0.03}) || !f.active() {
		t.Fatalf("fec-loss: %+v %v", f, err)
	}
	if f, err := parseTestFaults("refuse-tickets"); err != nil || f != (testFaults{refuseTickets: true}) || !f.active() {
		t.Fatalf("refuse-tickets: %+v %v", f, err)
	}
	for _, bad := range []string{"delay=every:97", "delay=every:0:10ms", "delay=every:5:-1ms", "delay=every:5:1h",
		"drop=every:x", "drop=sometimes:3", "drop=every:3:4", "recovery=maybe", "loss=1%", "intra-refresh=1",
		"ref-recovery=1", "ref-recovery,intra-refresh", "ref-recovery,recovery=skip", "recovery=invalidate",
		"still", "still=60", "still=after:0", "still=after:x", "still=every:60", "no-window=1",
		"rate-period=1s", // the 1.5 controller's hook, gone with it (GUIDE 2.2)
		"thin", "thin=every:10", "thin=every:10:for:10", "thin=every:10:for:0", "thin=every:1:for:1", "thin=each:10:for:3",
		"pre-stage-hold=1", "rumble-echo=1", "refuse-tickets=1", "fec-loss", "fec-loss=0", "fec-loss=0.6", "fec-loss=3%"} {
		if _, err := parseTestFaults(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeConn hands out recording frame streams; nothing else is used. The
// streams numbered in stall (from 0, in the order they are opened) stand
// still: their writes block until CancelWrite or release. partial: the peer
// negotiated RESET_STREAM_AT (transport.PartialDelivery); a stalled stream
// then takes the writes before its reliable boundary at once, as quic-go
// takes a small write whatever holds the rest back. afterWrite: run after
// the first write to the stream numbered so returns (on the writing
// goroutine): what another goroutine does before the writer's next call.
type fakeConn struct {
	transport.Conn
	mu         sync.Mutex
	streams    []*fakeStream
	stall      map[int]bool
	partial    bool
	afterWrite map[int]func()
}

func (c *fakeConn) PartialDelivery() bool { return c.partial }

func (c *fakeConn) OpenUniStreamSync(context.Context) (transport.SendStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &fakeStream{partial: c.partial, afterWrite: c.afterWrite[len(c.streams)], streamState: streamState{boundary: -1}}
	if c.stall[len(c.streams)] {
		st.release, st.reset = make(chan struct{}), make(chan struct{})
	}
	c.streams = append(c.streams, st)
	return st, nil
}

// release lets stream i's blocked write complete.
func (c *fakeConn) release(i int) {
	c.mu.Lock()
	st := c.streams[i]
	c.mu.Unlock()
	close(st.release)
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
	partial        bool
	release, reset chan struct{} // a stalled stream (fakeConn.stall)
	resetOnce      sync.Once
	afterWrite     func() // fakeConn.afterWrite
}

type streamState struct {
	data              []byte
	closed, cancelled bool
	doneAt            time.Time // closed or cancelled
	// boundary: the bytes marked reliable by the last SetReliableBoundary
	// (-1: never called), in calls; late: calls after CancelWrite (quic-go
	// then breaks the RESET_STREAM_AT it queued: the host must make none).
	boundary, boundaries, late int
}

// delivered is what the peer gets of a finished stream: all of it, or of a
// cancelled one the part marked reliable under partial delivery.
func (x streamState) delivered(partial bool) []byte {
	switch {
	case x.closed:
		return x.data
	case partial && x.boundary > 0:
		return x.data[:x.boundary]
	}
	return nil
}

func (s *fakeStream) Write(b []byte) (int, error) {
	s.mu.Lock()
	buffered := s.partial && s.boundary < 0
	s.mu.Unlock()
	if s.release != nil && !buffered {
		select {
		case <-s.release:
		case <-s.reset:
			return 0, errors.New("stream reset")
		}
	}
	s.mu.Lock()
	s.data = append(s.data, b...)
	after := s.afterWrite
	s.afterWrite = nil
	s.mu.Unlock()
	if after != nil {
		after()
	}
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
	if s.reset != nil {
		s.resetOnce.Do(func() { close(s.reset) })
	}
}
func (s *fakeStream) SetWriteDeadline(time.Time) error { return nil }
func (s *fakeStream) SetReliableBoundary() {
	s.mu.Lock()
	s.boundary = len(s.data)
	s.boundaries++
	if s.cancelled {
		s.late++
	}
	s.mu.Unlock()
}

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
