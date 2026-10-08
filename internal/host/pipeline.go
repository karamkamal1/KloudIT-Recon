package host

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The session's video pipeline (host config "pipeline", GUIDE 3.1): the
// native encoder helper (media.HelperVideo) or FFmpeg (media.Video). Both are
// a media.Pipeline; the session decides what to do from its Capabilities.

// helperExeName is the native encoder helper, installed next to recon-host.exe.
const helperExeName = "recon-encoder.exe"

// helperBackends are the helper's encoder backends in the order sessions try
// them (GUIDE 3.8): the GPU vendors' own encoders (AMF, NVENC: reference
// recovery, seamless bitrate changes), then libavcodec (Intel Quick Sync
// Video through FFmpeg's shared libraries: key-frame recovery). After them
// comes the FFmpeg command line.
var helperBackends = []string{"amf", "nvenc", backendLavc}

// backendLavc is the helper's libavcodec backend.
const backendLavc = "lavc"

// setupHelper finds the native encoder helper next to the running executable
// (Windows, host config "pipeline" auto or helper) and the FFmpeg libraries of
// its libavcodec backend (host config "helperFFmpegDir"), and logs what it
// found.
func (a *Agent) setupHelper() {
	switch {
	case a.cfg.pipeline() == media.PipelineFFmpeg:
		a.helperMissing = `host config "pipeline" is "ffmpeg"`
	case runtime.GOOS != "windows":
		a.helperMissing = "the native encoder helper is Windows-only"
	default:
		exe, err := os.Executable()
		dir := ""
		if err == nil {
			dir = filepath.Dir(exe)
			exe = filepath.Join(dir, helperExeName)
			_, err = os.Stat(exe)
		}
		if err != nil {
			a.helperMissing = helperExeName + " is not installed next to recon-host: " + err.Error()
			break
		}
		ffDir := a.cfg.helperFFmpegDir(dir)
		a.lavcMissing = encoder.LavcMissing(ffDir)
		a.launchHelper = func(log *slog.Logger, backend string) (*encoder.Helper, error) {
			return encoder.Launch(encoder.Options{Exe: exe, Backend: backend, FFmpegDir: ffDir, Log: log, CapsTimeout: 5 * time.Second})
		}
		lavc := "libraries in " + ffDir
		switch {
		case !a.cfg.libavcodecOn():
			lavc = `off (host config "helperLibavcodec")`
		case a.lavcMissing != "":
			lavc = "not installed: " + a.lavcMissing
		}
		a.log.Info("native encoder helper installed", "path", exe, "pipeline", a.cfg.pipeline(), "libavcodec", lavc)
		return
	}
	a.log.Info("native encoder helper not used", "reason", a.helperMissing, "pipeline", a.cfg.pipeline())
}

// vid returns the session's video pipeline.
func (s *Session) vid() media.Pipeline {
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()
	return s.video
}

// onHelper reports whether the session runs on the native helper, and its
// encoders (media.HelperEncoders) if so.
func (s *Session) onHelper() (bool, []media.EncoderInfo, encoder.Caps) {
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()
	return s.helperEncs != nil, s.helperEncs, s.helperCaps
}

// encoders returns the encoders the session negotiates from: the helper's
// first while the session runs on it (hardware encoders of its vendor), then
// FFmpeg's.
func (s *Session) encoders() []media.EncoderInfo {
	_, encs, _ := s.onHelper()
	if encs == nil {
		return s.a.caps.Encoders
	}
	return append(slices.Clone(encs), s.a.caps.Encoders...)
}

