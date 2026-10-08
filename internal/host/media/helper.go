package media

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// HelperVideo is the video pipeline of the native encoder helper
// (recon-encoder.exe, GUIDE Phase 3; docs/HELPER_PROTOCOL.md): one helper
// process captures and encodes, and runtime controls act on the running
// encoder: ForceKeyframe is an in-encoder IDR, SetRate a live bitrate change,
// Recover a reference recovery, where the helper's caps allow them.
//
// Recovery (GUIDE 3.5). Every frame of the live generation goes into a ring
// with its LTR slot; the client's frame ACKs (Ack) mark it acknowledged, and
// ACKs of LTR-marked frames go on to the helper, whose LTR policy only reuses
// slots the client holds. Recover(L) names the newest acknowledged LTR frame
// before L that is still in its slot (ackRing.newestAckedLTR) to the helper,
// which codes the next frame from it (AMF) or invalidates L and later (NVENC);
// that frame comes back flagged RECOVERY with refFloor, and the answer (or the
// key frame the helper fell back to) is reported as a Recovered event.
//
// Generations. The helper numbers its frames (frame ids, gaps = lost frames);
// the client sees (gen, seq). A generation starts at a key frame that starts a
// helper sequence (encoder.Frame.SeqStart: a helper's first frame, and the IDR
// that answers ForceIDR), with seq 0; seq is the frame id minus that frame's,
// which is also what the helper's frame barcode shows (GUIDE 0.2). So a forced
// key frame begins a new generation with the same parameters, exactly as the
// client expects after asking for a key frame (it discards the rest of the
// generation it asked in), only without a new encoder.
//
// Lifecycle. A helper runs for the session; a Start whose stream differs from
// the running one in more than bitrate and frame rate needs a new helper (it
// encodes one stream), started overlapped like Video's generations unless
// urgent. A helper that exits or fails is replaced at once (Restarted
// events), by the spare helper kept launched beside a live stream
// (KeepSpare); after GiveUp failures within GiveUpWindow the pipeline gives
// up (a Fallback event) and the session continues on FFmpeg.
type HelperVideo struct {
	opt    HelperOptions
	log    *slog.Logger
	events chan VideoEvent
	done   chan struct{}

	mu       sync.Mutex
	gen      uint8
	active   *helperProc
	pending  *helperProc
	spare    *encoder.Helper // launched, not started yet: the next helper to start
	refill   bool            // a spare is being launched
	caps     encoder.Caps    // of the newest helper
	closed   bool
	gaveUp   bool
	failures []time.Time // recent failures, for the give-up rule
	streak   int         // failures since a helper last went live, for the back-off
	// After a device_lost (driver reset) helpers that fail before they go
	// live do not count as failures until then: the GPU may need seconds.
	graceUntil time.Time
	// A zero-copy AMD Direct Capture source that changed (capture_failed) is
	// restarted as is once; the second time the new helper converts frames.
	zeroCopyFails int
}

// HelperOptions configures HelperVideo.
type HelperOptions struct {
	// Launch starts a helper (encoder.Launch with the agent's options; tests:
	// encoder.LaunchFake).
	Launch func() (*encoder.Helper, error)
	// First is a helper already launched (the session read its caps to decide
	// for this pipeline); the first Start uses it. Optional.
	First *encoder.Helper
	Log   *slog.Logger
	Clock *Clock
	// GiveUp failures (an exit, a fatal error, a refused start) within
	// GiveUpWindow end the pipeline (default 3 in 60 s).
	GiveUp       int
	GiveUpWindow time.Duration
	// The replacement of a failed helper starts at once, the next ones
	// RestartBackoff later per failure since a helper last went live (default
	// 300 ms, at most 5 x). Helpers that fail before going live within
	// ResetGrace of a device_lost (a driver reset, default 3 s) do not count
	// toward GiveUp.
	RestartBackoff time.Duration
	ResetGrace     time.Duration
	// KeepSpare keeps one more helper launched (caps read, nothing started)
	// while a stream is live, so a restart or a new stream skips the process
	// start and the caps probe (GUIDE 3.1: a restart within ~300 ms).
	KeepSpare bool
	// LiveBitrate picks a stream's rate-control mode (start "rc") and how its
	// bitrate changes from a live-bitrate qualification of the helper's
	// encoder (recon-host qualify, GUIDE 3.6; the host passes
	// qualify.Results.Choose): mode "seamless" or "flush" (start
	// "liveBitrate"), or "restart" (neither passed: a new helper per
	// change); ok false where it says nothing about this encoder, codec,
	// quality preset, LTR slots or mode, and the helper's defaults apply. sp
	// is the start so far (codec, quality, ltrSlots); adaptive: the
	// session's rate controller changes the bitrate. Optional.
	LiveBitrate func(c encoder.Caps, sp encoder.StartParams, adaptive bool) (rc, mode string, ok bool)
}

// ErrHelperGaveUp is returned by Start after the pipeline gave up.
var ErrHelperGaveUp = errors.New("video: the encoder helper failed too often")

