//go:build windows

package platform

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetCursorInfo = user32.NewProc("GetCursorInfo")
	procGetIconInfo   = user32.NewProc("GetIconInfo")
	procGetDC         = user32.NewProc("GetDC")
	procReleaseDC     = user32.NewProc("ReleaseDC")
	gdi32             = windows.NewLazySystemDLL("gdi32.dll")
	procGetObjectW    = gdi32.NewProc("GetObjectW")
	procGetDIBits     = gdi32.NewProc("GetDIBits")
	procDeleteObject  = gdi32.NewProc("DeleteObject")
)

type cursorInfo struct {
	cbSize  uint32
	flags   uint32
	hCursor uintptr
	x, y    int32
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmap struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// GetCursor returns the current cursor handle, position and visibility.
func GetCursor() (CursorState, error) {
	var ci cursorInfo
	ci.cbSize = uint32(unsafe.Sizeof(ci))
	if r, _, err := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return CursorState{}, err
	}
	return CursorState{Visible: ci.flags&1 != 0 && ci.hCursor != 0, X: int(ci.x), Y: int(ci.y), Handle: uint64(ci.hCursor)}, nil
}

// readBitmap returns 32-bit top-down BGRA pixels of an HBITMAP.
func readBitmap(hbm uintptr) (w, h int, px []byte, err error) {
	var bm bitmap
	if r, _, _ := procGetObjectW.Call(hbm, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return 0, 0, nil, errors.New("GetObject failed")
	}
	w, h = int(bm.bmWidth), int(bm.bmHeight)
	if w <= 0 || h <= 0 || w > 512 || h > 1024 {
		return 0, 0, nil, errors.New("unexpected cursor size")
	}
	// BITMAPINFO with room for a colour table.
	buf := make([]byte, unsafe.Sizeof(bitmapInfoHeader{})+256*4)
	bih := (*bitmapInfoHeader)(unsafe.Pointer(&buf[0]))
	bih.biSize = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bih.biWidth = int32(w)
	bih.biHeight = -int32(h) // top-down
	bih.biPlanes = 1
	bih.biBitCount = 32
	px = make([]byte, w*h*4)
	hdc, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdc)
	if r, _, _ := procGetDIBits.Call(hdc, hbm, 0, uintptr(h), uintptr(unsafe.Pointer(&px[0])), uintptr(unsafe.Pointer(&buf[0])), 0); r == 0 {
		return 0, 0, nil, errors.New("GetDIBits failed")
	}
	return w, h, px, nil
}

// CursorImage converts a cursor handle into an RGBA image. Monochrome cursors
// (AND/XOR masks, e.g. the text I-beam) are rendered white with a dark outline
// because browsers cannot invert the pixels beneath a cursor.
func CursorImage(handle uint64) (*CursorShape, error) {
	var ii iconInfo
	if r, _, err := procGetIconInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil, err
	}
	if ii.hbmMask != 0 {
		defer procDeleteObject.Call(ii.hbmMask)
	}
	if ii.hbmColor != 0 {
		defer procDeleteObject.Call(ii.hbmColor)
	}
	shape := &CursorShape{ID: handle, HotX: int(ii.xHotspot), HotY: int(ii.yHotspot)}
	if ii.hbmColor != 0 {
		w, h, px, err := readBitmap(ii.hbmColor)
		if err != nil {
			return nil, err
		}
		hasAlpha := false
		for i := 3; i < len(px); i += 4 {
			if px[i] != 0 {
				hasAlpha = true
				break
			}
		}
		var mask []byte
		if !hasAlpha && ii.hbmMask != 0 {
			if mw, mh, m, err := readBitmap(ii.hbmMask); err == nil && mw == w && mh >= h {
				mask = m
			}
		}
		rgba := make([]byte, w*h*4)
		for i := 0; i < w*h; i++ {
			b, g, r, a := px[4*i], px[4*i+1], px[4*i+2], px[4*i+3]
			if !hasAlpha {
				a = 255
				if mask != nil && mask[4*i] != 0 { // AND bit set -> transparent
					a = 0
				}
			}
			rgba[4*i], rgba[4*i+1], rgba[4*i+2], rgba[4*i+3] = r, g, b, a
		}
		shape.W, shape.H, shape.RGBA = w, h, rgba
		return shape, nil
	}
	// Monochrome: mask is twice as tall (AND on top, XOR below).
	w, h2, m, err := readBitmap(ii.hbmMask)
	if err != nil {
		return nil, err
	}
	h := h2 / 2
	rgba := make([]byte, w*h*4)
	invert := make([]bool, w*h)
	for i := 0; i < w*h; i++ {
		and := m[4*i] != 0
		xor := m[4*(i+w*h)] != 0
		switch {
		case !and && !xor:
			rgba[4*i+3] = 255 // black
		case !and && xor:
			rgba[4*i], rgba[4*i+1], rgba[4*i+2], rgba[4*i+3] = 255, 255, 255, 255
		case and && xor: // inverted: draw white, outline later
			rgba[4*i], rgba[4*i+1], rgba[4*i+2], rgba[4*i+3] = 255, 255, 255, 255
			invert[i] = true
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if rgba[4*i+3] != 0 {
				continue
			}
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d[0], y+d[1]
				if nx >= 0 && ny >= 0 && nx < w && ny < h && invert[ny*w+nx] {
					rgba[4*i], rgba[4*i+1], rgba[4*i+2], rgba[4*i+3] = 0, 0, 0, 200
					break
				}
			}
		}
	}
	shape.W, shape.H, shape.RGBA = w, h, rgba
	return shape, nil
}
