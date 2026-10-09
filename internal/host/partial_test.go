package host

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// waitStreams waits for n frame streams that are done (closed or reset).
func waitStreams(t *testing.T, c *fakeConn, n int) []streamState {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		st := c.snapshot()
		done := 0
		for _, x := range st {
			if x.closed || x.cancelled {
				done++
			}
		}
		if done >= n {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d frame streams done: %+v", done, n, st)
		}
	}
}

// TestFrameSenderPartialDelivery (GUIDE 2.4): with a client whose QUIC
// endpoint negotiated RESET_STREAM_AT, every frame stream marks its header
// (with the extension) reliable before the payload goes out, a key frame
// also its parameter sets, so a frame cancelled at its deadline (rung 1: its
// payload write held back) still delivers its header, and so do the test
// hook's half-written frames. Without the extension the streams are written
// as before: no boundary, and a cancelled frame delivers nothing.
func TestFrameSenderPartialDelivery(t *testing.T) {
	ps := codec.JoinAnnexB([]byte{0x40, 0x01, 0x0c}, []byte{0x42, 0x01, 0x01}, []byte{0x44, 0x01, 0xc1}) // VPS, SPS, PPS
	idr := append(append([]byte(nil), ps...), codec.JoinAnnexB(append([]byte{0x26, 0x01}, bytes.Repeat([]byte{0xaf}, 100)...))...)
	payload := func(seq uint32) []byte { return bytes.Repeat([]byte{byte(seq)}, 100) }
	// hdrLen: the header and extension of a stream that went out whole.
	hdrLen := func(t *testing.T, x streamState) int {
		t.Helper()
		_, _, p, err := proto.ParseFrame(x.data)
		if err != nil {
			t.Fatalf("frame stream: %v", err)
		}
		return len(x.data) - len(p)
	}
	// header checks what a cancelled stream delivers: nothing, or (partial)
	// exactly the header of frame seq.
	header := func(t *testing.T, x streamState, partial bool, seq uint32) {
		t.Helper()
		got := x.delivered(partial)
		if !partial {
			if got != nil || x.boundaries != 0 {
				t.Fatalf("seq %d without partial delivery: %d bytes delivered, %d boundaries", seq, len(got), x.boundaries)
			}
			return
		}
		h, _, p, err := proto.ParseFrame(got)
		if err != nil || h.Seq != seq || len(p) != 0 || x.boundaries != 1 {
			t.Fatalf("seq %d: cancelled stream delivers %d bytes (%v, seq %d, %d payload bytes, %d boundaries), want its header alone",
				seq, len(got), err, h.Seq, len(p), x.boundaries)
		}
	}

	for _, partial := range []bool{true, false} {
		t.Run(fmt.Sprintf("rung 1, partial=%v", partial), func(t *testing.T) {
			s, c, _ := testSession(t, testFaults{})
			s.hello.V = proto.HelloVersionFrameExt
			c.partial, s.partial = partial, partial
			c.stall = map[int]bool{2: true}
			logs := &lockedLog{}
			s.log = slog.New(slog.NewTextHandler(logs, nil))
			s.video = &ladderPipeline{caps: media.PipelineCaps{ForceIDR: true, Recovery: proto.RecoveryInvalidate}, events: make(chan media.VideoEvent)}
			s.healConfig(&proto.VideoConfig{Gen: 1, Family: codec.HEVC, Recovery: proto.RecoveryInvalidate}, 0)
			s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60}) // deadline 33 ms
			go s.frameSender()
			for seq := uint32(0); seq < 5; seq++ { // seq 2 stalls; 3 and 4 are newer
				f := &media.Frame{Gen: 1, Seq: seq, Key: seq == 0, Data: payload(seq)}
				if f.Key {
					f.Data = idr
				}
				s.frameQ <- f
				s.checkOut()
			}
			st := waitStreams(t, c, 3)
			if !st[0].closed || !st[1].closed || !st[2].cancelled {
				t.Fatalf("streams: %+v", st[:3])
			}
			h0, h1 := hdrLen(t, st[0]), hdrLen(t, st[1])
			if partial {
				if st[0].boundary != h0+len(ps) || st[0].boundaries != 1 {
					t.Errorf("key frame: boundary at %d (%d calls), want its header %d + parameter sets %d", st[0].boundary, st[0].boundaries, h0, len(ps))
				}
				if st[1].boundary != h1 || st[1].boundaries != 1 {
					t.Errorf("P-frame: boundary at %d (%d calls), want its header %d", st[1].boundary, st[1].boundaries, h1)
				}
			} else if st[0].boundaries+st[1].boundaries != 0 {
				t.Errorf("boundaries set without partial delivery: %+v", st[:2])
			}
			header(t, st[2], partial, 2)
			line := logs.lines(`msg="frame stream cancelled" gen=1 seq=2`)
			want := fmt.Sprintf("reliable_bytes=%d ", map[bool]int{true: h1, false: 0}[partial])
			if len(line) != 1 || !strings.Contains(line[0], want) {
				t.Errorf("cancel log %q, want %s", line, want)
			}
		})
		t.Run(fmt.Sprintf("test hook, partial=%v", partial), func(t *testing.T) {
			// As TestFrameSenderFaults: every 3rd frame late, every 5th
			// fails mid-stream.
			s, c, _ := testSession(t, testFaults{delayEvery: 3, delay: 50 * time.Millisecond, dropEvery: 5})
			s.hello.V = proto.HelloVersionFrameExt
			c.partial, s.partial = partial, partial
			go s.frameSender()
			for seq := uint32(0); seq < 10; seq++ {
				s.frameQ <- &media.Frame{Gen: 4, Seq: seq, Data: payload(seq)}
			}
			st := waitStreams(t, c, 10)
			for i, x := range st {
				seq := uint32(i)
				if (i+1)%5 == 0 {
					if !x.cancelled {
						t.Fatalf("seq %d: want a reset, got %+v", seq, x)
					}
					header(t, x, partial, seq)
					continue
				}
				h, _, p, err := proto.ParseFrame(x.delivered(partial))
				if err != nil || h.Seq != seq || !bytes.Equal(p, payload(seq)) {
					t.Fatalf("seq %d: delivered %d bytes (%v)", seq, len(x.data), err)
				}
				if want := map[bool]int{true: 1, false: 0}[partial]; x.boundaries != want || (partial && x.boundary != hdrLen(t, x)) {
					t.Errorf("seq %d: boundary at %d in %d calls, want the header (%d) in %d", seq, x.boundary, x.boundaries, hdrLen(t, x), want)
				}
			}
		})
	}
}

