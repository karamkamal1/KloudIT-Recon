package host

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// dgConn records the datagrams a session sends and hands it the ones a test
// queues as the client's.
type dgConn struct {
	transport.Conn
	mu   sync.Mutex
	sent [][]byte
	at   []time.Time
	in   chan []byte
}

func (c *dgConn) SendDatagram(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, append([]byte(nil), b...))
	c.at = append(c.at, time.Now())
	return nil
}

func (c *dgConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b := <-c.in:
		return b, nil
	}
}

// of returns the datagrams of type typ sent so far, with their send times.
func (c *dgConn) of(typ byte) ([][]byte, []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]byte
	var at []time.Time
	for i, b := range c.sent {
		if len(b) > 0 && b[0] == typ {
			out = append(out, b)
			at = append(at, c.at[i])
		}
	}
	return out, at
}

func dgSession(t *testing.T, faults testFaults) (*Session, *dgConn, *fakeCtrl) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, ctrl := &dgConn{in: make(chan []byte, 16)}, &fakeCtrl{}
	s := &Session{
		a: &Agent{hostClock: media.NewHostClock(), faults: faults, cfg: &Config{Audio: true}}, c: c, ctrl: ctrl, ctx: ctx, cancel: cancel,
		frameQ: make(chan *media.Frame, 6), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		rttSeen: make(chan struct{}, 1), rumbleGo: make(chan struct{}, 1), pongs: make(chan []byte, 4),
	}
	go s.pongSender() // as Session.run starts it (step 2.7: pongs leave the datagram loop)
	return s, c, ctrl
}

func TestRumbleState(t *testing.T) {
	var r rumbleState
	if out, more := r.due(); len(out) != 0 || more {
		t.Fatalf("idle: %v %v", out, more)
	}
	if !r.set(1, 200, 50) || r.set(1, 200, 50) {
		t.Fatal("set reports a change only when the motors change (ViGEmBus also reports LED changes)")
	}
	if r.set(-1, 1, 1) || r.set(maxRumblePads, 1, 1) {
		t.Fatal("pad index out of range accepted")
	}
	// A running state repeats every tick, for as long as it runs.
	for i := 0; i < 5; i++ {
		out, more := r.due()
		if len(out) != 1 || !bytes.Equal(out[0], proto.Rumble(1, 200, 50)) || !more {
			t.Fatalf("tick %d: %v more=%v", i, out, more)
		}
	}
	// A stop goes out at once (set reports it) and rumbleStops-1 more times.
	if !r.set(1, 0, 0) {
		t.Fatal("stop not reported")
	}
	var stops int
	for i := 0; i < 5; i++ {
		out, more := r.due()
		for _, d := range out {
			if !bytes.Equal(d, proto.Rumble(1, 0, 0)) {
				t.Fatalf("tick %d: %v", i, d)
			}
			stops++
		}
		if more != (stops < rumbleStops-1) {
			t.Fatalf("tick %d: more=%v after %d repeats", i, more, stops)
		}
	}
	if stops != rumbleStops-1 {
		t.Fatalf("stop repeated %d times, want %d", stops, rumbleStops-1)
	}
	// Pads are independent; a stop of one does not end another's repeats.
	r.set(0, 10, 0)
	r.set(3, 0, 20)
	r.set(0, 0, 0)
	out, more := r.due()
	if len(out) != 2 || !bytes.Equal(out[0], proto.Rumble(0, 0, 0)) || !bytes.Equal(out[1], proto.Rumble(3, 0, 20)) || !more {
		t.Fatalf("two pads: %v more=%v", out, more)
	}
	// A stop on a pad that never rumbled is not a change.
	if r.set(2, 0, 0) {
		t.Fatal("stop of an idle pad reported")
	}
}

