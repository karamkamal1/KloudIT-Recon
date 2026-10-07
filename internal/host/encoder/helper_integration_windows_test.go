//go:build windows

package encoder

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Integration tests against the real recon-encoder.exe with its mock backend
// (no GPU needed). They run when RECON_HELPER_EXE points at the helper:
//
//	RECON_HELPER_EXE=C:\path\recon-encoder.exe go test -run HelperIntegration ./internal/host/encoder
//
// RECON_HELPER_DUMP=file.h264 also writes the bitstream received before the
// ring-full check, which must decode cleanly (ffmpeg -i file.h264 -f null -).

func helperExe(t *testing.T) string {
	t.Helper()
	exe := os.Getenv("RECON_HELPER_EXE")
	if exe == "" {
		t.Skip("set RECON_HELPER_EXE to recon-encoder.exe to run the helper integration tests")
	}
	return exe
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

func launchMock(t *testing.T, extra ...string) *Helper {
	t.Helper()
	h, err := Launch(Options{
		Exe:      helperExe(t),
		Backend:  "mock",
		LogLevel: "debug",
		Args:     extra,
		Log:      slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// nalTypes lists the H.264 NAL unit types in an Annex-B access unit.
func nalTypes(au []byte) []byte {
	var types []byte
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			types = append(types, au[i+3]&0x1f)
			i += 2
		}
	}
	return types
}

func waitHelperError(t *testing.T, h *Helper, code string) *HelperError {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-h.Errors():
			var he *HelperError
			if errors.As(err, &he) && he.Code == code {
				return he
			}
			t.Logf("ignoring error %v", err)
		case <-deadline:
			t.Fatalf("no %q error", code)
		}
	}
}