// helperProc is one helper process and the stream it encodes.
type helperProc struct {
	params    Params
	sp        encoder.StartParams
	h         *encoder.Helper // nil until launched
	started   encoder.Started // zero until the helper started the stream
	codecCaps encoder.CodecCaps
	killed    bool
	since     time.Time // when it was asked for (restarts: when its predecessor failed)
	restart   bool      // it replaces a failed helper (logs the restart time)
	// resized: its source no longer has the size it started with
	// (captureChanged), so it encodes a scaled picture: the next Start needs a
	// new helper.
	resized bool
	// announce: its bitrate or frame rate changed in place while it was live;
	// a RateChange goes out before its next frame.
	announce bool
	// liveMeasured: its rc and live-bitrate mode come from a qualification
	// (HelperOptions.LiveBitrate); liveRestart: that qualification found no
	// live mode that works, so bitrate changes start a new helper.
	liveMeasured, liveRestart bool

	// The stream, guarded by HelperVideo.mu.
	live    bool
	gen     uint8  // generation of the frames it sends
	seqBase uint64 // frame id of the generation's seq 0
	lastID  uint64 // newest frame id seen
	ptsBase uint64 // capture time of the generation's first frame (host µs)
	noKey   int    // frames before the first key frame
	codec   *codec.Params
	acks    ackRing // gen's frames: LTR marks and the client's ACKs
	// A Recover of gen waiting for its answer: the next recovery frame that
	// references only frames before seq recoverFrom (the oldest loss), or a
	// key frame; recoverAt: when it was asked for.
	recovering  bool
	recoverFrom uint32
	recoverAt   time.Time
}

// NewHelperVideo creates the pipeline; nothing runs until Start.
func NewHelperVideo(opt HelperOptions) *HelperVideo {
	if opt.GiveUp <= 0 {
		opt.GiveUp = 3
	}
	if opt.GiveUpWindow <= 0 {
		opt.GiveUpWindow = time.Minute
	}
	if opt.RestartBackoff <= 0 {
		opt.RestartBackoff = 300 * time.Millisecond
	}
	if opt.ResetGrace <= 0 {
		opt.ResetGrace = 3 * time.Second
	}
	if opt.Clock == nil {
		opt.Clock = NewHostClock()
	}
	v := &HelperVideo{opt: opt, log: opt.Log, events: make(chan VideoEvent, 8), done: make(chan struct{}), spare: opt.First}
	if v.log == nil {
		v.log = slog.New(slog.DiscardHandler)
	}
	if opt.First != nil {
		v.caps = opt.First.Caps()
	}
	return v
}

// HelperEncoders lists a helper's codecs as encoders for the session's codec
// negotiation (hardware encoders of the helper's vendor, named
// <codec>_<backend>_helper, e.g. hevc_amf_helper), HEVC, AV1, H.264 first.
func HelperEncoders(c encoder.Caps) []EncoderInfo {
	if !c.Usable() {
		return nil
	}
	var out []EncoderInfo
	for _, fam := range []string{"hevc", "av1", "h264"} {
		if _, ok := c.Codecs[fam]; ok {
			out = append(out, EncoderInfo{Name: fam + "_" + c.Backend + "_helper", Family: fam, Vendor: c.Vendor, HW: true, Helper: true})
		}
	}
	return out
}

// Events returns the event stream (configs, frames, errors).
func (v *HelperVideo) Events() <-chan VideoEvent { return v.events }

// Gen returns the number of the newest generation.
func (v *HelperVideo) Gen() uint8 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// Start starts the stream for p. A running stream that differs only in
// bitrate and frame rate is changed in place where the encoder can (live
// bitrate), and urgent then forces a key frame in it; a helper still starting
// the same stream is kept (its first frame is a key frame; a new rate goes
// into its start or follows it). Anything else needs a new helper, started
// overlapped unless urgent (the current one stops at once), and so does a
// stream whose source changed size since it started (resized).
func (v *HelperVideo) Start(p Params, urgent bool) error {
	sp, err := v.startParams(p)
	if err != nil {
		return err
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return errors.New("video: closed")
	}
	if v.gaveUp {
		v.mu.Unlock()
		return ErrHelperGaveUp
	}
	cur := v.pending
	if cur == nil {
		cur = v.active
	}
	if cur != nil && !cur.resized && cur.params.Adaptive == p.Adaptive && sameHelperStream(cur.sp, sp) {
		fps := 0
		if cur.sp.FPS != sp.FPS {
			fps = sp.FPS
		}
		vbv := 0.0
		if cur.sp.VBVFrames != sp.VBVFrames {
			vbv = sp.VBVFrames
			if vbv <= 0 {
				vbv = 1 // back to the helper's default
			}
		}
		rate := cur.sp.Kbps != sp.Kbps || fps > 0 || vbv > 0
		if !rate || v.liveBitrate(cur) && (fps == 0 || v.liveFPS(cur)) {
			h, starting := cur.h, cur.started.Codec == ""
			// A frame-rate change alone goes out as one (Phase 5 setRate
			// with fps only), to helpers that know it.
			fpsOnly := fps > 0 && vbv == 0 && cur.sp.Kbps == sp.Kbps && cur.started.LiveFPS != ""
			sp.LTRSlots, sp.ZeroCopy, sp.RC, sp.LiveBitrate = cur.sp.LTRSlots, cur.sp.ZeroCopy, cur.sp.RC, cur.sp.LiveBitrate // as withCaps made them
			sp.IntraRefreshFrames, sp.SVCLayers = cur.sp.IntraRefreshFrames, cur.sp.SVCLayers
			cur.params, cur.sp = p, sp
			if urgent && cur == v.pending && v.active != nil {
				v.kill(v.active) // the starting stream takes over at its first key frame
				v.active = nil
			}
			v.mu.Unlock()
			if starting {
				return nil // run starts it with sp, or sets the rate right after its start
			}
			if rate {
				var err error
				if fpsOnly {
					err = h.SetFPS(fps)
				} else {
					err = h.SetRate(sp.Kbps, vbv, fps)
				}
				if err != nil {
					return err
				}
				v.mu.Lock()
				cur.announce = cur.live // a starting stream's config will have the rate
				v.mu.Unlock()
			}
			if urgent {
				return h.ForceIDR()
			}
			return nil
		}
	}
	if v.pending != nil {
		v.kill(v.pending)
		v.pending = nil
	}
	if urgent && v.active != nil {
		v.kill(v.active)
		v.active = nil
	}
	pr := &helperProc{params: p, sp: sp, since: time.Now()}
	v.pending = pr
	v.mu.Unlock()
	go v.run(pr)
	return nil
}

