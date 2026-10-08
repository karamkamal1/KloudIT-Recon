package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/nut"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Frame is one encoded video frame (an access unit / temporal unit).
type Frame struct {
	Gen          uint8
	Seq          uint32
	Key          bool
	PtsUs        int64  // relative to the generation's first frame
	CaptureUs    uint64 // host clock when the frame was captured (0 = unknown)
	EncodeDoneUs uint64 // host clock when the encoded frame was read from the encoder
	Data         []byte
}

// maxCaptureToEncoded bounds plausible capture->encoded times; anything else
// means the pts did not carry the capture clock (the stamp is dropped).
const maxCaptureToEncoded = 2_000_000 // µs

// wallOffsetEvery is how often the wall-clock to host-clock offset is
// re-measured: the two clocks drift apart (Windows: QPC vs a wall clock that
// W32Time slews and steps), and a generation can last a whole session.
const wallOffsetEvery = 1_000_000 // µs

// VideoEvent is delivered in order on Video.Events().
type VideoEvent struct {
	Config *proto.VideoConfig // a new generation starts with the next frame
	Frame  *Frame
	Err    error // the active encoder failed
	// Failed holds, with Err, the failed generation's parameters (the
	// generation is already gone from Current). Live is set when that
	// generation had gone live (sent its first key frame) before it failed.
	// EncoderFault is set when the encoder itself failed (encoderFault), not
	// e.g. the capture source.
	Failed       *Params
	Live         bool
	EncoderFault bool
}

// encoderFault reports whether an encoder process's stderr shows that the
// encoder itself failed: FFmpeg does not have it, refused its options, could
// not set up or open it, or the encoder logged an error (its own "[<name> @"
// lines; FFmpeg 8.1's encoder task logs as "[enc:<name> @"). When the capture
// source fails instead (ddagrab and gfxcapture lose the desktop to a UAC
// prompt, the lock screen or a display mode change), FFmpeg only adds "Could
// not open encoder before EOF" for the encoder.
func encoderFault(stderr, name string) bool {
	for _, s := range []string{"Unknown encoder", "Error applying encoder options", "Encoding hardware device setup failed",
		"Error while opening encoder", "[" + name + " @ "} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

// Video manages encoder generations. Restarting (to force a key frame or change
// bitrate/resolution/codec) is overlapped: the new ffmpeg process starts while
// the old one keeps streaming, and the switch happens on the new generation's
// first key frame, so settings changes do not freeze the picture.
type Video struct {
	caps   *Caps
	log    *slog.Logger
	clock  func() uint64
	events chan VideoEvent

	done    chan struct{}
	mu      sync.Mutex
	gen     uint8
	active  *encProc
	pending *encProc
	closed  bool
}

type encProc struct {
	gen     uint8
	params  Params
	args    []string // the ffmpeg command line (Recovery reads the encoder options)
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stderr  *stderrRing
	started time.Time
	killed  bool
	errDone chan struct{} // closed when stderr is fully consumed
}

// NewVideo creates a manager. clock returns the host monotonic time in µs.
func NewVideo(caps *Caps, log *slog.Logger, clock func() uint64) *Video {
	return &Video{caps: caps, log: log, clock: clock, events: make(chan VideoEvent, 8), done: make(chan struct{})}
}

// Events returns the event stream (configs, frames, errors).
func (v *Video) Events() <-chan VideoEvent { return v.events }

// Start launches a new encoder generation. If urgent is true the current
// generation is stopped immediately (its frames are useless to the client, e.g.
// after a decode error); otherwise it keeps streaming until the new one is ready.
func (v *Video) Start(p Params, urgent bool) error {
	args, err := v.caps.BuildArgs(p)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return errors.New("video: closed")
	}
	if v.pending != nil {
		v.pending.kill()
		v.pending = nil
	}
	if urgent && v.active != nil {
		v.active.kill()
		v.active = nil
	}
	v.gen++
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, v.caps.FFmpeg, args...)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	pr := &encProc{gen: v.gen, params: p, args: args, cmd: cmd, cancel: cancel, stderr: &stderrRing{log: v.log}, started: time.Now(), errDone: make(chan struct{})}
	if v.log != nil {
		v.log.Info("starting encoder", "gen", pr.gen, "encoder", p.Encoder.Name, "capture", p.Source.Backend,
			"fps", p.FPS, "kbps", p.BitrateKbps, "size", fmt.Sprintf("%dx%d", p.Width, p.Height), "adaptive", p.Adaptive)
		v.log.Debug("ffmpeg args", "args", args)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	raisePriority(cmd)
	go func() {
		pr.stderr.consume(bufio.NewReader(stderrPipe))
		close(pr.errDone)
	}()
	v.pending = pr
	go v.read(pr, stdout)
	return nil
}

// Hurry turns an overlapped Start that is still starting into an urgent one:
// the active generation stops now and the starting one takes over at its
// first key frame. It reports whether it stopped the active generation and
// whether a generation is starting (with or without one still active).
func (v *Video) Hurry() (stopped, starting bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pending == nil {
		return false, false
	}
	if v.active != nil {
		v.active.kill()
		v.active = nil
		stopped = true
	}
	return stopped, true
}

