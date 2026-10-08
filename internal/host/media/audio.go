package media

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesyncim/gopus"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// AudioSource produces interleaved stereo float32 samples at 48 kHz.
type AudioSource interface {
	// Run captures until ctx is cancelled or an error occurs, calling sink with
	// chunks of arbitrary size. sink must not retain the slice.
	Run(ctx context.Context, sink func([]float32)) error
	Name() string
}

const (
	audioRate     = 48000
	audioChannels = 2
)

// AudioConfig selects the codec.
type AudioConfig struct {
	Codec       string // opus | pcm
	BitrateKbps int    // opus only
	// FrameMs is the Opus frame duration at the start, OpusFrameLAN or
	// OpusFrameWAN (anything else: OpusFrameWAN); SetFrameMs changes it
	// while audio runs. PCM packets are always 5 ms.
	FrameMs int
	// CaptureChanged, if set, is called (from the capture goroutine) when
	// the duration of the source's capture packets changes (CaptureMs).
	CaptureChanged func()
}

// Opus frame durations in ms (step 4.6). The host collects a whole frame
// before it encodes it, and the client's jitter buffer must hold at least
// what arrives at once, so 5 ms frames can take up to 10 ms off the audio
// path compared with 10 ms ones, but only when the source delivers its audio
// at least every 5 ms. A source that delivers 10 ms at a time (WASAPI
// shared-mode loopback: one packet per audio engine period, 10 ms by
// default) makes two 5 ms packets go out together, at the moment one 10 ms
// packet would: no audio leaves earlier and the client still sees a 10 ms
// arrival cadence, only the packet rate doubles. So 5 ms frames are used on a
// LAN only while the capture packets are at most 5 ms (Audio.PickFrameMs).
// Over a WAN the saving is a small share of the latency, while 5 ms frames
// double the packet rate (twice the header overhead, a loss pattern of more
// and shorter gaps) and code less efficiently, so 10 ms frames are used
// there, and until the link and the capture are measured.
const (
	OpusFrameLAN = 5
	OpusFrameWAN = 10
)

// sourcePause is how long a source must deliver nothing for the host to treat
// it as paused rather than late: WASAPI loopback delivers nothing while
// nothing plays. The pts then moves on by the pause, so the client can tell
// the end of a sound from packets the network held up (its jitter buffer
// runs dry either way). Well above the capture period (10 ms) and buffer
// (20 ms): a capture thread held up for longer than those loses audio, which
// leaves a gap in the timeline too.
var sourcePause = 50 * time.Millisecond // a variable for tests

// The minimum round-trip times that mark a link as LAN (below lanMaxRTT) or
// WAN (above wanMinRTT). In between the frame duration stays as it is, so a
// link near a bound does not switch back and forth. A wired LAN measures well
// below 1 ms, Wi-Fi a few ms (the minimum filters out its jitter); a WAN
// within a city starts around 10 ms.
const (
	lanMaxRTT = 10 * time.Millisecond
	wanMinRTT = 20 * time.Millisecond
)

// OpusFrameMs returns the Opus frame duration (ms) for a link whose minimum
// round-trip time measures minRTT, cur being the duration in use
// (OpusFrameWAN when cur is neither). minRTT <= 0 (not measured) keeps cur.
func OpusFrameMs(cur int, minRTT time.Duration) int {
	if cur != OpusFrameLAN {
		cur = OpusFrameWAN
	}
	switch {
	case minRTT <= 0:
		return cur
	case minRTT < lanMaxRTT:
		return OpusFrameLAN
	case minRTT > wanMinRTT:
		return OpusFrameWAN
	}
	return cur
}