// sameHelperStream reports whether two starts describe the same stream apart
// from bitrate, frame rate and VBV size (which SetRate changes) and what
// withCaps adds or takes away (the rate-control mode follows from
// Params.Adaptive, which the caller compares; the temporal layers from the
// session's settings, constant for a session).
func sameHelperStream(a, b encoder.StartParams) bool {
	bc := a.Barcode == nil && b.Barcode == nil || a.Barcode != nil && b.Barcode != nil && *a.Barcode == *b.Barcode
	for _, sp := range []*encoder.StartParams{&a, &b} {
		sp.Kbps, sp.FPS, sp.VBVFrames, sp.LTRSlots, sp.ZeroCopy, sp.Barcode, sp.EncoderInstance = 0, 0, 0, 0, nil, nil, nil
		sp.RC, sp.LiveBitrate, sp.IntraRefreshFrames, sp.SVCLayers = "", "", 0, 0
	}
	return bc && a == b
}

// liveBitrate reports whether the proc's encoder changes its bitrate in place
// (before its helper is launched: as the newest helper's caps say). Called
// with v.mu held.
func (v *HelperVideo) liveBitrate(pr *helperProc) bool {
	if pr.liveRestart {
		return false
	}
	lb := v.liveMode(pr)
	return lb == "seamless" || lb == "flush"
}

// liveMode is how the proc's encoder applies SetRate: as started says, else
// as its start asks (a qualification), else as the caps say. Called with v.mu
// held.
func (v *HelperVideo) liveMode(pr *helperProc) string {
	lb := pr.started.LiveBitrate
	if lb == "" {
		lb = pr.sp.LiveBitrate
	}
	if lb == "" {
		lb = pr.codecCaps.LiveBitrate
	}
	if lb == "" && pr.h == nil {
		lb = v.caps.Codecs[pr.sp.Codec].LiveBitrate
	}
	return lb
}

// liveFPS reports whether the proc's encoder changes its frame rate in place
// (as its started liveFps says; helpers before Phase 5 apply a frame rate
// sent with the bitrate as they apply the bitrate). Called with v.mu held.
func (v *HelperVideo) liveFPS(pr *helperProc) bool {
	switch pr.started.LiveFPS {
	case "seamless", "flush":
		return true
	case "":
		return true // as liveBitrate, which the caller checks too
	}
	return false // restart: a new helper
}

// startParams turns a generation's parameters into the helper's start
// message: capture method and monitor from the source, rate control CBR when
// the rate controller may change the bitrate (else the encoder's low-latency
// VBR), two LTR slots where the codec recovers from long-term references, the
// frame barcode of GUIDE 0.2 when the session wants it (test pattern), the GPU
// priority mode of the host config.
func (v *HelperVideo) startParams(p Params) (encoder.StartParams, error) {
	sp := encoder.StartParams{
		Codec:       p.Encoder.Family,
		Width:       p.Width,
		Height:      p.Height,
		FPS:         p.FPS,
		Kbps:        p.BitrateKbps,
		RC:          "vbr",
		Quality:     p.Quality,
		GPUPriority: p.GPUPriority,
		VBVFrames:   p.VBVFrames,
	}
	if p.SVCLayers > 1 {
		sp.SVCLayers = p.SVCLayers
	}
	if p.Adaptive {
		sp.RC = "cbr"
	}
	src := p.Source
	switch src.Backend {
	case "ddagrab", "amf":
		sp.Capture, sp.HMonitor, sp.Monitor = "dda", src.HMonitor, src.Output
		if src.Backend == "amf" {
			sp.Capture = "amd-direct"
		}
	case "gfxcapture":
		sp.Capture = "wgc"
		if src.Window != "" {
			sp.WindowTitle = src.Window
		} else {
			sp.HMonitor = src.HMonitor
		}
	case "test":
		// The helper's test source: a simulated game on the GPU (no TestPad:
		// the helper pads only what its encoder needs).
		sp.Capture = "synthetic-gpu"
		sp.Width, sp.Height = src.NativeW, src.NativeH
		if sp.Width <= 0 || sp.Height <= 0 {
			sp.Width, sp.Height = 1280, 720
		}
	default:
		return sp, fmt.Errorf("the encoder helper cannot capture with %q", src.Backend)
	}
	if p.Barcode {
		sp.Barcode = &encoder.Barcode{Cell: proto.BarcodeCell}
	}
	return sp, nil
}

// liveChoice is what a qualification decided for a proc (withCaps).
type liveChoice struct {
	measured bool // rc and liveBitrate come from HelperOptions.LiveBitrate
	restart  bool // no live mode passed: bitrate changes start a new helper
}

