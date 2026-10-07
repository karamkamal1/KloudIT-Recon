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

	hello   proto.Hello
	prefsMu sync.Mutex // guards prefs and monitor
	prefs   proto.Prefs
	monitor platform.Monitor

	video    *media.Video
	audio    *media.Audio
	frameQ   chan *media.Frame
	paused   atomic.Bool
	lastKick time.Time // last key-frame restart (any reason)
	lastCong time.Time // last congestion back-off
	kickMu   sync.Mutex
	curKbps  atomic.Int64
	videoUp  atomic.Bool
	failures int
	tried    map[string]bool
	triedMu  sync.Mutex

	rel      input.RelTracker
	absGate  input.SeqGate
	padGates [4]input.SeqGate
	padWarn  sync.Once

	stats  sessionStats
	onAuth func()
}

type sessionStats struct {
	frames, bytes, acks atomic.Int64
	owdSum              atomic.Int64
	owdMax              atomic.Int64
}

var errClosed = errors.New("session closed")

func (a *Agent) newSession(c transport.Conn, meta SessionMeta) *Session {
	ctx, cancel := context.WithCancel(c.Context())
	s := &Session{
		a: a, c: c, meta: meta, id: auth.RandomToken(6),
		ctx: ctx, cancel: cancel,
		frameQ: make(chan *media.Frame, 6),
		tried:  map[string]bool{},
	}
	s.log = a.log.With("session", s.id, "path", meta.Path)
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
	return s.sendJSON(w)
}

// ---------------------------------------------------------------------------
// Video

func (s *Session) currentPrefs() proto.Prefs {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.prefs
}

// chooseEncoder negotiates the codec between the browser's decoders and the
// host's encoders.
func (s *Session) chooseEncoder(prefs proto.Prefs) (media.EncoderInfo, error) {
	caps := s.a.caps
	client := map[string]proto.DecoderInfo{}
	for _, d := range s.hello.Decoders {
		client[d.Family] = d
	}
	usable := func(e media.EncoderInfo) bool {
		s.triedMu.Lock()
		defer s.triedMu.Unlock()
		return !s.tried[e.Name]
	}
	if s.a.cfg.Encoder != "" {
		for _, e := range caps.Encoders {
			if e.Name == s.a.cfg.Encoder {
				if _, ok := client[e.Family]; ok && usable(e) {
					return e, nil
				}
			}
		}
	}
	pick := func(fam string, hwOnly bool) (media.EncoderInfo, bool) {
		if _, ok := client[fam]; !ok {
			return media.EncoderInfo{}, false
		}
		for _, e := range caps.Encoders {
			if e.Family == fam && (!hwOnly || e.HW) && usable(e) {
				return e, true
			}
		}
		return media.EncoderInfo{}, false
	}
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

func (s *Session) buildParams(prefs proto.Prefs) (media.Params, error) {
	cfg := s.a.cfg
	enc, err := s.chooseEncoder(prefs)
	if err != nil {
		return media.Params{}, err
	}
	mons := s.a.monitors()
	mon := mons[0]
	if prefs.Monitor >= 0 && prefs.Monitor < len(mons) {
		mon = mons[prefs.Monitor]
	}
	s.prefsMu.Lock()
	s.monitor = mon
	s.prefsMu.Unlock()
	s.a.inj.SetTarget(input.Rect{X: mon.X, Y: mon.Y, W: mon.W, H: mon.H})

	fps := prefs.FPS
	if fps <= 0 {
		fps = cfg.DefaultFPS
	}
	if fps > cfg.MaxFPS {
		fps = cfg.MaxFPS
	}
	if mon.Hz > 0 && fps > mon.Hz && s.a.backendFor(prefs) != "test" {
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
		Encoder:     enc,
		FPS:         fps,
		BitrateKbps: kbps,
		Quality:     prefs.Quality,
		DrawCursor:  cfg.DrawCursor || prefs.Cursor == "video" || !s.a.cursorSupported(),
		// Capture timestamps only reach clients that parse the frame extension.
		CaptureClock: s.hello.V >= proto.HelloVersionFrameExt && cfg.CaptureTimestamps != "off" && s.a.caps.CanStampCapture(),
	}
	backend := s.a.backendFor(prefs)
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
	case "x11grab":
		p.Source = media.Source{Backend: "x11grab", Display: cfg.X11Display, X: mon.X, Y: mon.Y, NativeW: mon.W, NativeH: mon.H}
		p.Width, p.Height = w, h
	case "gfxcapture":
		p.Source = media.Source{Backend: "gfxcapture", HMonitor: mon.HMonitor, Window: prefs.Window}
		p.Width, p.Height = w, h
	case "ddagrab":
		out := mon.DXGIOutput
		if out < 0 {
			out = mon.Index
		}
		p.Source = media.Source{Backend: "ddagrab", Output: out}
	}
	return p, nil
}

