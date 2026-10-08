package media

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// HelperVideo against the in-process fake helper of internal/host/encoder
// (portable: no Windows, no helper binary).

// fakeAMDCaps: an AMD helper whose H.264 recovers from long-term references
// and changes its bitrate seamlessly.
const fakeAMDCaps = `{"t":"caps","v":1,"helperVersion":"test","backend":"amf","vendor":"amd","adapterLuid":"00000000:0000c3a1",
"adapterName":"AMD Radeon RX 7900 XT","hagsEnabled":true,
"codecs":{"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":1,"alignH":1},
"av1":{"maxW":8192,"maxH":4352,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":64,"alignH":16}},
"capture":["dda","amd-direct"],"cursorInVideo":false,"outputs":[],"qpcFrequency":10000000}`

// An H.264 access unit: SPS (High, level 3.1) + PPS + IDR slice, or one P slice.
var (
	h264Key = []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x10,
		0, 0, 0, 1, 0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0, 0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00}
	h264P = []byte{0, 0, 0, 1, 0x41, 0x9a, 0x24}
)

// fakeHelpers launches fake helpers for a HelperVideo and remembers them.
type fakeHelpers struct {
	t       *testing.T
	caps    string
	handle  encoder.FakeHandler
	mu      sync.Mutex
	fakes   []*encoder.Fake
	started chan *encoder.Fake // a fake answered "start"
	fail    int                // the next launches fail
}

func newFakeHelpers(t *testing.T, caps string, started func(f *encoder.Fake, m map[string]any)) *fakeHelpers {
	fh := &fakeHelpers{t: t, caps: caps, started: make(chan *encoder.Fake, 16)}
	fh.handle = func(f *encoder.Fake, m map[string]any) {
		if m["t"] == "start" {
			started(f, m)
			fh.started <- f
		}
	}
	return fh
}

func (fh *fakeHelpers) launch() (*encoder.Helper, error) {
	fh.mu.Lock()
	if fh.fail > 0 {
		fh.fail--
		fh.mu.Unlock()
		return nil, errors.New("launch refused (test)")
	}
	fh.mu.Unlock()
	h, f, err := encoder.LaunchFake(fh.caps, fh.handle)
	if err != nil {
		return nil, err
	}
	fh.mu.Lock()
	fh.fakes = append(fh.fakes, f)
	fh.mu.Unlock()
	return h, nil
}

func (fh *fakeHelpers) launched() int {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return len(fh.fakes)
}

func (fh *fakeHelpers) nextStarted() *encoder.Fake {
	fh.t.Helper()
	select {
	case f := <-fh.started:
		return f
	case <-time.After(5 * time.Second):
		fh.t.Fatal("no helper started")
	}
	return nil
}

// testClock is a host clock whose QPC readings convert to known host times:
// qpcAt(c, us) is the reading at host time us.
func testClock() *Clock { return &Clock{start: 1_000_000} }

func qpcAt(c *Clock, us int64) int64 { return (c.start + us) * (encoder.FakeQPCFrequency / 1_000_000) }

func nextEvent(t *testing.T, v Pipeline) VideoEvent {
	t.Helper()
	select {
	case ev := <-v.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no video event")
	}
	return VideoEvent{}
}

// expectMsg returns the next message of type typ the fake received (others
// are skipped).
func expectMsg(t *testing.T, f *encoder.Fake, typ string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-f.Messages():
			if m["t"] == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("the helper got no %s", typ)
		}
	}
}

func helperParams() Params {
	return Params{Source: Source{Backend: "ddagrab", Output: 1, HMonitor: 65537, NativeW: 1920, NativeH: 1080},
		Encoder: EncoderInfo{Name: "h264_amf_helper", Family: "h264", Vendor: "amd", HW: true, Helper: true},
		FPS:     60, BitrateKbps: 20000, Quality: "speed", Adaptive: true, GPUPriority: GPUPriorityAuto}
}

