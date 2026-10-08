package qualify

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// encodeTestStream encodes frames 0..n-1 (except skip) with libx264: each a
// noisy 320x180 picture with the frame barcode of its number at barcodeAt,
// an IDR at frame 0 and at idr.
func encodeTestStream(t *testing.T, path string, n, idr, skip int) {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if out, err := exec.Command(ff, "-hide_banner", "-h", "encoder=libx264").CombinedOutput(); err != nil || !bytes.Contains(out, []byte("libx264")) {
		t.Skip("ffmpeg has no libx264")
	}
	const w, h = 320, 180
	rng := rand.New(rand.NewSource(1))
	var raw bytes.Buffer
	idrSeen := 0
	for i := 0; i < n; i++ {
		if i == skip {
			continue
		}
		if i < idr {
			idrSeen++
		}
		y := make([]byte, w*h)
		for k := range y {
			y[k] = byte(60 + rng.Intn(120))
		}
		word := proto.BarcodeWord(uint16(i))
		c := barcodeAt.Cell
		for k := 0; k < proto.BarcodeBits; k++ {
			v := byte(16)
			if word>>proto.BarcodeCellBit(k)&1 == 1 {
				v = 235
			}
			x0, y0 := barcodeAt.X+k%proto.BarcodeCols*c, barcodeAt.Y+k/proto.BarcodeCols*c
			for yy := y0; yy < y0+c; yy++ {
				for xx := x0; xx < x0+c; xx++ {
					y[yy*w+xx] = v
				}
			}
		}
		raw.Write(y)
		raw.Write(bytes.Repeat([]byte{128}, w*h/2))
	}
	cmd := exec.Command(ff, "-hide_banner", "-v", "error", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", "320x180", "-r", "60",
		"-i", "-", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-g", "1000", "-sc_threshold", "0",
		"-force_key_frames", "expr:eq(n,"+strconv.Itoa(idrSeen)+")", "-b:v", "2M", "-f", "h264", "-y", path)
	cmd.Stdin = &raw
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encoding the test stream: %v: %s", err, out)
	}
}

func TestDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.h264")
	encodeTestStream(t, path, 60, 20, 30) // frame 30 missing
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := Decode(ctx, "ffmpeg", path, "h264", &barcodeAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Frames) != 59 || len(d.Errors) != 0 {
		t.Fatalf("%d frames, errors %q", len(d.Frames), d.Errors)
	}
	for i, f := range d.Frames {
		want := i
		if i >= 30 {
			want = i + 1
		}
		if f.Barcode != want {
			t.Fatalf("frame %d: barcode %d, want %d", i, f.Barcode, want)
		}
		if f.Intra() != (i == 0 || i == 20) || (f.Key && f.Type != "I") {
			t.Fatalf("frame %d: key %v type %s", i, f.Key, f.Type)
		}
	}
	// Judged against a frame log without frame id 31 (frame 30): a frame-id
	// gap, which the barcodes agree with (no barcode gap of their own).
	l := run(60, 10, 50000, 20000, 60)
	l.Frames[20].Key = true
	l.Frames = append(l.Frames[:30], l.Frames[31:]...)
	l.Frames[30].Bytes = l.Frames[29].Bytes
	c := Cell{}
	Judge(Input{Mode: ModeSeamless, FPS: 60, Log: l, Decoded: d, Barcode: true}, &c)
	if c.Barcode == nil || c.Barcode.Gaps != 0 || c.Barcode.Wrong != 0 || c.FrameIDs.Gaps != 1 {
		t.Fatalf("barcode %+v ids %+v failures %q", c.Barcode, c.FrameIDs, c.Failures)
	}
	if c.KeyFrames.Mismatched != 0 || len(c.KeyFrames.Unexpected) != 1 || c.KeyFrames.Unexpected[0] != 21 {
		t.Fatalf("keys %+v", c.KeyFrames)
	}

	// Without the barcode: types only.
	d, err = Decode(ctx, "ffmpeg", path, "h264", nil)
	if err != nil || len(d.Frames) != 59 || d.Frames[5].Barcode != -2 || !d.Frames[20].Key {
		t.Fatalf("no barcode: %v %+v", err, d)
	}
	// A broken stream: errors are a finding, not a failure of Decode.
	b, _ := os.ReadFile(path)
	broken := filepath.Join(t.TempDir(), "broken.h264")
	_ = os.WriteFile(broken, append(b[:len(b)/2:len(b)/2], b[len(b)/2+2000:]...), 0o600)
	d, err = Decode(ctx, "ffmpeg", broken, "h264", &barcodeAt)
	if err != nil || len(d.Errors) == 0 {
		t.Fatalf("broken stream: %v, errors %q", err, d.Errors)
	}
	// Not a stream at all: no frames, an error.
	_ = os.WriteFile(broken, []byte("not a stream"), 0o600)
	if _, err := Decode(ctx, "ffmpeg", broken, "h264", nil); err == nil {
		t.Fatal("garbage decoded")
	}
	if _, err := Decode(ctx, "ffmpeg", broken, "vp9", nil); err == nil {
		t.Fatal("unknown codec accepted")
	}
}
