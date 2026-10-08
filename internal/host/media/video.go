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
	PresentUs    uint64 // host clock when the game presented the image (native helper; 0 = unknown)
	CaptureUs    uint64 // host clock when the frame was captured (0 = unknown)
	SubmitUs     uint64 // host clock when the frame went into the encoder (native helper; 0 = unknown)
	EncodeDoneUs uint64 // host clock when the encoded frame came out of the encoder
	// Recovery metadata (native helper; frame extension tags 5-7):
	// Recovery frames reference only frames the client acknowledged: seq
	// RefFloor of this generation is the newest earlier frame they (and the
	// frames after them) may reference, none between it and the recovery
	// frame; MarkedLTR frames are kept in long-term reference slot LTRSlot;
	// TemporalLayer is the SVC layer.
	Recovery      bool
	RefFloor      uint32
	MarkedLTR     bool
	LTRSlot       uint8
	TemporalLayer uint8
	Data          []byte
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
	// HealFrames, with Config: how many frames after a lost one the
	// generation's encoder needs to restore the picture by itself
	// (HealFrames of its arguments; 0: a lost frame needs a key frame).
	HealFrames int
	// CursorInVideo, with Config: the generation's frames contain the mouse
	// pointer, so the client must not draw its own.
	CursorInVideo bool
	// Restarted, with Err: the pipeline handles the failure itself
	// (HelperVideo restarts the native helper, or a starting one takes
	// over); a new generation follows with a key frame, the session only
	// tells the user. Fallback,
	// with Err: the pipeline gave up (the native helper failed too often);
	// the session continues on FFmpeg.
	Restarted bool
	Fallback  bool
	// Lost: frames that will never reach the session (HelperVideo: the
	// helper dropped them). Capture: the capture source changed (HelperVideo).
	// Rate: the live generation's bitrate or frame rate changed in its
	// encoder, without a new generation (HelperVideo). Recovered: how the
	// encoder answered a Recover (after the frame that answered it).
	Lost      *LostFrames
	Capture   *CaptureChange
	Rate      *RateChange
	Recovered *Recovered
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

	gpuLogged string // last GPU priority outcome logged at Info (logGPUPriority)
}

type encProc struct {
	gen      uint8
	params   Params
	recovery string   // the live generation's VideoConfig.Recovery
	args     []string // the ffmpeg command line (Recovery reads the encoder options)
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	stderr   *stderrRing
	started  time.Time
	killed   bool
	errDone  chan struct{} // closed when stderr is fully consumed
	// A Recover waiting for its recovery frame (Caps.UseTestRecovery only):
	// the oldest lost seq and when it was asked for. Guarded by Video.mu.
	recovering  bool
	recoverFrom uint32
	recoverAt   time.Time
}

// NewVideo creates a manager. clock returns the host monotonic time in µs.
func NewVideo(caps *Caps, log *slog.Logger, clock func() uint64) *Video {
	return &Video{caps: caps, log: log, clock: clock, events: make(chan VideoEvent, 8), done: make(chan struct{})}
}

// Events returns the event stream (configs, frames, errors).
func (v *Video) Events() <-chan VideoEvent { return v.events }

// Gen returns the number of the newest generation.
func (v *Video) Gen() uint8 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// ContinueAfter makes the next generation gen+1: a Video that takes over from
// another pipeline in a session keeps the client's generations increasing.
func (v *Video) ContinueAfter(gen uint8) {
	v.mu.Lock()
	v.gen = gen
	v.mu.Unlock()
}

// Capabilities: FFmpeg's command line can neither force a key frame nor
// change the bitrate of a running encoder (both are new generations); a lost
// frame needs a key frame unless the live generation runs intra refresh.
func (v *Video) Capabilities() PipelineCaps {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := PipelineCaps{Name: PipelineFFmpeg, Recovery: RecoveryNone}
	if pr := v.active; pr != nil {
		c.Recovery = RecoveryKeyframe
		switch pr.recovery {
		case proto.RecoverySkip:
			c.Recovery, c.IntraRefresh = RecoverySkip, true
		case proto.RecoveryInvalidate:
			c.Recovery = RecoveryInvalidate // tests: Caps.UseTestRecovery
		}
		c.CursorInVideo = pr.params.DrawCursor
	}
	return c
}

// ForceKeyframe starts a new generation at once with the current parameters:
// the FFmpeg command line cannot force a key frame in a running encoder. (The
// session starts it itself, with parameters it builds afresh.)
func (v *Video) ForceKeyframe() error {
	p, ok := v.Current()
	if !ok {
		return errors.New("video: nothing to restart")
	}
	return v.Start(p, true)
}

// SetRate starts an overlapped generation at the new bitrate (and frame rate):
// the FFmpeg command line sets them only at start.
func (v *Video) SetRate(kbps, fps int) error {
	p, ok := v.Current()
	if !ok {
		return errors.New("video: nothing to restart")
	}
	p.BitrateKbps = kbps
	if fps > 0 {
		p.FPS = fps
	}
	return v.Start(p, false)
}