func TestHelperEncoders(t *testing.T) {
	h, _, err := encoder.LaunchFake(fakeAMDCaps, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var got []string
	for _, e := range HelperEncoders(h.Caps()) {
		if !e.HW || !e.Helper || e.Vendor != "amd" {
			t.Fatalf("encoder %+v", e)
		}
		got = append(got, e.Name+"/"+e.Family)
	}
	if strings.Join(got, " ") != "av1_amf_helper/av1 h264_amf_helper/h264" {
		t.Fatalf("helper encoders %v", got)
	}
}

// The stream: start parameters, frames with exact host-clock stamps and the
// sequence numbering, the config with the coded size's crop, live bitrate,
// forced key frames as new generations, losses, LTR acks and recovery.
func TestHelperVideoStream(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000,
			CodedWidth: 1920, CodedHeight: 1088, CropBottom: 8, LiveBitrate: "seamless", LTRSlots: 2, Vendor: "amd"})
	})
	first, err := fh.launch()
	if err != nil {
		t.Fatal(err)
	}
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, First: first, Clock: clock})
	defer v.Stop()
	if c := v.Capabilities(); c.Name != PipelineHelper || c.Recovery != RecoveryNone || c.ForceIDR {
		t.Fatalf("capabilities before the start: %+v", c)
	}
	p := helperParams()
	p.Barcode = true
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f := fh.nextStarted()
	if fh.launched() != 1 {
		t.Fatalf("%d helpers launched, want the session's first one only", fh.launched())
	}
	m := expectMsg(t, f, "start")
	for k, want := range map[string]any{"capture": "dda", "monitor": float64(1), "hmonitor": float64(65537), "codec": "h264",
		"fps": float64(60), "kbps": float64(20000), "rc": "cbr", "quality": "speed", "ltrSlots": float64(2), "gpuPriority": "auto"} {
		if m[k] != want {
			t.Errorf("start %s = %v, want %v (%v)", k, m[k], want, m)
		}
	}
	if bc, _ := m["barcode"].(map[string]any); bc == nil || bc["cell"] != float64(proto.BarcodeCell) || bc["x"] != float64(0) {
		t.Errorf("start barcode %v, want cell %d at 0,0", m["barcode"], proto.BarcodeCell)
	}

	// Frame 1 starts the stream; its stamps convert exactly.
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: 0, Data: h264Key,
		PresentQPC: qpcAt(clock, 5_000_000), CaptureQPC: qpcAt(clock, 5_000_100), SubmitQPC: qpcAt(clock, 5_001_000),
		OutputQPC: qpcAt(clock, 5_004_321)})
	ev := nextEvent(t, v)
	c := ev.Config
	if c == nil || c.Gen != 1 || c.Codec != "avc1.64001f" || c.Family != "h264" || c.Width != 1920 || c.Height != 1080 ||
		c.CodedWidth != 1920 || c.CodedHeight != 1088 || c.CropBottom != 8 || c.CropRight != 0 || c.Encoder != "h264_amf_helper" ||
		c.Capture != "dda" || c.Recovery != proto.RecoveryKeyframe || c.BitrateKbps != 20000 || c.FPS != 60 {
		t.Fatalf("config %+v", c)
	}
	fr := nextEvent(t, v).Frame
	if fr == nil || fr.Gen != 1 || fr.Seq != 0 || !fr.Key || fr.PresentUs != 5_000_000 || fr.CaptureUs != 5_000_100 ||
		fr.SubmitUs != 5_001_000 || fr.EncodeDoneUs != 5_004_321 || fr.PtsUs != 0 || !fr.MarkedLTR || fr.LTRSlot != 0 {
		t.Fatalf("frame 1 %+v", fr)
	}
	if c := v.Capabilities(); !c.ForceIDR || !c.LiveBitrate || c.Recovery != RecoveryLTR || c.CursorInVideo {
		t.Fatalf("capabilities %+v", c)
	}
	if p, ok := v.Active(); !ok || p.BitrateKbps != 20000 {
		t.Fatalf("active %+v %v", p, ok)
	}
	// An unknown present time (0) stays unknown; a key frame that is no
	// sequence start (the encoder's own IDR) continues the generation.
	f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: -1, Data: h264P, CaptureQPC: qpcAt(clock, 5_016_767),
		SubmitQPC: qpcAt(clock, 5_017_000), OutputQPC: qpcAt(clock, 5_020_000)})
	f.Publish(&encoder.Frame{FrameID: 3, Key: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 5_033_433),
		OutputQPC: qpcAt(clock, 5_036_000)})
	for i, want := range []struct {
		seq     uint32
		key     bool
		pts     int64
		present uint64
	}{{1, false, 16_667, 0}, {2, true, 33_333, 0}} {
		fr := nextEvent(t, v).Frame
		if fr == nil || fr.Gen != 1 || fr.Seq != want.seq || fr.Key != want.key || fr.PtsUs != want.pts || fr.PresentUs != want.present || fr.MarkedLTR {
			t.Fatalf("frame %d: %+v", i+2, fr)
		}
	}

	// Live bitrate: no new helper.
	if err := v.SetRate(12000, 0); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, f, "setRate"); m["kbps"] != float64(12000) || m["fps"] != nil {
		t.Fatalf("setRate %v", m)
	}
	if p, _ := v.Current(); p.BitrateKbps != 12000 || fh.launched() != 1 {
		t.Fatalf("after SetRate: %d kbps, %d helpers", p.BitrateKbps, fh.launched())
	}

	// The client acknowledged the LTR frame (seq 0) and lost frame seq 2: the
	// recovery names the acknowledged LTR frame.
	v.Ack(1, 1) // not an LTR frame: not passed on
	v.Ack(1, 0)
	if m := expectMsg(t, f, "ack"); m["frameId"] != float64(1) {
		t.Fatalf("ack %v", m)
	}
	if err := v.Recover(1, 2); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, f, "recover"); m["lostFromFrameId"] != float64(3) || m["ackedLtrFrameId"] != float64(1) {
		t.Fatalf("recover %v", m)
	}
	if err := v.Recover(7, 0); err == nil {
		t.Fatal("Recover of a generation that does not stream")
	}

	// A forced key frame: the helper's IDR that starts a sequence begins
	// generation 2 at seq 0, with the same config.
	if err := v.ForceKeyframe(); err != nil {
		t.Fatal(err)
	}
	expectMsg(t, f, "forceIdr")
	f.Publish(&encoder.Frame{FrameID: 4, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 5_050_100)})
	c2 := nextEvent(t, v).Config
	if c2 == nil || c2.Gen != 2 || c2.Codec != c.Codec || c2.CropBottom != 8 || c2.BitrateKbps != 12000 {
		t.Fatalf("config after the forced key frame %+v", c2)
	}
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Gen != 2 || fr.Seq != 0 || !fr.Key || fr.PtsUs != 0 {
		t.Fatalf("forced key frame %+v", fr)
	}
	if fh.launched() != 1 {
		t.Fatalf("the forced key frame launched a helper (%d)", fh.launched())
	}
	if v.Gen() != 2 {
		t.Fatalf("Gen() = %d", v.Gen())
	}

	// Frame 5 lost in the helper (ring full): reported for its seq, before frame 6.
	f.Publish(&encoder.Frame{FrameID: 6, LTRSlot: -1, DroppedBefore: 1, Data: h264P, CaptureQPC: qpcAt(clock, 5_083_433)})
	lost := nextEvent(t, v).Lost
	if lost == nil || lost.Gen != 2 || lost.From != 1 || lost.Count != 1 {
		t.Fatalf("lost %+v", lost)
	}
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Seq != 2 {
		t.Fatalf("frame after the loss %+v", fr)
	}

	// Capture changes reach the session.
	f.Send(encoder.CaptureChanged{Reason: "resized", Width: 2560, Height: 1440})
	if cc := nextEvent(t, v).Capture; cc == nil || cc.Reason != "resized" || cc.Width != 2560 {
		t.Fatalf("capture change %+v", cc)
	}
}

