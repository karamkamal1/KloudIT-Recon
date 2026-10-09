package codec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscardableSVT: SVT-AV1's low-delay structure (the arguments of the
// FFmpeg path's libsvtav1) codes frames no later frame references; the
// stream without them must decode cleanly, and every frame kept must decode
// to exactly the picture it has in the whole stream (dav1d, frame MD5s).
func TestDiscardableSVT(t *testing.T) {
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=320x180:r=60", "-frames:v", "48",
		"-c:v", "libsvtav1", "-preset", "12", "-g", "1000", "-svtav1-params", "pred-struct=1:lookahead=0:scd=0:rc=2", "-b:v", "1M")
	st := streamsByTest[t.Name()][0]
	p := NewParams(AV1, st.Extradata)
	var all, kept bytes.Buffer
	var keep []bool
	n := 0
	for i, pk := range pkts {
		d := pk.Data
		if pk.Key {
			d = p.PrepareKeyFrame(d)
		}
		disc := p.Discardable(d, pk.Key)
		if pk.Key && disc {
			t.Fatalf("frame %d: a key frame counted as discardable", i)
		}
		tu := withTemporalDelimiter(d)
		all.Write(tu)
		if !disc {
			kept.Write(tu)
		} else {
			n++
		}
		keep = append(keep, !disc)
	}
	if n < len(pkts)/4 {
		t.Fatalf("%d of %d frames discardable, want at least a quarter (SVT-AV1 low delay: every second frame)", n, len(pkts))
	}
	full, fullErr := frameMD5s(t, all.Bytes())
	thin, thinErr := frameMD5s(t, kept.Bytes())
	if fullErr != "" || thinErr != "" {
		t.Fatalf("decode errors: whole stream %q, without the discardable frames %q", fullErr, thinErr)
	}
	var want []string
	for i, k := range keep {
		if k && i < len(full) {
			want = append(want, full[i])
		}
	}
	if len(full) != len(pkts) || strings.Join(thin, ",") != strings.Join(want, ",") {
		t.Fatalf("decoded %d / %d frames; kept frames differ from the whole stream's (%d vs %d)", len(full), len(thin), len(thin), len(want))
	}
	t.Logf("%d of %d frames discardable; the rest decode bit-identically without them", n, len(pkts))
}

// withTemporalDelimiter returns a temporal unit that starts with a temporal
// delimiter OBU (the obu demuxer splits the stream at them).
func withTemporalDelimiter(tu []byte) []byte {
	if len(tu) > 0 && int(tu[0]>>3)&0xf == obuTemporalDelimiter {
		return tu
	}
	return append([]byte{obuTemporalDelimiter<<3 | 2, 0}, tu...)
}

// frameMD5s decodes a low-overhead AV1 stream with dav1d and returns each
// frame's MD5 and the decoder's errors.
func frameMD5s(t *testing.T, obus []byte) ([]string, string) {
	t.Helper()
	in := filepath.Join(t.TempDir(), "in.obu")
	if err := os.WriteFile(in, obus, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-c:v", "libdav1d", "-f", "obu", "-i", in, "-f", "framemd5", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("ffmpeg with libdav1d: %v %s", err, stderr.String())
	}
	var md5s []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		md5s = append(md5s, strings.TrimSpace(f[len(f)-1]))
	}
	return md5s, strings.TrimSpace(stderr.String())
}

// av1FrameHdr writes a frame header OBU for one of the sequence headers of
// av1Seq (decoder model: every operating point has one, equal picture
// interval; frame ids 18 bits; order hint 7 bits; screen content tools
// chosen per frame or forced on, integer MV forced).
type av1FrameHdr struct {
	showExisting   bool
	frameType      uint32 // 0 key, 1 inter, 2 intra-only, 3 switch
	show           bool
	errorResilient bool
	refresh        uint32
}

func (f av1FrameHdr) obu(s av1Seq, typ int, ext bool) []byte {
	w := &bitWriter{}
	w.flag(f.showExisting)
	if f.showExisting {
		w.put(3, 3)
	} else {
		w.put(f.frameType, 2)
		w.flag(f.show)
		if !f.show {
			w.put(1, 1) // showable_frame
		}
		if f.frameType != 3 && !(f.frameType == 0 && f.show) {
			w.flag(f.errorResilient)
		}
		w.put(0, 1) // disable_cdf_update
		if s.chooseSCT {
			w.put(1, 1) // allow_screen_content_tools (integer MV forced: nothing more)
		}
		if s.frameIDs {
			w.put(0x2abcd, 18) // current_frame_id
		}
		if f.frameType != 3 {
			w.put(0, 1) // frame_size_override_flag
		}
		if s.orderHint {
			w.put(5, 7) // order_hint
		}
		intra := f.frameType == 0 || f.frameType == 2
		if !intra && !f.errorResilient && f.frameType != 3 {
			w.put(2, 3) // primary_ref_frame
		}
		if s.model {
			w.put(1, 1) // buffer_removal_time_present_flag
			for i := range s.levels {
				if (0x101+i)&1 == 1 { // operating_point_idc 0x101 + i: only those with temporal layer 0
					w.put(0xdeadbeef, 32) // buffer_removal_time
				}
			}
		}
		if f.frameType != 3 && !(f.frameType == 0 && f.show) {
			w.put(f.refresh, 8)
		}
		w.put(0x5a5a, 16) // the rest of the header, not parsed
	}
	h := []byte{byte(typ<<3) | 2}
	if ext {
		h[0] |= 4
		h = append(h, 0) // temporal_id 0, spatial_id 0
	}
	return append(append(h, byte(len(w.b))), w.b...)
}

