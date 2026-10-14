package host

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestInputAcks: a marked input datagram gets its acknowledgement (kind,
// pad, seq, arrival and injection on the host clock), an unmarked one none;
// an input stream message's carries the stream's id and its number.
func TestInputAcks(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	s.pongs = make(chan []byte, 8)
	got := func() []byte {
		select {
		case b := <-s.pongs:
			return b
		default:
			return nil
		}
	}
	pad := make([]byte, 20)
	pad[0] = proto.DgGamepad
	s.ackInputDatagram(pad, proto.InputAckGamepad, 1, 9, 100)
	if b := got(); b != nil {
		t.Fatalf("unmarked datagram acknowledged: % x", b)
	}
	pad[3] = proto.InputAckFlag
	recv := s.a.clock()
	s.ackInputDatagram(pad, proto.InputAckGamepad, 1, 9, recv)
	b := got()
	if len(b) != 24 || b[0] != proto.DgInputAck || b[1] != proto.InputAckGamepad || b[2] != 1 || binary.LittleEndian.Uint32(b[4:]) != 9 {
		t.Fatalf("acknowledgement % x", b)
	}
	if r, d := binary.LittleEndian.Uint64(b[8:]), binary.LittleEndian.Uint64(b[16:]); r != recv || d < r {
		t.Errorf("arrived %d, injected %d", r, d)
	}
	s.ackInputStream(5, 3, 50)
	if b := got(); len(b) != 24 || b[1] != proto.InputAckStream || b[2] != 5 || binary.LittleEndian.Uint32(b[4:]) != 3 || binary.LittleEndian.Uint64(b[8:]) != 50 {
		t.Fatalf("input stream acknowledgement % x", b)
	}
	// A full queue drops the acknowledgement: the datagram loop never waits.
	for range 8 {
		s.pongs <- nil
	}
	s.ackInputStream(5, 4, 60)
}

// inputStream is an input stream's client side as the host reads it.
type inputStream struct {
	io.Reader
	fakeCtrl
}

func (st *inputStream) Read(b []byte) (int, error) { return st.Reader.Read(b) }

func inputStreamOf(msgs ...[]byte) *inputStream {
	var buf bytes.Buffer
	for _, m := range msgs {
		_ = proto.WriteMsg(&buf, m)
	}
	return &inputStream{Reader: &buf}
}

type keyBackend struct {
	input.Backend
	keys int
}

func (b *keyBackend) Key(uint16, bool, bool) error { b.keys++; return nil }
func (b *keyBackend) Button(uint8, bool) error     { return nil }
func (b *keyBackend) Wheel(int16, int16) error     { return nil }

// TestInputLoopAcks: the host numbers an input stream's messages from 1 per
// stream, as the client numbers its writes per transport, and acknowledges
// those after the client's proto.InAckStream with that stream's id. A
// session moved to another connection (a new input stream, the same
// session) starts again from 1 under the new id: an acknowledgement of the
// earlier stream cannot match one of the later stream's inputs.
func TestInputLoopAcks(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	kb := &keyBackend{}
	s.a.inj = input.NewInjector(kb)
	s.a.active = s
	s.pongs = make(chan []byte, 16)
	key := proto.KeyEvent(0x1e, false, true)
	up := proto.KeyEvent(0x1e, false, false)
	type ack struct {
		id  byte
		seq uint32
	}
	acks := func() (out []ack) {
		for {
			select {
			case b := <-s.pongs:
				if len(b) != 24 || b[0] != proto.DgInputAck || b[1] != proto.InputAckStream {
					t.Fatalf("not an input stream acknowledgement: % x", b)
				}
				out = append(out, ack{b[2], binary.LittleEndian.Uint32(b[4:])})
			default:
				return out
			}
		}
	}
	// The first connection: a key before the client asked (not acknowledged), then its id 1.
	s.inputLoop(inputStreamOf(key, []byte{proto.InAckStream, 1}, up, []byte{proto.InMouseButton, 0, 1}, []byte{proto.InWheel, 0, 120, 0, 0, 0}))
	if got, want := acks(), []ack{{1, 3}, {1, 4}, {1, 5}}; !slices.Equal(got, want) {
		t.Errorf("first stream: acknowledged %v, want %v", got, want)
	}
	// The second connection's input stream (id 2): numbered from 1 again.
	s.inputLoop(inputStreamOf([]byte{proto.InAckStream, 2}, key, up))
	if got, want := acks(), []ack{{2, 2}, {2, 3}}; !slices.Equal(got, want) {
		t.Errorf("second stream: acknowledged %v, want %v", got, want)
	}
	// A stream that never asks gets none.
	s.inputLoop(inputStreamOf(key, up))
	if got := acks(); len(got) != 0 {
		t.Errorf("a stream without InAckStream acknowledged: %v", got)
	}
	if kb.keys != 6 {
		t.Errorf("%d keys injected, want 6", kb.keys)
	}
}

// TestLogInput: the client's report is logged with its rows in order, its
// clock sync, browser and the features it lacks; rows of unknown names or
// out of range are left out; one without input is logged with the clock sync
// and the browser; a malformed report, or one with neither rows nor the
// clock sync, logs nothing.
func TestLogInput(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, nil))
	rep := proto.InputReport{
		T: proto.MsgInput, Browser: "Chrome 122", Lacks: []string{"canvas2d-attributes", "reset-stream-at"},
		Rows: []proto.StageStat{
			{Name: "client", P50: 0.21, P95: 0.6, P99: 1.1, N: 40}, {Name: "mouse", P50: 3.14, P95: 5, P99: 6, N: 30},
			{Name: "keys", P50: 4, P95: 7, P99: 7, N: 4}, {Name: "bogus", P50: 1, N: 1}, {Name: "pad", P50: 1e9, N: 3},
		},
		Clock: &proto.ClockSync{MinRTTMs: 2.46, BoundMs: 1.31, AgeS: 3.2, Samples: 40},
	}
	b, _ := json.Marshal(rep)
	s.logInput(b)
	line := buf.String()
	for _, want := range []string{`msg="input latency p50/p95/p99 ms, last 10 s (client: browser event -> injected on the host)"`, `browser="Chrome 122"`,
		`keys="4.0/7.0/7.0 n=4" mouse="3.1/5.0/6.0 n=30" client="0.2/0.6/1.1 n=40"`, "clock_min_rtt_ms=2.5 clock_bound_ms=1.3 clock_age_s=3.2 clock_samples=40",
		"browser_lacks=canvas2d-attributes,reset-stream-at"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s:\n%s", want, line)
		}
	}
	if strings.Contains(line, "bogus") || strings.Contains(line, "pad=") {
		t.Errorf("unknown or out-of-range rows logged:\n%s", line)
	}
	buf.Reset()
	s.logInput([]byte(`{"t":"input","browser":"Chrome 122","lacks":["canvas2d-attributes"],"clock":{"minRttMs":3,"boundMs":1.6,"ageS":1,"samples":9}}`))
	if line := buf.String(); !strings.Contains(line, `browser="Chrome 122" clock_min_rtt_ms=3 clock_bound_ms=1.6`) || !strings.Contains(line, "browser_lacks=canvas2d-attributes") {
		t.Errorf("a report without input:\n%s", line)
	}
	for name, bad := range map[string]string{
		"not JSON":    `{"t":"input",`,
		"bad browser": `{"t":"input","browser":"a\nb","rows":[{"name":"mouse","n":1,"p50":1,"p95":1,"p99":1}]}`,
		"no rows":     `{"t":"input","browser":"Chrome 141"}`,
	} {
		buf.Reset()
		s.logInput([]byte(bad))
		if buf.Len() != 0 {
			t.Errorf("%s: logged %s", name, buf.String())
		}
	}
}