// A new stream (another codec or size) needs a new helper, overlapped: the old
// one streams until the new one's first key frame, then it is shut down.
// Bitrate-only changes stay in the running helper.
func TestHelperVideoRestart(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: m["codec"].(string), Width: 1280, Height: 720, FPS: 60,
			Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless"})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	p := helperParams()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f1 := fh.nextStarted()
	f1.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 1 {
		t.Fatalf("config %+v", c)
	}
	nextEvent(t, v)

	// Same stream, new bitrate: in place.
	p.BitrateKbps = 15000
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, f1, "setRate"); m["kbps"] != float64(15000) {
		t.Fatalf("setRate %v", m)
	}
	// Same stream, urgent: an in-place key frame.
	if err := v.Start(p, true); err != nil {
		t.Fatal(err)
	}
	expectMsg(t, f1, "forceIdr")
	if fh.launched() != 1 {
		t.Fatalf("%d helpers for rate and key frame changes", fh.launched())
	}

	// Another size: a second helper, while the first streams on.
	p.Width, p.Height = 1280, 720
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f2 := fh.nextStarted()
	f1.Publish(&encoder.Frame{FrameID: 2, LTRSlot: -1, Data: h264P, CaptureQPC: qpcAt(clock, 1_016_000)})
	// The live generation's new bitrate is announced before its next frame.
	if r := nextEvent(t, v).Rate; r == nil || r.Gen != 1 || r.Kbps != 15000 || r.FPS != 60 {
		t.Fatalf("rate change %+v", r)
	}
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Gen != 1 || fr.Seq != 1 {
		t.Fatalf("old stream's frame while the new one starts %+v", fr)
	}
	if _, ok := v.Active(); !ok {
		t.Fatal("no active stream during the overlapped start")
	}
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_020_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 || c.Width != 1280 {
		t.Fatalf("new stream's config %+v", c)
	}
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Gen != 2 || fr.Seq != 0 {
		t.Fatalf("new stream's first frame %+v", fr)
	}
	select {
	case <-f1.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the old helper was not shut down")
	}
	// Frames of the old helper are no longer delivered.
	if p, _ := v.Active(); p.Width != 1280 {
		t.Fatalf("active %+v", p)
	}
}

