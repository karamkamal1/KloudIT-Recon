// Package proto defines the Recon wire protocol shared by the host agent, the
// gateway relay and the browser client (web/static/js/protocol.js mirrors it).
//
// A streaming session is carried by one of three transports that all expose
// the same four logical channels:
//
//	control  reliable, ordered   length-prefixed JSON messages (client-opened bidi stream, kind 'C')
//	input    reliable, ordered   length-prefixed binary input events (client-opened bidi stream, kind 'I')
//	frames   reliable per frame  one host-opened unidirectional stream per encoded video frame
//	datagram unreliable          audio packets, pointer motion, gamepad state, clock pings,
//	                             and video frames in the "datagram + FEC" mode (VideoShard)
//
// Over WebTransport and the gateway<->host QUIC tunnel the channels map 1:1 onto
// QUIC streams and datagrams. Over the WebSocket fallback every channel becomes a
// binary message prefixed with a one-byte channel id.
//
// All integers are little-endian.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// ALPN identifiers for the gateway's UDP listener.
const (
	ALPNHostControl = "recon-host/1" // host agent control connection
	ALPNHostData    = "recon-data/1" // per-session media connection from host to gateway
	ALPNHTTP3       = "h3"
)

// First byte written on every client-opened bidirectional stream.
const (
	StreamKindControl byte = 'C'
	StreamKindInput   byte = 'I'
)

// WebSocket channel ids (first byte of every binary message).
const (
	WSControl  byte = 0
	WSInput    byte = 1
	WSFrame    byte = 2
	WSDatagram byte = 3
)

// Datagram types (first byte of every datagram).
const (
	DgAudio      byte = 0x10 // host->client: audio packet
	DgCursorPos  byte = 0x11 // host->client: cursor position/visibility
	DgVideoShard byte = 0x12 // host->client: a shard of a video frame, "datagram + FEC" mode (VideoShard)
	DgMouseRel   byte = 0x20 // client->host: cumulative relative motion
	DgMouseAbs   byte = 0x21 // client->host: absolute position (normalised)
	DgGamepad    byte = 0x22 // client->host: full gamepad state snapshot
	DgRumble     byte = 0x23 // host->client: force feedback
	DgPing       byte = 0x30 // client->host
	DgPong       byte = 0x31 // host->client
	DgFrameAck   byte = 0x40 // client->host
	DgRateReport byte = 0x41 // client->host: receive report for the rate controller (RateReport)
	DgFECNack    byte = 0x42 // client->host: shards a video frame still needs ("datagram + FEC" mode, FECNack)
)

// Input stream event types.
const (
	InKey         byte = 1
	InMouseButton byte = 2
	InWheel       byte = 3
	InReleaseAll  byte = 4
	InText        byte = 5
)

// Video frame stream header.
const (
	FrameHeaderLen      = 24
	FrameTypeVideo byte = 1
	FrameFlagKey   byte = 1
	FrameFlagExt   byte = 0x80 // a TLV extension block follows the header (hello v >= 2)
)

// Frame header extension tags (FrameFlagExt). Every timestamp is in the host
// clock domain (µs, the same clock as FrameHeader.SendUs and pongs).
const (
	ExtPresentUs      byte = 1 // u64 game present (native capture helper only)
	ExtCaptureUs      byte = 2 // u64 frame captured
	ExtEncodeSubmitUs byte = 3 // u64 frame submitted to the encoder (native helper only)
	ExtEncodeDoneUs   byte = 4 // u64 encoded frame available to the host
	ExtRefFloor       byte = 5 // u32 newest earlier frame a recovery frame (or a later one) may reference (recovery frames)
	ExtLTRSlot        byte = 6 // u8 long-term reference slot this frame is marked into
	ExtTemporalLayer  byte = 7 // u8 temporal layer id
	extMaxTag              = 7
)

