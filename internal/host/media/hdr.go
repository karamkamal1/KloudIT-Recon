package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// HDR10 on the FFmpeg path (GUIDE 3.9 / 4.5). Only the test pattern (capture
// "test") makes HDR10, with libsvtav1 (AV1 10-bit): a real 10-bit BT.2020 PQ
// stream with HDR metadata whose code values are known, for the browser
// client's HDR presentation and its tests. The Windows captures stay SDR on
// this path (docs/VENDOR_NOTES.md "3.9/4.5 HDR end to end"): FFmpeg 8.1's
// ddagrab has true HDR content only as FP16 scRGB (its 10-bit X2BGR10 output
// is DWM's SDR conversion, tagged sRGB: "According to MSDN, all integer
// formats contain sRGB image data"); NVENC takes no FP16 input, scale_d3d11
// converts to P010 without colour spaces (no PQ), and amfenc passes neither
// the input transfer of an RGBAF16 surface (only YUV inputs get the colour
// properties) nor HDR metadata (it reads mastering display side data, which
// ddagrab does not attach). HDR on Windows hosts is the native helper's
// (scRGB -> PQ P010 in its own tested shader, step 3.9).

// HDRTestEncoder is the encoder of the HDR10 test pattern.
const HDRTestEncoder = "libsvtav1"

// HDRTestWhite is the luminance (cd/m2) the test pattern's SDR content is
// shown at: the HDR reference white of ITU-R BT.2408, as the native helper
// maps an SDR desktop into an HDR10 stream.
const HDRTestWhite = 203

// HDRTestStrip is the height of the strip at the top of the HDR test pattern
// that holds the frame barcode and HDRTestPatches (the barcode's rows).
const HDRTestStrip = proto.BarcodeRows * proto.BarcodeCell

// HDRTestPatch is a patch of the HDR test pattern: the strip's rows from x,
// w pixels wide, in the colour drawbox makes of RGB (BT.601 limited range,
// libavutil/colorspace.h RGB_TO_*_CCIR) in 8 bits, converted to 10 bits by
// x 4 (the probe checks that this FFmpeg build converts exactly so). Y, Cb
// and Cr are the 10-bit codes the stream carries, which the client reads as
// BT.2020 PQ (Y 64 = 0, 508 ~ 100, 572 ~ 200, 724 ~ 1000, 856 ~ 4000, 940 =
// 10000 cd/m2).
type HDRTestPatch struct {
	Name      string
	X, W      int
	RGB       uint32
	Y, Cb, Cr int
}

// hdrTestPatchX is where the patches start (right of the barcode's 128 px).
const hdrTestPatchX = 160

// HDRTestPatches are the HDR test pattern's patches, 32 px each from x 160,
// those that fit in the picture's width (all of them from 448 px).
var HDRTestPatches = func() []HDRTestPatch {
	cols := []struct {
		name string
		rgb  uint32
	}{
		{"black", 0x000000}, {"100 cd/m2", 0x818181}, {"200 cd/m2", 0x949494}, {"1000 cd/m2", 0xC0C0C0},
		{"4000 cd/m2", 0xE7E7E7}, {"10000 cd/m2", 0xFFFFFF}, {"red", 0xC00000}, {"green", 0x00C000}, {"blue", 0x0000C0},
	}
	out := make([]HDRTestPatch, len(cols))
	for i, c := range cols {
		y, u, v := ccirYUV(c.rgb)
		out[i] = HDRTestPatch{Name: c.name, X: hdrTestPatchX + 32*i, W: 32, RGB: c.rgb, Y: 4 * y, Cb: 4 * u, Cr: 4 * v}
	}
	return out
}()

// ccirYUV converts an RGB colour as drawbox does (libavutil/colorspace.h
// RGB_TO_Y_CCIR, RGB_TO_U_CCIR, RGB_TO_V_CCIR with shift 0).
func ccirYUV(rgb uint32) (y, u, v int) {
	r, g, b := int(rgb>>16&0xff), int(rgb>>8&0xff), int(rgb&0xff)
	const scale, half = 10, 1 << 9
	fix := func(x float64) int { return int(x*(1<<scale) + 0.5) }
	y = (fix(0.29900*219.0/255.0)*r + fix(0.58700*219.0/255.0)*g + fix(0.11400*219.0/255.0)*b + (half + 16<<scale)) >> scale
	u = ((-fix(0.16874*224.0/255.0)*r - fix(0.33126*224.0/255.0)*g + fix(0.50000*224.0/255.0)*b + half - 1) >> scale) + 128
	v = ((fix(0.50000*224.0/255.0)*r - fix(0.41869*224.0/255.0)*g - fix(0.08131*224.0/255.0)*b + half - 1) >> scale) + 128
	return y, u, v
}