// A helper that fails is replaced at once, the new one starting with a key
// frame as a new generation; three failures within the window give up.
func TestHelperVideoFailures(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60,
			Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless"})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	if err := v.Start(helperParams(), false); err != nil {
		t.Fatal(err)
	}
	f1 := fh.nextStarted()
	f1.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	nextEvent(t, v)
	nextEvent(t, v)
	if err := v.SetRate(9000, 0); err != nil {
		t.Fatal(err)
	}
	expectMsg(t, f1, "setRate")

	// Failure 1: a fatal error, then the exit.
	f1.Send(encoder.HelperError{Code: "device_lost", Text: "TDR", Fatal: true})
	f1.Exit(3)
	ev := nextEvent(t, v)
	var he *encoder.HelperError
	if ev.Err == nil || !ev.Restarted || ev.Fallback || !ev.Live || !errors.As(ev.Err, &he) || he.Code != "device_lost" {
		t.Fatalf("failure event %+v", ev)
	}
	f2 := fh.nextStarted()
	if m := expectMsg(t, f2, "start"); m["kbps"] != float64(9000) {
		t.Fatalf("the replacement started at %v kbps, want the current 9000", m["kbps"])
	}
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 2_000_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 {
		t.Fatalf("replacement config %+v", c)
	}
	nextEvent(t, v)

	// Failure 2: an exit without a word; failure 3: the next launch fails.
	fh.mu.Lock()
	fh.fail = 1
	fh.mu.Unlock()
	f2.Exit(5)
	if ev := nextEvent(t, v); ev.Err == nil || !ev.Restarted {
		t.Fatalf("second failure %+v", ev)
	}
	ev = nextEvent(t, v)
	if ev.Err == nil || !ev.Fallback || !strings.Contains(ev.Err.Error(), "launch refused") {
		t.Fatalf("third failure %+v, want the fallback", ev)
	}
	if err := v.Start(helperParams(), true); !errors.Is(err, ErrHelperGaveUp) {
		t.Fatalf("Start after giving up: %v", err)
	}
}

// A source that changed size ("resized"; the helper scales it into the old
// size meanwhile) gets a new helper at the next Start, also with the same
// parameters: a stream at the native size has width and height 0, so only the
// resize tells it apart. A source back at its starting size needs none.
func TestHelperVideoResize(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000,
			CaptureWidth: 1920, CaptureHeight: 1080, LiveBitrate: "seamless"})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	p := helperParams() // the native size: width and height 0
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f1 := fh.nextStarted()
	if m := expectMsg(t, f1, "start"); m["width"] != nil || m["height"] != nil {
		t.Fatalf("start %v, want the capture size", m)
	}
	f1.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	nextEvent(t, v)
	nextEvent(t, v)
	if err := v.Start(p, false); err != nil || fh.launched() != 1 {
		t.Fatalf("the same stream again: %v, %d helpers", err, fh.launched())
	}
	// Turned by 180°: the same size, no new helper.
	f1.Send(encoder.CaptureChanged{Reason: "resized", Width: 1920, Height: 1080, Rotation: 180})
	if cc := nextEvent(t, v).Capture; cc == nil || cc.Reason != "resized" || cc.Rotation != 180 {
		t.Fatalf("capture change %+v", cc)
	}
	if err := v.Start(p, false); err != nil || fh.launched() != 1 {
		t.Fatalf("after a resize to the same size: %v, %d helpers", err, fh.launched())
	}
	f1.Send(encoder.CaptureChanged{Reason: "resized", Width: 1080, Height: 1920, Rotation: 90})
	if cc := nextEvent(t, v).Capture; cc == nil || cc.Reason != "resized" {
		t.Fatalf("capture change %+v", cc)
	}
	// What the session does next: the same parameters (the monitor's new
	// size is not in the helper's start).
	p.Source.NativeW, p.Source.NativeH = 1080, 1920
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f2 := fh.nextStarted()
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_100_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 {
		t.Fatalf("config of the new helper %+v", c)
	}
	select {
	case <-f1.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the resized helper was not shut down")
	}
}