// HelloVersionFrameExt is the first hello version whose clients parse
// FrameFlagExt; older clients get the plain 24-byte header.
const HelloVersionFrameExt = 2

// FeatureFrameExt is the Welcome.Features entry announcing the frame header
// extension (sent to clients with hello v >= HelloVersionFrameExt).
const FeatureFrameExt = "frame-ext"

// Limits.
const (
	MaxControlMsg = 1 << 20  // 1 MiB (cursor images are the largest control messages)
	MaxInputMsg   = 64 << 10 // 64 KiB (text paste)
	MaxFrameSize  = 32 << 20 // 32 MiB, far above any real encoded frame
	MaxDatagram   = 1200
)

// Mouse buttons used on the input channel.
const (
	ButtonLeft   = 0
	ButtonMiddle = 1
	ButtonRight  = 2
	ButtonX1     = 3
	ButtonX2     = 4
)

var ErrTooLarge = errors.New("proto: message too large")

// WriteMsg writes a length-prefixed message (u32 LE length + payload).
func WriteMsg(w io.Writer, p []byte) error {
	buf := make([]byte, 4+len(p))
	binary.LittleEndian.PutUint32(buf, uint32(len(p)))
	copy(buf[4:], p)
	_, err := w.Write(buf)
	return err
}

// ReadMsg reads one length-prefixed message, refusing anything above max bytes.
func ReadMsg(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if int64(n) > int64(max) {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, n, max)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	return p, nil
}

// FrameHeader precedes every encoded video frame.
type FrameHeader struct {
	Type   byte
	Flags  byte
	Gen    uint8  // encoder generation; increments on every encoder (re)start
	Seq    uint32 // frame sequence number within a generation
	PtsUs  uint64 // presentation timestamp in microseconds (encoder timeline)
	SendUs uint64 // host clock (µs): handed to the transport (hello v >= 2); v1 clients: encoder out (= ExtEncodeDoneUs)
}

func (h *FrameHeader) Marshal(b []byte) {
	_ = b[FrameHeaderLen-1]
	b[0] = h.Type
	b[1] = h.Flags
	b[2] = h.Gen
	b[3] = 0
	binary.LittleEndian.PutUint32(b[4:], h.Seq)
	binary.LittleEndian.PutUint64(b[8:], h.PtsUs)
	binary.LittleEndian.PutUint64(b[16:], h.SendUs)
}

func (h *FrameHeader) Unmarshal(b []byte) error {
	if len(b) < FrameHeaderLen {
		return io.ErrUnexpectedEOF
	}
	h.Type = b[0]
	h.Flags = b[1]
	h.Gen = b[2]
	h.Seq = binary.LittleEndian.Uint32(b[4:])
	h.PtsUs = binary.LittleEndian.Uint64(b[8:])
	h.SendUs = binary.LittleEndian.Uint64(b[16:])
	return nil
}

// FrameExt holds the optional per-frame fields of the header extension. A field
// is present when its tag was set; readers skip tags they do not know.
//
//	u16 extLen | extLen bytes of entries: u8 tag | u8 len | value (len bytes, LE)
type FrameExt struct {
	has  uint16
	vals [extMaxTag + 1]uint64
}

var extSize = [extMaxTag + 1]uint8{0, 8, 8, 8, 8, 4, 1, 1}

var ErrBadFrameExt = errors.New("proto: malformed frame header extension")

// Set stores a known tag's value (truncated to the tag's width on the wire).
func (e *FrameExt) Set(tag byte, v uint64) {
	if tag == 0 || tag > extMaxTag {
		return
	}
	e.has |= 1 << tag
	e.vals[tag] = v
}

// Get returns a tag's value and whether it was present.
func (e *FrameExt) Get(tag byte) (uint64, bool) {
	if tag == 0 || tag > extMaxTag || e.has&(1<<tag) == 0 {
		return 0, false
	}
	return e.vals[tag], true
}

// Empty reports whether no field is set.
func (e *FrameExt) Empty() bool { return e.has == 0 }

