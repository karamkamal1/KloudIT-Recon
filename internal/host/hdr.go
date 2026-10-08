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
//   - the client asks for it and can present it (hello / settings prefs.hdr:
//     its HDR setting Auto, an HDR display, a WebGPU canvas that confirmed
//     extended range, a 10-bit decoder for the codec family);
//   - the codec is HEVC (Main 10) or AV1 (10-bit): the negotiated codec is
//     not changed for HDR, an H.264 stream stays SDR;
//   - the pipeline can make it (hdrPipeline): the native helper with a codec
//     whose caps have hdr10 (it then streams HDR10 when the captured output
//     is in Windows HDR mode, else SDR with a note), or on the FFmpeg path
//     the test pattern with libsvtav1 (media.HDRTestGraph). FFmpeg's Windows
//     captures stay SDR (media/hdr.go says why).
//
// The decision goes into the generation's Params (HDR, HDRNote), the
// pipelines announce the result in the VideoConfig (hdr, bitDepth,
// colorSpace, hdrMetadata, and hdrNote: why not, for clients that sent
// prefs.hdr). A change of the client's HDR prefs (the setting, the display)
// restarts the video like any video setting, and so does Windows HDR turned
// on or off under an HDR10 helper stream (captureChanged "hdr"): a new
// generation with a new video config.

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
	}
	if ok, why := client.CanPresent(family); !ok {
		return false, why
	}
	if pipeline != "" {
		return false, pipeline
	}
	return true, ""
}

// hdrPipeline returns why the pipeline of p (its encoder and source) cannot
// make an HDR10 stream, or "" if it can: the native helper with a codec whose
// caps have hdr10 (helper: its caps), FFmpeg only for the test pattern with
// libsvtav1 when the probe ran it.
func (a *Agent) hdrPipeline(p media.Params, helper encoder.Caps) string {
	e := p.Encoder
	switch {
	case e.Helper:
		if !helper.Codecs[e.Family].HDR10 {
			return fmt.Sprintf("the native encoder cannot make %s HDR10 on this GPU (caps hdr10 false)", familyNames[e.Family])
		}
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
