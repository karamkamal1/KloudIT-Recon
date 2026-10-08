package media

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/thesyncim/gopus"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestOpusFrameMs: 5 ms frames below a 10 ms minimum RTT (LAN), 10 ms above
// 20 ms (WAN), the current duration in between and while unmeasured, 10 ms
// before anything is known.
func TestOpusFrameMs(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		cur  int
		rtt  time.Duration
		want int
	}{
		{0, 0, OpusFrameWAN},   // session start, nothing measured: 10 ms as before step 4.6
		{0, -ms, OpusFrameWAN}, // nonsense RTT: as unmeasured
		{OpusFrameWAN, 0, OpusFrameWAN},
		{OpusFrameLAN, 0, OpusFrameLAN},           // a client that stopped reporting keeps what it had
		{7, 0, OpusFrameWAN},                      // not a frame duration this host uses
		{0, 300 * time.Microsecond, OpusFrameLAN}, // wired LAN, loopback
		{OpusFrameWAN, 4 * ms, OpusFrameLAN},      // Wi-Fi LAN
		{OpusFrameWAN, 9900 * time.Microsecond, OpusFrameLAN},
		{OpusFrameWAN, 10 * ms, OpusFrameWAN}, // band: stays
		{OpusFrameLAN, 10 * ms, OpusFrameLAN}, // band: stays
		{OpusFrameLAN, 20 * ms, OpusFrameLAN}, // band (inclusive): stays
		{OpusFrameWAN, 20 * ms, OpusFrameWAN},
		{0, 15 * ms, OpusFrameWAN}, // unknown start in the band: the WAN default
		{OpusFrameLAN, 20*ms + time.Microsecond, OpusFrameWAN},
		{OpusFrameLAN, 40 * ms, OpusFrameWAN}, // netem "wan" profile
		{OpusFrameWAN, 150 * ms, OpusFrameWAN},
	} {
		if got := OpusFrameMs(c.cur, c.rtt); got != c.want {
			t.Errorf("OpusFrameMs(%d, %v) = %d, want %d", c.cur, c.rtt, got, c.want)
		}
	}
}

// chanSource feeds the chunks a test sends, as fast as it sends them.
type chanSource chan []float32

func (chanSource) Name() string { return "test-chan" }

func (c chanSource) Run(ctx context.Context, sink func([]float32)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case s := <-c:
			sink(s)
		}
	}
}

// TestAudioFrameSwitch encodes with 10 ms frames, switches to 5 ms and back
// while running, and checks every packet: its pts follows the previous
// packet's duration (no samples lost or repeated), its Opus TOC says the
// duration the encoder used, and a decoder decodes it to that many samples.
func TestAudioFrameSwitch(t *testing.T) {
	// The chunks come in bursts, with waits between them that a loaded
	// machine can stretch: no source pauses here (TestAudioSourcePause).
	defer func(d time.Duration) { sourcePause = d }(sourcePause)
	sourcePause = time.Hour
	src := make(chanSource)
	a := NewAudio(src, AudioConfig{Codec: "opus", BitrateKbps: 128}, nil)
	if a.FrameMs() != OpusFrameWAN {
		t.Fatalf("default frame %d ms", a.FrameMs())
	}
	pkts := make(chan []byte, 256)
	if err := a.Start(func(p []byte) { pkts <- append([]byte(nil), p...) }); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	chunk := func(ms int) []float32 { // a sawtooth, interleaved stereo
		s := make([]float32, 48*ms*2)
		for i := range s {
			s[i] = float32(0.3 * float64((i/2)%109) / 109)
		}
		return s
	}
	var want []int // samples per packet, in order
	feed := func(ms, n int) {
		for i := 0; i < n; i++ {
			src <- chunk(ms)
		}
	}
	feed(10, 3) // 3 × 10 ms frames
	want = append(want, 480, 480, 480)
	// Synchronise: the switch takes effect at the next frame boundary after
	// these packets were cut.
	recv := func(n int) [][]byte {
		var out [][]byte
		for len(out) < n {
			select {
			case p := <-pkts:
				out = append(out, p)
			case <-time.After(2 * time.Second):
				t.Fatalf("got %d of %d packets", len(out), n)
			}
		}
		return out
	}
	got := recv(3)
	if !a.SetFrameMs(OpusFrameLAN) || a.SetFrameMs(OpusFrameLAN) || a.FrameMs() != OpusFrameLAN {
		t.Fatal("SetFrameMs reports a change only when the duration changes")
	}
	if a.SetFrameMs(20) || a.FrameMs() != OpusFrameLAN {
		t.Fatal("SetFrameMs accepted 20 ms")
	}
	feed(10, 2) // 4 × 5 ms frames
	want = append(want, 240, 240, 240, 240)
	got = append(got, recv(4)...)
	a.SetFrameMs(OpusFrameWAN)
	feed(5, 4) // 2 × 10 ms frames
	want = append(want, 480, 480)
	got = append(got, recv(2)...)

	dec, err := gopus.NewDecoder(gopus.DefaultDecoderConfig(48000, 2))
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]float32, 5760*2)
	var pts uint32
	for i, p := range got {
		if p[0] != proto.DgAudio || p[1] != proto.AudioCodecOpus || int(binary.LittleEndian.Uint16(p[2:])) != i {
			t.Fatalf("packet %d header % x", i, p[:8])
		}
		if gotPts := binary.LittleEndian.Uint32(p[4:]); gotPts != pts {
			t.Fatalf("packet %d pts %d, want %d", i, gotPts, pts)
		}
		info, err := gopus.ParsePacket(p[8:])
		if err != nil || info.TOC.FrameSize*info.FrameCount != want[i] {
			t.Fatalf("packet %d: TOC %+v × %d frames (%v), want %d samples", i, info.TOC, info.FrameCount, err, want[i])
		}
		if n, err := dec.Decode(p[8:], pcm); err != nil || n != want[i] {
			t.Fatalf("packet %d decodes to %d samples (%v), want %d", i, n, err, want[i])
		}
		pts += uint32(want[i])
	}

	pcmA := NewAudio(src, AudioConfig{Codec: "pcm", FrameMs: OpusFrameWAN}, nil)
	if pcmA.FrameMs() != 5 || pcmA.SetFrameMs(OpusFrameWAN) || pcmA.FrameMs() != 5 {
		t.Fatal("PCM packets are always 5 ms")
	}
	if NewAudio(src, AudioConfig{FrameMs: OpusFrameLAN}, nil).FrameMs() != OpusFrameLAN {
		t.Fatal("AudioConfig.FrameMs not used at the start")
	}
}

