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
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// SessionMeta describes how a connection reached the host.
type SessionMeta struct {
	Path          string // relay | direct
	User          string // authenticated by the gateway (relay path)
	RequireTicket bool   // direct path: the client must present a gateway-signed ticket
	Origin        string // direct path: Origin header of the WebTransport request
}

// Session is one streaming client.
type Session struct {
	a    *Agent
	c    transport.Conn
	meta SessionMeta
	id   string
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	ctrlMu sync.Mutex
	ctrl   transport.BidiStream

	hello       proto.Hello
	prefsMu     sync.Mutex // guards prefs, monitor, alignNotice and amfFallback
	prefs       proto.Prefs
	monitor     platform.Monitor
	alignNotice string // last coded-size alignment notice, sent once
	amfFallback string // why the last generation did not use capture "amf", logged once
	amfFailed   atomic.Bool

	video    *media.Video
	audio    *media.Audio
	frameQ   chan *media.Frame
	paused   atomic.Bool
	lastKick time.Time // last key-frame restart (any reason)
	kickMu   sync.Mutex
	rate     rateController // video bitrate: congestion back-off and recovery
	videoUp  atomic.Bool
	failures int
	tried    map[string]bool   // encoders excluded after failing
	usage    map[string]string // encoder -> usage it is retried with (media.RetryUsage)
	encFails map[string]int    // encoder -> its own start failures since a generation last went live
	triedMu  sync.Mutex        // guards tried, usage and encFails

	// Recovery "skip" bounded in time (watchHeal), guarded by healMu: the
	// live generation, how many frames after a lost one it needs to heal it
	// (0: it announced "keyframe"), and its reported loss not yet healed.
	healMu     sync.Mutex
	healGen    uint8
	healFrames int
	heal       *healWatch

	ccTarget  atomic.Pointer[ccTarget] // media congestion controller (setCongestionTarget)
	audioKbps atomic.Int64             // audio bitrate while audio runs

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
}

var errClosed = errors.New("session closed")

func (a *Agent) newSession(c transport.Conn, meta SessionMeta) *Session {
	ctx, cancel := context.WithCancel(c.Context())
	s := &Session{
		a: a, c: c, meta: meta, id: auth.RandomToken(6),
		ctx: ctx, cancel: cancel,
		frameQ:   make(chan *media.Frame, 6),
		tried:    map[string]bool{},
		usage:    map[string]string{},
		encFails: map[string]int{},
	}
	s.log = a.log.With("session", s.id, "path", meta.Path)
	s.rate.period = a.faults.ratePeriod
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
		user, err := s.a.verifyTicket(s.hello.Ticket, s.meta.Origin)
		if err != nil {
			s.sendJSON(proto.Notice{T: "error", Level: "error", Msg: "unauthorized"})
			s.c.Close(transport.CodeAuth, "unauthorized")
			return fmt.Errorf("direct ticket: %w", err)
		}
		s.meta.User = user
		if s.onAuth != nil {
			s.onAuth()
		}
	}
	s.log.Info("session started", "user", s.meta.User, "remote", s.c.RemoteAddr().String(), "ua", trunc(s.hello.Client.UA, 80))

	// One active session per host: a new connection takes over.
	s.a.setActive(s)
	defer s.a.clearActive(s)
	defer s.a.inj.ReleaseAll()
	defer s.releasePads()

	s.prefs = s.hello.Prefs
	if err := s.sendWelcome(); err != nil {
		return err
	}
	if s.hello.V >= proto.HelloVersionFrameExt {
		go s.wallClockLoop()
	}

	s.video = media.NewVideo(s.a.caps, s.log, s.a.clock)
	defer s.video.Stop()
	go s.frameSender()
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		s.notice("error", "Could not start video: "+err.Error())
		return err
	}
	s.startAudio()
	defer s.stopAudio()
	go s.datagrams()
	go s.cursorLoop()
	go s.statsLoop()
	go s.rateLoop()
	return s.controlLoop()
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Session) close(reason string) {
	s.sendJSON(proto.Notice{T: "bye", Level: "info", Msg: reason})
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
	if s.ctrl == nil {
		return errClosed
	}
	_ = s.ctrl.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return proto.WriteMsg(s.ctrl, b)
}

