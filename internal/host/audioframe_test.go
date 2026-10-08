package host

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// audioConfigs returns the audio configs the host sent on the control stream.
func (c *fakeCtrl) audioConfigs(t *testing.T) []proto.AudioConfig {
	t.Helper()
	c.mu.Lock()
	r := bytes.NewReader(c.buf.Bytes())
	c.mu.Unlock()
	var out []proto.AudioConfig
	for {
		b, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		var m proto.AudioConfig
		if json.Unmarshal(b, &m) == nil && m.T == "audio" {
			out = append(out, m)
		}
	}
}

// ptsSteps returns the pts step (samples) between consecutive audio packets
// sent so far, and the number of packets.
func (c *dgConn) ptsSteps() ([]uint32, int) {
	pkts, _ := c.of(proto.DgAudio)
	var steps []uint32
	for i := 1; i < len(pkts); i++ {
		steps = append(steps, binary.LittleEndian.Uint32(pkts[i][4:])-binary.LittleEndian.Uint32(pkts[i-1][4:]))
	}
	return steps, len(pkts)
}

// TestAudioFrameFromRTT: the session starts Opus with 10 ms frames, switches
// to 5 ms when a ping reports a LAN round-trip time (the test tone delivers
// 5 ms capture packets), back to 10 ms for a WAN one, keeps the duration for
// one between the bounds and for pings without one (clients before step
// 4.6), and announces each switch in an audio config of the same stream.
func TestAudioFrameFromRTT(t *testing.T) {
	s, c, ctrl := dgSession(t, testFaults{})
	s.a.audioSource = media.ToneSource{}
	s.hello.Audio = proto.AudioCaps{Opus: true, PCM: true}
	go s.datagrams()
	go s.audioFrameLoop()
	s.startAudio()
	defer s.stopAudio()

	ping := func(id uint32, rtt time.Duration) {
		b := proto.PingDatagram(id, 1, uint32(rtt.Microseconds()))
		if rtt < 0 {
			b = b[:16] // a client before step 4.6
		}
		c.in <- b
	}
	waitFrame := func(ms int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			cfgs := ctrl.audioConfigs(t)
			if n := len(cfgs); n > 0 && cfgs[n-1].FrameMs == ms {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no audio config with %d ms frames: %+v", ms, ctrl.audioConfigs(t))
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Packets of the new duration follow from the next frame on.
		want := uint32(48 * ms)
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			steps, _ := c.ptsSteps()
			n := len(steps)
			if n >= 3 && steps[n-1] == want && steps[n-2] == want && steps[n-3] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("packets not %d ms after the switch: pts steps %v", ms, steps[max(0, n-10):])
			}
		}
	}

	cfgs := ctrl.audioConfigs(t)
	if len(cfgs) != 1 || cfgs[0].FrameMs != media.OpusFrameWAN || cfgs[0].Codec != "opus" || !cfgs[0].Enabled || cfgs[0].SameStream {
		t.Fatalf("start: %+v, want a new opus stream with %d ms frames until the RTT is known", cfgs, media.OpusFrameWAN)
	}
	time.Sleep(100 * time.Millisecond)
	if steps, n := c.ptsSteps(); n < 5 || steps[0] != 480 {
		t.Fatalf("start: %d packets, pts steps %v", n, steps)
	}

	ping(1, -1) // no RTT: nothing changes
	ping(2, 0)  // not measured yet: nothing changes
	time.Sleep(100 * time.Millisecond)
	if n := len(ctrl.audioConfigs(t)); n != 1 {
		t.Fatalf("a ping without an RTT changed the audio config (%d configs)", n)
	}
	if pongs, _ := c.of(proto.DgPong); len(pongs) != 2 {
		t.Fatalf("%d pongs, want 2", len(pongs))
	}

	ping(3, 800*time.Microsecond) // loopback / wired LAN
	waitFrame(media.OpusFrameLAN)
	ping(4, 15*time.Millisecond) // between the bounds: stays
	ping(5, 14*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if cfgs := ctrl.audioConfigs(t); len(cfgs) != 2 {
		t.Fatalf("an RTT between the bounds switched: %+v", cfgs)
	}
	ping(6, 40*time.Millisecond) // the netem "wan" profile
	waitFrame(media.OpusFrameWAN)
	ping(7, 12*time.Millisecond) // between the bounds again: stays at 10 ms
	time.Sleep(100 * time.Millisecond)
	cfgs = ctrl.audioConfigs(t)
	if len(cfgs) != 3 || cfgs[2].FrameMs != media.OpusFrameWAN || !cfgs[1].SameStream || !cfgs[2].SameStream {
		t.Fatalf("configs %+v", cfgs)
	}
	// Every pts step is one of the two frame durations: no samples lost or
	// repeated at a switch. (The test tone held up for 50 ms or more on a
	// loaded machine counts as a source pause: the pts moves on by it.)
	steps, _ := c.ptsSteps()
	for i, st := range steps {
		if st != 240 && st != 480 && st < 48*50 {
			t.Fatalf("pts step %d = %d", i, st)
		}
	}

	// A restart of audio (settings change) is a new stream: 10 ms frames,
	// then the duration the last RTT asks for once the capture packets
	// are known.
	ping(8, 2*time.Millisecond)
	waitFrame(media.OpusFrameLAN)
	s.stopAudio()
	s.startAudio()
	cfgs = ctrl.audioConfigs(t)
	n := len(cfgs)
	if last := cfgs[n-1]; last.FrameMs != media.OpusFrameWAN || last.SameStream {
		t.Fatalf("restart: %+v, want a new stream with %d ms frames", last, media.OpusFrameWAN)
	}
	waitFrame(media.OpusFrameLAN)
	if cfgs = ctrl.audioConfigs(t); len(cfgs) != n+1 || !cfgs[n].SameStream {
		t.Fatalf("after the restart: %+v", cfgs[n-1:])
	}
}