// backendFor picks the capture backend for the requested preferences.
func (a *Agent) backendFor(prefs proto.Prefs) string {
	switch a.cfg.Capture {
	case "ddagrab", "gfxcapture", "x11grab", "test":
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
// immediately (used when the client needs a key frame right away).
func (s *Session) startVideo(urgent bool, reason string) error {
	prefs := s.currentPrefs()
	p, err := s.buildParams(prefs)
	if err != nil {
		return err
	}
	if kb := s.curKbps.Load(); kb > 0 && reason == "congestion" {
		p.BitrateKbps = int(kb)
	}
	s.curKbps.Store(int64(p.BitrateKbps))
	if reason != "" {
		s.log.Info("restarting video", "reason", reason, "urgent", urgent)
	}
	return s.video.Start(p, urgent)
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
			s.handleEncoderFailure(ev.Err)
		case ev.Config != nil:
			s.failures = 0
			s.videoUp.Store(true)
			s.sendJSON(ev.Config)
		case ev.Frame != nil:
			if s.paused.Load() {
				continue
			}
			select {
			case s.frameQ <- ev.Frame:
			default:
				// The network cannot keep up: drop what is queued, cut the
				// bitrate and restart with a fresh key frame.
				s.drainQueue()
				s.congestion(0)
			}
		}
	}
}

func (s *Session) drainQueue() {
	for {
		select {
		case <-s.frameQ:
		default:
			return
		}
	}
}

func (s *Session) handleEncoderFailure(err error) {
	s.failures++
	s.log.Warn("encoder failed", "err", err, "attempt", s.failures)
	if p, ok := s.video.Current(); ok && s.failures >= 2 {
		// Same encoder failed twice: exclude it and fall back to the next one.
		s.triedMu.Lock()
		s.tried[p.Encoder.Name] = true
		s.triedMu.Unlock()
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

// congestion lowers the bitrate by 25 % and forces a new key frame. It is
// rate-limited to once every two seconds.
func (s *Session) congestion(delayMs int) {
	s.kickMu.Lock()
	if time.Since(s.lastCong) < 2*time.Second {
		s.kickMu.Unlock()
		return
	}
	s.lastCong = time.Now()
	s.lastKick = s.lastCong // the restart below also delivers a key frame
	s.kickMu.Unlock()
	cur := s.curKbps.Load()
	next := cur * 3 / 4
	if next < 2000 {
		next = 2000
	}
	s.curKbps.Store(next)
	s.log.Warn("congestion: lowering bitrate", "from", cur, "to", next, "delayMs", delayMs)
	s.notice("warn", fmt.Sprintf("Network congestion detected — bitrate lowered to %.1f Mbps", float64(next)/1000))
	if err := s.startVideo(true, "congestion"); err != nil {
		s.log.Warn("restart after congestion failed", "err", err)
	}
}

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
	for {
		var f *media.Frame
		select {
		case <-s.ctx.Done():
			return
		case f = <-s.frameQ:
		}
		h := proto.FrameHeader{Type: proto.FrameTypeVideo, Gen: f.Gen, Seq: f.Seq, PtsUs: uint64(f.PtsUs)}
		if f.Key {
			h.Flags |= proto.FrameFlagKey
		}
		var ext proto.FrameExt
		if s.hello.V >= proto.HelloVersionFrameExt {
			h.Flags |= proto.FrameFlagExt
			ext.Set(proto.ExtEncodeDoneUs, f.EncodeDoneUs)
			if f.CaptureUs != 0 {
				ext.Set(proto.ExtCaptureUs, f.CaptureUs)
			}
		}
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		st, err := s.c.OpenUniStreamSync(ctx)
		cancel()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.log.Debug("open frame stream", "err", err)
			continue
		}
		// SendUs is the moment the frame is handed to the transport, so
		// encodeDone -> send is the host queue (frame queue + stream credit).
		h.SendUs = s.a.clock()
		buf = buf[:proto.FrameHeaderLen]
		h.Marshal(buf)
		if h.Flags&proto.FrameFlagExt != 0 {
			buf = ext.Append(buf)
		}
		buf = append(buf, f.Data...)
		_ = st.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := st.Write(buf); err != nil {
			st.CancelWrite()
			continue
		}
		st.Close()
		s.stats.frames.Add(1)
		s.stats.bytes.Add(int64(len(buf)))
	}
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
	}
}

func (s *Session) stopAudio() {
	if s.audio != nil {
		s.audio.Stop()
		s.audio = nil
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
			s.curKbps.Store(0) // explicit user choice resets congestion back-off
			videoChanged := old.Codec != m.Prefs.Codec || old.BitrateKbps != m.Prefs.BitrateKbps || old.FPS != m.Prefs.FPS ||
				old.Width != m.Prefs.Width || old.Height != m.Prefs.Height || old.Monitor != m.Prefs.Monitor ||
				old.Window != m.Prefs.Window || old.Quality != m.Prefs.Quality || old.Cursor != m.Prefs.Cursor
			if videoChanged {
				s.triedMu.Lock()
				s.tried = map[string]bool{}
				s.triedMu.Unlock()
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
			s.congestion(m.DelayMs)
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

// stageNames are the rows a client latency summary may contain.
var stageNames = map[string]bool{"capture": true, "queue": true, "network": true, "transfer": true, "wait": true,
	"decode": true, "draw": true, "display": true, "e2e": true}

// logStages records a client's per-stage latency summary next to the encoder
// that produced the frames, so results can be compared per GPU vendor.
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
	s.log.Info("latency stages p50/p95/p99 ms (client, last 10 s)", args...)
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
		avg := int64(0)
		if acks > 0 {
			avg = owdSum / acks
		}
		s.log.Info("stream stats", "fps", float64(frames)/10, "mbps", float64(bytes)*8/10/1e6,
			"owd_avg_ms", float64(avg)/1000, "owd_max_ms", float64(owdMax)/1000, "kbps_target", s.curKbps.Load())
	}
}
