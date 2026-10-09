package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"math"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/codec"
	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// SessionMeta describes how a connection reached the host.
type SessionMeta struct {
	Path          string // direct | relay (UDP relay) | relay-splice (gateway data connection)
	User          string // authenticated by the gateway (relay-splice path)
	RequireTicket bool   // direct and relay paths: the client must present a gateway-signed ticket
	Origin        string // direct and relay paths: Origin header of the WebTransport request
	Relay         string // relay path: the allocation the connection arrived through
	// Gen is the agent's end number (Agent.endGen) when the gateway
	// authorised the session: its open (relay-splice), or when its ticket
	// was checked. A gateway "end" for the user after that refuses it.
	Gen uint64
}

// Session is one streaming client.
type Session struct {
	a    *Agent
	c    transport.Conn
	meta SessionMeta
	id   string
	log  *slog.Logger
	// partial: frame streams mark their header (and a key frame's parameter
	// sets) reliable, so a cancelled frame still delivers them (GUIDE 2.4:
	// the client's QUIC endpoint negotiated RESET_STREAM_AT); resetStreamAt:
	// that negotiation for the session log ("yes", "no"; "n/a" on the splice,
	// whose peer is the gateway).
	partial       bool
	resetStreamAt string

	ctx    context.Context
	cancel context.CancelFunc

	ctrlMu sync.Mutex // one control write at a time
	ctrl   transport.BidiStream
	// ctrlDL guards the control stream's write deadline between the writer
	// and close(): a writer arms its deadline under it unless the session is
	// closing, and close() cuts a stalled write short under it.
	ctrlDL   sync.Mutex
	ctrlDue  time.Time // deadline of the write in progress (or the last one)
	closing  bool      // close() ran: no control writes but its bye
	ctrlTorn bool      // a control write failed, maybe part-way: no framing to append to (ctrlMu)

	hello       proto.Hello
	prefsMu     sync.Mutex // guards prefs, monitor, codecWhy, alignNotice and amfFallback
	prefs       proto.Prefs
	monitor     platform.Monitor
	codecWhy    string // last codec choice and its reason, logged once
	hdrChoice   string // last HDR10 decision (chooseHDR), logged once
	alignNotice string // last coded-size alignment notice, sent once
	amfFallback string // why the last generation did not use capture "amf", logged once
	amfFailed   atomic.Bool

	pipeMu     sync.Mutex     // guards video, helperEncs and helperCaps
	video      media.Pipeline // FFmpeg or the native helper (openPipeline); use vid()
	pipeSwap   chan struct{}  // leaveHelper replaced video: videoEvents reads the new one's events
	helperEncs []media.EncoderInfo
	helperCaps encoder.Caps // the native helper's, while video is one (helperEncs != nil)
	// cursorInVideo: the live generation's frames show the mouse pointer
	// (VideoEvent.CursorInVideo), read by cursorLoop without the pipeline's lock.
	cursorInVideo atomic.Bool
	resizeTimer   *time.Timer // a capture size change waiting to settle (captureChanged)

	// The session's virtual display (GUIDE 3.7, virtualdisplay.go): vd, nil
	// without one, created for mode vdMode; vdOff says why the session may
	// not have one any more (it lost one), vdWhy is the last decision not to
	// use one (logged once). vdMu also serialises creating and replacing it,
	// which takes seconds: buildParams waits for that rather than capturing
	// a display on its way out.
	vdMu   sync.Mutex
	vd     *vdisplay.Display
	vdMode vdisplay.Mode
	vdOff  string
	vdWhy  string

	audioMu  sync.Mutex // guards audio: startAudio and stopAudio, applyAudioFrame
	audio    *media.Audio
	frameQ   chan *media.Frame
	paused   atomic.Bool
	lastKick time.Time // last key-frame restart (any reason)
	kickMu   sync.Mutex
	rate     rateController // video bitrate: the delay-based rate controller (bitrate.go)
	track    sendTrack      // frames sent, for the rate controller's feedback (ratefeedback.go)
	fb       rateFeedback   // the client's receive reports (or acks), turned into feedback
	noticeAt atomic.Int64   // unix ns of the last congestion notice to the user (rate limited)
	rateLog  rateLog        // how applyRate logs the controller's changes

	// display keeps the PC's display on while set (keepDisplayOn, nil
	// without one); displayHeld: it is set (the client watches).
	display     displayRequest
	displayHeld bool

	// sendSince: unix ns when frameSender took the frame it sends (0: it
	// waits for one); sendOpening: it still waits for the frame's stream.
	// For the overflow log (logOverflow).
	sendSince   atomic.Int64
	sendOpening atomic.Bool
	// send: the frame streams being written and the loss the client waits
	// on, for the loss-recovery ladder (ladder.go); discards: the frames it
	// discarded and has not reported yet.
	send     sendState
	discards discardRun
	// win: the frames in flight (window.go, GUIDE 2.7); windowSince: unix
	// ns since frameSender holds a frame for it (0: none), for the
	// overflow log; meter: a test's stand-in for the connection's delivery
	// meter (nil: transport.MediaControl).
	win         videoWindow
	windowSince atomic.Int64
	meter       func() deliveryMeter
	// pongs: answers to the client's pings, sent by pongSender so the
	// datagram loop (input) never waits for the datagram queue.
	pongs chan []byte
	// thinning: discardable frames left out under congestion (thin.go);
	// static: the encoder's bitrate on a static desktop (activity.go).
	thinning thinState
	static   staticCap
	// roi: the encoder's regions of interest from the pointer input (roi.go).
	roi roiFocus
	// fec: the "datagram + FEC" video mode (fec.go); fecNacks: the client's
	// NACKs for fecRepairs.
	fec      fecState
	fecNacks chan proto.FECNack

	// rateChanges carries the rate controller's decisions on the client's
	// reports from the datagram loop to rateLoop, which applies them in
	// order with its own (an FFmpeg restart must not hold up input).
	rateChanges chan rateChange
	videoUp     atomic.Bool
	failures    int
	tried       map[string]bool   // encoders excluded after failing
	usage       map[string]string // encoder -> usage it is retried with (media.RetryUsage)
	encFails    map[string]int    // encoder -> its own start failures since a generation last went live
	triedMu     sync.Mutex        // guards tried, usage and encFails

	// Recovery "skip" bounded in time (watchHeal), guarded by healMu: the
	// live generation, how many frames after a lost one it needs to heal it
	// (0: it announced "keyframe"), and its reported loss not yet healed.
	// liveRecovery: the recovery mode the client was told for healGen
	// (VideoConfig.Recovery: the loss-recovery ladder's mode, Session.loss).
	healMu       sync.Mutex
	healGen      uint8
	healFrames   int
	heal         *healWatch
	liveRecovery string
	// genFamily: each generation's codec family (VideoConfig.Family, by
	// gen), for the parameter sets of its key frames (reliablePrefix).
	genFamily [256]string

	ccTarget  atomic.Pointer[ccTarget] // media congestion controller (setCongestionTarget)
	audioKbps atomic.Int64             // audio bitrate while audio runs

	// The client's minimum round-trip time (ns) from its pings (clients
	// since step 4.6, proto.PingMinRTT; 0 until one carries it), and the
	// signal that it or the audio capture packets changed, for
	// audioFrameLoop.
	clientRTT atomic.Int64
	rttSeen   chan struct{}

	rumbles  rumbleState   // force feedback being forwarded to the client
	rumbleGo chan struct{} // a rumble started: rumbleLoop repeats it

	rel      input.RelTracker
	absGate  input.SeqGate
	padGates [4]input.SeqGate
	padWarn  sync.Once

	stats      sessionStats
	hostStages hostStages
	onAuth     func()
}

type sessionStats struct {
	frames, bytes, acks atomic.Int64
	owdSum              atomic.Int64
	owdMax              atomic.Int64
	dropped             atomic.Int64 // frames the host discarded (reportDropped)
	// Losses under reference recovery (Session.loss): answered with a
	// recovery frame, or with a key frame (none possible, or the encoder fell
	// back to one).
	recovered, recoveredByKey atomic.Int64
	// The ladder's other work: frame streams cancelled past their deadline
	// (rung 1), frames not sent because the client waits for the answer to
	// a loss before them, key frames asked of the pipeline (rung 4: IDRs in
	// the encoder, or new generations).
	cancelled, discarded, keyframes atomic.Int64
	// thinned: discardable frames left out under congestion (thin.go).
	thinned atomic.Int64
	// reencoded: frames the encoder encoded a second time because they were
	// oversized (host config reencodeOversized).
	reencoded atomic.Int64
}

var errClosed = errors.New("session closed")

// errFrameCancelled: the ladder cancelled the frame stream being written.
var errFrameCancelled = errors.New("frame stream cancelled")

func (a *Agent) newSession(c transport.Conn, meta SessionMeta) *Session {
	ctx, cancel := context.WithCancel(c.Context())
	s := &Session{
		a: a, c: c, meta: meta, id: auth.RandomToken(6),
		ctx: ctx, cancel: cancel,
		frameQ:      make(chan *media.Frame, 6),
		pipeSwap:    make(chan struct{}, 1),
		rttSeen:     make(chan struct{}, 1),
		rumbleGo:    make(chan struct{}, 1),
		pongs:       make(chan []byte, 4),
		tried:       map[string]bool{},
		usage:       map[string]string{},
		encFails:    map[string]int{},
		rateChanges: make(chan rateChange, 16),
		fecNacks:    make(chan proto.FECNack, 64),
	}
	s.log = a.log.With("session", s.id, "path", meta.Path)
	// The direct path and the UDP relay end at the client; the splice relay
	// (relay-splice, also WebSocket) ends at the gateway.
	atClient := meta.Path == "direct" || meta.Path == "relay"
	s.rate.setPath(atClient)
	// Partial delivery (GUIDE 2.4) where the client's QUIC endpoint
	// negotiated it. The splice's peer is the gateway, which passes a reset
	// frame stream on as a plain reset (relay.go): nothing to gain there.
	s.resetStreamAt = "n/a"
	if atClient {
		s.partial = transport.PartialDelivery(c)
		s.resetStreamAt = map[bool]string{true: "yes", false: "no"}[s.partial]
	}
	s.rate.setFPSFloor(a.cfg.FPSFloor)
	s.static.on, s.static.kbps = a.cfg.staticBitrate(), a.cfg.StaticKbps
	s.roi.mode = a.cfg.roi()
	return s
}

// HandleConn runs a streaming session on an established connection. It
// returns when the session ends.
func (a *Agent) HandleConn(c transport.Conn, meta SessionMeta) {
	s := a.newSession(c, meta)
	defer c.Close(transport.CodeNone, "bye")
	if err := s.run(); err != nil && !errors.Is(err, errClosed) && !errors.Is(err, context.Canceled) {
		s.log.Info("session ended", "err", err)
	} else {
		s.log.Info("session ended")
	}
}

var pendingDirect atomic.Int32

