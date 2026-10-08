package codec

// Non-reference frames (Phase 5 temporal SVC thinning): a frame that no later
// frame references can be left out under congestion without breaking the
// decoding of any other frame. The native helper reports it per frame
// (encoder.Frame.Discardable); for the FFmpeg path the host reads it from the
// bitstream here. SVT-AV1's low-delay prediction structure (pred-struct=1,
// the test pattern's software AV1 encoder) codes every second frame with
// refresh_frame_flags 0; hardware encoders behind FFmpeg's command line code
// none (no temporal layers configured), and x264 with -tune zerolatency
// neither.

// av1FrameHeader, av1Frame: OBU types that carry an uncompressed frame header.
const (
	av1FrameHeader = 3
	av1Frame       = 6
)

// Discardable reports whether no later frame references the access unit /
// temporal unit data (never a key frame): H.264 a picture whose slices have
// nal_ref_idc 0; AV1 a temporal unit whose every frame header is a shown
// frame that refreshes no reference slot (refresh_frame_flags 0). HEVC and
// anything that cannot be parsed: false (a sub-layer non-reference picture is
// only discardable at the highest temporal layer, which the SPS would have to
// tell). AV1 needs the stream's sequence header (extradata or a key frame
// seen before).
func (p *Params) Discardable(data []byte, key bool) bool {
	if key {
		return false
	}
	switch p.Family {
	case H264:
		return h264NonReference(data)
	case AV1:
		return p.av1Seq != nil && av1Discardable(p.av1Seq.hdr, data)
	}
	return false
}

// h264NonReference reports whether the first VCL NAL unit of an Annex-B
// access unit (a slice: nal_unit_type 1-5) has nal_ref_idc 0; every slice of
// a picture has the same (7.4.1). An IDR (type 5) never qualifies. Only the
// NAL units before the first slice are scanned (delimiter, parameter sets,
// SEI).
func h264NonReference(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		h := b[i+3]
		switch typ := h & 0x1f; {
		case typ == 5:
			return false
		case typ >= 1 && typ <= 4:
			return h&0x60 == 0
		}
		i += 3
	}
	return false
}

// av1Discardable reports whether a temporal unit holds at least one frame
// header and every one is a shown, non-key frame with refresh_frame_flags 0
// (show_existing_frame headers do not qualify: showing a key frame refreshes
// every slot). Parse errors: false.
func av1Discardable(hi av1HeaderInfo, b []byte) bool {
	frames := 0
	for len(b) > 0 {
		h := b[0]
		if h&0x80 != 0 {
			return false
		}
		typ := int(h>>3) & 0xf
		var temporalID, spatialID uint32
		hl := 1
		if h&4 != 0 { // obu_extension_flag
			if len(b) < 2 {
				return false
			}
			temporalID, spatialID = uint32(b[1]>>5), uint32(b[1]>>3)&3
			hl = 2
		}
		size := uint64(len(b) - hl)
		if h&2 != 0 { // obu_has_size_field
			v, n, err := leb128(b[hl:])
			if err != nil {
				return false
			}
			size, hl = v, hl+n
		}
		if uint64(len(b)-hl) < size {
			return false
		}
		if typ == av1FrameHeader || typ == av1Frame {
			refresh, ok := av1RefreshFlags(hi, b[hl:hl+int(size)], temporalID, spatialID)
			if !ok || refresh != 0 {
				return false
			}
			frames++
		}
		b = b[hl+int(size):]
	}
	return frames > 0
}

// av1RefreshFlags parses an uncompressed frame header (AV1 5.9.2) up to
// refresh_frame_flags and returns them; ok is false for a header that is not
// a shown, non-key frame without show_existing_frame (or does not parse).
func av1RefreshFlags(hi av1HeaderInfo, hdr []byte, temporalID, spatialID uint32) (uint32, bool) {
	const keyFrame, intraOnly, switchFrame = 0, 2, 3
	if hi.reduced {
		return 0, false // every frame is a shown key frame
	}
	br := &bitReader{b: hdr}
	if br.u(1) == 1 { // show_existing_frame
		return 0, false
	}
	frameType := br.u(2)
	showFrame := br.u(1)
	if frameType == keyFrame || showFrame == 0 {
		return 0, false
	}
	if hi.decoderModel && !hi.equalPictureInterval {
		br.u(hi.presentationTimeLen) // temporal_point_info: frame_presentation_time
	}
	errorResilient := uint32(1)
	if frameType != switchFrame {
		errorResilient = br.u(1)
	}
	br.u(1) // disable_cdf_update
	sct := hi.forceSCT
	if sct == av1Select {
		sct = br.u(1) // allow_screen_content_tools
	}
	if sct == 1 && hi.forceIntegerMV == av1Select {
		br.u(1) // force_integer_mv
	}
	if hi.frameIDLen > 0 {
		br.u(hi.frameIDLen) // current_frame_id
	}
	if frameType != switchFrame {
		br.u(1) // frame_size_override_flag
	}
	br.u(hi.orderHintBits) // order_hint
	if frameType != intraOnly && errorResilient == 0 {
		br.u(3) // primary_ref_frame
	}
	if hi.decoderModel && br.u(1) == 1 { // buffer_removal_time_present_flag
		for op, idc := range hi.opIdc {
			if op < len(hi.opDecoderModel) && hi.opDecoderModel[op] {
				inTemporal, inSpatial := (idc>>temporalID)&1 == 1, (idc>>(spatialID+8))&1 == 1
				if idc == 0 || inTemporal && inSpatial {
					br.u(hi.removalTimeLen) // buffer_removal_time
				}
			}
		}
	}
	refresh := uint32(0xff) // switch frames refresh every slot
	if frameType != switchFrame {
		refresh = br.u(8)
	}
	if br.err {
		return 0, false
	}
	return refresh, true
}
