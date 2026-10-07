package proto

import (
	"bytes"
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