func (s *Session) run() error {
	defer s.cancel()
	if s.meta.RequireTicket {
		// Unauthenticated until the hello's ticket checks out: cap how many
		// such sessions may wait at once.
		if pendingDirect.Add(1) > 8 {
			pendingDirect.Add(-1)
			return errors.New("too many pending direct sessions")
		}
		authed := false
		defer func() {
			if !authed {
				pendingDirect.Add(-1)
			}
		}()
		s.onAuth = func() { authed = true; pendingDirect.Add(-1) }
	}
	// Streams arrive in any order; dispatch by their first byte.
	ctrlCh := make(chan transport.BidiStream, 1)
	go s.acceptStreams(ctrlCh)

	var ctrl transport.BidiStream
	select {
	case ctrl = <-ctrlCh:
	case <-time.After(10 * time.Second):
		return errors.New("no control stream")
	case <-s.ctx.Done():
		return errClosed
	}
	s.ctrl = ctrl
	_ = ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := proto.ReadMsg(ctrl, proto.MaxControlMsg)
	if err != nil {
		return fmt.Errorf("reading hello: %w", err)
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	if err := json.Unmarshal(b, &s.hello); err != nil || s.hello.T != "hello" {
		return errors.New("bad hello")
	}
	if s.meta.RequireTicket {
		user, gen, err := s.a.verifyTicket(s.hello.Ticket, s.meta.Origin, s.meta.Relay)
		if err != nil {
			// The refusal has to reach the client, which then leaves out the
			// paths the host authorises: closing the connection resets the
			// control stream with it in flight. The client ends the session
			// when it reads it (as on a bye); byeGrace at most.
			if s.sendJSON(proto.Notice{T: "error", Level: "error", Msg: "unauthorized"}) == nil {
				select {
				case <-s.c.Context().Done():
				case <-time.After(byeGrace):
				}
			}
			s.c.Close(transport.CodeAuth, "unauthorized")
			return fmt.Errorf("%s ticket: %w", s.meta.Path, err)
		}
		s.meta.User, s.meta.Gen = user, gen
		if s.onAuth != nil {
			s.onAuth()
		}
	}
	s.log.Info("session started", "user", s.meta.User, "remote", s.c.RemoteAddr().String(), "ua", trunc(s.hello.Client.UA, 80),
		"decoders", decoderSummary(s.hello.Decoders), "reset_stream_at", s.resetStreamAt)

	// One active session per host: a new connection takes over, unless the
	// gateway revoked the user's access since it authorised this one.
	if reason := s.a.setActive(s); reason != "" {
		s.log.Info("session refused: the gateway revoked the user's access", "user", s.meta.User)
		s.close(reason)
		return errClosed
	}
	defer s.a.clearActive(s)
	defer s.a.inj.ReleaseAll()
	defer s.releasePads()

	s.prefs = s.hello.Prefs
	s.fecInit()
	// A virtual display first: the pipeline captures it (the helper's caps
	// list its output) and the welcome lists it. Released (removed after the
	// linger) once the video has stopped.
	vdNotice := s.openVirtualDisplay(s.prefs)
	defer s.closeVirtualDisplay()
	pipeNotice := s.openPipeline() // the helper's encoders are in the welcome
	defer func() { s.vid().Stop() }()
	if err := s.sendWelcome(); err != nil {
		return err
	}
	for _, n := range []string{vdNotice, pipeNotice} {
		if n != "" {
			s.notice("warn", n)
		}
	}
	defer s.keepDisplayOn()()
	if s.hello.V >= proto.HelloVersionFrameExt {
		go s.wallClockLoop()
	}

	go s.frameSender()
	go s.videoEvents()
	if s.fec.avail {
		go s.fecRepairs()
	}
	if err := s.startVideo(false, ""); err != nil {
		s.notice("error", "Could not start video: "+err.Error())
		return err
	}
	s.startAudio()
	defer s.stopAudio()
	go s.audioFrameLoop()
	go s.rumbleLoop()
	go s.pongSender()
	go s.datagrams()
	go s.cursorLoop()
	go s.roiLoop()
	go s.statsLoop()
	go s.rateLoop()
	return s.controlLoop()
}

// displayRequest keeps the PC's display on while set (platform.DisplayRequest).
type displayRequest interface {
	Set(on bool) error
	Close()
}

// newDisplayRequest is platform.NewDisplayRequest (tests replace it).
var newDisplayRequest = func(reason string) (displayRequest, error) {
	r, err := platform.NewDisplayRequest(reason)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// keepDisplayOn keeps the PC's display on while the session streams,
// whatever the pipeline, and returns the release. Windows turns the display
// off after the power plan's timeout without keyboard or mouse input, and a
// controller's virtual pad does not count as input: a display that goes to
// sleep stops presenting, so the stream freezes, and waking it restarts the
// capture. The native helper's capture also keeps the display on; FFmpeg
// (ddagrab, gfxcapture) does not. Not while the client is hidden
// (displayOn): nobody watches then, and the helper's capture stops too.
func (s *Session) keepDisplayOn() (release func()) {
	r, err := newDisplayRequest("KloudIT Recon is streaming this PC's display")
	if err != nil {
		if !errors.Is(err, platform.ErrUnsupported) {
			s.log.Warn("cannot keep the display on while streaming: it may turn off without keyboard or mouse input", "err", err)
		}
		return func() {}
	}
	s.display = r
	s.displayOn(!s.paused.Load())
	return func() {
		s.displayOn(false)
		r.Close()
	}
}

// displayOn sets the session's display request while the client watches
// and clears it while it is hidden (run's goroutine: its control loop).
func (s *Session) displayOn(on bool) {
	if s.display == nil || s.displayHeld == on {
		return
	}
	if err := s.display.Set(on); err != nil {
		s.log.Warn("cannot keep the display on while streaming: it may turn off without keyboard or mouse input", "on", on, "err", err)
		return
	}
	s.displayHeld = on
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// byeGrace bounds each wait of a takeover on the replaced session (close):
// for the control write in progress, for the bye, and for the client to
// confirm the bye. A live path finishes each in about a round trip; on one
// that died (the old client lost its network, nothing is acknowledged)
// writes would block until their 5 s deadline, one after another, while the
// new session waits for its welcome.
const byeGrace = 500 * time.Millisecond

// close ends a session that another connection replaces. It runs on the new
// session's goroutine before that one's welcome, so it waits on this
// session's control stream for byeGrace at most per step. The bye has to
// reach the client, or it reconnects and takes the session back: closing
// the connection resets the session's streams, the control stream with a
// bye still in flight, so close() first waits for the client to end the
// session, which it does on the bye.
func (s *Session) close(reason string) {
	s.ctrlDL.Lock()
	s.closing = true
	if s.ctrl != nil && (s.ctrlDue.IsZero() || time.Until(s.ctrlDue) > byeGrace) {
		_ = s.ctrl.SetWriteDeadline(time.Now().Add(byeGrace))
	}
	s.ctrlDL.Unlock()
	sent := false
	s.ctrlMu.Lock() // writers queued behind the stalled one now return at once
	if s.ctrl != nil && !s.ctrlTorn {
		b, _ := json.Marshal(proto.Notice{T: "bye", Level: "info", Msg: reason})
		_ = s.ctrl.SetWriteDeadline(time.Now().Add(byeGrace))
		sent = s.writeCtrl(b) == nil
	}
	s.ctrlMu.Unlock()
	if sent {
		select {
		case <-s.c.Context().Done():
		case <-time.After(byeGrace):
		}
	}
	s.cancel()
	s.c.Close(transport.CodeReplaced, reason)
}

func (s *Session) acceptStreams(ctrlCh chan<- transport.BidiStream) {
	for {
		st, err := s.c.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		go func() {
			var kind [1]byte
			_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(st, kind[:]); err != nil {
				st.CancelRead()
				return
			}
			_ = st.SetReadDeadline(time.Time{})
			switch kind[0] {
			case proto.StreamKindControl:
				select {
				case ctrlCh <- st:
				default:
					st.CancelRead()
					st.CancelWrite()
				}
			case proto.StreamKindInput:
				s.inputLoop(st)
			default:
				st.CancelRead()
				st.CancelWrite()
			}
		}()
	}
}

func (s *Session) sendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.ctrlMu.Lock()
	defer s.ctrlMu.Unlock()
	if s.ctrl == nil || s.ctrlTorn {
		return errClosed
	}
	s.ctrlDL.Lock()
	if s.closing {
		s.ctrlDL.Unlock()
		return errClosed
	}
	s.ctrlDue = time.Now().Add(5 * time.Second)
	_ = s.ctrl.SetWriteDeadline(s.ctrlDue)
	s.ctrlDL.Unlock()
	if err := s.writeCtrl(b); err != nil {
		s.ctrlFailed(err)
		return err
	}
	return nil
}

// writeCtrl writes one control message; ctrlMu is held.
func (s *Session) writeCtrl(b []byte) error {
	err := proto.WriteMsg(s.ctrl, b)
	if err != nil {
		s.ctrlTorn = true
	}
	return err
}

// ctrlFailed ends the session after a control write failed (ctrlMu is
// held). A write that reaches its deadline on a stalled path may have sent
// part of its message (quic-go keeps what it queued, and the stream stays
// open): the client cannot parse anything after it. Or it sent none, and the
// client misses the message (a VideoConfig: the picture freezes). Either way
// the control channel is lost while the connection, and the video, may go
// on, and the client would not notice: closing the connection makes it
// reconnect. A takeover (close) that cut the write short ends the session
// itself, with its own code.
func (s *Session) ctrlFailed(err error) {
	s.end("control stream write failed", "err", err)
}

// end ends the session from inside: it is cancelled and the connection
// closed with CodeProtocol and why. Cancelling alone would leave run() in
// controlLoop's read (the client does not close on a notice), so the session
// would hold its capture, audio, virtual display and the agent's active slot
// until the client went away. The client reconnects (no bye) and shows why.
// Not after a takeover's or revocation's close (its own bye and code) or a
// session already ending.
func (s *Session) end(why string, attrs ...any) {
	s.ctrlDL.Lock()
	closing := s.closing
	s.ctrlDL.Unlock()
	if closing || s.ctx.Err() != nil {
		return
	}
	s.log.Warn(why+", ending the session", attrs...)
	s.cancel()
	go s.c.Close(transport.CodeProtocol, why)
}

func (s *Session) notice(level, msg string) {
	s.sendJSON(proto.Notice{T: "notice", Level: level, Msg: msg})
}

func (s *Session) sendWelcome() error {
	w := proto.Welcome{
		T: "welcome", Session: s.id, Host: s.a.pair().Name, OS: runtime.GOOS + "/" + runtime.GOARCH, Version: Version,
		MaxKbps: s.a.cfg.MaxKbps, MaxFPS: s.a.cfg.MaxFPS,
	}
	w.Monitors = s.welcomeMonitors()
	for _, e := range s.encoders() {
		w.Encoders = append(w.Encoders, e.Name)
	}
	w.Features = append(s.a.features(), proto.FeatureRateReport)
	if !s.a.faults.preStageHold {
		w.Features = append(w.Features, proto.FeatureStageHold)
	}
	if s.hello.V >= proto.HelloVersionFrameExt {
		w.Features = append(w.Features, proto.FeatureFrameExt)
	}
	if s.a.cfg.hdr() == proto.HDRAuto {
		w.Features = append(w.Features, proto.FeatureHDR)
	}
	if s.fec.avail {
		w.Features = append(w.Features, proto.FeatureVideoFEC)
	}
	// The test pattern's frames carry their seq as a barcode: FFmpeg's
	// drawbox chain, or the native helper's conversion shader.
	if helper, _, _ := s.onHelper(); s.a.backendFor(s.prefs) == "test" && (s.a.caps.CanDrawBarcode() || helper) {
		w.Features = append(w.Features, proto.FeatureBarcodeSeq)
	}
	w.WallOffsetUs = media.WallOffset(s.a.clock)
	return s.sendJSON(w)
}

// wallClockEvery is how often clients get a fresh wall-clock offset.
const wallClockEvery = 5 * time.Second

// wallClockLoop keeps the client's wall-clock to host-clock offset current (the
// latency probe converts the test page's wall-clock barcode with it).
func (s *Session) wallClockLoop() {
	t := time.NewTicker(wallClockEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		m := proto.Clock{T: "clock", WallOffsetUs: media.WallOffset(s.a.clock)}
		if s.a.faults.tornControl && s.a.tornDone.CompareAndSwap(false, true) {
			s.tearControl(m)
			continue
		}
		if err := s.sendJSON(m); errors.Is(err, errClosed) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Video

func (s *Session) currentPrefs() proto.Prefs {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.prefs
}

// chooseEncoder negotiates the codec between the browser's decoders and the
// host's encoders for a w x h picture (0, 0: size unknown); why says how
// (negotiateEncoder). An encoder that would pad that size (alignment; AV1 on
// RDNA3 at 1920x1080, on either pipeline) gives way to HEVC, else H.264, also
// when the client asks for its codec; notice tells the user why ("" when
// nothing changed). An encoder forced in the host config is kept: its
// padding is announced for the client to crop.
func (s *Session) chooseEncoder(prefs proto.Prefs, w, h int) (e media.EncoderInfo, why, notice string, err error) {
	e, why, err = s.negotiateEncoder(prefs, w, h, true)
	if err != nil || !s.alignment(e).Pads(w, h) {
		return e, why, "", err
	}
	if e.Name == s.a.cfg.Encoder {
		s.log.Debug("forced encoder pads this size, the client crops", "encoder", e.Name, "size", fmt.Sprintf("%dx%d", w, h))
		return e, why, "", nil
	}
	// Hardware encoders first, HEVC before H.264.
	for _, hwOnly := range []bool{true, false} {
		for _, fam := range []string{"hevc", "h264"} {
			if alt, ok := s.pickEncoder(fam, hwOnly, func(c media.EncoderInfo) bool { return !s.alignment(c).Pads(w, h) }); ok {
				a := s.alignment(e)
				s.log.Debug("encoder would pad this size, using another codec", "encoder", e.Name, "size", fmt.Sprintf("%dx%d", w, h),
					"alignment", fmt.Sprintf("%dx%d", a.W, a.H), "using", alt.Name)
				return alt, why + "; " + e.Name + " pads this size", fmt.Sprintf("%s on this GPU needs %d×%d-aligned sizes; using %s",
					familyNames[e.Family], a.W, a.H, familyNames[alt.Family]), nil
			}
		}
	}
	// Nothing else works end-to-end: keep it, VideoConfig announces the crop.
	return e, why, "", nil
}

// alignment returns encoder e's coded-size alignment: for the native
// helper's encoders its caps' (alignW/alignH of the codec; AV1 on RDNA3:
// 64x16), for FFmpeg's the probe's (Caps.Alignment, step 1.7).
func (s *Session) alignment(e media.EncoderInfo) media.Alignment {
	if !e.Helper {
		return s.a.caps.Alignment(e.Name)
	}
	_, _, c := s.onHelper()
	cc := c.Codecs[e.Family]
	return media.Alignment{W: max(cc.AlignW, 1), H: max(cc.AlignH, 1)}
}

// familyNames are the codec families as users know them.
var familyNames = map[string]string{"h264": "H.264", "hevc": "HEVC", "av1": "AV1"}

// clientDecoders returns the browser's decoders by family.
func (s *Session) clientDecoders() map[string]proto.DecoderInfo {
	client := map[string]proto.DecoderInfo{}
	for _, d := range s.hello.Decoders {
		client[d.Family] = d
	}
	return client
}

// usableEncoder reports whether an encoder has not been excluded after
// failing in this session.
func (s *Session) usableEncoder(e media.EncoderInfo) bool {
	s.triedMu.Lock()
	defer s.triedMu.Unlock()
	return !s.tried[e.Name]
}

// pickEncoder returns the host's preferred usable encoder of a family the
// browser decodes (only hardware encoders if hwOnly) that also passes ok. The
// native helper's encoders (hardware) come first while the session runs on it.
func (s *Session) pickEncoder(fam string, hwOnly bool, ok func(media.EncoderInfo) bool) (media.EncoderInfo, bool) {
	if _, dec := s.clientDecoders()[fam]; !dec {
		return media.EncoderInfo{}, false
	}
	for _, e := range s.encoders() {
		if e.Family == fam && (!hwOnly || e.HW) && s.usableEncoder(e) && (ok == nil || ok(e)) {
			return e, true
		}
	}
	return media.EncoderInfo{}, false
}

// negotiateEncoder picks the encoder by configuration, preference and the
// browser's decoders, for a w x h picture (0, 0: unknown); why says how, for
// the log; notify: tell the user when the codec they asked for is not
// available. Automatically: the first tier of autoTiers with a family both
// ends can use, the family in it by chooseFamily (codec.go: HEVC by default, a
// family the client decodes clearly faster instead), with software encoding
// the first in its order. The encoders are the session's (encoders: the
// native helper's first while the session runs on it).
func (s *Session) negotiateEncoder(prefs proto.Prefs, w, h int, notify bool) (e media.EncoderInfo, why string, err error) {
	client := s.clientDecoders()
	usable := s.usableEncoder
	if s.a.cfg.Encoder != "" {
		for _, e := range s.encoders() {
			if e.Name == s.a.cfg.Encoder {
				if _, ok := client[e.Family]; ok && usable(e) {
					return e, "forced in the host config", nil
				}
			}
		}
	}
	if prefs.Codec != "" && prefs.Codec != "auto" {
		if e, ok := s.pickEncoder(prefs.Codec, false, nil); ok {
			return e, "the client's codec setting", nil
		}
		if notify {
			s.notice("warn", fmt.Sprintf("Codec %s is not available end-to-end; choosing automatically.", prefs.Codec))
		}
	}
	policy := s.a.cfg.av1()
	for _, tier := range autoTiers {
		var cands []codecCandidate
		for _, fam := range tier.order {
			d, ok := client[fam]
			if !ok || tier.hwDec && !d.HW {
				continue
			}
			if e, ok := s.pickEncoder(fam, tier.hwEnc, nil); ok {
				cands = append(cands, codecCandidate{enc: e, dec: d, pads: s.alignment(e).Pads(w, h)})
			}
		}
		if len(cands) > 0 {
			// Software encoding keeps its order: the host's CPU cost, which
			// the client's decode times do not tell.
			c, why := cands[0], "first choice"
			if tier.hwEnc {
				c, why = chooseFamily(cands, policy, w, h)
			}
			return c.enc, "auto, " + tier.name + ": " + why, nil
		}
	}
	return media.EncoderInfo{}, "", errors.New("no codec is supported by both this browser and the host")
}

// sessionParams is the encoder-independent part of buildParams for prefs on
// monitor mon: frame rate and bitrate within the host's limits, cursor,
// capture timestamps (frameExt: the client parses the frame extension) and
// the FFmpeg capture source of backend (backendFor, or the session's
// captureBackend).
func (a *Agent) sessionParams(prefs proto.Prefs, mon platform.Monitor, backend string, frameExt bool) media.Params {
	cfg := a.cfg
	fps := prefs.FPS
	if fps <= 0 {
		fps = cfg.DefaultFPS
	}
	if fps > cfg.MaxFPS {
		fps = cfg.MaxFPS
	}
	if mon.Hz > 0 && fps > mon.Hz && backend != "test" {
		fps = mon.Hz // capturing faster than the display refreshes only duplicates frames
	}
	if fps < 10 {
		fps = 10
	}
	kbps := prefs.BitrateKbps
	if kbps <= 0 {
		kbps = cfg.DefaultKbps
	}
	if kbps > cfg.MaxKbps {
		kbps = cfg.MaxKbps
	}
	if kbps < 500 {
		kbps = 500
	}
	p := media.Params{
		FPS:          fps,
		BitrateKbps:  kbps,
		Quality:      prefs.Quality,
		Adaptive:     prefs.AdaptiveBitrate(),
		DrawCursor:   cfg.DrawCursor || prefs.Cursor == "video" || !a.cursorSupported(),
		CaptureClock: frameExt && cfg.CaptureTimestamps != "off" && a.caps.CanStampCapture(),
		GPUPriority:  cfg.gpuPriority(),
	}
	w, h := prefs.Width, prefs.Height
	if w > 0 && h > 0 && (w >= mon.W && h >= mon.H) {
		w, h = 0, 0 // never upscale
	}
	switch backend {
	case "test":
		p.Source = media.Source{Backend: "test", NativeW: cfg.TestWidth, NativeH: cfg.TestHeight}
		if w > 0 && h > 0 {
			p.Source.NativeW, p.Source.NativeH = w&^1, h&^1
		}
		p.TestPad = cfg.TestPad
	case "x11grab":
		p.Source = media.Source{Backend: "x11grab", Display: cfg.X11Display, X: mon.X, Y: mon.Y, NativeW: mon.W, NativeH: mon.H}
		p.Width, p.Height = w, h
	case "gfxcapture":
		p.Source = media.Source{Backend: "gfxcapture", HMonitor: mon.HMonitor, Window: prefs.Window, NativeW: mon.W, NativeH: mon.H}
		p.Width, p.Height = w, h
	case "ddagrab", "amf":
		// "amf" needs the encoder: buildParams (useAMFCapture) decides. Both
		// capture the whole monitor at its native size.
		out := mon.DXGIOutput
		if out < 0 {
			out = mon.Index
		}
		p.Source = media.Source{Backend: "ddagrab", Output: out, NativeW: mon.W, NativeH: mon.H}
	}
	// The test pattern carries each frame's Seq as a barcode (welcome feature
	// barcode-seq): the client checks the picture it draws against the header.
	p.Barcode = backend == "test" && a.caps.CanDrawBarcode()
	return p
}

// monitorFor returns the monitor a session with prefs captures: the one it
// names, else the first.
func (a *Agent) monitorFor(prefs proto.Prefs) platform.Monitor {
	mons := a.monitors()
	if prefs.Monitor >= 0 && prefs.Monitor < len(mons) {
		return mons[prefs.Monitor]
	}
	return mons[0]
}

// buildParams builds the next generation's parameters for prefs: the monitor
// it captures (the session's virtual display, else the one prefs name; input
// and the cursor map to its rectangle), the pipeline (leaving the helper when
// it cannot serve them) and the encoder.
func (s *Session) buildParams(prefs proto.Prefs) (media.Params, error) {
	mon, vd := s.captureMonitor(prefs)
	virt := vd != nil
	s.prefsMu.Lock()
	s.monitor = mon
	s.prefsMu.Unlock()
	s.a.inj.SetTarget(input.Rect{X: mon.X, Y: mon.Y, W: mon.W, H: mon.H})

	backend := s.captureBackend(prefs, mon, virt)
	// Capture timestamps only reach clients that parse the frame extension.
	p := s.a.sessionParams(prefs, mon, backend, s.hello.V >= proto.HelloVersionFrameExt)
	if helper, _, c := s.onHelper(); helper {
		why := s.helperBlocker(prefs, p.DrawCursor, &c)
		if why == "" {
			why = s.adapterBlocker(prefs, mon, &c) // another monitor, on another GPU
		}
		if why != "" {
			s.leaveHelper(why)
		}
	}
	outW, outH := p.OutputSize()
	enc, why, notice, err := s.chooseEncoder(prefs, outW, outH)
	if err != nil {
		return media.Params{}, err
	}
	if helper, encs, _ := s.onHelper(); helper && !enc.Helper {
		s.leaveHelper(fmt.Sprintf("the codec negotiated with this browser (%s) is not one of the helper's (%s)", enc.Name, encoderNames(encs)))
	}
	p.Encoder = enc
	switch {
	case enc.Helper:
		s.helperSource(&p, prefs, mon)
	case backend == "amf":
		s.useAMFCapture(&p, mon, virt)
	}
	if virt && !enc.Helper && p.Source.Backend == "ddagrab" && mon.DXGIOutput < 0 {
		// On FFmpeg (the helper gave up) without gfxcapture: ddagrab's
		// output_idx counts the outputs of DXGI adapter 0 only.
		// Removed before the monitor is looked up again: the restore can
		// move it.
		const why = "FFmpeg cannot capture it: it is not an output of DXGI adapter 0"
		if s.takeVirtualDisplay(vd, why) {
			s.removeVirtualDisplay(vd, why)
		}
		return s.buildParams(prefs)
	}
	s.chooseHDR(&p, prefs)
	s.triedMu.Lock()
	p.Usage = s.usage[enc.Name]
	s.triedMu.Unlock()
	if s.thinOK() {
		// Temporal SVC (Phase 5): two layers where the native helper's
		// encoder has them (HelperVideo.withCaps decides and logs), so
		// frameSender can thin the enhancement layer under congestion.
		p.SVCLayers = s.a.cfg.SVCLayers()
	}
	// Once per change: buildParams runs again for every restart.
	s.prefsMu.Lock()
	choice := enc.Name + " " + why
	newChoice := choice != s.codecWhy
	s.codecWhy = choice
	repeat := notice == s.alignNotice
	s.alignNotice = notice
	s.prefsMu.Unlock()
	if newChoice {
		s.log.Info("codec choice", "encoder", enc.Name, "family", enc.Family, "reason", why,
			"size", fmt.Sprintf("%dx%d", outW, outH), "av1", s.a.cfg.av1(), "decoders", decoderSummary(s.hello.Decoders))
	}
	if notice != "" && !repeat {
		s.log.Info("coded-size alignment", "notice", notice, "size", fmt.Sprintf("%dx%d", outW, outH), "encoder", enc.Name)
		s.notice("info", notice)
	}
	return p, nil
}

// ProbeSample returns, per encoder, the parameters of the session "recon-host
// probe" prints the ffmpeg command line for: a current browser client at its
// default settings (the first monitor at its native size, 60 fps, 30 Mbit/s,
// balanced quality, local cursor) on this host's configuration, by the
// agent's own rules. Call platform.EnableDPIAwareness first, as the agent does.
func ProbeSample(cfg *Config, caps *media.Caps) func(media.EncoderInfo) media.Params {
	a := &Agent{cfg: cfg, caps: caps}
	return a.probeSample(a.monitors()[0])
}

func (a *Agent) probeSample(mon platform.Monitor) func(media.EncoderInfo) media.Params {
	prefs := proto.Prefs{FPS: 60, BitrateKbps: 30000, Quality: "balanced", Cursor: "local"}
	backend := a.backendFor(prefs)
	p := a.sessionParams(prefs, mon, backend, true)
	return func(enc media.EncoderInfo) media.Params {
		q := p
		q.Encoder = enc
		if backend == "amf" && a.amfCaptureBlocker(enc, q.DrawCursor, mon) == "" { // as useAMFCapture
			q.Source.Backend = "amf"
		}
		return q
	}
}

// useAMFCapture switches p from ddagrab to AMD Direct Capture of the same
// monitor (capture "amf", experimental) unless amfCaptureBlocker, the monitor
// being the session's virtual display (virt: AMD Direct Capture reads the
// GPU's own display outputs, which never scan out an IddCx monitor) or an
// earlier failure in this session rules it out; then p stays on ddagrab and
// the reason is logged once per change.
func (s *Session) useAMFCapture(p *media.Params, mon platform.Monitor, virt bool) {
	why := s.a.amfCaptureBlocker(p.Encoder, p.DrawCursor, mon)
	if virt {
		why = fmt.Sprintf("monitor %s is a virtual display, which AMD Direct Capture cannot capture", mon.Name)
	}
	if why == "" && s.amfFailed.Load() {
		why = "it failed earlier in this session"
	}
	s.prefsMu.Lock()
	repeat := why == s.amfFallback
	s.amfFallback = why
	s.prefsMu.Unlock()
	if why == "" {
		p.Source.Backend = "amf" // Output: the monitor's DXGI output index
		return
	}
	if !repeat {
		s.log.Info("AMD Direct Capture (capture \"amf\") not used, capturing with ddagrab", "reason", why)
	}
}

// amfCaptureBlocker returns why AMD Direct Capture cannot capture mon for enc,
// or "" if it can. vsrc_amf hands AMF surfaces to the encoder, which only an
// AMF encoder takes (CanCaptureAMF); it has no cursor option, so a video that
// must carry the cursor stays on ddagrab (draw_mouse) until the driver is
// shown to include it (docs/VENDOR_NOTES.md 1.6). Its monitor_index is taken
// to be the DXGI output index on adapter 0, on whose device FFmpeg opens AMF
// (VERIFY): a monitor that is not an output of adapter 0 (another GPU, an
// IddCx virtual display) has none. vsrc_amf ignores the capture's rotation
// (AMF leaves it to the consumer; ddagrab rotates), so a rotated monitor
// stays on ddagrab.
func (a *Agent) amfCaptureBlocker(enc media.EncoderInfo, drawCursor bool, mon platform.Monitor) string {
	if err := a.caps.CanCaptureAMF(enc); err != nil {
		return err.Error()
	}
	switch {
	case drawCursor:
		return "the video must carry the cursor"
	case mon.DXGIOutput < 0 || mon.DXGIOutput > 8:
		return fmt.Sprintf("monitor %d is not output 0-8 of DXGI adapter 0", mon.Index)
	case mon.Rotated:
		return fmt.Sprintf("monitor %d is rotated", mon.Index)
	}
	return ""
}

// backendFor picks the capture backend for the requested preferences. AMD
// Direct Capture ("amf") is opt-in, never chosen automatically, and is only
// a request: buildParams falls back to ddagrab when amfCaptureBlocker rules it
// out for the session's encoder.
func (a *Agent) backendFor(prefs proto.Prefs) string {
	switch a.cfg.Capture {
	case "ddagrab", "gfxcapture", "x11grab", "test", "amf":
		return a.cfg.Capture
	}
	gfx := a.caps.Filters["gfxcapture"]
	dda := a.caps.Filters["ddagrab"]
	needScale := prefs.Width > 0 && prefs.Height > 0
	switch {
	case gfx && (prefs.Window != "" || needScale):
		return "gfxcapture"
	case dda:
		return "ddagrab"
	case gfx:
		return "gfxcapture"
	case runtime.GOOS == "linux":
		return "x11grab"
	}
	return "test"
}

// startVideo starts a new encoder generation. urgent discards the current one
// immediately (used when the client needs a key frame right away). The
// bitrate and frame rate are the rate controller's, at most the settings': a
// congestion back-off stays in effect for every restart until the controller
// raises the bitrate again or a video settings change resets it, so a
// key-frame restart after a loss does not go back to the full bitrate.
//
// While the client is hidden (paused) nothing starts: resume starts the next
// generation with the settings and rate of that moment. A key-frame request,
// a settings change or a rate change in between would otherwise run an
// encoder for frames videoEvents drops.
func (s *Session) startVideo(urgent bool, reason string) error {
	return s.startVideoLog(urgent, reason, false)
}

// startVideoLog is startVideo; quiet: a restart that puts a change of the
// rate controller into effect (setRate on a pipeline that cannot change its
// bitrate live: FFmpeg), whose "restarting video" and the generation's
// "starting encoder" and "encoder ready" are debug lines (media.Params.Quiet).
// Where the path carries less than the setting the controller changes the
// rate about once a second; applyRate logs the changes (rateLog), "stream
// stats" the target every 10 s.
func (s *Session) startVideoLog(urgent bool, reason string, quiet bool) error {
	if s.paused.Load() {
		s.log.Debug("client hidden: video starts on resume", "reason", reason)
		return nil
	}
	prefs := s.currentPrefs()
	p, err := s.buildParams(prefs)
	if err != nil {
		return err
	}
	s.rate.setAdaptive(p.Adaptive)
	p.BitrateKbps, p.FPS = s.rate.target(p.BitrateKbps, p.FPS)
	p.Quiet = quiet
	if reason != "" {
		lvl := slog.LevelInfo
		if quiet {
			lvl = slog.LevelDebug
		}
		s.log.Log(context.Background(), lvl, "restarting video", "reason", reason, "urgent", urgent)
	}
	// An overlapped start leaves the active generation streaming at its
	// bitrate until the new one is live (its VideoConfig sets the target
	// then): pacing below that would queue its frames at the host, which on
	// a path that carries them overflows the frame queue for nothing.
	pace := p
	if a, ok := s.vid().Active(); ok && !urgent && a.BitrateKbps > pace.BitrateKbps {
		pace.BitrateKbps = a.BitrateKbps
	}
	s.setCongestionTarget(pace)
	return s.vid().Start(p, urgent)
}

// ccOverheadKbps is the media congestion controller's allowance for packet and
// stream headers and the cursor and input datagrams, on top of the video and
// audio bitrates.
const ccOverheadKbps = 200

// ccTarget is what the session sends besides audio and overhead.
type ccTarget struct {
	videoKbps     int64
	frameInterval time.Duration
}

// setCongestionTarget hands the encoder's bitrate and frame rate to the media
// congestion controller (host config "congestion": "media"), which paces at
// 1.2 × (video + audio + ccOverheadKbps). A no-op with reno.
func (s *Session) setCongestionTarget(p media.Params) {
	if p.FPS <= 0 {
		return
	}
	s.ccTarget.Store(&ccTarget{videoKbps: int64(p.BitrateKbps), frameInterval: time.Second / time.Duration(p.FPS)})
	if kbps := s.applyCongestionTarget(); kbps > 0 {
		s.log.Debug("media congestion control", "target_kbps", kbps, "video_kbps", p.BitrateKbps, "fps", p.FPS)
	}
}

// applyCongestionTarget sets the target on the media controller of the
// connection's current path and returns it in kbit/s (0: reno). A path
// migration, also the client's NAT rebinding, replaces the controller with one
// at the defaults, so frameSender re-applies the target for every frame; that
// also picks up audio starting or stopping.
func (s *Session) applyCongestionTarget() int64 {
	t := s.ccTarget.Load()
	if t == nil {
		return 0
	}
	m := transport.MediaControl(s.c)
	if m == nil {
		return 0
	}
	// Frames sent as shards (fec.go) carry parity and shard headers too.
	kbps := t.videoKbps + s.fecOverheadKbps(t.videoKbps) + s.audioKbps.Load() + ccOverheadKbps
	m.SetTarget(kbps*1000, t.frameInterval)
	return kbps
}

func (s *Session) videoEvents() {
	for {
		var ev media.VideoEvent
		select {
		case <-s.ctx.Done():
			return
		case <-s.pipeSwap:
			continue // the pipeline changed: read the new one's events
		case ev = <-s.vid().Events():
		}
		switch {
		case ev.Err != nil && ev.Fallback:
			s.helperFallback(ev)
		case ev.Err != nil && ev.Restarted:
			// The pipeline replaces its failed encoder itself (the native
			// helper); a new generation with a key frame follows.
			s.notice("warn", "Video encoder restarted ("+trunc(ev.Err.Error(), 160)+")")
		case ev.Err != nil:
			s.handleEncoderFailure(ev)
		case ev.Lost != nil:
			s.encoderLost(ev.Lost)
		case ev.Recovered != nil:
			s.lossRecovered(ev.Recovered)
		case ev.Capture != nil:
			s.captureChanged(ev.Capture)
		case ev.Rate != nil:
			// The live generation's encoder changed its bitrate or frame
			// rate in place: the client's config of it is updated. The
			// rate controller hears its own target while a static desktop
			// caps the encoder (activity.go controllerKbps).
			target, ceiling := s.rate.kbps()
			pc := s.vid().Capabilities()
			s.rate.live(s.static.rate(s.static.clock(), ev.Rate.Kbps, target, pc.LiveBitrate && !pc.LiveBitrateFlush), ev.Rate.FPS)
			s.sendJSON(proto.Rate{T: "rate", Gen: ev.Rate.Gen, BitrateKbps: ev.Rate.Kbps, FPS: ev.Rate.FPS, MaxBitrateKbps: ceiling})
		case ev.Config != nil:
			s.encoderLive()
			s.videoUp.Store(true)
			s.cursorInVideo.Store(ev.CursorInVideo)
			c := *ev.Config
			if proto.RefRecovery(c.Recovery) && s.hello.V < proto.HelloVersionRecovery {
				// Older clients cannot wait for a recovery frame: after a
				// loss they ask for a key frame (an IDR in the encoder).
				c.Recovery = proto.RecoveryKeyframe
			}
			if r := s.a.faults.recovery; r != "" {
				c.Recovery = r
			}
			target, ceiling := s.rate.kbps()
			c.MaxBitrateKbps = ceiling
			pc := s.vid().Capabilities()
			s.rate.live(s.static.generation(s.static.clock(), c.BitrateKbps, target, pc.LiveBitrate && !pc.LiveBitrateFlush), c.FPS)
			s.setCongestionTarget(media.Params{BitrateKbps: c.BitrateKbps, FPS: c.FPS})
			s.healConfig(&c, ev.HealFrames)
			if s.paused.Load() {
				// Its frames are dropped: a hidden client would wait for
				// its key frame and ask again and again. Resume starts a
				// generation with a config of its own.
				continue
			}
			s.sendJSON(&c)
		case ev.Frame != nil:
			if s.paused.Load() {
				continue
			}
			if n := s.a.faults.stillAfter; n > 0 && ev.Frame.Seq >= uint32(n) {
				continue // test hook: a still desktop, the source sends nothing
			}
			s.healFrame(ev.Frame)
			if ev.Frame.Reencoded {
				s.stats.reencoded.Add(1)
			}
			s.rate.output(len(ev.Frame.Data))
			s.staticFrame(ev.Frame)
			if s.a.cfg != nil && s.a.cfg.CaptureTimestamps == "off" {
				// No capture stamps on any pipeline: FFmpeg then makes
				// none, the native helper always measures its own.
				ev.Frame.CaptureUs, ev.Frame.PresentUs = 0, 0
			}
			select {
			case s.frameQ <- ev.Frame:
				// A newer frame is ready: a frame stream past its deadline
				// gives way to it (rung 1).
				s.checkOut()
			default:
				// The network cannot keep up: drop what is queued and this
				// frame (the client is told at once) and cut the bitrate.
				// The loss is answered by the ladder: a recovery frame
				// where the encoder makes one (the cut then needs no key
				// frame), else a fresh key frame right away (with the cut:
				// an urgent restart or an IDR). Within 2 s of the last cut
				// the bitrate stays, but the key frame does not wait
				// either: the dropped frames were the newest ones. A
				// generation starting at a lower bitrate takes over then
				// (urgentRestart, or only that where a recovery frame
				// answers the loss).
				dropped := append(s.drainQueue(), ev.Frame)
				s.logOverflow(dropped)
				s.reportDropped(dropped, "queue overflow")
				recovered := s.overflowLoss(dropped)
				if !s.overflowCut(recovered) {
					switch {
					case !recovered:
						s.urgentRestart("queue overflow")
					case !s.vid().Capabilities().LiveBitrate:
						s.takeover("queue overflow")
					}
				}
			}
		}
	}
}

// logOverflow records what led to a frame-queue overflow: the span of the
// dropped frames' timestamps and of their encodeDone times (frames the
// encoder delivered in a burst of its own, or a sender that stood still for
// their whole span), what the sender was doing and for how long, and the
// media congestion controller's window.
func (s *Session) logOverflow(fs []*media.Frame) {
	attrs := []any{"frames", len(fs)}
	if n := len(fs); n > 1 {
		attrs = append(attrs, "pts_span_ms", (fs[n-1].PtsUs-fs[0].PtsUs)/1000,
			"encode_done_span_ms", (int64(fs[n-1].EncodeDoneUs)-int64(fs[0].EncodeDoneUs))/1000)
	}
	stage := "idle"
	if at := s.sendSince.Load(); at != 0 {
		stage = "write"
		if s.sendOpening.Load() {
			stage = "open stream"
		} else if w := s.windowSince.Load(); w != 0 {
			stage = "window" // holding the frame for the frames in flight (GUIDE 2.7)
			attrs = append(attrs, "window_ms", time.Since(time.Unix(0, w)).Milliseconds())
		}
		attrs = append(attrs, "sender_busy_ms", time.Since(time.Unix(0, at)).Milliseconds())
	}
	attrs = append(attrs, "sender", stage)
	if m := transport.MediaControl(s.c); m != nil {
		st := m.Stats()
		attrs = append(attrs, "cc_target_kbps", st.TargetBitrate/1000, "cc_window", st.Window, "cc_collapses", st.Collapses,
			"cc_lost", st.LostPackets)
	}
	s.log.Info("frame queue overflow", attrs...)
}

// drainQueue discards the queued frames and returns them, oldest first.
func (s *Session) drainQueue() []*media.Frame {
	var fs []*media.Frame
	for {
		select {
		case f := <-s.frameQ:
			fs = append(fs, f)
		default:
			return fs
		}
	}
}

// reportDropped tells the client about frames it will never get
// ({"t":"dropped"}, one message per run of consecutive frames of a
// generation): it acts on the loss at once instead of waiting for them, and
// knows that any other gap is a late frame. frames are in send order. The
// messages go out asynchronously so a slow control stream never holds up the
// frame path.
func (s *Session) reportDropped(frames []*media.Frame, why string) {
	var msgs []proto.Dropped
	for _, f := range frames {
		if n := len(msgs); n > 0 && msgs[n-1].Gen == f.Gen && msgs[n-1].FromSeq+uint32(msgs[n-1].Count) == f.Seq {
			msgs[n-1].Count++
			continue
		}
		msgs = append(msgs, proto.Dropped{T: "dropped", Gen: f.Gen, FromSeq: f.Seq, Count: 1})
	}
	if len(msgs) == 0 {
		return
	}
	s.stats.dropped.Add(int64(len(frames)))
	for _, m := range msgs {
		s.log.Info("frames dropped", "why", why, "gen", m.Gen, "from_seq", m.FromSeq, "count", m.Count)
	}
	s.watchHeal(frames)
	go func() {
		for _, m := range msgs {
			s.sendJSON(m)
		}
	}()
}

// healWatch is a reported loss in a generation with recovery "skip": the
// picture is whole again once the encoder has produced frame seq.
type healWatch struct{ seq uint32 }

// healConfig notes the generation going live. Its losses heal by themselves
// when it announces recovery "skip" from an encoder that refreshes
// (healFrames > 0; a skip forced by the test hook on an encoder without intra
// refresh never heals and is not watched). Its key frame also ends the damage
// of an earlier generation's loss.
func (s *Session) healConfig(c *proto.VideoConfig, healFrames int) {
	s.healMu.Lock()
	defer s.healMu.Unlock()
	s.healGen, s.healFrames, s.heal, s.liveRecovery = c.Gen, 0, nil, c.Recovery
	s.genFamily[c.Gen] = c.Family
	if c.Recovery == proto.RecoverySkip {
		s.healFrames = healFrames
	}
}

// watchHeal bounds the damage of dropped frames (the client skips them under
// recovery "skip") in time. Intra refresh restores the picture healFrames
// frames after a loss, but the capture sources send a frame only when the
// screen changes (ddagrab without dup_frames, gfxcapture), so on a still
// desktop that never happens, and a game rendering below the stream's rate
// takes longer than the frame count assumes. When the encoder has not
// produced that frame within media.MaxHeal of the first loss that has not
// healed (later losses only move the frame on), healDue restarts it.
func (s *Session) watchHeal(frames []*media.Frame) {
	s.healMu.Lock()
	defer s.healMu.Unlock()
	for _, f := range frames {
		if s.healFrames == 0 || f.Gen != s.healGen {
			continue
		}
		seq := f.Seq + uint32(s.healFrames)
		if w := s.heal; w != nil {
			w.seq = max(w.seq, seq)
			continue
		}
		w := &healWatch{seq: seq}
		s.heal = w
		time.AfterFunc(media.MaxHeal, func() { s.healDue(w) })
	}
}

// healFrame ends the watch when the encoder produces the frame from which the
// picture is whole again.
func (s *Session) healFrame(f *media.Frame) {
	s.healMu.Lock()
	if w := s.heal; w != nil && f.Gen == s.healGen && f.Seq >= w.seq {
		s.heal = nil
	}
	s.healMu.Unlock()
}

// healDue runs media.MaxHeal after watch w opened: if it is still open, the
// picture has not healed, and an overlapped restart delivers a key frame (the
// damaged picture stays on screen until then instead of freezing). A restart
// under way already delivers one.
func (s *Session) healDue(w *healWatch) {
	s.healMu.Lock()
	due := s.heal == w
	if due {
		s.heal = nil
	}
	s.healMu.Unlock()
	if !due || s.ctx.Err() != nil || s.paused.Load() {
		return
	}
	s.kickMu.Lock()
	if time.Since(s.lastKick) < 500*time.Millisecond {
		s.kickMu.Unlock()
		return
	}
	s.lastKick = time.Now()
	s.kickMu.Unlock()
	var err error
	if st := ladder(ladderIn{event: lossUnhealed, forceIDR: s.vid().Capabilities().ForceIDR}); st.restart {
		err = s.startVideo(false, "loss not healed")
	} else {
		err = s.keyframe("loss not healed")
	}
	if err != nil {
		s.log.Warn("restart after an unhealed loss failed", "err", err)
	}
}

// encoderLive resets the failure counts when a generation goes live: capture
// and encoder work again, so earlier failures no longer count.
func (s *Session) encoderLive() {
	s.failures = 0
	s.triedMu.Lock()
	clear(s.encFails)
	s.triedMu.Unlock()
}

// handleEncoderFailure handles a failure event (ev.Err) of the video manager.
// Only a generation that failed to start because of its encoder counts
// against the encoder (encoderFailed). A generation that had gone live, or
// whose capture source failed, does not: a capture outage of a few seconds
// (a UAC prompt, the lock screen, a display mode change) fails every restart
// until it ends and must not move the session to another encoder or usage.
// Nor does a generation that captured with AMD Direct Capture: the session
// leaves that capture (noteCaptureFailure), and the restart on ddagrab tests
// the encoder.
func (s *Session) handleEncoderFailure(ev media.VideoEvent) {
	err := ev.Err
	s.failures++
	s.log.Warn("encoder failed", "err", err, "attempt", s.failures, "live", ev.Live, "encoder_fault", ev.EncoderFault)
	if !s.noteCaptureFailure(ev) && ev.Failed != nil && ev.EncoderFault && !ev.Live {
		s.encoderFailed(*ev.Failed)
	}
	if s.failures > 6 {
		s.notice("error", "Video encoder keeps failing: "+err.Error())
		s.end("video encoder keeps failing", "failures", s.failures)
		return
	}
	s.notice("warn", "Video encoder restarted ("+trunc(err.Error(), 160)+")")
	time.AfterFunc(time.Duration(s.failures)*300*time.Millisecond, func() {
		if s.ctx.Err() == nil {
			if err := s.startVideo(true, "encoder failure"); err != nil {
				s.notice("error", "Could not restart video: "+err.Error())
			}
		}
	})
}

// helperFallback moves the session to FFmpeg for good after the native
// helper failed too often, and starts the first FFmpeg generation at once.
func (s *Session) helperFallback(ev media.VideoEvent) {
	s.log.Warn("native encoder helper gave up, streaming with FFmpeg for the rest of the session", "err", ev.Err)
	s.leaveHelper("the helper failed too often: " + ev.Err.Error())
	s.notice("warn", "The native encoder failed repeatedly; streaming with FFmpeg for the rest of this session.")
	if s.ctx.Err() == nil && !s.paused.Load() {
		if err := s.startVideo(true, "helper fallback"); err != nil {
			s.notice("error", "Could not start video: "+err.Error())
		}
	}
}

// encoderLost handles frames the pipeline lost before they reached the
// session (the native helper's ring was full: this process fell behind, or
// an encoder error): the client is told at once, as for frames the session
// dropped, and the ladder answers the loss.
func (s *Session) encoderLost(l *media.LostFrames) {
	frames := make([]*media.Frame, 0, l.Count)
	for i := 0; i < l.Count; i++ {
		frames = append(frames, &media.Frame{Gen: l.Gen, Seq: l.From + uint32(i)})
	}
	s.reportDropped(frames, l.Why)
	s.loss(lossConfirmed, l.Gen, l.From, l.Why)
}

// lostFrame handles a frame the session could not send (rung 1 cancelled
// it, its stream failed, the test hook's drops): the client is told, and the
// ladder answers the loss.
func (s *Session) lostFrame(f *media.Frame, why string) {
	s.reportDropped([]*media.Frame{f}, why)
	s.loss(lossConfirmed, f.Gen, f.Seq, why)
}

// ladderIn returns a ladder question about the live stream: its generation
// and the recovery mode its client was told.
func (s *Session) ladderIn(ev lossEvent, gen uint8, seq uint32) ladderIn {
	s.healMu.Lock()
	defer s.healMu.Unlock()
	return ladderIn{event: ev, gen: gen, seq: seq, live: s.healGen, mode: s.liveRecovery}
}

// loss answers a confirmed loss of generation gen's frames from seq on (ev
// lossConfirmed, or lossOverflow; why: what lost them) with the ladder's
// rung and returns it. Rung 2 (reference recovery, VideoConfig.Recovery ltr /
// invalidate, GUIDE 3.5): the encoder codes its next frame from frames the
// client still holds (Pipeline.Recover: an acknowledged LTR, or invalidating
// the lost frames), and the frames up to it are not sent; where the pipeline
// cannot, rung 4. Rung 3 ("skip"): nothing, the client skips the frame and
// the encoder heals it (watchHeal). Rung 4: a key frame (an IDR in the
// running encoder on the helper, a new generation on FFmpeg), and nothing
// else of the generation is sent until it; an overflow's key frame comes with
// the bitrate cut (the caller). A generation the client has left needs
// nothing.
func (s *Session) loss(ev lossEvent, gen uint8, seq uint32, why string) ladderStep {
	in := s.ladderIn(ev, gen, seq)
	v := s.vid()
	if v != nil {
		in.forceIDR = v.Capabilities().ForceIDR
	}
	st := ladder(in)
	if st.act == actRecover {
		// A loss that joins the client's wait (its answer lost) is recovered
		// from the wait's first loss (sendState.recoverFrom).
		from := s.send.recoverFrom(gen, seq)
		err := v.Recover(gen, from)
		if err == nil {
			attrs := []any{"gen", gen, "from_seq", seq, "why", why}
			if from != seq {
				attrs = append(attrs, "wait_from", from)
			}
			s.log.Info("recovering from a loss", attrs...)
			s.send.setWait(gen, seq, st.rung, false)
			s.checkOut()
			return st
		}
		in.recoverFailed = true
		st = ladder(in)
		st.why += ": " + err.Error()
	}
	if st.act != actKeyframe {
		return st
	}
	ref := proto.RefRecovery(in.mode)
	if ref {
		s.stats.recoveredByKey.Add(1)
		s.log.Info("no recovery frame possible, forcing a key frame", "gen", gen, "from_seq", seq, "why", why, "ladder", st.why)
	}
	// Under reference recovery the client waits for a key frame (or a
	// recovery frame); under "keyframe" it gives the generation up and asks
	// for a new one. Under "skip" it decodes on until the key frame.
	if ref || in.mode == proto.RecoveryKeyframe {
		s.send.setWait(gen, seq, st.rung, !ref)
		s.checkOut()
	}
	if ev != lossOverflow {
		s.requestKeyframe("frame lost")
	}
	return st
}

// overflowLoss answers a frame-queue overflow's dropped frames (oldest
// first) with the ladder: it reports whether a recovery frame answers them
// (rung 2); otherwise the bitrate cut that follows delivers the key frame.
func (s *Session) overflowLoss(dropped []*media.Frame) bool {
	in := s.ladderIn(lossOverflow, 0, 0)
	for _, f := range dropped {
		if f.Gen == in.live {
			return s.loss(lossOverflow, f.Gen, f.Seq, "queue overflow").act == actRecover
		}
	}
	return false // frames of a generation the client has left
}

// checkOut asks the ladder about the frame streams being written: one past
// its deadline while a newer frame is ready is cancelled and lost (rung 1),
// one the client would discard (it waits for the answer to a loss before
// it) is stopped. Runs at a stream's deadline, when a frame is queued and
// when a wait begins.
func (s *Session) checkOut() {
	if s.ctx.Err() != nil {
		return
	}
	for _, c := range s.send.due(s.ladderIn(lossOutgoing, 0, 0), len(s.frameQ) > 0, time.Now()) {
		f := c.of.f
		if c.step.act == actDiscard {
			if c.of.st != nil { // nil: a shard frame the video window holds (sendFEC)
				c.of.st.CancelWrite()
			}
			s.discard(f, c.step)
			c.of.release()
			continue
		}
		s.stats.cancelled.Add(1)
		// reliable_bytes: what the reset still delivers (GUIDE 2.4: the
		// header where the client has partial delivery and it was marked
		// before the cancel, sendState.markReliable).
		s.log.Info("frame stream cancelled", "gen", f.Gen, "seq", f.Seq, "why", c.step.why, "reliable_bytes", c.of.relSent.Load(),
			"age_ms", c.age.Milliseconds(), "deadline_ms", c.of.deadline.Milliseconds())
		// The loss first: the reset (or the release of a frame the video
		// window holds) frees frameSender, whose next frames must find the
		// wait for the answer to it.
		s.lostFrame(f, "deadline")
		if c.of.st != nil {
			c.of.st.CancelWrite()
		}
		c.of.release()
	}
}

// discard reports a frame the session does not send (or stops sending)
// because the client waits for the answer to a loss before it (step: the
// ladder's, actDiscard): the client would discard it anyway. Consecutive
// discards are reported as one run (discardRun).
func (s *Session) discard(f *media.Frame, step ladderStep) {
	s.stats.discarded.Add(1)
	why := "awaiting recovery frame"
	if step.rung == 4 {
		why = "awaiting key frame"
	}
	done, doneWhy, begun := s.discards.add(f.Gen, f.Seq, why)
	if done != nil {
		s.reportDropped(done, doneWhy)
	}
	if begun != 0 {
		time.AfterFunc(discardReportAfter, func() { s.reportDiscards(begun) })
	}
}

// reportDiscards reports the run of discarded frames id (0: whichever is
// open) unless it was reported already.
func (s *Session) reportDiscards(id uint64) {
	if fs, why := s.discards.take(id); fs != nil && s.ctx.Err() == nil {
		s.reportDropped(fs, why)
	}
}

// lossRecovered logs the encoder's answer to a Recover (Session.loss): the
// frame that recovered the loss (one per loss: under the "wifi" profile,
// GUIDE T5 wants >= 90 % of them recovery frames, not key frames).
func (s *Session) lossRecovered(r *media.Recovered) {
	by := "recovery frame"
	if r.Key {
		by = "key frame"
		s.stats.recoveredByKey.Add(1)
	} else {
		s.stats.recovered.Add(1)
	}
	s.log.Info("loss recovered", "gen", r.Gen, "from_seq", r.From, "by", by, "at", fmt.Sprintf("%d/%d", r.AtGen, r.AtSeq),
		"wait_ms", r.Wait.Milliseconds())
}

// resizeSettle is how long a capture source must keep its new size before
// the stream restarts at it: a window being resized reports every frame.
const resizeSettle = 300 * time.Millisecond

// captureChanged follows the native helper's capture source: a new size or
// rotation restarts the stream at the new size (a new helper, overlapped; and
// maps input to the monitor's new geometry) once it settled, a lost capture
// (secure desktop, mode switch) is shown to the user. Called by videoEvents
// only.
func (s *Session) captureChanged(c *media.CaptureChange) {
	s.log.Info("capture changed", "reason", c.Reason, "size", fmt.Sprintf("%dx%d", c.Width, c.Height), "rotation", c.Rotation, "hdr", c.HDR, "text", c.Text)
	switch c.Reason {
	case "resized":
		if s.resizeTimer != nil {
			s.resizeTimer.Reset(resizeSettle)
			return
		}
		s.resizeTimer = time.AfterFunc(resizeSettle, func() {
			if s.ctx.Err() != nil || s.paused.Load() {
				return // resume starts afresh
			}
			if err := s.startVideo(false, "capture resized"); err != nil {
				s.log.Warn("restart after a capture change failed", "err", err)
			}
		})
	case "lost":
		s.notice("info", "Screen capture is paused ("+trunc(c.Text, 120)+"); the last picture stays until it is back.")
	case "hdr":
		// An HDR10 stream follows Windows HDR: a new helper (a new
		// generation and video config) in the output's new mode.
		if c.Restart {
			why := fmt.Sprintf("Windows HDR turned %s", map[bool]string{true: "on", false: "off"}[c.HDR])
			go func() {
				if s.ctx.Err() != nil || s.paused.Load() {
					return // resume starts afresh
				}
				if err := s.startVideo(false, why); err != nil {
					s.log.Warn("restart after a Windows HDR change failed", "err", err)
				}
			}()
		}
	}
}

// noteCaptureFailure takes the session off AMD Direct Capture when a
// generation that used it failed, and reports whether it had: the
// experimental capture is suspected before the encoder, and the restart that
// follows uses ddagrab.
func (s *Session) noteCaptureFailure(ev media.VideoEvent) bool {
	if ev.Failed == nil || ev.Failed.Source.Backend != "amf" {
		return false
	}
	if !s.amfFailed.Swap(true) {
		s.log.Warn("AMD Direct Capture failed, using ddagrab for this session", "err", ev.Err)
	}
	return true
}

// encoderFailed decides how the next start treats the encoder of a generation
// that failed to start because of the encoder. An encoder with a fallback usage
// (media.RetryUsage) is retried once with that usage (AMF issue #410, an init
// failure), kept until the video settings change. Otherwise the encoder's
// second such failure since a generation last went live excludes it, and
// chooseEncoder falls back to the next encoder, which gets two tries too.
func (s *Session) encoderFailed(p media.Params) {
	name := p.Encoder.Name
	s.triedMu.Lock()
	defer s.triedMu.Unlock()
	s.encFails[name]++
	if u := media.RetryUsage(p.Encoder); u != "" {
		if _, retried := s.usage[name]; !retried {
			s.usage[name] = u
			s.log.Info("retrying encoder with another usage", "encoder", name, "usage", u)
			return
		}
	}
	if s.encFails[name] >= 2 {
		s.tried[name] = true
	}
}

// congestion handles a congestion signal from outside the rate reports
// (rateController.congestion) and reports whether it cut the bitrate. An
// emergency discards the current generation at once: the host's frame queue
// overflowed (its frames were dropped), or the client flushed its decoder;
// such cuts are at most 2 s apart, and a decoder flush also caps later
// increases. An older client's delay report ({"t":"congestion"} from clients
// without rate reports) decreases like the controller's own delay decision,
// overlapped: the current generation streams on until the new one's first key
// frame.
func (s *Session) congestion(delayMs int, sig rateSignal) bool {
	s.rate.setPolicy(ratePolicy(s.vid().Capabilities()))
	c, ok := s.rate.congestion(sig)
	if !ok {
		s.log.Debug("congestion: bitrate kept", "delayMs", delayMs, "signal", sig)
		return false
	}
	if sig == signalDecoder {
		s.log.Info("bitrate recovery limited by the client's decoder", "max", s.rate.decoderLimit())
	}
	s.applyRate(c, delayMs)
	return true
}

// overflowCut cuts the bitrate for a frame-queue overflow (congestion with
// signalOverflow) and reports whether it did. recovered: the ladder answered
// the dropped frames with a recovery frame (rung 2), so an encoder that
// changes its bitrate seamlessly takes the cut without the emergency's key
// frame (GUIDE 2.3: an IDR only where rung 2 is unavailable).
func (s *Session) overflowCut(recovered bool) bool {
	caps := s.vid().Capabilities()
	s.rate.setPolicy(ratePolicy(caps))
	c, ok := s.rate.congestion(signalOverflow)
	if !ok {
		s.log.Debug("congestion: bitrate kept", "signal", signalOverflow)
		return false
	}
	if recovered && caps.LiveBitrate && !caps.LiveBitrateFlush {
		c.urgent = false
	}
	s.applyRate(c, 0)
	return true
}

// rateLogEvery: at the default log level a change of the rate controller is
// logged at most once per rateLogEvery in each direction, and a frame-rate
// step always. Where the path carries less than the setting the controller
// moves every few hundred milliseconds (2 % steps up, a cut, up again); the
// changes in between are debug lines, and the next logged one counts them
// (suppressed). "stream stats" has the target every 10 s.
const rateLogEvery = 10 * time.Second

// rateLog decides the level of applyRate's lines.
type rateLog struct {
	mu   sync.Mutex
	last [2]time.Time // the last change logged at the default level: [0] up, [1] down
	n    [2]int       // changes logged at debug level since
}

// level returns the level for a change (normal: its level when it is
// logged) and how many changes in its direction were debug lines before it.
func (l *rateLog) level(c rateChange, now time.Time, normal slog.Level) (slog.Level, int) {
	d := 0
	if c.down {
		d = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.toFPS == c.fromFPS && !l.last[d].IsZero() && now.Sub(l.last[d]) < rateLogEvery {
		l.n[d]++
		return slog.LevelDebug, 0
	}
	n := l.n[d]
	l.last[d], l.n[d] = now, 0
	return normal, n
}

// applyRate puts a change of the rate controller into effect (setRate) and
// logs it (rateLog); decreases also tell the user, at most once per 30 s.
func (s *Session) applyRate(c rateChange, delayMs int) {
	_, ceiling := s.rate.kbps()
	reason := "bitrate recovery"
	if c.down {
		reason = "congestion"
		attrs := []any{"from", c.fromKbps, "to", c.toKbps, "why", c.why, "fps", c.toFPS, "delayMs", delayMs, "urgent", c.urgent}
		lvl, n := s.rateLog.level(c, time.Now(), slog.LevelWarn)
		if n > 0 {
			attrs = append(attrs, "suppressed", n)
		}
		s.log.Log(context.Background(), lvl, "congestion: lowering bitrate", attrs...)
		if now, last := time.Now().UnixNano(), s.noticeAt.Load(); now-last >= int64(30*time.Second) && s.noticeAt.CompareAndSwap(last, now) {
			s.notice("warn", fmt.Sprintf("Network congestion detected — bitrate lowered to %.1f Mbps", float64(c.toKbps)/1000))
		}
	} else {
		attrs := []any{"from", c.fromKbps, "to", c.toKbps, "fps", c.toFPS, "max", ceiling}
		lvl, n := s.rateLog.level(c, time.Now(), slog.LevelInfo)
		if n > 0 {
			attrs = append(attrs, "suppressed", n)
		}
		s.log.Log(context.Background(), lvl, "bitrate recovery: raising bitrate", attrs...)
	}
	if err := s.setRate(c.toKbps, c.toFPS, c.urgent, reason); err != nil {
		s.log.Warn("bitrate change failed", "reason", reason, "err", err)
	}
}

// rateLoop advances the rate controller every rateTick: increases, changes
// the encoder could not take yet (its policy's gap), the acks of clients
// without rate reports turned into feedback, and the stalled-path check. It
// also applies the decisions on the client's reports (rateChanges).
func (s *Session) rateLoop() {
	t := time.NewTicker(rateTick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case c := <-s.rateChanges:
			s.applyRate(c, 0)
			continue
		case <-t.C:
		}
		if _, live := s.vid().Active(); !live || s.paused.Load() {
			s.rate.hold() // nothing streams (paused, or the encoder is starting or failing): nothing to judge
			continue
		}
		s.rate.setPolicy(ratePolicy(s.vid().Capabilities()))
		now := time.Now()
		if !s.fb.reportsActive(now) {
			if fb, ok := s.fb.fromAcks(now, s.ccCounters()); ok {
				if c, ok := s.rate.report(fb); ok {
					s.applyRate(c, int(fb.qd.Milliseconds()))
				}
			}
		}
		if c, ok := s.rate.tick(s.stalled()); ok {
			s.applyRate(c, 0)
		}
	}
}

// stalled reports whether a frame went out ackTimeout ago or longer and no
// feedback has covered it, from a client that sends feedback: the path to
// the client stalls.
func (s *Session) stalled() bool {
	at, ok := s.track.uncoveredSince()
	return ok && s.a.clock()-at >= uint64(ackTimeout.Microseconds())
}

// ccCounters returns the media congestion controller's packet and byte
// counters (ok false with another controller).
func (s *Session) ccCounters() ccCounters {
	m := transport.MediaControl(s.c)
	if m == nil {
		return ccCounters{}
	}
	st := m.Stats()
	nonVideo := s.audioKbps.Load() + ccOverheadKbps
	if t := s.ccTarget.Load(); t != nil {
		nonVideo += s.fecOverheadKbps(t.videoKbps) // parity and shard headers (fec.go)
	}
	return ccCounters{ok: true, lost: st.LostPackets, total: st.LostPackets + st.AckedPackets, acked: st.AckedBytes,
		nonVideoKbps: int(nonVideo)}
}

// rateReport feeds a client's receive report to the rate controller.
func (s *Session) rateReport(r proto.RateReport) {
	s.fecReport(r, time.Now())
	var comp time.Duration
	if r.Flags&proto.RateReportFrame != 0 {
		comp, _ = s.track.cover(r.Gen, r.LastSeq)
	}
	fb, ok := s.fb.fromReport(r, time.Now(), comp, s.ccCounters())
	if !ok {
		return // out of order
	}
	fb.pending, fb.pendingValid = s.track.pending(s.a.clock())
	before := s.rate.decreasedAt()
	c, ok := s.rate.report(fb)
	if ok || !s.rate.decreasedAt().Equal(before) {
		// A decrease on this report (applied now, or when the policy's
		// gap since the last change has passed: deferred).
		s.log.Debug("rate report decision", "why", c.why, "deferred", !ok, "qd_ms", fb.qd.Milliseconds(),
			"owd_ms", fb.owd.Milliseconds(), "pending_ms", fb.pending.Milliseconds(), "interval_ms", fb.interval.Milliseconds())
	}
	if ok {
		select {
		case s.rateChanges <- c:
		default:
			s.log.Warn("rate change dropped: the rate loop is behind")
		}
	}
}

// videoPacingBps is the pacing rate available to video frames (bit/s): the
// media congestion controller's pacing less audio and the overhead allowance
// (setCongestionTarget); 0 with another controller.
func (s *Session) videoPacingBps() float64 {
	m := transport.MediaControl(s.c)
	if m == nil {
		return 0
	}
	return max(0, float64(m.TargetBitrate())*cc.PacingGain-float64((s.audioKbps.Load()+ccOverheadKbps)*1000))
}

// urgentRestart discards the current generation at the current bitrate, for
// a queue overflow within two seconds of a bitrate cut. After an overlapped
// back-off (a client congestion report) the old generation streams on at the
// old bitrate while the new one starts, and keeps overflowing the queue: the
// starting generation takes over at once and the active one stops. With
// nothing starting, a new generation starts now: the dropped frames were the
// newest generation's, possibly its key frame. The client's key-frame request
// for the dropped frames is then covered (lastKick): the next generation
// starts with a key frame.
func (s *Session) urgentRestart(reason string) {
	s.kickMu.Lock()
	s.lastKick = time.Now()
	s.kickMu.Unlock()
	if s.takeover(reason) {
		return
	}
	if err := s.keyframe(reason); err != nil {
		s.log.Warn("urgent restart failed", "reason", reason, "err", err)
	}
}

// takeover makes a generation that is starting (an overlapped back-off) take
// over at once and stops the active one, for a queue overflow within two
// seconds of a bitrate cut; it reports whether one was starting. Where a
// recovery frame answers the overflow's loss (rung 2) this is all: no key
// frame (GUIDE 2.3: an IDR only where rung 2 is unavailable); the starting
// generation begins with a key frame of its own (lastKick).
func (s *Session) takeover(reason string) bool {
	stopped, starting := s.vid().Hurry()
	if starting {
		s.kicked()
	}
	if stopped {
		s.log.Info("restarting video", "reason", reason, "urgent", true, "takeover", true)
	}
	return starting
}

// kicked notes that a key frame is on its way (lastKick).
func (s *Session) kicked() {
	s.kickMu.Lock()
	s.lastKick = time.Now()
	s.kickMu.Unlock()
}

// requestKeyframe gets a key frame to a client that needs one (the client's
// request: a decoder error, a loss under recovery "keyframe", its watchdog;
// or the ladder's rung 4 for a loss the host knows of), at most one every
// 500 ms.
func (s *Session) requestKeyframe(reason string) {
	s.kickMu.Lock()
	if time.Since(s.lastKick) < 500*time.Millisecond {
		s.kickMu.Unlock()
		return
	}
	s.lastKick = time.Now()
	s.kickMu.Unlock()
	if err := s.keyframe(reason); err != nil {
		s.log.Warn("keyframe restart failed", "err", err)
	}
}

// keyframe makes the next frame a key frame of a new generation (the
// ladder's rung 4): in the running encoder where the pipeline can force one
// (Capabilities().ForceIDR: the native helper, never a restart), else with a
// new generation started at once (FFmpeg's command line cannot force one),
// built afresh from the settings.
func (s *Session) keyframe(reason string) error {
	v := s.vid()
	s.stats.keyframes.Add(1)
	if !ladder(ladderIn{event: lossKeyRequest, forceIDR: v.Capabilities().ForceIDR}).restart {
		err := v.ForceKeyframe()
		if err == nil {
			s.log.Info("forcing a key frame", "reason", reason)
			return nil
		}
		s.log.Warn("forcing a key frame failed, restarting", "reason", reason, "err", err)
	}
	return s.startVideo(true, reason)
}

// setRate puts the rate controller's new bitrate and frame rate into effect:
// in the running encoder where the pipeline can (Capabilities().LiveBitrate:
// the native helper), else as a new generation (overlapped unless urgent).
// urgent: at once, and the client also needs a key frame (frames were
// dropped, or it flushed its decoder), or the old FFmpeg generation must stop
// now (a large cut: applyPolicy.cutUrgent). A change that delivers a key
// frame (a restart, an encoder flush, urgent) covers the client's key-frame
// requests of the next 500 ms (lastKick); a seamless live change does not.
func (s *Session) setRate(kbps, fps int, urgent bool, reason string) error {
	v := s.vid()
	c := v.Capabilities()
	if !c.LiveBitrate || c.LiveBitrateFlush || urgent {
		s.kicked()
	}
	if !c.LiveBitrate {
		return s.startVideoLog(urgent, reason, true)
	}
	newFPS := 0
	if cur, ok := v.Current(); ok && fps > 0 && fps != cur.FPS {
		newFPS = fps
	}
	// A static desktop keeps the encoder below the target (activity.go).
	now := s.static.clock()
	s.static.mu.Lock()
	enc, vbv := s.static.want(now, kbps, !c.LiveBitrateFlush)
	attrs := []any{"reason", reason, "kbps", enc, "fps", newFPS, "urgent", urgent}
	if enc < kbps {
		attrs = append(attrs, "target", kbps, "static_desktop", true)
	}
	s.log.Debug("changing the bitrate in the encoder", attrs...) // applyRate logged the change
	err := v.SetRate(enc, newFPS, vbv)
	if err == nil {
		s.static.applied(now, enc, kbps)
	}
	s.static.mu.Unlock()
	if err != nil {
		s.log.Warn("bitrate change in the encoder failed, restarting", "err", err)
		s.kicked()
		return s.startVideo(urgent, reason)
	}
	if p, ok := v.Current(); ok {
		s.setCongestionTarget(p)
	}
	// The client hears of it from the pipeline (VideoEvent.Rate) once the
	// live encoder runs at it; a starting stream's config has it.
	if urgent {
		return s.keyframe(reason)
	}
	return nil
}

// frameSender sends the queued frames, each on its own stream, oldest first.
// The ladder decides about every frame (ladder.go): a frame the client would
// discard anyway (it waits for the answer to a loss before it) is not sent,
// and a frame stream still being written past its deadline while a newer
// frame is ready is cancelled (checkOut, rung 1). A frame goes out only when
// the video window has room for it (window.go, GUIDE 2.7).
func (s *Session) frameSender() {
	buf := make([]byte, 0, 1<<20)
	faults := s.a.faults
	for n := 1; ; n++ {
		var f *media.Frame
		s.sendSince.Store(0)
		select {
		case <-s.ctx.Done():
			return
		case f = <-s.frameQ:
		}
		num, step := s.send.take(f)
		if step.act == actDiscard {
			s.discard(f, step)
			continue
		}
		// A frame the test hook drops or delays is never thinned: the
		// faults stay as configured, whatever the load.
		drop, delay := faults.at(n)
		if !drop && delay == 0 && s.thin(f, num) {
			continue // left out under congestion: no loss (thin.go)
		}
		s.reportDiscards(0) // a frame goes out again: the run before it is complete
		// Datagram shards (fec.go). They bypass the stream path below;
		// sendFEC puts them through the video window (GUIDE 2.7) itself
		// (fec.go, "Send priorities").
		if s.useFEC() && s.sendFEC(f, n, num) {
			continue
		}
		s.sendOpening.Store(true)
		s.sendSince.Store(time.Now().UnixNano())
		s.applyCongestionTarget()
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		st, err := s.c.OpenUniStreamSync(ctx)
		cancel()
		s.sendOpening.Store(false)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.log.Debug("open frame stream", "err", err)
			s.lostFrame(f, "stream failed")
			continue
		}
		h, ext := videoHeader(f, s.hello.V, s.a.clock(), s.thinning.mask(f.Gen, f.Seq))
		buf = buf[:proto.FrameHeaderLen]
		h.Marshal(buf)
		if h.Flags&proto.FrameFlagExt != 0 {
			buf = ext.Append(buf)
		}
		buf = append(buf, f.Data...)
		of := &outFrame{f: f, st: st, n: num, opened: time.Now(), deadline: s.frameDeadline(len(buf)), gone: make(chan struct{}), held: true}
		// A frame the ladder may cancel when it is late: look again at
		// its deadline (a newer frame queued later looks too).
		in := s.ladderIn(lossOutgoing, f.Gen, f.Seq)
		in.key, in.recovery, in.age, in.deadline, in.newer = f.Key, f.Recovery, of.deadline, of.deadline, true
		lateCancel := ladder(in).act == actCancel
		if s.partial { // GUIDE 2.4: the prefix a cancel still delivers (writeFrame)
			of.reliable = s.reliablePrefix(f, len(buf)-len(f.Data))
		}
		s.send.register(of)
		if drop || delay > 0 {
			s.send.start(of, lateCancel, s.checkOut) // the test hooks bypass the window
		}
		if drop {
			// Test hook: the stream fails mid-frame.
			_ = st.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.writeFrame(of, buf[:len(buf)/2])
			if s.send.finish(of, outCancelled) {
				st.CancelWrite()
				s.lostFrame(f, "test fault")
			}
			continue
		} else if delay > 0 {
			// Test hook: this frame arrives late, the next ones on time
			// (its stream's write stands still meanwhile; rung 1 treats
			// it as a write the transport holds back).
			s.log.Debug("test fault: delaying frame", "gen", f.Gen, "seq", f.Seq, "delay", delay)
			late := append([]byte(nil), buf...)
			if of.reliable > 0 {
				// quic-go takes a small write at once, whatever holds
				// the rest back: the header goes out now.
				_ = st.SetWriteDeadline(time.Now().Add(3 * time.Second))
				_ = s.writeFrame(of, late[:of.reliable])
			}
			time.AfterFunc(delay, func() { s.sendFrame(of, h, late) })
			continue
		}
		// Send priorities (GUIDE 2.7): while the path falls short of the
		// pacing rate, at most videoInFlight frames in flight beyond those
		// in transit, so datagrams never queue behind a video backlog in
		// the network (window.go). The frame waits with its stream open,
		// at most three quarters of its deadline; a frame the client would
		// discard meanwhile is released at once.
		if s.admit(of) > 0 && h.Flags&proto.FrameFlagExt != 0 {
			h.SendUs = s.a.clock() // handed to the transport now: the wait is host queue
			h.Marshal(buf)
		}
		// Its deadline (rung 1) runs from here: the hold was host queue,
		// not a write the transport holds back.
		s.send.start(of, lateCancel, s.checkOut)
		s.sendFrame(of, h, buf)
	}
}

// frameDeadline is the deadline of a frame stream of the given size (rung 1:
// frameDeadline) at the session's frame rate and video pacing rate (the
// media congestion controller's, else 1.2 x the video bitrate).
func (s *Session) frameDeadline(bytes int) time.Duration {
	interval, pacing := time.Second/60, s.videoPacingBps()
	if t := s.ccTarget.Load(); t != nil {
		interval = t.frameInterval
		if pacing <= 0 {
			pacing = float64(t.videoKbps) * 1000 * cc.PacingGain
		}
	}
	return frameDeadline(interval, bytes, pacing)
}

// sendFrame writes a frame (header h, encoded as b) to its stream; a frame
// that cannot be written is reported dropped. A frame the ladder cancelled
// meanwhile (checkOut) is lost already.
func (s *Session) sendFrame(of *outFrame, h proto.FrameHeader, b []byte) {
	st, f := of.st, of.f
	if of.state.Load() != outWriting {
		return // cancelled while the test hook held it
	}
	_ = st.SetWriteDeadline(time.Now().Add(3 * time.Second))
	m := s.deliveryMeter()
	ws := startWrite(m) // the video window measures the frame's delivery from here
	if err := s.writeFrame(of, b); err != nil {
		if s.send.finish(of, outCancelled) {
			st.CancelWrite()
			if s.ctx.Err() == nil {
				s.lostFrame(f, "stream failed")
			}
		}
		return
	}
	if m2 := s.deliveryMeter(); m2 != nil {
		if m2 != m {
			ws.pos = math.MaxUint64 // the path changed during the write
		}
		s.win.sent(m2, ws, time.Now()) // in flight until the peer acknowledged what was sent up to here
	}
	if !s.send.finish(of, outDone) {
		return // cancelled as its write completed: reported lost
	}
	st.Close()
	s.track.sent(f.Gen, f.Seq, f.EncodeDoneUs, s.a.clock(), len(b), s.videoPacingBps())
	s.stats.frames.Add(1)
	s.stats.bytes.Add(int64(len(b)))
	if h.Flags&proto.FrameFlagExt != 0 {
		s.hostStages.sentFrame(f, h.SendUs)
	}
}

// reliablePrefix is how much of frame f's stream (its header and extension:
// hdrLen bytes, then f.Data) a cancel must still deliver under partial
// delivery (GUIDE 2.4): the header, so the client learns at once which
// frame it will not get, and of a key frame also its parameter sets
// (codec.ParamSetsLen), the part of a new generation's first frame that
// describes the stream.
func (s *Session) reliablePrefix(f *media.Frame, hdrLen int) int {
	if !f.Key {
		return hdrLen
	}
	s.healMu.Lock()
	family := s.genFamily[f.Gen]
	s.healMu.Unlock()
	return hdrLen + codec.ParamSetsLen(family, f.Data)
}

// writeFrame writes b, a frame stream's data from its start (or the first
// part of it), to the frame's stream. Under partial delivery (GUIDE 2.4) the
// reliable prefix goes first, once, and is marked reliable: a CancelWrite
// after that (rung 1, a failed write) still delivers it, the rest of the
// frame not. A stream the ladder cancelled before the mark stays unmarked
// (sendState.markReliable) and gets no more writes. Without partial
// delivery, one write as before.
func (s *Session) writeFrame(of *outFrame, b []byte) error {
	if n := min(of.reliable, len(b)); n > 0 && of.relSent.Load() == 0 {
		if _, err := of.st.Write(b[:n]); err != nil {
			return err
		}
		if !s.send.markReliable(of, n) {
			return errFrameCancelled
		}
	}
	_, err := of.st.Write(b[of.relSent.Load():])
	return err
}

// videoHeader builds a video frame's header, plus the extension for clients
// that parse it. now is the host clock as the frame goes to the transport;
// thinned is the frame's ExtThinned mask (thinState.mask; sent to clients
// that read it only).
func videoHeader(f *media.Frame, helloV int, now uint64, thinned uint32) (proto.FrameHeader, proto.FrameExt) {
	h := proto.FrameHeader{Type: proto.FrameTypeVideo, Gen: f.Gen, Seq: f.Seq, PtsUs: uint64(f.PtsUs)}
	if f.Key {
		h.Flags |= proto.FrameFlagKey
	}
	var ext proto.FrameExt
	if helloV < proto.HelloVersionFrameExt {
		// v1 clients use SendUs as the encoder-out time (congestion detection,
		// frame acks, latency readout): keep that meaning for them.
		h.SendUs = f.EncodeDoneUs
		return h, ext
	}
	// SendUs is the moment the frame is handed to the transport, so
	// encodeDone -> send is the host queue (frame queue + stream credit).
	h.SendUs = now
	h.Flags |= proto.FrameFlagExt
	ext.Set(proto.ExtEncodeDoneUs, f.EncodeDoneUs)
	if f.CaptureUs != 0 {
		ext.Set(proto.ExtCaptureUs, f.CaptureUs)
	}
	// The native helper's stages and recovery metadata. refFloor is present
	// exactly on recovery frames (that is the marker: readers skip unknown
	// tags, v1 clients get no extension at all).
	if f.PresentUs != 0 {
		ext.Set(proto.ExtPresentUs, f.PresentUs)
	}
	if f.SubmitUs != 0 {
		ext.Set(proto.ExtEncodeSubmitUs, f.SubmitUs)
	}
	if f.Recovery {
		ext.Set(proto.ExtRefFloor, uint64(f.RefFloor))
	}
	if f.MarkedLTR {
		ext.Set(proto.ExtLTRSlot, uint64(f.LTRSlot))
	}
	if f.TemporalLayer != 0 {
		ext.Set(proto.ExtTemporalLayer, uint64(f.TemporalLayer))
	}
	if thinned != 0 && helloV >= proto.HelloVersionThinned {
		ext.Set(proto.ExtThinned, uint64(thinned))
	}
	return h, ext
}

// ---------------------------------------------------------------------------
// Audio

func (s *Session) startAudio() {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	prefs := s.currentPrefs()
	enabled := s.a.cfg.Audio && prefs.AudioEnabled()
	codec := "opus"
	if prefs.AudioCodec == "pcm" || (!s.hello.Audio.Opus && s.hello.Audio.PCM) {
		codec = "pcm"
	}
	if enabled && !s.hello.Audio.Opus && !s.hello.Audio.PCM {
		enabled = false
	}
	if !enabled {
		s.sendJSON(proto.AudioConfig{T: "audio", Enabled: false})
		return
	}
	// 10 ms frames until the first capture packet says whether 5 ms ones
	// would save anything (media.Audio.PickFrameMs; applyAudioFrame).
	s.audio = media.NewAudio(s.a.audioSource, media.AudioConfig{Codec: codec, BitrateKbps: s.a.cfg.AudioKbps, CaptureChanged: s.audioFrameCheck}, s.log)
	s.sendJSON(audioConfig(s.audio, false))
	err := s.audio.Start(func(pkt []byte) {
		if !s.paused.Load() {
			_ = s.c.SendDatagram(pkt)
		}
	})
	if err != nil {
		s.log.Warn("audio start failed", "err", err)
		s.audio = nil
		return
	}
	s.audioKbps.Store(int64(s.audio.Kbps()))
}

func (s *Session) stopAudio() {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if s.audio != nil {
		s.audio.Stop()
		s.audio = nil
		s.audioKbps.Store(0)
	}
}

// audioConfig announces a's stream; same marks a change within the running
// stream (its frame duration), whose sequence numbers go on.
func audioConfig(a *media.Audio, same bool) proto.AudioConfig {
	return proto.AudioConfig{T: "audio", Enabled: true, Codec: a.Codec(), SampleRate: 48000, Channels: 2, FrameMs: a.FrameMs(), SameStream: same}
}

// audioFrameCheck has audioFrameLoop check the frame duration.
func (s *Session) audioFrameCheck() {
	select {
	case s.rttSeen <- struct{}{}:
	default:
	}
}

// audioFrameLoop picks the Opus frame duration from the client's minimum
// round-trip time whenever a ping reports a new one or the capture packets
// change (step 4.6: 5 ms frames on a LAN when the capture delivers at most
// 5 ms at a time, 10 ms otherwise; media.Audio.PickFrameMs). Clients before
// step 4.6 report no RTT and keep 10 ms frames.
func (s *Session) audioFrameLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.rttSeen:
		}
		s.applyAudioFrame()
	}
}