func TestHelperIntegrationMock(t *testing.T) {
	h := launchMock(t)
	var dump *os.File
	if p := os.Getenv("RECON_HELPER_DUMP"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		dump = f
	}

	// Caps: the mock, plus the real backends probed and reported unavailable.
	c := h.Caps()
	h264 := c.Codecs["h264"]
	if c.V != ProtocolVersion || c.Backend != "mock" || c.Vendor != "mock" || !c.Usable() || len(c.Codecs) != 1 ||
		!h264.ForceIDR || h264.Recovery != "none" || h264.LiveBitrate != "seamless" || h264.MaxW != 320 ||
		len(c.Capture) != 1 || c.Capture[0] != "synthetic" || c.QPCFrequency <= 0 {
		t.Fatalf("caps %+v", c)
	}
	for _, n := range []string{"amf", "nvenc", "dda", "amd-direct", "wgc"} {
		if c.Unavailable[n] == "" {
			t.Errorf("caps do not say why %s is unavailable: %v", n, c.Unavailable)
		}
	}
	t.Logf("unavailable: %v", c.Unavailable)

	// Before start, and bad requests: non-fatal errors, the helper keeps running.
	if err := h.ForceIDR(); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "not_started"); e.Fatal || e.Re != "forceIdr" {
		t.Fatalf("forceIdr before start: %v", e)
	}
	if err := h.send(simpleMsg{T: "bogus"}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "bad_message"); e.Fatal {
		t.Fatalf("unknown message: %v", e)
	}
	_, err := h.Start(StartParams{Codec: "hevc", FPS: 60, Kbps: 4000})
	var he *HelperError
	if !errors.As(err, &he) || he.Code != "unsupported" || he.Fatal {
		t.Fatalf("hevc start on the mock: %v", err)
	}
	_, err = h.Start(StartParams{Codec: "h264", FPS: 9999, Kbps: 4000})
	if !errors.As(err, &he) || he.Code != "bad_message" {
		t.Fatalf("start with fps 9999: %v", err)
	}

	st, err := h.Start(StartParams{Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 4000})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Backend != "mock" || st.Capture != "synthetic" || st.Codec != "h264" || st.Width != 320 || st.Height != 180 {
		t.Fatalf("started %+v", st)
	}
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); !errors.As(err, &he) || he.Code != "already_started" {
		t.Fatalf("second start: %v", err)
	}

	var last *Frame
	sinceKey := 0
	// check validates every frame read and keeps the position in the clip.
	check := func(f *Frame) {
		t.Helper()
		if last != nil && f.FrameID != last.FrameID+1+uint64(f.DroppedBefore) {
			t.Fatalf("frame %d after %d (DroppedBefore %d)", f.FrameID, last.FrameID, f.DroppedBefore)
		}
		if !bytes.HasPrefix(f.Data, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(f.Data, []byte{0, 0, 1}) {
			t.Fatalf("frame %d is not Annex-B: % x", f.FrameID, f.Data[:min(8, len(f.Data))])
		}
		types := nalTypes(f.Data)
		hasIDR := bytes.IndexByte(types, 5) >= 0
		if f.Key != hasIDR || (f.Key && (bytes.IndexByte(types, 7) < 0 || bytes.IndexByte(types, 8) < 0)) ||
			(!f.Key && bytes.IndexByte(types, 1) < 0) {
			t.Fatalf("frame %d key=%v has NAL types %v", f.FrameID, f.Key, types)
		}
		if f.CaptureQPC <= 0 || f.SubmitQPC < f.CaptureQPC || f.OutputQPC < f.SubmitQPC || f.Width != 320 || f.Height != 180 {
			t.Fatalf("frame %d: %+v", f.FrameID, *f)
		}
		if dump != nil {
			dump.Write(f.Data)
		}
		if f.Key {
			sinceKey = 0
		} else {
			sinceKey++
		}
		last = f
	}
	next := func() *Frame {
		t.Helper()
		select {
		case f, ok := <-h.Frames():
			if !ok {
				t.Fatalf("frames closed: %v", h.Err())
			}
			check(f)
			return f
		case <-time.After(5 * time.Second):
			t.Fatalf("no frame: %v", h.Err())
		}
		return nil
	}

	// The stream starts with an IDR (frame 1), then P frames.
	if f := next(); !f.Key || f.FrameID != 1 {
		t.Fatalf("first frame %+v", *f)
	}
	for i := 0; i < 10; i++ {
		if f := next(); f.Key {
			t.Fatalf("unexpected key frame %d", f.FrameID)
		}
	}

	// forceIdr / recover (no LTR on the mock: IDR) give a key frame within a
	// few frames. The canned clip has its own IDR every 60 frames, so only
	// ask when that one is far away.
	for _, ask := range []func() error{
		h.ForceIDR,
		func() error { return h.Recover(last.FrameID, nil) },
	} {
		for sinceKey < 5 || sinceKey > 40 {
			next()
		}
		asked := last.FrameID
		if err := ask(); err != nil {
			t.Fatal(err)
		}
		for !next().Key {
			if last.FrameID-asked > 6 {
				t.Fatalf("no key frame within 6 frames of the request at %d", asked)
			}
		}
		t.Logf("key frame %d, %d frames after the request", last.FrameID, last.FrameID-asked)
	}

	// setRate is recorded and reported in the stats.
	if err := h.SetRate(2500, 1.5, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.SetROI([]ROIRect{{X: 0, Y: 0, W: 64, H: 64, Weight: 5}}); err != nil {
		t.Fatal(err)
	}
	statsDeadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case s := <-h.Stats():
			got = s.Kbps == 2500 && s.VBVFrames == 1.5 && s.FPS == 60
		case f := <-h.Frames(): // keep the ring moving
			check(f)
		case err := <-h.Errors():
			if he, ok := err.(*HelperError); !ok || he.Fatal || he.Re == "setRate" || he.Re == "setRoi" {
				t.Fatalf("error after setRate/setRoi: %v", err)
			}
		case <-statsDeadline:
			t.Fatal("stats never reported the new rate")
		}
	}

	// The dump stops here: what follows loses frames on purpose.
	if dump != nil {
		dump.Close()
		dump = nil
	}

	// Ring full: stop reading frames for a while (stats keep flowing). The
	// helper must drop the newest frames and say so, never stall.
	pause := time.After(1500 * time.Millisecond)
	droppedStats := 0
	for paused := true; paused; {
		select {
		case s := <-h.Stats():
			if s.Dropped {
				if s.Reason != "ringFull" || s.RingDropped == 0 {
					t.Fatalf("drop stats %+v", s)
				}
				droppedStats++
			}
		case <-pause:
			paused = false
		}
	}
	if droppedStats == 0 {
		t.Fatal("no dropped=true stats while frames were not read")
	}
	t.Logf("%d frames reported dropped while paused", droppedStats)
	// Reading again: the frames that were buffered, then the first frame
	// after the drops carries DroppedBefore (next() checks the id arithmetic).
	gapSeen := false
	for i := 0; i < 40 && !gapSeen; i++ {
		gapSeen = next().DroppedBefore > 0
	}
	if !gapSeen {
		t.Fatal("no frame reported the dropped frames")
	}

	// Clean shutdown.
	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close hung")
	}
	if err := h.Err(); err != nil {
		t.Fatalf("clean shutdown reported %v", err)
	}
}