// A helper still starting the same stream is kept: a key frame request
// (urgent Start) or a bitrate change while the replacement of a failed helper
// starts neither kills it nor launches another; the new bitrate follows its
// start.
func TestHelperVideoKeepsStartingHelper(t *testing.T) {
	clock := testClock()
	var mu sync.Mutex
	hold := false // the next start is answered by the test
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		mu.Lock()
		h := hold
		mu.Unlock()
		if !h {
			f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60,
				Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless"})
		}
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	p := helperParams()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f1 := fh.nextStarted()
	f1.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	nextEvent(t, v)
	nextEvent(t, v)
	mu.Lock()
	hold = true
	mu.Unlock()
	f1.Send(encoder.HelperError{Code: "encode_failed", Text: "test", Fatal: true})
	if ev := nextEvent(t, v); ev.Err == nil || !ev.Restarted {
		t.Fatalf("failure %+v", ev)
	}
	f2 := fh.nextStarted() // asked to start, not answered yet
	if c := v.Capabilities(); c.ForceIDR {
		t.Fatalf("capabilities of a starting helper %+v", c)
	}
	// What the session does for a key frame request now (no ForceIDR): an
	// urgent Start with the same parameters. Then a bitrate change.
	if err := v.Start(p, true); err != nil {
		t.Fatal(err)
	}
	if err := v.SetRate(12000, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f2.Exited():
		t.Fatal("the starting helper was killed")
	case <-time.After(100 * time.Millisecond):
	}
	if n := fh.launched(); n != 2 {
		t.Fatalf("%d helpers launched, want 2", n)
	}
	f2.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000,
		LiveBitrate: "seamless"})
	if m := expectMsg(t, f2, "setRate"); m["kbps"] != float64(12000) {
		t.Fatalf("setRate after the start %v", m)
	}
	f2.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 2_000_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 || c.BitrateKbps != 12000 {
		t.Fatalf("config %+v", c)
	}
}

// A frame rate change in place (a settings change): the helper gets setRate
// with the fps, the live generation's change is announced (RateChange) before
// its next frame, and later configs (a forced key frame's generation) carry
// the new frame rate.
func TestHelperVideoRateInPlace(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000,
			LiveBitrate: "seamless"})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	p := helperParams()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f := fh.nextStarted()
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	if c := nextEvent(t, v).Config; c == nil || c.FPS != 60 {
		t.Fatalf("config %+v", c)
	}
	nextEvent(t, v)
	p.FPS = 120
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, f, "setRate"); m["fps"] != float64(120) || m["kbps"] != float64(20000) || fh.launched() != 1 {
		t.Fatalf("setRate %v (%d helpers)", m, fh.launched())
	}
	f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: -1, Data: h264P, CaptureQPC: qpcAt(clock, 1_016_000)})
	if r := nextEvent(t, v).Rate; r == nil || r.Gen != 1 || r.FPS != 120 || r.Kbps != 20000 {
		t.Fatalf("rate change %+v", r)
	}
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Seq != 1 {
		t.Fatalf("frame %+v", fr)
	}
	f.Publish(&encoder.Frame{FrameID: 3, LTRSlot: -1, Data: h264P, CaptureQPC: qpcAt(clock, 1_024_000)})
	if fr := nextEvent(t, v).Frame; fr == nil || fr.Seq != 2 {
		t.Fatalf("frame after the announcement %+v (announced once)", fr)
	}
	if err := v.ForceKeyframe(); err != nil {
		t.Fatal(err)
	}
	f.Publish(&encoder.Frame{FrameID: 4, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_032_000)})
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 || c.FPS != 120 {
		t.Fatalf("config after the in-place fps change %+v", c)
	}
}

