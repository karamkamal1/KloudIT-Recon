package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/nut"
)

func TestSplitAnnexB(t *testing.T) {
	in := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5, 6}
	got := SplitAnnexB(in)
	want := [][]byte{{0x67, 1, 2}, {0x68, 3}, {0x65, 4, 5, 6}}
	if len(got) != len(want) {
		t.Fatalf("got %d nals", len(got))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("nal %d = %x want %x", i, got[i], want[i])
		}
	}
	if !bytes.Equal(JoinAnnexB(want...), []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5, 6}) {
		t.Fatal("JoinAnnexB")
	}
}

func TestUnescape(t *testing.T) {
	got := unescapeRBSP([]byte{1, 0, 0, 3, 1, 0, 0, 3, 0, 0, 3})
	if !bytes.Equal(got, []byte{1, 0, 0, 1, 0, 0, 0, 0}) {
		t.Fatalf("%x", got)
	}
}

func TestHEVCCodecStringKnown(t *testing.T) {
	// SPS from a Main profile, level 3.1 stream (x265 defaults).
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5d, 0xa0, 0x02, 0x80}
	s, err := HEVCCodecString(sps)
	if err != nil {
		t.Fatal(err)
	}
	if s != "hev1.1.6.L93.90" {
		t.Fatalf("got %s", s)
	}
}

func encodeNUT(t *testing.T, args ...string) []*nut.Packet {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(t.TempDir(), "x.nut")
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, "-f", "nut", out)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg: %v %s", err, b)
	}
	f, _ := os.Open(out)
	defer f.Close()
	d := nut.NewDemuxer(f, 64<<20)
	var pkts []*nut.Packet
	for {
		p, err := d.ReadPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, p)
	}
	streamsByTest[t.Name()] = d.Streams()
	return pkts
}

var streamsByTest = map[string][]*nut.Stream{}