// applyAudioFrame switches a running Opus encoder to the frame duration the
// client's RTT and the capture packets ask for, from its next frame on, and
// announces it in a new audio config of the same stream (the packets carry
// their duration too: the client decodes them without it; it takes the
// config's frameMs only for its display and as a fallback).
func (s *Session) applyAudioFrame() {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	a := s.audio
	if a == nil || a.Codec() != "opus" {
		return
	}
	rtt := time.Duration(s.clientRTT.Load())
	ms := a.PickFrameMs(rtt)
	if !a.SetFrameMs(ms) {
		return
	}
	s.log.Info("audio frame size", "ms", ms, "client_min_rtt_ms", float64(rtt.Microseconds())/1000, "capture_ms", a.CaptureMs())
	s.sendJSON(audioConfig(a, true))
}

// ---------------------------------------------------------------------------
// Input

// inputAllowed reports whether the session's client may give input now: it is
// the active session and is not being closed (a takeover moves the active
// session first; a revocation of the user's access closes it, with a bye,
// while it is still the active one).
func (s *Session) inputAllowed() bool {
	if !s.a.isActive(s) {
		return false
	}
	s.ctrlDL.Lock()
	defer s.ctrlDL.Unlock()
	return !s.closing
}

func (s *Session) inputLoop(st transport.BidiStream) {
	defer st.CancelRead()
	inj := s.a.inj
	for {
		// A message above the limit is skipped: ending the stream would end
		// the session's keyboard and mouse buttons (clients split long text).
		b, err := proto.ReadMsgSkip(st, proto.MaxInputMsg)
		if errors.Is(err, proto.ErrTooLarge) {
			s.log.Warn("input message skipped", "err", err)
			continue
		}
		if err != nil {
			return
		}
		if !s.inputAllowed() {
			return
		}
		ev, err := proto.ParseInput(b)
		if err != nil {
			continue
		}
		switch ev.Type {
		case proto.InKey:
			err = inj.Key(ev.Scancode, ev.Extended, ev.Down)
		case proto.InMouseButton:
			err = inj.Button(ev.Button, ev.Down)
		case proto.InWheel:
			err = inj.Wheel(ev.WheelY, ev.WheelX)
		case proto.InReleaseAll:
			inj.ReleaseAll()
		case proto.InText:
			err = inj.Text(ev.Text)
		}
		if err != nil {
			s.log.Debug("inject", "err", err)
		}
	}
}

