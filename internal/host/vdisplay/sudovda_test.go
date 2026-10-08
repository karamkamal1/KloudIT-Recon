package vdisplay

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestSudoVDAIoctlCodes(t *testing.T) {
	// CTL_CODE(FILE_DEVICE_UNKNOWN, 0x800.., METHOD_BUFFERED, FILE_ANY_ACCESS)
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"ADD", ioctlAddVirtualDisplay, 0x222000},
		{"REMOVE", ioctlRemoveVirtualDisplay, 0x222004},
		{"SET_RENDER_ADAPTER", ioctlSetRenderAdapter, 0x222008},
		{"GET_WATCHDOG", ioctlGetWatchdog, 0x22200c},
		{"PING", ioctlDriverPing, 0x222220},
		{"GET_PROTOCOL_VERSION", ioctlGetProtocolVersion, 0x2223fc},
	} {
		if c.got != c.want {
			t.Errorf("IOCTL_%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
	if g := sudovdaInterface; g.Data1 != 0xe5bcc234 || g.Data2 != 0x1e0c || g.Data3 != 0x418a || g.Data4 != [8]byte{0xa0, 0xd4, 0xef, 0x8b, 0x75, 0x01, 0x41, 0x4d} {
		t.Fatalf("interface GUID %+v", g)
	}
}

func TestSudoVDAAddParams(t *testing.T) {
	id := monitorID{Serial: "0123456789ABCDEF"} // longer than the 13-character field
	for i := range id.GUID {
		id.GUID[i] = byte(i + 1)
	}
	b := encodeSudovdaAdd(Mode{Width: 2560, Height: 1440, Hz: 120}, id)
	if len(b) != 56 {
		t.Fatalf("size %d", len(b))
	}
	le := binary.LittleEndian
	if le.Uint32(b[0:]) != 2560 || le.Uint32(b[4:]) != 1440 || le.Uint32(b[8:]) != 120000 {
		t.Fatalf("mode fields % x", b[:12])
	}
	if !bytes.Equal(b[12:28], id.GUID[:]) {
		t.Fatalf("GUID % x", b[12:28])
	}
	if string(b[28:41]) != "KloudIT Recon" || b[41] != 0 {
		t.Fatalf("DeviceName %q", b[28:42])
	}
	if string(b[42:55]) != "0123456789ABC" || b[55] != 0 {
		t.Fatalf("SerialNumber %q", b[42:56])
	}
	short := encodeSudovdaAdd(Mode{Width: 1920, Height: 1080, Hz: 60}, monitorID{Serial: "AB"})
	if string(short[42:44]) != "AB" || !bytes.Equal(short[44:56], make([]byte, 12)) {
		t.Fatalf("short serial % x", short[42:56])
	}
	if r := encodeSudovdaRemove(id); !bytes.Equal(r, id.GUID[:]) {
		t.Fatalf("remove % x", r)
	}
}

func TestSudoVDAReplies(t *testing.T) {
	out := []byte{0xa1, 0xc3, 0, 0, 2, 0, 0, 0, 0x05, 0x01, 0, 0}
	tg, err := decodeSudovdaAddOut(out)
	if err != nil || tg != (Target{LUID{Low: 0xc3a1, High: 2}, 0x105}) {
		t.Fatalf("%v %v", tg, err)
	}
	if _, err := decodeSudovdaAddOut(out[:8]); err == nil {
		t.Fatal("short reply accepted")
	}
	if b := encodeLUID(LUID{Low: 0xc3a1, High: 2}); !bytes.Equal(b, out[:8]) {
		t.Fatalf("LUID % x", b)
	}
	to, cd, err := decodeSudovdaWatchdog([]byte{3, 0, 0, 0, 2, 0, 0, 0})
	if err != nil || to != 3 || cd != 2 {
		t.Fatalf("watchdog %d %d %v", to, cd, err)
	}
	if sudovdaPingEvery(3) != time.Second || sudovdaPingEvery(0) != 0 {
		t.Fatal("ping interval")
	}
	for _, c := range []struct {
		b    []byte
		ok   bool
		text string
	}{
		{[]byte{0, 2, 1, 1}, true, "0.2.1-test"},
		{[]byte{0, 3, 0, 0}, true, "0.3.0"},
		{[]byte{0, 1, 9, 0}, false, "0.1.9"},
		{[]byte{1, 2, 0, 0}, false, "1.2.0"},
	} {
		v, err := decodeSudovdaVersion(c.b)
		if err != nil || v.compatible() != c.ok || v.String() != c.text {
			t.Errorf("version % x: %v compatible=%v %v", c.b, v, v.compatible(), err)
		}
	}
}
