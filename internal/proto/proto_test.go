package proto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFraming(t *testing.T) {
	var b bytes.Buffer
	for _, m := range [][]byte{[]byte("hello"), {}, bytes.Repeat([]byte{7}, 70000)} {
		if err := WriteMsg(&b, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int{5, 0, 70000} {
		got, err := ReadMsg(&b, 1<<20)
		if err != nil || len(got) != want {
			t.Fatalf("got %d %v want %d", len(got), err, want)
		}
	}
	WriteMsg(&b, make([]byte, 100))
	if _, err := ReadMsg(&b, 10); err == nil {
		t.Fatal("oversized message accepted")
	}
}

func TestFrameHeader(t *testing.T) {
	h := FrameHeader{Type: FrameTypeVideo, Flags: FrameFlagKey, Gen: 7, Seq: 123456, PtsUs: 1 << 40, SendUs: 99}
	b := make([]byte, FrameHeaderLen)
	h.Marshal(b)
	var g FrameHeader
	if err := g.Unmarshal(b); err != nil || g != h {
		t.Fatalf("%+v != %+v (%v)", g, h, err)
	}
}

// extFrame builds a frame with an extension block: tag 2 as a 6-byte value,
// an unknown tag 0x42, tag 4 as u64 and tag 6 as u8.
func extFrame() []byte {
	h := FrameHeader{Type: FrameTypeVideo, Flags: FrameFlagKey | FrameFlagExt, Gen: 3, Seq: 77, PtsUs: 16667, SendUs: 5_000_123}
	b := make([]byte, FrameHeaderLen)
	h.Marshal(b)
	entries := []byte{
		ExtCaptureUs, 6, 0x10, 0x20, 0x30, 0x40, 0x50, 0x00,
		0x42, 3, 9, 9, 9,
		ExtEncodeDoneUs, 8, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x00, 0x00,
		ExtLTRSlot, 1, 2,
	}
	b = append(b, byte(len(entries)), 0)
	b = append(b, entries...)
	return append(b, "payload"...)
}

func TestFrameExt(t *testing.T) {
	// Round trip.
	var e FrameExt
	e.Set(ExtCaptureUs, 1_000_000)
	e.Set(ExtEncodeDoneUs, 1_004_321)
	e.Set(ExtRefFloor, 1<<31+5)
	e.Set(ExtLTRSlot, 1)
	e.Set(ExtTemporalLayer, 0)
	h := FrameHeader{Type: FrameTypeVideo, Flags: FrameFlagExt, Gen: 1, Seq: 2, PtsUs: 3, SendUs: 1_005_000}
	b := make([]byte, FrameHeaderLen)
	h.Marshal(b)
	b = e.Append(b)
	b = append(b, 0xAA, 0xBB)
	gh, ge, payload, err := ParseFrame(b)
	if err != nil || gh != h || ge != e || !bytes.Equal(payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("round trip: %+v %+v %x %v", gh, ge, payload, err)
	}
	if v, ok := ge.Get(ExtTemporalLayer); !ok || v != 0 {
		t.Fatal("zero-valued field lost")
	}
	if _, ok := ge.Get(ExtPresentUs); ok {
		t.Fatal("absent field reported present")
	}
	// Without the flag the bytes after the header are payload (v1 framing).
	h.Flags = FrameFlagKey
	h.Marshal(b)
	if _, ge, payload, err := ParseFrame(b); err != nil || !ge.Empty() || len(payload) != len(b)-FrameHeaderLen {
		t.Fatalf("plain frame: %+v %d %v", ge, len(payload), err)
	}

	// Unknown tags are skipped, known tags accept narrower widths.
	_, ge, payload, err = ParseFrame(extFrame())
	if err != nil || string(payload) != "payload" {
		t.Fatalf("%x %v", payload, err)
	}
	if v, _ := ge.Get(ExtCaptureUs); v != 0x5040302010 {
		t.Fatalf("capture %x", v)
	}
	if v, _ := ge.Get(ExtEncodeDoneUs); v != 0x060504030201 {
		t.Fatalf("encodeDone %x", v)
	}
	if v, ok := ge.Get(ExtLTRSlot); !ok || v != 2 {
		t.Fatalf("ltr slot %d %v", v, ok)
	}

	// Malformed blocks are rejected.
	for name, bad := range malformedExt() {
		if _, _, _, err := ParseFrame(bad); err == nil {
			t.Fatalf("%s: malformed extension accepted", name)
		}
	}
}

func malformedExt() map[string][]byte {
	hdr := func(ext ...byte) []byte {
		h := FrameHeader{Type: FrameTypeVideo, Flags: FrameFlagExt}
		b := make([]byte, FrameHeaderLen)
		h.Marshal(b)
		return append(b, ext...)
	}
	return map[string][]byte{
		"no length":          hdr(5),
		"length past end":    hdr(10, 0, ExtLTRSlot, 1, 0),
		"entry past block":   hdr(3, 0, ExtCaptureUs, 8, 1, 2, 3, 4, 5, 6, 7, 8),
		"truncated entry":    hdr(1, 0, ExtLTRSlot),
		"zero-width known":   hdr(2, 0, ExtLTRSlot, 0),
		"oversized known":    hdr(11, 0, ExtCaptureUs, 9, 1, 2, 3, 4, 5, 6, 7, 8, 9),
		"short frame header": make([]byte, FrameHeaderLen-1),
	}
}

// TestFrameExtJS checks that web/static/js/protocol.js parses the same test
// vectors identically (needs node).
func TestFrameExtJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "protocol.js")
	vectors := map[string]string{"good": hex.EncodeToString(extFrame())}
	for name, b := range malformedExt() {
		vectors[name] = hex.EncodeToString(b)
	}
	in, _ := json.Marshal(vectors)
	script := `
const P = await import(process.argv[1]);
const vec = JSON.parse(process.argv[2]);
const out = {};
for (const [name, hex] of Object.entries(vec)) {
  const b = Uint8Array.from(hex.match(/../g).map((x) => parseInt(x, 16)));
  const h = P.parseFrameHeader(b);
  out[name] = h && { ...h, payload: new TextDecoder().decode(b.subarray(h.headerLen)) };
}
console.log(JSON.stringify(out));`
	cmd := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), string(in))
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]*struct {
		Type, Gen, Seq, PtsUs, SendUs int
		Key                           bool
		HeaderLen                     int
		Payload                       string
		Ext                           map[string]float64
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	for name := range malformedExt() {
		if got[name] != nil {
			t.Fatalf("protocol.js accepted malformed %q: %+v", name, got[name])
		}
	}
	g := got["good"]
	want := map[string]float64{"captureUs": 0x5040302010, "encodeDoneUs": 0x060504030201, "ltrSlot": 2}
	if g == nil || g.Type != 1 || !g.Key || g.Gen != 3 || g.Seq != 77 || g.PtsUs != 16667 || g.SendUs != 5_000_123 ||
		g.Payload != "payload" || len(g.Ext) != len(want) {
		t.Fatalf("protocol.js parse: %s", strings.TrimSpace(string(b)))
	}
	for k, v := range want {
		if g.Ext[k] != v {
			t.Fatalf("protocol.js ext %s = %v, want %v", k, g.Ext[k], v)
		}
	}
}