func (s *Session) datagrams() {
	for {
		d, err := s.c.ReceiveDatagram(s.ctx)
		if err != nil {
			return
		}
		if len(d) == 0 {
			continue
		}
		switch d[0] {
		case proto.DgPing:
			if p := proto.Pong(d, s.a.clock()); p != nil {
				s.sendPong(p)
			}
			if rtt := proto.PingMinRTT(d); rtt > 0 {
				s.fec.rttReports.Add(1)
				if s.clientRTT.Swap(int64(rtt)) != int64(rtt) {
					s.audioFrameCheck()
				}
			}
		case proto.DgMouseRel:
			if m, ok := proto.ParseMouseRel(d); ok && s.inputAllowed() {
				s.roi.pointerRel(time.Now()) // pointer lock: a game's crosshair (roi.go)
				if dx, dy := s.rel.Update(m.Seq, m.CumX, m.CumY); dx != 0 || dy != 0 {
					_ = s.a.inj.MoveRel(dx, dy)
				}
			}
		case proto.DgMouseAbs:
			if m, ok := proto.ParseMouseAbs(d); ok && s.absGate.Accept(m.Seq) && s.inputAllowed() {
				s.roi.pointerAbs(m.X, m.Y, time.Now())
				_ = s.a.inj.MoveAbs(m.X, m.Y)
			}
		case proto.DgGamepad:
			// The shared virtual pads take only the active session's
			// client, as the keyboard and mouse do.
			if g, ok := proto.ParseGamepad(d); ok && g.Index < 4 && s.padGates[g.Index].Accept(g.Seq) && s.inputAllowed() {
				s.gamepad(g)
			}
		case proto.DgRateReport:
			if r, ok := proto.ParseRateReport(d); ok {
				s.rateReport(r)
			}
		case proto.DgFECNack:
			if n, ok := proto.ParseFECNack(d); ok && s.fec.avail {
				s.fecNack(n)
			}
		case proto.DgFrameAck:
			if a, ok := proto.ParseFrameAck(d); ok {
				s.vid().Ack(a.Gen, a.Seq)
				s.hostStages.acked(a.Gen, a.Seq, s.a.clock())
				// Clients without rate reports: their acks are the rate
				// controller's feedback (rateLoop).
				if now := time.Now(); !s.fb.reportsActive(now) {
					if f, ok := s.track.frame(a.Gen, a.Seq); ok {
						s.fb.ack(time.Duration(a.OWDUs)*time.Microsecond, time.Duration(f.comp)*time.Microsecond, f.bytes, now)
					}
				}
				s.stats.acks.Add(1)
				owd := int64(a.OWDUs)
				s.stats.owdSum.Add(owd)
				for {
					m := s.stats.owdMax.Load()
					if owd <= m || s.stats.owdMax.CompareAndSwap(m, owd) {
						break
					}
				}
			}
		}
	}
}

