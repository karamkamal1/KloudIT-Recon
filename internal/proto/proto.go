// Package proto defines the Recon wire protocol shared by the host agent, the
// gateway relay and the browser client (web/static/js/protocol.js mirrors it).
//
// A streaming session is carried by one of three transports that all expose
// the same four logical channels:
//
//	control  reliable, ordered   length-prefixed JSON messages (client-opened bidi stream, kind 'C')
//	input    reliable, ordered   length-prefixed binary input events (client-opened bidi stream, kind 'I')
//	frames   reliable per frame  one host-opened unidirectional stream per encoded video frame
//	datagram unreliable          audio packets, pointer motion, gamepad state, clock pings
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
	DgAudio     byte = 0x10 // host->client: audio packet
	DgCursorPos byte = 0x11 // host->client: cursor position/visibility
	DgMouseRel  byte = 0x20 // client->host: cumulative relative motion
	DgMouseAbs  byte = 0x21 // client->host: absolute position (normalised)
	DgGamepad   byte = 0x22 // client->host: full gamepad state snapshot
	DgRumble    byte = 0x23 // host->client: force feedback
	DgPing      byte = 0x30 // client->host
	DgPong      byte = 0x31 // host->client
	DgFrameAck  byte = 0x40 // client->host
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
)

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
	SendUs uint64 // host monotonic clock (µs) when the frame left the encoder
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

// Rumble builds a force-feedback datagram for gamepad idx.
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

func PingDatagram(id uint32, t0 float64) []byte {
	b := make([]byte, 16)
	b[0] = DgPing
	binary.LittleEndian.PutUint32(b[4:], id)
	binary.LittleEndian.PutUint64(b[8:], math.Float64bits(t0))
	return b
}