// openPipeline decides the session's video pipeline, once, trying in this
// order (GUIDE 3.8): the native helper with a GPU vendor's encoder backend
// (AMF, NVENC), the helper's libavcodec backend (Intel Quick Sync Video; host
// config "helperLibavcodec", its FFmpeg libraries in "helperFFmpegDir"), then
// FFmpeg's command line. The helper is used when the host config allows it
// ("pipeline" auto or helper), it is installed, starts and can encode, the
// codec negotiated with this browser is one of its codecs and the session
// needs nothing only FFmpeg offers (helperBlocker); chooseHelper steps through
// its backends. It logs the decision, why, and why each earlier rung was
// skipped, and returns a notice for the user when "pipeline" "helper" could
// not be honoured. Later the session moves to FFmpeg for good when it needs
// FFmpeg after all (leaveHelper).
func (s *Session) openPipeline() (notice string) {
	mode := s.a.cfg.pipeline()
	prefs := s.currentPrefs()
	drawCursor := s.a.cfg.DrawCursor || prefs.Cursor == "video" || !s.a.cursorSupported() // as sessionParams
	why := s.a.helperMissing
	if why == "" && s.a.launchHelper == nil {
		why = "no native encoder helper"
	}
	if why == "" {
		why = s.helperBlocker(prefs, drawCursor, nil)
	}
	var h *encoder.Helper
	skipped := helperSkips{}
	if why == "" {
		h, why = s.chooseHelper(prefs, drawCursor, skipped)
	}
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()
	if h == nil {
		s.helperEncs, s.helperCaps = nil, encoder.Caps{}
		s.video = media.NewVideo(s.a.caps, s.log, s.a.clock)
		attrs := []any{"pipeline", media.PipelineFFmpeg, "config", mode, "reason", why}
		if len(skipped) > 0 {
			attrs = append(attrs, "skipped", skipped.String())
		}
		s.log.Info("video pipeline", attrs...)
		if mode == media.PipelineHelper {
			if len(skipped) > 0 {
				why += "; " + skipped.String()
			}
			return "The native encoder is not used (" + why + "); streaming with FFmpeg."
		}
		return ""
	}
	c := h.Caps()
	lb := s.a.liveBitrateResults(s.log, c)
	backend := c.Backend // restarts and the spare run the same backend
	s.video = media.NewHelperVideo(media.HelperOptions{
		Launch: func() (*encoder.Helper, error) { return s.a.launchHelper(s.log, backend) },
		First:  h, Log: s.log, Clock: s.a.hostClock, KeepSpare: true,
		LiveBitrate: func(c encoder.Caps, sp encoder.StartParams, adaptive bool) (string, string, bool) {
			return lb.Choose(c, sp, adaptive)
		},
	})
	attrs := []any{"pipeline", media.PipelineHelper, "config", mode, "backend", c.Backend, "vendor", c.Vendor,
		"adapter", c.AdapterName, "encoders", encoderNames(s.helperEncs), "capture", strings.Join(c.Capture, ","),
		"hags", hagsText(c.HAGSEnabled)}
	if len(skipped) > 0 {
		attrs = append(attrs, "skipped", skipped.String())
	}
	s.log.Info("video pipeline", attrs...)
	return ""
}

// helperSkips collects why the pipeline selection passed over each of the
// helper's backends (backend -> why), for the "video pipeline" log line.
type helperSkips map[string]string

// String lists them in the selection order ("amf: ...; nvenc: ...").
func (k helperSkips) String() string {
	var parts []string
	for _, b := range helperBackends {
		if why, ok := k[b]; ok {
			parts = append(parts, b+": "+why)
		}
	}
	for _, b := range slices.Sorted(maps.Keys(k)) {
		if !slices.Contains(helperBackends, b) {
			parts = append(parts, b+": "+k[b])
		}
	}
	return strings.Join(parts, "; ")
}