// withCaps completes a start for the helper that runs it: two LTR slots where
// the codec recovers from long-term references (GUIDE 3.5), intra refresh
// wherever it does not conflict with them (below), no zero-copy capture after
// two capture_failed restarts of a zero-copy stream, and the rate-control and
// live-bitrate modes a qualification of this encoder chose
// (HelperOptions.LiveBitrate; else the helper's defaults). Called with v.mu
// held.
func (v *HelperVideo) withCaps(sp encoder.StartParams, adaptive bool, caps encoder.Caps) (encoder.StartParams, liveChoice) {
	sp.LTRSlots = caps.LTRSlots(sp.Codec)
	// Temporal SVC (Phase 5) where the session asks for it and the encoder
	// has the layers; only from a Phase 5 helper (its caps have liveFps):
	// older ones may mark LTR frames in the enhancement layer, which the
	// session leaves out under congestion.
	if sp.SVCLayers > 1 {
		cc := caps.Codecs[sp.Codec]
		why := ""
		switch {
		case cc.MaxTemporalLayers < sp.SVCLayers:
			why = fmt.Sprintf("the %s encoder has %d temporal layers", sp.Codec, cc.MaxTemporalLayers)
		case cc.LiveFPS == "":
			why = "a helper before Phase 5 (LTR marks may fall on enhancement-layer frames)"
		}
		if why != "" {
			v.log.Info("temporal SVC not used", "codec", sp.Codec, "reason", why)
			sp.SVCLayers = 0
		}
	}
	// The loss-recovery ladder's safety net (GUIDE 2.3, rung 3): intra
	// refresh, over half a second of frames as on the FFmpeg path, where
	// the encoder has it and it does not conflict: not with LTR slots, nor
	// with SVC where the caps say so (intraRefreshSvc false: AMF); so NVENC
	// (beside reference invalidation, with or without SVC) and AMF H.264
	// without LTR or SVC. A picture a recovery leaves damaged heals by
	// itself; losses are still answered by recovery frames or IDRs.
	sp.IntraRefreshFrames = caps.IntraRefreshFrames(sp.Codec, sp.FPS, sp.SVCLayers)
	if v.zeroCopyFails >= 2 {
		off := false
		sp.ZeroCopy = &off
	}
	var lc liveChoice
	if v.opt.LiveBitrate != nil {
		if rc, mode, ok := v.opt.LiveBitrate(caps, sp, adaptive); ok {
			lc.measured = true
			if rc != "" {
				sp.RC = rc
			}
			switch mode {
			case "seamless", "flush":
				sp.LiveBitrate = mode
			case "restart":
				lc.restart = true
			}
		}
	}
	return sp, lc
}

// kill stops a proc's helper (asynchronously: Close waits for it to exit).
// Called with v.mu held.
func (v *HelperVideo) kill(pr *helperProc) {
	if pr.killed {
		return
	}
	pr.killed = true
	if h := pr.h; h != nil {
		go h.Close()
	}
}

// refillSpare launches a spare helper in the background (KeepSpare) unless
// there is one.
func (v *HelperVideo) refillSpare() {
	v.mu.Lock()
	if !v.opt.KeepSpare || v.closed || v.gaveUp || v.spare != nil || v.refill {
		v.mu.Unlock()
		return
	}
	v.refill = true
	v.mu.Unlock()
	go func() {
		h, err := v.opt.Launch()
		v.mu.Lock()
		v.refill = false
		keep := err == nil && !v.closed && !v.gaveUp && v.spare == nil
		if keep {
			v.spare, v.caps = h, h.Caps()
		}
		v.mu.Unlock()
		switch {
		case err != nil:
			v.log.Warn("encoder helper: launching the spare failed", "err", err)
		case !keep:
			h.Close()
		}
	}()
}

// launch returns the spare helper, if it is still alive, else a new one.
func (v *HelperVideo) launch() (*encoder.Helper, error) {
	v.mu.Lock()
	h := v.spare
	v.spare = nil
	v.mu.Unlock()
	if h != nil {
		select {
		case <-h.Done():
			go h.Close()
		default:
			return h, nil
		}
	}
	h, err := v.opt.Launch()
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.caps = h.Caps()
	v.mu.Unlock()
	return h, nil
}

