package media

import (
	"bytes"
	"errors"
	"image"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Phase 5 session wiring, part B (encoder options) in HelperVideo: the start
// options from the caps x the host config, the viewer's focus as regions of
// interest, the first-slice and re-encode marks of the frames.

// lineLog collects log output (any goroutine).
type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lineLog) lines(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if line != "" && strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// fakeP5BCaps: a helper's caps with one H.264 encoder whose caps members are
// extended by extra (`,"roi":"importance"`, ...).
func fakeP5BCaps(extra string) string {
	return `{"t":"caps","v":1,"helperVersion":"test","backend":"amf","vendor":"amd","adapterLuid":"00000000:0000c3a1",
"adapterName":"GPU","hagsEnabled":true,
"codecs":{"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",
"alignW":1,"alignH":1` + extra + `}},"capture":["dda"],"cursorInVideo":false,"outputs":[],"qpcFrequency":10000000}`
}

// TestHelperEncoderOptions: the engine, re-encode and slice-output options
// of a start by host config x the codec's caps (vendor-neutral: the caps of
// an AMF-like and an NVENC-like encoder), each decision logged once.
func TestHelperEncoderOptions(t *testing.T) {
	amfLike := encoder.CodecCaps{HWInstances: 2, InstanceSelect: true, SliceOutput: true, ROI: "importance"}
	nvencLike := encoder.CodecCaps{HWInstances: 3, Reencode: true, ROI: "emphasis"}
	single := encoder.CodecCaps{HWInstances: 1, InstanceSelect: true}
	for _, c := range []struct {
		name     string
		opt      HelperOptions
		cc       encoder.CodecCaps
		engine   int // -1: nil (the backend's default)
		reencode float64
		slices   int
		log      string // the decision logged
	}{
		{"default", HelperOptions{}, amfLike, -1, 0, 0, `msg="encoder engine: the backend's default (engine 0)" codec=h264 config=auto engines=2`},
		{"auto", HelperOptions{EncoderInstance: "auto"}, amfLike, -1, 0, 0, `config=auto engines=2`},
		{"dedicated, two engines", HelperOptions{EncoderInstance: "dedicated"}, amfLike, 1, 0, 0, `msg="encoder engine" codec=h264 config=dedicated engine=1`},
		{"dedicated, engines picked by the encoder", HelperOptions{EncoderInstance: "dedicated"}, nvencLike, -1, 0, 0, `spreads its work over its engines`},
		{"dedicated, one engine", HelperOptions{EncoderInstance: "dedicated"}, single, -1, 0, 0, `the GPU has one engine`},
		{"engine 1", HelperOptions{EncoderInstance: "1"}, amfLike, 1, 0, 0, `engine=1 engines=2`},
		{"engine 0", HelperOptions{EncoderInstance: "0"}, amfLike, 0, 0, 0, `engine=0 engines=2`},
		{"engine 1, engines picked by the encoder", HelperOptions{EncoderInstance: "1"}, nvencLike, -1, 0, 0, `picks its engines itself`},
		{"engine 5 of 2", HelperOptions{EncoderInstance: "5"}, amfLike, -1, 0, 0, `the GPU has 2 engine(s)`},
		{"re-encode", HelperOptions{ReencodeOversized: 3}, nvencLike, -1, 3, 0, `msg="re-encoding oversized frames" codec=h264 average_frames=3`},
		{"re-encode without the cap", HelperOptions{ReencodeOversized: 3}, amfLike, -1, 0, 0, `re-encoding oversized frames not used`},
		{"slices", HelperOptions{SliceOutput: 4}, amfLike, -1, 0, 4, `msg="sub-frame output (frames still sent whole)" codec=h264 slices=4`},
		{"slices without the cap", HelperOptions{SliceOutput: 4}, nvencLike, -1, 0, 0, `sub-frame output not used`},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &lineLog{}
			opt := c.opt
			opt.Log = slog.New(slog.NewTextHandler(logs, nil))
			v := NewHelperVideo(opt)
			defer v.Stop()
			caps := encoder.Caps{Codecs: map[string]encoder.CodecCaps{"h264": c.cc}}
			sp, _ := v.startParams(helperParams())
			sp, _ = v.withCaps(sp, true, caps)
			engine := -1
			if sp.EncoderInstance != nil {
				engine = *sp.EncoderInstance
			}
			if engine != c.engine || sp.ReencodeOversized != c.reencode || sp.SliceOutput != c.slices {
				t.Fatalf("engine %d reencode %v slices %d, want %d %v %d", engine, sp.ReencodeOversized, sp.SliceOutput, c.engine, c.reencode, c.slices)
			}
			if l := logs.lines(c.log); len(l) != 1 {
				t.Fatalf("decision %q logged %d times: %q", c.log, len(l), logs.lines(""))
			}
			// Logged once: the next helper's start logs nothing new.
			n := len(logs.lines(""))
			sp2, _ := v.startParams(helperParams())
			v.withCaps(sp2, true, caps)
			if len(logs.lines("")) != n {
				t.Fatalf("a second start logged again: %q", logs.lines(""))
			}
			// They are no reason for a new helper: a bitrate change stays in
			// place (sameHelperStream).
			later := sp2
			later.Kbps = 5000
			if !sameHelperStream(sp, later) {
				t.Fatalf("%+v and %+v not the same stream", sp, later)
			}
		})
	}
}