// TestDiscardableAV1Headers parses crafted frame headers through every
// branch before refresh_frame_flags.
func TestDiscardableAV1Headers(t *testing.T) {
	seqs := map[string]av1Seq{
		"plain":                  {levels: []uint32{8}, w: 1920, h: 1080, wBits: 11, hBits: 11},
		"decoder model, ids":     {timing: true, model: true, levels: []uint32{12, 9}, w: 3456, h: 1440, wBits: 12, hBits: 11, frameIDs: true, orderHint: true},
		"order hint, choose sct": {levels: []uint32{13}, w: 2560, h: 1440, wBits: 12, hBits: 11, orderHint: true, chooseSCT: true},
	}
	td := []byte{obuTemporalDelimiter<<3 | 2, 0}
	for name, s := range seqs {
		hdr := s.header()
		p := NewParams(AV1, append([]byte{obuSequenceHeader<<3 | 2, byte(len(hdr))}, hdr...))
		if p.av1Seq == nil {
			t.Fatalf("%s: sequence header not kept", name)
		}
		tu := func(fs ...av1FrameHdr) []byte {
			b := append([]byte(nil), td...)
			for i, f := range fs {
				typ := av1Frame
				if i%2 == 1 {
					typ = av1FrameHeader
				}
				b = append(b, f.obu(s, typ, i == 0)...)
			}
			return b
		}
		inter := av1FrameHdr{frameType: 1, show: true}
		nonRef := inter
		ref := inter
		ref.refresh = 0x08
		resilient := nonRef
		resilient.errorResilient = true
		hidden := nonRef
		hidden.show = false
		for _, c := range []struct {
			what string
			data []byte
			key  bool
			want bool
		}{
			{"shown inter frame, refresh 0", tu(nonRef), false, true},
			{"error resilient, refresh 0", tu(resilient), false, true},
			{"two frame headers, both refresh 0", tu(nonRef, nonRef), false, true},
			{"inter frame refreshing a slot", tu(ref), false, false},
			{"one of two refreshes", tu(nonRef, ref), false, false},
			{"hidden frame", tu(hidden), false, false},
			{"key frame", tu(av1FrameHdr{frameType: 0, show: true}), true, false},
			{"intra-only, refresh 0", tu(av1FrameHdr{frameType: 2, show: true}), false, true},
			{"switch frame", tu(av1FrameHdr{frameType: 3, show: true}), false, false},
			{"show existing frame", tu(av1FrameHdr{showExisting: true}), false, false},
			{"no frame header", td, false, false},
			{"truncated", tu(nonRef)[:5], false, false},
		} {
			if got := p.Discardable(c.data, c.key); got != c.want {
				t.Errorf("%s: %s: discardable %v, want %v", name, c.what, got, c.want)
			}
		}
	}
	if (&Params{Family: AV1}).Discardable(append(td, av1FrameHdr{frameType: 1, show: true}.obu(av1Seq{}, av1Frame, false)...), false) {
		t.Error("discardable without a sequence header")
	}
}

// TestDiscardableH264: nal_ref_idc of the first slice decides; IDRs, key
// frames and HEVC never qualify; x264's low-latency stream has none.
func TestDiscardableH264(t *testing.T) {
	p := NewParams(H264, nil)
	aud, sei := []byte{0, 0, 0, 1, 0x09, 0xf0}, []byte{0, 0, 1, 0x06, 0x05, 0x01, 0x00, 0x80}
	slice := func(hdr byte) []byte { return []byte{0, 0, 1, hdr, 0x9a, 0x00, 0x00, 0x03, 0x01} }
	for _, c := range []struct {
		what string
		data []byte
		key  bool
		want bool
	}{
		{"non-reference P slice after AUD and SEI", append(append(append([]byte{}, aud...), sei...), slice(0x01)...), false, true},
		{"reference P slice", append(append([]byte{}, aud...), slice(0x41)...), false, false},
		{"nal_ref_idc 3", slice(0x61), false, false},
		{"IDR", slice(0x65), false, false},
		{"marked key", slice(0x01), true, false},
		{"no slice", aud, false, false},
	} {
		if got := p.Discardable(c.data, c.key); got != c.want {
			t.Errorf("%s: %v, want %v", c.what, got, c.want)
		}
	}
	if NewParams(HEVC, nil).Discardable([]byte{0, 0, 1, 0x00, 0x01, 0xaf}, false) {
		t.Error("HEVC TRAIL_N counted as discardable without the SPS")
	}
	pkts := encodeNUT(t, "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "20",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
	for i, pk := range pkts {
		if p.Discardable(pk.Data, pk.Key) {
			t.Fatalf("x264 zerolatency frame %d discardable", i)
		}
	}
}