// run launches and starts a proc's helper and then delivers its frames.
func (v *HelperVideo) run(pr *helperProc) {
	h, err := v.launch()
	v.mu.Lock()
	if pr.killed {
		v.mu.Unlock()
		if h != nil {
			h.Close()
		}
		return
	}
	pr.h = h
	if h != nil {
		pr.codecCaps = h.Caps().Codecs[pr.sp.Codec]
		var lc liveChoice
		pr.sp, lc = v.withCaps(pr.sp, pr.params.Adaptive, h.Caps())
		pr.liveMeasured, pr.liveRestart = lc.measured, lc.restart
	}
	sp := pr.sp
	v.mu.Unlock()
	if err != nil {
		v.failed(pr, fmt.Errorf("starting the encoder helper: %w", err))
		return
	}
	st, err := h.Start(sp)
	if err != nil {
		v.failed(pr, err)
		return
	}
	v.mu.Lock()
	pr.started = st
	later := pr.sp // a SetRate while the start was on its way
	v.mu.Unlock()
	v.log.Info("encoder helper started", "backend", st.Backend, "capture", st.Capture, "codec", st.Codec,
		"size", fmt.Sprintf("%dx%d", st.Width, st.Height), "fps", st.FPS, "kbps", st.Kbps, "adapter", st.AdapterName,
		"vendor", st.Vendor, "gpu_priority", st.GPUPriority, "live_bitrate", st.LiveBitrate, "rate_control", st.RateControl,
		"live_bitrate_from", liveSource(pr), "ltr_slots", st.LTRSlots, "intra_refresh", st.IntraRefreshFrames, "zero_copy", st.ZeroCopy,
		"barcode", st.Barcode, "cursor_in_video", st.CursorInVideo, "svc_layers", max(1, st.SVCLayers), "live_fps", st.LiveFPS)
	if later.Kbps != sp.Kbps || later.FPS != sp.FPS || later.VBVFrames != sp.VBVFrames {
		vbv := later.VBVFrames
		if vbv <= 0 && sp.VBVFrames > 0 {
			vbv = 1 // back to the helper's default
		}
		_ = h.SetRate(later.Kbps, vbv, later.FPS)
	}
	v.read(pr)
}

// liveSource says where a proc's live-bitrate mode comes from, for the log.
func liveSource(pr *helperProc) string {
	switch {
	case pr.liveRestart:
		return "qualification (no live mode passed: a new helper per change)"
	case pr.liveMeasured:
		return "qualification"
	}
	return "helper default"
}

// read delivers a started proc's frames and capture changes until its helper
// is gone.
func (v *HelperVideo) read(pr *helperProc) {
	h := pr.h
	freq := h.QPCFrequency()
	for {
		select {
		case f, ok := <-h.Frames():
			if !ok {
				err := h.Err()
				if err == nil {
					err = errors.New("encoder helper stopped")
				}
				v.failed(pr, err)
				return
			}
			v.frame(pr, f, freq)
		case c := <-h.CaptureChanges():
			v.mu.Lock()
			relevant := !pr.killed && (v.active == pr || v.pending == pr)
			if c.Reason == "resized" {
				// Back at the size it started with (or turned by 180°) it
				// needs no new helper.
				st := pr.started
				pr.resized = st.CaptureWidth <= 0 || c.Width != st.CaptureWidth || c.Height != st.CaptureHeight
			}
			v.mu.Unlock()
			if relevant {
				v.emit(VideoEvent{Capture: &CaptureChange{Reason: c.Reason, Width: c.Width, Height: c.Height, Rotation: c.Rotation, Text: c.Text}})
			}
		case err := <-h.Errors():
			// A fatal error ends the helper. Its replacement starts at once,
			// not after the exit: the helper may take up to 500 ms to let go
			// of a stuck driver call (its exit watchdog).
			var he *encoder.HelperError
			if errors.As(err, &he) && he.Fatal {
				v.failed(pr, err)
				return
			}
			v.log.Warn("encoder helper error", "err", err)
		}
	}
}

// frame turns a helper frame into the session's: generation and seq, host
// clock stamps, recovery metadata; and the generation's config on its first
// frame.
func (v *HelperVideo) frame(pr *helperProc, f *encoder.Frame, freq int64) {
	now := v.opt.Clock.Now()
	v.mu.Lock()
	if pr.killed || (v.active != pr && v.pending != pr) {
		v.mu.Unlock()
		return // superseded: drain until its helper is gone
	}
	var evs, after []VideoEvent // before and after the frame
	if pr.live && f.FrameID > pr.lastID+1 {
		// The helper lost frames (ring full: this process did not keep up;
		// or an encoder error): gaps in the frame ids.
		why := "encoder error"
		if f.DroppedBefore > 0 {
			why = "helper ring full"
		}
		evs = append(evs, VideoEvent{Lost: &LostFrames{Gen: pr.gen, From: uint32(pr.lastID + 1 - pr.seqBase),
			Count: int(f.FrameID - pr.lastID - 1), Why: why}})
	}
	pr.lastID = max(pr.lastID, f.FrameID)
	capture, _ := v.opt.Clock.FromQPC(f.CaptureQPC, freq)
	data := f.Data
	switch {
	case f.Key && (f.SeqStart || !pr.live):
		first := !pr.live
		if first {
			if v.pending == pr {
				if v.active != nil {
					v.kill(v.active)
				}
				v.active, v.pending = pr, nil
			}
			pr.live = true
			pr.codec = codec.NewParams(pr.sp.Codec, nil)
			v.streak, v.graceUntil = 0, time.Time{} // capture and encoder work (again)
			defer v.refillSpare()                   // after v.mu is released
		}
		if pr.recovering {
			// A new generation answers the loss with its key frame.
			pr.recovering = false
			after = append(after, VideoEvent{Recovered: &Recovered{Gen: pr.gen, From: pr.recoverFrom, AtGen: v.gen + 1, Key: true,
				Wait: time.Since(pr.recoverAt)}})
		}
		v.gen++
		pr.gen, pr.seqBase, pr.ptsBase = v.gen, f.FrameID, capture
		pr.acks.reset()
		pr.announce = false // the new config has the current rate
		data = pr.codec.PrepareKeyFrame(data)
		cfg := v.config(pr)
		if first {
			startup := time.Since(pr.since).Round(time.Millisecond)
			v.log.Info("encoder ready", "gen", pr.gen, "codec", cfg.Codec, "size", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
				"pipeline", PipelineHelper, "startup", startup, "restart", pr.restart)
			if cfg.CropRight > 0 || cfg.CropBottom > 0 {
				v.log.Info("coded picture is padded, client crops", "gen", pr.gen,
					"coded", fmt.Sprintf("%dx%d", cfg.CodedWidth, cfg.CodedHeight), "crop_right", cfg.CropRight, "crop_bottom", cfg.CropBottom)
			}
		} else {
			v.log.Debug("forced key frame: new generation", "gen", pr.gen, "frame_id", f.FrameID)
		}
		evs = append(evs, VideoEvent{Config: cfg, CursorInVideo: pr.started.CursorInVideo})
	case !pr.live:
		// Nothing to switch to before the stream's first key frame (it was
		// dropped): ask for one, and give up on a helper that sends none.
		pr.noKey++
		h, n, limit := pr.h, pr.noKey, max(2*pr.sp.FPS, 30)
		v.mu.Unlock()
		if n == 1 {
			_ = h.ForceIDR()
		}
		if n >= limit {
			v.failed(pr, fmt.Errorf("encoder helper sent %d frames without a key frame", n))
		}
		return
	case f.Key:
		data = pr.codec.PrepareKeyFrame(data)
	}
	if pr.announce {
		pr.announce = false
		evs = append(evs, VideoEvent{Rate: &RateChange{Gen: pr.gen, Kbps: pr.sp.Kbps, FPS: pr.sp.FPS}})
	}
	fr := v.convert(pr, f, data, capture, freq, now)
	if pr.recovering && (fr.Key || fr.Recovery && fr.RefFloor < pr.recoverFrom) {
		pr.recovering = false
		after = append(after, VideoEvent{Recovered: &Recovered{Gen: pr.gen, From: pr.recoverFrom, AtGen: pr.gen, AtSeq: fr.Seq, Key: fr.Key,
			Wait: time.Since(pr.recoverAt)}})
	}
	v.mu.Unlock()
	for _, ev := range evs {
		v.emit(ev)
	}
	v.emit(VideoEvent{Frame: fr})
	for _, ev := range after {
		v.emit(ev)
	}
}