// HDRTestMetadata is the HDR10 metadata of the test pattern: a BT.2020 / D65
// mastering display spanning PQ's range (synthetic content, not mastered),
// MaxCLL its brightest patch (10000 cd/m2), MaxFALL an upper bound of its
// frame average (SDR content at the reference white, small patches).
var HDRTestMetadata = proto.HDRMetadata{
	DisplayPrimaries: [3][2]float64{{0.708, 0.292}, {0.170, 0.797}, {0.131, 0.046}},
	WhitePoint:       [2]float64{0.3127, 0.3290},
	MaxLuminance:     10000, MinLuminance: 0.0001, MaxCLL: 10000, MaxFALL: HDRTestWhite,
}

// HDRTestGraph returns the filter graph of the HDR10 test pattern after src
// (the w x h source and its pacing / capture clock, one chain): the source as
// SDR content at HDRTestWhite (zscale: BT.709 -> BT.2020 primaries, SMPTE ST
// 2084, BT.2020 NCL matrix, limited range, 10 bits; npl: SDR 1.0 =
// HDRTestWhite cd/m2) and, overlaid on its top HDRTestStrip rows, a strip
// drawn in 8 bits and converted to 10 (codes x 4: black 64, white 940, as the
// native helper's P010 barcode): black, the frame barcode (barcode, of the
// frame index like BarcodeFilter) and HDRTestPatches. Both branches carry the
// same frames (split), so overlay pairs them by their pts, whatever clock the
// source stamps. Output: yuv420p10le.
func HDRTestGraph(src string, w, h int, barcode bool) string {
	strip := []string{fmt.Sprintf("crop=w=iw:h=%d:x=0:y=0", HDRTestStrip), "drawbox=x=0:y=0:w=iw:h=ih:color=black:t=fill"}
	if barcode && w >= proto.BarcodeCols*proto.BarcodeCell {
		strip = append(strip, BarcodeFilter(proto.BarcodeCell))
	}
	for _, p := range HDRTestPatches {
		if p.X+p.W <= w {
			strip = append(strip, fmt.Sprintf("drawbox=x=%d:y=0:w=%d:h=ih:color=0x%06X:t=fill", p.X, p.W, p.RGB))
		}
	}
	return src + ",split[hdrm][hdrs];[hdrs]" + strings.Join(strip, ",") + ",format=yuv420p10le[hdrstrip];" +
		fmt.Sprintf("[hdrm]format=gbrp,zscale=tin=bt709:pin=bt709:min=gbr:rin=full:t=smpte2084:p=bt2020:m=2020_ncl:r=tv:npl=%d,format=yuv420p10le[hdrmain];", HDRTestWhite) +
		"[hdrmain][hdrstrip]overlay=x=0:y=0:format=yuv420p10"
}

// hdrTestFilters reports whether an FFmpeg build has the filters
// HDRTestGraph uses.
func hdrTestFilters(f map[string]bool) bool {
	for _, n := range []string{"testsrc2", "split", "crop", "drawbox", "zscale", "overlay"} {
		if !f[n] {
			return false
		}
	}
	return true
}

// HDRColorArgs are the encoder's colour description of an HDR10 stream:
// BT.2020 primaries, SMPTE ST 2084, BT.2020 NCL, limited range (the VUI /
// AV1 colour config).
var HDRColorArgs = []string{"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv"}

// svtav1HDRParams are SVT-AV1's HDR10 metadata parameters (svtav1-params) of
// m: the mastering display (chromaticity G, B, R, white point; luminance max,
// min) and the content light level, written as metadata OBUs.
func svtav1HDRParams(m proto.HDRMetadata) string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	xy := func(p [2]float64) string { return "(" + f(p[0]) + "," + f(p[1]) + ")" }
	return fmt.Sprintf("mastering-display=G%sB%sR%sWP%sL(%s,%s):content-light=%d,%d", xy(m.DisplayPrimaries[1]), xy(m.DisplayPrimaries[2]),
		xy(m.DisplayPrimaries[0]), xy(m.WhitePoint), f(m.MaxLuminance), f(m.MinLuminance), m.MaxCLL, m.MaxFALL)
}

// CanHDRTest reports whether this FFmpeg build ran the HDR10 test pattern in
// the probe (HDRTestGraph's codes exact, libsvtav1 encoding it at 10 bits).
func (c *Caps) CanHDRTest() bool { return c.hdrTest }

// SetHDRTest records the HDR10 test pattern's probe result (the probe does;
// tests).
func (c *Caps) SetHDRTest(ok bool) { c.hdrTest = ok }

// hdrTestSize is the picture the probe runs the HDR10 test pattern at: every
// patch fits.
const hdrTestW, hdrTestH = 480, 270

