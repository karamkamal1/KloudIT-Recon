package proto

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// crc8 is a plain bitwise CRC-8/I-432-1 over arbitrary bytes, to pin
// BarcodeCRC to the published parameters (check value 0xA1).
func crc8(b []byte) uint8 {
	crc := uint8(0)
	for _, x := range b {
		crc ^= x
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

func TestBarcode(t *testing.T) {
	if c := crc8([]byte("123456789")); c != 0xa1 {
		t.Fatalf("CRC-8/I-432-1 check value %#x, want 0xa1", c)
	}
	for v := 0; v < 1<<16; v++ {
		if BarcodeCRC(uint16(v)) != crc8([]byte{byte(v >> 8), byte(v)}) {
			t.Fatalf("BarcodeCRC(%d) differs from CRC-8/I-432-1", v)
		}
		w := BarcodeWord(uint16(v))
		if got, ok := BarcodeDecode(w); !ok || got != uint16(v) {
			t.Fatalf("round trip %d: %d %v", v, got, ok)
		}
		// Every single and double cell error is detected.
		for i := 0; i < BarcodeBits; i++ {
			if _, ok := BarcodeDecode(w ^ 1<<i); ok {
				t.Fatalf("value %d: flipped bit %d accepted", v, i)
			}
			for j := i + 1; j < BarcodeBits; j++ {
				if _, ok := BarcodeDecode(w ^ 1<<i ^ 1<<j); ok {
					t.Fatalf("value %d: flipped bits %d, %d accepted", v, i, j)
				}
			}
		}
	}
	// A uniform corner (black, white) is never a barcode.
	for _, w := range []uint32{0, 1<<BarcodeBits - 1} {
		if _, ok := BarcodeDecode(w); ok {
			t.Fatalf("uniform word %#x accepted", w)
		}
	}
	if _, ok := BarcodeDecode(1 << BarcodeBits); ok {
		t.Fatal("word wider than 24 bits accepted")
	}

	luma := cellLuma(0xbeef, 16, 235)
	if v, ok := BarcodeDecodeLuma(luma); !ok || v != 0xbeef {
		t.Fatalf("limited-range luma: %#x %v", v, ok)
	}
	luma[5] = 128 // neither dark nor light
	if _, ok := BarcodeDecodeLuma(luma); ok {
		t.Fatal("undecided cell accepted")
	}
	if _, ok := BarcodeDecodeLuma(make([]float64, BarcodeBits)); ok {
		t.Fatal("all-black corner accepted")
	}

	// A drawn barcode in a luma plane with a fractional cell size (test page
	// scaled to 1280 wide: 13.3 px) and soft edges (box blur).
	for _, cell := range []float64{16, 1280.0 / BarcodeWallclockCells, 20} {
		const stride = 200
		plane := drawLuma(0x1234, cell, stride, 80)
		if v, ok := BarcodeReadLuma(plane, stride, cell); !ok || v != 0x1234 {
			t.Fatalf("cell %.1f: read %#x %v", cell, v, ok)
		}
	}
	if _, ok := BarcodeReadLuma(make([]byte, 64*64), 64, 16); ok {
		t.Fatal("plane smaller than the barcode accepted")
	}
}

// cellLuma returns per-cell luma for value v with the given black/white levels.
func cellLuma(v uint16, black, white float64) []float64 {
	w := BarcodeWord(v)
	l := make([]float64, BarcodeBits)
	for k := range l {
		l[k] = black
		if w>>BarcodeCellBit(k)&1 == 1 {
			l[k] = white
		}
	}
	return l
}

// drawLuma renders value v like the test page (cell edges rounded) on a grey
// background, then blurs it with a 3x3 box filter.
func drawLuma(v uint16, cell float64, stride, rows int) []byte {
	img := make([]byte, stride*rows)
	for i := range img {
		img[i] = 128
	}
	w := BarcodeWord(v)
	for k := 0; k < BarcodeBits; k++ {
		c, r := k%BarcodeCols, k/BarcodeCols
		x0, x1 := int(float64(c)*cell+0.5), int(float64(c+1)*cell+0.5)
		y0, y1 := int(float64(r)*cell+0.5), int(float64(r+1)*cell+0.5)
		val := byte(16)
		if w>>BarcodeCellBit(k)&1 == 1 {
			val = 235
		}
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img[y*stride+x] = val
			}
		}
	}
	out := make([]byte, len(img))
	for y := 0; y < rows; y++ {
		for x := 0; x < stride; x++ {
			sum, n := 0, 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if xx, yy := x+dx, y+dy; xx >= 0 && yy >= 0 && xx < stride && yy < rows {
						sum += int(img[yy*stride+xx])
						n++
					}
				}
			}
			out[y*stride+x] = byte(sum / n)
		}
	}
	return out
}