// A live-bitrate qualification (HelperOptions.LiveBitrate, recon-host
// qualify) picks the stream's rate-control and live-bitrate modes: seamless
// and flush go into the start and SetRate stays in the running encoder,
// restart makes every bitrate change a new helper; without a result the
// helper's defaults apply. Switching adaptive bitrate on or off is a new
// stream.
func TestHelperVideoLiveBitrateQualified(t *testing.T) {
	type choice struct {
		rc, mode string
		ok       bool
	}
	for _, c := range []struct {
		choice       choice
		wantRC       string
		wantLive     any // the start's liveBitrate (nil: not sent)
		live, flush  bool
		measured     bool
		helpersAfter int // helpers launched after a bitrate change
	}{
		{choice{"vbr_peak", "seamless", true}, "vbr_peak", "seamless", true, false, true, 1},
		{choice{"cbr", "flush", true}, "cbr", "flush", true, true, true, 1},
		{choice{"cbr", "restart", true}, "cbr", nil, false, false, true, 2},
		{choice{"", "", false}, "cbr", nil, true, false, false, 1},
	} {
		t.Run(c.choice.rc+"-"+c.choice.mode, func(t *testing.T) {
			clock := testClock()
			fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
				live, _ := m["liveBitrate"].(string)
				if live == "" {
					live = "seamless" // the caps' default
				}
				f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60,
					Kbps: int(m["kbps"].(float64)), LiveBitrate: live, RateControl: m["rc"].(string)})
			})
			var asked []string
			var mu sync.Mutex
			v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock,
				LiveBitrate: func(caps encoder.Caps, sp encoder.StartParams, adaptive bool) (string, string, bool) {
					mu.Lock()
					asked = append(asked, fmt.Sprintf("%s %s %s ltr %d %v", caps.AdapterName, sp.Codec, sp.Quality, sp.LTRSlots, adaptive))
					mu.Unlock()
					return c.choice.rc, c.choice.mode, c.choice.ok
				}})
			defer v.Stop()
			p := helperParams()
			if err := v.Start(p, false); err != nil {
				t.Fatal(err)
			}
			f := fh.nextStarted()
			m := expectMsg(t, f, "start")
			if m["rc"] != c.wantRC || m["liveBitrate"] != c.wantLive {
				t.Fatalf("start rc %v liveBitrate %v, want %v %v", m["rc"], m["liveBitrate"], c.wantRC, c.wantLive)
			}
			f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
			nextEvent(t, v) // config
			nextEvent(t, v) // frame
			pc := v.Capabilities()
			if pc.LiveBitrate != c.live || pc.LiveBitrateFlush != c.flush || pc.LiveBitrateMeasured != c.measured {
				t.Fatalf("capabilities %+v", pc)
			}
			if err := v.SetRate(15000, 0); err != nil {
				t.Fatal(err)
			}
			if c.helpersAfter == 1 {
				if m := expectMsg(t, f, "setRate"); m["kbps"] != float64(15000) {
					t.Fatalf("setRate %v", m)
				}
			} else if m := expectMsg(t, fh.nextStarted(), "start"); m["kbps"] != float64(15000) || m["rc"] != c.wantRC {
				t.Fatalf("new helper's start %v", m)
			}
			if n := fh.launched(); n != c.helpersAfter {
				t.Fatalf("%d helpers after the bitrate change, want %d", n, c.helpersAfter)
			}
			mu.Lock()
			defer mu.Unlock()
			// The qualification is asked about the stream as it starts:
			// codec, quality preset, two LTR slots (recovery ltr).
			if asked[0] != "AMD Radeon RX 7900 XT h264 speed ltr 2 true" {
				t.Fatalf("asked %q", asked)
			}
		})
	}
	// Adaptive bitrate on or off is another rate-control mode: a new helper.
	clock := testClock()
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60,
			Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless", RateControl: m["rc"].(string)})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock,
		LiveBitrate: func(_ encoder.Caps, _ encoder.StartParams, adaptive bool) (string, string, bool) {
			if adaptive {
				return "cbr", "seamless", true
			}
			return "vbr", "flush", true
		}})
	defer v.Stop()
	p := helperParams()
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	f := fh.nextStarted()
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	nextEvent(t, v)
	nextEvent(t, v)
	p.Adaptive = false
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, fh.nextStarted(), "start"); m["rc"] != "vbr" || m["liveBitrate"] != "flush" {
		t.Fatalf("fixed-bitrate start %v", m)
	}
}