// testHDRTest runs HDRTestGraph over three frames into raw 10-bit video and
// checks the strip: the barcode reads the frame index (codes 64 / 940) and
// every patch has its codes; then encodes two frames with libsvtav1 as
// BuildArgs does for an HDR generation and checks the AV1 sequence header
// says 10 bits.
func testHDRTest(ctx context.Context, ffmpeg string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const w, h, frames = hdrTestW, hdrTestH, 3
	graph := HDRTestGraph(fmt.Sprintf("testsrc2=s=%dx%d:r=30", w, h), w, h, true) + "[v]"
	var stdout, stderr bytes.Buffer
	cmd := quietCmd(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-filter_complex", graph, "-map", "[v]",
		"-frames:v", strconv.Itoa(frames), "-f", "rawvideo", "-pix_fmt", "yuv420p10le", "pipe:1")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, causeLines(stderr.String(), 3))
	}
	if err := checkHDRTestFrames(stdout.Bytes(), w, h, frames); err != nil {
		return err
	}
	p := Params{Source: Source{Backend: "test", NativeW: w, NativeH: h}, Encoder: EncoderInfo{Name: HDRTestEncoder, Family: "av1", Vendor: "software"},
		FPS: 30, BitrateKbps: 2000, HDR: true}
	c := &Caps{options: map[string]map[string]bool{}, optValues: map[string]map[string]map[string]bool{}}
	c.options[HDRTestEncoder], c.optValues[HDRTestEncoder] = encoderOptions(ctx, ffmpeg, HDRTestEncoder)
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-filter_complex", graph, "-map", "[v]", "-frames:v", "2",
		"-c:v", HDRTestEncoder}, c.encoderArgs(p, 200, 1000)...)
	sh, err := av1Header(ctx, ffmpeg, append(args, HDRColorArgs...))
	if err != nil {
		return err
	}
	if sh.BitDepth != 10 {
		return fmt.Errorf("%s encoded the 10-bit test pattern at %d bits", HDRTestEncoder, sh.BitDepth)
	}
	return nil
}

// checkHDRTestFrames checks raw yuv420p10le frames of HDRTestGraph: frame i's
// barcode reads i at codes 64 / 940 and every patch has its codes (the
// middle of each).
func checkHDRTestFrames(b []byte, w, h, frames int) error {
	size := w * h * 3 // 16-bit samples, 4:2:0
	if len(b) != size*frames {
		return fmt.Errorf("got %d bytes of raw video, want %d", len(b), size*frames)
	}
	at := func(f []byte, off int) int { return int(f[off]) | int(f[off+1])<<8 }
	for i := 0; i < frames; i++ {
		f := b[i*size : (i+1)*size]
		luma := make([]byte, w*h)
		for k := range luma {
			code := at(f, 2*k)
			if code != 64 && code != 940 && k%w < proto.BarcodeCols*proto.BarcodeCell && k/w < HDRTestStrip {
				return fmt.Errorf("frame %d: barcode code %d at (%d,%d), want 64 or 940", i, code, k%w, k/w)
			}
			luma[k] = byte(code >> 2)
		}
		if v, ok := proto.BarcodeReadLuma(luma, w, proto.BarcodeCell); !ok || v != uint16(i) {
			return fmt.Errorf("frame %d: barcode reads %d (valid %v)", i, v, ok)
		}
		cw := w / 2
		for _, p := range HDRTestPatches {
			x, y := p.X+p.W/2, HDRTestStrip/2
			cb, cr := at(f, 2*w*h+2*(y/2*cw+x/2)), at(f, 2*w*h+w*h/2+2*(y/2*cw+x/2))
			if got := at(f, 2*(y*w+x)); got != p.Y || cb != p.Cb || cr != p.Cr {
				return fmt.Errorf("frame %d: patch %s codes %d/%d/%d, want %d/%d/%d", i, p.Name, got, cb, cr, p.Y, p.Cb, p.Cr)
			}
		}
	}
	return nil
}

// errHDRSource: HDR10 on the FFmpeg path is the test pattern's only.
var errHDRSource = errors.New("HDR10 on the FFmpeg path needs the test source and " + HDRTestEncoder)

// HDRConfig fills a generation's VideoConfig HDR fields: HDR10 (10-bit
// BT.2020 PQ with metadata md) when hdr and the stream's codec string names a
// 10-bit profile, else none; note: why the generation is not HDR (asked: the
// session's Params.HDRNote).
func HDRConfig(c *proto.VideoConfig, hdr bool, md *proto.HDRMetadata, note string) {
	c.HDR, c.BitDepth, c.ColorSpace, c.HDRMetadata, c.HDRNote = false, 0, nil, nil, note
	if !hdr {
		return
	}
	cs := proto.HDR10ColorSpace
	c.HDR, c.BitDepth, c.ColorSpace, c.HDRMetadata, c.HDRNote = true, 10, &cs, md, ""
}