// startFocusRig starts a HelperVideo on fake helpers with caps whose start
// answers capture 1920x1080 encoded at 1280x720, and returns it with the
// first helper after its start message.
func startFocusRig(t *testing.T, caps string, opt HelperOptions) (*HelperVideo, *fakeHelpers, *encoder.Fake, *lineLog) {
	t.Helper()
	clock := testClock()
	fh := newFakeHelpers(t, caps, func(f *encoder.Fake, m map[string]any) {
		slices, _ := m["sliceOutput"].(float64)
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1280, Height: 720, CaptureWidth: 1920,
			CaptureHeight: 1080, FPS: 60, Kbps: 20000, LiveBitrate: "seamless", LiveFPS: "seamless", SliceOutput: int(slices)})
	})
	logs := &lineLog{}
	opt.Launch, opt.Clock, opt.Log = fh.launch, clock, slog.New(slog.NewTextHandler(logs, nil))
	v := NewHelperVideo(opt)
	t.Cleanup(v.Stop)
	if err := v.Start(helperParams(), false); err != nil {
		t.Fatal(err)
	}
	f := fh.nextStarted()
	expectMsg(t, f, "start")
	f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Dirty: -1, Data: h264Key, CaptureQPC: qpcAt(clock, 1_000_000)})
	if c := nextEvent(t, v).Config; c == nil {
		t.Fatal("no config")
	}
	nextEvent(t, v)
	return v, fh, f, logs
}

// nextMsg returns the next message the fake received.
func nextMsg(t *testing.T, f *encoder.Fake) map[string]any {
	t.Helper()
	select {
	case m := <-f.Messages():
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("the helper got no message")
	}
	return nil
}

// roiRects decodes a setRoi message's rects.
func roiRects(t *testing.T, m map[string]any) []encoder.ROIRect {
	t.Helper()
	if m["t"] != "setRoi" {
		t.Fatalf("message %v, want setRoi", m)
	}
	var out []encoder.ROIRect
	for _, r := range m["rects"].([]any) {
		o := r.(map[string]any)
		out = append(out, encoder.ROIRect{X: int(o["x"].(float64)), Y: int(o["y"].(float64)), W: int(o["w"].(float64)),
			H: int(o["h"].(float64)), Weight: int(o["weight"].(float64))})
	}
	return out
}