// Append appends the encoded extension block (u16 length + entries) to b.
func (e *FrameExt) Append(b []byte) []byte {
	at := len(b)
	b = append(b, 0, 0)
	for tag := byte(1); tag <= extMaxTag; tag++ {
		if e.has&(1<<tag) == 0 {
			continue
		}
		n := extSize[tag]
		b = append(b, tag, n)
		for i := uint8(0); i < n; i++ {
			b = append(b, byte(e.vals[tag]>>(8*i)))
		}
	}
	binary.LittleEndian.PutUint16(b[at:], uint16(len(b)-at-2))
	return b
}

// ParseFrameExt decodes an extension block at the start of b and returns the
// number of bytes it occupies. Unknown tags are skipped; a known tag may use
// any width from 1 to 8 bytes.
func ParseFrameExt(b []byte) (FrameExt, int, error) {
	var e FrameExt
	if len(b) < 2 {
		return e, 0, ErrBadFrameExt
	}
	n := int(binary.LittleEndian.Uint16(b))
	if len(b) < 2+n {
		return e, 0, ErrBadFrameExt
	}
	p := b[2 : 2+n]
	for len(p) > 0 {
		if len(p) < 2 || len(p) < 2+int(p[1]) {
			return e, 0, ErrBadFrameExt
		}
		tag, l := p[0], int(p[1])
		if tag >= 1 && tag <= extMaxTag {
			if l < 1 || l > 8 {
				return e, 0, ErrBadFrameExt
			}
			var v uint64
			for i := 0; i < l; i++ {
				v |= uint64(p[2+i]) << (8 * i)
			}
			e.Set(tag, v)
		}
		p = p[2+l:]
	}
	return e, 2 + n, nil
}

// ParseFrame splits a frame stream into header, extension and payload.
func ParseFrame(b []byte) (FrameHeader, FrameExt, []byte, error) {
	var h FrameHeader
	if err := h.Unmarshal(b); err != nil {
		return h, FrameExt{}, nil, err
	}
	b = b[FrameHeaderLen:]
	if h.Flags&FrameFlagExt == 0 {
		return h, FrameExt{}, b, nil
	}
	e, n, err := ParseFrameExt(b)
	if err != nil {
		return h, e, nil, err
	}
	return h, e, b[n:], nil
}

