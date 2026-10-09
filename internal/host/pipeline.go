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
		ffDir, refused := a.cfg.helperFFmpegDir(dir)
		if refused != nil {
			a.log.Warn(`host config "helperFFmpegDir" ignored`, "err", refused, "using", ffDir)
		}
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
		// Phase 5 encoder options (HelperVideo.withCaps decides from the
		// caps and logs each decision once).
		EncoderInstance: string(s.a.cfg.EncoderInstance), ReencodeOversized: s.a.cfg.ReencodeOversized,
		SliceOutput: s.a.cfg.SliceOutput,
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
// helper's backends (backend -> why; "auto": the helper's own choice, when
// it did not start), for the "video pipeline" log line.
type helperSkips map[string]string

// String lists them in the selection order ("auto: ...; amf: ...; nvenc:
// ...").
func (k helperSkips) String() string {
	var parts []string
	for _, b := range append([]string{"auto"}, helperBackends...) {
		if why, ok := k[b]; ok {
			parts = append(parts, b+": "+why)
		}
	}
	for _, b := range slices.Sorted(maps.Keys(k)) {
		if b != "auto" && !slices.Contains(helperBackends, b) {
			parts = append(parts, b+": "+k[b])
		}
	}
	return strings.Join(parts, "; ")
}

// chooseHelper launches the native helper and returns it when it can serve
// the session (helperFits), else nil and why not. The first launch lets the
// helper choose its backend ("auto": the primary display adapter's vendor
// encoder first, libavcodec last, or first on an Intel primary adapter, whose
// outputs AMF and NVENC cannot encode), except that a helper encoder forced
// in host.json (<codec>_<backend>_helper) launches its backend first, and
// that with the libavcodec backend off the vendor backends are launched by
// name (so the helper never chooses that backend or opens its Quick Sync
// encoders). When a backend cannot serve the session for a reason of its own
// (it is not usable, its encoder runs on another vendor's GPU than the
// monitor, the libavcodec backend is off, the codec negotiated with this
// browser is not one of its codecs) or the helper did not start, the next
// backend in the selection order (helperBackends) no launch reported
// unavailable is launched instead; after a failed "auto" launch only the
// vendor backends (the libavcodec backend's probe may be what failed), and a
// second failed start ends the selection. skipped gets why each backend
// before the one chosen (all of them, when none is, except the last one
// tried: its reason is returned) was passed over.
func (s *Session) chooseHelper(prefs proto.Prefs, drawCursor bool, skipped helperSkips) (*encoder.Helper, string) {
	cfg := s.a.cfg
	mon, _ := s.captureMonitor(prefs) // the session's virtual display, if any
	tried := map[string]bool{}
	unavailable := map[string]string{} // over every launch
	choice := ""                       // why the first launch was not of an earlier backend, for the log
	gotCaps := false                   // a launch reported which backends are usable
	autoFailed := false                // the "auto" launch did not start
	// lavcOut says why the libavcodec backend is not launched by name, or "".
	lavcOut := func() string {
		switch {
		case !cfg.libavcodecOn():
			return `off (host config "helperLibavcodec")`
		case s.a.lavcMissing != "":
			return "its FFmpeg libraries are not installed: " + s.a.lavcMissing
		case autoFailed:
			return "not launched: the helper did not start with its own choice of backend, which may have been this one"
		}
		return ""
	}
	next := func() string {
		for _, b := range helperBackends {
			if !tried[b] && unavailable[b] == "" && (b != backendLavc || lavcOut() == "") {
				return b
			}
		}
		return ""
	}
	// fill notes why the backends before index upTo of helperBackends were
	// not used where no launch said so: unavailable, turned off, not
	// installed, usable but not the first launch's choice, or not tried
	// (no launch reported caps).
	fill := func(upTo int, except string) {
		for _, b := range helperBackends[:upTo] {
			if _, done := skipped[b]; done || b == except {
				continue
			}
			switch {
			case b == backendLavc && lavcOut() != "":
				skipped[b] = lavcOut()
			case unavailable[b] != "":
				skipped[b] = unavailable[b]
			case tried[b]:
				skipped[b] = "not usable"
			case !gotCaps:
				skipped[b] = "not tried"
			case choice != "":
				skipped[b] = "usable, not tried (" + choice + ")"
			default:
				skipped[b] = "usable, not tried"
			}
		}
	}
	backend := "" // auto
	switch fb := forcedHelperBackend(cfg.Encoder); {
	case fb != "" && (fb != backendLavc || lavcOut() == ""):
		backend, choice = fb, "host.json forces "+cfg.Encoder
	case !cfg.libavcodecOn():
		backend = next()
	}
	failures := 0
	for {
		name := backend // what was launched, for skipped and the log
		if name == "" {
			name = "auto"
		}
		why, skip, backendOnly := "", "", true // skip: why, for skipped
		h, err := s.a.launchHelper(s.log, backend)
		if err != nil {
			failures++
			tried[backend] = true
			autoFailed = autoFailed || backend == ""
			why, skip = "it did not start: "+err.Error(), "it did not start: "+err.Error()
			if backend != "" {
				why = fmt.Sprintf("it did not start with backend %s: %v", backend, err)
			}
			backendOnly = failures < 2 // twice: the helper itself fails
		} else {
			c := h.Caps()
			tried[backend], tried[c.Backend], gotCaps = true, true, true
			for k, v := range c.Unavailable {
				if unavailable[k] == "" {
					unavailable[k] = v
				}
			}
			if backend == "" && choice == "" {
				choice = "the helper chose " + c.Backend
				if c.AdapterName != "" {
					choice += " for " + c.AdapterName
				}
			}
			why, backendOnly = s.helperFits(prefs, drawCursor, mon, &c)
			if why == "" {
				if i := slices.Index(helperBackends, c.Backend); i > 0 {
					fill(i, "")
				}
				s.forcedHelperEncoder(c)
				return h, ""
			}
			go h.Close()
			switch {
			case !c.Usable() && backend == "":
				fill(len(helperBackends), "")
				return nil, why // no backend in the helper's own order is usable
			case !c.Usable():
				skip = c.Unavailable[backend]
				if skip == "" {
					skip = "no encoder"
				}
				why, backendOnly = fmt.Sprintf("its %s backend is not usable (%s)", backend, skip), true
			case !backendOnly:
				return nil, why // the session needs something no backend changes
			default:
				name, skip = c.Backend, why
			}
		}
		nb := ""
		if backendOnly {
			nb = next()
		}
		if nb == "" {
			fill(len(helperBackends), name)
			return nil, why
		}
		skipped[name] = skip
		s.log.Info("native encoder helper: trying another backend", "backend", nb, "instead_of", name, "reason", why)
		backend = nb
	}
}

