package encoder

import (
	"encoding/json"
	"errors"
	"testing"
)

// Exactly what recon-encoder.exe --backend=mock --print-caps prints (under Wine).
const mockCapsJSON = `{"t":"caps","v":1,"helperVersion":"0.1.0","backend":"mock","vendor":"mock","adapterLuid":"","adapterName":"","hagsEnabled":false,"codecs":{"h264":{"maxW":320,"maxH":180,"tenBit":false,"yuv444":false,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"seamless","maxTemporalLayers":1,"roi":"none","sliceOutput":false,"hwInstances":1,"queryTimeout":false,"alignW":1,"alignH":1}},"capture":["synthetic"],"unavailable":{"amf":"AMF runtime (amfrt64.dll) not found in System32: Module not found (error 126)","nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: Module not found (error 126)","dda":"desktop duplication capture is not implemented yet (step 3.2)","amd-direct":"AMF runtime (amfrt64.dll) not found in System32: Module not found (error 126)","wgc":"Windows.Graphics.Capture is not implemented yet (step 3.2)"},"qpcFrequency":10000000}`

func TestDecodeCaps(t *testing.T) {
	m, err := decodeMessage([]byte(mockCapsJSON))
	if err != nil {
		t.Fatal(err)
	}
	c, ok := m.(*Caps)
	if !ok {
		t.Fatalf("got %T", m)
	}
	h := c.Codecs["h264"]
	if c.V != 1 || c.Backend != "mock" || c.Vendor != "mock" || !c.Usable() || c.QPCFrequency != 10_000_000 ||
		h.MaxW != 320 || !h.ForceIDR || h.Recovery != "none" || h.LiveBitrate != "seamless" || h.AlignW != 1 ||
		len(c.Capture) != 1 || c.Capture[0] != "synthetic" || c.Unavailable["dda"] == "" {
		t.Fatalf("caps %+v", c)
	}

	// The GUIDE Arch-2 example shape decodes too.
	arch2 := `{"t":"caps","v":1,"vendor":"amd","adapterLuid":"0:1234","hagsEnabled":true,"backend":"amf",
		"codecs":{"hevc":{"maxW":7680,"maxH":4320,"tenBit":true,"yuv444":false,"forceIdr":true,"recovery":"ltr","maxLtr":4,
		"intraRefresh":true,"liveBitrate":"seamless","maxTemporalLayers":4,"roi":"importance","sliceOutput":false,
		"hwInstances":2,"queryTimeout":true},"av1":{"alignW":64,"alignH":16}},"capture":["dda","amd-direct","wgc"]}`
	m, err = decodeMessage([]byte(arch2))
	if err != nil {
		t.Fatal(err)
	}
	c = m.(*Caps)
	if c.Codecs["hevc"].MaxLTR != 4 || c.Codecs["hevc"].Recovery != "ltr" || c.Codecs["av1"].AlignW != 64 || !c.HAGSEnabled {
		t.Fatalf("arch-2 caps %+v", c)
	}
	if (&Caps{Backend: "none"}).Usable() || (&Caps{Backend: "amf"}).Usable() {
		t.Fatal("caps without codecs reported usable")
	}
}

func TestDecodeMessages(t *testing.T) {
	m, err := decodeMessage([]byte(`{"t":"started","backend":"mock","capture":"synthetic","codec":"h264","width":320,"height":180,"fps":60,"kbps":4000}`))
	if s, ok := m.(*Started); err != nil || !ok || s.Width != 320 || s.Capture != "synthetic" {
		t.Fatalf("started: %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"stats","frameId":7,"gen":0,"dropped":true,"key":false,"recovery":false,"bytes":512,"presentQpc":1,"captureQpc":2,"submitQpc":3,"outputQpc":4,"ltrSlot":-1,"temporalLayer":0,"refLtrMask":0,"kbps":4000,"vbvFrames":1.5,"fps":60,"ringDropped":3,"reason":"ringFull"}`))
	if s, ok := m.(*Stats); err != nil || !ok || s.FrameID != 7 || !s.Dropped || s.Reason != "ringFull" || s.LTRSlot != -1 || s.VBVFrames != 1.5 || s.RingDropped != 3 {
		t.Fatalf("stats: %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"error","code":"unsupported","text":"no hevc","fatal":false,"re":"start"}`))
	if e, ok := m.(*HelperError); err != nil || !ok || e.Code != "unsupported" || e.Fatal || e.Re != "start" || e.Error() == "" {
		t.Fatalf("error: %+v %v", m, err)
	}
	if _, err := decodeMessage([]byte(`{"t":"hello"}`)); !errors.Is(err, errUnknownMessage) {
		t.Fatalf("unknown type: %v", err)
	}
	for _, bad := range []string{`not json`, `{"t":"stats","frameId":-1}`, `{"t":"caps","codecs":[]}`, `[]`} {
		if _, err := decodeMessage([]byte(bad)); err == nil || errors.Is(err, errUnknownMessage) {
			t.Errorf("%s: err=%v", bad, err)
		}
	}
}

// The Go -> helper messages use the field names src/protocol.cpp parses.
func TestEncodeMessages(t *testing.T) {
	acked := uint64(40)
	for _, c := range []struct {
		v    any
		want string
	}{
		{startMsg{T: "start", StartParams: StartParams{Codec: "h264", FPS: 60, Kbps: 4000}},
			`{"t":"start","monitor":0,"codec":"h264","fps":60,"kbps":4000}`},
		{startMsg{T: "start", StartParams: StartParams{Capture: "dda", Monitor: 1, Codec: "hevc", Width: 2560, Height: 1440,
			FPS: 120, Kbps: 60000, VBVFrames: 1.5, RC: "cbr", Quality: "balanced", HDR: true, LTRSlots: 2, SVCLayers: 2}},
			`{"t":"start","capture":"dda","monitor":1,"codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"vbvFrames":1.5,"rc":"cbr","quality":"balanced","hdr":true,"ltrSlots":2,"svcLayers":2}`},
		{simpleMsg{T: "forceIdr"}, `{"t":"forceIdr"}`},
		{recoverMsg{T: "recover", LostFromFrameID: 42}, `{"t":"recover","lostFromFrameId":42}`},
		{recoverMsg{T: "recover", LostFromFrameID: 42, AckedLTRFrameID: &acked}, `{"t":"recover","lostFromFrameId":42,"ackedLtrFrameId":40}`},
		{setRateMsg{T: "setRate", Kbps: 20000, VBVFrames: 1, FPS: 90}, `{"t":"setRate","kbps":20000,"vbvFrames":1,"fps":90}`},
		{setROIMsg{T: "setRoi", Rects: []ROIRect{{X: 1, Y: 2, W: 3, H: 4, Weight: 5}}}, `{"t":"setRoi","rects":[{"x":1,"y":2,"w":3,"h":4,"weight":5}]}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil || string(b) != c.want {
			t.Errorf("got %s (%v), want %s", b, err, c.want)
		}
	}
}