// convert builds the session's frame. Called with v.mu held.
func (v *HelperVideo) convert(pr *helperProc, f *encoder.Frame, data []byte, capture uint64, freq int64, now uint64) *Frame {
	host := func(qpc int64) uint64 {
		us, _ := v.opt.Clock.FromQPC(qpc, freq)
		return us
	}
	fr := &Frame{Gen: pr.gen, Seq: uint32(f.FrameID - pr.seqBase), Key: f.Key, PtsUs: int64(capture) - int64(pr.ptsBase),
		PresentUs: host(f.PresentQPC), CaptureUs: capture, SubmitUs: host(f.SubmitQPC), EncodeDoneUs: host(f.OutputQPC), Data: data}
	if fr.EncodeDoneUs == 0 {
		fr.EncodeDoneUs = now
	}
	// Stamps must run present <= capture <= submit <= encode done; one that
	// does not is not reported.
	if fr.CaptureUs > fr.EncodeDoneUs {
		fr.CaptureUs = 0
	}
	if fr.SubmitUs > fr.EncodeDoneUs || fr.SubmitUs < fr.CaptureUs {
		fr.SubmitUs = 0
	}
	if fr.PresentUs > fr.CaptureUs {
		fr.PresentUs = 0
	}
	if f.Recovery && f.RefFloor >= pr.seqBase && f.RefFloor < f.FrameID {
		fr.Recovery, fr.RefFloor = true, uint32(f.RefFloor-pr.seqBase)
	}
	ltr := -1
	if f.LTRSlot >= 0 && f.LTRSlot < 256 {
		fr.MarkedLTR, fr.LTRSlot = true, uint8(f.LTRSlot)
		ltr = int(f.LTRSlot)
	}
	pr.acks.add(f.FrameID, ltr, f.Key)
	fr.TemporalLayer = uint8(min(f.TemporalLayer, 255))
	// Only frames the client cannot miss stay: a recovery frame whose
	// refFloor was not usable is no recovery frame for the client either.
	fr.Discardable = f.Droppable() && !fr.Recovery
	if f.Dirty >= 0 {
		fr.Dirty, fr.HasDirty = min(f.Dirty, 1), true
		if f.Repeat {
			fr.Dirty = 0
		}
	}
	return fr
}

// config is the VideoConfig of a proc's generation: codec string from the key
// frame's parameter sets, size and crop from started (AV1 on RDNA3 is coded in
// 64x16 multiples; GUIDE 1.7's crop fields make the client crop it like an
// FFmpeg stream), bitrate and frame rate as last set (start or setRate).
// Called with v.mu held.
func (v *HelperVideo) config(pr *helperProc) *proto.VideoConfig {
	st := pr.started
	fps := pr.sp.FPS
	if fps <= 0 {
		fps = st.FPS
	}
	if fps <= 0 {
		fps = pr.params.FPS
	}
	c := &proto.VideoConfig{T: "video", Gen: pr.gen, Family: pr.codec.Family, Codec: pr.codec.Codec, FPS: fps,
		BitrateKbps: pr.sp.Kbps, Encoder: pr.params.Encoder.Name, Capture: st.Capture, Recovery: pr.recovery()}
	if c.Codec == "" {
		c.Codec = defaultCodecString[c.Family]
		v.log.Warn("no parameter sets in the encoder helper's key frame, using a generic codec string", "codec", c.Codec)
	}
	c.SetCrop(st.Width, st.Height, max(st.CodedWidth, pr.codec.CodedWidth), max(st.CodedHeight, pr.codec.CodedHeight))
	return c
}

