// Package codec contains the small amount of bitstream knowledge the host needs:
// deriving exact WebCodecs codec strings from parameter sets and guaranteeing
// that every key frame is independently decodable (parameter sets in-band).
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Families.
const (
	H264 = "h264"
	HEVC = "hevc"
	AV1  = "av1"
)

// FamilyFromFourCC maps a NUT/ffmpeg fourcc to a family.
func FamilyFromFourCC(fcc string) string {
	switch strings.ToUpper(fcc) {
	case "H264", "AVC1":
		return H264
	case "HEVC", "HEV1", "HVC1", "H265":
		return HEVC
	case "AV01":
		return AV1
	}
	return ""
}

// ---------------------------------------------------------------------------
// Annex B

// SplitAnnexB returns the NAL units (without start codes) in an Annex-B buffer.
func SplitAnnexB(b []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && b[end-1] == 0 { // strip trailing zero (4-byte start code / trailing_zero_8bits)
					end--
				}
				nals = append(nals, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		nals = append(nals, b[start:])
	}
	return nals
}

var startCode = []byte{0, 0, 0, 1}

// JoinAnnexB concatenates NAL units with 4-byte start codes.
func JoinAnnexB(nals ...[]byte) []byte {
	var out bytes.Buffer
	for _, n := range nals {
		out.Write(startCode)
		out.Write(n)
	}
	return out.Bytes()
}

func isAnnexB(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0, 0, 1}) || bytes.HasPrefix(b, []byte{0, 0, 0, 1})
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 -> 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// H.264

func h264Type(n []byte) byte { return n[0] & 0x1f }

