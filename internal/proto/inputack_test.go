package proto

import (
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInputAck: the acknowledgement's layout, and the mark in byte 3 of the
// three input datagrams, which their parsers (hosts before it) ignore.
func TestInputAck(t *testing.T) {
	b := InputAck(InputAckGamepad, 2, 0x01020304, 1_000_000_123, 1_000_000_456)
	if len(b) != 24 || b[0] != DgInputAck || b[1] != InputAckGamepad || b[2] != 2 || b[3] != 0 {
		t.Fatalf("% x", b)
	}
	rel := MouseRelDatagram(7, -3, 4)
	abs := make([]byte, 12)
	abs[0] = DgMouseAbs
	pad := make([]byte, 20)
	pad[0] = DgGamepad
	for _, d := range [][]byte{rel, abs, pad} {
		if WantsInputAck(d) {
			t.Errorf("% x: marked without the flag", d)
		}
		d[3] |= InputAckFlag
		if !WantsInputAck(d) {
			t.Errorf("% x: not marked", d)
		}
	}
	if m, ok := ParseMouseRel(rel); !ok || m.Seq != 7 || m.CumX != -3 || m.CumY != 4 {
		t.Errorf("a marked DgMouseRel parses as %+v %v", m, ok)
	}
	if _, ok := ParseMouseAbs(abs); !ok {
		t.Error("a marked DgMouseAbs does not parse")
	}
	if _, ok := ParseGamepad(pad); !ok {
		t.Error("a marked DgGamepad does not parse")
	}
	if WantsInputAck([]byte{DgMouseRel, 0, 0}) {
		t.Error("a short datagram is marked")
	}
}

// TestInputReportValid: what the host logs as given is of the expected form.
func TestInputReportValid(t *testing.T) {
	ok := InputReport{T: MsgInput, Browser: "Chrome 122", Lacks: []string{"canvas2d-attributes", "reset-stream-at"}, Rows: []StageStat{{Name: "mouse", N: 3}}}
	if !ok.Valid() {
		t.Errorf("%+v invalid", ok)
	}
	for name, r := range map[string]InputReport{
		"browser with a newline": {Browser: "Chrome\n122"},
		"browser too long":       {Browser: "Chrome 122 and a very long tail that goes on and on"},
		"lack with spaces":       {Lacks: []string{"two words"}},
		"too many lacks":         {Lacks: make([]string, 17)},
		"too many rows":          {Rows: make([]StageStat, len(InputRowNames)+1)},
	} {
		if r.Valid() {
			t.Errorf("%s: valid", name)
		}
	}
}

// TestInputAckJS: protocol.js reads the acknowledgement Go builds and marks
// its input datagrams where WantsInputAck looks (needs node).
func TestInputAckJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "protocol.js")
	ack := InputAck(InputAckStream, 0, 4000000001, 1<<40+5, 1<<40+77)
	script := `
const P = await import(process.argv[1]);
const hex = (b) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
const ack = Uint8Array.from(process.argv[2].match(/../g).map((h) => parseInt(h, 16)));
console.log(JSON.stringify({
  ack: P.parseInputAck(ack), short: P.parseInputAck(ack.subarray(0, 20)), consts: [P.DG_INPUT_ACK, P.INPUT_ACK_FLAG, P.INPUT_ACK_STREAM, P.INPUT_ACK_MOUSE_REL, P.INPUT_ACK_MOUSE_ABS, P.INPUT_ACK_GAMEPAD, P.FEATURE_INPUT_ACK, P.FEATURE_RESET_STREAM_AT, P.MSG_INPUT_ACK, P.MSG_INPUT],
  marked: [P.markInputAck(P.mouseRel(5, 1, 2)), P.markInputAck(P.mouseAbs(6, 3, 4)), P.markInputAck(P.gamepadState(1, true, 7, { buttons: 0, lt: 0, rt: 0, lx: 0, ly: 0, rx: 0, ry: 0 }))].map(hex),
}));`
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), hex.EncodeToString(ack)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Ack *struct {
			Kind, Idx      byte
			Seq            uint32
			RecvUs, DoneUs uint64
		}
		Short  any
		Consts []any
		Marked []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if a := got.Ack; a == nil || a.Kind != InputAckStream || a.Seq != 4000000001 || a.RecvUs != 1<<40+5 || a.DoneUs != 1<<40+77 {
		t.Errorf("protocol.js parseInputAck: %+v", got.Ack)
	}
	if got.Short != nil {
		t.Errorf("protocol.js parsed a short acknowledgement: %v", got.Short)
	}
	want := []any{float64(DgInputAck), float64(InputAckFlag), float64(InputAckStream), float64(InputAckMouseRel), float64(InputAckMouseAbs), float64(InputAckGamepad),
		FeatureInputAck, FeatureResetStreamAt, MsgInputAck, MsgInput}
	for i := range want {
		if i >= len(got.Consts) || got.Consts[i] != want[i] {
			t.Errorf("protocol.js constant %d = %v, want %v", i, got.Consts, want)
			break
		}
	}
	for i, h := range got.Marked {
		b, _ := hex.DecodeString(h)
		if !WantsInputAck(b) {
			t.Errorf("protocol.js marked datagram %d (% x) is not marked for Go", i, b)
		}
	}
	if m, ok := ParseMouseRel(mustHex(got.Marked[0])); !ok || m.Seq != 5 || m.CumX != 1 || m.CumY != 2 {
		t.Errorf("marked mouseRel parses as %+v", m)
	}
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}
