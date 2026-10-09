package host

import "testing"

// Phase 5 session wiring, part B (encoder options): the host config's
// encoderInstance, reencodeOversized and sliceOutput reach the helper's start
// where the encoder's caps allow them (vendor-neutral: an AMF-like and an
// NVENC-like encoder's caps), the engine decision is logged once, and the
// frames the encoder re-encoded are counted for the stream stats.

func TestSessionEncoderOptions(t *testing.T) {
	const (
		// Engines the start may pick, slice output, no re-encode (AMF-like).
		pickEngines = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",` +
			`"hwInstances":2,"instanceSelect":true,"sliceOutput":true,"alignW":1,"alignH":1}`
		// Engines the encoder spreads its work over, re-encode (NVENC-like).
		spreadEngines = `"h264":{"maxW":4096,"maxH":2304,"forceIdr":true,"recovery":"invalidate","liveBitrate":"seamless","liveFps":"seamless",` +
			`"hwInstances":3,"reencode":true,"alignW":1,"alignH":1}`
	)
	all := Config{EncoderInstance: "dedicated", ReencodeOversized: 3, SliceOutput: 4}
	for _, c := range []struct {
		name                     string
		codec                    string
		cfg                      Config
		engine, reencode, slices any // the start's fields (nil: absent)
	}{
		{"defaults", pickEngines, Config{}, nil, nil, nil},
		{"all, engines picked by the start", pickEngines, all, float64(1), nil, float64(4)},
		{"all, engines spread by the encoder", spreadEngines, all, nil, float64(3), nil},
		{"engine number", pickEngines, Config{EncoderInstance: "0"}, float64(0), nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newP5Rig(t, c.codec, 4, c.cfg, "seamless")
			if r.start["encoderInstance"] != c.engine || r.start["reencodeOversized"] != c.reencode || r.start["sliceOutput"] != c.slices {
				t.Fatalf("start %v: want encoderInstance %v reencodeOversized %v sliceOutput %v", r.start, c.engine, c.reencode, c.slices)
			}
			if l := r.logs.lines(`encoder engine`); len(l) != 1 {
				t.Fatalf("engine decision logged %d times", len(l))
			}
			r.publish(t, svcFrame(1))
			fr := svcFrame(2)
			fr.Reencoded = true
			r.publish(t, fr)
			r.publish(t, svcFrame(3))
			if n := r.s.stats.reencoded.Load(); n != 1 {
				t.Fatalf("%d frames counted as re-encoded, want 1", n)
			}
		})
	}
}