// A frame stream the ladder cancels between the write of its reliable
// prefix and the boundary (checkOut runs at stream deadlines and on the
// control and video event goroutines) is reset with nothing marked and
// nothing more written. A boundary after the reset would break quic-go's
// bookkeeping of the RESET_STREAM_AT it queued: a lost reset not sent again,
// a stream that never completes, or (nothing marked before) a panic on the
// ACK of its data. The cancel log says what the reset delivers: nothing.
func TestFrameSenderPartialCancelBeforeBoundary(t *testing.T) {
	s, c, _ := testSession(t, testFaults{})
	s.hello.V = proto.HelloVersionFrameExt
	c.partial, s.partial = true, true
	logs := &lockedLog{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	s.video = &ladderPipeline{caps: media.PipelineCaps{ForceIDR: true, Recovery: proto.RecoveryInvalidate}, events: make(chan media.VideoEvent)}
	s.healConfig(&proto.VideoConfig{Gen: 1, Family: codec.HEVC, Recovery: proto.RecoveryInvalidate}, 0)
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60}) // deadline 33 ms
	payload := func(seq uint32) []byte { return bytes.Repeat([]byte{byte(seq)}, 100) }
	c.afterWrite = map[int]func(){1: func() {
		// Frame 1's header is written, not yet marked: the frame is past
		// its deadline and a newer one is ready (rung 1).
		time.Sleep(50 * time.Millisecond)
		s.frameQ <- &media.Frame{Gen: 1, Seq: 2, Data: payload(2)}
		s.checkOut()
	}}
	go s.frameSender()
	for seq := uint32(0); seq < 2; seq++ {
		s.frameQ <- &media.Frame{Gen: 1, Seq: seq, Data: payload(seq)}
	}
	st := waitStreams(t, c, 2)
	if !st[0].closed || st[0].boundaries != 1 {
		t.Fatalf("frame 0: %+v", st[0])
	}
	x := st[1]
	if !x.cancelled || x.late != 0 || x.boundaries != 0 {
		t.Fatalf("frame 1 cancelled after its header was written: cancelled %v, %d boundaries, %d after the reset; want a reset and none",
			x.cancelled, x.boundaries, x.late)
	}
	if h, _, p, err := proto.ParseFrame(x.data); err != nil || h.Seq != 1 || len(p) != 0 {
		t.Errorf("frame 1: %d bytes written (%v, %d payload bytes), want its header alone", len(x.data), err, len(p))
	}
	line := logs.lines(`msg="frame stream cancelled" gen=1 seq=1`)
	if len(line) != 1 || !strings.Contains(line[0], "reliable_bytes=0 ") {
		t.Errorf("cancel log %q, want reliable_bytes=0", line)
	}
}

// reliablePrefix: a P-frame's header alone; a key frame's header and its
// generation's parameter sets, by the family of the generation it belongs
// to (the config of a newer generation may come before its frames are sent).
func TestReliablePrefix(t *testing.T) {
	s, _, _ := testSession(t, testFaults{})
	h264 := codec.JoinAnnexB([]byte{0x09, 0xf0}, []byte{0x67, 0x64, 0x00, 0x1f}, []byte{0x68, 0xee}, []byte{0x65, 0x88, 0x84, 0x21})
	s.healConfig(&proto.VideoConfig{Gen: 3, Family: codec.H264}, 0)
	s.healConfig(&proto.VideoConfig{Gen: 4, Family: codec.AV1}, 0)
	for _, tc := range []struct {
		f    media.Frame
		want int
	}{
		{media.Frame{Gen: 3, Seq: 0, Key: true, Data: h264}, 30 + len(h264) - 8},
		{media.Frame{Gen: 3, Seq: 1, Data: h264}, 30},
		{media.Frame{Gen: 4, Seq: 0, Key: true, Data: h264}, 30}, // AV1 parses nothing out of it
		{media.Frame{Gen: 5, Seq: 0, Key: true, Data: h264}, 30}, // no config seen
	} {
		if got := s.reliablePrefix(&tc.f, 30); got != tc.want {
			t.Errorf("gen %d seq %d key %v: %d, want %d", tc.f.Gen, tc.f.Seq, tc.f.Key, got, tc.want)
		}
	}
}
