package codec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The browser's decoder self-test (web/static/js/decoder-selftest.js, guide
// step 4.1) decodes a short clip per codec family and checks that the first
// output arrives after one chunk: a decoder that holds frames back adds that
// many frame intervals to every frame. The clips live in
// web/static/js/decoder-selftest-clips.js, generated here from FFmpeg the way
// the host sends a stream: NUT packets, parameter sets in-band on the key
// frame (Params.PrepareKeyFrame), the codec string the host would announce.
// Each is a key frame and then P frames only, decode order = display order,
// nothing a decoder may hold back for reordering (x264 and x265 signal zero
// reorder frames; SVT-AV1 low-delay prediction has no hidden frames).
//
//	RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips
//
// regenerates the file; otherwise the test checks the committed clips.

const (
	selfTestFrames = 10
	selfTestW      = 640
	selfTestH      = 360
	clipsPrefix    = "export const CLIPS = "
)

// Color bars with a white square moving 20 px per frame: real P frames, a
// few kB per clip.
const selfTestSource = "smptebars=s=640x360:r=60,drawbox=x=40+t*1200:y=150:w=64:h=64:color=white:t=fill,format=yuv420p"

var selfTestEncoders = []struct {
	family string
	args   []string
}{
	{H264, []string{"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency", "-profile:v", "high", "-g", "600", "-qp", "45"}},
	{HEVC, []string{"-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency", "-x265-params", "log-level=none:keyint=600:qp=45"}},
	{AV1, []string{"-c:v", "libsvtav1", "-preset", "12", "-svtav1-params", "pred-struct=1:lookahead=0:scd=0:rc=0", "-qp", "55", "-g", "600"}},
}

type selfTestClip struct {
	Codec  string   `json:"codec"`
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Frames []string `json:"frames"` // base64, the first one a key frame
}

func clipsPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "decoder-selftest-clips.js")
}

func TestDecoderSelfTestClips(t *testing.T) {
	if os.Getenv("RECON_UPDATE_CLIPS") == "1" {
		writeSelfTestClips(t)
	}
	src, err := os.ReadFile(clipsPath())
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, clipsPrefix)
	if i < 0 || !strings.HasSuffix(s, ";\n") {
		t.Fatalf("%s: %q ... ; not found", clipsPath(), clipsPrefix)
	}
	var clips map[string]selfTestClip
	if err := json.Unmarshal([]byte(s[i+len(clipsPrefix):len(s)-2]), &clips); err != nil {
		t.Fatal(err)
	}
	for _, e := range selfTestEncoders {
		c, ok := clips[e.family]
		if !ok {
			t.Errorf("%s: no clip", e.family)
			continue
		}
		if len(c.Frames) != selfTestFrames || c.Width != selfTestW || c.Height != selfTestH {
			t.Errorf("%s: %d frames %dx%d", e.family, len(c.Frames), c.Width, c.Height)
			continue
		}
		var es bytes.Buffer
		for k, f := range c.Frames {
			d, err := base64.StdEncoding.DecodeString(f)
			if err != nil {
				t.Fatalf("%s frame %d: %v", e.family, k, err)
			}
			p := NewParams(e.family, nil)
			if k == 0 && (!p.hasParamSets(d) || !p.updateCodec(d) || p.Codec != c.Codec) {
				t.Errorf("%s: key frame without parameter sets, or codec %q != %q", e.family, p.Codec, c.Codec)
			}
			if k > 0 && p.hasParamSets(d) {
				t.Errorf("%s frame %d carries parameter sets (a key frame?)", e.family, k)
			}
			if e.family == AV1 {
				if obus, err := SplitOBUs(d); err != nil || len(obus) == 0 || obus[0].typ != obuTemporalDelimiter {
					es.Write([]byte{0x12, 0x00}) // the obu demuxer splits temporal units at delimiters
				}
			}
			es.Write(d)
		}
		probeSelfTestClip(t, e.family, es.Bytes())
	}
}

// probeSelfTestClip decodes a clip with FFmpeg: ten frames, an I frame and
// then P frames, and no reorder delay (has_b_frames 0).
func probeSelfTestClip(t *testing.T, family string, es []byte) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Log("ffprobe not installed: clip not decoded")
		return
	}
	format := map[string]string{H264: "h264", HEVC: "hevc", AV1: "obu"}[family]
	cmd := exec.Command("ffprobe", "-v", "error", "-f", format, "-select_streams", "v:0",
		"-show_entries", "stream=width,height,has_b_frames:frame=pict_type,key_frame", "-of", "json", "-")
	cmd.Stdin = bytes.NewReader(es)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: ffprobe: %v %s", family, err, stderr.String())
	}
	var r struct {
		Streams []struct {
			Width      int `json:"width"`
			Height     int `json:"height"`
			HasBFrames int `json:"has_b_frames"`
		} `json:"streams"`
		Frames []struct {
			PictType string `json:"pict_type"`
			Key      int    `json:"key_frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &r); err != nil || len(r.Streams) != 1 {
		t.Fatalf("%s: ffprobe output %s (%v)", family, out, err)
	}
	types := ""
	for _, f := range r.Frames {
		types += f.PictType
	}
	st := r.Streams[0]
	if types != "I"+strings.Repeat("P", selfTestFrames-1) || r.Frames[0].Key != 1 || st.HasBFrames != 0 ||
		st.Width != selfTestW || st.Height != selfTestH || stderr.Len() > 0 {
		t.Errorf("%s: frames %q, %dx%d, has_b_frames %d, errors: %s", family, types, st.Width, st.Height, st.HasBFrames, stderr.String())
	}
	t.Logf("%s: %d bytes, frames %s, has_b_frames %d", family, len(es), types, st.HasBFrames)
}

func writeSelfTestClips(t *testing.T) {
	clips := map[string]selfTestClip{}
	for _, e := range selfTestEncoders {
		args := append([]string{"-f", "lavfi", "-i", selfTestSource, "-frames:v", "10"}, e.args...)
		pkts := encodeNUT(t, args...)
		st := streamsByTest[t.Name()][0]
		if FamilyFromFourCC(st.FourCC) != e.family || len(pkts) != selfTestFrames {
			t.Fatalf("%s: fourcc %q, %d packets", e.family, st.FourCC, len(pkts))
		}
		p := NewParams(e.family, st.Extradata)
		c := selfTestClip{Width: st.Width, Height: st.Height}
		for k, pk := range pkts {
			if pk.Key != (k == 0) || (k > 0 && pk.Pts <= pkts[k-1].Pts) {
				t.Fatalf("%s packet %d: key %v pts %d: not one key frame then P frames in display order", e.family, k, pk.Key, pk.Pts)
			}
			d := pk.Data
			if pk.Key {
				d = p.PrepareKeyFrame(d)
			}
			c.Frames = append(c.Frames, base64.StdEncoding.EncodeToString(d))
		}
		c.Codec = p.Codec
		clips[e.family] = c
	}
	js, err := json.MarshalIndent(clips, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	head := "// Generated by `RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips`\n" +
		"// (internal/codec/selftest_clips_test.go); do not edit. The decoder self-test clips\n" +
		"// (decoder-selftest.js): per codec family ten 640x360 frames, a key frame with its\n" +
		"// parameter sets and then P frames only (decode order = display order), as the host\n" +
		"// sends them. Base64 per frame.\n"
	if err := os.WriteFile(clipsPath(), []byte(head+clipsPrefix+string(js)+";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