// Replacements back off: the first at once, the next RestartBackoff later
// per failure since a helper last went live. After a device_lost, helpers
// that fail before going live within ResetGrace do not count toward giving
// up (a driver reset takes seconds); once one goes live the counting is back.
func TestHelperVideoBackoff(t *testing.T) {
	clock := testClock()
	var mu sync.Mutex
	refuse := 0 // starts to refuse
	var starts []time.Time
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		mu.Lock()
		starts = append(starts, time.Now())
		r := refuse > 0
		if r {
			refuse--
		}
		mu.Unlock()
		if r {
			f.Send(encoder.HelperError{Code: "init_failed", Text: "D3D11CreateDevice: DXGI_ERROR_DEVICE_REMOVED", Re: "start"})
			return
		}
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000})
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key})
	})
	const backoff = 150 * time.Millisecond
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock, RestartBackoff: backoff, ResetGrace: 5 * time.Second})
	defer v.Stop()
	if err := v.Start(helperParams(), false); err != nil {
		t.Fatal(err)
	}
	f1 := fh.nextStarted()
	if c := nextEvent(t, v).Config; c == nil {
		t.Fatal("no config")
	}
	nextEvent(t, v)

	// A driver reset: the live helper reports device_lost, three replacements
	// in a row refuse to start (more than GiveUp), the fourth streams.
	mu.Lock()
	refuse = 3
	mu.Unlock()
	lost := time.Now()
	f1.Send(encoder.HelperError{Code: "device_lost", Text: "TDR (test)", Fatal: true})
	for i := 0; i < 4; i++ {
		ev := nextEvent(t, v)
		if ev.Err == nil || !ev.Restarted || ev.Fallback {
			t.Fatalf("failure %d: %+v, want a restart", i+1, ev)
		}
	}
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 2 {
		t.Fatalf("config after the reset %+v", c)
	}
	// Starts: f1, then the replacements: at once, and 1, 2, 3 backoffs after
	// the previous one.
	mu.Lock()
	gaps := []time.Duration{starts[1].Sub(lost)}
	for i := 2; i < len(starts); i++ {
		gaps = append(gaps, starts[i].Sub(starts[i-1]))
	}
	mu.Unlock()
	if len(gaps) != 4 || gaps[0] > backoff/2 {
		t.Fatalf("starts after the failure %v, the first at once", gaps)
	}
	for i, g := range gaps[1:] {
		if g < time.Duration(i+1)*backoff {
			t.Fatalf("restart %d came %v after the previous one, want >= %v (%v)", i+2, g, time.Duration(i+1)*backoff, gaps)
		}
	}

	// Live again: a refused start counts again. With the device_lost, the
	// third failure within the window gives up.
	mu.Lock()
	refuse = 1
	mu.Unlock()
	fh.mu.Lock()
	f5 := fh.fakes[len(fh.fakes)-1]
	fh.mu.Unlock()
	nextEvent(t, v) // its first frame
	f5.Send(encoder.HelperError{Code: "encode_failed", Text: "test", Fatal: true})
	if ev := nextEvent(t, v); ev.Err == nil || !ev.Restarted {
		t.Fatalf("failure after the reset %+v", ev)
	}
	if ev := nextEvent(t, v); ev.Err == nil || !ev.Fallback {
		t.Fatalf("refused start after the reset %+v, want the fallback (device_lost + 2)", ev)
	}
}

// A refused start counts as a failure; the stream starts with the next
// helper.
func TestHelperVideoRefusedStart(t *testing.T) {
	clock := testClock()
	var mu sync.Mutex
	refuse := true
	fh := newFakeHelpers(t, fakeAMDCaps, func(f *encoder.Fake, m map[string]any) {
		mu.Lock()
		r := refuse
		refuse = false
		mu.Unlock()
		if r {
			f.Send(encoder.HelperError{Code: "init_failed", Text: "DuplicateOutput: E_ACCESSDENIED", Re: "start"})
			return
		}
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000})
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key})
	})
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock})
	defer v.Stop()
	if err := v.Start(helperParams(), false); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, v)
	if ev.Err == nil || !ev.Restarted || ev.Live || !strings.Contains(ev.Err.Error(), "init_failed") {
		t.Fatalf("refused start %+v", ev)
	}
	if c := nextEvent(t, v).Config; c == nil || c.Gen != 1 {
		t.Fatalf("config after the retry %+v", c)
	}
	if n := fh.launched(); n != 2 {
		t.Fatalf("%d helpers, want 2", n)
	}
}