func TestHelperIntegrationErrors(t *testing.T) {
	// A non-fatal error: reported, frames keep coming.
	h := launchMock(t, "--mock-error-at=5")
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "mock_error"); e.Fatal {
		t.Fatalf("mock_error is fatal: %v", e)
	}
	for f := range h.Frames() {
		if f.FrameID >= 10 {
			break
		}
	}
	select {
	case <-h.Done():
		t.Fatalf("helper exited after a non-fatal error: %v", h.Err())
	default:
	}
	h.Close()

	// A fatal error: reported, the helper exits, Frames closes; a restarted
	// helper starts again with an IDR (what recon-host does).
	h = launchMock(t, "--mock-fatal-at=20")
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	if e := waitHelperError(t, h, "mock_fatal"); !e.Fatal {
		t.Fatalf("mock_fatal not fatal: %v", e)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after a fatal error")
	}
	n := 0
	for range h.Frames() {
		n++
	}
	var he *HelperError
	if err := h.Err(); !errors.As(err, &he) || he.Code != "mock_fatal" {
		t.Fatalf("Err() = %v", err)
	}
	t.Logf("%d frames before the fatal error", n)
	if err := h.ForceIDR(); err == nil {
		t.Fatal("command to an exited helper succeeded")
	}
	h.Close()

	start := time.Now()
	h = launchMock(t)
	if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-h.Frames():
		if f == nil || !f.Key || f.FrameID != 1 {
			t.Fatalf("first frame after restart: %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame after restart")
	}
	t.Logf("restart to first frame: %v", time.Since(start))

	// Broken framing (a length above the limit) is fatal.
	h2 := launchMock(t)
	h2.ctrlQ <- []byte{0xff, 0xff, 0xff, 0x7f}
	if e := waitHelperError(t, h2, "protocol"); !e.Fatal {
		t.Fatalf("protocol error not fatal: %v", e)
	}
	<-h2.Done()
	if !errors.As(h2.Err(), &he) || he.Code != "protocol" {
		t.Fatalf("Err() after protocol error = %v", h2.Err())
	}

	// Killed from outside: Done, an ExitError, Frames closes.
	h.c.kill()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after kill")
	}
	var ee *ExitError
	if !errors.As(h.Err(), &ee) {
		t.Fatalf("Err() after kill = %v", h.Err())
	}
	for range h.Frames() {
	}
}

