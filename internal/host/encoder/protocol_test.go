package encoder

import (
	"encoding/json"
	"errors"
	"testing"
)

// Exactly what recon-encoder.exe --backend=mock --print-caps prints (under Wine).
const mockCapsJSON = `{"t":"caps","v":1,"helperVersion":"0.1.0","backend":"mock","vendor":"mock","adapterLuid":"","adapterName":"","hagsEnabled":null,"codecs":{"h264":{"maxW":320,"maxH":180,"tenBit":false,"yuv444":false,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"seamless","maxTemporalLayers":1,"roi":"none","sliceOutput":false,"hwInstances":2,"queryTimeout":false,"alignW":1,"alignH":1,"dynamicResolution":false,"hdr10":false,"liveFps":"seamless","instanceSelect":true,"reencode":false}},"capture":["synthetic"],"cursorInVideo":false,"outputs":[],"unavailable":{"amf":"AMF runtime (amfrt64.dll) not found in System32: Module not found (error 126)","nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: Module not found (error 126)","dda":"no DXGI output is attached to the desktop (no display, or a session without one)","amd-direct":"no display output on an AMD adapter","wgc":"this build has no C++/WinRT headers (mingw-w64 build): use the MSVC build for Windows.Graphics.Capture"},"qpcFrequency":10000000}`

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
		h.LiveFPS != "seamless" || !h.InstanceSelect || h.HWInstances != 2 || h.Reencode ||
		len(c.Capture) != 1 || c.Capture[0] != "synthetic" || c.Unavailable["dda"] == "" || c.HAGSEnabled != nil ||
		c.CursorInVideo || c.Outputs == nil || len(c.Outputs) != 0 {
		t.Fatalf("caps %+v", c)
	}

	// Under Wine with an X display (Xvfb): one output, DDA probed usable.
	withOutput := `{"t":"caps","v":1,"backend":"mock","vendor":"mock","hagsEnabled":false,"codecs":{"h264":{}},
		"capture":["synthetic","dda"],"cursorInVideo":false,"outputs":[{"index":0,"adapterIndex":0,"outputIndex":0,
		"adapterLuid":"00000000:000003f0","adapterName":"NVIDIA GeForce GTX 470","vendor":"nvidia","name":"\\\\.\\DISPLAY1",
		"hmonitor":1,"x":0,"y":0,"width":1280,"height":720,"rotation":0,"attached":true}],"qpcFrequency":10000000}`
	m, err = decodeMessage([]byte(withOutput))
	if err != nil {
		t.Fatal(err)
	}
	c = m.(*Caps)
	if len(c.Outputs) != 1 || c.Outputs[0].Name != `\\.\DISPLAY1` || c.Outputs[0].HMonitor != 1 || c.Outputs[0].Width != 1280 ||
		!c.Outputs[0].Attached || c.Outputs[0].AdapterLUID != "00000000:000003f0" || c.HAGSEnabled == nil || *c.HAGSEnabled ||
		len(c.Capture) != 2 || c.Capture[1] != "dda" {
		t.Fatalf("caps with an output %+v", c)
	}

	// The GUIDE Arch-2 example shape decodes too.
	arch2 := `{"t":"caps","v":1,"vendor":"amd","adapterLuid":"0:1234","hagsEnabled":true,"backend":"amf",
		"codecs":{"hevc":{"maxW":7680,"maxH":4320,"tenBit":true,"yuv444":false,"forceIdr":true,"recovery":"ltr","maxLtr":4,
		"intraRefresh":true,"liveBitrate":"seamless","maxTemporalLayers":4,"roi":"importance","sliceOutput":false,
		"hwInstances":2,"queryTimeout":true},"av1":{"alignW":64,"alignH":16,"roi":"importance",
		"assumed":["roi","liveBitrate"]}},"capture":["dda","amd-direct","wgc"]}`
	m, err = decodeMessage([]byte(arch2))
	if err != nil {
		t.Fatal(err)
	}
	c = m.(*Caps)
	if c.Codecs["hevc"].MaxLTR != 4 || c.Codecs["hevc"].Recovery != "ltr" || c.Codecs["av1"].AlignW != 64 ||
		c.HAGSEnabled == nil || !*c.HAGSEnabled || c.Codecs["hevc"].IsAssumed("roi") || !c.Codecs["av1"].IsAssumed("roi") ||
		c.Codecs["av1"].IsAssumed("alignW") {
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
	m, err = decodeMessage([]byte(`{"t":"started","backend":"mock","capture":"dda","codec":"h264","width":320,"height":180,"fps":60,"kbps":4000,"captureWidth":2560,"captureHeight":1440,"adapterLuid":"00000000:0000c3a1","adapterName":"AMD Radeon RX 7900 XT","vendor":"amd","hagsEnabled":true,"gpuPriority":"realtime","idleRepeatMs":100,"barcode":true,"cursorInVideo":false}`))
	if s, ok := m.(*Started); err != nil || !ok || s.CaptureWidth != 2560 || s.Vendor != "amd" || s.HAGSEnabled == nil ||
		!*s.HAGSEnabled || s.GPUPriority != "realtime" || s.IdleRepeatMs != 100 || !s.Barcode || s.CursorInVideo {
		t.Fatalf("started (dda): %+v %v", m, err)
	}
	// The AMF backend at 1920x1080 AV1 on RDNA3: coded 64x16-aligned, cropped.
	m, err = decodeMessage([]byte(`{"t":"started","backend":"amf","capture":"dda","codec":"av1","width":1920,"height":1080,"fps":60,"kbps":20000,"codedWidth":1920,"codedHeight":1088,"cropRight":0,"cropBottom":8,"liveBitrate":"seamless","rateControl":"cbr","usage":"ultra_low_latency","ltrSlots":2,"ltrInterval":6,"encoderInstance":0,"hwInstances":2,"queryTimeoutMs":5,"zeroCopy":false,"intraRefreshFrames":0}`))
	if s, ok := m.(*Started); err != nil || !ok || s.CodedHeight != 1088 || s.CropBottom != 8 || s.CodedWidth != 1920 ||
		s.LiveBitrate != "seamless" || s.RateControl != "cbr" || s.LTRSlots != 2 || s.LTRInterval != 6 || s.HWInstances != 2 ||
		s.QueryTimeoutMs != 5 || s.Usage != "ultra_low_latency" {
		t.Fatalf("started (amf av1): %+v %v", m, err)
	}
	// The NVENC backend (step 3.4).
	m, err = decodeMessage([]byte(`{"t":"started","backend":"nvenc","capture":"dda","codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"codedWidth":2560,"codedHeight":1440,"cropRight":0,"cropBottom":0,"liveBitrate":"seamless","rateControl":"cbr","usage":"ultra_low_latency","ltrSlots":0,"ltrInterval":0,"encoderInstance":0,"hwInstances":2,"queryTimeoutMs":0,"zeroCopy":false,"intraRefreshFrames":0,"preset":"p4","asyncEncode":true,"refFrames":6}`))
	if s, ok := m.(*Started); err != nil || !ok || s.Backend != "nvenc" || s.Preset != "p4" || !s.AsyncEncode || s.RefFrames != 6 ||
		s.LTRSlots != 0 || s.HWInstances != 2 {
		t.Fatalf("started (nvenc): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"caps","v":1,"backend":"nvenc","vendor":"nvidia","codecs":{"av1":{"maxW":8192,"maxH":8192,"recovery":"invalidate","liveBitrate":"seamless","roi":"emphasis","dynamicResolution":true,"assumed":["liveBitrate","roi"]}}}`))
	if c, ok := m.(*Caps); err != nil || !ok || !c.Usable() || c.Codecs["av1"].Recovery != "invalidate" || !c.Codecs["av1"].DynamicResolution ||
		!c.Codecs["av1"].IsAssumed("roi") {
		t.Fatalf("caps (nvenc): %+v %v", m, err)
	}
	// HDR10 (step 3.9): an HEVC Main10 stream with its metadata, and the caps that announce it.
	m, err = decodeMessage([]byte(`{"t":"started","backend":"amf","capture":"dda","codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"hdr":true,"bitDepth":10,"colorSpace":"bt2020-pq","hdrMetadata":{"displayPrimaries":[[0.708,0.292],[0.17,0.797],[0.131,0.046]],"whitePoint":[0.3127,0.329],"maxLuminance":1000,"minLuminance":0.005,"maxCll":1000,"maxFall":400}}`))
	if s, ok := m.(*Started); err != nil || !ok || !s.HDR || s.BitDepth != 10 || s.ColorSpace != "bt2020-pq" || s.HDRMetadata == nil ||
		s.HDRMetadata.DisplayPrimaries[1] != [2]float64{0.17, 0.797} || s.HDRMetadata.WhitePoint[0] != 0.3127 ||
		s.HDRMetadata.MaxLuminance != 1000 || s.HDRMetadata.MinLuminance != 0.005 || s.HDRMetadata.MaxCLL != 1000 || s.HDRMetadata.MaxFALL != 400 {
		t.Fatalf("started (hdr10): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"started","backend":"nvenc","capture":"dda","codec":"hevc","width":1920,"height":1080,"fps":60,"kbps":20000,"hdr":false,"bitDepth":8,"colorSpace":"bt709"}`))
	if s, ok := m.(*Started); err != nil || !ok || s.HDR || s.BitDepth != 8 || s.ColorSpace != "bt709" || s.HDRMetadata != nil {
		t.Fatalf("started (sdr): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"caps","v":1,"backend":"amf","vendor":"amd","codecs":{"hevc":{"maxW":7680,"maxH":4320,"tenBit":true,"hdr10":true},"h264":{"maxW":4096,"maxH":2160,"hdr10":false}},"outputs":[{"index":0,"name":"\\\\.\\DISPLAY1","width":2560,"height":1440,"attached":true,"hdr":true,"bitsPerColor":10,"minLuminance":0.005,"maxLuminance":1015.5,"maxFullFrameLuminance":400}]}`))
	if c, ok := m.(*Caps); err != nil || !ok || !c.Codecs["hevc"].HDR10 || c.Codecs["h264"].HDR10 || len(c.Outputs) != 1 || !c.Outputs[0].HDR ||
		c.Outputs[0].BitsPerColor != 10 || c.Outputs[0].MaxLuminance != 1015.5 || c.Outputs[0].MinLuminance != 0.005 ||
		c.Outputs[0].MaxFullFrameLuminance != 400 {
		t.Fatalf("caps (hdr): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"captureChanged","reason":"hdr","width":2560,"height":1440,"rotation":0,"hdr":false,"text":"Windows HDR turned off for the output; the stream stays HDR10"}`))
	if c, ok := m.(*CaptureChanged); err != nil || !ok || c.Reason != "hdr" || c.HDR || c.Width != 2560 {
		t.Fatalf("captureChanged (hdr): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"started","backend":"mock","capture":"wgc","codec":"h264","width":320,"height":180,"fps":60,"kbps":4000,"cursorInVideo":true}`))
	if s, ok := m.(*Started); err != nil || !ok || !s.CursorInVideo {
		t.Fatalf("started (wgc with the pointer): %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"stats","frameId":8,"gen":0,"dropped":false,"key":false,"recovery":false,"repeat":true,"dirtyPct":0,"bytes":40,"presentQpc":0,"captureQpc":2,"submitQpc":3,"outputQpc":4,"ltrSlot":-1,"temporalLayer":0,"refLtrMask":0,"kbps":4000,"vbvFrames":1,"fps":60,"ringDropped":0}`))
	if s, ok := m.(*Stats); err != nil || !ok || !s.Repeat || s.DirtyPct != 0 || s.PresentQPC != 0 {
		t.Fatalf("repeat stats: %+v %v", m, err)
	}
	if s, ok := m.(*Stats); !ok || s.Dirty != -1 || s.Discardable || s.Reencoded || s.Slices != 0 {
		t.Fatalf("stats of an older helper: dirty %v (want unknown)", s.Dirty)
	}
	// Phase 5: the dirty share, an SVC enhancement frame, a re-encoded frame,
	// sub-frame output; the caps and started fields.
	m, err = decodeMessage([]byte(`{"t":"stats","frameId":9,"gen":0,"dropped":false,"key":false,"recovery":false,"repeat":false,"dirtyPct":1,"dirty":1.9e-05,"discardable":true,"bytes":900,"presentQpc":1,"captureQpc":2,"submitQpc":3,"outputQpc":9,"ltrSlot":-1,"temporalLayer":1,"refLtrMask":0,"kbps":4000,"vbvFrames":1,"fps":60,"ringDropped":0,"reencoded":true,"oversizeBytes":200046,"slices":4,"firstSliceQpc":6}`))
	if s, ok := m.(*Stats); err != nil || !ok || s.Dirty != 1.9e-05 || s.DirtyPct != 1 || !s.Discardable || s.TemporalLayer != 1 || !s.Reencoded ||
		s.OversizeBytes != 200046 || s.Slices != 4 || s.FirstSliceQPC != 6 {
		t.Fatalf("phase 5 stats: %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"started","backend":"nvenc","capture":"dda","codec":"hevc","width":1920,"height":1080,"fps":120,"kbps":30000,"svcLayers":2,"liveFps":"seamless","reencodeOversized":4,"sliceOutput":0}`))
	if s, ok := m.(*Started); err != nil || !ok || s.SVCLayers != 2 || s.LiveFPS != "seamless" || s.ReencodeOversized != 4 || s.SliceOutput != 0 {
		t.Fatalf("phase 5 started: %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"caps","v":1,"backend":"amf","vendor":"amd","codecs":{"av1":{"maxW":8192,"maxH":4352,"maxTemporalLayers":4,"sliceOutput":true,"hwInstances":2,"liveFps":"seamless","instanceSelect":true,"reencode":false,"assumed":["liveFps"]}}}`))
	if c, ok := m.(*Caps); err != nil || !ok || c.Codecs["av1"].LiveFPS != "seamless" || !c.Codecs["av1"].InstanceSelect || c.Codecs["av1"].Reencode ||
		!c.Codecs["av1"].IsAssumed("liveFps") || !c.Codecs["av1"].SliceOutput {
		t.Fatalf("phase 5 caps: %+v %v", m, err)
	}
	m, err = decodeMessage([]byte(`{"t":"captureChanged","reason":"resized","width":1920,"height":1080,"rotation":90,"text":"was 2560x1440 rotation 0"}`))
	if c, ok := m.(*CaptureChanged); err != nil || !ok || c.Reason != "resized" || c.Width != 1920 || c.Rotation != 90 {
		t.Fatalf("captureChanged: %+v %v", m, err)
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
	instance, zeroCopy := 0, false
	for _, c := range []struct {
		v    any
		want string
	}{
		{startMsg{T: "start", StartParams: StartParams{Codec: "h264", FPS: 60, Kbps: 4000}},
			`{"t":"start","monitor":0,"codec":"h264","fps":60,"kbps":4000}`},
		{startMsg{T: "start", StartParams: StartParams{Capture: "dda", Monitor: 1, Codec: "hevc", Width: 2560, Height: 1440,
			FPS: 120, Kbps: 60000, VBVFrames: 1.5, RC: "cbr", Quality: "balanced", HDR: true, LTRSlots: 2, SVCLayers: 2}},
			`{"t":"start","capture":"dda","monitor":1,"codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"vbvFrames":1.5,"rc":"cbr","quality":"balanced","hdr":true,"ltrSlots":2,"svcLayers":2}`},
		{startMsg{T: "start", StartParams: StartParams{Codec: "hevc", FPS: 60, Kbps: 20000, AdapterLUID: "00000000:0000c3a1",
			Monitor: 1, HMonitor: 65537, Window: 0x20a3c, WindowTitle: "Game", GPUPriority: "high", IdleRepeatMs: 250,
			Barcode: &Barcode{X: 0, Y: 8, BlockW: 8, BlockH: 8, Cols: 32, Bits: 32, MSBFirst: true}}},
			`{"t":"start","monitor":1,"codec":"hevc","fps":60,"kbps":20000,"hmonitor":65537,"adapterLuid":"00000000:0000c3a1","window":133692,"windowTitle":"Game","gpuPriority":"high","idleRepeatMs":250,"barcode":{"x":0,"y":8,"blockW":8,"blockH":8,"cols":32,"bits":32,"msbFirst":true}}`},
		{startMsg{T: "start", StartParams: StartParams{Codec: "av1", FPS: 60, Kbps: 20000, LTRSlots: 2, LiveBitrate: "flush",
			EncoderInstance: &instance, LTRInterval: 6, ZeroCopy: &zeroCopy}},
			`{"t":"start","monitor":0,"codec":"av1","fps":60,"kbps":20000,"ltrSlots":2,"liveBitrate":"flush","encoderInstance":0,"ltrInterval":6,"zeroCopy":false}`},
		{startMsg{T: "start", StartParams: StartParams{Codec: "h264", FPS: 60, Kbps: 8000, IntraRefreshFrames: 30}},
			`{"t":"start","monitor":0,"codec":"h264","fps":60,"kbps":8000,"intraRefreshFrames":30}`},
		{ackMsg{T: "ack", FrameID: 1234}, `{"t":"ack","frameId":1234}`},
		{simpleMsg{T: "forceIdr"}, `{"t":"forceIdr"}`},
		{recoverMsg{T: "recover", LostFromFrameID: 42}, `{"t":"recover","lostFromFrameId":42}`},
		{recoverMsg{T: "recover", LostFromFrameID: 42, AckedLTRFrameID: &acked}, `{"t":"recover","lostFromFrameId":42,"ackedLtrFrameId":40}`},
		{setRateMsg{T: "setRate", Kbps: 20000, VBVFrames: 1, FPS: 90}, `{"t":"setRate","kbps":20000,"vbvFrames":1,"fps":90}`},
		{setRateMsg{T: "setRate", FPS: 60}, `{"t":"setRate","fps":60}`}, // SetFPS: kbps unchanged
		{startMsg{T: "start", StartParams: StartParams{Codec: "hevc", FPS: 120, Kbps: 30000, SVCLayers: 2, ReencodeOversized: 4, SliceOutput: 4}},
			`{"t":"start","monitor":0,"codec":"hevc","fps":120,"kbps":30000,"svcLayers":2,"reencodeOversized":4,"sliceOutput":4}`},
		{setROIMsg{T: "setRoi", Rects: []ROIRect{{X: 1, Y: 2, W: 3, H: 4, Weight: 5}}}, `{"t":"setRoi","rects":[{"x":1,"y":2,"w":3,"h":4,"weight":5}]}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil || string(b) != c.want {
			t.Errorf("got %s (%v), want %s", b, err, c.want)
		}
	}
}