// TestHelperVideoFocus: SetFocus becomes the helper's regions of interest:
// the pointer's position across the picture mapped to the capture (1920x1080)
// and scaled to the stream (1280x720), the centre square with the background
// behind it; the same regions are not sent again; the zero focus clears them;
// a helper started later (a restart) gets the current focus right after its
// start; an encoder without a map gets nothing (ErrNoROI) and one that refuses
// the map gets no more.
func TestHelperVideoFocus(t *testing.T) {
	roiCaps := fakeP5BCaps(`,"roi":"importance"`)
	t.Run("regions", func(t *testing.T) {
		v, fh, f, _ := startFocusRig(t, roiCaps, HelperOptions{})
		if c := v.Capabilities(); !c.ROI {
			t.Fatalf("capabilities %+v: want ROI", c)
		}
		// The middle of the picture: capture pixel (959, 539), its 135 px
		// square from (892, 472) scaled by 2/3 and rounded outwards.
		if err := v.SetFocus(Focus{Pointer: true, X: 32768, Y: 32768}); err != nil {
			t.Fatal(err)
		}
		if got, want := roiRects(t, nextMsg(t, f)), []encoder.ROIRect{{X: 594, Y: 314, W: 91, H: 91, Weight: 6}}; !slices.Equal(got, want) {
			t.Fatalf("pointer regions %v, want %v", got, want)
		}
		// The top-left corner: clipped to the picture.
		if err := v.SetFocus(Focus{Pointer: true}); err != nil {
			t.Fatal(err)
		}
		if got, want := roiRects(t, nextMsg(t, f)), []encoder.ROIRect{{X: 0, Y: 0, W: 46, H: 46, Weight: 6}}; !slices.Equal(got, want) {
			t.Fatalf("corner regions %v, want %v", got, want)
		}
		// The same regions again: nothing sent (the next message is the
		// key frame request below).
		if err := v.SetFocus(Focus{Pointer: true, X: 1, Y: 1}); err != nil {
			t.Fatal(err)
		}
		if err := v.ForceKeyframe(); err != nil {
			t.Fatal(err)
		}
		if m := nextMsg(t, f); m["t"] != "forceIdr" {
			t.Fatalf("message %v: the same regions sent again", m)
		}
		center := Focus{Center: true, Background: -2}
		if err := v.SetFocus(center); err != nil {
			t.Fatal(err)
		}
		want := encoder.FocusROI(1920, 1080, 1280, 720, nil, encoder.FocusOptions{Background: -2})
		if got := roiRects(t, nextMsg(t, f)); !slices.Equal(got, want) || len(got) != 2 || got[0] != (encoder.ROIRect{W: 1280, H: 720, Weight: -2}) {
			t.Fatalf("centre regions %v, want %v", got, want)
		}
		if err := v.SetFocus(Focus{}); err != nil {
			t.Fatal(err)
		}
		if got := roiRects(t, nextMsg(t, f)); len(got) != 0 {
			t.Fatalf("cleared: %v", got)
		}
		// A restart: the new helper gets the focus right after its start.
		if err := v.SetFocus(center); err != nil {
			t.Fatal(err)
		}
		roiRects(t, nextMsg(t, f))
		f.Exit(1)
		f2 := fh.nextStarted()
		expectMsg(t, f2, "start")
		if got := roiRects(t, nextMsg(t, f2)); !slices.Equal(got, want) {
			t.Fatalf("restarted helper's regions %v, want %v", got, want)
		}
	})
	t.Run("pointer mapped to the capture", func(t *testing.T) {
		st := encoder.Started{Width: 1280, Height: 720, CaptureWidth: 2560, CaptureHeight: 1440}
		got := focusRects(st, Focus{Pointer: true, X: 65535, Y: 0})
		want := encoder.FocusROI(2560, 1440, 1280, 720, &image.Point{X: 2559, Y: 0}, encoder.FocusOptions{CenterSize: -1})
		if !slices.Equal(got, want) || len(got) != 1 || got[0].X+got[0].W != 1280 || got[0].Y != 0 {
			t.Fatalf("right edge: %v, want %v", got, want)
		}
		// An older helper's started without the capture size: the stream's.
		got = focusRects(encoder.Started{Width: 1280, Height: 720}, Focus{Pointer: true, X: 32768, Y: 32768})
		want = encoder.FocusROI(1280, 720, 1280, 720, &image.Point{X: 639, Y: 359}, encoder.FocusOptions{CenterSize: -1})
		if !slices.Equal(got, want) {
			t.Fatalf("no capture size: %v, want %v", got, want)
		}
		if focusRects(st, Focus{Background: -2}) != nil {
			t.Fatal("regions for a focus on nothing")
		}
	})
	t.Run("no map", func(t *testing.T) {
		v, _, f, _ := startFocusRig(t, fakeP5BCaps(`,"roi":"none"`), HelperOptions{})
		if c := v.Capabilities(); c.ROI {
			t.Fatalf("capabilities %+v: ROI without a map", c)
		}
		if err := v.SetFocus(Focus{Center: true}); !errors.Is(err, ErrNoROI) {
			t.Fatalf("SetFocus: %v, want ErrNoROI", err)
		}
		if err := v.ForceKeyframe(); err != nil {
			t.Fatal(err)
		}
		if m := nextMsg(t, f); m["t"] != "forceIdr" {
			t.Fatalf("message %v: regions sent to an encoder without a map", m)
		}
	})
	t.Run("refused", func(t *testing.T) {
		v, _, f, logs := startFocusRig(t, roiCaps, HelperOptions{})
		if err := v.SetFocus(Focus{Center: true}); err != nil {
			t.Fatal(err)
		}
		roiRects(t, nextMsg(t, f))
		f.Send(encoder.HelperError{Code: "encode_failed", Text: "allocating the ROI map: AMF_OUT_OF_MEMORY", Re: "setRoi"})
		for deadline := time.Now().Add(5 * time.Second); v.Capabilities().ROI; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("ROI still on after the helper refused the map")
			}
		}
		if err := v.SetFocus(Focus{Pointer: true}); !errors.Is(err, ErrNoROI) {
			t.Fatalf("SetFocus after the refusal: %v", err)
		}
		if err := v.ForceKeyframe(); err != nil {
			t.Fatal(err)
		}
		if m := nextMsg(t, f); m["t"] != "forceIdr" {
			t.Fatalf("message %v: regions sent after the refusal", m)
		}
		if l := logs.lines("refused the regions of interest"); len(l) != 1 {
			t.Fatalf("refusal logged %d times", len(l))
		}
	})
}