func TestInputParsing(t *testing.T) {
	ev, err := ParseInput(KeyEvent(0x48, true, true))
	if err != nil || ev.Scancode != 0x48 || !ev.Extended || !ev.Down {
		t.Fatalf("%+v %v", ev, err)
	}
	for _, bad := range [][]byte{nil, {InKey, 1}, {InMouseButton, 9, 1}, {InWheel, 0}, {99}} {
		if _, err := ParseInput(bad); err == nil {
			t.Fatalf("accepted malformed %v", bad)
		}
	}
	m, ok := ParseMouseRel(MouseRelDatagram(5, -10, 20))
	if !ok || m.Seq != 5 || m.CumX != -10 || m.CumY != 20 {
		t.Fatalf("%+v", m)
	}
	p := Pong(PingDatagram(9, 123.5), 42)
	if len(p) != 24 || p[0] != DgPong || !bytes.Equal(p[4:16], PingDatagram(9, 123.5)[4:16]) {
		t.Fatal("pong does not echo ping")
	}
}

func TestPairingCode(t *testing.T) {
	pc := PairingCode{Gateway: "10.0.0.2:8443", HostID: "abc", Token: "tok", Pin: "pin", Name: "PC"}
	got, err := ParsePairingCode(pc.Encode())
	if err != nil || got != pc {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParsePairingCode("recon1:e30"); err == nil {
		t.Fatal("incomplete code accepted")
	}
}