// chooseHelper launches the native helper and returns it when it can serve
// the session (helperFits), else nil and why not. The first launch lets the
// helper choose its backend ("auto": the primary display adapter's vendor
// encoder first, libavcodec last, or first on an Intel primary adapter, whose
// outputs AMF and NVENC cannot encode); when that backend cannot serve the
// session for a reason of its own (its encoder runs on another adapter than
// the monitor's, the libavcodec backend is off, the codec negotiated with
// this browser is not one of its codecs), the next backend in the selection
// order (helperBackends) the helper reported usable is launched instead.
// skipped gets why each backend before the one chosen (all of them, when
// none is, except the last one tried: its reason is returned) was passed
// over.
func (s *Session) chooseHelper(prefs proto.Prefs, drawCursor bool, skipped helperSkips) (*encoder.Helper, string) {
	mon := s.a.monitorFor(prefs)
	tried := map[string]bool{}
	unavailable := map[string]string{} // over every launch
	choice := ""                       // the helper's own choice of backend, for the log
	// fill notes why the backends before index upTo of helperBackends were
	// not used where no launch said so: unavailable, turned off, not
	// installed, or usable but not the helper's choice.
	fill := func(upTo int, except string) {
		for _, b := range helperBackends[:upTo] {
			if _, done := skipped[b]; done || b == except {
				continue
			}
			switch {
			case b == backendLavc && !s.a.cfg.libavcodecOn():
				skipped[b] = `off (host config "helperLibavcodec")`
			case b == backendLavc && s.a.lavcMissing != "":
				skipped[b] = "its FFmpeg libraries are not installed: " + s.a.lavcMissing
			case unavailable[b] != "":
				skipped[b] = unavailable[b]
			case tried[b]:
				skipped[b] = "not usable"
			default:
				skipped[b] = "usable, not tried (" + choice + ")"
			}
		}
	}
	backend := "" // auto
	for {
		h, err := s.a.launchHelper(s.log, backend)
		if err != nil {
			if backend == "" {
				return nil, "it did not start: " + err.Error()
			}
			tried[backend] = true
			fill(len(helperBackends), backend)
			return nil, fmt.Sprintf("it did not start with backend %s: %v", backend, err)
		}
		c := h.Caps()
		tried[backend], tried[c.Backend] = true, true
		for k, v := range c.Unavailable {
			if unavailable[k] == "" {
				unavailable[k] = v
			}
		}
		if choice == "" {
			choice = "the helper chose " + c.Backend
			if c.AdapterName != "" {
				choice += " for " + c.AdapterName
			}
		}
		why, backendOnly := s.helperFits(prefs, drawCursor, mon, &c)
		if why == "" {
			if i := slices.Index(helperBackends, c.Backend); i > 0 {
				fill(i, "")
			}
			return h, ""
		}
		go h.Close()
		switch {
		case !c.Usable() && backend != "":
			fill(len(helperBackends), backend)
			why := c.Unavailable[backend]
			if why == "" {
				why = "no encoder"
			}
			return nil, fmt.Sprintf("its %s backend is not usable (%s)", backend, why)
		case !c.Usable():
			fill(len(helperBackends), "")
			return nil, why
		case !backendOnly:
			return nil, why // the session needs something no backend changes
		}
		next := ""
		for _, b := range helperBackends {
			usable := unavailable[b] == "" && (b != backendLavc || s.a.cfg.libavcodecOn() && s.a.lavcMissing == "")
			if !tried[b] && usable {
				next = b
				break
			}
		}
		if next == "" {
			fill(len(helperBackends), c.Backend)
			return nil, why
		}
		skipped[c.Backend] = why
		s.log.Info("native encoder helper: trying another backend", "backend", next, "instead_of", c.Backend, "reason", why)
		backend = next
	}
}

// helperFits returns why the helper with caps c cannot serve a session with
// prefs on monitor mon, or "" if it can; backendOnly: the reason is its
// backend's (another backend may serve the session). On success the session's
// encoders are the helper's (onHelper).
func (s *Session) helperFits(prefs proto.Prefs, drawCursor bool, mon platform.Monitor, c *encoder.Caps) (why string, backendOnly bool) {
	switch {
	case !c.Usable():
		return "it has no usable encoder", false
	case c.Backend == backendLavc && !s.a.cfg.libavcodecOn():
		return `its libavcodec backend is off (host config "helperLibavcodec")`, true
	}
	if why := s.helperBlocker(prefs, drawCursor, c); why != "" {
		return why, false
	}
	if why := s.adapterBlocker(prefs, mon, c); why != "" {
		return why, true
	}
	// The codec this browser and the host agree on must be one of the
	// helper's (its encoders come first in the negotiation), at the stream's
	// size as buildParams negotiates it (padding, decode times).
	s.pipeMu.Lock()
	s.helperEncs, s.helperCaps = media.HelperEncoders(*c), *c
	encs := s.helperEncs
	s.pipeMu.Unlock()
	p, _ := s.a.sessionParams(prefs, mon, s.hello.V >= proto.HelloVersionFrameExt)
	w, h := p.OutputSize()
	if e, _, err := s.negotiateEncoder(prefs, w, h, false); err != nil || !e.Helper {
		return fmt.Sprintf("the codec negotiated with this browser (%s) is not one of the helper's (%s)", e.Name, encoderNames(encs)), true
	}
	return "", false
}

