package encoder

import (
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// fakeHelper plays recon-encoder.exe over in-memory pipes and a Go ring
// producer, so the client side can be tested on any OS.
type fakeHelper struct {
	t       *testing.T
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	ring    *testWriter
	event   chan struct{}

	exitOnce sync.Once
	exited   chan struct{}
	code     int
	released chan struct{}

	msgs chan map[string]any // messages from Go, after the handler saw them
}

type fakeHandler func(f *fakeHelper, m map[string]any)

func launchFake(t *testing.T, caps string, handle fakeHandler) (*Helper, *fakeHelper, error) {
	t.Helper()
	r, w := newTestRing(t, 4, 64<<10)
	f := &fakeHelper{t: t, ring: w, event: make(chan struct{}, 1), exited: make(chan struct{}),
		released: make(chan struct{}), msgs: make(chan map[string]any, 64)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go f.run(caps, handle)
	h, err := newHelper(Options{CapsTimeout: 2 * time.Second, StartTimeout: 2 * time.Second}.withDefaults(), conn{
		ctrlW: f.stdinW,
		ctrlR: f.stdoutR,
		ring:  r,
		wait: func(d time.Duration) error {
			select {
			case <-f.event:
			case <-time.After(d):
			}
			return nil
		},
		kill:     func() error { f.exit(1); return nil },
		exited:   f.exited,
		exitCode: func() int { return f.code },
		release:  func() { close(f.released) },
	})
	return h, f, err
}

func (f *fakeHelper) send(v any) {
	b, _ := json.Marshal(v)
	// Tag Go structs with their message type, as the helper does.
	t := ""
	switch v.(type) {
	case Started:
		t = "started"
	case HelperError:
		t = "error"
	case Stats:
		t = "stats"
	}
	if t != "" {
		var m map[string]any
		json.Unmarshal(b, &m)
		m["t"] = t
		b, _ = json.Marshal(m)
	}
	_ = proto.WriteMsg(f.stdoutW, b)
}

func (f *fakeHelper) exit(code int) {
	f.exitOnce.Do(func() {
		f.code = code
		f.stdoutW.Close()
		f.stdinR.Close()
		close(f.exited)
	})
}

func (f *fakeHelper) publish(fr *Frame) bool { return f.publishMangled(fr, nil) }

func (f *fakeHelper) publishMangled(fr *Frame, mangle func([]byte)) bool {
	ok := f.ring.writeMangled(fr, mangle)
	select {
	case f.event <- struct{}{}:
	default:
	}
	return ok
}

func (f *fakeHelper) run(caps string, handle fakeHandler) {
	f.send(json.RawMessage(caps))
	for {
		b, err := proto.ReadMsg(f.stdinR, MaxControlMsg)
		if err != nil {
			f.exit(0) // stdin closed: exit like the real helper
			return
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			f.t.Errorf("fake helper: bad message %q", b)
			continue
		}
		if m["t"] == "shutdown" {
			f.exit(0)
			return
		}
		if handle != nil {
			handle(f, m)
		}
		f.msgs <- m
	}
}

func nextFrame(t *testing.T, h *Helper) *Frame {
	t.Helper()
	select {
	case fr, ok := <-h.Frames():
		if !ok {
			t.Fatal("frames channel closed")
		}
		return fr
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

func nextError(t *testing.T, h *Helper) *HelperError {
	t.Helper()
	select {
	case err := <-h.Errors():
		var he *HelperError
		if !errors.As(err, &he) {
			t.Fatalf("error %v is not a HelperError", err)
		}
		return he
	case <-time.After(3 * time.Second):
		t.Fatal("no error")
	}
	return nil
}

// replyStarted answers start like the mock backend and publishes n frames.
func replyStarted(n int) fakeHandler {
	return func(f *fakeHelper, m map[string]any) {
		if m["t"] != "start" {
			return
		}
		f.send(Started{Backend: "mock", Capture: "synthetic", Codec: "h264", Width: 320, Height: 180, FPS: 60, Kbps: 4000})
		for i := 1; i <= n; i++ {
			f.publish(&Frame{FrameID: uint64(i), Key: i == 1, LTRSlot: -1, Data: []byte{0, 0, 0, 1, byte(i)}})
		}
	}
}

func TestHelperStartFramesClose(t *testing.T) {
	h, f, err := launchFake(t, mockCapsJSON, replyStarted(3))
	if err != nil {
		t.Fatal(err)
	}
	if c := h.Caps(); c.Backend != "mock" || !c.Usable() || h.QPCFrequency() != 10_000_000 {
		t.Fatalf("caps %+v", c)
	}
	st, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000})
	if err != nil || st.Width != 320 || st.Capture != "synthetic" {
		t.Fatalf("start: %+v %v", st, err)
	}
	if m := <-f.msgs; m["codec"] != "h264" || m["kbps"] != float64(4000) {
		t.Fatalf("start message %v", m)
	}
	for i := 1; i <= 3; i++ {
		fr := nextFrame(t, h)
		if fr.FrameID != uint64(i) || fr.Key != (i == 1) || fr.Data[4] != byte(i) {
			t.Fatalf("frame %d: %+v", i, fr)
		}
	}

	// Commands reach the helper with the protocol's field names.
	acked := uint64(2)
	if err := h.ForceIDR(); err != nil {
		t.Fatal(err)
	}
	h.Recover(3, &acked)
	h.SetRate(2500, 1.5, 90)
	h.SetROI(nil)
	for _, want := range []string{"forceIdr", "recover", "setRate", "setRoi"} {
		m := <-f.msgs
		if m["t"] != want {
			t.Fatalf("got %v, want %s", m, want)
		}
		switch want {
		case "recover":
			if m["lostFromFrameId"] != float64(3) || m["ackedLtrFrameId"] != float64(2) {
				t.Fatalf("recover %v", m)
			}
		case "setRate":
			if m["kbps"] != float64(2500) || m["vbvFrames"] != 1.5 || m["fps"] != float64(90) {
				t.Fatalf("setRate %v", m)
			}
		case "setRoi":
			if rects, ok := m["rects"].([]any); !ok || len(rects) != 0 {
				t.Fatalf("setRoi %v", m)
			}
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := h.Err(); err != nil {
		t.Fatalf("clean close reported %v", err)
	}
	<-f.released
	if _, ok := <-h.Frames(); ok {
		t.Fatal("frames channel still open after Close")
	}
	if err := h.ForceIDR(); !errors.Is(err, ErrClosed) {
		t.Fatalf("command after close: %v", err)
	}
}

func TestHelperStartRejected(t *testing.T) {
	h, _, err := launchFake(t, mockCapsJSON, func(f *fakeHelper, m map[string]any) {
		if m["t"] == "start" {
			f.send(HelperError{Code: "unsupported", Text: "the mock backend only encodes h264", Re: "start"})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, err = h.Start(StartParams{Codec: "hevc", FPS: 60, Kbps: 4000})
	var he *HelperError
	if !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
		t.Fatalf("start: %v", err)
	}
	// Non-fatal: the helper is still there.
	select {
	case <-h.Done():
		t.Fatal("helper gone after a non-fatal error")
	default:
	}
	// Start returned the error; it is not repeated on Errors.
	select {
	case e := <-h.Errors():
		t.Fatalf("start error also on Errors: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHelperFatal(t *testing.T) {
	h, _, err := launchFake(t, mockCapsJSON, func(f *fakeHelper, m map[string]any) {
		replyStarted(2)(f, m)
		if m["t"] == "start" {
			f.send(HelperError{Code: "mock_fatal", Text: "injected", Fatal: true})
			f.exit(3)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	if e := nextError(t, h); !e.Fatal || e.Code != "mock_fatal" {
		t.Fatalf("got %v", e)
	}
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done not closed after the helper exited")
	}
	// Frames published before the exit are still delivered, then the channel closes.
	n := 0
	for range h.Frames() {
		n++
	}
	if n != 2 {
		t.Fatalf("%d frames after fatal exit, want 2", n)
	}
	var he *HelperError
	if err := h.Err(); !errors.As(err, &he) || he.Code != "mock_fatal" {
		t.Fatalf("Err() = %v", err)
	}
	if err := h.Close(); !errors.As(err, &he) {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := h.Start(StartParams{Codec: "h264"}); err == nil {
		t.Fatal("start after exit succeeded")
	}
}

// A helper that reported a fatal error exits; one that does not (threads stuck
// in the driver) is killed after a short grace, not only at Close.
func TestHelperFatalKillsStuckHelper(t *testing.T) {
	h, f, err := launchFake(t, mockCapsJSON, func(f *fakeHelper, m map[string]any) {
		if m["t"] == "forceIdr" {
			f.send(HelperError{Code: "encode_failed", Text: "stuck in the driver", Fatal: true}) // and no exit
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	start := time.Now()
	if err := h.ForceIDR(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(closeGrace):
		t.Fatal("helper not killed after its fatal error")
	}
	if d := time.Since(start); d < fatalExitGrace {
		t.Fatalf("killed %v after the fatal error, before the grace period", d)
	}
	var he *HelperError
	if err := h.Err(); !errors.As(err, &he) || he.Code != "encode_failed" {
		t.Fatalf("Err() = %v", err)
	}
	if f.code != 1 {
		t.Fatalf("helper exited with %d, want killed (1)", f.code)
	}
}

// A helper that stops reading its stdin (suspended, frozen in a debugger) must
// stall neither the callers nor Close, which kills it.
func TestHelperFrozenHelper(t *testing.T) {
	frozen := make(chan struct{})
	h, f, err := launchFake(t, mockCapsJSON, func(f *fakeHelper, m map[string]any) {
		if m["t"] == "setRate" {
			<-frozen // never reads stdin again
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close(frozen)
	start := time.Now()
	full := false
	for i := 0; i < 2*ctrlQueueLen && !full; i++ {
		full = h.SetRate(1000+i, 0, 0) != nil
	}
	if !full {
		t.Fatal("SetRate never reported the full queue")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("SetRate blocked for %v", d)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	select {
	case <-closed:
	case <-time.After(closeGrace + 3*time.Second):
		t.Fatal("Close blocked on a helper that stopped reading")
	}
	<-f.exited
	if f.code != 1 {
		t.Fatalf("helper exited with %d, want killed (1)", f.code)
	}
	<-f.released
}

// Requests the helper would treat as a fatal protocol error, or can never
// accept, fail locally and leave the helper running.
func TestHelperRejectsOversizedRequests(t *testing.T) {
	h, f, err := launchFake(t, mockCapsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.SetROI(make([]ROIRect, MaxROIRects+1)); err == nil {
		t.Fatal("SetROI accepted more than MaxROIRects rects")
	}
	if err := h.send(simpleMsg{T: strings.Repeat("x", MaxControlMsg)}); err == nil {
		t.Fatal("send accepted a message above MaxControlMsg")
	}
	// Nothing was written (the fake would have quit on an over-long message).
	if err := h.SetROI(make([]ROIRect, MaxROIRects)); err != nil {
		t.Fatal(err)
	}
	if m := <-f.msgs; m["t"] != "setRoi" || len(m["rects"].([]any)) != MaxROIRects {
		t.Fatalf("got %v", m["t"])
	}
	select {
	case <-h.Done():
		t.Fatalf("helper gone: %v", h.Err())
	default:
	}
}

func TestHelperUnexpectedExit(t *testing.T) {
	h, f, err := launchFake(t, mockCapsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.exit(5)
	<-h.Done()
	var ee *ExitError
	if !errors.As(h.Err(), &ee) || ee.Code != 5 {
		t.Fatalf("Err() = %v", h.Err())
	}
	h.Close()
}

func TestHelperRingCorruptKillsHelper(t *testing.T) {
	h, _, err := launchFake(t, mockCapsJSON, func(f *fakeHelper, m map[string]any) {
		if m["t"] == "start" {
			f.send(Started{Backend: "mock"})
			f.publishMangled(&Frame{FrameID: 1, Data: []byte{1}}, func(s []byte) { s[slotPayloadOffset] = 1 })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000})
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("corrupt ring did not stop the helper")
	}
	if !errors.Is(h.Err(), ErrRingCorrupt) {
		t.Fatalf("Err() = %v", h.Err())
	}
	if _, ok := <-h.Frames(); ok {
		t.Fatal("corrupt frame delivered")
	}
}

func TestHelperBadCaps(t *testing.T) {
	for _, caps := range []string{
		`{"t":"caps","v":2,"backend":"mock","qpcFrequency":10000000}`, // protocol mismatch
		`{"t":"caps","v":1,"backend":"mock","qpcFrequency":0}`,        // ring not attached
		`{"t":"stats","frameId":1}`,                                   // no caps at all (times out)
	} {
		h, f, err := launchFake(t, caps, nil)
		if err == nil {
			h.Close()
			t.Fatalf("%s: launch succeeded", caps)
		}
		<-f.exited // the client stopped the helper
	}
}

func TestHelperBackpressureDropsNewest(t *testing.T) {
	h, f, err := launchFake(t, mockCapsJSON, replyStarted(0))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	// Nobody reads Frames: the channel (4) and the ring (4 slots) fill, then
	// the producer drops the newest frames.
	var written []uint64
	for id := uint64(1); id <= 30; id++ {
		if f.publish(&Frame{FrameID: id, Data: []byte{byte(id)}}) {
			written = append(written, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(written) == 30 {
		t.Fatal("producer never saw a full ring")
	}
	last := uint64(0)
	for _, id := range written {
		fr := nextFrame(t, h)
		if fr.FrameID != id || uint64(fr.DroppedBefore) != id-last-1 {
			t.Fatalf("frame %d (DroppedBefore %d), want %d after %d", fr.FrameID, fr.DroppedBefore, id, last)
		}
		last = id
	}
	// Every written frame was delivered, so the ring is empty again: the next
	// frame is written and carries the gap.
	if !f.publish(&Frame{FrameID: 31, Data: []byte{31}}) {
		t.Fatal("ring still full after the reader caught up")
	}
	fr := nextFrame(t, h)
	if fr.DroppedBefore == 0 || uint64(fr.DroppedBefore) != fr.FrameID-last-1 {
		t.Fatalf("frame %d after %d reports DroppedBefore %d", fr.FrameID, last, fr.DroppedBefore)
	}
}

func TestLaunchUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Launch is supported on Windows")
	}
	if _, err := Launch(Options{Exe: "recon-encoder.exe"}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Launch: %v", err)
	}
}