// H264CodecString returns "avc1.PPCCLL" from an SPS NAL unit.
func H264CodecString(sps []byte) (string, error) {
	if len(sps) < 4 || h264Type(sps) != 7 {
		return "", errors.New("codec: not an H.264 SPS")
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", sps[1], sps[2], sps[3]), nil
}

// ---------------------------------------------------------------------------
// HEVC

func hevcType(n []byte) byte { return (n[0] >> 1) & 0x3f }

// HEVCCodecString derives "hev1.P.C.TL.CC..." (ISO/IEC 14496-15 Annex E) from an SPS NAL unit.
func HEVCCodecString(sps []byte) (string, error) {
	if len(sps) < 3 || hevcType(sps) != 33 {
		return "", errors.New("codec: not an HEVC SPS")
	}
	rb := unescapeRBSP(sps[2:]) // skip 2-byte NAL header
	br := &bitReader{b: rb}
	br.u(4) // sps_video_parameter_set_id
	br.u(3) // sps_max_sub_layers_minus1
	br.u(1) // sps_temporal_id_nesting_flag
	space := br.u(2)
	tier := br.u(1)
	profile := br.u(5)
	var compat uint32
	for j := 0; j < 32; j++ {
		if br.u(1) == 1 {
			compat |= 1 << uint(j) // reversed bit order, as the codec string requires
		}
	}
	var cons [6]byte
	for i := range cons {
		cons[i] = byte(br.u(8))
	}
	level := br.u(8)
	if br.err {
		return "", errors.New("codec: truncated HEVC SPS")
	}
	var sb strings.Builder
	sb.WriteString("hev1.")
	if space > 0 {
		sb.WriteByte(byte('A' + space - 1))
	}
	fmt.Fprintf(&sb, "%d.%X.", profile, compat)
	if tier == 1 {
		sb.WriteByte('H')
	} else {
		sb.WriteByte('L')
	}
	fmt.Fprintf(&sb, "%d", level)
	last := -1
	for i, c := range cons {
		if c != 0 {
			last = i
		}
	}
	for i := 0; i <= last; i++ {
		fmt.Fprintf(&sb, ".%X", cons[i])
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// AV1

const (
	obuSequenceHeader    = 1
	obuTemporalDelimiter = 2
)

type obu struct {
	typ  int
	raw  []byte // complete OBU including header
	body []byte
}

func leb128(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("codec: bad leb128")
}

// SplitOBUs parses a low-overhead (Section 5) AV1 bitstream.
func SplitOBUs(b []byte) ([]obu, error) {
	var out []obu
	for len(b) > 0 {
		h := b[0]
		if h&0x80 != 0 {
			return nil, errors.New("codec: forbidden bit set")
		}
		typ := int(h>>3) & 0xf
		ext := h&4 != 0
		hasSize := h&2 != 0
		hl := 1
		if ext {
			hl = 2
		}
		if len(b) < hl {
			return nil, errors.New("codec: truncated OBU header")
		}
		var size uint64
		if hasSize {
			v, n, err := leb128(b[hl:])
			if err != nil {
				return nil, err
			}
			size = v
			hl += n
		} else {
			size = uint64(len(b) - hl)
		}
		if uint64(len(b)-hl) < size {
			return nil, errors.New("codec: truncated OBU")
		}
		end := hl + int(size)
		out = append(out, obu{typ: typ, raw: b[:end], body: b[hl:end]})
		b = b[end:]
	}
	return out, nil
}

// AV1SequenceHeader holds the sequence header fields the host uses.
type AV1SequenceHeader struct {
	Profile, Level, Tier, BitDepth int
	// MaxWidth and MaxHeight are max_frame_width/height_minus_1 + 1: the
	// size of every frame unless its header overrides it
	// (frame_size_override_flag), which streaming encoders do not do. AV1
	// has no cropping window: an encoder that codes in blocks (RDNA3: 64x16)
	// writes the padded size here and decoders output the padding rows.
	MaxWidth, MaxHeight int
}

// ParseAV1SequenceHeader parses a sequence header OBU payload up to its
// color_config bit depth.
func ParseAV1SequenceHeader(seqHdr []byte) (AV1SequenceHeader, error) {
	var h AV1SequenceHeader
	br := &bitReader{b: seqHdr}
	profile := br.u(3)
	br.u(1) // still_picture
	reduced := br.u(1)
	var level, tier uint32
	if reduced == 1 {
		level = br.u(5)
	} else {
		timingInfo := br.u(1)
		decoderModel := uint32(0)
		bufferDelayLen := uint32(0)
		if timingInfo == 1 {
			br.u(32)          // num_units_in_display_tick
			br.u(32)          // time_scale
			if br.u(1) == 1 { // equal_picture_interval
				br.uvlc()
			}
			decoderModel = br.u(1)
			if decoderModel == 1 {
				bufferDelayLen = br.u(5) + 1
				br.u(32) // num_units_in_decoding_tick
				br.u(5)  // buffer_removal_time_length_minus_1
				br.u(5)  // frame_presentation_time_length_minus_1
			}
		}
		initialDisplayDelay := br.u(1)
		opCount := br.u(5) + 1
		for i := uint32(0); i < opCount; i++ {
			br.u(12) // operating_point_idc
			l := br.u(5)
			t := uint32(0)
			if l > 7 {
				t = br.u(1)
			}
			if decoderModel == 1 {
				if br.u(1) == 1 {
					br.u(bufferDelayLen) // decoder_buffer_delay
					br.u(bufferDelayLen) // encoder_buffer_delay
					br.u(1)              // low_delay_mode_flag
				}
			}
			if initialDisplayDelay == 1 {
				if br.u(1) == 1 {
					br.u(4)
				}
			}
			if i == 0 {
				level, tier = l, t
			}
		}
	}
	wBits := br.u(4) + 1
	hBits := br.u(4) + 1
	h.MaxWidth = int(br.u(wBits)) + 1
	h.MaxHeight = int(br.u(hBits)) + 1
	// Skip to color_config to find the bit depth.
	frameIDs := uint32(0)
	if reduced == 0 {
		frameIDs = br.u(1)
	}
	if frameIDs == 1 {
		br.u(4)
		br.u(3)
	}
	br.u(1) // use_128x128_superblock
	br.u(1) // enable_filter_intra
	br.u(1) // enable_intra_edge_filter
	if reduced == 0 {
		br.u(1) // interintra
		br.u(1) // masked compound
		br.u(1) // warped motion
		br.u(1) // dual filter
		orderHint := br.u(1)
		if orderHint == 1 {
			br.u(1) // jnt_comp
			br.u(1) // ref_frame_mvs
		}
		forceSCT := uint32(2)
		if br.u(1) == 0 { // seq_choose_screen_content_tools
			forceSCT = br.u(1)
		}
		if forceSCT > 0 {
			if br.u(1) == 0 { // seq_choose_integer_mv
				br.u(1)
			}
		}
		if orderHint == 1 {
			br.u(3)
		}
	}
	br.u(1) // enable_superres
	br.u(1) // enable_cdef
	br.u(1) // enable_restoration
	highBitDepth := br.u(1)
	depth := 8
	if profile == 2 && highBitDepth == 1 {
		if br.u(1) == 1 {
			depth = 12
		} else {
			depth = 10
		}
	} else if highBitDepth == 1 {
		depth = 10
	}
	if br.err {
		return h, errors.New("codec: truncated AV1 sequence header")
	}
	h.Profile, h.Level, h.Tier, h.BitDepth = int(profile), int(level), int(tier), depth
	return h, nil
}

// CodecString returns the WebCodecs codec string "av01.P.LLT.DD".
func (h AV1SequenceHeader) CodecString() string {
	t := 'M'
	if h.Tier == 1 {
		t = 'H'
	}
	return fmt.Sprintf("av01.%d.%02d%c.%02d", h.Profile, h.Level, t, h.BitDepth)
}

// AV1CodecString derives "av01.P.LLT.DD" from a sequence header OBU payload.
func AV1CodecString(seqHdr []byte) (string, error) {
	h, err := ParseAV1SequenceHeader(seqHdr)
	if err != nil {
		return "", err
	}
	return h.CodecString(), nil
}

// AV1FrameSize returns the frame size of the first sequence header in a
// low-overhead AV1 bitstream (a temporal unit, or sequence header OBUs as
// extradata).
func AV1FrameSize(data []byte) (w, h int, ok bool) {
	obus, err := SplitOBUs(data)
	if err != nil {
		return 0, 0, false
	}
	for _, o := range obus {
		if o.typ == obuSequenceHeader {
			if sh, err := ParseAV1SequenceHeader(o.body); err == nil {
				return sh.MaxWidth, sh.MaxHeight, true
			}
		}
	}
	return 0, 0, false
}

// ---------------------------------------------------------------------------
// Parameter set handling

// Params collects the parameter sets of a stream and the derived codec string.
type Params struct {
	Family string
	Codec  string
	// CodedWidth and CodedHeight are the frame size of the AV1 sequence
	// header (0 for H.264/HEVC, whose decoders apply the SPS cropping
	// window themselves).
	CodedWidth, CodedHeight int
	sets                    []byte // Annex-B parameter sets (H.264/HEVC) or OBUs (AV1)
}

// NewParams seeds the parameter cache from container extradata (may be empty).
func NewParams(family string, extradata []byte) *Params {
	p := &Params{Family: family}
	if len(extradata) > 0 {
		p.sets = normalizeExtradata(family, extradata)
		p.updateCodec(p.sets)
	}
	return p
}

// normalizeExtradata converts avcC/hvcC/av1C extradata into in-band form.
func normalizeExtradata(family string, ex []byte) []byte {
	switch family {
	case H264:
		if isAnnexB(ex) {
			return ex
		}
		if nals, err := parseAVCC(ex); err == nil {
			return JoinAnnexB(nals...)
		}
	case HEVC:
		if isAnnexB(ex) {
			return ex
		}
		if nals, err := parseHVCC(ex); err == nil {
			return JoinAnnexB(nals...)
		}
	case AV1:
		if len(ex) >= 4 && ex[0] == 0x81 { // av1C
			return ex[4:]
		}
		return ex
	}
	return nil
}

func parseAVCC(b []byte) ([][]byte, error) {
	if len(b) < 7 || b[0] != 1 {
		return nil, errors.New("not avcC")
	}
	var nals [][]byte
	n := int(b[5] & 0x1f)
	p := 6
	for i := 0; i < n; i++ {
		if p+2 > len(b) {
			return nil, errors.New("truncated avcC")
		}
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			return nil, errors.New("truncated avcC")
		}
		nals = append(nals, b[p:p+l])
		p += l
	}
	if p >= len(b) {
		return nals, nil
	}
	n = int(b[p])
	p++
	for i := 0; i < n; i++ {
		if p+2 > len(b) {
			return nil, errors.New("truncated avcC")
		}
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			return nil, errors.New("truncated avcC")
		}
		nals = append(nals, b[p:p+l])
		p += l
	}
	return nals, nil
}

func parseHVCC(b []byte) ([][]byte, error) {
	if len(b) < 23 || b[0] != 1 {
		return nil, errors.New("not hvcC")
	}
	var nals [][]byte
	arrays := int(b[22])
	p := 23
	for a := 0; a < arrays; a++ {
		if p+3 > len(b) {
			return nil, errors.New("truncated hvcC")
		}
		n := int(binary.BigEndian.Uint16(b[p+1:]))
		p += 3
		for i := 0; i < n; i++ {
			if p+2 > len(b) {
				return nil, errors.New("truncated hvcC")
			}
			l := int(binary.BigEndian.Uint16(b[p:]))
			p += 2
			if p+l > len(b) {
				return nil, errors.New("truncated hvcC")
			}
			nals = append(nals, b[p:p+l])
			p += l
		}
	}
	return nals, nil
}

// updateCodec scans Annex-B / OBU data for an SPS / sequence header.
func (p *Params) updateCodec(data []byte) bool {
	switch p.Family {
	case H264:
		for _, n := range SplitAnnexB(data) {
			if len(n) > 0 && h264Type(n) == 7 {
				if s, err := H264CodecString(n); err == nil {
					p.Codec = s
					return true
				}
			}
		}
	case HEVC:
		for _, n := range SplitAnnexB(data) {
			if len(n) > 1 && hevcType(n) == 33 {
				if s, err := HEVCCodecString(n); err == nil {
					p.Codec = s
					return true
				}
			}
		}
	case AV1:
		obus, err := SplitOBUs(data)
		if err != nil {
			return false
		}
		for _, o := range obus {
			if o.typ == obuSequenceHeader {
				if h, err := ParseAV1SequenceHeader(o.body); err == nil {
					p.Codec = h.CodecString()
					p.CodedWidth, p.CodedHeight = h.MaxWidth, h.MaxHeight
					return true
				}
			}
		}
	}
	return false
}

// hasParamSets reports whether a key frame carries its own parameter sets.
func (p *Params) hasParamSets(data []byte) bool {
	switch p.Family {
	case H264:
		sps, pps := false, false
		for _, n := range SplitAnnexB(data) {
			if len(n) == 0 {
				continue
			}
			switch h264Type(n) {
			case 7:
				sps = true
			case 8:
				pps = true
			}
		}
		return sps && pps
	case HEVC:
		vps, sps, pps := false, false, false
		for _, n := range SplitAnnexB(data) {
			if len(n) < 2 {
				continue
			}
			switch hevcType(n) {
			case 32:
				vps = true
			case 33:
				sps = true
			case 34:
				pps = true
			}
		}
		return vps && sps && pps
	case AV1:
		obus, err := SplitOBUs(data)
		if err != nil {
			return true
		}
		for _, o := range obus {
			if o.typ == obuSequenceHeader {
				return true
			}
		}
		return false
	}
	return true
}

// PrepareKeyFrame makes sure a key frame carries parameter sets (prepending the
// cached ones when the encoder put them only in global extradata), refreshes the
// cached parameter sets / codec string, and returns the frame to send.
func (p *Params) PrepareKeyFrame(data []byte) []byte {
	if p.hasParamSets(data) {
		p.updateCodec(data)
		return data
	}
	if len(p.sets) == 0 {
		return data
	}
	if p.Family == AV1 {
		// Insert the sequence header after the temporal delimiter, if any.
		obus, err := SplitOBUs(data)
		if err == nil && len(obus) > 0 && obus[0].typ == obuTemporalDelimiter {
			td := obus[0].raw
			out := make([]byte, 0, len(data)+len(p.sets))
			out = append(out, td...)
			out = append(out, p.sets...)
			return append(out, data[len(td):]...)
		}
	}
	out := make([]byte, 0, len(data)+len(p.sets))
	out = append(out, p.sets...)
	return append(out, data...)
}

// ---------------------------------------------------------------------------

type bitReader struct {
	b   []byte
	pos int // bit position
	err bool
}

func (r *bitReader) u(n uint32) uint32 {
	var v uint32
	for i := uint32(0); i < n; i++ {
		byteIdx := r.pos >> 3
		if byteIdx >= len(r.b) {
			r.err = true
			return 0
		}
		bit := (r.b[byteIdx] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) uvlc() uint32 {
	lz := 0
	for r.u(1) == 0 {
		if r.err || lz >= 32 {
			r.err = true
			return 0
		}
		lz++
	}
	if lz >= 32 {
		return 0xffffffff
	}
	return r.u(uint32(lz)) + (1<<uint(lz) - 1)
}
