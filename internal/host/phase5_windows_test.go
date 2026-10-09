//go:build windows

package host

import (
	"context"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestSessionHelperMockPhase5: the Phase 5 session wiring (part A) against
// the real recon-encoder.exe (RECON_HELPER_EXE; make helper-test runs it under
// Wine with an X display): a v4 client's session on the mock backend's
// synthetic GPU source starts two temporal layers (the mock's enhancement
// layer: non-reference copies of its canned frames); under the test hook's
// simulated congestion frameSender leaves out exactly the droppable frames,
// the frames after them carry the mask, nothing is reported dropped; the
// source's pauses (1 s of presents, 0.6 s of idle repeats: dirty share 1,
// then 0) lower the bitrate and the first present brings it back at once
// (the activity window shortened to 300 ms for the pause); a frame-rate
// change alone goes to the helper as one, without a new generation.
func TestSessionHelperMockPhase5(t *testing.T) {
	exe := os.Getenv("RECON_HELPER_EXE")
	if exe == "" {
		t.Skip("set RECON_HELPER_EXE to recon-encoder.exe to run the helper integration tests")
	}
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(logs, testWriter{t}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	launch := func(l *slog.Logger) (*encoder.Helper, error) {
		return encoder.Launch(encoder.Options{Exe: exe, Backend: "mock", Log: l})
	}
	cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
		DefaultKbps: 4000, MaxKbps: 100000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, ctrl := &fakeConn{}, &fakeCtrl{}
	s := &Session{
		a: &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: launch,
			faults: testFaults{thinEvery: 40, thinFor: 20}},
		c: conn, hello: proto.Hello{V: proto.HelloVersionThinned, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 6), pipeSwap: make(chan struct{}, 1), log: log,
	}
	s.static.on = true
	s.static.meter.Window = 300 * time.Millisecond
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	defer s.vid().Stop()
	go s.videoEvents()
	go s.frameSender()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(conn.snapshot())+int(s.stats.thinned.Load()) < 150 {
		if time.Now().After(deadline) {
			if l := logs.lines(`init_failed`); len(l) > 0 {
				t.Skipf("no D3D11 device for the synthetic GPU source (Wine needs an X display): %s", l[0])
			}
			t.Fatalf("%d frames after 20 s", len(conn.snapshot()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := s.vid().Capabilities(); c.SVCLayers != 2 || !c.LiveFPS {
		t.Fatalf("capabilities %+v: want two temporal layers and a seamless frame rate", c)
	}
	// Thinning: only droppable frames (the mock's: every second one after a
	// key frame), each announced in the masks of the frames after it.
	seqs, masks := sentFrames(t, conn)
	sent := map[uint32]bool{}
	announced := map[uint32]bool{}
	for i, seq := range seqs {
		sent[seq] = true
		for b := uint32(0); b < 32; b++ {
			if masks[i] > 0 && masks[i]&(1<<b) != 0 {
				announced[seq-1-b] = true
			}
		}
	}
	thinned := s.stats.thinned.Load()
	if thinned < 10 {
		t.Fatalf("%d frames thinned", thinned)
	}
	for seq := range announced {
		if sent[seq] {
			t.Fatalf("seq %d sent and announced as left out", seq)
		}
	}
	for seq := uint32(0); seq < seqs[len(seqs)-1]; seq++ {
		if !sent[seq] && !announced[seq] {
			t.Fatalf("seq %d neither sent nor announced (sent %d, thinned %d)", seq, len(seqs), thinned)
		}
	}
	if hasMsg(ctrl.messages(t), `"t":"dropped"`) {
		t.Fatalf("thinning reported as dropped frames")
	}
	// The static desktop's cut and the restore (the source's pauses).
	for deadline := time.Now().Add(10 * time.Second); len(logs.lines(`msg="desktop changes: full bitrate back"`)) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("no cut and restore: %q", logs.lines(`static desktop`))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l := logs.lines(`msg="static desktop: lowering the bitrate"`); len(l) == 0 || !strings.Contains(l[0], "kbps=2000") {
		t.Fatalf("cut %q: want the floor 2000 kbps", l)
	}
	// A frame-rate change alone: no new generation.
	gen := s.vid().Gen()
	kbps, _ := s.rate.kbps()
	if err := s.setRate(kbps, 20, false, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if p, _ := s.vid().Current(); p.FPS != 20 || s.vid().Gen() != gen {
		t.Fatalf("after the frame-rate change: %d fps, generation %d (was %d)", p.FPS, s.vid().Gen(), gen)
	}
	t.Logf("%d frames sent, %d thinned; static cut and restore logged", len(seqs), thinned)
}

// TestSessionHelperMockPhase5B: the Phase 5 session wiring, part B, against
// the real recon-encoder.exe's mock backend (make helper-test, Wine + X):
// host config encoderInstance "dedicated" starts the mock's second engine
// (its caps: two, selectable), sliceOutput 2 its emulated sub-frame output,
// whose first-slice times reach the host's latency stages through the ring
// (host_encode_first_slice); the pointer input becomes the mock's regions of
// interest (it logs them), around the pointer for an absolute position and
// around the centre under pointer lock, rate-limited.
func TestSessionHelperMockPhase5B(t *testing.T) {
	exe := os.Getenv("RECON_HELPER_EXE")
	if exe == "" {
		t.Skip("set RECON_HELPER_EXE to recon-encoder.exe to run the helper integration tests")
	}
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(logs, testWriter{t}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	launch := func(l *slog.Logger) (*encoder.Helper, error) {
		return encoder.Launch(encoder.Options{Exe: exe, Backend: "mock", Log: l})
	}
	cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
		DefaultKbps: 4000, MaxKbps: 100000, EncoderInstance: "dedicated", SliceOutput: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, ctrl := &fakeConn{}, &fakeCtrl{}
	s := &Session{
		a: &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: launch},
		c: conn, hello: proto.Hello{V: proto.HelloVersionThinned, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 6), pipeSwap: make(chan struct{}, 1), log: log,
		roi: roiFocus{mode: cfg.roi()},
	}
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	defer s.vid().Stop()
	go s.videoEvents()
	go s.frameSender()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(conn.snapshot()) < 30 {
		if time.Now().After(deadline) {
			if l := logs.lines(`init_failed`); len(l) > 0 {
				t.Skipf("no D3D11 device for the synthetic GPU source (Wine needs an X display): %s", l[0])
			}
			t.Fatalf("%d frames after 20 s", len(conn.snapshot()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l := logs.lines(`msg="encoder helper started"`); len(l) != 1 || !strings.Contains(l[0], "encoder_instance=1 hw_instances=2 roi=importance") ||
		!strings.Contains(l[0], "slice_output=2") {
		t.Fatalf("started %q: want engine 1 of 2, ROI and 2 slices", l)
	}
	if l := logs.lines(`msg="encoder engine" codec=h264 config=dedicated engine=1 engines=2`); len(l) != 1 {
		t.Fatalf("engine decision %q", logs.lines("encoder engine"))
	}
	// The client acknowledges every frame sent so far: the host's stage rows
	// now have the first slices (the mock's: when the frame was queued).
	seqs, _ := sentFrames(t, conn)
	gen := s.vid().Gen()
	for _, seq := range seqs {
		s.hostStages.acked(gen, seq, s.a.clock())
	}
	if sum := s.hostStages.summary(s.a.clock()); sum.firstSlice == "" || sum.sliceRest == "" {
		t.Fatalf("host stages %+v: no first slices", sum)
	}
	// Regions of interest: the pointer in the middle of the picture (the
	// mock's 320x180 stream of the synthetic source, whose size the helper
	// logs: the pointer's square is sized and placed in it), then pointer
	// lock.
	var capW, capH int
	started := logs.lines("encoder helper: started: ")
	i := -1
	if len(started) == 1 {
		i = strings.Index(started[0], "capture synthetic-gpu ")
	}
	if i < 0 {
		t.Fatalf("helper start %q: no capture size", started)
	}
	if _, err := fmt.Sscanf(started[0][i:], "capture synthetic-gpu %dx%d", &capW, &capH); err != nil {
		t.Fatalf("helper start %q: %v", started, err)
	}
	now := time.Now()
	s.roi.pointerAbs(32768, 32768, now)
	if !s.roiTick(now) {
		t.Fatal("roiTick ended")
	}
	pointer := encoder.FocusROI(capW, capH, 320, 180, &image.Point{X: 32768 * (capW - 1) / 65535, Y: 32768 * (capH - 1) / 65535},
		encoder.FocusOptions{CenterSize: -1})
	s.roi.pointerAbs(40000, 32768, now.Add(10*time.Millisecond))
	s.roiTick(now.Add(10 * time.Millisecond)) // within the interval: nothing
	s.roi.pointerRel(now.Add(150 * time.Millisecond))
	s.roiTick(now.Add(150 * time.Millisecond))
	center := encoder.FocusROI(capW, capH, 320, 180, nil, encoder.FocusOptions{Background: roiCenterBackground})
	rect := func(r encoder.ROIRect) string {
		return fmt.Sprintf("%d,%d %dx%d weight %d;", r.X, r.Y, r.W, r.H, r.Weight)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		set := logs.lines("mock: setRoi")
		if len(set) >= 2 {
			if len(set) != 2 || !strings.Contains(set[0], "1 rect(s): "+rect(pointer[0])) ||
				!strings.Contains(set[1], "2 rect(s): "+rect(center[0])+" "+rect(center[1])) {
				t.Fatalf("regions %q: want %v then %v", set, pointer, center)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("regions %q: want two setRoi", set)
		}
	}
	t.Logf("%d frames; host stages %+v", len(seqs), s.hostStages.summary(s.a.clock()))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