// TestSessionRumble: a rumble goes out at once, repeats every rumbleRepeat
// while it runs, and its stop goes out rumbleStops times, then nothing.
// With the test hook rumble-echo a client gamepad's triggers come back as
// the motors; a pad that disconnects stops.
func TestSessionRumble(t *testing.T) {
	s, c, _ := dgSession(t, testFaults{rumbleEcho: true})
	go s.rumbleLoop()
	start := time.Now()
	s.gamepad(proto.Gamepad{Index: 2, Connected: true, Seq: 1, LT: 255, RT: 64})
	time.Sleep(350 * time.Millisecond)
	s.gamepad(proto.Gamepad{Index: 2, Connected: true, Seq: 2, LT: 0, RT: 0})
	time.Sleep(450 * time.Millisecond)
	got, at := c.of(proto.DgRumble)
	var running, stops int
	for i, d := range got {
		switch {
		case bytes.Equal(d, proto.Rumble(2, 255, 64)):
			running++
			if i == 0 && at[0].Sub(start) > 50*time.Millisecond {
				t.Errorf("first rumble after %v, want at once", at[0].Sub(start))
			}
		case bytes.Equal(d, proto.Rumble(2, 0, 0)):
			stops++
		default:
			t.Fatalf("unexpected datagram %v", d)
		}
	}
	// 350 ms at a 100 ms repeat: the first datagram and 3 repeats (timer jitter: 2-4).
	if running < 3 || running > 5 || stops != rumbleStops {
		t.Fatalf("%d running, %d stop datagrams; want ~4 and %d", running, stops, rumbleStops)
	}
	if !bytes.Equal(got[len(got)-1], proto.Rumble(2, 0, 0)) {
		t.Fatal("last datagram is not a stop")
	}

	// A pad that disconnects while it rumbles stops (no repeats for a pad
	// that is gone), also without the test hook.
	s2, c2, _ := dgSession(t, testFaults{})
	go s2.rumbleLoop()
	s2.rumble(0, 90, 90)
	s2.gamepad(proto.Gamepad{Index: 0, Connected: false, Seq: 3})
	time.Sleep(400 * time.Millisecond)
	got, _ = c2.of(proto.DgRumble)
	if len(got) != 1+rumbleStops || !bytes.Equal(got[0], proto.Rumble(0, 90, 90)) || !bytes.Equal(got[len(got)-1], proto.Rumble(0, 0, 0)) {
		t.Fatalf("disconnect: %v", got)
	}
}

// TestAgentRumble: the agent passes force feedback to the active session only.
func TestAgentRumble(t *testing.T) {
	s, c, _ := dgSession(t, testFaults{})
	a := s.a
	a.rumble(1, 5, 6) // no active session
	if got, _ := c.of(proto.DgRumble); len(got) != 0 {
		t.Fatalf("sent without an active session: %v", got)
	}
	a.mu.Lock()
	a.active = s
	a.mu.Unlock()
	a.rumble(1, 5, 6)
	if got, _ := c.of(proto.DgRumble); len(got) != 1 || !bytes.Equal(got[0], proto.Rumble(1, 5, 6)) {
		t.Fatalf("active session got %v", got)
	}
}

// TestGamepadOnlyFromActiveSession: the shared virtual pads take a client's
// gamepad datagrams only while its session is the active one and is not being
// closed, as the keyboard and mouse do: not from a session another one took
// over, nor from one being closed (the user's access revoked) during its bye.
// The test hook rumble-echo shows what reached the pads.
func TestGamepadOnlyFromActiveSession(t *testing.T) {
	pad := func(seq uint32) []byte {
		b := make([]byte, 20)
		b[0], b[1], b[2], b[10] = proto.DgGamepad, 1, 1, 200 // pad 1 connected, LT 200
		binary.LittleEndian.PutUint32(b[4:], seq)
		return b
	}
	for _, c := range []struct {
		name            string
		active, closing bool
	}{{"active", true, false}, {"taken over", false, false}, {"closing", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			s, dc, _ := dgSession(t, testFaults{rumbleEcho: true})
			s.a.active = &Session{}
			if c.active {
				s.a.active = s
			}
			s.closing = c.closing
			go s.rumbleLoop()
			go s.datagrams()
			dc.in <- pad(1)
			time.Sleep(100 * time.Millisecond)
			got, _ := dc.of(proto.DgRumble)
			if want := c.active && !c.closing; (len(got) > 0) != want {
				t.Fatalf("%d rumble datagrams: the pad state was applied %v, want %v", len(got), len(got) > 0, want)
			}
		})
	}
}