// sendPong queues the answer to a ping for pongSender. quic-go's
// SendDatagram blocks while its queue holds 32 datagrams (a congestion
// window full for long enough: an outage, a path slower than the datagrams
// alone): the datagram loop must not wait there, or input waits with it. A
// pong that finds the queue full is dropped: the client keeps its best
// (lowest round-trip) sample, and one that waited would not be that.
func (s *Session) sendPong(p []byte) {
	select {
	case s.pongs <- p:
	default:
	}
}

func (s *Session) pongSender() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case p := <-s.pongs:
			_ = s.c.SendDatagram(p)
		}
	}
}

func (s *Session) gamepad(g proto.Gamepad) {
	if !g.Connected {
		s.rumble(int(g.Index), 0, 0) // a pad that is gone has nothing to repeat
	} else if s.a.faults.rumbleEcho {
		// Test hook: the triggers come back as force feedback, as a game's
		// would through ViGEmBus (also without it).
		s.rumble(int(g.Index), g.LT, g.RT)
	}
	pads := s.a.gamepads()
	if pads == nil {
		s.padWarn.Do(func() {
			s.notice("warn", "Gamepad detected, but the host has no ViGEmBus driver installed — controller input is disabled.")
		})
		return
	}
	if !g.Connected {
		pads.Unplug(int(g.Index))
		// Again: a change ViGEmBus reported while the pad was being
		// unplugged can have set the motors after the stop above (the
		// unplug has ended the pad's listener now), and nothing would
		// stop them.
		s.rumble(int(g.Index), 0, 0)
		return
	}
	err := pads.Update(int(g.Index), platform.Pad{Buttons: g.Buttons, LT: g.LT, RT: g.RT, LX: g.LX, LY: g.LY, RX: g.RX, RY: g.RY})
	if err != nil {
		s.padWarn.Do(func() { s.notice("warn", "Virtual gamepad error: "+err.Error()) })
	}
}