// defaultCodecString is a WebCodecs codec string per family for a stream
// whose parameter sets could not be read (High / Main profile, level 5.1).
var defaultCodecString = map[string]string{"h264": "avc1.640033", "hevc": "hvc1.1.6.L153.B0", "av1": "av01.0.13M.08"}

// failed handles a proc whose helper exited, failed or refused the start:
// unless it was stopped on purpose, a new helper takes its place (Restarted):
// at once after the first failure since a helper last went live, then
// RestartBackoff later per further failure; after GiveUp failures within
// GiveUpWindow the pipeline gives up (Fallback). A device_lost (driver reset)
// opens a grace of ResetGrace in which helpers that fail before they go live
// are retried without counting.
func (v *HelperVideo) failed(pr *helperProc, err error) {
	v.mu.Lock()
	if pr.killed || v.closed || (v.active != pr && v.pending != pr) {
		v.mu.Unlock()
		return
	}
	live := pr.live
	v.kill(pr)
	if v.active == pr {
		v.active = nil
	}
	if v.pending == pr {
		v.pending = nil
	}
	now := time.Now()
	v.failures = slices.DeleteFunc(v.failures, func(t time.Time) bool { return now.Sub(t) > v.opt.GiveUpWindow })
	var he *encoder.HelperError
	isHE := errors.As(err, &he)
	counted := live || !now.Before(v.graceUntil)
	if counted {
		v.failures = append(v.failures, now)
		if isHE && he.Code == "device_lost" {
			v.graceUntil = now.Add(v.opt.ResetGrace)
		}
	}
	v.streak++
	if isHE && he.Code == "capture_failed" && pr.started.ZeroCopy {
		v.zeroCopyFails++
	}
	giveUp := len(v.failures) >= v.opt.GiveUp
	failedParams := pr.params
	var next *helperProc
	var serr error
	delay := time.Duration(min(v.streak-1, 5)) * v.opt.RestartBackoff
	if giveUp {
		v.gaveUp = true
		for _, o := range []*helperProc{v.active, v.pending} {
			if o != nil {
				v.kill(o)
			}
		}
		v.active, v.pending = nil, nil
	} else if v.pending == nil {
		// Replace it (a starting generation that failed, or the one that
		// streamed with nothing starting), at its current bitrate.
		next = &helperProc{params: pr.params, since: now, restart: true}
		next.params.BitrateKbps = pr.sp.Kbps
		next.params.FPS = pr.sp.FPS
		next.sp, serr = v.startParams(next.params)
		v.pending = next
	}
	n := len(v.failures)
	v.mu.Unlock()
	if giveUp {
		v.log.Warn("encoder helper failed too often, giving up", "failures", n, "window", v.opt.GiveUpWindow, "err", err)
		v.emit(VideoEvent{Err: fmt.Errorf("encoder helper failed %d times within %v: %w", n, v.opt.GiveUpWindow, err),
			Failed: &failedParams, Live: live, EncoderFault: true, Fallback: true})
		return
	}
	v.log.Warn("encoder helper failed, restarting it", "err", err, "live", live, "failures", n, "counted", counted, "retry_in", delay)
	// Restarted also when a starting helper (overlapped Start) takes over:
	// either way the pipeline handles it.
	v.emit(VideoEvent{Err: err, Failed: &failedParams, Live: live, EncoderFault: true, Restarted: true})
	switch {
	case next == nil:
	case serr != nil:
		v.failed(next, serr)
	case delay <= 0:
		go v.run(next)
	default:
		// Not on a GPU that is still failing (a driver reset): a Start of the
		// same stream meanwhile keeps next waiting, another one replaces it.
		time.AfterFunc(delay, func() {
			v.mu.Lock()
			gone := next.killed || v.closed
			v.mu.Unlock()
			if !gone {
				v.run(next)
			}
		})
	}
}

// Hurry: the starting helper takes over at its first key frame, the active
// one stops now.
func (v *HelperVideo) Hurry() (stopped, starting bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pending == nil {
		return false, false
	}
	if v.active != nil {
		v.kill(v.active)
		v.active = nil
		stopped = true
	}
	return stopped, true
}

// Suspend stops the helpers; the next Start launches a new one.
func (v *HelperVideo) Suspend() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, pr := range []*helperProc{v.active, v.pending} {
		if pr != nil {
			v.kill(pr)
		}
	}
	v.active, v.pending = nil, nil
}

// Stop ends the pipeline and every helper.
func (v *HelperVideo) Stop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		close(v.done)
	}
	v.closed = true
	for _, pr := range []*helperProc{v.active, v.pending} {
		if pr != nil {
			v.kill(pr)
		}
	}
	v.active, v.pending = nil, nil
	if v.spare != nil {
		go v.spare.Close()
		v.spare = nil
	}
}

// Active returns the parameters of the stream that is live.
func (v *HelperVideo) Active() (Params, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.active != nil {
		return v.active.current(), true
	}
	return Params{}, false
}

// Current returns the parameters of the starting stream, else the live one.
func (v *HelperVideo) Current() (Params, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pending != nil {
		return v.pending.current(), true
	}
	if v.active != nil {
		return v.active.current(), true
	}
	return Params{}, false
}

