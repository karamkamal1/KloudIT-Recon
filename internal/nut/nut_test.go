package nut

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func haveFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

type probePacket struct {
	Pts   json.Number `json:"pts"`
	Size  json.Number `json:"size"`
	Flags string      `json:"flags"`
}

// TestAgainstFFprobe muxes real encoder output into NUT with ffmpeg and checks
// that every packet we demux matches ffprobe's view (size, pts, key flag) and
// that the payload is byte-identical to what ffmpeg's own demuxer returns.
func TestAgainstFFprobe(t *testing.T) {
	haveFFmpeg(t)
	cases := []struct {
		name string
		args []string
	}{
		{"h264", []string{"-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "45", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "20"}},
		{"h264-big", []string{"-f", "lavfi", "-i", "testsrc2=s=1280x720:r=30", "-frames:v", "10", "-c:v", "libx264", "-preset", "ultrafast", "-qp", "1"}},
		// Wall-clock capture timestamps in µs (the host's stage timestamps): pts
		// around 1.8e15 must survive syncpoint rescaling without overflow.
		{"h264-wallclock", []string{"-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-vf", "settb=AVTB,setpts=time(0)*1000000", "-frames:v", "45", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-enc_time_base", "1:1000000", "-fps_mode", "passthrough"}},
		{"hevc", []string{"-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "30", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=none:keyint=15:bframes=0"}},
		{"av1", []string{"-f", "lavfi", "-i", "testsrc2=s=320x240:r=30", "-frames:v", "20", "-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-g", "10"}},
		{"opus", []string{"-f", "lavfi", "-i", "sine=f=440:d=1", "-c:a", "libopus", "-b:a", "96k"}},
		{"av+audio", []string{"-f", "lavfi", "-i", "testsrc2=s=160x120:r=25", "-f", "lavfi", "-i", "sine=f=1000:d=1", "-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "libopus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "out.nut")
			args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, tc.args...)
			args = append(args, "-f", "nut", out)
			if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Skipf("ffmpeg cannot produce %s: %v %s", tc.name, err, b)
			}
			// Reference: ffprobe packet list per stream.
			pb, err := exec.Command("ffprobe", "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,pts,size,flags", "-of", "json", out).Output()
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Packets []struct {
					probePacket
					Stream int `json:"stream_index"`
				} `json:"packets"`
			}
			if err := json.Unmarshal(pb, &probe); err != nil {
				t.Fatal(err)
			}
			// Reference payload hashes via framemd5 (stream copy keeps packet data intact).
			mb, err := exec.Command("ffmpeg", "-v", "error", "-i", out, "-map", "0", "-c", "copy", "-f", "framemd5", "-").Output()
			if err != nil {
				t.Fatal(err)
			}
			var hashes []string
			for _, line := range strings.Split(string(mb), "\n") {
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				f := strings.Split(line, ",")
				hashes = append(hashes, strings.TrimSpace(f[len(f)-1]))
			}

			f, err := os.Open(out)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDemuxer(f, 64<<20)
			var got []*Packet
			for {
				p, err := d.ReadPacket()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("after %d packets: %v", len(got), err)
				}
				got = append(got, p)
			}
			if len(got) != len(probe.Packets) {
				t.Fatalf("packet count: got %d want %d", len(got), len(probe.Packets))
			}
			for i, p := range got {
				want := probe.Packets[i]
				if p.Stream != want.Stream {
					t.Fatalf("pkt %d stream %d want %d", i, p.Stream, want.Stream)
				}
				if strconv.Itoa(len(p.Data)) != want.Size.String() {
					t.Fatalf("pkt %d size %d want %s", i, len(p.Data), want.Size)
				}
				if strconv.FormatInt(p.Pts, 10) != want.Pts.String() {
					t.Fatalf("pkt %d pts %d want %s", i, p.Pts, want.Pts)
				}
				if st := d.Streams()[p.Stream]; tc.name == "h264-wallclock" && (p.PtsMicros(st) != p.Pts || p.Pts < 1e15) {
					t.Fatalf("pkt %d: %d µs is not a wall-clock time in µs (pts %d, time base %v)", i, p.PtsMicros(st), p.Pts, st.TimeBase)
				}
				if p.Key != strings.HasPrefix(want.Flags, "K") {
					t.Fatalf("pkt %d key %v flags %s", i, p.Key, want.Flags)
				}
				if i < len(hashes) {
					sum := md5.Sum(p.Data)
					if hex.EncodeToString(sum[:]) != hashes[i] {
						t.Fatalf("pkt %d payload md5 mismatch", i)
					}
				}
			}
			for _, s := range d.Streams() {
				if s == nil {
					t.Fatal("missing stream header")
				}
				if s.Class == ClassVideo && (s.Width == 0 || s.Height == 0) {
					t.Fatalf("stream %d has no dimensions", s.ID)
				}
			}
			t.Logf("%s: %d packets ok, streams=%d fourcc=%q", tc.name, len(got), len(d.Streams()), d.Streams()[0].FourCC)
		})
	}
}

// oneByteReader serves one byte per Read and blocks once limit bytes have been
// consumed, emulating a pipe whose writer has gone quiet.
type oneByteReader struct {
	data     []byte
	pos      int
	limit    int
	blocking chan struct{}
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= r.limit {
		<-r.blocking
		return 0, io.EOF
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// TestStreamingLatency verifies that a packet is returned as soon as its bytes
// are available, without waiting for any byte of the next packet.
func TestStreamingLatency(t *testing.T) {
	haveFFmpeg(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "s.nut")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=160x120:r=10",
		"-frames:v", "3", "-c:v", "libx264", "-preset", "ultrafast", "-f", "nut", out).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg: %v %s", err, b)
	}
	all, _ := os.ReadFile(out)
	// Pass 1: learn exactly how many bytes the first two packets need.
	counter := &oneByteReader{data: all, limit: len(all), blocking: make(chan struct{})}
	d := NewDemuxer(counter, 1<<20)
	var ends []int
	for i := 0; i < 2; i++ {
		if _, err := d.ReadPacket(); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, counter.pos)
	}
	// Pass 2: data stops right after the second packet; ReadPacket must not block.
	r := &oneByteReader{data: all, limit: ends[1], blocking: make(chan struct{})}
	defer close(r.blocking)
	d2 := NewDemuxer(r, 1<<20)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			if _, err := d2.ReadPacket(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demuxer blocked waiting for bytes beyond the packet")
	}
}

func TestVarint(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<35 + 7} {
		var enc []byte
		n := 1
		for x := v >> 7; x > 0; x >>= 7 {
			n++
		}
		for i := n - 1; i > 0; i-- {
			enc = append(enc, 0x80|byte(v>>(7*uint(i))))
		}
		enc = append(enc, byte(v&0x7f))
		got, err := readV(bytes.NewReader(enc))
		if err != nil || got != v {
			t.Fatalf("readV(%x) = %d,%v want %d", enc, got, err, v)
		}
	}
	for _, c := range []struct {
		in   uint64
		want int64
	}{{0, 0}, {1, 1}, {2, -1}, {3, 2}, {4, -2}} {
		got, _ := readS(bytes.NewReader([]byte{byte(c.in)}))
		if got != c.want {
			t.Fatalf("readS(%d)=%d want %d", c.in, got, c.want)
		}
	}
}
