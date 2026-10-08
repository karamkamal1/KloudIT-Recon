package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestHDRTestPatches: the patches' 10-bit codes are drawbox's BT.601 colour
// conversion x 4 (checked against an FFmpeg run in TestHDRTestStream and the
// probe), and the grey ones sit where BT.2020 PQ puts the luminance they are
// named after.
func TestHDRTestPatches(t *testing.T) {
	want := map[string][3]int{
		"black": {64, 512, 512}, "100 cd/m2": {508, 512, 512}, "200 cd/m2": {572, 512, 512}, "1000 cd/m2": {724, 512, 512},
		"4000 cd/m2": {856, 512, 512}, "10000 cd/m2": {940, 512, 512},
		"red": {260, 396, 848}, "green": {452, 288, 228}, "blue": {140, 848, 456},
	}
	pq := func(code int) float64 { // ST 2084 EOTF of a limited-range 10-bit luma code, cd/m2
		const m1, m2, c1, c2, c3 = 2610.0 / 16384, 2523.0 / 4096 * 128, 3424.0 / 4096, 2413.0 / 4096 * 32, 2392.0 / 4096 * 32
		e := math.Pow(float64(code-64)/876, 1/m2)
		return 10000 * math.Pow(math.Max(e-c1, 0)/(c2-c3*e), 1/m1)
	}
	for i, p := range HDRTestPatches {
		if w, ok := want[p.Name]; !ok || [3]int{p.Y, p.Cb, p.Cr} != w {
			t.Errorf("patch %s: codes %d/%d/%d, want %v", p.Name, p.Y, p.Cb, p.Cr, w)
		}
		if p.X != 160+32*i || p.W != 32 {
			t.Errorf("patch %s at x %d, %d wide", p.Name, p.X, p.W)
		}
		var nits float64
		if n, err := strconvAtoi(strings.TrimSuffix(p.Name, " cd/m2")); err == nil {
			nits = float64(n)
			if got := pq(p.Y); math.Abs(got-nits)/nits > 0.06 {
				t.Errorf("patch %s: code %d is %.1f cd/m2", p.Name, p.Y, got)
			}
		}
	}
}