// Stop terminates all encoder processes.
func (v *Video) Stop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		close(v.done)
	}
	v.closed = true
	if v.active != nil {
		v.active.kill()
	}
	if v.pending != nil {
		v.pending.kill()
	}
	v.active, v.pending = nil, nil
}

// Suspend stops all encoder processes but keeps the manager usable; the next
// Start begins a fresh generation.
func (v *Video) Suspend() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.active != nil {
		v.active.kill()
	}
	if v.pending != nil {
		v.pending.kill()
	}
	v.active, v.pending = nil, nil
}

// Active returns the parameters of the generation currently streaming.
func (v *Video) Active() (Params, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.active != nil {
		return v.active.params, true
	}
	return Params{}, false
}

// Current returns the parameters of the active (or starting) generation.
func (v *Video) Current() (Params, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pending != nil {
		return v.pending.params, true
	}
	if v.active != nil {
		return v.active.params, true
	}
	return Params{}, false
}

func (p *encProc) kill() {
	if p.killed {
		return
	}
	p.killed = true
	p.cancel()
}

func (v *Video) read(pr *encProc, stdout io.Reader) {
	d := nut.NewDemuxer(stdout, proto.MaxFrameSize)
	var params *codec.Params
	var seq uint32
	var pts0 int64
	var wallOff int64 // CaptureClock: wall clock minus host clock (µs)
	var wallOffAt uint64
	warnedStamp := false
	defer func() {
		select {
		case <-pr.errDone:
		case <-time.After(2 * time.Second):
		}
		_ = pr.cmd.Wait()
	}()
	for {
		pkt, err := d.ReadPacket()
		if err != nil {
			v.mu.Lock()
			killed := pr.killed
			live := v.active == pr
			relevant := live || v.pending == pr
			if relevant {
				if v.active == pr {
					v.active = nil
				}
				if v.pending == pr {
					v.pending = nil
				}
			}
			v.mu.Unlock()
			if !killed && relevant {
				select {
				case <-pr.errDone:
				case <-time.After(2 * time.Second):
				}
				msg := pr.stderr.String()
				fault := encoderFault(msg, pr.params.Encoder.Name)
				if msg == "" {
					msg = err.Error()
				}
				failed := pr.params
				v.emit(VideoEvent{Err: fmt.Errorf("encoder %s exited: %s", pr.params.Encoder.Name, msg), Failed: &failed, Live: live,
					EncoderFault: fault})
			}
			return
		}
		st := d.Streams()[pkt.Stream]
		if st == nil || st.Class != nut.ClassVideo {
			continue
		}
		if params == nil {
			fam := codec.FamilyFromFourCC(st.FourCC)
			if fam == "" {
				fam = pr.params.Encoder.Family
			}
			params = codec.NewParams(fam, st.Extradata)
		}
		done := v.clock()
		data := pkt.Data
		if pkt.Key {
			data = params.PrepareKeyFrame(data)
		}
		pts := pkt.PtsMicros(st)
		if seq == 0 {
			pts0 = pts
		}
		f := &Frame{Gen: pr.gen, Seq: seq, Key: pkt.Key, PtsUs: pts - pts0, EncodeDoneUs: done, Data: data}
		if pr.params.CaptureClock {
			if seq == 0 || done-wallOffAt >= wallOffsetEvery {
				wallOff, wallOffAt = WallOffset(v.clock), done
			}
			// pts is the wall-clock capture time (CaptureClockFilter).
			if c := pts - wallOff; c > 0 && uint64(c) <= done && done-uint64(c) <= maxCaptureToEncoded {
				f.CaptureUs = uint64(c)
			} else if !warnedStamp && v.log != nil {
				warnedStamp = true
				v.log.Warn("implausible capture timestamp, not reported", "gen", pr.gen, "seq", seq,
					"capture_to_encoded_us", int64(done)-c)
			}
		}
		seq++

		v.mu.Lock()
		switch {
		case v.pending == pr:
			if !f.Key {
				v.mu.Unlock()
				continue // cannot switch on a non-key frame
			}
			if v.active != nil {
				v.active.kill()
			}
			v.active, v.pending = pr, nil
			cfg := &proto.VideoConfig{
				T: "video", Gen: pr.gen, Family: params.Family, Codec: params.Codec,
				Width: st.Width, Height: st.Height, FPS: pr.params.FPS, BitrateKbps: pr.params.BitrateKbps,
				Encoder: pr.params.Encoder.Name, Capture: pr.params.Source.Backend,
				Recovery: Recovery(pr.args, st.Width, st.Height, pr.params.FPS),
			}
			if v.log != nil {
				v.log.Info("encoder ready", "gen", pr.gen, "codec", cfg.Codec, "size", fmt.Sprintf("%dx%d", st.Width, st.Height),
					"startup", time.Since(pr.started).Round(time.Millisecond), "recovery", cfg.Recovery)
			}
			v.mu.Unlock()
			v.emit(VideoEvent{Config: cfg})
			v.emit(VideoEvent{Frame: f})
		case v.active == pr:
			v.mu.Unlock()
			v.emit(VideoEvent{Frame: f})
		default:
			v.mu.Unlock() // superseded generation, drain until killed
		}
	}
}

func (v *Video) emit(ev VideoEvent) {
	select {
	case v.events <- ev:
	case <-v.done:
	}
}