func (s *Session) releasePads() {
	if pads := s.a.gamepads(); pads != nil {
		for i := 0; i < 4; i++ {
			pads.Unplug(i)
		}
	}
}

// ---------------------------------------------------------------------------
// Cursor (local rendering in the browser = zero-latency pointer)

// The host pointer's state and shapes (tests replace them).
var (
	getCursor   = platform.GetCursor
	cursorImage = platform.CursorImage
)

// cursorLoop sends the host pointer's shape (control messages) and position
// (datagrams) for the client to draw it locally, for the whole session when
// the agent reads the pointer and does not draw it into the video itself.
// While the client's cursor setting is "video" (the video shows the pointer)
// it sends nothing; the client can switch to its local cursor mid-session (a
// live settings change), and the current shape and position then follow at
// once, as they do at the start.
func (s *Session) cursorLoop() {
	if !s.a.cursorSupported() || s.a.cfg.DrawCursor {
		return
	}
	t := time.NewTicker(8 * time.Millisecond)
	defer t.Stop()
	var lastHandle uint64
	lastVisible := false
	var lastX, lastY uint16
	var seq uint32
	sent := map[uint64]bool{}
	resend := true // the shape (or hidden) and position, whatever was sent before
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if s.currentPrefs().Cursor == "video" {
			resend = true
			continue
		}
		cs, err := getCursor()
		if err != nil {
			continue
		}
		if s.cursorInVideo.Load() {
			cs.Visible = false // the stream shows the pointer already (a WGC capture that could not leave it out)
		}
		if resend || cs.Visible != lastVisible || (cs.Visible && cs.Handle != lastHandle) {
			lastVisible, lastHandle = cs.Visible, cs.Handle
			msg := proto.CursorShape{T: "cursor", ID: cs.Handle, Hidden: !cs.Visible}
			if cs.Visible && !sent[cs.Handle] {
				if shape, err := cursorImage(cs.Handle); err == nil {
					img := &image.NRGBA{Pix: shape.RGBA, Stride: shape.W * 4, Rect: image.Rect(0, 0, shape.W, shape.H)}
					var pb bytes.Buffer
					if png.Encode(&pb, img) == nil {
						msg.PNG = base64.StdEncoding.EncodeToString(pb.Bytes())
						msg.HotX, msg.HotY, msg.Width, msg.Height = shape.HotX, shape.HotY, shape.W, shape.H
						sent[cs.Handle] = true
					}
				}
			}
			s.sendJSON(msg)
		}
		s.prefsMu.Lock()
		m := s.monitor
		s.prefsMu.Unlock()
		if m.W > 1 && m.H > 1 {
			nx := clampU16((cs.X - m.X) * 65535 / (m.W - 1))
			ny := clampU16((cs.Y - m.Y) * 65535 / (m.H - 1))
			if resend || nx != lastX || ny != lastY {
				lastX, lastY = nx, ny
				seq++
				_ = s.c.SendDatagram(proto.CursorPos(seq, cs.Visible, nx, ny))
			}
		}
		resend = false
	}
}

