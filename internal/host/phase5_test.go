package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Phase 5 session wiring, part A: temporal SVC thinning (thin.go), the
// frame-rate ladder on an encoder that changes its frame rate in place
// (bitrate.go) and the static-desktop bitrate (activity.go), driven through
// the session with a fake native helper (encoder.Fake).

const (
	// An encoder with temporal SVC and a seamless frame rate that cannot
	// combine intra refresh with SVC (a Phase 5 helper's caps without
	// intraRefreshSvc).
	fakeSVCH264 = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",` +
		`"maxTemporalLayers":2,"intraRefresh":true,"alignW":1,"alignH":1}`
	// The same encoder able to combine them (NVENC: intraRefreshSvc).
	fakeSVCIRH264 = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",` +
		`"maxTemporalLayers":2,"intraRefresh":true,"intraRefreshSvc":true,"alignW":1,"alignH":1}`
	// The same encoder as a helper before Phase 5 reports it (no liveFps).
	fakeOldH264 = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless",` +
		`"maxTemporalLayers":2,"intraRefresh":true,"alignW":1,"alignH":1}`
)

var (
	p5Key    = []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
	p5Frame  = []byte{0, 0, 0, 1, 0x41, 0x9a}
	p5NonRef = []byte{0, 0, 0, 1, 0x01, 0x9a}
)

