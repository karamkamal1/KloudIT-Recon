package host

import (
	"io"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
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
		m.swap(nw)
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

	// A move taken back (undo): the stream before is the only one again.
	a, b := newMovePipe(), newMovePipe()
	m = newMoveStream(a)
	m.swap(b)
	m.undo(b)
	if m.stream() != a || len(m.streams) != 1 {
		t.Fatal("undo did not restore the stream before")
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