// Start parameters by source, and what the helper cannot capture.
func TestHelperStartParams(t *testing.T) {
	h, _, err := encoder.LaunchFake(fakeAMDCaps, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := NewHelperVideo(HelperOptions{First: h, Launch: func() (*encoder.Helper, error) { return nil, errors.New("unused") }})
	defer v.Stop()
	for _, c := range []struct {
		src  Source
		want string
	}{
		{Source{Backend: "ddagrab", Output: 2, HMonitor: 7}, "capture=dda monitor=2 hmonitor=7 window= size=0x0"},
		{Source{Backend: "amf", Output: 0, HMonitor: 7}, "capture=amd-direct monitor=0 hmonitor=7 window= size=0x0"},
		{Source{Backend: "gfxcapture", HMonitor: 7}, "capture=wgc monitor=0 hmonitor=7 window= size=0x0"},
		{Source{Backend: "gfxcapture", HMonitor: 7, Window: "Notepad"}, "capture=wgc monitor=0 hmonitor=0 window=Notepad size=0x0"},
		{Source{Backend: "test", NativeW: 640, NativeH: 360}, "capture=synthetic-gpu monitor=0 hmonitor=0 window= size=640x360"},
	} {
		p := helperParams()
		p.Source = c.src
		p.Adaptive = false
		sp, err := v.startParams(p)
		sp, _ = v.withCaps(sp, p.Adaptive, h.Caps())
		got := fmt.Sprintf("capture=%s monitor=%d hmonitor=%d window=%s size=%dx%d", sp.Capture, sp.Monitor, sp.HMonitor,
			sp.WindowTitle, sp.Width, sp.Height)
		if err != nil || got != c.want || sp.RC != "vbr" || sp.LTRSlots != 2 || sp.ZeroCopy != nil {
			t.Errorf("%+v: %s rc %s ltr %d (%v), want %s", c.src, got, sp.RC, sp.LTRSlots, err, c.want)
		}
	}
	p := helperParams()
	p.Source = Source{Backend: "x11grab"}
	if _, err := v.startParams(p); err == nil {
		t.Error("x11grab start accepted")
	}
	// No LTR slots for a codec that does not recover from long-term references.
	p = helperParams()
	p.Encoder.Family = "hevc"
	if sp, _ := v.startParams(p); first(v.withCaps(sp, false, h.Caps())).LTRSlots != 0 {
		t.Errorf("hevc (not in caps) got LTR slots")
	}
	// Two zero-copy capture failures: the next helper converts.
	v.zeroCopyFails = 2
	if sp, _ := v.startParams(helperParams()); first(v.withCaps(sp, true, h.Caps())).ZeroCopy == nil || *first(v.withCaps(sp, true, h.Caps())).ZeroCopy {
		t.Errorf("zeroCopy not off after two zero-copy capture failures")
	}
}

func first[A, B any](a A, _ B) A { return a }

// The clock conversion is exact: QPC ticks at any frequency map to the host
// clock's microseconds with the same integer arithmetic as the clock itself.
func TestClockFromQPC(t *testing.T) {
	c := &Clock{start: 1_000_000}
	for _, x := range []struct {
		ticks, freq int64
		us          uint64
		ok          bool
	}{
		{20_000_000, 10_000_000, 1_000_000, true},             // 2 s
		{20_000_017, 10_000_000, 1_000_001, true},             // 2.0000017 s: floor
		{3_579_545 * 3, 3_579_545, 2_000_000, true},           // the old ACPI PM timer frequency
		{3_579_545*3 + 1_234_567, 3_579_545, 2_344_894, true}, // 3.344894... s
		{9_999_999, 10_000_000, 0, false},                     // before the clock started
		{0, 10_000_000, 0, false},                             // unknown
		{1 << 62, 10_000_000, uint64((1<<62)/10_000_000*1_000_000+(1<<62)%10_000_000/10) - 1_000_000, true},
	} {
		us, ok := c.FromQPC(x.ticks, x.freq)
		if us != x.us || ok != x.ok {
			t.Errorf("FromQPC(%d, %d) = %d %v, want %d %v", x.ticks, x.freq, us, ok, x.us, x.ok)
		}
	}
}