// current returns the proc's parameters with its current rate.
func (pr *helperProc) current() Params {
	p := pr.params
	if pr.sp.Kbps > 0 {
		p.BitrateKbps = pr.sp.Kbps
	}
	if pr.sp.FPS > 0 {
		p.FPS = pr.sp.FPS
	}
	return p
}

// Capabilities of the stream that is live (or starting, once started):
// forced IDRs and live bitrate as the helper's caps and started say; recovery
// from long-term references (AMF, with LTR slots in use) or by reference
// invalidation (NVENC), else by key frames.
func (v *HelperVideo) Capabilities() PipelineCaps {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := PipelineCaps{Name: PipelineHelper, Recovery: RecoveryNone}
	pr := v.active
	if pr == nil {
		pr = v.pending
	}
	if pr == nil || pr.started.Codec == "" {
		return c
	}
	c.ForceIDR = pr.codecCaps.ForceIDR
	c.LiveBitrate = v.liveBitrate(pr)
	c.LiveBitrateFlush = c.LiveBitrate && v.liveMode(pr) == "flush"
	c.LiveBitrateMeasured = pr.liveMeasured
	c.LiveFPS = c.LiveBitrate && pr.started.LiveFPS == "seamless"
	c.SVCLayers = pr.started.SVCLayers
	c.CursorInVideo = pr.started.CursorInVideo
	c.IntraRefresh = pr.started.IntraRefreshFrames > 0
	c.Recovery = pr.recovery()
	return c
}

// recovery is how a started proc's stream recovers a lost frame: from an
// acknowledged long-term reference where it runs LTR slots (AMF), by
// reference invalidation where the encoder has it (NVENC), else with a key
// frame.
func (pr *helperProc) recovery() string {
	switch {
	case pr.started.LTRSlots > 0:
		return RecoveryLTR
	case pr.codecCaps.Recovery == "invalidate":
		return RecoveryInvalidate
	}
	return RecoveryKeyframe
}

// ForceKeyframe makes the live encoder's next frame an IDR, which starts a
// new generation. While a new helper is starting its first frame is the key
// frame.
func (v *HelperVideo) ForceKeyframe() error {
	v.mu.Lock()
	pr := v.active
	starting := v.pending != nil
	v.mu.Unlock()
	if pr == nil || !pr.live {
		if starting {
			return nil
		}
		return errors.New("video: no encoder helper streams")
	}
	return pr.h.ForceIDR()
}

// SetRate changes the bitrate (and frame rate, fps > 0; and the VBV size,
// vbvFrames > 0 or 0 for the helper's default) of the stream: in the running
// encoder with live bitrate (seamless, or flush: an encoder flush with an
// IDR; a frame-rate change also needs the helper's liveFps), else with a new
// helper (overlapped).
func (v *HelperVideo) SetRate(kbps, fps int, vbvFrames float64) error {
	p, ok := v.Current()
	if !ok {
		return errors.New("video: no encoder helper streams")
	}
	p.BitrateKbps = kbps
	if fps > 0 {
		p.FPS = fps
	}
	p.VBVFrames = vbvFrames
	return v.Start(p, false)
}

// Recover asks the live encoder to code its next frame from frames the client
// still holds (helper "recover": LTR or reference invalidation), naming the
// newest acknowledged LTR frame before the loss that its slot still holds.
// The answer comes as a Recovered event.
func (v *HelperVideo) Recover(gen uint8, lostFrom uint32) error {
	v.mu.Lock()
	pr := v.active
	if pr == nil || !pr.live || pr.gen != gen {
		v.mu.Unlock()
		return fmt.Errorf("video: generation %d does not stream", gen)
	}
	if r := pr.recovery(); r != RecoveryLTR && r != RecoveryInvalidate {
		v.mu.Unlock()
		return ErrNoRecovery
	}
	if lostFrom == 0 {
		v.mu.Unlock()
		return fmt.Errorf("video: the key frame of generation %d was lost: nothing to recover from", gen)
	}
	id := pr.seqBase + uint64(lostFrom)
	var acked *uint64
	if a, ok := pr.acks.newestAckedLTR(id); ok {
		acked = &a
	}
	fresh := !pr.recovering
	if fresh {
		pr.recovering, pr.recoverFrom, pr.recoverAt = true, lostFrom, time.Now()
	}
	pr.recoverFrom = min(pr.recoverFrom, lostFrom)
	h := pr.h
	v.mu.Unlock()
	err := h.Recover(id, acked)
	if err != nil && fresh {
		v.mu.Lock()
		pr.recovering = false // the caller falls back to a key frame
		v.mu.Unlock()
	}
	return err
}

// Ack records the client's acknowledgement of a frame and passes those of
// LTR-marked frames on to the helper, which only reuses long-term references
// the client holds.
func (v *HelperVideo) Ack(gen uint8, seq uint32) {
	v.mu.Lock()
	pr := v.active
	if pr == nil || !pr.live || pr.gen != gen {
		v.mu.Unlock()
		return
	}
	id := pr.seqBase + uint64(seq)
	ltr, _ := pr.acks.ack(id)
	h := pr.h
	v.mu.Unlock()
	if ltr {
		_ = h.Ack(id)
	}
}

func (v *HelperVideo) emit(ev VideoEvent) {
	select {
	case v.events <- ev:
	case <-v.done:
	}
}