// TestBarcodeJS checks that web/static/js/protocol.js and the copy of the
// encoder in tools/latency-test/index.html draw the same words as Go for all
// 65536 values, and that protocol.js decodes and samples cells identically.
func TestBarcodeJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	page, err := os.ReadFile(filepath.Join(root, "tools", "latency-test", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)// BEGIN barcode format[^\n]*\n(.*?)// END barcode format`).FindSubmatch(page)
	if m == nil {
		t.Fatal("tools/latency-test/index.html: barcode format block not found")
	}
	vectors := [][]float64{cellLuma(0xbeef, 16, 235), cellLuma(7, 0, 255), make([]float64, BarcodeBits), cellLuma(1, 95.9, 160.1), cellLuma(1, 96, 235), cellLuma(1, 0, 160)}
	in, _ := json.Marshal(map[string]any{"page": string(m[1]), "luma": vectors})
	script := `
const P = await import(process.argv[1]);
const input = JSON.parse(process.argv[2]);
const page = new Function(input.page + '; return { barcodeWord, BARCODE_COLS, BARCODE_ROWS, BARCODE_WALLCLOCK_CELLS };')();
const words = [];
let pageDiff = -1;
for (let v = 0; v < 65536; v++) {
  words.push(P.barcodeWord(v));
  if (pageDiff < 0 && page.barcodeWord(v) !== P.barcodeWord(v)) pageDiff = v;
}
const rects = [];
for (const cell of [16, 1280 / 96, 20, 3840 / 96]) for (let k = 0; k < P.BARCODE_BITS; k++) rects.push(P.barcodeSampleRect(k, cell));
console.log(JSON.stringify({
  words, pageDiff, rects,
  decoded: input.luma.map((l) => P.barcodeDecodeLuma(l)),
  consts: [P.BARCODE_COLS, P.BARCODE_ROWS, P.BARCODE_BITS, P.BARCODE_CELL, P.BARCODE_WALLCLOCK_CELLS, P.BARCODE_DARK, P.BARCODE_LIGHT, P.FEATURE_BARCODE_SEQ],
  pageConsts: [page.BARCODE_COLS, page.BARCODE_ROWS, page.BARCODE_WALLCLOCK_CELLS],
}));`
	js := filepath.Join(root, "web", "static", "js", "protocol.js")
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), string(in)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Words      []uint32
		PageDiff   int
		Rects      [][4]int
		Decoded    []*uint16
		Consts     []any
		PageConsts []int
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Words) != 1<<16 {
		t.Fatalf("%d words", len(got.Words))
	}
	for v, w := range got.Words {
		if w != BarcodeWord(uint16(v)) {
			t.Fatalf("protocol.js barcodeWord(%d) = %#x, Go %#x", v, w, BarcodeWord(uint16(v)))
		}
	}
	if got.PageDiff >= 0 {
		t.Fatalf("latency test page barcodeWord(%d) differs from protocol.js", got.PageDiff)
	}
	want := []any{float64(BarcodeCols), float64(BarcodeRows), float64(BarcodeBits), float64(BarcodeCell),
		float64(BarcodeWallclockCells), float64(BarcodeDark), float64(BarcodeLight), FeatureBarcodeSeq}
	for i := range want {
		if got.Consts[i] != want[i] {
			t.Fatalf("protocol.js constant %d = %v, Go %v", i, got.Consts[i], want[i])
		}
	}
	if got.PageConsts[0] != BarcodeCols || got.PageConsts[1] != BarcodeRows || got.PageConsts[2] != BarcodeWallclockCells {
		t.Fatalf("latency test page constants %v", got.PageConsts)
	}
	i := 0
	for _, cell := range []float64{16, 1280.0 / 96, 20, 3840.0 / 96} {
		for k := 0; k < BarcodeBits; k++ {
			x0, y0, x1, y1 := BarcodeSampleRect(k, cell)
			if got.Rects[i] != [4]int{x0, y0, x1, y1} {
				t.Fatalf("barcodeSampleRect(%d, %.2f) = %v, Go %v", k, cell, got.Rects[i], [4]int{x0, y0, x1, y1})
			}
			i++
		}
	}
	for i, l := range vectors {
		v, ok := BarcodeDecodeLuma(l)
		js := got.Decoded[i]
		if ok != (js != nil) || (ok && *js != v) {
			t.Fatalf("luma vector %d: Go %d/%v, protocol.js %v", i, v, ok, js)
		}
	}
}
