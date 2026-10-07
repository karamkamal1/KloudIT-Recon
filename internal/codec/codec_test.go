package codec

import (
	"bytes"
	"encoding/json"
	"io"
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