// Audio captures, encodes and packetises audio into datagrams.
type Audio struct {
	src     AudioSource
	cfg     AudioConfig
	log     *slog.Logger
	frameMs atomic.Int32 // Opus frame duration the encoder switches to at its next frame
	capMs   atomic.Int32 // the source's last capture packet in ms, 0 before the first

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewAudio(src AudioSource, cfg AudioConfig, log *slog.Logger) *Audio {
	if cfg.Codec != "pcm" {
		cfg.Codec = "opus"
	}
	if cfg.BitrateKbps <= 0 {
		cfg.BitrateKbps = 160
	}
	a := &Audio{src: src, cfg: cfg, log: log}
	a.frameMs.Store(int32(OpusFrameMs(cfg.FrameMs, 0)))
	return a
}

// FrameMs is the packet duration: for Opus the one SetFrameMs set last
// (packets of the new duration follow from the next frame on).
func (a *Audio) FrameMs() int {
	if a.cfg.Codec == "pcm" {
		return 5
	}
	return int(a.frameMs.Load())
}

// SetFrameMs changes the Opus frame duration (OpusFrameLAN or OpusFrameWAN)
// from the next frame on, while audio runs; Opus packets carry their duration,
// so the client needs no new configuration to decode them. It reports whether
// the duration changed (never for PCM or another value).
func (a *Audio) SetFrameMs(ms int) bool {
	if a.cfg.Codec == "pcm" || (ms != OpusFrameLAN && ms != OpusFrameWAN) {
		return false
	}
	return a.frameMs.Swap(int32(ms)) != int32(ms)
}

// CaptureMs is the duration of the source's last capture packet (one call of
// its sink) in whole ms, 0 before the first.
func (a *Audio) CaptureMs() int { return int(a.capMs.Load()) }

// PickFrameMs returns the Opus frame duration for a link whose minimum
// round-trip time measures minRTT (OpusFrameMs from the current duration),
// with 5 ms frames only while the source's capture packets are at most 5 ms
// (none before the first): with larger ones they would save nothing.
func (a *Audio) PickFrameMs(minRTT time.Duration) int {
	ms := OpusFrameMs(a.FrameMs(), minRTT)
	if c := a.CaptureMs(); ms == OpusFrameLAN && (c == 0 || c > OpusFrameLAN) {
		return OpusFrameWAN
	}
	return ms
}

func (a *Audio) Codec() string { return a.cfg.Codec }

// Kbps is the audio payload bitrate.
func (a *Audio) Kbps() int {
	if a.cfg.Codec == "pcm" {
		return audioRate * audioChannels * 16 / 1000
	}
	return a.cfg.BitrateKbps
}

// Start begins capture; every packet is passed to send as a complete datagram.
// Capture is restarted automatically (e.g. when the default device changes).
func (a *Audio) Start(send func([]byte)) error {
	frame := audioRate * a.FrameMs() / 1000 // samples per channel
	var enc *gopus.Encoder
	if a.cfg.Codec == "opus" {
		var err error
		enc, err = gopus.NewEncoder(gopus.EncoderConfig{SampleRate: audioRate, Channels: audioChannels, Application: gopus.ApplicationLowDelay})
		if err != nil {
			return err
		}
		if err := enc.SetBitrate(a.cfg.BitrateKbps * 1000); err != nil {
			return err
		}
		if err := enc.SetFrameSize(frame); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.done = make(chan struct{})
	done := a.done
	a.mu.Unlock()

	go func() {
		defer close(done)
		pcm := make([]float32, 0, frame*audioChannels*2)
		enc16 := make([]byte, frame*audioChannels*2)
		out := make([]byte, 4000)
		pkt := make([]byte, 0, 2048)
		var seq uint16
		var pts uint32
		codecID := proto.AudioCodecOpus
		if a.cfg.Codec == "pcm" {
			codecID = proto.AudioCodecPCM
		}
		var last time.Time // the source's last delivery
		sink := func(s []float32) {
			now := time.Now()
			frames := len(s) / audioChannels
			if ms := int32((frames*1000 + audioRate/2) / audioRate); a.capMs.Swap(ms) != ms {
				if a.log != nil {
					a.log.Info("audio capture packet", "ms", ms, "frames", frames, "source", a.src.Name())
				}
				if a.cfg.CaptureChanged != nil {
					a.cfg.CaptureChanged()
				}
			}
			// The source paused (sourcePause): the pts moves on by the
			// pause. A partial frame from before it is dropped (less than a
			// frame, the tail of a sound).
			if idle := now.Sub(last) - time.Duration(frames)*time.Second/audioRate; !last.IsZero() && idle >= sourcePause {
				pts += uint32(len(pcm)/audioChannels) + uint32(idle*audioRate/time.Second)
				pcm = pcm[:0]
			}
			last = now
			pcm = append(pcm, s...)
			for {
				if enc != nil {
					if want := audioRate * a.FrameMs() / 1000; want != frame {
						if err := enc.SetFrameSize(want); err != nil {
							if a.log != nil {
								a.log.Warn("opus frame size", "samples", want, "err", err)
							}
							a.frameMs.Store(int32(frame * 1000 / audioRate))
						} else {
							frame = want
						}
					}
				}
				if len(pcm) < frame*audioChannels {
					break
				}
				chunk := pcm[:frame*audioChannels]
				var payload []byte
				if enc != nil {
					n, err := enc.Encode(chunk, out)
					if err != nil {
						if a.log != nil {
							a.log.Warn("opus encode", "err", err)
						}
						pcm = pcm[:0]
						return
					}
					payload = out[:n]
				} else {
					for i, v := range chunk {
						if v > 1 {
							v = 1
						} else if v < -1 {
							v = -1
						}
						binary.LittleEndian.PutUint16(enc16[2*i:], uint16(int16(math.Round(float64(v)*32767))))
					}
					payload = enc16
				}
				pkt = proto.AudioPacket(pkt, codecID, seq, pts, payload)
				send(pkt)
				seq++
				pts += uint32(frame)
				pcm = append(pcm[:0], pcm[frame*audioChannels:]...)
			}
		}
		backoff := 500 * time.Millisecond
		for ctx.Err() == nil {
			err := a.src.Run(ctx, sink)
			if ctx.Err() != nil {
				return
			}
			if a.log != nil {
				a.log.Warn("audio capture stopped, restarting", "source", a.src.Name(), "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}()
	return nil
}

// Stop ends capture and waits for the worker to exit.
func (a *Audio) Stop() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// ToneSource generates a test signal in real time (used on non-Windows hosts
// and in tests): a 440 Hz tone with a short 880 Hz blip every second.
type ToneSource struct{}

func (ToneSource) Name() string { return "test-tone" }

func (ToneSource) Run(ctx context.Context, sink func([]float32)) error {
	const chunk = 240 // 5 ms
	buf := make([]float32, chunk*audioChannels)
	start := time.Now()
	var n int64
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		due := int64(time.Since(start).Seconds() * audioRate)
		for n+chunk <= due {
			for i := 0; i < chunk; i++ {
				s := n + int64(i)
				tt := float64(s) / audioRate
				f := 440.0
				if s%audioRate < audioRate/20 {
					f = 880
				}
				v := float32(0.2 * math.Sin(2*math.Pi*f*tt))
				buf[2*i], buf[2*i+1] = v, v
			}
			sink(buf)
			n += chunk
		}
	}
}

// ErrNoAudio is returned by sources that are unavailable on this platform.
var ErrNoAudio = errors.New("audio capture unavailable")

// convertToStereo48k converts interleaved float32 frames of any channel count
// and sample rate into stereo 48 kHz (simple downmix + linear resampling). It
// is only used when the OS refuses to convert for us.
type resampler struct {
	inRate   int
	channels int
	pos      float64 // fractional read position into pending
	pending  []float32
}

func (r *resampler) process(in []float32, out []float32) []float32 {
	ch := r.channels
	frames := len(in) / ch
	for i := 0; i < frames; i++ {
		f := in[i*ch : i*ch+ch]
		var l, rr float32
		switch {
		case ch == 1:
			l, rr = f[0], f[0]
		default:
			l, rr = f[0], f[1]
			if ch >= 3 { // FL FR FC [LFE] [BL BR] [SL SR]
				c := f[2] * 0.7071
				l += c
				rr += c
			}
			if ch >= 6 {
				l += f[4] * 0.7071
				rr += f[5] * 0.7071
			}
			if ch >= 8 {
				l += f[6] * 0.7071
				rr += f[7] * 0.7071
			}
			if ch >= 3 {
				l *= 0.5
				rr *= 0.5
			}
		}
		r.pending = append(r.pending, l, rr)
	}
	if r.inRate == audioRate {
		out = append(out, r.pending...)
		r.pending = r.pending[:0]
		return out
	}
	step := float64(r.inRate) / audioRate
	n := len(r.pending) / 2
	for r.pos+1 < float64(n) {
		i := int(r.pos)
		fr := float32(r.pos - float64(i))
		out = append(out,
			r.pending[2*i]*(1-fr)+r.pending[2*i+2]*fr,
			r.pending[2*i+1]*(1-fr)+r.pending[2*i+3]*fr)
		r.pos += step
	}
	consumed := int(r.pos)
	r.pending = append(r.pending[:0], r.pending[2*consumed:]...)
	r.pos -= float64(consumed)
	return out
}