func TestHelperIntegrationStuckExit(t *testing.T) {
	// A capture/encoder call stuck in the driver (the mock never returns from
	// submitting frame 5) must not keep the helper alive once it should exit:
	// its watchdog ends it with code 4 about 500 ms later.
	const exitStuck = 4
	stuck := func() *Helper {
		t.Helper()
		h := launchMock(t, "--mock-hang-at=5")
		if _, err := h.Start(StartParams{Codec: "h264", FPS: 60, Kbps: 4000}); err != nil {
			t.Fatal(err)
		}
		for f := range h.Frames() {
			if f.FrameID == 4 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond) // frame 5 is stuck in submit by now
		return h
	}
	var ee *ExitError

	// recon-host went away: stdin EOF, and nobody is left to kill the helper.
	h := stuck()
	start := time.Now()
	h.c.ctrlW.Close() // writeLoop is idle
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper with a stuck thread still running after stdin EOF")
	}
	t.Logf("exited %v after stdin EOF", time.Since(start))
	if !errors.As(h.Err(), &ee) || ee.Code != exitStuck {
		t.Fatalf("Err() after stdin EOF = %v", h.Err())
	}

	// Close (shutdown): the helper ends itself well before Close would kill it.
	h = stuck()
	start = time.Now()
	h.Close()
	if d := time.Since(start); d >= closeGrace {
		t.Fatalf("Close took %v: the helper did not end itself", d)
	}
	t.Logf("Close took %v", time.Since(start))
	if !errors.As(h.Err(), &ee) || ee.Code != exitStuck {
		t.Fatalf("Err() after Close = %v", h.Err())
	}
}

func TestHelperIntegrationAuto(t *testing.T) {
	// The real backends: in CI and under Wine there is no GPU runtime, so the
	// caps must say so and start must fail cleanly (recon-host then uses FFmpeg).
	h, err := Launch(Options{Exe: helperExe(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.Caps()
	t.Logf("auto caps: backend=%s vendor=%s adapter=%q unavailable=%v", c.Backend, c.Vendor, c.AdapterName, c.Unavailable)
	if c.Usable() {
		t.Skip("a real encoder backend is available here")
	}
	_, err = h.Start(StartParams{Codec: "hevc", FPS: 60, Kbps: 20000})
	var he *HelperError
	if !errors.As(err, &he) || he.Code != "unavailable" || he.Fatal {
		t.Fatalf("start without a backend: %v", err)
	}

	// A small ring works too.
	h2, err := Launch(Options{Exe: helperExe(t), Backend: "mock", Slots: 2, SlotSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	h2.Close()
}

func TestHelperIntegrationCommandLine(t *testing.T) {
	exe := helperExe(t)
	// --print-caps prints the same caps message, unframed.
	out, err := exec.Command(exe, "--print-caps", "--backend=mock").Output()
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMessage(bytes.TrimSpace(out))
	if c, ok := m.(*Caps); err != nil || !ok || c.Backend != "mock" || c.V != ProtocolVersion {
		t.Fatalf("--print-caps: %v %v (%s)", m, err, out)
	}
	// Handles that were not inherited: the helper reports a fatal ring error
	// on stdout and exits with code 2.
	cmd := exec.Command(exe, "--ring-handle=0x1234", "--ring-size=200704", "--event-handle=0x1238", "--backend=mock")
	out, err = cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("bad handles: exit %v", err)
	}
	b, err := proto.ReadMsg(bytes.NewReader(out), MaxControlMsg)
	if err != nil {
		t.Fatalf("bad handles: no framed message in %q", out)
	}
	m, err = decodeMessage(b)
	if e, ok := m.(*HelperError); err != nil || !ok || e.Code != "ring" || !e.Fatal {
		t.Fatalf("bad handles: %v %v", m, err)
	}
	// Bad arguments: usage on stderr, exit code 2.
	if err := exec.Command(exe, "--frobnicate").Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("unknown argument: %v", err)
	}
	if err := exec.Command(exe, "--print-caps", "--backend=amf", "--mock-fatal-at=3").Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("mock option without the mock backend: %v", err)
	}
}
