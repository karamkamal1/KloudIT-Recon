package host

import (
	"fmt"
	"log/slog"
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

// setupHelper finds the native encoder helper next to the running executable
// (Windows, host config "pipeline" auto or helper) and logs what it found.
func (a *Agent) setupHelper() {
	switch {
	case a.cfg.pipeline() == media.PipelineFFmpeg:
		a.helperMissing = `host config "pipeline" is "ffmpeg"`
	case runtime.GOOS != "windows":
		a.helperMissing = "the native encoder helper is Windows-only"
	default:
		exe, err := os.Executable()
		if err == nil {
			exe = filepath.Join(filepath.Dir(exe), helperExeName)
			_, err = os.Stat(exe)
		}
		if err != nil {
			a.helperMissing = helperExeName + " is not installed next to recon-host: " + err.Error()
			break
		}
		a.launchHelper = func(log *slog.Logger) (*encoder.Helper, error) {
			return encoder.Launch(encoder.Options{Exe: exe, Log: log, CapsTimeout: 5 * time.Second})
		}
		a.log.Info("native encoder helper installed", "path", exe, "pipeline", a.cfg.pipeline())
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

// openPipeline decides the session's video pipeline, once: the native helper
// when the host config allows it ("pipeline" auto or helper), it is installed,
// starts and can encode, the codec negotiated with this browser is one of its
// codecs and the session needs nothing only FFmpeg offers (helperBlocker);
// else FFmpeg. It logs the decision and why, and returns a notice for the
// user when "pipeline" "helper" could not be honoured. Later the session moves
// to FFmpeg for good when it needs FFmpeg after all (leaveHelper).
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
	if why == "" {
		var err error
		if h, err = s.a.launchHelper(s.log); err != nil {
			why = "it did not start: " + err.Error()
		} else if c := h.Caps(); !c.Usable() {
			why = "it has no usable encoder (" + unavailableText(c) + ")"
		} else if why = s.helperBlocker(prefs, drawCursor, &c); why == "" {
			// The codec this browser and the host agree on must be one of
			// the helper's (its encoders come first in the negotiation).
			s.pipeMu.Lock()
			s.helperEncs, s.helperCaps = media.HelperEncoders(c), c
			s.pipeMu.Unlock()
			if e, err := s.negotiateEncoder(prefs, false); err != nil || !e.Helper {
				why = fmt.Sprintf("the codec negotiated with this browser (%s) is not one of the helper's (%s)", e.Name, encoderNames(s.helperEncs))
			}
		}
		if why != "" && h != nil {
			go h.Close()
			h = nil
		}
	}
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()
	if h == nil {
		s.helperEncs, s.helperCaps = nil, encoder.Caps{}
		s.video = media.NewVideo(s.a.caps, s.log, s.a.clock)
		s.log.Info("video pipeline", "pipeline", media.PipelineFFmpeg, "config", mode, "reason", why)
		if mode == media.PipelineHelper {
			return "The native encoder is not used (" + why + "); streaming with FFmpeg."
		}
		return ""
	}
	c := h.Caps()
	lb := s.a.liveBitrateResults(s.log, c)
	s.video = media.NewHelperVideo(media.HelperOptions{
		Launch: func() (*encoder.Helper, error) { return s.a.launchHelper(s.log) },
		First:  h, Log: s.log, Clock: s.a.hostClock, KeepSpare: true,
		LiveBitrate: func(c encoder.Caps, sp encoder.StartParams, adaptive bool) (string, string, bool) {
			return lb.Choose(c, sp, adaptive)
		},
	})
	s.log.Info("video pipeline", "pipeline", media.PipelineHelper, "config", mode, "backend", c.Backend, "vendor", c.Vendor,
		"adapter", c.AdapterName, "encoders", encoderNames(s.helperEncs), "capture", strings.Join(c.Capture, ","),
		"hags", hagsText(c.HAGSEnabled))
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

func unavailableText(c encoder.Caps) string {
	keys := make([]string, 0, len(c.Unavailable))
	for k := range c.Unavailable {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+c.Unavailable[k])
	}
	return strings.Join(parts, "; ")
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
