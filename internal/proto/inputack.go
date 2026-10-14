package proto

import (
	"encoding/binary"
	"regexp"
)

// Input acknowledgements (second hardware run): the input latency figure.
// A host that lists FeatureInputAck answers the inputs a client marks with a
// DgInputAck datagram that carries the host-clock µs the input arrived and
// was injected (SendInput returned). With its clock sync the client turns
// that into "browser event -> injected on the host" (and, without the
// clock sync, the round trip of the input and its acknowledgement), shown in
// its overlay and reported to the host every ~10 s (MsgInput), which logs it.
//
// Marking: a mouse or gamepad datagram (DgMouseRel, DgMouseAbs, DgGamepad)
// with InputAckFlag in its byte 3 (zero padding before; hosts before this
// ignore it); the messages on the input stream all of them once the client
// sent the control message MsgInputAck, counted from 1 in the order the host
// reads them (the client counts its writes the same way). Clients before
// this send neither and get no acknowledgements.
const (
	DgInputAck byte = 0x24 // host->client: a marked input was injected (InputAck)

	// FeatureInputAck is the Welcome.Features entry of hosts that answer
	// marked inputs with DgInputAck and log the client's MsgInput reports.
	FeatureInputAck = "input-ack"
	// FeatureResetStreamAt is the Welcome.Features entry, from the hosts
	// that list FeatureInputAck, telling the client that its QUIC endpoint
	// negotiated RESET_STREAM_AT (partial delivery, GUIDE 2.4) on this
	// connection: without it there (and FeatureInputAck present) the
	// client's browser did not, on a path that ends at the client.
	FeatureResetStreamAt = "reset-stream-at"

	// InputAckFlag in byte 3 of an input datagram asks for its DgInputAck.
	InputAckFlag byte = 1

	// MsgInputAck ({"t":"inputAck"}): acknowledge every input stream
	// message from now on.
	MsgInputAck = "inputAck"
	// MsgInput ({"t":"input", ...}, InputReport): the client's input
	// latency summary, every ~10 s.
	MsgInput = "input"
)

// The input an acknowledgement answers (InputAck kind).
const (
	InputAckStream   byte = 1 // an input stream message (seq: its number, from 1)
	InputAckMouseRel byte = 2 // DgMouseRel (seq: its seq)
	InputAckMouseAbs byte = 3 // DgMouseAbs (seq: its seq)
	InputAckGamepad  byte = 4 // DgGamepad (idx: the pad, seq: its seq)
)

// WantsInputAck reports whether an input datagram carries InputAckFlag.
func WantsInputAck(d []byte) bool { return len(d) > 3 && d[3]&InputAckFlag != 0 }

// InputAck builds the acknowledgement of an input: kind, idx (the gamepad's
// index), seq, and the host-clock µs it arrived (recvUs) and was injected
// (doneUs), 24 bytes.
func InputAck(kind, idx byte, seq uint32, recvUs, doneUs uint64) []byte {
	b := make([]byte, 24)
	b[0], b[1], b[2] = DgInputAck, kind, idx
	binary.LittleEndian.PutUint32(b[4:], seq)
	binary.LittleEndian.PutUint64(b[8:], recvUs)
	binary.LittleEndian.PutUint64(b[16:], doneUs)
	return b
}

// InputReport is the client's input latency summary (MsgInput): rows of
// percentiles in ms over its last ~10 s (InputRowNames), the quality of its
// clock sync, and its browser with the features the client uses that it
// lacks (Lacks: short ids).
type InputReport struct {
	T       string      `json:"t"`
	Rows    []StageStat `json:"rows"`
	Clock   *ClockSync  `json:"clock,omitempty"`
	Browser string      `json:"browser,omitempty"` // e.g. "Chrome 122"
	Lacks   []string    `json:"lacks,omitempty"`
}

// ClockSync is the client's clock sync (its offset to the host clock from
// the ping with the smallest error bound): that ping's round trip, the
// bound now (half that round trip plus the drift allowance for its age),
// its age, and the pings answered in the last 30 s.
type ClockSync struct {
	MinRTTMs float64 `json:"minRttMs"`
	BoundMs  float64 `json:"boundMs"`
	AgeS     float64 `json:"ageS"`
	Samples  int     `json:"samples"`
}

// InputRowNames are the rows an InputReport may carry: browser event ->
// injected on the host (with the clock sync) for key and button presses
// (keys), mouse motion (mouse) and gamepads (pad); the same for all of them
// from the round trip without the clock sync (rtt_half: event -> sent, plus
// half of sent -> acknowledgement received less the host's part, plus the
// host's part); and the parts: event -> handed to the transport (client), of
// it event -> the page handed it to its worker (dispatch), sent -> arrived at
// the host (network, clock-synced), arrived -> injected (host, host clock
// only).
var InputRowNames = map[string]bool{"keys": true, "mouse": true, "pad": true, "rtt_half": true, "client": true, "dispatch": true, "network": true, "host": true}

// browserName accepts what a client may report as its browser.
var browserName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ._-]{0,39}$`)

// lackID accepts the ids of the features a client reports missing.
var lackID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Valid reports whether the report's browser and lacks are of the expected
// form (they are logged as given).
func (r InputReport) Valid() bool {
	if r.Browser != "" && !browserName.MatchString(r.Browser) {
		return false
	}
	if len(r.Lacks) > 16 || len(r.Rows) > len(InputRowNames) {
		return false
	}
	for _, l := range r.Lacks {
		if !lackID.MatchString(l) {
			return false
		}
	}
	return true
}
