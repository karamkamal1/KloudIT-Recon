package qualify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// DecodedFrame is one frame as the decoder saw it.
type DecodedFrame struct {
	Key  bool   // the decoder's key-frame flag (IDR / IRAP / AV1 key frame)
	Type string // picture type: I, P, B, ... ("?" unknown)
	// Barcode: the frame barcode's value, -1 unreadable, -2 not read.
	Barcode int
}

// Intra reports whether the frame is a key frame or an intra picture.
func (d DecodedFrame) Intra() bool { return d.Key || d.Type == "I" }

// Decoded is a decoder run over a stream file.
type Decoded struct {
	Frames   []DecodedFrame
	Errors   []string // the decoder's error lines
	Warnings int
}

// BarcodeArea is where the frame barcode is in the picture (output pixels).
type BarcodeArea struct {
	X, Y, Cell int
}

var (
	showinfoRe = regexp.MustCompile(`\[info\] n:\s*\d+ .* iskey:(\d) type:(\S)`)
	levelRe    = regexp.MustCompile(`\[(warning|error|fatal|panic)\]`)
)

// Decode decodes a stream file (Annex-B H.264 / HEVC, IVF AV1) with FFmpeg
// 5.1 or newer and returns every frame's key flag and picture type
// (FFmpeg's showinfo filter) and, with bc, the value of its frame barcode
// (the barcode area cropped, read as gray with proto.BarcodeReadLuma, as the
// browser's probe reads it). Decoder errors are collected, not returned: they
// are a finding (artifacts), not a failure of the check.
func Decode(ctx context.Context, ffmpeg, file, codec string, bc *BarcodeArea) (*Decoded, error) {
	args := []string{"-hide_banner", "-nostats", "-nostdin", "-loglevel", "level+info"}
	switch codec {
	case "h264", "hevc":
		args = append(args, "-f", codec)
	case "av1":
		args = append(args, "-f", "ivf")
	default:
		return nil, fmt.Errorf("unknown codec %q", codec)
	}
	args = append(args, "-i", file)
	w, h := 0, 0
	if bc != nil {
		w, h = proto.BarcodeCols*bc.Cell, proto.BarcodeRows*bc.Cell
		args = append(args, "-vf", fmt.Sprintf("crop=%d:%d:%d:%d,showinfo,format=gray", w, h, bc.X, bc.Y),
			"-fps_mode", "passthrough", "-f", "rawvideo", "-")
	} else {
		args = append(args, "-vf", "showinfo", "-fps_mode", "passthrough", "-f", "null", "-")
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := &Decoded{}
	var tail []string // the last lines, for an ffmpeg that fails
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if m := showinfoRe.FindStringSubmatch(line); m != nil {
				d.Frames = append(d.Frames, DecodedFrame{Key: m[1] == "1", Type: m[2], Barcode: -2})
				continue
			}
			if m := levelRe.FindStringSubmatch(line); m != nil {
				if m[1] == "warning" {
					d.Warnings++
				} else {
					d.Errors = append(d.Errors, strings.TrimSpace(line))
				}
			}
			if tail = append(tail, line); len(tail) > 5 {
				tail = tail[1:]
			}
		}
	}()
	var values []int
	if bc != nil {
		r := bufio.NewReaderSize(stdout, 1<<20)
		buf := make([]byte, w*h)
		for {
			if _, err := io.ReadFull(r, buf); err != nil {
				break
			}
			v, ok := proto.BarcodeReadLuma(buf, w, float64(bc.Cell))
			if ok {
				values = append(values, int(v))
			} else {
				values = append(values, -1)
			}
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	<-logDone
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(d.Frames) == 0 {
			return nil, fmt.Errorf("%s: %w: %s", ffmpeg, err, strings.Join(tail, " | "))
		}
		// Frames came out: a decoder that gave up part-way is a finding.
		d.Errors = append(d.Errors, "ffmpeg: "+err.Error())
	}
	if bc != nil {
		for i := range d.Frames {
			if i < len(values) {
				d.Frames[i].Barcode = values[i]
			}
		}
		if len(values) != len(d.Frames) {
			d.Errors = append(d.Errors, "ffmpeg: "+strconv.Itoa(len(values))+" barcode crops for "+strconv.Itoa(len(d.Frames))+" frames")
		}
	}
	return d, nil
}

// ffmpegWorks reports whether ffmpeg runs (for a clear skip reason).
func ffmpegWorks(ctx context.Context, ffmpeg string) error {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", ffmpeg, err)
	}
	if !bytes.HasPrefix(out, []byte("ffmpeg version")) {
		return fmt.Errorf("%s is not ffmpeg", ffmpeg)
	}
	return nil
}