// TestHelperVideoSliceStamps: a frame's first-slice time (sub-frame output)
// in host time when it lies between the encoder submit and the whole frame,
// and the re-encode mark.
func TestHelperVideoSliceStamps(t *testing.T) {
	v, _, f, _ := startFocusRig(t, fakeP5BCaps(`,"sliceOutput":true`), HelperOptions{SliceOutput: 2})
	clock := v.opt.Clock
	for _, c := range []struct {
		fr    encoder.Frame
		first uint64
	}{
		{encoder.Frame{FrameID: 2, Slices: 2, SubmitQPC: qpcAt(clock, 1_016_000), FirstSliceQPC: qpcAt(clock, 1_017_500),
			OutputQPC: qpcAt(clock, 1_019_000)}, 1_017_500},
		{encoder.Frame{FrameID: 3, Slices: 2, SubmitQPC: qpcAt(clock, 1_032_000), FirstSliceQPC: qpcAt(clock, 1_036_000),
			OutputQPC: qpcAt(clock, 1_035_000)}, 0}, // after the whole frame: not reported
		{encoder.Frame{FrameID: 4, Slices: 2, SubmitQPC: qpcAt(clock, 1_048_000), FirstSliceQPC: qpcAt(clock, 1_047_000),
			OutputQPC: qpcAt(clock, 1_050_000)}, 0}, // before the submit
		{encoder.Frame{FrameID: 5, Reencoded: true, SubmitQPC: qpcAt(clock, 1_064_000), OutputQPC: qpcAt(clock, 1_066_000)}, 0},
	} {
		fr := c.fr
		fr.LTRSlot, fr.Dirty, fr.Data = -1, -1, h264P
		fr.CaptureQPC = fr.SubmitQPC
		f.Publish(&fr)
		got := nextEvent(t, v).Frame
		if got == nil || got.FirstSliceUs != c.first || got.Reencoded != fr.Reencoded {
			t.Fatalf("frame %d: %+v, want first slice %d reencoded %v", fr.FrameID, got, c.first, fr.Reencoded)
		}
	}
}

// TestHelperVideoPlainStart: a helper that refuses a start with the Phase 5
// encoder options (here slice output) is replaced by one started without
// them, which then streams.
func TestHelperVideoPlainStart(t *testing.T) {
	clock := testClock()
	fh := newFakeHelpers(t, fakeP5BCaps(`,"sliceOutput":true,"hwInstances":2,"instanceSelect":true`), func(f *encoder.Fake, m map[string]any) {
		if m["sliceOutput"] != nil {
			f.Send(encoder.HelperError{Code: "init_failed", Text: "OUTPUT_MODE SLICE: AMF_NOT_SUPPORTED", Re: "start"})
			return
		}
		f.Send(encoder.Started{Backend: "amf", Capture: "dda", Codec: "h264", Width: 1920, Height: 1080, FPS: 60, Kbps: 20000})
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: h264Key})
	})
	logs := &lineLog{}
	v := NewHelperVideo(HelperOptions{Launch: fh.launch, Clock: clock, SliceOutput: 4, EncoderInstance: "dedicated",
		Log: slog.New(slog.NewTextHandler(logs, nil))})
	defer v.Stop()
	if err := v.Start(helperParams(), false); err != nil {
		t.Fatal(err)
	}
	if m := expectMsg(t, fh.nextStarted(), "start"); m["sliceOutput"] != float64(4) || m["encoderInstance"] != float64(1) {
		t.Fatalf("first start %v", m)
	}
	if ev := nextEvent(t, v); ev.Err == nil || !ev.Restarted {
		t.Fatalf("refused start %+v", ev)
	}
	if m := expectMsg(t, fh.nextStarted(), "start"); m["sliceOutput"] != nil || m["encoderInstance"] != nil {
		t.Fatalf("retry %v: want it without the Phase 5 encoder options", m)
	}
	if c := nextEvent(t, v).Config; c == nil {
		t.Fatal("no config after the retry")
	}
	if l := logs.lines("refused a start with the Phase 5 encoder options"); len(l) != 1 {
		t.Fatalf("logged %q", l)
	}
}