// adapterBlocker returns why the helper with caps c cannot encode monitor
// mon, or "": every backend encodes on one GPU (c.AdapterLUID) and takes only
// captures of that GPU's outputs (the helper captures on the output's own
// GPU: DXGI Desktop Duplication, AMD Direct Capture and its WGC device alike),
// so a monitor on another GPU (a laptop's external port on the discrete GPU,
// a desktop with monitors on two) needs another backend. A window capture
// (its monitor is not known here) and the test source are not checked, nor a
// monitor the caps do not list.
func (s *Session) adapterBlocker(prefs proto.Prefs, mon platform.Monitor, c *encoder.Caps) string {
	if prefs.Window != "" || s.a.cfg.Capture == "test" || mon.HMonitor == 0 || c.AdapterLUID == "" {
		return ""
	}
	for _, o := range c.Outputs {
		if o.HMonitor == mon.HMonitor && o.AdapterLUID != "" && !strings.EqualFold(o.AdapterLUID, c.AdapterLUID) {
			return fmt.Sprintf("its %s encoder runs on %s, the monitor (%s) is on %s", c.Backend, c.AdapterName, o.Name, o.AdapterName)
		}
	}
	return ""
}

// liveBitrateResults reads the live-bitrate qualification of the helper's
// encoder (recon-host qualify, GUIDE 3.6: live-bitrate.json next to the host
// config) for a session on the helper with caps c, and logs what it says; nil
// when there is none or it is for another GPU or backend (the helper's
// defaults apply).
func (a *Agent) liveBitrateResults(log *slog.Logger, c encoder.Caps) *qualify.Results {
	if a.cfg.path == "" {
		return nil
	}
	path := qualify.PathFor(a.cfg.path)
	r, err := qualify.Load(path)
	switch {
	case err != nil:
		log.Warn("live-bitrate qualification not used", "file", path, "err", err)
		return nil
	case r == nil:
		log.Info("no live-bitrate qualification: the helper's defaults apply (run recon-host qualify)", "file", path)
		return nil
	}
	if ok, why := r.Matches(c); !ok {
		log.Warn("live-bitrate qualification not used", "file", path, "reason", why)
		return nil
	}
	log.Info("live-bitrate qualification", "file", path, "measured", r.Time.Format(time.RFC3339), "adapter", r.AdapterName,
		"choice", strings.Join(r.ChoiceLines(), "; "))
	return r
}

// leaveHelper moves the session from the native helper to FFmpeg for the
// rest of the session; the FFmpeg generations continue the client's
// generation numbers. The caller starts the next generation.
func (s *Session) leaveHelper(reason string) {
	s.pipeMu.Lock()
	old, ok := s.video.(*media.HelperVideo)
	if !ok {
		s.pipeMu.Unlock()
		return
	}
	v := media.NewVideo(s.a.caps, s.log, s.a.clock)
	v.ContinueAfter(old.Gen())
	s.video, s.helperEncs, s.helperCaps = v, nil, encoder.Caps{}
	s.pipeMu.Unlock()
	old.Stop()
	select {
	case s.pipeSwap <- struct{}{}: // videoEvents reads the new pipeline's events
	default:
	}
	s.log.Info("video pipeline", "pipeline", media.PipelineFFmpeg, "was", media.PipelineHelper, "reason", reason)
}

