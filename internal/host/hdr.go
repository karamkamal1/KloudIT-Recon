package host

import (
	"fmt"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// HDR10 negotiation (GUIDE 3.9 / 4.5), opt-in at both ends and never on by
// accident. A generation is HDR10 only when all of these hold (decideHDR),
// else it is SDR exactly as before:
//
//   - the host config allows it ("hdr": "auto"; default "off");
//   - the codec is HEVC (Main 10) or AV1 (10-bit): the negotiated codec is
//     not changed for HDR, an H.264 stream stays SDR;
//   - the pipeline can make it (hdrPipeline): the native helper with a codec
//     whose caps have hdr10 and a capture with an HDR path (DDA, AMD Direct
//     Capture; not WGC), which then streams HDR10 when the captured output
//     is in Windows HDR mode, else SDR with a note; or on the FFmpeg path
//     the test pattern with libsvtav1 (media.HDRTestGraph). FFmpeg's Windows
//     captures stay SDR (media/hdr.go says why);
//   - the client asks for it and can present it (hello / settings prefs.hdr:
//     a WebGPU canvas that confirmed extended range, a 10-bit decoder for the
//     codec family whose frames it can draw as HDR, its HDR setting Auto, an
//     HDR display).
//
// The first that fails is the reason, in this order: what stays the same for
// the session before what can change (the client's setting and display), so
// that a stream that cannot be HDR keeps its reason when they change.
//
// The decision goes into the generation's Params (HDR, HDRNote), the
// pipelines announce the result in the VideoConfig (hdr, bitDepth,
// colorSpace, hdrMetadata, and hdrNote: why not, for clients that sent
// prefs.hdr). A change of the client's HDR prefs alone (the setting, the
// display, a decoder the client withdrew) restarts the video only when it
// changes the current generation's decision (hdrRestart); Windows HDR turned
// on or off under an HDR10 helper stream (captureChanged "hdr") restarts it
// too: a new generation with a new video config.

// hdrFamilies are the codec families with an HDR10 profile here.
var hdrFamilies = map[string]bool{"hevc": true, "av1": true}

// decideHDR decides whether a generation of codec family is HDR10, from the
// host config's "hdr" (config), the client's HDR prefs (nil: a client before
// HDR) and why the pipeline cannot make HDR10 ("" if it can). It returns the
// decision and why not (for the client and the log; "" when HDR, and for
// clients before HDR).
func decideHDR(config string, client *proto.HDRPrefs, family, pipeline string) (bool, string) {
	switch {
	case client == nil:
		return false, ""
	case config != proto.HDRAuto:
		return false, `HDR is off in the host config ("hdr")`
	case !hdrFamilies[family]:
		return false, fmt.Sprintf("%s streams are SDR (HDR10 needs HEVC Main 10 or AV1 10-bit)", familyNames[family])
	case pipeline != "":
		return false, pipeline
	}
	return client.CanPresent(family)
}

// hdrPipeline returns why the pipeline of p (its encoder and source) cannot
// make an HDR10 stream, or "" if it can: the native helper with a codec whose
// caps have hdr10 (helper: its caps) and a capture with an HDR path (its
// Windows Graphics Capture, for a window or host capture "gfxcapture", has
// none: docs/HELPER_PROTOCOL.md "HDR10"), FFmpeg only for the test pattern
// with libsvtav1 when the probe ran it.
func (a *Agent) hdrPipeline(p media.Params, helper encoder.Caps) string {
	e := p.Encoder
	switch {
	case e.Helper && !helper.Codecs[e.Family].HDR10:
		return fmt.Sprintf("the native encoder cannot make %s HDR10 on this GPU (caps hdr10 false)", familyNames[e.Family])
	case e.Helper && p.Source.Backend == "gfxcapture":
		return media.HelperWGCNoHDR
	case e.Helper:
		return ""
	case p.Source.Backend != "test":
		return "FFmpeg's screen capture makes no HDR10 (HDR needs the native encoder helper)"
	case e.Name != media.HDRTestEncoder:
		return fmt.Sprintf("the HDR test pattern is encoded with %s only, not %s", media.HDRTestEncoder, e.Name)
	case !a.caps.CanHDRTest():
		return "this FFmpeg build cannot make the HDR test pattern (see the host log's probe)"
	}
	return ""
}

// chooseHDR decides HDR10 for the generation p (encoder and source chosen)
// with the client's prefs, sets p.HDR and p.HDRNote, and logs a new decision
// once.
func (s *Session) chooseHDR(p *media.Params, prefs proto.Prefs) {
	_, _, helper := s.onHelper()
	p.HDR, p.HDRNote = decideHDR(s.a.cfg.hdr(), prefs.HDR, p.Encoder.Family, s.a.hdrPipeline(*p, helper))
	if prefs.HDR == nil {
		return
	}
	choice := fmt.Sprintf("%v %s %s", p.HDR, p.Encoder.Name, p.HDRNote)
	s.prefsMu.Lock()
	repeat := choice == s.hdrChoice
	s.hdrChoice = choice
	s.prefsMu.Unlock()
	if !repeat {
		h := prefs.HDR
		s.log.Info("hdr choice", "hdr", p.HDR, "encoder", p.Encoder.Name, "reason", p.HDRNote, "config", s.a.cfg.hdr(),
			"client", fmt.Sprintf("mode=%s display=%v canvas=%v decoders=%v", h.Mode, h.Display, h.Canvas, h.Decoders), "client_why", h.Why)
	}
}

// hdrRestart reports whether a settings message that changes only the
// client's HDR prefs (prefs: the new ones) changes the HDR decision of the
// current generation (HDR10 or not, or why not): only then does the video
// restart. Mostly it does not: the window moved between an HDR and an SDR
// monitor under a stream that stays SDR anyway (host config off, H.264, a
// pipeline or a client that cannot make or show HDR10). With nothing
// streaming (paused, stopped) the next start decides with the new prefs.
func (s *Session) hdrRestart(prefs proto.Prefs) bool {
	p, ok := s.vid().Current()
	if !ok {
		return false
	}
	_, _, helper := s.onHelper()
	hdr, note := decideHDR(s.a.cfg.hdr(), prefs.HDR, p.Encoder.Family, s.a.hdrPipeline(p, helper))
	return hdr != p.HDR || note != p.HDRNote
}
