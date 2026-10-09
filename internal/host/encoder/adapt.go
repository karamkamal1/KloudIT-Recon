package encoder

import (
	"fmt"
	"strconv"
	"strings"
)

// Droppable reports whether recon-host may leave the frame out under
// congestion without breaking the decoding of any other frame (GUIDE 5
// temporal SVC, StartParams.SVCLayers 2: "drop enhancement frames under
// congestion (halve fps), no corruption, no IDR"): it is discardable (no
// later frame references it) and neither a key frame nor a recovery frame
// (the client waits for that one after a loss). A frame left out on purpose is
// not a loss: send no Recover for it, and give it no sequence number, so the
// client sees no gap.
func (f *Frame) Droppable() bool { return f.Discardable && !f.Key && !f.Recovery }

// FPSSteps are the frame rates LowerFPS / RaiseFPS move between ("FPS before
// resolution", GUIDE 2.2 / 5: 120 -> 90 -> 60 before the resolution).
var FPSSteps = []int{240, 165, 144, 120, 100, 90, 75, 60, 50, 45, 30}

// LowerFPS returns the next step below fps that is at least floor (for
// SetFPS when the bitrate is at its floor), or fps when there is none.
func LowerFPS(fps, floor int) int {
	for _, s := range FPSSteps {
		if s < fps && s >= floor {
			return s
		}
	}
	return fps
}

// RaiseFPS returns the next step above fps, at most ceiling (the frame rate
// the session asked for, which need not be a step), or ceiling.
func RaiseFPS(fps, ceiling int) int {
	for i := len(FPSSteps) - 1; i >= 0; i-- {
		if s := FPSSteps[i]; s > fps && s < ceiling {
			return s
		}
	}
	return max(fps, ceiling)
}

// EncoderInstanceFor turns a configured engine choice into
// StartParams.EncoderInstance (GUIDE 5 "dedicated encode engine": AMF
// INSTANCE_INDEX away from the engine Adrenalin's recording uses, VERIFY):
//
//	"" | "default" | "auto"  nil: the backend's default (engine 0; host config
//	                         "encoderInstance" auto until the hardware check of
//	                         docs/VENDOR_NOTES.md says which engine Adrenalin uses)
//	"dedicated"              engine 1 where the codec has more than one engine and
//	                         the backend lets start pick it (CodecCaps.InstanceSelect),
//	                         else nil
//	"0", "1", ...            that engine; an error where it cannot be picked
func EncoderInstanceFor(choice string, cc CodecCaps) (*int, error) {
	switch c := strings.TrimSpace(choice); c {
	case "", "default", "auto":
		return nil, nil
	case "dedicated":
		if !cc.InstanceSelect || cc.HWInstances < 2 {
			return nil, nil
		}
		one := 1
		return &one, nil
	default:
		n, err := strconv.Atoi(c)
		if err != nil {
			return nil, fmt.Errorf("encoder instance %q: want auto, dedicated or an engine number", choice)
		}
		if !cc.InstanceSelect {
			return nil, fmt.Errorf("encoder instance %d: this encoder picks its engines itself", n)
		}
		if n < 0 || n >= max(1, cc.HWInstances) {
			return nil, fmt.Errorf("encoder instance %d: the GPU has %d engine(s) for this codec", n, max(1, cc.HWInstances))
		}
		return &n, nil
	}
}