// AudioPacket builds an audio datagram: type, codec id, u16 seq, u32 pts (48 kHz samples).
func AudioPacket(dst []byte, codec byte, seq uint16, pts uint32, payload []byte) []byte {
	dst = append(dst[:0], DgAudio, codec, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(dst[2:], seq)
	binary.LittleEndian.PutUint32(dst[4:], pts)
	return append(dst, payload...)
}

const (
	AudioCodecOpus byte = 1
	AudioCodecPCM  byte = 2 // s16le interleaved
)

// Pong builds the reply to a ping datagram (echoing id and client time).
// Bytes after the 16 it echoes (PingMinRTT) are not echoed.
func Pong(ping []byte, hostNowUs uint64) []byte {
	if len(ping) < 16 {
		return nil
	}
	out := make([]byte, 24)
	out[0] = DgPong
	copy(out[4:16], ping[4:16])
	binary.LittleEndian.PutUint64(out[16:], hostNowUs)
	return out
}

// MouseRel is a cumulative relative-motion datagram. The client sends running
// totals so that a lost datagram only delays motion instead of losing it.
type MouseRel struct {
	Seq        uint32
	CumX, CumY int32
}

func ParseMouseRel(b []byte) (MouseRel, bool) {
	if len(b) < 16 || b[0] != DgMouseRel {
		return MouseRel{}, false
	}
	return MouseRel{
		Seq:  binary.LittleEndian.Uint32(b[4:]),
		CumX: int32(binary.LittleEndian.Uint32(b[8:])),
		CumY: int32(binary.LittleEndian.Uint32(b[12:])),
	}, true
}

// MouseAbs carries a position normalised to 0..65535 on both axes.
type MouseAbs struct {
	Seq  uint32
	X, Y uint16
}

func ParseMouseAbs(b []byte) (MouseAbs, bool) {
	if len(b) < 12 || b[0] != DgMouseAbs {
		return MouseAbs{}, false
	}
	return MouseAbs{
		Seq: binary.LittleEndian.Uint32(b[4:]),
		X:   binary.LittleEndian.Uint16(b[8:]),
		Y:   binary.LittleEndian.Uint16(b[10:]),
	}, true
}

// Gamepad is an XInput-style state snapshot.
type Gamepad struct {
	Index     uint8
	Connected bool
	Seq       uint32
	Buttons   uint16 // XUSB button bits
	LT, RT    uint8
	LX, LY    int16
	RX, RY    int16
}

func ParseGamepad(b []byte) (Gamepad, bool) {
	if len(b) < 20 || b[0] != DgGamepad {
		return Gamepad{}, false
	}
	return Gamepad{
		Index:     b[1],
		Connected: b[2]&1 != 0,
		Seq:       binary.LittleEndian.Uint32(b[4:]),
		Buttons:   binary.LittleEndian.Uint16(b[8:]),
		LT:        b[10],
		RT:        b[11],
		LX:        int16(binary.LittleEndian.Uint16(b[12:])),
		LY:        int16(binary.LittleEndian.Uint16(b[14:])),
		RX:        int16(binary.LittleEndian.Uint16(b[16:])),
		RY:        int16(binary.LittleEndian.Uint16(b[18:])),
	}, true
}

// PingMinRTT returns the round-trip time a ping reports (clients since step
// 4.6 append a u32 after t0: the smallest RTT, in µs, of their pings in the
// last 30 s), or 0: from an older client, or before the client measured one.
// The host picks the Opus frame duration by it (media.OpusFrameMs). Hosts
// before it ignore the extra bytes.
func PingMinRTT(b []byte) time.Duration {
	if len(b) < 20 || b[0] != DgPing {
		return 0
	}
	return time.Duration(binary.LittleEndian.Uint32(b[16:])) * time.Microsecond
}

// Rumble builds a force-feedback datagram for gamepad idx: the speeds of its
// large (low-frequency) and small (high-frequency) motors, 0-255, as a game
// set them (XInputSetState). The host repeats a running state every 100 ms
// (the client plays each for a little longer) and a stop three times.
func Rumble(idx, large, small uint8) []byte {
	return []byte{DgRumble, idx, large, small}
}

// CursorPos builds a cursor position datagram (normalised coordinates).
func CursorPos(seq uint32, visible bool, x, y uint16) []byte {
	b := make([]byte, 12)
	b[0] = DgCursorPos
	if visible {
		b[1] = 1
	}
	binary.LittleEndian.PutUint32(b[4:], seq)
	binary.LittleEndian.PutUint16(b[8:], x)
	binary.LittleEndian.PutUint16(b[10:], y)
	return b
}

// FrameAck is sent by the client for every frame it receives.
type FrameAck struct {
	Gen      uint8
	Seq      uint32
	OWDUs    int32 // client-estimated one-way delay in µs (clock-sync corrected)
	DecodeUs uint32
}

func ParseFrameAck(b []byte) (FrameAck, bool) {
	if len(b) < 16 || b[0] != DgFrameAck {
		return FrameAck{}, false
	}
	return FrameAck{
		Gen:      b[1],
		Seq:      binary.LittleEndian.Uint32(b[4:]),
		OWDUs:    int32(binary.LittleEndian.Uint32(b[8:])),
		DecodeUs: binary.LittleEndian.Uint32(b[12:]),
	}, true
}

// FeatureRateReport is the Welcome.Features entry of hosts that run the
// delay-based rate controller (GUIDE 2.2): the client sends a RateReport
// every 20-50 ms and leaves the one-way delay to the host (no delay-based
// "congestion" messages; the decoder's still go). Clients that do not know
// it keep their own delay detection and the 0x40 acks, which the host then
// reads instead.
const FeatureRateReport = "rate-report"

// RateReportLen is the size of a RateReport datagram; RateReportShardsLen
// with the shard counters (RateReportShards).
const (
	RateReportLen       = 40
	RateReportShardsLen = 48
)

// RateReport flags.
const (
	RateReportOWD   byte = 1 // OWDP50Us / OWDMaxUs are valid: clock synced and frames since the previous report
	RateReportFrame byte = 2 // Gen / LastSeq name a frame: one was received
	// RateReportShards: Shards / ShardsLost follow (RateReportShardsLen
	// bytes; clients that receive video shards, GUIDE 2.5). Hosts before
	// them read the first 40 bytes.
	RateReportShards byte = 4
)

// RateReport is the client's receive report for the host's rate controller
// (DgRateReport, every 20-50 ms):
//
//	u8 type 0x41 | u8 flags | u8 gen | u8 0 | u32 timeMs | u32 lastSeq |
//	u32 frames | u32 bytes | i32 owdP50Us | i32 owdMaxUs | u32 lost |
//	u32 audio | u16 decodeQueue | u16 0 [| u32 shards | u32 shardsLost]
//
// The counters are cumulative since the client connected (they wrap), so a
// lost report loses nothing but its delay samples: the host takes the
// difference to the last report it got. The one-way delays are those of the
// frames received since the previous report (last byte received minus the
// frame's encodeDoneUs, or SendUs without the extension, on the client's
// synchronised clock: the same measure as the 0x40 ack).
type RateReport struct {
	Flags   byte
	Gen     uint8  // generation of LastSeq
	TimeMs  uint32 // client clock in ms (wraps): when the report was made
	LastSeq uint32 // newest frame of Gen received
	Frames  uint32 // video frames received (complete frame streams)
	Bytes   uint32 // their bytes, headers included (wraps)
	// One-way delay p50 and maximum of the frames received since the
	// previous report, µs (RateReportOWD).
	OWDP50Us, OWDMaxUs int32
	Lost               uint32 // frames lost on the way (gap timeout; not those the host reported dropped) + audio packets lost
	Audio              uint32 // audio packets received
	DecodeQueue        uint16 // frames handed to the decoder and not yet out of it
	// Video shards ("datagram + FEC" mode, RateReportShards): of the
	// frames' first transmissions (data and parity, not repairs), those
	// received and those that never arrived, both counted when the client
	// accounts a frame (about 100 ms after its first transmission is
	// through), so they cover the same frames. Cumulative, wrapping.
	Shards, ShardsLost uint32
}

// ParseRateReport decodes a DgRateReport datagram.
func ParseRateReport(b []byte) (RateReport, bool) {
	if len(b) < RateReportLen || b[0] != DgRateReport {
		return RateReport{}, false
	}
	le := binary.LittleEndian
	r := RateReport{
		Flags: b[1], Gen: b[2],
		TimeMs:      le.Uint32(b[4:]),
		LastSeq:     le.Uint32(b[8:]),
		Frames:      le.Uint32(b[12:]),
		Bytes:       le.Uint32(b[16:]),
		OWDP50Us:    int32(le.Uint32(b[20:])),
		OWDMaxUs:    int32(le.Uint32(b[24:])),
		Lost:        le.Uint32(b[28:]),
		Audio:       le.Uint32(b[32:]),
		DecodeQueue: le.Uint16(b[36:]),
	}
	if r.Flags&RateReportShards != 0 {
		if len(b) < RateReportShardsLen {
			r.Flags &^= RateReportShards
		} else {
			r.Shards, r.ShardsLost = le.Uint32(b[40:]), le.Uint32(b[44:])
		}
	}
	return r, true
}

// Marshal encodes the report as a DgRateReport datagram (tests and the Go
// reference client; web/static/js/protocol.js rateReport mirrors it).
func (r RateReport) Marshal() []byte {
	n := RateReportLen
	if r.Flags&RateReportShards != 0 {
		n = RateReportShardsLen
	}
	b := make([]byte, n)
	le := binary.LittleEndian
	b[0], b[1], b[2] = DgRateReport, r.Flags, r.Gen
	le.PutUint32(b[4:], r.TimeMs)
	le.PutUint32(b[8:], r.LastSeq)
	le.PutUint32(b[12:], r.Frames)
	le.PutUint32(b[16:], r.Bytes)
	le.PutUint32(b[20:], uint32(r.OWDP50Us))
	le.PutUint32(b[24:], uint32(r.OWDMaxUs))
	le.PutUint32(b[28:], r.Lost)
	le.PutUint32(b[32:], r.Audio)
	le.PutUint16(b[36:], r.DecodeQueue)
	if n == RateReportShardsLen {
		le.PutUint32(b[40:], r.Shards)
		le.PutUint32(b[44:], r.ShardsLost)
	}
	return b
}

// Input events ---------------------------------------------------------------

// InputEvent is a decoded event from the input channel.
type InputEvent struct {
	Type     byte
	Scancode uint16 // InKey
	Extended bool   // InKey
	Down     bool   // InKey, InMouseButton
	Button   uint8  // InMouseButton
	WheelY   int16  // InWheel, WHEEL_DELTA units (120 = one notch)
	WheelX   int16  // InWheel
	Text     string // InText
}

var errBadInput = errors.New("proto: malformed input event")

func ParseInput(b []byte) (InputEvent, error) {
	if len(b) == 0 {
		return InputEvent{}, errBadInput
	}
	ev := InputEvent{Type: b[0]}
	switch b[0] {
	case InKey:
		if len(b) < 4 {
			return ev, errBadInput
		}
		ev.Scancode = binary.LittleEndian.Uint16(b[1:])
		ev.Down = b[3]&1 != 0
		ev.Extended = b[3]&2 != 0
	case InMouseButton:
		if len(b) < 3 || b[1] > ButtonX2 {
			return ev, errBadInput
		}
		ev.Button = b[1]
		ev.Down = b[2] != 0
	case InWheel:
		if len(b) < 6 {
			return ev, errBadInput
		}
		ev.WheelY = int16(binary.LittleEndian.Uint16(b[2:]))
		ev.WheelX = int16(binary.LittleEndian.Uint16(b[4:]))
	case InReleaseAll:
	case InText:
		ev.Text = string(b[1:])
	default:
		return ev, errBadInput
	}
	return ev, nil
}

// Helpers used by tests and the Go reference client.

func KeyEvent(sc uint16, ext, down bool) []byte {
	b := []byte{InKey, 0, 0, 0}
	binary.LittleEndian.PutUint16(b[1:], sc)
	if down {
		b[3] |= 1
	}
	if ext {
		b[3] |= 2
	}
	return b
}

func MouseRelDatagram(seq uint32, cx, cy int32) []byte {
	b := make([]byte, 16)
	b[0] = DgMouseRel
	binary.LittleEndian.PutUint32(b[4:], seq)
	binary.LittleEndian.PutUint32(b[8:], uint32(cx))
	binary.LittleEndian.PutUint32(b[12:], uint32(cy))
	return b
}

// PingDatagram builds a ping as clients since step 4.6 send it: with their
// minimum RTT (PingMinRTT; 0: none measured yet).
func PingDatagram(id uint32, t0 float64, minRTTUs uint32) []byte {
	b := make([]byte, 20)
	b[0] = DgPing
	binary.LittleEndian.PutUint32(b[4:], id)
	binary.LittleEndian.PutUint64(b[8:], math.Float64bits(t0))
	binary.LittleEndian.PutUint32(b[16:], minRTTUs)
	return b
}