// Recover: the FFmpeg encoders keep no references the client could recover
// from. Tests (Caps.UseTestRecovery): the next key frame after frame lostFrom
// goes out as the recovery frame.
func (v *Video) Recover(gen uint8, lostFrom uint32) error {
	if !v.caps.testRecovery {
		return ErrNoRecovery
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	pr := v.active
	switch {
	case pr == nil || pr.gen != gen:
		return fmt.Errorf("video: generation %d does not stream", gen)
	case lostFrom == 0:
		return fmt.Errorf("video: the key frame of generation %d was lost: nothing to recover from", gen)
	case !pr.recovering:
		pr.recovering, pr.recoverFrom, pr.recoverAt = true, lostFrom, time.Now()
	}
	pr.recoverFrom = min(pr.recoverFrom, lostFrom)
	return nil
}

// Ack: unused (no long-term references).
func (v *Video) Ack(gen uint8, seq uint32) {}

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
	gpu, host, gerr := raisePriority(cmd, p.Encoder.Vendor, p.GPUPriority)
	v.logGPUPriority(pr.gen, p.Encoder.Vendor, p.GPUPriority, gpu, host, gerr)
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
	noKey := 0 // frames before the generation's first key frame
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
				// Cannot switch on a non-key frame. Every encoder starts with
				// one (NVENC with intra refresh too: only later frames
				// refresh instead of an IDR), so a generation that sends
				// none would otherwise hang unnoticed: it fails, as an
				// encoder fault (the source delivers frames).
				if noKey++; noKey < max(2*pr.params.FPS, 30) {
					v.mu.Unlock()
					continue
				}
				v.pending = nil
				pr.kill()
				v.mu.Unlock()
				failed := pr.params
				v.emit(VideoEvent{Err: fmt.Errorf("encoder %s sent %d frames without a key frame", pr.params.Encoder.Name, noKey), Failed: &failed,
					EncoderFault: true})
				return
			}
			if v.active != nil {
				v.active.kill()
			}
			v.active, v.pending = pr, nil
			pr.recovery = Recovery(pr.args, st.Width, st.Height, pr.params.FPS)
			if v.caps.testRecovery {
				pr.recovery = proto.RecoveryInvalidate
			}
			cfg := &proto.VideoConfig{
				T: "video", Gen: pr.gen, Family: params.Family, Codec: params.Codec,
				FPS: pr.params.FPS, BitrateKbps: pr.params.BitrateKbps,
				Encoder: pr.params.Encoder.Name, Capture: pr.params.Source.Backend,
				Recovery: pr.recovery,
			}
			cfg.SetCrop(visibleSize(st, params, pr.params))
			if v.log != nil {
				v.log.Info("encoder ready", "gen", pr.gen, "codec", cfg.Codec, "size", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
					"startup", time.Since(pr.started).Round(time.Millisecond), "recovery", cfg.Recovery)
				if cfg.CropRight > 0 || cfg.CropBottom > 0 {
					v.log.Info("coded picture is padded, client crops", "gen", pr.gen,
						"coded", fmt.Sprintf("%dx%d", cfg.CodedWidth, cfg.CodedHeight), "crop_right", cfg.CropRight, "crop_bottom", cfg.CropBottom)
				}
			}
			v.mu.Unlock()
			v.emit(VideoEvent{Config: cfg, HealFrames: HealFrames(pr.args, st.Width, st.Height), CursorInVideo: pr.params.DrawCursor})
			v.emit(VideoEvent{Frame: f})
		case v.active == pr:
			rec := v.testRecoveryFrame(pr, f)
			v.mu.Unlock()
			v.emit(VideoEvent{Frame: f})
			if rec != nil {
				v.emit(VideoEvent{Recovered: rec})
			}
		default:
			v.mu.Unlock() // superseded generation, drain until killed
		}
	}
}

// testRecoveryFrame turns a key frame after the generation's first into a
// P-frame, or into the recovery frame a Recover waits for (Caps.UseTestRecovery
// only; see there), and returns the Recover's answer then. Called with v.mu
// held.
func (v *Video) testRecoveryFrame(pr *encProc, f *Frame) *Recovered {
	if !v.caps.testRecovery || !f.Key {
		return nil
	}
	f.Key = false
	if !pr.recovering || f.Seq <= pr.recoverFrom {
		return nil
	}
	pr.recovering = false
	f.Recovery, f.RefFloor = true, pr.recoverFrom-1
	return &Recovered{Gen: pr.gen, From: pr.recoverFrom, AtGen: pr.gen, AtSeq: f.Seq, Wait: time.Since(pr.recoverAt)}
}

// visibleSize returns the picture to show (w, h) and the coded picture the
// decoder outputs (codedW, codedH) for a generation. NUT carries the size the
// encoder was configured with; an AV1 encoder that pads codes a larger frame,
// which only its sequence header tells (FFmpeg's av1_amf reports the crop
// as stream side data, which NUT does not store). TestPad rows below the
// test pattern are padding by definition.
func visibleSize(st *nut.Stream, params *codec.Params, p Params) (w, h, codedW, codedH int) {
	w, h = st.Width, st.Height
	codedW, codedH = max(w, params.CodedWidth), max(h, params.CodedHeight)
	if p.Source.Backend == "test" && p.TestPad > 0 && p.TestPad < h {
		h -= p.TestPad
	}
	return w, h, codedW, codedH
}

func (v *Video) emit(ev VideoEvent) {
	select {
	case v.events <- ev:
	case <-v.done:
	}
}