// forcedHelperBackend returns the helper backend of a helper encoder forced
// in host.json ("encoder" <codec>_<backend>_helper, e.g. hevc_nvenc_helper),
// or "".
func forcedHelperBackend(enc string) string {
	name, ok := strings.CutSuffix(enc, "_helper")
	if !ok {
		return ""
	}
	if _, b, ok := strings.Cut(name, "_"); ok && slices.Contains(helperBackends, b) {
		return b
	}
	return ""
}

// forcedHelperEncoder logs, once per session, that the helper encoder forced
// in host.json is not one of the chosen helper's (caps c): the codec is then
// chosen automatically among them (negotiateEncoder).
func (s *Session) forcedHelperEncoder(c encoder.Caps) {
	enc := s.a.cfg.Encoder
	if !strings.HasSuffix(enc, "_helper") {
		return
	}
	encs := media.HelperEncoders(c)
	if slices.ContainsFunc(encs, func(e media.EncoderInfo) bool { return e.Name == enc }) {
		return
	}
	s.log.Info("host config encoder not used", "encoder", enc,
		"reason", fmt.Sprintf("not one of the encoders of the helper backend chosen (%s: %s); choosing automatically", c.Backend, encoderNames(encs)))
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
	p := s.a.sessionParams(prefs, mon, s.captureBackend(prefs, mon, s.onVirtualDisplay()), s.hello.V >= proto.HelloVersionFrameExt)
	w, h := p.OutputSize()
	if e, _, err := s.negotiateEncoder(prefs, w, h, false); err != nil || !e.Helper {
		return fmt.Sprintf("the codec negotiated with this browser (%s) is not one of the helper's (%s)", e.Name, encoderNames(encs)), true
	}
	return "", false
}

// adapterBlocker returns why the helper with caps c cannot encode monitor
// mon, or "". The helper captures on the output's own GPU (DXGI Desktop
// Duplication, AMD Direct Capture and its WGC device alike) and every backend
// encodes on that capture's device, taking only GPUs of its own vendor
// (caps.vendor; the helper checks the same at start): a monitor whose output
// (caps.outputs, by HMONITOR) is on another vendor's GPU (a hybrid laptop's
// external port on the discrete GPU, a desktop with monitors on an iGPU and
// a dGPU) needs another backend. A second GPU of the same vendor is the same
// backend's (caps.adapterLuid is only the GPU its probe read the caps on;
// should that GPU lack a codec of the caps, the helper refuses the start and
// HelperVideo's failure fallback applies). A window capture (its monitor is
// not known here) and the test source are not checked, nor a monitor the
// caps do not list.
func (s *Session) adapterBlocker(prefs proto.Prefs, mon platform.Monitor, c *encoder.Caps) string {
	if prefs.Window != "" || s.a.cfg.Capture == "test" || mon.HMonitor == 0 || c.Vendor == "" {
		return ""
	}
	for _, o := range c.Outputs {
		if o.HMonitor == mon.HMonitor && o.Vendor != "" && !strings.EqualFold(o.Vendor, c.Vendor) {
			return fmt.Sprintf("its %s encoder runs on %s GPUs (%s), the monitor (%s) is on %s", c.Backend, c.Vendor, c.AdapterName, o.Name, o.AdapterName)
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
// Direct Capture, DDA; the session's capture: DDA for a virtual display).
func (s *Session) helperBlocker(prefs proto.Prefs, drawCursor bool, c *encoder.Caps) string {
	cfg := s.a.cfg
	capture := s.capture()
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
	case (prefs.Window != "" || capture == "gfxcapture") && !has("wgc"):
		return lacks("wgc")
	case prefs.Window == "" && capture == "amf" && !has("amd-direct"):
		return lacks("amd-direct")
	case prefs.Window == "" && capture != "amf" && capture != "gfxcapture" && !has("dda"):
		return lacks("dda")
	}
	return ""
}

// helperSource points p (from sessionParams) at what the native helper
// captures for monitor mon: a window (WGC) when the client asks for one, AMD
// Direct Capture or WGC when the host config asks for them (not AMD Direct
// Capture for a virtual display: Session.capture), the helper's synthetic GPU
// source with the frame barcode for the test pattern, else DDA.
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
	case s.capture() == "gfxcapture":
		src.Backend = "gfxcapture"
	case s.capture() == "amf":
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
