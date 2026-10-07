package proto

import "math"

// Frame barcode: an in-band frame counter drawn into the picture's top-left
// corner, read back by the client from decoded frames (latency probe).
//
// A 16-bit value and its CRC-8 form the 24-bit word value<<8 | crc. The word is
// drawn as an 8x3 grid of square black/white cells, row-major, most
// significant bit first: cell k shows bit 23-k (row 0 = value bits 15..8,
// row 1 = value bits 7..0, row 2 = the CRC). White = 1. The CRC rejects
// random picture content (and an all-black or all-white corner) and torn or
// half-updated barcodes.
//
// Two sources draw it:
//   - the test pattern (capture "test"): value = the encoder's frame index,
//     which is the frame's Seq within a generation (low 16 bits); cells are
//     BarcodeCell pixels of the encoded picture. Announced with
//     FeatureBarcodeSeq.
//   - tools/latency-test/index.html on the host's screen: value = the host's
//     wall clock in ms (low 16 bits); cells are 1/BarcodeWallclockCells of the
//     picture width, so scaling the capture does not change the geometry.
//
// web/static/js/protocol.js mirrors the format (the test page carries a copy of
// the encoder); TestBarcodeJS checks that all three agree.
const (
	BarcodeCols = 8
	BarcodeRows = 3
	BarcodeBits = BarcodeCols * BarcodeRows

	// BarcodeCell is the cell size of the test pattern's barcode in pixels.
	BarcodeCell = 16
	// BarcodeWallclockCells: the test page's cell size is the picture width
	// divided by this (20 px at 1920, 40 px at 3840 wide).
	BarcodeWallclockCells = 96

	// A cell's mean luma (0-255; full or limited range) below BarcodeDark
	// reads as 0, above BarcodeLight as 1; anything between means the corner
	// holds no barcode.
	BarcodeDark  = 96
	BarcodeLight = 160
)

// FeatureBarcodeSeq is the Welcome.Features entry announcing that every video
// frame carries the barcode of its Seq (test pattern source).
const FeatureBarcodeSeq = "barcode-seq"

// BarcodeCRC is CRC-8 (polynomial 0x07, init 0, no reflection, xorout 0x55;
// CRC-8/I-432-1) over the value's two bytes, high byte first. The xorout
// makes all-black (word 0) and all-white (0xffffff) invalid.
func BarcodeCRC(v uint16) uint8 {
	crc := uint8(0)
	for _, b := range [2]uint8{uint8(v >> 8), uint8(v)} {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc ^ 0x55
}

// BarcodeWord returns the 24 bits drawn for value v.
func BarcodeWord(v uint16) uint32 { return uint32(v)<<8 | uint32(BarcodeCRC(v)) }

// BarcodeCellBit returns which bit of the word cell k (row-major) shows.
func BarcodeCellBit(k int) int { return BarcodeBits - 1 - k }

// BarcodeDecode checks a word's CRC and returns its value.
func BarcodeDecode(word uint32) (uint16, bool) {
	if word>>BarcodeBits != 0 {
		return 0, false
	}
	v := uint16(word >> 8)
	return v, uint8(word) == BarcodeCRC(v)
}

// BarcodeDecodeLuma decodes the cells' mean luma (row-major, 0-255). It fails
// if any cell is neither clearly dark nor clearly light, or the CRC is wrong.
func BarcodeDecodeLuma(luma []float64) (uint16, bool) {
	if len(luma) != BarcodeBits {
		return 0, false
	}
	var word uint32
	for k, l := range luma {
		switch {
		case l < BarcodeDark:
		case l > BarcodeLight:
			word |= 1 << BarcodeCellBit(k)
		default:
			return 0, false
		}
	}
	return BarcodeDecode(word)
}

// BarcodeSampleRect returns the part of cell k (row-major) a reader averages:
// the inner half in each direction, away from the edges compression blurs.
// cell is the cell size in pixels; x1 and y1 are exclusive.
func BarcodeSampleRect(k int, cell float64) (x0, y0, x1, y1 int) {
	c, r := float64(k%BarcodeCols), float64(k/BarcodeCols)
	x0, x1 = int(math.Round((c+0.25)*cell)), int(math.Round((c+0.75)*cell))
	y0, y1 = int(math.Round((r+0.25)*cell)), int(math.Round((r+0.75)*cell))
	return x0, y0, max(x1, x0+1), max(y1, y0+1)
}

// BarcodeReadLuma decodes the barcode in the top-left corner of an 8-bit luma
// plane with stride bytes per row and cells of cell pixels.
func BarcodeReadLuma(plane []byte, stride int, cell float64) (uint16, bool) {
	var luma [BarcodeBits]float64
	for k := range luma {
		x0, y0, x1, y1 := BarcodeSampleRect(k, cell)
		if x1 > stride || y1*stride > len(plane) {
			return 0, false
		}
		sum := 0
		for y := y0; y < y1; y++ {
			for _, p := range plane[y*stride+x0 : y*stride+x1] {
				sum += int(p)
			}
		}
		luma[k] = float64(sum) / float64((x1-x0)*(y1-y0))
	}
	return BarcodeDecodeLuma(luma[:])
}