// TestAudioSourcePause: a source that delivers nothing for a while (WASAPI
// loopback while nothing plays) moves the pts on by the pause, so the client
// can tell it from late packets; the sequence goes on, and a partial frame
// from before the pause is dropped. Back-to-back chunks (a source ahead of
// real time) move nothing.
func TestAudioSourcePause(t *testing.T) {
	src := make(chanSource)
	a := NewAudio(src, AudioConfig{Codec: "opus", BitrateKbps: 128}, nil)
	pkts := make(chan []byte, 64)
	if err := a.Start(func(p []byte) { pkts <- append([]byte(nil), p...) }); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	chunk := func(frames int) []float32 { return make([]float32, frames*2) }
	recv := func() []byte {
		select {
		case p := <-pkts:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no packet")
			return nil
		}
	}
	for i := 0; i < 3; i++ {
		src <- chunk(480)
	}
	src <- chunk(336) // 7 ms: no packet, left over at the pause
	for i := 0; i < 3; i++ {
		if p := recv(); binary.LittleEndian.Uint32(p[4:]) != uint32(480*i) {
			t.Fatalf("packet %d pts %d", i, binary.LittleEndian.Uint32(p[4:]))
		}
	}
	const pause = 200 * time.Millisecond
	time.Sleep(pause)
	src <- chunk(480)
	src <- chunk(480)
	p3, p4 := recv(), recv()
	if seq := binary.LittleEndian.Uint16(p3[2:]); seq != 3 || binary.LittleEndian.Uint16(p4[2:]) != 4 {
		t.Fatalf("sequence after the pause: %d, %d", seq, binary.LittleEndian.Uint16(p4[2:]))
	}
	// 1440 + the dropped 336 + the pause less the chunk's own 10 ms (a
	// loaded machine stretches the sleep).
	jump := int(binary.LittleEndian.Uint32(p3[4:])) - 1440 - 336
	if lo := 48 * int((pause - 10*time.Millisecond).Milliseconds()); jump < lo || jump > lo+48*300 {
		t.Fatalf("pts moved on by %d samples (%.1f ms) after a %v pause", jump, float64(jump)/48, pause)
	}
	if d := binary.LittleEndian.Uint32(p4[4:]) - binary.LittleEndian.Uint32(p3[4:]); d != 480 {
		t.Fatalf("pts step after the pause %d", d)
	}
}

