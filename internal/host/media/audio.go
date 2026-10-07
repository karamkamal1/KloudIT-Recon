package media

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"sync"
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
}

// Audio captures, encodes and packetises audio into datagrams.
type Audio struct {
	src AudioSource
	cfg AudioConfig
	log *slog.Logger

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
	return &Audio{src: src, cfg: cfg, log: log}
}

// FrameMs is the packet duration.
func (a *Audio) FrameMs() int {
	if a.cfg.Codec == "pcm" {
		return 5
	}
	return 10
}

func (a *Audio) Codec() string { return a.cfg.Codec }

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
		sink := func(s []float32) {
			pcm = append(pcm, s...)
			for len(pcm) >= frame*audioChannels {
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