// chunkSource delivers frames-sample chunks of silence in real time, as
// WASAPI shared-mode loopback delivers one engine period (480: 10 ms) at a
// time.
type chunkSource struct{ frames int }

func (chunkSource) Name() string { return "test-chunks" }

func (c chunkSource) Run(ctx context.Context, sink func([]float32)) error {
	buf := make([]float32, c.frames*2)
	t := time.NewTicker(time.Duration(c.frames) * time.Second / 48000)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			sink(buf)
		}
	}
}

// TestAudioFrameCapture: with 10 ms capture packets a LAN RTT keeps 10 ms
// frames (5 ms ones would go out in pairs, saving nothing).
func TestAudioFrameCapture(t *testing.T) {
	s, c, ctrl := dgSession(t, testFaults{})
	s.a.audioSource = chunkSource{frames: 480}
	s.hello.Audio = proto.AudioCaps{Opus: true, PCM: true}
	go s.datagrams()
	go s.audioFrameLoop()
	s.startAudio()
	defer s.stopAudio()
	c.in <- proto.PingDatagram(1, 1, 800)
	time.Sleep(200 * time.Millisecond)
	cfgs := ctrl.audioConfigs(t)
	if len(cfgs) != 1 || cfgs[0].FrameMs != media.OpusFrameWAN || s.audio.CaptureMs() != 10 {
		t.Fatalf("configs %+v, capture %d ms", cfgs, s.audio.CaptureMs())
	}
	if steps, n := c.ptsSteps(); n < 5 || steps[len(steps)-1] != 480 {
		t.Fatalf("%d packets, pts steps %v", n, steps)
	}
}

// TestAudioFramePCM: PCM keeps its 5 ms packets whatever the RTT.
func TestAudioFramePCM(t *testing.T) {
	s, c, ctrl := dgSession(t, testFaults{})
	s.a.audioSource = media.ToneSource{}
	s.hello.Audio = proto.AudioCaps{PCM: true}
	go s.datagrams()
	go s.audioFrameLoop()
	s.startAudio()
	defer s.stopAudio()
	c.in <- proto.PingDatagram(1, 1, 40_000)
	time.Sleep(150 * time.Millisecond)
	cfgs := ctrl.audioConfigs(t)
	if len(cfgs) != 1 || cfgs[0].Codec != "pcm" || cfgs[0].FrameMs != 5 {
		t.Fatalf("configs %+v", cfgs)
	}
	if steps, n := c.ptsSteps(); n < 5 || steps[len(steps)-1] != 240 {
		t.Fatalf("%d packets, pts steps %v", n, steps)
	}
}