// TestAudioCaptureMs: the capture packet duration is measured from the
// source's chunks (rounded, reported on a change), and 5 ms Opus frames are
// picked on a LAN only while it is at most 5 ms: a source delivering 10 ms at
// a time would send two 5 ms packets at once, saving nothing.
func TestAudioCaptureMs(t *testing.T) {
	defer func(d time.Duration) { sourcePause = d }(sourcePause)
	sourcePause = time.Hour
	src := make(chanSource)
	changed := make(chan int, 8)
	var a *Audio
	a = NewAudio(src, AudioConfig{Codec: "opus", BitrateKbps: 128, CaptureChanged: func() { changed <- a.CaptureMs() }}, nil)
	lan := time.Millisecond
	if a.CaptureMs() != 0 || a.PickFrameMs(lan) != OpusFrameWAN {
		t.Fatalf("before the first capture packet: %d ms, pick %d", a.CaptureMs(), a.PickFrameMs(lan))
	}
	if err := a.Start(func([]byte) {}); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	wait := func(want int) {
		t.Helper()
		select {
		case got := <-changed:
			if got != want {
				t.Fatalf("capture %d ms, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no change to %d ms reported", want)
		}
	}
	src <- make([]float32, 480*2) // WASAPI shared mode: one engine period
	wait(10)
	src <- make([]float32, 481*2) // a resampled period: still 10 ms
	src <- make([]float32, 479*2)
	if a.PickFrameMs(lan) != OpusFrameWAN || a.PickFrameMs(40*lan) != OpusFrameWAN {
		t.Fatal("5 ms frames picked for 10 ms capture packets")
	}
	src <- make([]float32, 240*2)
	wait(5)
	if len(changed) != 0 {
		t.Fatalf("changes reported for the same duration: %d", <-changed)
	}
	for _, c := range []struct {
		rtt  time.Duration
		want int
	}{{0, OpusFrameWAN}, {lan, OpusFrameLAN}, {15 * lan, OpusFrameWAN}, {40 * lan, OpusFrameWAN}} {
		if got := a.PickFrameMs(c.rtt); got != c.want {
			t.Errorf("5 ms capture, RTT %v: pick %d, want %d", c.rtt, got, c.want)
		}
	}
	a.SetFrameMs(OpusFrameLAN)
	if a.PickFrameMs(15*lan) != OpusFrameLAN { // between the bounds: stays
		t.Error("5 ms frames not kept between the bounds")
	}
}

// TestAudioPacketSamplesJS: web/static/js/protocol.js reads a packet's
// duration as gopus does, for the host's own packets at both frame durations
// and for every TOC config and frame-count code (needs node).
func TestAudioPacketSamplesJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "..", "web", "static", "js", "protocol.js")

	type vec struct {
		Hex  string `json:"hex"`
		Want int    `json:"want"`
	}
	var vecs []vec
	add := func(dg []byte, want int) { vecs = append(vecs, vec{hex.EncodeToString(dg), want}) }
	for _, ms := range []int{OpusFrameLAN, OpusFrameWAN} {
		enc, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: 48000, Channels: 2, Application: gopus.ApplicationLowDelay})
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.SetFrameSize(48 * ms); err != nil {
			t.Fatal(err)
		}
		out := make([]byte, 4000)
		n, err := enc.Encode(make([]float32, 48*ms*2), out)
		if err != nil {
			t.Fatal(err)
		}
		add(proto.AudioPacket(nil, proto.AudioCodecOpus, 1, 0, out[:n]), 48*ms)
	}
	add(proto.AudioPacket(nil, proto.AudioCodecPCM, 1, 0, make([]byte, 240*4)), 240)
	add([]byte{proto.DgAudio, proto.AudioCodecOpus, 0, 0, 0, 0, 0, 0}, 0) // no payload
	add([]byte{proto.DgAudio, 9, 0, 0, 0, 0, 0, 0, 0}, 0)                 // unknown codec
	for toc := 0; toc < 256; toc++ {
		payload := []byte{byte(toc), 3, 0, 0, 0, 0}
		frames := map[int]int{0: 1, 1: 2, 2: 2, 3: 3}[toc&3]
		info := gopus.ParseTOC(byte(toc))
		add(proto.AudioPacket(nil, proto.AudioCodecOpus, 1, 0, payload), info.FrameSize*frames)
	}
	in, _ := json.Marshal(vecs)
	script := `
const P = await import(process.argv[1]);
const vecs = JSON.parse(process.argv[2]);
console.log(JSON.stringify(vecs.map((v) => P.audioPacketSamples(Uint8Array.from(v.hex.match(/../g) || [], (x) => parseInt(x, 16))))));`
	b, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js), string(in)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got []int
	if err := json.Unmarshal(b, &got); err != nil || len(got) != len(vecs) {
		t.Fatalf("%v: %s", err, b)
	}
	for i, v := range vecs {
		if got[i] != v.Want {
			t.Errorf("datagram %s: protocol.js %d samples, want %d", v.Hex[:min(len(v.Hex), 24)], got[i], v.Want)
		}
	}
}
