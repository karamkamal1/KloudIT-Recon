//go:build windows

package host

import (
	"context"
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

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
