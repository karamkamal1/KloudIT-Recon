package proto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// TestLossRecoveryJS checks that protocol.js reads the host's "dropped"
// message and VideoConfig.Recovery as Go writes them, and which frames end
// the wait for a recovery frame under reference recovery (GUIDE 3.5; needs
// node).
func TestLossRecoveryJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "protocol.js")
	msg := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	in := map[string]string{
		"dropped":  msg(Dropped{T: "dropped", Gen: 200, FromSeq: 4_000_000_000, Count: 3}),
		"noCount":  `{"t":"dropped","gen":1,"fromSeq":5}`,
		"tooMany":  msg(Dropped{T: "dropped", Gen: 1, FromSeq: 5, Count: 1 << 20}),
		"badSeq":   `{"t":"dropped","gen":1,"fromSeq":-1,"count":1}`,
		"noGen":    `{"t":"dropped","fromSeq":1,"count":1}`,
		"skip":     msg(VideoConfig{T: "video", Recovery: RecoverySkip}),
		"keyframe": msg(VideoConfig{T: "video", Recovery: RecoveryKeyframe}),
		"oldHost":  msg(VideoConfig{T: "video"}),
		"ltr":      msg(VideoConfig{T: "video", Recovery: RecoveryLTR}),
		"inval":    msg(VideoConfig{T: "video", Recovery: RecoveryInvalidate}),
		"future":   msg(VideoConfig{T: "video", Recovery: "something-new"}),
		"lost":     msg(ClientMsg{T: MsgLost, Gen: 3, FromSeq: 77}),
	}
	// Frames as the host sends them to a v3 client: a recovery frame (refFloor
	// 9), a key frame, a P-frame, a P-frame without the extension.
	frame := func(key bool, refFloor int) string {
		h := FrameHeader{Type: FrameTypeVideo, Gen: 3, Seq: 12, Flags: FrameFlagExt}
		if key {
			h.Flags |= FrameFlagKey
		}
		var ext FrameExt
		ext.Set(ExtEncodeDoneUs, 1234)
		if refFloor >= 0 {
			ext.Set(ExtRefFloor, uint64(refFloor))
		}
		if refFloor == -2 {
			h.Flags &^= FrameFlagExt
		}
		b := make([]byte, FrameHeaderLen)
		h.Marshal(b)
		if h.Flags&FrameFlagExt != 0 {
			b = ext.Append(b)
		}
		return hex.EncodeToString(append(b, 0, 0, 0, 1))
	}
	frames := map[string]string{"recovery": frame(false, 9), "key": frame(true, -1), "p": frame(false, -1), "plain": frame(false, -2)}
	in["frames"] = msg(frames)
	b, _ := json.Marshal(in)
	script := `
const P = await import(process.argv[1]);
const m = Object.fromEntries(Object.entries(JSON.parse(process.argv[2])).map(([k, v]) => [k, JSON.parse(v)]));
console.log(JSON.stringify({
  consts: [P.MSG_DROPPED, P.RECOVERY_SKIP, P.RECOVERY_KEYFRAME, P.CONGESTION_DECODER],
  dropped: P.parseDropped(m.dropped), noCount: P.parseDropped(m.noCount), tooMany: P.parseDropped(m.tooMany),
  badSeq: P.parseDropped(m.badSeq), noGen: P.parseDropped(m.noGen),
  recovery: [P.recoveryOf(m.skip), P.recoveryOf(m.keyframe), P.recoveryOf(m.oldHost), P.recoveryOf(null),
    P.recoveryOf(m.ltr), P.recoveryOf(m.inval), P.recoveryOf(m.future)],
  ref: [P.isRefRecovery(P.recoveryOf(m.ltr)), P.isRefRecovery(P.recoveryOf(m.inval)), P.isRefRecovery(P.recoveryOf(m.skip)), P.isRefRecovery(P.recoveryOf(m.future))],
  refConsts: [P.RECOVERY_LTR, P.RECOVERY_INVALIDATE, P.MSG_LOST, String(P.HELLO_VERSION), String(m.lost.gen), String(m.lost.fromSeq)],
  // endsRecovery after a loss at seq 10 (and 9): the recovery frame (refFloor 9) ends it only for 10.
  ends: Object.entries(m.frames).map(([k, hex]) => {
    const h = P.parseFrameHeader(Uint8Array.from(hex.match(/../g).map((x) => parseInt(x, 16))));
    return k + ':' + P.endsRecovery(h, 10) + '/' + P.endsRecovery(h, 9);
  }).sort(),
}));`
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), string(b)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	type rng struct{ Gen, From, Count int }
	var got struct {
		Consts                                   []string
		Dropped, NoCount, TooMany, BadSeq, NoGen *rng
		Recovery, RefConsts, Ends                []string
		Ref                                      []bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Join(got.Consts, ",") != "dropped,"+RecoverySkip+","+RecoveryKeyframe+","+CongestionDecoder {
		t.Errorf("constants %v", got.Consts)
	}
	if got.Dropped == nil || *got.Dropped != (rng{200, 4_000_000_000, 3}) {
		t.Errorf("dropped: %+v", got.Dropped)
	}
	if got.NoCount == nil || got.NoCount.Count != 1 || got.TooMany == nil || got.TooMany.Count != 1024 {
		t.Errorf("count: missing -> %+v, huge -> %+v", got.NoCount, got.TooMany)
	}
	if got.BadSeq != nil || got.NoGen != nil {
		t.Errorf("malformed accepted: %+v %+v", got.BadSeq, got.NoGen)
	}
	if strings.Join(got.Recovery, ",") != "skip,keyframe,keyframe,keyframe,ltr,invalidate,keyframe" {
		t.Errorf("recoveryOf: %v (skip, keyframe, old host, none, ltr, invalidate, unknown)", got.Recovery)
	}
	if fmt.Sprint(got.Ref) != "[true true false false]" {
		t.Errorf("isRefRecovery (ltr, invalidate, skip, unknown): %v", got.Ref)
	}
	if want := []string{RecoveryLTR, RecoveryInvalidate, MsgLost, fmt.Sprint(HelloVersionRecovery), "3", "77"}; fmt.Sprint(got.RefConsts) != fmt.Sprint(want) {
		t.Errorf("constants and lost message %v, want %v", got.RefConsts, want)
	}
	if want := "[key:true/true p:false/false plain:false/false recovery:true/false]"; fmt.Sprint(got.Ends) != want {
		t.Errorf("endsRecovery: %v, want %s", got.Ends, want)
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

// TestPrefsAdaptive: adaptive bitrate is on unless the client says otherwise
// (clients before the field always adapted).
func TestPrefsAdaptive(t *testing.T) {
	for _, tc := range []struct {
		json string
		want bool
	}{{`{}`, true}, {`{"adaptive":true}`, true}, {`{"adaptive":false}`, false}} {
		var p Prefs
		if err := json.Unmarshal([]byte(tc.json), &p); err != nil || p.AdaptiveBitrate() != tc.want {
			t.Errorf("%s: adaptive %v (%v), want %v", tc.json, p.AdaptiveBitrate(), err, tc.want)
		}
	}
}

// TestVideoConfigCrop checks the crop fields the host announces for a padded
// coded picture, and that protocol.js shows exactly the announced visible
// part of the frames a decoder outputs (needs node for that part).
func TestVideoConfigCrop(t *testing.T) {
	var c VideoConfig
	c.SetCrop(1920, 1080, 1920, 1080)
	b, _ := json.Marshal(c)
	if c.Width != 1920 || c.Height != 1080 || strings.Contains(string(b), "crop") || strings.Contains(string(b), "coded") {
		t.Fatalf("no padding: %s", b)
	}
	c.SetCrop(1920, 1080, 1920, 1082) // RDNA3 AV1 1080p
	b, _ = json.Marshal(c)
	if !strings.Contains(string(b), `"width":1920,"height":1080`) ||
		!strings.Contains(string(b), `"codedWidth":1920,"codedHeight":1082,"cropBottom":2`) || strings.Contains(string(b), "cropRight") {
		t.Fatalf("1080p: %s", b)
	}
	c.SetCrop(3440, 1440, 3456, 1440)
	if c.CodedWidth != 3456 || c.CodedHeight != 1440 || c.CropRight != 16 || c.CropBottom != 0 {
		t.Fatalf("3440x1440: %+v", c)
	}
	c.SetCrop(1280, 720, 1280, 720) // a later generation without padding clears the fields
	if c.CodedWidth != 0 || c.CropRight != 0 || c.CropBottom != 0 {
		t.Fatalf("cleared: %+v", c)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "protocol.js")
	type frame struct{ W, H, DW, DH float64 }
	cases := []struct {
		name             string
		w, h, codW, codH int
		f                frame
		wantW, wantH     float64
		wantFx, wantFy   float64
	}{
		{"no crop", 1920, 1080, 1920, 1080, frame{1920, 1080, 1920, 1080}, 1920, 1080, 1, 1},
		{"av1 1080p padded", 1920, 1080, 1920, 1082, frame{1920, 1082, 1920, 1082}, 1920, 1080, 1, 1080.0 / 1082},
		{"width padded", 3440, 1440, 3456, 1440, frame{3456, 1440, 3456, 1440}, 3440, 1440, 3440.0 / 3456, 1},
		{"decoder already cropped", 1920, 1080, 1920, 1082, frame{1920, 1080, 1920, 1080}, 1920, 1080, 1, 1},
		{"display size differs (render size)", 1920, 1080, 1920, 1082, frame{1920, 1082, 1920, 1080}, 1920, 1080.0 * 1080 / 1082, 1, 1080.0 / 1082},
		{"test pad", 960, 540, 960, 556, frame{960, 556, 960, 556}, 960, 540, 1, 540.0 / 556},
	}
	var in []any
	for _, c := range cases {
		var v VideoConfig
		v.SetCrop(c.w, c.h, c.codW, c.codH)
		j, _ := json.Marshal(v)
		in = append(in, map[string]any{"cfg": json.RawMessage(j), "f": c.f})
	}
	vec, _ := json.Marshal(in)
	script := `
const P = await import(process.argv[1]);
console.log(JSON.stringify(JSON.parse(process.argv[2]).map(({ cfg, f }) => P.visibleArea(cfg, f.W, f.H, f.DW, f.DH))));`
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), string(vec)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got []struct{ W, H, Fx, Fy float64 }
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("%v: %s", err, out)
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	for i, c := range cases {
		g := got[i]
		if !near(g.W, c.wantW) || !near(g.H, c.wantH) || !near(g.Fx, c.wantFx) || !near(g.Fy, c.wantFy) {
			t.Errorf("%s: visibleArea %+v, want w %v h %v fx %v fy %v", c.name, g, c.wantW, c.wantH, c.wantFx, c.wantFy)
		}
	}
}
