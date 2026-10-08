package codec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
// Step 4.2 times the decoders on a second set made the same way
// (web/static/js/decoder-timing-clips.js): 1920x1080 with detail and motion
// over the whole picture, so the time from decode() to a frame's output is the
// decoder's work on a full-size frame, not mostly the fixed cost of a call,
// which is what the small hygiene clip measures (a software decoder beats a
// hardware decoder's round trip there). The host compares these times between
// codec families.
//
//	RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips
//
// regenerates both files; otherwise the test checks the committed clips.

// clipSet is one generated module of clips: the same frame count and size
// for every family.
type clipSet struct {
	file     string // in web/static/js
	export   string // the module's export
	frames   int
	w, h     int
	source   string // lavfi source
	encoders []clipEncoder
	about    string // the generated file's description (comment lines)
}

type clipEncoder struct {
	family string
	args   []string
}

var clipSets = []clipSet{
	{
		file: "decoder-selftest-clips.js", export: "CLIPS", frames: 10, w: 640, h: 360,
		// Color bars with a white square moving 20 px per frame: real P
		// frames, a few kB per clip.
		source: "smptebars=s=640x360:r=60,drawbox=x=40+t*1200:y=150:w=64:h=64:color=white:t=fill,format=yuv420p",
		encoders: []clipEncoder{
			{H264, []string{"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency", "-profile:v", "high", "-g", "600", "-qp", "45"}},
			{HEVC, []string{"-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency", "-x265-params", "log-level=none:keyint=600:qp=45"}},
			{AV1, []string{"-c:v", "libsvtav1", "-preset", "12", "-svtav1-params", "pred-struct=1:lookahead=0:scd=0:rc=0", "-qp", "55", "-g", "600"}},
		},
		about: "// (internal/codec/selftest_clips_test.go); do not edit. The decoder self-test clips\n" +
			"// (decoder-selftest.js): per codec family ten 640x360 frames, a key frame with its\n" +
			"// parameter sets and then P frames only (decode order = display order), as the host\n" +
			"// sends them. Base64 per frame.\n",
	},
	{
		file: "decoder-timing-clips.js", export: "TIMING_CLIPS", frames: 8, w: 1920, h: 1080,
		// FFmpeg's moving test pattern: gradients, edges and text over the
		// whole picture, all of it moving (2-6 kB per P frame).
		source: "testsrc2=s=1920x1080:r=60,format=yuv420p",
		encoders: []clipEncoder{
			{H264, []string{"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency", "-profile:v", "high", "-g", "600", "-qp", "40"}},
			{HEVC, []string{"-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency", "-x265-params", "log-level=none:keyint=600:qp=40"}},
			{AV1, []string{"-c:v", "libsvtav1", "-preset", "12", "-svtav1-params", "pred-struct=1:lookahead=0:scd=0:rc=0", "-qp", "52", "-g", "600"}},
		},
		about: "// (internal/codec/selftest_clips_test.go); do not edit. The decode timing clips\n" +
			"// (decoder-selftest.js timeDecoder, step 4.2): per codec family eight 1920x1080 frames of\n" +
			"// FFmpeg's moving test pattern, a key frame with its parameter sets and then P frames\n" +
			"// only (decode order = display order), as the host sends them. Base64 per frame.\n",
	},
}

type selfTestClip struct {
	Codec  string   `json:"codec"`
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Frames []string `json:"frames"` // base64, the first one a key frame
}

func (cs clipSet) path() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", cs.file)
}

func (cs clipSet) prefix() string { return "export const " + cs.export + " = " }

func TestDecoderSelfTestClips(t *testing.T) {
	for _, cs := range clipSets {
		t.Run(cs.export, func(t *testing.T) {
			if os.Getenv("RECON_UPDATE_CLIPS") == "1" {
				writeClips(t, cs)
			}
			checkClips(t, cs)
		})
	}
}

// checkClips checks a committed clip module: every family's clip has the
// set's frame count and size, its key frame first with the parameter sets and
// the codec string the host's Params derives from them, no later frame with
// parameter sets, and FFmpeg decodes it as an I frame and P frames with no
// reorder delay.
func checkClips(t *testing.T, cs clipSet) {
	src, err := os.ReadFile(cs.path())
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, cs.prefix())
	if i < 0 || !strings.HasSuffix(s, ";\n") {
		t.Fatalf("%s: %q ... ; not found", cs.path(), cs.prefix())
	}
	var clips map[string]selfTestClip
	if err := json.Unmarshal([]byte(s[i+len(cs.prefix()):len(s)-2]), &clips); err != nil {
		t.Fatal(err)
	}
	for _, e := range cs.encoders {
		c, ok := clips[e.family]
		if !ok {
			t.Errorf("%s: no clip", e.family)
			continue
		}
		if len(c.Frames) != cs.frames || c.Width != cs.w || c.Height != cs.h {
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
		probeSelfTestClip(t, cs, e.family, es.Bytes())
	}
}

// probeSelfTestClip decodes a clip with FFmpeg: the set's frame count, an I
// frame and then P frames, and no reorder delay (has_b_frames 0).
func probeSelfTestClip(t *testing.T, cs clipSet, family string, es []byte) {
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
	if err := json.Unmarshal(out, &r); err != nil || len(r.Streams) != 1 || len(r.Frames) == 0 {
		t.Fatalf("%s: ffprobe output %s (%v)", family, out, err)
	}
	types := ""
	for _, f := range r.Frames {
		types += f.PictType
	}
	st := r.Streams[0]
	if types != "I"+strings.Repeat("P", cs.frames-1) || r.Frames[0].Key != 1 || st.HasBFrames != 0 ||
		st.Width != cs.w || st.Height != cs.h || stderr.Len() > 0 {
		t.Errorf("%s: frames %q, %dx%d, has_b_frames %d, errors: %s", family, types, st.Width, st.Height, st.HasBFrames, stderr.String())
	}
	t.Logf("%s: %d bytes, frames %s, has_b_frames %d", family, len(es), types, st.HasBFrames)
}

func writeClips(t *testing.T, cs clipSet) {
	clips := map[string]selfTestClip{}
	for _, e := range cs.encoders {
		args := append([]string{"-f", "lavfi", "-i", cs.source, "-frames:v", strconv.Itoa(cs.frames)}, e.args...)
		pkts := encodeNUT(t, args...)
		st := streamsByTest[t.Name()][0]
		if FamilyFromFourCC(st.FourCC) != e.family || len(pkts) != cs.frames {
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
	head := "// Generated by `RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips`\n" + cs.about
	if err := os.WriteFile(cs.path(), []byte(head+cs.prefix()+string(js)+";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
