package media

import (
	"errors"
	"time"
)

// Pipeline is a video pipeline: it runs encoder generations for Params and
// delivers their configs, frames and failures as VideoEvents. Video (one
// FFmpeg child process per generation) and HelperVideo (the native encoder
// helper, GUIDE Phase 3) implement it. The session talks only to this
// interface and picks its behaviour from Capabilities, never from a vendor.
type Pipeline interface {
	// Start begins a new generation for p. urgent: the current generation's
	// frames are useless to the client (it waits for a key frame), stop it at
	// once; otherwise it streams until the new one's first key frame.
	Start(p Params, urgent bool) error
	// Events delivers configs, frames and failures, in order.
	Events() <-chan VideoEvent
	// Hurry turns an overlapped Start that is still starting into an urgent
	// one; it reports whether it stopped the active generation and whether
	// one is starting.
	Hurry() (stopped, starting bool)
	// Suspend stops encoding but keeps the pipeline usable (the next Start
	// begins afresh); Stop ends it for good.
	Suspend()
	Stop()
	// Active returns the parameters of the generation that streams, Current
	// those of the starting one, else of the one that streams.
	Active() (Params, bool)
	Current() (Params, bool)
	// Gen is the number of the newest generation so far.
	Gen() uint8

	// ForceKeyframe makes the client get a key frame: with
	// Capabilities().ForceIDR the running encoder's next frame is an IDR,
	// which starts a new generation (same parameters, a new VideoConfig);
	// otherwise a new generation starts urgently with the current parameters.
	ForceKeyframe() error
	// SetRate changes the bitrate (and the frame rate, fps > 0): with
	// Capabilities().LiveBitrate in the running encoder, otherwise as an
	// overlapped restart with the current parameters.
	SetRate(kbps, fps int) error
	// Recover reports that the frames of generation gen from seq lostFrom on
	// were lost: with Capabilities().Recovery "ltr" or "invalidate" the
	// encoder codes the next frame from frames the client still has (ltr: an
	// acknowledged long-term reference) and flags it as a recovery frame
	// (Frame.Recovery), else ErrNoRecovery. The outcome follows as a
	// VideoEvent.Recovered: a recovery frame, or a key frame where the encoder
	// had nothing to recover from.
	Recover(gen uint8, lostFrom uint32) error
	// Ack reports that the client decoded frame seq of generation gen (frame
	// ack datagram); pipelines with long-term references use it.
	Ack(gen uint8, seq uint32)
	// Capabilities describes the generation that streams (or starts).
	Capabilities() PipelineCaps
}

// PipelineCaps is what a pipeline's encoder can do in the running stream.
type PipelineCaps struct {
	Name string // PipelineFFmpeg | PipelineHelper
	// Recovery is how a lost frame is recovered: RecoverySkip (intra refresh
	// heals the picture, the client skips the frame), RecoveryLTR or
	// RecoveryInvalidate (Recover), RecoveryKeyframe (a key frame), or
	// RecoveryNone while nothing streams.
	Recovery string
	// LiveBitrate: SetRate changes the bitrate in the running encoder (no new
	// encoder process, no key frame unless the encoder flushes).
	LiveBitrate bool
	// LiveBitrateFlush: a live change flushes the encoder and makes a key
	// frame (helper liveBitrate "flush"), so changes should be rare.
	LiveBitrateFlush bool
	// LiveBitrateMeasured: how the bitrate changes was measured on this GPU
	// (recon-host qualify, GUIDE 3.6) rather than assumed from the encoder's
	// defaults.
	LiveBitrateMeasured bool
	// ForceIDR: ForceKeyframe forces an IDR in the running encoder.
	ForceIDR bool
	// IntraRefresh: the encoder runs periodic intra refresh.
	IntraRefresh bool
	// CursorInVideo: the frames contain the mouse pointer.
	CursorInVideo bool
}

// Pipeline names (PipelineCaps.Name, host config "pipeline").
const (
	PipelineFFmpeg = "ffmpeg"
	PipelineHelper = "helper"
)

// PipelineCaps.Recovery values.
const (
	RecoverySkip       = "skip"
	RecoveryLTR        = "ltr"
	RecoveryInvalidate = "invalidate"
	RecoveryKeyframe   = "keyframe"
	RecoveryNone       = "none"
)

// ErrNoRecovery: the pipeline cannot recover a lost frame without a key
// frame (Recover); use ForceKeyframe.
var ErrNoRecovery = errors.New("video: the encoder cannot recover a lost frame without a key frame")

// LostFrames are frames of generation Gen from seq From on (Count of them)
// that will never reach the session: the native helper dropped them (its ring
// was full) or its encoder failed on them.
type LostFrames struct {
	Gen   uint8
	From  uint32
	Count int
	Why   string
}

// Recovered is the encoder's answer to a Recover of generation Gen's frames
// from seq From on: frame AtSeq of generation AtGen, a recovery frame (it
// references only frames before From) or, with Key, a key frame (no usable
// reference: an IDR in the running encoder, or the first frame of a new
// generation). Wait is the time from Recover to that frame.
type Recovered struct {
	Gen   uint8
	From  uint32
	AtGen uint8
	AtSeq uint32
	Key   bool
	Wait  time.Duration
}

// RateChange: the encoder of generation Gen now runs at Kbps and FPS, changed
// in place (HelperVideo: a live setRate), so the generation's VideoConfig is
// out of date.
type RateChange struct {
	Gen       uint8
	Kbps, FPS int
}

// CaptureChange is a change of the capture source (HelperVideo): Reason
// "resized" (new size or rotation: the stream keeps its size, scaled, until
// restarted; the next Start starts a new helper even with the same Params),
// "lost" (capture is impossible for now, the last image is repeated),
// "restored" or "hdr" (Windows HDR was turned on or off for the output: HDR
// says which). Restart: the stream no longer is what it was asked for (an
// HDR10 stream after an "hdr" change keeps the format it started with), so
// the session restarts it; the next Start starts a new helper.
type CaptureChange struct {
	Reason        string
	Width, Height int
	Rotation      int
	Text          string
	HDR           bool
	Restart       bool
}