// decodeCount decodes an elementary stream with ffmpeg and returns the number
// of frames and stderr (errors).
func decodeCount(t *testing.T, format string, data []byte) (int, string) {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error", "-f", format, "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames,codec_name,profile,level", "-of", "json", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v %s", err, stderr.String())
	}
	var r struct {
		Streams []struct {
			N string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	json.Unmarshal(out, &r)
	n := 0
	if len(r.Streams) > 0 {
		for _, c := range r.Streams[0].N {
			n = n*10 + int(c-'0')
		}
	}
	return n, stderr.String()
}

// checkParamSetsLen: the prefix ParamSetsLen marks in an encoder's key frame
// holds all its parameter sets and the rest none.
func checkParamSetsLen(t *testing.T, p *Params, key []byte) {
	t.Helper()
	n := ParamSetsLen(p.Family, key)
	if n <= 0 || n >= len(key) || !p.hasParamSets(key[:n]) || p.hasParamSets(key[n:]) {
		t.Fatalf("%s key frame of %d bytes: parameter-set prefix %d bytes", p.Family, len(key), n)
	}
}

// ParamSetsLen on hand-made frames: delimiters and SEI in front count, the
// prefix ends with the last parameter set before the coded picture, and a
// parameter set after it does not extend it.
func TestParamSetsLen(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	h264 := cat(sc, []byte{0x09, 0xf0}, sc, []byte{0x06, 5, 1, 0x80}, sc, []byte{0x67, 0x64, 0, 0x1f}, []byte{0, 0, 1}, []byte{0x68, 0xee}, sc, []byte{0x65, 0x88, 0x84}, sc, []byte{0x68, 0xee})
	hevc := cat(sc, []byte{0x46, 0x01, 0x50}, sc, []byte{0x40, 0x01, 0x0c}, sc, []byte{0x42, 0x01, 0x01}, sc, []byte{0x44, 0x01, 0xc1}, sc, []byte{0x4e, 0x01, 0x05}, sc, []byte{0x26, 0x01, 0xaf})
	obu := func(typ byte, payload ...byte) []byte {
		return append([]byte{typ<<3 | 0x02, byte(len(payload))}, payload...)
	}
	av1 := cat(obu(obuTemporalDelimiter), obu(obuSequenceHeader, 0, 0, 0), obu(5, 1), obu(6, 0x10, 0x20, 0x30))
	for _, tc := range []struct {
		name, family string
		data         []byte
		want         int
	}{
		{"h264 aud sei sps pps idr", H264, h264, len(h264) - 2*len(sc) - 5},
		{"h264 p-frame", H264, cat(sc, []byte{0x09, 0x30}, sc, []byte{0x41, 0x9a}), 0},
		{"hevc aud vps sps pps sei idr", HEVC, hevc, len(hevc) - 2*len(sc) - 6},
		{"hevc no start code", HEVC, []byte{0x40, 0x01}, 0},
		{"av1 td seq metadata frame", AV1, av1, 2 + 5},
		{"av1 frame only", AV1, cat(obu(obuTemporalDelimiter), obu(6, 1)), 0},
		{"av1 truncated", AV1, []byte{0x0a, 0x05, 0}, 0},
		{"unknown family", "vp9", h264, 0},
	} {
		if got := ParamSetsLen(tc.family, tc.data); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// paramSetsLenAll is ParamSetsLen of Annex-B data as a scan of every unit
// (SplitAnnexB's offsets) would compute it: TestParamSetsLenScan's reference.
func paramSetsLenAll(family string, data []byte) int {
	var units [][2]int
	start := -1
	for i := 0; i+2 < len(data); {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			i++
			continue
		}
		if start >= 0 {
			end := i
			for end > start && data[end-1] == 0 {
				end--
			}
			units = append(units, [2]int{start, end})
		}
		i += 3
		start = i
	}
	if start >= 0 && start < len(data) {
		units = append(units, [2]int{start, len(data)})
	}
	end := 0
	for _, u := range units {
		n := data[u[0]:u[1]]
		if len(n) == 0 {
			continue
		}
		var vcl, set bool
		if family == H264 {
			t := h264Type(n)
			vcl, set = (t >= 1 && t <= 5) || t == 14 || (t >= 19 && t <= 21), t == 7 || t == 8 || t == 13 || t == 15
		} else {
			t := hevcType(n)
			vcl, set = t < 32, t >= 32 && t <= 34
		}
		if vcl {
			break
		}
		if set {
			end = u[1]
		}
	}
	return end
}

// ParamSetsLen of an Annex-B frame stops at the coded picture (most of a
// key frame, not read): the same answer as a scan of every unit on random
// mixes of start codes, zeros, parameter sets, slices and other units.
func TestParamSetsLenScan(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	parts := map[string][][]byte{
		H264: {{0x09, 0xf0}, {0x06, 5}, {0x67, 0x64}, {0x68, 0xee}, {0x6d}, {0x6f}, {0x65, 0x88}, {0x41, 0x9a}, {0x74}, {0x01}, {0x00, 0x05}},
		HEVC: {{0x46, 0x01}, {0x40, 0x01}, {0x42, 0x01}, {0x44, 0x01}, {0x4e, 0x01}, {0x26, 0x01}, {0x02, 0x01}, {0x00, 0x01}, {0x48, 0x01}},
	}
	for _, family := range []string{H264, HEVC} {
		for i := 0; i < 20000; i++ {
			var b []byte
			for k := r.IntN(10); k >= 0; k-- {
				switch r.IntN(5) {
				case 0:
					b = append(b, 0, 0, 1)
				case 1:
					b = append(b, 0, 0, 0, 1)
				case 2:
					b = append(b, make([]byte, r.IntN(4))...)
				case 3:
					b = append(b, byte(r.IntN(4)), byte(r.IntN(256)))
				default:
					ps := parts[family]
					b = append(b, ps[r.IntN(len(ps))]...)
				}
			}
			if got, want := ParamSetsLen(family, b), paramSetsLenAll(family, b); got != want {
				t.Fatalf("%s % x: %d, want %d", family, b, got, want)
			}
		}
	}
}

// A large HEVC key frame (VPS/SPS/PPS, one IDR slice of 1 MiB): the host
// computes its reliable prefix before the frame goes out (GUIDE 2.4).
func BenchmarkParamSetsLenHEVC1MB(b *testing.B) {
	ps := JoinAnnexB([]byte{0x40, 0x01, 0x0c}, []byte{0x42, 0x01, 0x01}, []byte{0x44, 0x01, 0xc1})
	key := append(ps, JoinAnnexB(append([]byte{0x26, 0x01}, bytes.Repeat([]byte{0xaf, 0x00, 0x13}, 1<<20/3)...))...)
	b.SetBytes(int64(len(key)))
	for b.Loop() {
		if ParamSetsLen(HEVC, key) != len(ps) {
			b.Fatal("wrong prefix")
		}
	}
}

func TestH264GlobalHeaderKeyframes(t *testing.T) {
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "40",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "high", "-g", "15")
	st := streamsByTest[t.Name()][0]
	if FamilyFromFourCC(st.FourCC) != H264 {
		t.Fatalf("fourcc %q", st.FourCC)
	}
	p := NewParams(H264, st.Extradata)
	if !strings.HasPrefix(p.Codec, "avc1.") || len(p.Codec) != 11 {
		t.Fatalf("codec string from extradata: %q", p.Codec)
	}
	// The NUT muxer requests global headers, so key frames lack SPS/PPS.
	if p.hasParamSets(pkts[0].Data) {
		t.Log("encoder repeated headers in-band (fine)")
	}
	// Re-assemble the stream starting at the *second* key frame, as a client
	// joining mid-stream would see it, and make sure ffmpeg decodes all frames.
	var es bytes.Buffer
	started := false
	frames := 0
	keys := 0
	for _, pk := range pkts {
		if pk.Key {
			keys++
		}
		if !started && (!pk.Key || keys < 2) {
			continue
		}
		started = true
		d := pk.Data
		if pk.Key {
			d = p.PrepareKeyFrame(d)
			if !p.hasParamSets(d) {
				t.Fatal("key frame still lacks parameter sets")
			}
			checkParamSetsLen(t, p, d)
		}
		es.Write(d)
		frames++
	}
	n, errs := decodeCount(t, "h264", es.Bytes())
	if n != frames || errs != "" {
		t.Fatalf("decoded %d/%d frames, errors: %s", n, frames, errs)
	}
	t.Logf("codec=%s decoded %d frames from mid-stream join", p.Codec, n)
}

func TestHEVCKeyframes(t *testing.T) {
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "30",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=none:keyint=10:bframes=0")
	st := streamsByTest[t.Name()][0]
	p := NewParams(HEVC, st.Extradata)
	if !strings.HasPrefix(p.Codec, "hev1.1.6.L") {
		t.Fatalf("codec string %q", p.Codec)
	}
	var es bytes.Buffer
	keys, frames := 0, 0
	for _, pk := range pkts {
		if pk.Key {
			keys++
		}
		if keys < 2 {
			continue
		}
		d := pk.Data
		if pk.Key {
			d = p.PrepareKeyFrame(d)
			checkParamSetsLen(t, p, d)
		}
		es.Write(d)
		frames++
	}
	n, errs := decodeCount(t, "hevc", es.Bytes())
	if n != frames || errs != "" {
		t.Fatalf("decoded %d/%d frames, errors: %s", n, frames, errs)
	}
	t.Logf("codec=%s decoded %d frames", p.Codec, n)
}

func TestAV1Keyframes(t *testing.T) {
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "20",
		"-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-g", "8")
	st := streamsByTest[t.Name()][0]
	p := NewParams(AV1, st.Extradata)
	for _, pk := range pkts {
		if pk.Key {
			d := p.PrepareKeyFrame(pk.Data)
			if !p.hasParamSets(d) {
				t.Fatal("AV1 key frame without sequence header")
			}
			checkParamSetsLen(t, p, d)
		}
	}
	if !strings.HasPrefix(p.Codec, "av01.0.") || !strings.HasSuffix(p.Codec, "M.08") {
		t.Fatalf("codec string %q", p.Codec)
	}
	t.Logf("codec=%s", p.Codec)
}