func clampU16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// ---------------------------------------------------------------------------
// Control

func (s *Session) controlLoop() error {
	for {
		b, err := proto.ReadMsg(s.ctrl, proto.MaxControlMsg)
		if err != nil {
			if s.ctx.Err() != nil {
				return errClosed
			}
			return err
		}
		var m proto.ClientMsg
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		switch m.T {
		case "settings":
			if m.Prefs == nil {
				continue
			}
			s.prefsMu.Lock()
			old := s.prefs
			s.prefs = *m.Prefs
			s.prefsMu.Unlock()
			videoChanged := old.Codec != m.Prefs.Codec || old.BitrateKbps != m.Prefs.BitrateKbps || old.FPS != m.Prefs.FPS ||
				old.Width != m.Prefs.Width || old.Height != m.Prefs.Height || old.Monitor != m.Prefs.Monitor ||
				old.Window != m.Prefs.Window || old.Quality != m.Prefs.Quality || old.Cursor != m.Prefs.Cursor ||
				old.AdaptiveBitrate() != m.Prefs.AdaptiveBitrate()
			// HDR prefs alone (the display, the setting) restart the video
			// only when they change the current generation's HDR decision.
			hdrChanged := !videoChanged && !proto.SameHDR(old.HDR, m.Prefs.HDR) && s.hdrRestart(*m.Prefs)
			if videoChanged || hdrChanged {
				s.triedMu.Lock()
				s.tried = map[string]bool{}
				s.usage = map[string]string{}
				clear(s.encFails)
				s.triedMu.Unlock()
				// An explicit video choice resets the congestion back-off;
				// an audio-only or HDR-only change keeps it.
				reason := "HDR settings"
				if videoChanged {
					s.rate.reset()
					reason = "settings"
				}
				// A virtual display follows the client's size and frame
				// rate (and goes for a window capture): a new one is a new
				// capture, started at once.
				urgent := false
				if old.Width != m.Prefs.Width || old.Height != m.Prefs.Height || old.FPS != m.Prefs.FPS || old.Monitor != m.Prefs.Monitor ||
					old.Window != m.Prefs.Window {
					urgent = s.updateVirtualDisplay(*m.Prefs)
				}
				if err := s.startVideo(urgent, reason); err != nil {
					s.notice("error", "Could not apply settings: "+err.Error())
				}
			}
			if old.AudioEnabled() != m.Prefs.AudioEnabled() || old.AudioCodec != m.Prefs.AudioCodec {
				s.stopAudio()
				s.startAudio()
			}
		case "keyframe":
			// While the client is hidden nothing streams, and resume starts
			// a generation with a key frame: its watchdog's requests and
			// losses are moot.
			if !s.paused.Load() {
				s.requestKeyframe("keyframe request")
			}
		case proto.MsgLost:
			// A loss only the client saw (a gap that outlasted its wait),
			// under reference recovery: it waits for the recovery frame. A
			// frame left out on purpose is none (thin.go): the loss starts
			// at the next frame sent.
			if !s.paused.Load() {
				s.loss(lossConfirmed, m.Gen, s.thinning.firstSent(m.Gen, m.FromSeq), "client")
			}
		case "stages":
			s.logStages(m.Stages, m.Renderer, m.Pacing, m.Upscale)
		case "congestion":
			// Overlapped: the client keeps decoding the current generation
			// until the new one is ready, unless it flushed its decoder: then
			// it waits for a key frame, also when the bitrate stays.
			if m.Reason != proto.CongestionDecoder {
				s.congestion(m.DelayMs, signalDelay)
			} else if !s.congestion(m.DelayMs, signalDecoder) && !s.paused.Load() {
				s.requestKeyframe("keyframe request")
			}
		case "pause":
			if !s.paused.Swap(true) {
				s.log.Info("client hidden: pausing video")
				s.vid().Suspend()
				s.drainQueue()
				s.displayOn(false)
			}
		case "resume":
			if s.paused.Swap(false) {
				s.displayOn(true)
				if err := s.startVideo(true, "resume"); err != nil {
					s.notice("error", err.Error())
				}
			}
		case "bye":
			return errClosed
		}
	}
}