func (s *Session) notice(level, msg string) {
	s.sendJSON(proto.Notice{T: "notice", Level: level, Msg: msg})
}

func (s *Session) sendWelcome() error {
	w := proto.Welcome{
		T: "welcome", Session: s.id, Host: s.a.pair().Name, OS: runtime.GOOS + "/" + runtime.GOARCH, Version: Version,
		MaxKbps: s.a.cfg.MaxKbps, MaxFPS: s.a.cfg.MaxFPS,
	}
	for _, m := range s.a.monitors() {
		w.Monitors = append(w.Monitors, proto.MonitorInfo{Index: m.Index, Name: m.Name, Width: m.W, Height: m.H, X: m.X, Y: m.Y, Primary: m.Primary, Hz: m.Hz})
	}
	for _, e := range s.a.caps.Encoders {
		w.Encoders = append(w.Encoders, e.Name)
	}
	w.Features = s.a.features()
	if s.hello.V >= proto.HelloVersionFrameExt {
		w.Features = append(w.Features, proto.FeatureFrameExt)
	}
	if s.a.backendFor(s.prefs) == "test" && s.a.caps.CanDrawBarcode() {
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
		if err := s.sendJSON(proto.Clock{T: "clock", WallOffsetUs: media.WallOffset(s.a.clock)}); errors.Is(err, errClosed) {
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
// host's encoders for a w x h picture (0, 0: size unknown). An encoder that
// would pad that size (Caps.Pads; AV1 on RDNA3 at 1920x1080) gives way to
// HEVC, else H.264, also when the client asks for its codec; notice tells
// the user why ("" when nothing changed). An encoder forced in the host
// config is kept: its padding is announced for the client to crop.
func (s *Session) chooseEncoder(prefs proto.Prefs, w, h int) (e media.EncoderInfo, notice string, err error) {
	e, err = s.negotiateEncoder(prefs)
	caps := s.a.caps
	if err != nil || !caps.Pads(e.Name, w, h) {
		return e, "", err
	}
	if e.Name == s.a.cfg.Encoder {
		s.log.Debug("forced encoder pads this size, the client crops", "encoder", e.Name, "size", fmt.Sprintf("%dx%d", w, h))
		return e, "", nil
	}
	// Hardware encoders first, HEVC before H.264.
	for _, hwOnly := range []bool{true, false} {
		for _, fam := range []string{"hevc", "h264"} {
			if alt, ok := s.pickEncoder(fam, hwOnly, func(c media.EncoderInfo) bool { return !caps.Pads(c.Name, w, h) }); ok {
				a := caps.Alignment(e.Name)
				s.log.Debug("encoder would pad this size, using another codec", "encoder", e.Name, "size", fmt.Sprintf("%dx%d", w, h),
					"alignment", fmt.Sprintf("%dx%d", a.W, a.H), "using", alt.Name)
				return alt, fmt.Sprintf("%s on this GPU needs %d×%d-aligned sizes; using %s",
					familyNames[e.Family], a.W, a.H, familyNames[alt.Family]), nil
			}
		}
	}
	// Nothing else works end-to-end: keep it, VideoConfig announces the crop.
	return e, "", nil
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
// browser decodes (only hardware encoders if hwOnly) that also passes ok.
func (s *Session) pickEncoder(fam string, hwOnly bool, ok func(media.EncoderInfo) bool) (media.EncoderInfo, bool) {
	if _, dec := s.clientDecoders()[fam]; !dec {
		return media.EncoderInfo{}, false
	}
	for _, e := range s.a.caps.Encoders {
		if e.Family == fam && (!hwOnly || e.HW) && s.usableEncoder(e) && (ok == nil || ok(e)) {
			return e, true
		}
	}
	return media.EncoderInfo{}, false
}

// negotiateEncoder picks the encoder by configuration, preference and the
// browser's decoders.
func (s *Session) negotiateEncoder(prefs proto.Prefs) (media.EncoderInfo, error) {
	caps := s.a.caps
	client := s.clientDecoders()
	usable := s.usableEncoder
	if s.a.cfg.Encoder != "" {
		for _, e := range caps.Encoders {
			if e.Name == s.a.cfg.Encoder {
				if _, ok := client[e.Family]; ok && usable(e) {
					return e, nil
				}
			}
		}
	}
	pick := func(fam string, hwOnly bool) (media.EncoderInfo, bool) { return s.pickEncoder(fam, hwOnly, nil) }
	if prefs.Codec != "" && prefs.Codec != "auto" {
		if e, ok := pick(prefs.Codec, false); ok {
			return e, nil
		}
		s.notice("warn", fmt.Sprintf("Codec %s is not available end-to-end; choosing automatically.", prefs.Codec))
	}
	// Hardware encode + hardware decode first: HEVC, then AV1, then H.264.
	for _, fam := range []string{"hevc", "av1", "h264"} {
		if d, ok := client[fam]; ok && d.HW {
			if e, ok := pick(fam, true); ok {
				return e, nil
			}
		}
	}
	for _, fam := range []string{"h264", "hevc", "av1"} {
		if e, ok := pick(fam, true); ok {
			return e, nil
		}
	}
	for _, fam := range []string{"h264", "av1", "hevc"} {
		if e, ok := pick(fam, false); ok {
			return e, nil
		}
	}
	return media.EncoderInfo{}, errors.New("no codec is supported by both this browser and the host")
}

// sessionParams is the encoder-independent part of buildParams for prefs on
// monitor mon: frame rate and bitrate within the host's limits, cursor,
// capture timestamps (frameExt: the client parses the frame extension) and
// the capture source of backendFor(prefs), which it also returns.
func (a *Agent) sessionParams(prefs proto.Prefs, mon platform.Monitor, frameExt bool) (media.Params, string) {
	cfg := a.cfg
	backend := a.backendFor(prefs)
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
	return p, backend
}

func (s *Session) buildParams(prefs proto.Prefs) (media.Params, error) {
	mons := s.a.monitors()
	mon := mons[0]
	if prefs.Monitor >= 0 && prefs.Monitor < len(mons) {
		mon = mons[prefs.Monitor]
	}
	s.prefsMu.Lock()
	s.monitor = mon
	s.prefsMu.Unlock()
	s.a.inj.SetTarget(input.Rect{X: mon.X, Y: mon.Y, W: mon.W, H: mon.H})

	// Capture timestamps only reach clients that parse the frame extension.
	p, backend := s.a.sessionParams(prefs, mon, s.hello.V >= proto.HelloVersionFrameExt)
	outW, outH := p.OutputSize()
	enc, notice, err := s.chooseEncoder(prefs, outW, outH)
	if err != nil {
		return media.Params{}, err
	}
	p.Encoder = enc
	s.triedMu.Lock()
	p.Usage = s.usage[enc.Name]
	s.triedMu.Unlock()
	if backend == "amf" {
		s.useAMFCapture(&p, mon)
	}
	// Once per change: buildParams runs again for every restart.
	s.prefsMu.Lock()
	repeat := notice == s.alignNotice
	s.alignNotice = notice
	s.prefsMu.Unlock()
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
	p, backend := a.sessionParams(proto.Prefs{FPS: 60, BitrateKbps: 30000, Quality: "balanced", Cursor: "local"}, mon, true)
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
// monitor (capture "amf", experimental) unless amfCaptureBlocker or an
// earlier failure in this session rules it out; then p stays on ddagrab and
// the reason is logged once per change.
func (s *Session) useAMFCapture(p *media.Params, mon platform.Monitor) {
	why := s.a.amfCaptureBlocker(p.Encoder, p.DrawCursor, mon)
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
// bitrate is the rate controller's target, at most the settings' bitrate: a
// congestion back-off stays in effect for every restart until the controller
// raises the bitrate again or a video settings change resets it, so a
// key-frame restart after a loss does not go back to the full bitrate.
func (s *Session) startVideo(urgent bool, reason string) error {
	prefs := s.currentPrefs()
	p, err := s.buildParams(prefs)
	if err != nil {
		return err
	}
	p.BitrateKbps = s.rate.target(p.BitrateKbps)
	if reason != "" {
		s.log.Info("restarting video", "reason", reason, "urgent", urgent)
	}
	s.setCongestionTarget(p)
	return s.video.Start(p, urgent)
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
	kbps := t.videoKbps + s.audioKbps.Load() + ccOverheadKbps
	m.SetTarget(kbps*1000, t.frameInterval)
	return kbps
}

func (s *Session) videoEvents() {
	for {
		var ev media.VideoEvent
		select {
		case <-s.ctx.Done():
			return
		case ev = <-s.video.Events():
		}
		switch {
		case ev.Err != nil:
			s.handleEncoderFailure(ev)
		case ev.Config != nil:
			s.encoderLive()
			s.videoUp.Store(true)
			c := *ev.Config
			if r := s.a.faults.recovery; r != "" {
				c.Recovery = r
			}
			_, c.MaxBitrateKbps = s.rate.kbps()
			s.healConfig(&c, ev.HealFrames)
			s.sendJSON(&c)
		case ev.Frame != nil:
			if s.paused.Load() {
				continue
			}
			if n := s.a.faults.stillAfter; n > 0 && ev.Frame.Seq >= uint32(n) {
				continue // test hook: a still desktop, the source sends nothing
			}
			s.healFrame(ev.Frame)
			select {
			case s.frameQ <- ev.Frame:
			default:
				// The network cannot keep up: drop what is queued and this
				// frame (the client is told at once), cut the bitrate and
				// restart with a fresh key frame right away. Within 2 s of
				// the last cut the bitrate stays, but the restart does not
				// wait either: the dropped frames were the newest ones.
				s.reportDropped(append(s.drainQueue(), ev.Frame), "queue overflow")
				if !s.congestion(0, signalOverflow) {
					s.urgentRestart("queue overflow")
				}
			}
		}
	}
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
	s.healGen, s.healFrames, s.heal = c.Gen, 0, nil
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
	if err := s.startVideo(false, "loss not healed"); err != nil {
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
		s.cancel()
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

// congestion lowers the bitrate by 25 % (rateController.congestion) with a
// new encoder generation and reports whether it did. An emergency discards
// the current generation at once: the host's frame queue overflowed (its
// frames were dropped), or the client flushed its decoder; such cuts are at
// most 2 s apart, and a decoder flush also caps later raises. Otherwise (the
// client saw the delay grow) the restart is overlapped: the current
// generation streams on until the new one's first key frame; such a cut
// needs 10 s since the last change of the bitrate. A signal that cuts nothing
// still holds off the next raise.
func (s *Session) congestion(delayMs int, sig rateSignal) bool {
	urgent := sig != signalDelay
	from, to, ok := s.rate.congestion(sig)
	if !ok {
		s.log.Debug("congestion: bitrate kept", "kbps", from, "delayMs", delayMs, "urgent", urgent)
		return false
	}
	s.kickMu.Lock()
	s.lastKick = time.Now() // the restart below also delivers a key frame
	s.kickMu.Unlock()
	s.log.Warn("congestion: lowering bitrate", "from", from, "to", to, "delayMs", delayMs, "urgent", urgent)
	if sig == signalDecoder {
		s.log.Info("bitrate recovery limited by the client's decoder", "max", s.rate.decoderLimit())
	}
	s.notice("warn", fmt.Sprintf("Network congestion detected — bitrate lowered to %.1f Mbps", float64(to)/1000))
	if err := s.startVideo(urgent, "congestion"); err != nil {
		s.log.Warn("restart after congestion failed", "err", err)
	}
	return true
}

// rateLoop raises the bitrate after a congestion back-off once the network
// has been quiet (rateController.tick): an overlapped restart at the new
// bitrate, like a settings change.
func (s *Session) rateLoop() {
	t := time.NewTicker(rateTick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if _, live := s.video.Active(); !live || s.paused.Load() {
			s.rate.hold() // nothing streams (paused, or the encoder is starting or failing): nothing to judge
			continue
		}
		from, to, ok := s.rate.tick()
		if !ok {
			continue
		}
		s.kickMu.Lock()
		s.lastKick = time.Now() // the restart below also delivers a key frame
		s.kickMu.Unlock()
		_, ceiling := s.rate.kbps()
		s.log.Info("bitrate recovery: raising bitrate", "from", from, "to", to, "max", ceiling)
		if err := s.startVideo(false, "bitrate recovery"); err != nil {
			s.log.Warn("restart after bitrate recovery failed", "err", err)
		}
	}
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
	if stopped, starting := s.video.Hurry(); starting {
		if stopped {
			s.log.Info("restarting video", "reason", reason, "urgent", true, "takeover", true)
		}
		return
	}
	if err := s.startVideo(true, reason); err != nil {
		s.log.Warn("urgent restart failed", "reason", reason, "err", err)
	}
}

// requestKeyframe restarts the encoder for a client that needs a key frame
// (a confirmed loss under recovery "keyframe", a decoder error, or its
// watchdog): the FFmpeg command line cannot force one in a running encoder.
func (s *Session) requestKeyframe() {
	s.kickMu.Lock()
	if time.Since(s.lastKick) < 500*time.Millisecond {
		s.kickMu.Unlock()
		return
	}
	s.lastKick = time.Now()
	s.kickMu.Unlock()
	if err := s.startVideo(true, "keyframe request"); err != nil {
		s.log.Warn("keyframe restart failed", "err", err)
	}
}

func (s *Session) frameSender() {
	buf := make([]byte, 0, 1<<20)
	faults := s.a.faults
	for n := 1; ; n++ {
		var f *media.Frame
		select {
		case <-s.ctx.Done():
			return
		case f = <-s.frameQ:
		}
		s.applyCongestionTarget()
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		st, err := s.c.OpenUniStreamSync(ctx)
		cancel()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.log.Debug("open frame stream", "err", err)
			s.reportDropped([]*media.Frame{f}, "stream failed")
			continue
		}
		h, ext := videoHeader(f, s.hello.V, s.a.clock())
		buf = buf[:proto.FrameHeaderLen]
		h.Marshal(buf)
		if h.Flags&proto.FrameFlagExt != 0 {
			buf = ext.Append(buf)
		}
		buf = append(buf, f.Data...)
		if drop, delay := faults.at(n); drop {
			// Test hook: the stream fails mid-frame.
			_ = st.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = st.Write(buf[:len(buf)/2])
			st.CancelWrite()
			s.reportDropped([]*media.Frame{f}, "test fault")
			continue
		} else if delay > 0 {
			// Test hook: this frame arrives late, the next ones on time.
			s.log.Debug("test fault: delaying frame", "gen", f.Gen, "seq", f.Seq, "delay", delay)
			late := append([]byte(nil), buf...)
			time.AfterFunc(delay, func() { s.sendFrame(st, f, h, late) })
			continue
		}
		s.sendFrame(st, f, h, buf)
	}
}

// sendFrame writes a frame (header h, encoded as b) to its stream; a frame
// that cannot be written is reported dropped.
func (s *Session) sendFrame(st transport.SendStream, f *media.Frame, h proto.FrameHeader, b []byte) {
	_ = st.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := st.Write(b); err != nil {
		st.CancelWrite()
		if s.ctx.Err() == nil {
			s.reportDropped([]*media.Frame{f}, "stream failed")
		}
		return
	}
	st.Close()
	s.rate.sent()
	s.stats.frames.Add(1)
	s.stats.bytes.Add(int64(len(b)))
	if h.Flags&proto.FrameFlagExt != 0 {
		s.hostStages.sentFrame(f, h.SendUs)
	}
}

// videoHeader builds a video frame's header, plus the extension for clients
// that parse it. now is the host clock as the frame goes to the transport.
func videoHeader(f *media.Frame, helloV int, now uint64) (proto.FrameHeader, proto.FrameExt) {
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
	return h, ext
}

// ---------------------------------------------------------------------------
// Audio

func (s *Session) startAudio() {
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
	s.audio = media.NewAudio(s.a.audioSource, media.AudioConfig{Codec: codec, BitrateKbps: s.a.cfg.AudioKbps}, s.log)
	s.sendJSON(proto.AudioConfig{T: "audio", Enabled: true, Codec: codec, SampleRate: 48000, Channels: 2, FrameMs: s.audio.FrameMs()})
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
	if s.audio != nil {
		s.audio.Stop()
		s.audio = nil
		s.audioKbps.Store(0)
	}
}

// ---------------------------------------------------------------------------
// Input

func (s *Session) inputLoop(st transport.BidiStream) {
	defer st.CancelRead()
	inj := s.a.inj
	for {
		b, err := proto.ReadMsg(st, proto.MaxInputMsg)
		if err != nil {
			return
		}
		if !s.a.isActive(s) {
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
				_ = s.c.SendDatagram(p)
			}
		case proto.DgMouseRel:
			if m, ok := proto.ParseMouseRel(d); ok && s.a.isActive(s) {
				if dx, dy := s.rel.Update(m.Seq, m.CumX, m.CumY); dx != 0 || dy != 0 {
					_ = s.a.inj.MoveRel(dx, dy)
				}
			}
		case proto.DgMouseAbs:
			if m, ok := proto.ParseMouseAbs(d); ok && s.absGate.Accept(m.Seq) && s.a.isActive(s) {
				_ = s.a.inj.MoveAbs(m.X, m.Y)
			}
		case proto.DgGamepad:
			if g, ok := proto.ParseGamepad(d); ok && g.Index < 4 && s.padGates[g.Index].Accept(g.Seq) {
				s.gamepad(g)
			}
		case proto.DgFrameAck:
			if a, ok := proto.ParseFrameAck(d); ok {
				s.hostStages.acked(a.Gen, a.Seq, s.a.clock())
				s.rate.ack(time.Duration(a.OWDUs) * time.Microsecond)
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

func (s *Session) gamepad(g proto.Gamepad) {
	pads := s.a.gamepads()
	if pads == nil {
		s.padWarn.Do(func() {
			s.notice("warn", "Gamepad detected, but the host has no ViGEmBus driver installed — controller input is disabled.")
		})
		return
	}
	if !g.Connected {
		pads.Unplug(int(g.Index))
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

func (s *Session) cursorLoop() {
	if !s.a.cursorSupported() || s.a.cfg.DrawCursor || s.currentPrefs().Cursor == "video" {
		return
	}
	t := time.NewTicker(8 * time.Millisecond)
	defer t.Stop()
	var lastHandle uint64
	lastVisible := false
	var lastX, lastY uint16
	var seq uint32
	sent := map[uint64]bool{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		cs, err := platform.GetCursor()
		if err != nil {
			continue
		}
		if cs.Visible != lastVisible || (cs.Visible && cs.Handle != lastHandle) {
			lastVisible, lastHandle = cs.Visible, cs.Handle
			msg := proto.CursorShape{T: "cursor", ID: cs.Handle, Hidden: !cs.Visible}
			if cs.Visible && !sent[cs.Handle] {
				if shape, err := platform.CursorImage(cs.Handle); err == nil {
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
			if nx != lastX || ny != lastY {
				lastX, lastY = nx, ny
				seq++
				_ = s.c.SendDatagram(proto.CursorPos(seq, cs.Visible, nx, ny))
			}
		}
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
			if videoChanged {
				s.triedMu.Lock()
				s.tried = map[string]bool{}
				s.usage = map[string]string{}
				clear(s.encFails)
				s.triedMu.Unlock()
				// An explicit video choice resets the congestion back-off;
				// an audio-only change keeps it.
				s.rate.reset()
				if err := s.startVideo(false, "settings"); err != nil {
					s.notice("error", "Could not apply settings: "+err.Error())
				}
			}
			if old.AudioEnabled() != m.Prefs.AudioEnabled() || old.AudioCodec != m.Prefs.AudioCodec {
				s.stopAudio()
				s.startAudio()
			}
		case "keyframe":
			s.requestKeyframe()
		case "stages":
			s.logStages(m.Stages)
		case "congestion":
			// Overlapped: the client keeps decoding the current generation
			// until the new one is ready, unless it flushed its decoder: then
			// it waits for a key frame, also when the bitrate stays.
			if m.Reason != proto.CongestionDecoder {
				s.congestion(m.DelayMs, signalDelay)
			} else if !s.congestion(m.DelayMs, signalDecoder) {
				s.requestKeyframe()
			}
		case "pause":
			if !s.paused.Swap(true) {
				s.log.Info("client hidden: pausing video")
				s.video.Suspend()
				s.drainQueue()
			}
		case "resume":
			if s.paused.Swap(false) {
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
// frames a v2 client acknowledged in the last 10 s (it acks exactly the frames
// it records stages for). They are logged next to the client's summary as a
// reference for its rows that needs no clock sync.
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
}

const stageWindowUs = 10_000_000

func (h *hostStages) sentFrame(f *media.Frame, sentUs uint64) {
	r := hostStage{gen: f.Gen, seq: f.Seq, at: sentUs, capture: -1, queue: float64(sentUs-f.EncodeDoneUs) / 1000}
	if f.CaptureUs != 0 {
		r.capture = float64(f.EncodeDoneUs-f.CaptureUs) / 1000
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

// summary returns "p50/p95/p99 n=N" (as the client reports them) of the host's
// capture->encoded and queue times over the 10 s before now; "" if none.
func (h *hostStages) summary(now uint64) (capture, queue string) {
	h.mu.Lock()
	var c, q []float64
	for _, r := range h.recs {
		if now-r.at > stageWindowUs {
			continue
		}
		if r.capture >= 0 {
			c = append(c, r.capture)
		}
		q = append(q, r.queue)
	}
	h.mu.Unlock()
	return pctString(c), pctString(q)
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

// stageNames are the rows a client latency summary may contain.
var stageNames = map[string]bool{"capture": true, "queue": true, "network": true, "transfer": true, "wait": true,
	"decode": true, "draw": true, "display": true, "e2e": true}

// logStages records a client's per-stage latency summary next to the encoder
// that produced the frames, so results can be compared per GPU vendor, and
// the host's own measurement of its stages (host_capture, host_queue).
func (s *Session) logStages(rows []proto.StageStat) {
	p, ok := s.video.Active()
	if !ok || len(rows) == 0 || len(rows) > len(stageNames) {
		return
	}
	args := []any{"encoder", p.Encoder.Name, "vendor", p.Encoder.Vendor, "source", p.Source.Backend, "fps", p.FPS,
		"kbps", p.BitrateKbps}
	for _, r := range rows {
		if !stageNames[r.Name] {
			continue
		}
		if r.Name == "e2e" && (r.From == "capture" || r.From == "send") {
			args = append(args, "e2e_from", r.From)
		}
		args = append(args, r.Name, fmt.Sprintf("%.1f/%.1f/%.1f n=%d", r.P50, r.P95, r.P99, r.N))
	}
	if c, q := s.hostStages.summary(s.a.clock()); q != "" {
		if c != "" {
			args = append(args, "host_capture", c)
		}
		args = append(args, "host_queue", q)
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
		avg := int64(0)
		if acks > 0 {
			avg = owdSum / acks
		}
		target, ceiling := s.rate.kbps()
		s.log.Info("stream stats", "fps", float64(frames)/10, "mbps", float64(bytes)*8/10/1e6,
			"owd_avg_ms", float64(avg)/1000, "owd_max_ms", float64(owdMax)/1000, "kbps_target", target,
			"kbps_max", ceiling, "dropped", dropped)
	}
}
