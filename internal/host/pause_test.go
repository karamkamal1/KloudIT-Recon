package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// While the client is hidden (paused) nothing starts an encoder: its
// watchdog's key-frame requests, a loss it reports, a decoder flush and a
// settings change wait for resume, which starts one generation with the
// settings of that moment; the config of a generation that goes live while
// it is hidden is not sent. Before, each request started a generation
// (FFmpeg: a new process; the helper: relaunched, then an IDR) whose config
// the hidden client got without its frames, so its watchdog asked again
// every second until the tab was shown.
func TestPausedClientStartsNoEncoder(t *testing.T) {
	s, p, ctrl := fakePipelineSession(t, slog.New(slog.NewTextHandler(io.Discard, nil)), media.PipelineCaps{})
	go s.videoEvents()
	control := func(msgs ...proto.ClientMsg) {
		t.Helper()
		var in bytes.Buffer
		for _, m := range msgs {
			b, _ := json.Marshal(m)
			if err := proto.WriteMsg(&in, b); err != nil {
				t.Fatal(err)
			}
		}
		ctrl.r = &in
		if err := s.controlLoop(); !errors.Is(err, io.EOF) {
			t.Fatalf("control loop: %v", err)
		}
	}
	starts := func() int {
		_, _, st := p.state()
		return len(st)
	}
	configs := func() int {
		ctrl.mu.Lock()
		r := bytes.NewReader(append([]byte(nil), ctrl.buf.Bytes()...))
		ctrl.mu.Unlock()
		n := 0
		for {
			b, err := proto.ReadMsg(r, proto.MaxControlMsg)
			if err != nil {
				return n
			}
			var m struct{ T string }
			if json.Unmarshal(b, &m) == nil && m.T == "video" {
				n++
			}
		}
	}
	live := func(gen uint8) {
		t.Helper()
		select {
		case p.events <- media.VideoEvent{Config: &proto.VideoConfig{T: "video", Gen: gen, Codec: "avc1.42e01f", Width: 320, Height: 180, FPS: 30, BitrateKbps: 4000}}:
		case <-time.After(5 * time.Second):
			t.Fatal("videoEvents does not read")
		}
	}

	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	live(1)
	for deadline := time.Now().Add(5 * time.Second); configs() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first generation's config was not sent")
		}
	}
	control(proto.ClientMsg{T: "pause"},
		proto.ClientMsg{T: "keyframe"}, proto.ClientMsg{T: "keyframe"},
		proto.ClientMsg{T: proto.MsgLost, Gen: 1, FromSeq: 40},
		proto.ClientMsg{T: "congestion", Reason: proto.CongestionDecoder},
		proto.ClientMsg{T: "settings", Prefs: &proto.Prefs{BitrateKbps: 8000, FPS: 30}})
	if n := starts(); n != 1 {
		t.Fatalf("%d encoder starts while the client is hidden, want none after the first", n-1)
	}
	live(2) // a generation that went live anyway (it was starting as the pause came)
	live(3) // (the second send returns once videoEvents handled the first)
	if n := configs(); n != 1 {
		t.Fatalf("%d video configs sent, want only the one before the pause", n)
	}

	control(proto.ClientMsg{T: "resume"})
	if n := starts(); n != 2 {
		t.Fatalf("%d encoder starts after resume, want one", n-1)
	}
	if b := s.currentPrefs().BitrateKbps; b != 8000 {
		t.Fatalf("resume starts with %d kbps, want the setting made while hidden (8000)", b)
	}
	live(4)
	live(5)
	if n := configs(); n < 2 {
		t.Fatal("no video config after resume")
	}
	s.kickMu.Lock()
	s.lastKick = time.Time{} // (the decoder flush's cut counts as a key frame for 500 ms)
	s.kickMu.Unlock()
	control(proto.ClientMsg{T: "keyframe"})
	if n := starts(); n != 3 {
		t.Fatalf("a key-frame request after resume started %d generations, want one", n-2)
	}
}

// fakePipelineSession returns a session on a stand-in pipeline with caps
// (ladderPipeline: it records the starts) whose parameters come from the
// test source and libx264, and its control stream.
func fakePipelineSession(t *testing.T, log *slog.Logger, caps media.PipelineCaps) (*Session, *ladderPipeline, *scriptedCtrl) {
	cfg := &Config{Capture: "test", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60, DefaultKbps: 4000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &ladderPipeline{caps: caps, events: make(chan media.VideoEvent)}
	ctrl := &scriptedCtrl{}
	s := &Session{
		a: &Agent{cfg: cfg, caps: &media.Caps{Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264"}}},
			inj: input.NewInjector(nil), hostClock: media.NewHostClock()},
		hello: proto.Hello{V: proto.HelloVersionThinned, Decoders: []proto.DecoderInfo{{Family: "h264"}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 6), pipeSwap: make(chan struct{}, 1),
		log: log, video: p,
	}
	return s, p, ctrl
}

// TestCaptureTimestampsOff: host config captureTimestamps "off" sends no
// capture stamps on any pipeline. The FFmpeg path then makes none; the native
// helper's frames carry their own (capture and present), which the session
// drops before they are sent. Before, "off" did nothing on the helper.
func TestCaptureTimestampsOff(t *testing.T) {
	for _, off := range []bool{false, true} {
		s, p, _ := fakePipelineSession(t, slog.New(slog.NewTextHandler(io.Discard, nil)), media.PipelineCaps{})
		if off {
			s.a.cfg.CaptureTimestamps = "off"
		}
		go s.videoEvents()
		p.events <- media.VideoEvent{Frame: &media.Frame{Gen: 1, Seq: 0, Key: true, PresentUs: 800, CaptureUs: 900, EncodeDoneUs: 1000,
			Data: []byte{1}}}
		var f *media.Frame
		select {
		case f = <-s.frameQ:
		case <-time.After(2 * time.Second):
			t.Fatal("no frame queued")
		}
		h, ext := videoHeader(f, s.hello.V, 1100, 0)
		_, capture := ext.Get(proto.ExtCaptureUs)
		_, present := ext.Get(proto.ExtPresentUs)
		if h.Flags&proto.FrameFlagExt == 0 || capture == off || present == off {
			t.Fatalf("captureTimestamps off %v: capture stamp sent %v, present stamp sent %v", off, capture, present)
		}
	}
}