// hostStages keeps the host's own capture->encoded and queue times of the
// frames a v2 client acknowledged in the last 10 s. They are logged next to
// the client's summary as a reference for its rows that needs no clock sync.
// The client acks every frame it decoded; it records stages only for the
// frames it drew, so the few outputs it closed unseen for a newer one
// (superseded, 4.1) are in these rows but not in its own.
type hostStages struct {
	mu   sync.Mutex
	sent [512]hostStage // recent frames by seq, until acknowledged
	recs []hostStage    // acknowledged frames, oldest first
}

type hostStage struct {
	gen            uint8
	seq            uint32
	at             uint64  // host clock when sent, then when acknowledged
	capture, queue float64 // ms; capture < 0: no capture stamp
	// Sub-frame output (native helper sliceOutput, Phase 5 wiring B): encoder
	// submit -> first slice ready, and first slice -> whole frame (what
	// sending slices as they come could gain); ms, < 0: not measured.
	firstSlice, sliceRest float64
}

const stageWindowUs = 10_000_000

func (h *hostStages) sentFrame(f *media.Frame, sentUs uint64) {
	r := hostStage{gen: f.Gen, seq: f.Seq, at: sentUs, capture: -1, queue: float64(sentUs-f.EncodeDoneUs) / 1000,
		firstSlice: -1, sliceRest: -1}
	if f.CaptureUs != 0 {
		r.capture = float64(f.EncodeDoneUs-f.CaptureUs) / 1000
	}
	if f.FirstSliceUs != 0 && f.SubmitUs != 0 && f.SubmitUs <= f.FirstSliceUs && f.FirstSliceUs <= f.EncodeDoneUs {
		r.firstSlice = float64(f.FirstSliceUs-f.SubmitUs) / 1000
		r.sliceRest = float64(f.EncodeDoneUs-f.FirstSliceUs) / 1000
	}
	h.mu.Lock()
	h.sent[f.Seq%uint32(len(h.sent))] = r
	h.mu.Unlock()
}

func (h *hostStages) acked(gen uint8, seq uint32, now uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := &h.sent[seq%uint32(len(h.sent))]
	if r.at == 0 || r.gen != gen || r.seq != seq {
		return // unknown, already counted or overwritten
	}
	a := *r
	r.at = 0
	a.at = now
	h.recs = append(h.recs, a)
	i := 0
	for i < len(h.recs) && now-h.recs[i].at > stageWindowUs {
		i++
	}
	h.recs = h.recs[i:]
}

// stageSummary is the host's own stage rows, "p50/p95/p99 n=N" (as the
// client reports them) over the 10 s before now; "" where nothing was
// measured: capture->encoded, encoded->sent (queue), and with sub-frame
// output encoder submit->first slice and first slice->whole frame.
type stageSummary struct{ capture, queue, firstSlice, sliceRest string }

func (h *hostStages) summary(now uint64) stageSummary {
	h.mu.Lock()
	var c, q, fs, sr []float64
	for _, r := range h.recs {
		if now-r.at > stageWindowUs {
			continue
		}
		if r.capture >= 0 {
			c = append(c, r.capture)
		}
		if r.firstSlice >= 0 {
			fs, sr = append(fs, r.firstSlice), append(sr, r.sliceRest)
		}
		q = append(q, r.queue)
	}
	h.mu.Unlock()
	return stageSummary{capture: pctString(c), queue: pctString(q), firstSlice: pctString(fs), sliceRest: pctString(sr)}
}

// pctString formats percentiles with the client's definition (sorted[floor(p*n)]).
func pctString(v []float64) string {
	if len(v) == 0 {
		return ""
	}
	sort.Float64s(v)
	q := func(p float64) float64 { return v[min(len(v)-1, int(p*float64(len(v))))] }
	return fmt.Sprintf("%.1f/%.1f/%.1f n=%d", q(0.5), q(0.95), q(0.99), len(v))
}

// stageNames are the rows a client latency summary may contain: present,
// submit and encode split capture->encoded where the native helper stamps
// present and encoder submit times; hold is the frame pacing wait (step 4.4,
// to hosts that announce FeatureStageHold).
var stageNames = map[string]bool{"present": true, "capture": true, "submit": true, "encode": true, "queue": true, "network": true,
	"transfer": true, "wait": true, "decode": true, "hold": true, "draw": true, "display": true, "e2e": true}

// stageNamesBeforeHold are the rows hosts before step 4.4 accepted (no hold),
// as the hosts before step 3.1b had them (no present, submit and encode
// either, at most nine): what the test hook pre-stage-hold (TestFaultsEnv)
// plays, the strictest host without stage-hold a client meets (hosts from
// 3.1b on took twelve).
var stageNamesBeforeHold = map[string]bool{"capture": true, "queue": true, "network": true, "transfer": true, "wait": true,
	"decode": true, "draw": true, "display": true, "e2e": true}

// rendererName accepts the presentation paths a client may name.
var rendererName = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)

// pacingModes are the frame pacing modes a client may name (step 4.4).
var pacingModes = map[string]bool{"latency": true, "smooth": true, "mixed": true}

// upscaleModes are the client-side upscaling states a client may name (Phase 5).
var upscaleModes = map[string]bool{"fsr": true, "off": true, "mixed": true}

// logStages records a client's per-stage latency summary next to the encoder
// that produced the frames, so results can be compared per GPU vendor, and
// the host's own measurement of its stages (host_capture, host_queue; with
// sub-frame output host_encode_first_slice and host_encode_rest), with the
// client's presentation path (renderer), frame pacing mode (pacing) and
// upscaling (upscale) when it names them.
func (s *Session) logStages(rows []proto.StageStat, renderer, pacing, upscale string) {
	names := stageNames
	if s.a.faults.preStageHold {
		names = stageNamesBeforeHold
	}
	p, ok := s.vid().Active()
	if !ok || len(rows) == 0 || len(rows) > len(names) {
		return
	}
	args := []any{"encoder", p.Encoder.Name, "vendor", p.Encoder.Vendor, "source", p.Source.Backend, "fps", p.FPS,
		"kbps", p.BitrateKbps}
	if rendererName.MatchString(renderer) {
		args = append(args, "renderer", renderer)
	}
	if pacingModes[pacing] {
		args = append(args, "pacing", pacing)
	}
	if upscaleModes[upscale] {
		args = append(args, "upscale", upscale)
	}
	for _, r := range rows {
		if !names[r.Name] {
			continue
		}
		if r.Name == "e2e" && (r.From == "capture" || r.From == "send") {
			args = append(args, "e2e_from", r.From)
		}
		args = append(args, r.Name, fmt.Sprintf("%.1f/%.1f/%.1f n=%d", r.P50, r.P95, r.P99, r.N))
	}
	if h := s.hostStages.summary(s.a.clock()); h.queue != "" {
		if h.capture != "" {
			args = append(args, "host_capture", h.capture)
		}
		args = append(args, "host_queue", h.queue)
		if h.firstSlice != "" {
			// Sub-frame output (sliceOutput, experimental): the frames still
			// go out whole; host_encode_rest is what sending each slice as
			// it comes could take off the client's encode row.
			args = append(args, "host_encode_first_slice", h.firstSlice, "host_encode_rest", h.sliceRest)
		}
	}
	s.log.Info("latency stages p50/p95/p99 ms, last 10 s (client; host_*: measured by the host)", args...)
}

func (s *Session) statsLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		frames := s.stats.frames.Swap(0)
		bytes := s.stats.bytes.Swap(0)
		acks := s.stats.acks.Swap(0)
		owdSum := s.stats.owdSum.Swap(0)
		owdMax := s.stats.owdMax.Swap(0)
		dropped := s.stats.dropped.Swap(0)
		recovered, byKey := s.stats.recovered.Swap(0), s.stats.recoveredByKey.Swap(0)
		cancelled, discarded, keyframes := s.stats.cancelled.Swap(0), s.stats.discarded.Swap(0), s.stats.keyframes.Swap(0)
		thinned := s.stats.thinned.Swap(0)
		reencoded := s.stats.reencoded.Swap(0)
		avg := int64(0)
		if acks > 0 {
			avg = owdSum / acks
		}
		target, ceiling := s.rate.kbps()
		est, fpsTarget, margin, loss := s.rate.state()
		args := []any{"fps", float64(frames) / 10, "mbps", float64(bytes) * 8 / 10 / 1e6,
			"owd_avg_ms", float64(avg) / 1000, "owd_max_ms", float64(owdMax) / 1000, "kbps_target", target,
			"kbps_max", ceiling, "dropped", dropped, "recovered", recovered, "recovered_by_key", byKey,
			// The loss-recovery ladder (GUIDE 2.3): frame streams cancelled
			// past their deadline (rung 1), frames not sent while the client
			// waited for a recovery or key frame, key frames asked of the
			// pipeline (rung 4).
			"deadline_drops", cancelled, "discarded", discarded, "key_frames", keyframes,
			// Phase 5: discardable frames left out under congestion
			// (thin.go), and the encoder's bitrate held down on a static
			// desktop (activity.go).
			"thinned", thinned, "static_desktop", s.static.isCapped(),
			// Phase 5 wiring B: oversized frames encoded again at a
			// higher QP (host config reencodeOversized).
			"reencoded", reencoded}
		// Send priorities (GUIDE 2.7): frames the video window held for the
		// frames in flight, and the longest hold.
		held, maxHold := s.win.stats()
		args = append(args, "window_held", held, "window_max_ms", maxHold.Milliseconds())
		// The rate controller's view: the one-way delay of the client's
		// reports (p50/p95 of their p50s, the largest maximum), the
		// continuous target, the frame rate, the queueing-delay margin and
		// the packet loss of the last second.
		if p50, p95, mx, n := s.fb.stats(); n > 0 {
			args = append(args, "report_owd_p50_ms", p50.Milliseconds(), "report_owd_p95_ms", p95.Milliseconds(), "report_owd_max_ms", mx.Milliseconds())
		}
		args = append(args, "kbps_est", int(est), "fps_target", fpsTarget, "queue_margin_ms", margin.Milliseconds(),
			"loss_pct", fmt.Sprintf("%.2f", loss*100))
		// The "datagram + FEC" mode (fec.go): frames sent as shards, the
		// parity's share of the data shards, the bytes on the wire beyond the
		// frames' (parity, headers, repairs), the client's shard loss, NACKs
		// and the repair shards that answered them.
		args = append(args, s.fecStats()...)
		s.log.Info("stream stats", args...)
	}
}
