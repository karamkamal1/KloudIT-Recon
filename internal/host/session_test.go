package host

import (
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestVideoHeader checks both meanings of send_us: v1 clients keep the
// encoder-out time (their congestion detection, acks and latency readout
// measure from it), v2 clients get the transport hand-off plus the extension.
func TestVideoHeader(t *testing.T) {
	f := &media.Frame{Gen: 3, Seq: 7, Key: true, PtsUs: 1000, CaptureUs: 900, EncodeDoneUs: 5000}
	const now = 7500 // frame waited 2.5 ms in the host queue

	h, ext := videoHeader(f, 1, now)
	if h.SendUs != f.EncodeDoneUs || h.Flags != proto.FrameFlagKey || !ext.Empty() {
		t.Fatalf("v1: send %d flags %#x ext %v, want send = encodeDone %d, key only", h.SendUs, h.Flags, !ext.Empty(), f.EncodeDoneUs)
	}

	h, ext = videoHeader(f, proto.HelloVersionFrameExt, now)
	done, _ := ext.Get(proto.ExtEncodeDoneUs)
	capture, _ := ext.Get(proto.ExtCaptureUs)
	if h.SendUs != now || h.Flags != proto.FrameFlagKey|proto.FrameFlagExt || done != f.EncodeDoneUs || capture != f.CaptureUs {
		t.Fatalf("v2: send %d flags %#x encodeDone %d capture %d", h.SendUs, h.Flags, done, capture)
	}

	f.CaptureUs = 0
	_, ext = videoHeader(f, proto.HelloVersionFrameExt, now)
	if _, ok := ext.Get(proto.ExtCaptureUs); ok {
		t.Fatal("capture tag sent without a capture stamp")
	}
}

// TestHostStages checks the host's own stage window: only acknowledged frames
// (once each), 10 s by ack time, and the client's percentile definition.
func TestHostStages(t *testing.T) {
	var h hostStages
	var now uint64
	for i := uint64(0); i < 100; i++ {
		now = 1_000_000 + i*200_000
		f := &media.Frame{Gen: 1, Seq: uint32(i), EncodeDoneUs: now, CaptureUs: now - (i+1)*100}
		if i%10 == 0 {
			f.CaptureUs = 0
		}
		h.sentFrame(f, now+60)
		if i%2 == 1 {
			continue // not acknowledged (dropped by the client)
		}
		h.acked(1, uint32(i), now+1000)
		h.acked(1, uint32(i), now+1000) // duplicate
	}
	h.acked(2, 98, now+1000) // other generation
	// Window: frames acknowledged in the last 10 s: even i = 50..98 (25 frames),
	// 20 with a capture stamp ((i+1)/10 ms: 5.3, 5.5, ... 9.9 without i%10 == 0).
	c, q := h.summary(now + 1000)
	if c != "7.7/9.9/9.9 n=20" || q != "0.1/0.1/0.1 n=25" {
		t.Fatalf("capture %q queue %q", c, q)
	}
}
