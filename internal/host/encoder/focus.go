package encoder

import "image"

// FocusOptions tunes FocusROI. Zero values take the defaults.
type FocusOptions struct {
	// CursorSize is the side of the square around the pointer, in source
	// pixels (default: an eighth of the source height, 135 at 1080p).
	CursorSize   int
	CursorWeight int // default 6
	// CenterSize is the side of the square around the picture's centre, where
	// games draw their crosshair (default: a sixth of the source height, 180
	// at 1080p); negative leaves the centre out (a desktop session).
	CenterSize   int
	CenterWeight int // default 8
	// Background is the weight of the rest of the picture: 0 (default) adds no
	// region; a negative one lets the encoder take bits from it for the
	// regions above (AMF importance below its default 5, NVENC a positive QP
	// offset), which keeps their gain when the rate control is tight.
	Background int
}

// FocusROI returns the regions of interest that keep the pointer and the
// crosshair sharp (GUIDE 5 "sharper crosshair / cursor"), for SetROI: the
// helper turns the weights into an AMF ROI_DATA importance map (64x64 blocks,
// H.264 16x16) or an NVENC QP delta map (16 / 32 / 64 blocks), the higher
// weight winning where regions overlap (docs/HELPER_PROTOCOL.md "setRoi").
//
// srcW x srcH is the picture the pointer position refers to (the capture,
// Started.CaptureWidth / CaptureHeight; desktop coordinates relative to the
// captured monitor), encW x encH the stream (Started.Width / Height): the
// regions are scaled to it, rounded outwards and clipped to the picture.
// cursor nil: no pointer region (hidden, or outside the captured monitor).
// The order is background, pointer, centre. Nil when there is nothing to
// emphasise: then call SetROI(nil).
//
// The helper's --self-test-encoder (native/recon-encoder/src/codec/selftest.cpp
// "ROI maps of the cursor / crosshair rects") checks the maps of exactly the
// rects TestFocusROI pins for 1920x1080.
func FocusROI(srcW, srcH, encW, encH int, cursor *image.Point, opt FocusOptions) []ROIRect {
	if encW <= 0 || encH <= 0 {
		return nil
	}
	if srcW <= 0 || srcH <= 0 {
		srcW, srcH = encW, encH
	}
	if opt.CursorSize == 0 {
		opt.CursorSize = max(16, srcH/8)
	}
	if opt.CursorWeight == 0 {
		opt.CursorWeight = 6
	}
	if opt.CenterSize == 0 {
		opt.CenterSize = max(16, srcH/6)
	}
	if opt.CenterWeight == 0 {
		opt.CenterWeight = 8
	}
	// A square of side size centred on (cx, cy) in source pixels, scaled to
	// the stream and clipped; ok false when nothing of it is inside.
	square := func(cx, cy, size, weight int) (ROIRect, bool) {
		x0, y0 := cx-size/2, cy-size/2
		return scaleRect(x0, y0, x0+size, y0+size, srcW, srcH, encW, encH, weight)
	}
	var out []ROIRect
	if opt.Background != 0 {
		out = append(out, ROIRect{X: 0, Y: 0, W: encW, H: encH, Weight: clampWeight(opt.Background)})
	}
	if cursor != nil && opt.CursorSize > 0 {
		if r, ok := square(cursor.X, cursor.Y, opt.CursorSize, opt.CursorWeight); ok {
			out = append(out, r)
		}
	}
	if opt.CenterSize > 0 {
		if r, ok := square(srcW/2, srcH/2, opt.CenterSize, opt.CenterWeight); ok {
			out = append(out, r)
		}
	}
	if len(out) == 1 && opt.Background != 0 {
		return nil // only the background: nothing stands out
	}
	return out
}

// scaleRect maps [x0, x1) x [y0, y1) from a srcW x srcH picture to encW x
// encH, rounding outwards, and clips it to the picture.
func scaleRect(x0, y0, x1, y1, srcW, srcH, encW, encH, weight int) (ROIRect, bool) {
	floorDiv := func(a, b int) int {
		q := a / b
		if a%b != 0 && a < 0 {
			q--
		}
		return q
	}
	ceilDiv := func(a, b int) int { return -floorDiv(-a, b) }
	sx0, sy0 := floorDiv(x0*encW, srcW), floorDiv(y0*encH, srcH)
	sx1, sy1 := ceilDiv(x1*encW, srcW), ceilDiv(y1*encH, srcH)
	sx0, sy0 = max(0, sx0), max(0, sy0)
	sx1, sy1 = min(encW, sx1), min(encH, sy1)
	if sx0 >= sx1 || sy0 >= sy1 {
		return ROIRect{}, false
	}
	return ROIRect{X: sx0, Y: sy0, W: sx1 - sx0, H: sy1 - sy0, Weight: clampWeight(weight)}, true
}

func clampWeight(w int) int { return min(10, max(-10, w)) }