func TestAV1SVT(t *testing.T) {
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=640x360:r=30", "-frames:v", "10",
		"-c:v", "libsvtav1", "-preset", "12", "-svtav1-params", "tune=0")
	st := streamsByTest[t.Name()][0]
	p := NewParams(AV1, st.Extradata)
	d := p.PrepareKeyFrame(pkts[0].Data)
	if !p.hasParamSets(d) || p.Codec == "" {
		t.Fatalf("svt-av1: params=%v codec=%q", p.hasParamSets(d), p.Codec)
	}
	t.Logf("codec=%s", p.Codec)
}

// bitWriter writes the AV1 sequence headers of TestAV1SequenceHeader.
type bitWriter struct {
	b []byte
	n int // bits written
}

func (w *bitWriter) put(v uint32, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 0x80 >> uint(w.n%8)
		}
		w.n++
	}
}

func (w *bitWriter) flag(b bool) {
	if b {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

// av1Seq describes a sequence header (AV1 spec 5.5); seqHeader writes it.
type av1Seq struct {
	profile       uint32
	reduced       bool
	timing, model bool // timing_info, decoder_model_info
	displayDelay  bool // initial_display_delay_present_flag
	levels        []uint32
	w, h          uint32
	wBits, hBits  int
	frameIDs      bool
	orderHint     bool
	chooseSCT     bool
	highBitDepth  bool
	twelveBit     bool
}

func (s av1Seq) header() []byte {
	w := &bitWriter{}
	w.put(s.profile, 3)
	w.put(0, 1) // still_picture
	w.flag(s.reduced)
	if s.reduced {
		w.put(s.levels[0], 5)
	} else {
		w.flag(s.timing)
		if s.timing {
			w.put(1, 32)    // num_units_in_display_tick
			w.put(60, 32)   // time_scale
			w.put(1, 1)     // equal_picture_interval
			w.put(0b010, 3) // num_ticks_per_picture_minus_1 = 1 (uvlc)
			w.flag(s.model) // decoder_model_info_present_flag
			if s.model {
				w.put(23, 5) // buffer_delay_length_minus_1: 24-bit delays
				w.put(1, 32) // num_units_in_decoding_tick
				w.put(31, 5) // buffer_removal_time_length_minus_1
				w.put(31, 5) // frame_presentation_time_length_minus_1
			}
		}
		w.flag(s.displayDelay)
		w.put(uint32(len(s.levels)-1), 5)
		for i, l := range s.levels {
			w.put(uint32(0x101+i), 12) // operating_point_idc
			w.put(l, 5)
			if l > 7 {
				w.flag(l >= 12) // seq_tier: high from level 6.0 on, main below
			}
			if s.model {
				w.put(1, 1)         // decoder_model_present_for_this_op
				w.put(0x123456, 24) // decoder_buffer_delay
				w.put(0x654321, 24) // encoder_buffer_delay
				w.put(1, 1)         // low_delay_mode_flag
			}
			if s.displayDelay {
				w.put(1, 1) // initial_display_delay_present_for_this_op
				w.put(9, 4)
			}
		}
	}
	w.put(uint32(s.wBits-1), 4)
	w.put(uint32(s.hBits-1), 4)
	w.put(s.w-1, s.wBits)
	w.put(s.h-1, s.hBits)
	if !s.reduced {
		w.flag(s.frameIDs)
	}
	if s.frameIDs {
		w.put(13, 4)
		w.put(2, 3)
	}
	w.put(0b011, 3) // use_128x128_superblock, enable_filter_intra, enable_intra_edge_filter
	if !s.reduced {
		w.put(0b1010, 4) // interintra, masked compound, warped motion, dual filter
		w.flag(s.orderHint)
		if s.orderHint {
			w.put(0b11, 2) // jnt_comp, ref_frame_mvs
		}
		w.flag(s.chooseSCT)
		if !s.chooseSCT {
			w.put(1, 1) // seq_force_screen_content_tools
		}
		w.put(0, 1) // seq_choose_integer_mv
		w.put(1, 1) // seq_force_integer_mv
		if s.orderHint {
			w.put(6, 3) // order_hint_bits_minus_1
		}
	}
	w.put(0b011, 3) // enable_superres, enable_cdef, enable_restoration
	w.flag(s.highBitDepth)
	if s.profile == 2 && s.highBitDepth {
		w.flag(s.twelveBit)
	}
	// The rest of color_config, film_grain_params_present, trailing bits:
	// not parsed, only there so the header looks complete.
	w.put(0, 12)
	return w.b
}

// TestAV1SequenceHeader parses crafted sequence headers that reach every
// branch before the frame size and bit depth, among them the 1920x1082 an
// RDNA3 GPU codes 1920x1080 as.
func TestAV1SequenceHeader(t *testing.T) {
	for _, c := range []struct {
		name  string
		seq   av1Seq
		codec string
	}{
		{"rdna3 1080p", av1Seq{levels: []uint32{8}, w: 1920, h: 1082, wBits: 11, hBits: 11}, "av01.0.08M.08"},
		{"reduced still picture header", av1Seq{reduced: true, levels: []uint32{5}, w: 1280, h: 720, wBits: 16, hBits: 16}, "av01.0.05M.08"},
		{"timing, decoder model, display delay, two operating points, frame ids", av1Seq{
			timing: true, model: true, displayDelay: true, levels: []uint32{12, 9}, w: 3456, h: 1440, wBits: 12, hBits: 11,
			frameIDs: true, orderHint: true, highBitDepth: true}, "av01.0.12H.10"},
		{"timing without decoder model", av1Seq{timing: true, levels: []uint32{13}, w: 2560, h: 1440, wBits: 12, hBits: 11, chooseSCT: true},
			"av01.0.13H.08"},
		{"profile 2 twelve bit 8K", av1Seq{profile: 2, levels: []uint32{16}, w: 7680, h: 4320, wBits: 13, hBits: 13, orderHint: true,
			highBitDepth: true, twelveBit: true}, "av01.2.16H.12"},
		{"one pixel", av1Seq{levels: []uint32{0}, w: 1, h: 1, wBits: 1, hBits: 1}, "av01.0.00M.08"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hdr := c.seq.header()
			h, err := ParseAV1SequenceHeader(hdr)
			if err != nil {
				t.Fatal(err)
			}
			if h.MaxWidth != int(c.seq.w) || h.MaxHeight != int(c.seq.h) || h.CodecString() != c.codec {
				t.Fatalf("got %dx%d %s, want %dx%d %s", h.MaxWidth, h.MaxHeight, h.CodecString(), c.seq.w, c.seq.h, c.codec)
			}
			// As OBUs: a temporal delimiter, then the sequence header with
			// an extension byte and a size field.
			obus := append([]byte{0x12, 0x00, obuSequenceHeader<<3 | 0x06, 0x00, byte(len(hdr))}, hdr...)
			p := NewParams(AV1, obus)
			if p.Codec != c.codec || p.CodedWidth != int(c.seq.w) || p.CodedHeight != int(c.seq.h) {
				t.Fatalf("params: %s %dx%d", p.Codec, p.CodedWidth, p.CodedHeight)
			}
			if w, h, ok := AV1FrameSize(obus); !ok || w != int(c.seq.w) || h != int(c.seq.h) {
				t.Fatalf("AV1FrameSize: %dx%d %v", w, h, ok)
			}
			// Cut before the bit depth: an error, never a guess.
			if _, err := ParseAV1SequenceHeader(hdr[:len(hdr)-3]); err == nil {
				t.Fatal("truncated header parsed")
			}
		})
	}
	if _, _, ok := AV1FrameSize([]byte{0x12, 0x00}); ok {
		t.Fatal("frame size without a sequence header")
	}
}

// TestAV1CodedSizeSVT reads the frame size from real SVT-AV1 streams (the
// extradata and the key frame) and compares it with ffprobe's: 1920x1080,
// and the 1920x1082 an RDNA3 GPU codes 1080p as (padded here by a filter).
func TestAV1CodedSizeSVT(t *testing.T) {
	for _, c := range []struct {
		vf   string
		w, h int
	}{{"null", 1920, 1080}, {"pad=iw:ih+2", 1920, 1082}, {"pad=iw+16:ih", 1936, 1080}} {
		pkts := encodeNUT(t, "-f", "lavfi", "-i", "color=c=black:s=1920x1080:r=30", "-frames:v", "3", "-vf", c.vf,
			"-pix_fmt", "yuv420p", "-c:v", "libsvtav1", "-preset", "12")
		st := streamsByTest[t.Name()][0]
		p := NewParams(AV1, st.Extradata)
		if p.CodedWidth != c.w || p.CodedHeight != c.h {
			t.Fatalf("%s: extradata %dx%d, want %dx%d", c.vf, p.CodedWidth, p.CodedHeight, c.w, c.h)
		}
		p = NewParams(AV1, nil)
		p.PrepareKeyFrame(pkts[0].Data)
		if p.CodedWidth != c.w || p.CodedHeight != c.h || !strings.HasPrefix(p.Codec, "av01.0.") {
			t.Fatalf("%s: key frame %s %dx%d", c.vf, p.Codec, p.CodedWidth, p.CodedHeight)
		}
		// ffprobe reads the same stream as low-overhead OBUs.
		var es bytes.Buffer
		for _, pk := range pkts {
			es.Write(pk.Data)
		}
		cmd := exec.Command("ffprobe", "-v", "error", "-f", "obu", "-show_entries", "stream=width,height", "-of", "csv=p=0", "-")
		cmd.Stdin = &es
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != fmt.Sprintf("%d,%d", c.w, c.h) {
			t.Fatalf("%s: ffprobe says %s, want %dx%d", c.vf, got, c.w, c.h)
		}
	}
}
