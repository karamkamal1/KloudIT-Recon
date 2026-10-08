package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

func TestBarcodeArgs(t *testing.T) {
	caps := &Caps{Filters: map[string]bool{"ddagrab": true, "realtime": true}, options: map[string]map[string]bool{}}
	enc := EncoderInfo{"libx264", "h264", "software", false, false}
	args, _ := caps.BuildArgs(Params{Source: Source{Backend: "test", NativeW: 640, NativeH: 360}, Encoder: enc, FPS: 60, CaptureClock: true, Barcode: true})
	joined := strings.Join(args, " ")
	// After the source and its capture clock, before the format conversion.
	if !strings.Contains(joined, "realtime,"+CaptureClockFilter+","+BarcodeFilter(proto.BarcodeCell)+",format=yuv420p[v]") {
		t.Fatalf("test source barcode: %s", joined)
	}
	args, _ = caps.BuildArgs(Params{Source: Source{Backend: "test"}, Encoder: enc, FPS: 60})
	if strings.Contains(strings.Join(args, " "), "drawbox") {
		t.Fatal("barcode drawn without Params.Barcode")
	}
	args, _ = caps.BuildArgs(Params{Source: Source{Backend: "ddagrab"}, Encoder: enc, FPS: 60, Barcode: true})
	if strings.Contains(strings.Join(args, " "), "drawbox") {
		t.Fatal("barcode drawn into a screen capture")
	}
}

// TestBarcodeFilter runs real encoders on the test source with the frame
// barcode and decodes every frame the Video manager delivers again with FFmpeg:
// each must carry its own Seq (drawbox's n = Frame.Seq), at a normal and a low
// bitrate. Drawing it must not change the frame rate, bitrate or
// capture->encoded time.
func TestBarcodeFilter(t *testing.T) {
	caps := probeOrSkip(t)
	if !caps.Filters["drawbox"] {
		t.Skip("ffmpeg lacks drawbox")
	}
	if !caps.CanDrawBarcode() {
		t.Fatalf("probe rejected the barcode: %v", testBarcode(context.Background(), caps.FFmpeg))
	}
	for _, name := range []string{"libx264", "libsvtav1"} {
		var enc EncoderInfo
		for _, e := range caps.Encoders {
			if e.Name == name {
				enc = e
			}
		}
		if enc.Name == "" {
			t.Logf("%s not available", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			type run struct {
				fps, mbps, p50 float64
				frames         []*Frame
			}
			const w, h, n = 960, 540, 180
			measure := func(barcode bool, kbps int) run {
				v := NewVideo(caps, nil, NewClock())
				defer v.Stop()
				p := Params{Source: Source{Backend: "test", NativeW: w, NativeH: h}, Encoder: enc, FPS: 60, BitrateKbps: kbps,
					CaptureClock: caps.CanStampCapture(), Barcode: barcode}
				if err := v.Start(p, false); err != nil {
					t.Fatal(err)
				}
				var r run
				var lat []float64
				var bytes int
				var first time.Time
				deadline := time.After(30 * time.Second)
				for len(r.frames) < n {
					select {
					case ev := <-v.Events():
						if ev.Err != nil {
							t.Fatal(ev.Err)
						}
						if ev.Frame == nil {
							continue
						}
						if len(r.frames) == 0 {
							first = time.Now()
						}
						r.frames = append(r.frames, ev.Frame)
						if len(r.frames) > 30 {
							bytes += len(ev.Frame.Data)
							if ev.Frame.CaptureUs != 0 {
								lat = append(lat, float64(ev.Frame.EncodeDoneUs-ev.Frame.CaptureUs)/1000)
							}
						}
					case <-deadline:
						t.Fatalf("timeout after %d frames", len(r.frames))
					}
				}
				r.fps = float64(n-1) / time.Since(first).Seconds()
				r.mbps = float64(bytes) * 8 / (float64(n-30) / 60) / 1e6
				if len(lat) > 0 {
					sort.Float64s(lat)
					r.p50 = lat[len(lat)/2]
				}
				return r
			}
			base := measure(false, 4000)
			got := measure(true, 4000)
			t.Logf("%s without barcode: %.1f fps %.2f Mbit/s capture->encoded p50 %.2f ms; with: %.1f fps %.2f Mbit/s p50 %.2f ms",
				name, base.fps, base.mbps, base.p50, got.fps, got.mbps, got.p50)
			if got.fps < 40 || got.fps > 75 {
				t.Fatalf("frame rate %.1f with the barcode", got.fps)
			}
			if r := got.mbps / base.mbps; r < 0.8 || r > 1.25 {
				t.Fatalf("bitrate changed: %.2f vs %.2f Mbit/s", got.mbps, base.mbps)
			}
			for _, kbps := range []int{4000, 500} {
				frames := got.frames
				if kbps != 4000 {
					frames = measure(true, kbps).frames
				}
				planes := decodeLuma(t, caps.FFmpeg, enc.Family, frames, w, h)
				if len(planes) != len(frames) {
					t.Fatalf("%d kbit/s: decoded %d of %d frames", kbps, len(planes), len(frames))
				}
				for i, pl := range planes {
					v, ok := proto.BarcodeReadLuma(pl, w, proto.BarcodeCell)
					if !ok || v != uint16(frames[i].Seq) {
						t.Fatalf("%d kbit/s: frame %d (seq %d): barcode %d, valid %v", kbps, i, frames[i].Seq, v, ok)
					}
				}
				t.Logf("%d kbit/s: all %d frames carry their seq (%d..%d)", kbps, len(frames), frames[0].Seq, frames[len(frames)-1].Seq)
			}
		})
	}
}

// decodeLuma decodes a generation's frames in order with FFmpeg (H.264 as
// Annex B, AV1 in IVF) and returns each picture's luma plane. decOpts are
// decoder options.
func decodeLuma(t *testing.T, ffmpeg, family string, frames []*Frame, w, h int, decOpts ...string) [][]byte {
	t.Helper()
	var in bytes.Buffer
	format := "h264"
	switch family {
	case "h264":
		for _, f := range frames {
			in.Write(f.Data)
		}
	case "av1":
		format = "ivf"
		hdr := make([]byte, 32)
		copy(hdr, "DKIF")
		binary.LittleEndian.PutUint16(hdr[6:], 32)
		copy(hdr[8:], "AV01")
		binary.LittleEndian.PutUint16(hdr[12:], uint16(w))
		binary.LittleEndian.PutUint16(hdr[14:], uint16(h))
		binary.LittleEndian.PutUint32(hdr[16:], 60)
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
	default:
		t.Fatalf("no decoder set up for %s", family)
	}
	var out, stderr bytes.Buffer
	args := append([]string{"-hide_banner", "-loglevel", "error"}, decOpts...)
	args = append(args, "-f", format, "-i", "pipe:0", "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
	cmd := exec.Command(ffmpeg, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = &in, &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("decoding: %v: %s", err, stderr.String())
	}
	var planes [][]byte
	for {
		p := make([]byte, w*h)
		if _, err := io.ReadFull(&out, p); err != nil {
			break
		}
		planes = append(planes, p)
	}
	return planes
}