// p5Rig is a session on a fake helper with frameSender writing to a fake
// connection.
type p5Rig struct {
	s    *Session
	f    *encoder.Fake
	conn *fakeConn
	ctrl *fakeCtrl
	in   *io.PipeWriter
	logs *lockedLog
	// start is the helper's start message.
	start map[string]any
	clock *fakeClock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newP5Rig starts a session (hello helloV, host config cfg on the helper's
// synthetic source) on a fake helper with caps codec, which answers start with
// started (its liveFps, svcLayers as asked).
func newP5Rig(t *testing.T, codec string, helloV int, cfg Config, liveFPS string) *p5Rig {
	t.Helper()
	l := &fakeLauncher{caps: helperCaps(codec, `"dda"`, false), started: make(chan *encoder.Fake, 8)}
	l.handle = func(f *encoder.Fake, m map[string]any) {
		if m["t"] == "start" {
			svc, _ := m["svcLayers"].(float64)
			f.Send(encoder.Started{Backend: "nvenc", Capture: "synthetic-gpu", Codec: "h264", Width: 320, Height: 180, FPS: int(m["fps"].(float64)),
				Kbps: int(m["kbps"].(float64)), LiveBitrate: "seamless", LiveFPS: liveFPS, SVCLayers: max(1, int(svc))})
		}
	}
	cfg.Capture, cfg.Pipeline, cfg.TestWidth, cfg.TestHeight = "test", "helper", 320, 180
	if cfg.DefaultFPS == 0 {
		cfg.DefaultFPS, cfg.MaxFPS = 60, 240
	}
	if cfg.DefaultKbps == 0 {
		cfg.DefaultKbps, cfg.MaxKbps = 20000, 100000
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &lockedLog{}
	inR, inW := io.Pipe()
	ctrl := &scriptedCtrl{r: inR}
	conn := &fakeConn{}
	clock := &fakeClock{t: time.Unix(2_000_000, 0)}
	s := &Session{
		a: &Agent{cfg: &cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
		c: conn, hello: proto.Hello{V: helloV, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
		tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
		ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 6), pipeSwap: make(chan struct{}, 1),
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	s.rate.setFPSFloor(cfg.FPSFloor)
	s.static.on, s.static.kbps, s.static.now = cfg.staticBitrate(), cfg.StaticKbps, clock.now
	if n := s.openPipeline(); n != "" {
		t.Fatalf("notice %q", n)
	}
	t.Cleanup(func() { inW.Close(); s.vid().Stop() })
	go s.videoEvents()
	go s.frameSender()
	go func() { _ = s.controlLoop() }()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	f := <-l.started
	return &p5Rig{s: s, f: f, conn: conn, ctrl: &ctrl.fakeCtrl, in: inW, logs: logs, start: expectFakeMsg(t, f, "start"), clock: clock}
}

// publish sends a helper frame and waits until frameSender handled it (sent
// it or left it out).
func (r *p5Rig) publish(t *testing.T, fr *encoder.Frame) {
	t.Helper()
	before := len(r.conn.snapshot()) + int(r.s.stats.thinned.Load())
	for !r.f.Publish(fr) {
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(r.conn.snapshot())+int(r.s.stats.thinned.Load()) == before {
		if time.Now().After(deadline) {
			t.Fatalf("frame %d not handled", fr.FrameID)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *p5Rig) send(t *testing.T, m proto.ClientMsg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := proto.WriteMsg(r.in, b); err != nil {
		t.Fatal(err)
	}
}

// svcFrame is helper frame id of a two-layer SVC stream that started at
// frame 1: layer (id - 1) % 2, the enhancement layer discardable.
func svcFrame(id uint64) *encoder.Frame {
	fr := &encoder.Frame{FrameID: id, LTRSlot: -1, Dirty: -1, CaptureQPC: int64(2 * id), OutputQPC: int64(2*id + 1), Data: p5Frame}
	switch {
	case id == 1:
		fr.Key, fr.SeqStart, fr.Data = true, true, p5Key
	case (id-1)%2 == 1:
		fr.TemporalLayer, fr.Discardable, fr.Data = 1, true, p5NonRef
	}
	return fr
}

// noMsg fails on a message of type typ the helper gets within 200 ms.
func noMsg(t *testing.T, f *encoder.Fake, typ string) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case m := <-f.Messages():
			if m["t"] == typ {
				t.Fatalf("unexpected %s: %v", typ, m)
			}
		case <-deadline:
			return
		}
	}
}

// TestSessionThinning: a v4 client gets a two-layer SVC stream; under
// congestion (the rate controller over its delay target) the enhancement
// frames are left out before they are sent: no loss report, no Recover, no
// key frame, the frames after them carry the mask; a loss the client reports
// from a thinned seq is recovered from the next frame sent. A v3 client gets
// no SVC (and no thinning).
func TestSessionThinning(t *testing.T) {
	t.Run("v4 client", func(t *testing.T) {
		r := newP5Rig(t, fakeSVCH264, proto.HelloVersionThinned, Config{}, "seamless")
		if r.start["svcLayers"] != float64(2) || r.start["intraRefreshFrames"] != nil {
			t.Fatalf("start %v: want svcLayers 2 and no intra refresh", r.start)
		}
		for id := uint64(1); id <= 6; id++ {
			r.publish(t, svcFrame(id))
		}
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`)
		if c := r.s.vid().Capabilities(); c.SVCLayers != 2 || !c.LiveFPS {
			t.Fatalf("capabilities %+v", c)
		}
		// Congestion: the last report was over the delay target.
		r.s.rate.mu.Lock()
		r.s.rate.overRun = thinOverReports
		r.s.rate.mu.Unlock()
		for id := uint64(7); id <= 14; id++ {
			r.publish(t, svcFrame(id))
		}
		r.s.rate.mu.Lock()
		r.s.rate.overRun = 0
		r.s.rate.mu.Unlock()
		for id := uint64(15); id <= 18; id++ {
			r.publish(t, svcFrame(id))
		}
		seqs, masks := sentFrames(t, r.conn)
		// seq = id - 1: the enhancement frames under pressure (ids 8, 10, 12, 14: seqs 7, 9, 11, 13) left out.
		if fmt.Sprint(seqs) != "[0 1 2 3 4 5 6 8 10 12 14 15 16 17]" {
			t.Fatalf("sent %v", seqs)
		}
		for i, seq := range seqs {
			want := int64(-1)
			for _, th := range []uint32{7, 9, 11, 13} {
				if th < seq {
					want = max(want, 0) | 1<<(seq-1-th)
				}
			}
			if masks[i] != want {
				t.Errorf("seq %d: mask %b, want %b", seq, masks[i], want)
			}
		}
		if n := r.s.stats.thinned.Load(); n != 4 {
			t.Fatalf("thinned %d", n)
		}
		noMsg(t, r.f, "recover")
		if hasMsg(r.ctrl.messages(t), `"t":"dropped"`) {
			t.Fatalf("thinned frames reported dropped: %q", r.ctrl.messages(t))
		}
		if l := r.logs.lines(`msg="thinning: leaving out discardable frames under congestion"`); len(l) != 1 || !strings.Contains(l[0], "why=delay") {
			t.Fatalf("episode log %q", l)
		}
		// The client lost the frame carrying the mask and reports a loss
		// from the thinned seq 13: the loss is of seq 14 (frame 15).
		r.send(t, proto.ClientMsg{T: proto.MsgLost, Gen: 1, FromSeq: 13})
		if m := expectFakeMsg(t, r.f, "recover"); m["lostFromFrameId"] != float64(15) {
			t.Fatalf("recover %v, want from frame 15", m)
		}
		noMsg(t, r.f, "forceIdr")
	})
	t.Run("v3 client", func(t *testing.T) {
		r := newP5Rig(t, fakeSVCH264, proto.HelloVersionRecovery, Config{}, "seamless")
		if r.start["svcLayers"] != nil {
			t.Fatalf("start %v: SVC for a client that cannot be thinned", r.start)
		}
		r.s.rate.mu.Lock()
		r.s.rate.overRun = thinOverReports
		r.s.rate.mu.Unlock()
		for id := uint64(1); id <= 6; id++ {
			r.publish(t, svcFrame(id))
		}
		if seqs, _ := sentFrames(t, r.conn); len(seqs) != 6 || r.s.stats.thinned.Load() != 0 {
			t.Fatalf("v3 client thinned: sent %v", seqs)
		}
	})
	t.Run("intra refresh beside SVC", func(t *testing.T) {
		// GUIDE 2.3 rung 3 stays where the encoder combines the two.
		r := newP5Rig(t, fakeSVCIRH264, proto.HelloVersionThinned, Config{}, "seamless")
		if r.start["svcLayers"] != float64(2) || r.start["intraRefreshFrames"] != float64(30) {
			t.Fatalf("start %v: want svcLayers 2 and 30 frames of intra refresh", r.start)
		}
	})
	t.Run("svc off", func(t *testing.T) {
		r := newP5Rig(t, fakeSVCH264, proto.HelloVersionThinned, Config{SVC: "off"}, "seamless")
		if r.start["svcLayers"] != nil {
			t.Fatalf("start %v: SVC with svc off", r.start)
		}
	})
	t.Run("helper before Phase 5", func(t *testing.T) {
		r := newP5Rig(t, fakeOldH264, proto.HelloVersionThinned, Config{}, "")
		if r.start["svcLayers"] != nil || r.start["intraRefreshFrames"] == nil {
			t.Fatalf("start %v: SVC from a helper before Phase 5", r.start)
		}
		if l := r.logs.lines(`msg="temporal SVC not used"`); len(l) != 1 {
			t.Fatalf("decision log %q", l)
		}
	})
}

// TestSessionLiveFPS: the frame-rate ladder at the bitrate floor goes to a
// Phase 5 helper with a seamless frame rate as a frame-rate change alone
// (setRate fps, SetFPS), finely (LowerFPS steps); to an older helper with the
// bitrate (setRate kbps + fps), on the 2.2 rungs.
func TestSessionLiveFPS(t *testing.T) {
	for _, c := range []struct {
		name, codec, liveFPS string
		fine                 bool
	}{
		{"Phase 5 helper", fakeSVCH264, "seamless", true},
		{"older helper", fakeOldH264, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newP5Rig(t, c.codec, proto.HelloVersionRecovery, Config{FPSFloor: 30}, c.liveFPS)
			r.publish(t, svcFrame(1))
			waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`)
			pol := ratePolicy(r.s.vid().Capabilities())
			if pol.fineFPS != c.fine {
				t.Fatalf("policy %+v: fine steps %v", pol, c.fine)
			}
			r.s.rate.setPolicy(pol)
			r.s.rate.mu.Lock()
			r.s.rate.est = r.s.rate.floor()
			ok := r.s.rate.fpsDown(time.Now())
			next := r.s.rate.fps
			r.s.rate.mu.Unlock()
			// 60 fps, fpsFloor 30: LowerFPS 50; the rungs have nothing below 60.
			want := map[bool]int{true: 50, false: 60}[c.fine]
			if c.fine != ok || c.fine && next != want {
				t.Fatalf("fpsDown at 60 fps: %v -> %d", ok, next)
			}
			kbps, _ := r.s.rate.kbps()
			if err := r.s.setRate(kbps, 45, false, "test"); err != nil {
				t.Fatal(err)
			}
			m := expectFakeMsg(t, r.f, "setRate")
			if m["fps"] != float64(45) || (m["kbps"] == nil) == !c.fine {
				t.Fatalf("setRate %v: want fps 45, kbps only for the older helper", m)
			}
			noMsg(t, r.f, "forceIdr")
		})
	}
}

// TestStaticCap: the static-desktop cap on a fake clock: a cut to the floor
// (a quarter of the target, at least 2000; host config staticKbps) with the
// VBV kept at one full-target frame once the picture has been still for the
// hold (3 s: idle repeats, a caret's new image twice a second, a small
// encoder output); the full target back with the first frame that changes,
// and the next hold twice as long after a short cut; partial activity in
// between; never for a game's presents, whatever their dirty share (0 from
// helpers before the first hardware run's fix, 1 since, a menu's composed
// frame now and then: the owner's log), nor for an encoder making half the
// floor; nothing with an unknown share, a pipeline that is not live, or
// staticBitrate off.
func TestStaticCap(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// A source: the frame at ms (nil: none), every 4 ms.
	type source func(ms int) *media.Frame
	run := func(c *staticCap, target int, live bool, to int, src source) (out []string) {
		c.generation(t0, target, target, live)
		for ms := 0; ms <= to; ms += 4 {
			f := src(ms)
			if f == nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.frame(t0.Add(time.Duration(ms)*time.Millisecond), f, target, live)
			c.mu.Unlock()
			if ok {
				out = append(out, fmt.Sprintf("%d:%d/%.1f", ms, ch.kbps, ch.vbv))
			}
		}
		return out
	}
	caret := 40.0 / (1920 * 1080)
	repeat := &media.Frame{HasDirty: true, Repeat: true, Data: make([]byte, 200)}
	// still: idle repeats every 100 ms, a caret's new image every 500 ms
	// (share, size: what it reports, as the frame's), between from and to.
	still := func(share float64, size int) source {
		return func(ms int) *media.Frame {
			switch {
			case ms%500 == 0:
				return &media.Frame{Dirty: share, HasDirty: share >= 0, Data: make([]byte, size)}
			case ms%100 == 0:
				return repeat
			}
			return nil
		}
	}
	// game: new images at 60 fps (every 16 ms) with this share.
	game := func(share float64) source {
		return func(ms int) *media.Frame {
			if ms%16 != 0 {
				return nil
			}
			return &media.Frame{Dirty: share, HasDirty: true, Data: make([]byte, 40000)}
		}
	}
	join := func(parts ...any) source { // ms boundary, source, ms boundary, source...
		return func(ms int) *media.Frame {
			var cur source
			for i := 0; i+1 < len(parts); i += 2 {
				if ms >= parts[i].(int) {
					cur = parts[i+1].(source)
				}
			}
			return cur(ms)
		}
	}
	once := func(at int, share float64) source { // a single frame that changes share of the picture
		return func(ms int) *media.Frame {
			if ms == at {
				return &media.Frame{Dirty: share, HasDirty: true, Data: make([]byte, 20000)}
			}
			return still(caret, 600)(ms)
		}
	}
	// Still: cut after the hold. A window opens: back at once. Still again:
	// cut twice the hold after the window left the meter (the cut lasted
	// 0.5 s); still for 12 s more, motion: the hold is back to 3 s.
	src := join(0, still(caret, 600), 3500, once(3500, 0.3), 3504, still(caret, 600), 23000, once(23000, 0.3), 23004, still(caret, 600))
	got := run(&staticCap{on: true}, 20000, true, 28000, src)
	want := []string{"3000:5000/4.0", "3500:20000/0.0", "10600:5000/4.0", "23000:20000/0.0", "27100:5000/4.0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("still, motion, still: %v, want %v", got, want)
	}
	// Partial activity (2 % changed twice a second): between the floor and
	// the target, linearly, after the hold.
	if got := run(&staticCap{on: true}, 20000, true, 3500, still(0.02, 600)); fmt.Sprint(got) != "[3000:10625/1.9]" {
		t.Fatalf("partial activity: %v", got)
	}
	// Host config staticKbps; the VBV bound.
	if got := run(&staticCap{on: true, kbps: 1000}, 20000, true, 3500, still(0, 100)); fmt.Sprint(got) != "[3000:1000/20.0]" {
		t.Fatalf("staticKbps 1000: %v", got)
	}
	if got := run(&staticCap{on: true, kbps: 100}, 20000, true, 3500, still(0, 1)); fmt.Sprint(got) != "[3000:100/30.0]" {
		t.Fatalf("staticKbps 100: %v", got)
	}
	// The owner's game menu (docs/VENDOR_NOTES.md "First hardware run: the
	// static desktop during a game"): presents at 60 fps that DDA handed
	// out without dirty rects, a frame the compositor drew (36 % of the
	// picture) every 1.5 s. 52f801f cut it to the floor after a second
	// and restored it with the composed frame, every 1-2 s.
	menu := func(ms int) *media.Frame {
		f := game(0)(ms)
		if f != nil && ms%1500 < 16 {
			f.Dirty = 0.363
		}
		return f
	}
	for name, c := range map[string]struct {
		cap    *staticCap
		target int
		live   bool
		src    source
	}{
		"a game, presents reported unchanged": {&staticCap{on: true}, 30000, true, game(0)},
		"a game, presents counted whole":      {&staticCap{on: true}, 30000, true, game(1)},
		"a game menu with a composed frame":   {&staticCap{on: true}, 30000, true, menu},
		"a game menu at 151 Mbit/s":           {&staticCap{on: true}, 151000, true, menu},
		"a caret-sized change at 60 fps":      {&staticCap{on: true}, 30000, true, game(caret)},
		"an encoder making half the floor": {&staticCap{on: true}, 20000, true, func(ms int) *media.Frame {
			f := still(0, 0)(ms)
			if f != nil {
				f = &media.Frame{HasDirty: true, Repeat: f.Repeat, Dirty: f.Dirty, Data: make([]byte, 32000)}
			}
			return f
		}},
		"unknown share":             {&staticCap{on: true}, 20000, true, still(-1, 100)},
		"not live":                  {&staticCap{on: true}, 20000, false, still(0, 100)},
		"staticBitrate off":         {&staticCap{}, 20000, true, still(0, 100)},
		"target below the floor":    {&staticCap{on: true}, 1500, true, still(0, 100)},
		"staticKbps above a target": {&staticCap{on: true, kbps: 30000}, 20000, true, still(0, 100)},
	} {
		if got := run(c.cap, c.target, c.live, 20000, c.src); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Content that changes every 4 s (still for 4 s, a window for 1 s):
	// cut after 3 s, the next hold 6 s, more than such content is ever still.
	blink := func(ms int) *media.Frame {
		if ms%5000 >= 4000 {
			return game(0.3)(ms)
		}
		return still(caret, 600)(ms)
	}
	if got := run(&staticCap{on: true}, 20000, true, 60000, blink); fmt.Sprint(got) != "[3000:5000/4.0 4000:20000/0.0]" {
		t.Fatalf("content that changes every 5 s: %v, want one cut", got)
	}
	// Content still for 11 s and changing for 1: the hold doubles with each
	// short cut (3, 6, then 12 s: longer than it is ever still).
	slow := func(ms int) *media.Frame {
		if ms%12000 >= 11000 {
			return game(0.3)(ms)
		}
		return still(caret, 600)(ms)
	}
	want = []string{"3000:5000/4.0", "11008:20000/0.0", "19000:5000/4.0", "23008:20000/0.0"}
	if got := run(&staticCap{on: true}, 20000, true, 90000, slow); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("content still for 11 s at a time: %v, want %v", got, want)
	}
	// A generation that goes live below the target (a helper restarted at
	// a capped rate) is capped (the rate controller hears its target): the
	// first frame that changes restores it.
	c := &staticCap{on: true}
	if k := (&staticCap{on: true}).generation(t0, 5000, 20000, false); k != 5000 {
		t.Fatalf("a generation below the target of a pipeline that is not live: the rate controller hears %d", k)
	}
	if k := c.generation(t0, 5000, 20000, true); k != 20000 {
		t.Fatalf("restarted capped: the rate controller hears %d, want its target", k)
	}
	c.mu.Lock()
	ch, ok := c.frame(t0, &media.Frame{Dirty: 0.5, HasDirty: true}, 20000, true)
	c.mu.Unlock()
	if !ok || ch.kbps != 20000 {
		t.Fatalf("restarted capped: %+v %v", ch, ok)
	}
	// The encoder's rate changed in place by someone else (a settings change
	// while capped: the pipeline announces it): followed, and cut again a
	// second later on a still static desktop (the hold had passed).
	c = &staticCap{on: true}
	got = run(c, 20000, true, 3100, still(0, 100))
	if k := c.rate(t0.Add(3100*time.Millisecond), 5000, 20000, true); k != 20000 {
		t.Fatalf("the cut announced: the rate controller hears %d, want its target", k)
	}
	if k := (&staticCap{on: true}).rate(t0, 5000, 20000, false); k != 5000 {
		t.Fatalf("a rate below the target from a pipeline that is not live: the rate controller hears %d", k)
	}
	if k := c.rate(t0.Add(3200*time.Millisecond), 20000, 20000, true); k != 20000 {
		t.Fatalf("the change announced: the rate controller hears %d", k)
	}
	if c.capped || c.sent != 20000 || fmt.Sprint(got) != "[3000:5000/4.0]" {
		t.Fatalf("after an announced change: capped %v sent %d (%v)", c.capped, c.sent, got)
	}
	var again []string
	for ms := 3300; ms <= 10000; ms += 100 {
		c.mu.Lock()
		if ch, ok := c.frame(t0.Add(time.Duration(ms)*time.Millisecond), repeat, 20000, true); ok {
			again = append(again, fmt.Sprintf("%d:%d", ms, ch.kbps))
		}
		c.mu.Unlock()
	}
	if fmt.Sprint(again) != "[4200:5000]" {
		t.Fatalf("cut again: %v", again)
	}
}

// TestStaticCapRateController: the static-desktop cap changes what the
// encoder makes, not what the rate controller measures against: a delay
// spike, a loss burst or lasting thinning on a static desktop (the encoder
// makes 500 kbit/s whatever its target) decreases exactly as without the cap,
// and the target climbs back on a clean path while the desktop stays static.
func TestStaticCapRateController(t *testing.T) {
	type result struct{ after, final int }
	run := func(capped bool, event string) (result, *ctlHarness) {
		h := newCtl(t, 20000, 60, seamless)
		h.outKbps = 500
		if capped {
			h.static = &staticCap{on: true, hold: time.Second}
			h.static.generation(h.clock, 20000, 20000, true)
		}
		h.run(3*time.Second, flat(20*time.Millisecond))
		if capped && (!h.static.isCapped() || h.static.sent != 5000) {
			t.Fatalf("%s: encoder at %d (capped %v), want the static 5000", event, h.static.sent, h.static.isCapped())
		}
		switch event {
		case "delay":
			h.run(300*time.Millisecond, flat(45*time.Millisecond))
		case "loss":
			h.loss = 0.1
			h.run(300*time.Millisecond, flat(20*time.Millisecond))
			h.loss = 0
		case "thinning":
			for i := 0; i < 24; i++ {
				h.r.thinned(true)
				h.run(50*time.Millisecond, flat(20*time.Millisecond))
			}
		}
		r := result{after: h.cur()}
		h.run(10*time.Second, flat(20*time.Millisecond))
		r.final = h.cur()
		return r, h
	}
	for _, event := range []string{"delay", "loss", "thinning"} {
		plain, _ := run(false, event)
		got, h := run(true, event)
		if plain.after >= 20000 || plain.final != 20000 {
			t.Fatalf("%s without the cap: %+v, want a decrease and back to 20000", event, plain)
		}
		if got != plain {
			t.Errorf("%s with the cap: %+v, want %+v as without it", event, got, plain)
		}
		if want := max(rateFloorKbps, got.final/4); !h.static.isCapped() || h.static.sent != want {
			t.Errorf("%s: encoder at %d (capped %v) after the climb, want the static %d", event, h.static.sent, h.static.isCapped(), want)
		}
	}
}

// TestSessionStaticDesktop: a static desktop (the helper's idle repeats and
// a caret's dirty share) lowers the encoder's bitrate and the media
// congestion controller's target once it has been still for the hold, while
// the rate controller keeps its own; the first frame that changes gets the
// full bitrate back before it is sent; a change of the rate controller
// meanwhile stays capped. A game's presents that the helper reports as
// unchanged (the first hardware run) are never a static desktop.
func TestSessionStaticDesktop(t *testing.T) {
	r := newP5Rig(t, fakeSVCH264, proto.HelloVersionRecovery, Config{}, "seamless")
	r.s.static.mu.Lock()
	r.s.static.hold = 500 * time.Millisecond
	r.s.static.mu.Unlock()
	frame := func(id uint64, dirty float64) *encoder.Frame {
		fr := &encoder.Frame{FrameID: id, LTRSlot: -1, Dirty: dirty, CaptureQPC: int64(2 * id), OutputQPC: int64(2*id + 1), Data: p5Frame}
		if id == 1 {
			fr.Key, fr.SeqStart, fr.Data = true, true, p5Key
		}
		return fr
	}
	cc := func() int64 { return r.s.ccTarget.Load().videoKbps }
	id := uint64(1)
	r.publish(t, frame(id, 1))
	waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`)
	// Two seconds of a game whose presents the helper reports as unchanged
	// (a game flipping past the compositor, before the helper's fix), 60 fps:
	// nothing changes.
	for i := 0; i < 120; i++ {
		r.clock.add(16 * time.Millisecond)
		id++
		r.publish(t, frame(id, 0))
	}
	noMsg(t, r.f, "setRate")
	// A still desktop: idle repeats every 100 ms, a blinking caret's new
	// image every 500 ms, for two and a half seconds (the game leaves the
	// meter's second, then the hold).
	for i := 0; i < 25; i++ {
		r.clock.add(100 * time.Millisecond)
		id++
		fr := frame(id, 0)
		fr.Repeat = true
		if i%5 == 0 {
			fr = frame(id, 0.0001)
		}
		r.publish(t, fr)
	}
	m := expectFakeMsg(t, r.f, "setRate")
	if m["kbps"] != float64(5000) || m["vbvFrames"] != float64(4) {
		t.Fatalf("setRate %v: want 5000 kbps with a 4-frame VBV", m)
	}
	if target, _ := r.s.rate.kbps(); target != 20000 || cc() != 5000 || !r.s.static.isCapped() {
		t.Fatalf("rate controller %d kbps, congestion target %d", target, cc())
	}
	if l := r.logs.lines(`msg="static desktop: lowering the bitrate"`); len(l) != 1 {
		t.Fatalf("log %q", l)
	}
	// The helper announces the cut with its next frame: the client hears
	// 5000, the rate controller its own 20000 (its measurements are in
	// units of its target).
	r.clock.add(100 * time.Millisecond)
	id++
	rep := frame(id, 0)
	rep.Repeat = true
	r.publish(t, rep)
	waitMsg(t, r.ctrl, `"t":"rate"`, `"bitrate":5000`)
	r.s.rate.mu.Lock()
	heard := r.s.rate.liveKbps
	r.s.rate.mu.Unlock()
	if heard != 20000 {
		t.Fatalf("the rate controller heard %d kbps of the capped encoder, want its target 20000", heard)
	}
	// The rate controller's change while static: capped too (a quarter).
	r.s.rate.mu.Lock()
	r.s.rate.est, r.s.rate.applied = 16000, 16000 // as decide() sets them
	r.s.rate.mu.Unlock()
	if err := r.s.setRate(16000, 0, false, "test"); err != nil {
		t.Fatal(err)
	}
	// (The VBV stays 4 frames: 16000 / 4000, unchanged, so not sent.)
	if m := expectFakeMsg(t, r.f, "setRate"); m["kbps"] != float64(4000) || m["vbvFrames"] != nil {
		t.Fatalf("setRate %v: want 4000 kbps (a quarter of 16000), the VBV unchanged", m)
	}
	// A window opens: the full bitrate back before the frame is sent.
	r.clock.add(16 * time.Millisecond)
	id++
	r.publish(t, frame(id, 0.4))
	if m := expectFakeMsg(t, r.f, "setRate"); m["kbps"] != float64(16000) || m["vbvFrames"] != float64(1) {
		t.Fatalf("setRate %v: want 16000 kbps with the default VBV", m)
	}
	if cc() != 16000 || r.s.static.isCapped() {
		t.Fatalf("congestion target %d after the restore", cc())
	}
	if l := r.logs.lines(`msg="desktop changes: full bitrate back"`); len(l) != 1 {
		t.Fatalf("log %q", l)
	}
	noMsg(t, r.f, "forceIdr")
}