func strconvAtoi(s string) (int, error) {
	var n int
	if s == "" {
		return 0, io.EOF
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.EOF
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// TestHDRBuildArgs: an HDR10 generation of the test source with libsvtav1
// runs HDRTestGraph (the barcode in its strip, 10 bits to the encoder) and
// gets the colour description and the HDR10 metadata; HDR anywhere else is an
// error, and an SDR generation is unchanged.
func TestHDRBuildArgs(t *testing.T) {
	caps := &Caps{Filters: map[string]bool{"realtime": true}, options: map[string]map[string]bool{"libsvtav1": {"svtav1-params": true, "preset": true}}}
	svt := EncoderInfo{"libsvtav1", "av1", "software", false, false}
	p := Params{Source: Source{Backend: "test", NativeW: 480, NativeH: 270}, Encoder: svt, FPS: 30, BitrateKbps: 4000, CaptureClock: true, Barcode: true,
		HDR: true, TestPad: 16}
	args, err := caps.BuildArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	graph := HDRTestGraph("testsrc2=s=480x270:r=30,realtime,"+CaptureClockFilter, 480, 270, true)
	for _, want := range []string{
		graph + ",pad=w=iw:h=ih+16:x=0:y=0:color=white,format=yuv420p10le[v]",
		"crop=w=iw:h=48:x=0:y=0,drawbox=x=0:y=0:w=iw:h=ih:color=black:t=fill," + BarcodeFilter(proto.BarcodeCell) + ",drawbox=x=160:y=0:w=32:h=ih:color=0x000000:t=fill",
		"zscale=tin=bt709:pin=bt709:min=gbr:rin=full:t=smpte2084:p=bt2020:m=2020_ncl:r=tv:npl=203",
		"overlay=x=0:y=0:format=yuv420p10",
		"-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -color_range tv",
		"-svtav1-params pred-struct=1:lookahead=0:scd=0:rc=2:mastering-display=G(0.17,0.797)B(0.131,0.046)R(0.708,0.292)WP(0.3127,0.329)L(10000,0.0001):content-light=10000,203",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("HDR args lack %q:\n%s", want, joined)
		}
	}
	// The patches that fit: a 400 px wide picture has 7 (160 + 7 x 32 = 384).
	if g := HDRTestGraph("src", 400, 270, false); strings.Count(g, "drawbox=x=") != 7+1 || strings.Contains(g, "enable=") {
		t.Errorf("400 px graph: %s", g)
	}
	sdr := p
	sdr.HDR = false
	args, _ = caps.BuildArgs(sdr)
	if j := strings.Join(args, " "); strings.Contains(j, "zscale") || strings.Contains(j, "bt2020") || strings.Contains(j, "mastering") ||
		!strings.Contains(j, "format=yuv420p[v]") {
		t.Errorf("SDR args changed: %s", j)
	}
	for _, bad := range []Params{
		{Source: Source{Backend: "ddagrab"}, Encoder: EncoderInfo{"hevc_amf", "hevc", "amd", true, false}, FPS: 60, HDR: true},
		{Source: Source{Backend: "test"}, Encoder: EncoderInfo{"libx264", "h264", "software", false, false}, FPS: 60, HDR: true},
	} {
		if _, err := (&Caps{Filters: map[string]bool{"ddagrab": true}}).BuildArgs(bad); err != errHDRSource {
			t.Errorf("HDR with %s/%s: %v", bad.Source.Backend, bad.Encoder.Name, err)
		}
	}
}

// TestHDRTestStream runs an HDR10 generation of the test pattern through the
// Video manager and decodes it again: the probe passed it, the video config
// announces HDR10 (10-bit codec string, BT.2020 PQ, the metadata), the AV1
// sequence header carries the colour description, the key frame the HDR10
// metadata OBUs, and the decoded 10-bit frames their barcode (seq at codes
// 64 / 940) and the patches' codes.
func TestHDRTestStream(t *testing.T) {
	caps := probeOrSkip(t)
	svt, ok := EncoderInfo{}, false
	for _, e := range caps.Encoders {
		if e.Name == HDRTestEncoder {
			svt, ok = e, true
		}
	}
	if !ok || !hdrTestFilters(caps.Filters) {
		t.Skip("no libsvtav1 or zscale here")
	}
	if !caps.CanHDRTest() {
		t.Fatalf("probe rejected the HDR test pattern: %v", testHDRTest(context.Background(), caps.FFmpeg))
	}
	const w, h, n = 480, 270, 20
	v := NewVideo(caps, nil, NewClock())
	defer v.Stop()
	p := Params{Source: Source{Backend: "test", NativeW: w, NativeH: h}, Encoder: svt, FPS: 30, BitrateKbps: 3000,
		CaptureClock: caps.CanStampCapture(), Barcode: true, HDR: true, TestPad: 8}
	if err := v.Start(p, false); err != nil {
		t.Fatal(err)
	}
	var cfg *proto.VideoConfig
	var frames []*Frame
	deadline := time.After(30 * time.Second)
	for len(frames) < n {
		select {
		case ev := <-v.Events():
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			if ev.Config != nil {
				cfg = ev.Config
			}
			if ev.Frame != nil {
				frames = append(frames, ev.Frame)
			}
		case <-deadline:
			t.Fatalf("timeout after %d frames", len(frames))
		}
	}
	if cfg == nil || !cfg.HDR || cfg.BitDepth != 10 || cfg.ColorSpace == nil || *cfg.ColorSpace != proto.HDR10ColorSpace ||
		cfg.HDRMetadata == nil || *cfg.HDRMetadata != HDRTestMetadata || !codec.TenBit(cfg.Codec) || cfg.HDRNote != "" ||
		cfg.Height != h || cfg.CropBottom != 8 {
		t.Fatalf("video config %+v", cfg)
	}
	sh, ok := codec.FindAV1SequenceHeader(frames[0].Data)
	if !ok || sh.BitDepth != 10 || sh.ColorPrimaries != 9 || sh.TransferCharacteristics != 16 || sh.MatrixCoefficients != 9 || sh.FullRange {
		t.Fatalf("sequence header %+v", sh)
	}
	if md := codec.AV1MetadataTypes(frames[0].Data); !slices.Contains(md, codec.AV1MetadataHDRCLL) || !slices.Contains(md, codec.AV1MetadataHDRMDCV) {
		t.Fatalf("key frame metadata OBUs %v, want HDR CLL and MDCV", md)
	}
	pics := decode10(t, caps.FFmpeg, frames, w, h+8)
	if len(pics) != n {
		t.Fatalf("decoded %d of %d frames", len(pics), n)
	}
	cw := w / 2
	for i, f := range pics {
		luma := make([]byte, w*h)
		for k := range luma {
			luma[k] = byte(f[k] >> 2)
		}
		if v, ok := proto.BarcodeReadLuma(luma, w, proto.BarcodeCell); !ok || v != uint16(frames[i].Seq) {
			t.Fatalf("frame %d: barcode %d (valid %v), want %d", i, v, ok, frames[i].Seq)
		}
		if f[2*w+2] != 64 || f[2*w+20] != 940 && f[2*w+20] != 64 {
			t.Fatalf("frame %d: barcode codes %d / %d", i, f[2*w+2], f[2*w+20])
		}
		for _, p := range HDRTestPatches {
			x, y := p.X+p.W/2, HDRTestStrip/2
			cb, cr := f[w*(h+8)+y/2*cw+x/2], f[w*(h+8)+w*(h+8)/4+y/2*cw+x/2]
			// Flat 32x48 patches survive the encoder within a few codes
			// (saturated colours at 3 Mbit/s: up to 5).
			if d := max(abs(int(f[y*w+x])-p.Y), abs(int(cb)-p.Cb), abs(int(cr)-p.Cr)); d > 8 {
				t.Fatalf("frame %d: patch %s decodes as %d/%d/%d, want %d/%d/%d", i, p.Name, f[y*w+x], cb, cr, p.Y, p.Cb, p.Cr)
			}
		}
	}
	t.Logf("%d frames, codec %s, barcode and %d patches checked", len(pics), cfg.Codec, len(HDRTestPatches))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// decode10 decodes AV1 frames (IVF) with FFmpeg into 10-bit 4:2:0 pictures
// (Y, then Cb, then Cr; one uint16 per sample).
func decode10(t *testing.T, ffmpeg string, frames []*Frame, w, h int) [][]uint16 {
	t.Helper()
	var in bytes.Buffer
	hdr := make([]byte, 32)
	copy(hdr, "DKIF")
	binary.LittleEndian.PutUint16(hdr[6:], 32)
	copy(hdr[8:], "AV01")
	binary.LittleEndian.PutUint16(hdr[12:], uint16(w))
	binary.LittleEndian.PutUint16(hdr[14:], uint16(h))
	binary.LittleEndian.PutUint32(hdr[16:], 30)
	binary.LittleEndian.PutUint32(hdr[20:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(len(frames)))
	in.Write(hdr)
	for i, f := range frames {
		var fh [12]byte
		binary.LittleEndian.PutUint32(fh[:], uint32(len(f.Data)))
		binary.LittleEndian.PutUint64(fh[4:], uint64(i))
		in.Write(fh[:])
		in.Write(f.Data)
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "ivf", "-i", "pipe:0", "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "yuv420p10le", "pipe:1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = &in, &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("decoding: %v: %s", err, stderr.String())
	}
	var pics [][]uint16
	for {
		b := make([]byte, w*h*3)
		if _, err := io.ReadFull(&out, b); err != nil {
			break
		}
		p := make([]uint16, len(b)/2)
		for i := range p {
			p[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		pics = append(pics, p)
	}
	return pics
}