// helperBlocker returns why the session cannot use the native helper, or ""
// if it can: something only FFmpeg offers (an FFmpeg capture source or
// encoder named in host.json), and with the helper's caps the video carrying
// the cursor (drawCursor; the helper's captures leave it out: cursorInVideo
// false) and the capture methods it lacks (window capture without WGC, AMD
// Direct Capture, DDA).
func (s *Session) helperBlocker(prefs proto.Prefs, drawCursor bool, c *encoder.Caps) string {
	cfg := s.a.cfg
	switch {
	case cfg.Capture == "x11grab":
		return `capture "x11grab" is FFmpeg's`
	case cfg.Capture == "test" && cfg.pipeline() != media.PipelineHelper:
		return `the test pattern (capture "test") is FFmpeg's; "pipeline": "helper" streams the helper's synthetic source instead`
	case cfg.Encoder != "" && !strings.HasSuffix(cfg.Encoder, "_helper"):
		return fmt.Sprintf("host.json forces the FFmpeg encoder %s", cfg.Encoder)
	case c == nil:
		return ""
	}
	has := func(m string) bool { return slices.Contains(c.Capture, m) }
	lacks := func(m string) string {
		why := c.Unavailable[m]
		if why == "" {
			why = "not listed in its caps"
		}
		return fmt.Sprintf("the helper cannot capture with %s (%s)", m, why)
	}
	test := cfg.Capture == "test"
	switch {
	case test:
		return "" // the helper's synthetic source: no cursor, no capture method to probe
	case drawCursor && !c.CursorInVideo:
		return "the video must carry the cursor, which the helper's captures leave out"
	case (prefs.Window != "" || cfg.Capture == "gfxcapture") && !has("wgc"):
		return lacks("wgc")
	case prefs.Window == "" && cfg.Capture == "amf" && !has("amd-direct"):
		return lacks("amd-direct")
	case prefs.Window == "" && cfg.Capture != "amf" && cfg.Capture != "gfxcapture" && !has("dda"):
		return lacks("dda")
	}
	return ""
}

// helperSource points p (from sessionParams) at what the native helper
// captures for monitor mon: a window (WGC) when the client asks for one, AMD
// Direct Capture or WGC when the host config asks for them, the helper's
// synthetic GPU source with the frame barcode for the test pattern, else DDA.
// A monitor is scaled to the largest even size within the client's that has
// the monitor's aspect ratio (the helper scales any capture, FFmpeg needs
// gfxcapture to scale; the helper stretches the source to the size it is
// given, gfxcapture's scale_aspect letterboxes); a window is encoded at its
// own size, whose aspect ratio is not known here.
func (s *Session) helperSource(p *media.Params, prefs proto.Prefs, mon platform.Monitor) {
	if p.Source.Backend == "test" {
		p.Barcode = true // the helper draws it (GUIDE 0.2's format)
		return
	}
	w, h := prefs.Width, prefs.Height
	if w > 0 && h > 0 && (w >= mon.W && h >= mon.H) {
		w, h = 0, 0 // never upscale
	}
	out := mon.DXGIOutput
	if out < 0 {
		out = mon.Index
	}
	src := media.Source{Backend: "ddagrab", Output: out, HMonitor: mon.HMonitor, NativeW: mon.W, NativeH: mon.H}
	switch {
	case prefs.Window != "":
		src = media.Source{Backend: "gfxcapture", HMonitor: mon.HMonitor, Window: prefs.Window, NativeW: mon.W, NativeH: mon.H}
		w, h = 0, 0
	case s.a.cfg.Capture == "gfxcapture":
		src.Backend = "gfxcapture"
	case s.a.cfg.Capture == "amf":
		src.Backend = "amf"
	}
	if w > 0 && h > 0 && mon.W > 0 && mon.H > 0 {
		w, h = media.FitAspect(w, h, mon.W, mon.H)
	}
	if w&^1 == 0 || h&^1 == 0 {
		w, h = 0, 0 // 0 x 0: the capture size (one 0 would keep that dimension only)
	}
	p.Source = src
	p.Width, p.Height = w&^1, h&^1
	p.DrawCursor = false // helperBlocker: the video never needs it here
}

func encoderNames(encs []media.EncoderInfo) string {
	names := make([]string, len(encs))
	for i, e := range encs {
		names[i] = e.Name
	}
	return strings.Join(names, ",")
}

func hagsText(h *bool) string {
	switch {
	case h == nil:
		return "unknown"
	case *h:
		return "on"
	}
	return "off"
}
