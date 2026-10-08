package proto

import "bytes"

// UDP relay control packets (guide step 2.6). The gateway forwards the QUIC
// packets of one browser <-> host connection through a relay allocation, a
// UDP port of its own. Before that, the host binds its relay socket to the
// allocation from the inside with the allocation's secret token, which it got
// over the authenticated tunnel ("relay" message):
//
//	bind    (host -> gateway):  u8 0x01 | "RLY" | 32-byte token
//	bound   (gateway -> host):  u8 0x02 | "RLY"
//	release (host -> gateway):  u8 0x03 | "RLY" | 32-byte token
//
// The host releases the allocation when the relayed connection has ended, so
// the gateway frees the port at once instead of after its idle timeout.
//
// The first byte has its two high bits clear, so the packets can never be
// QUIC packets (RFC 9000 sets the fixed bit 0x40 on every packet): quic-go
// hands them to Transport.ReadNonQUICPacket on the host's relay socket, and the
// gateway never mistakes forwarded QUIC for them. "bound" is shorter than
// "bind", so the gateway's answer cannot amplify anything.
const (
	RelayBind     = 0x01
	RelayBound    = 0x02
	RelayRelease  = 0x03
	RelayTokenLen = 32
)

var relayMagic = []byte("RLY")

// RelayBindPacket builds a bind packet for token (RelayTokenLen bytes).
func RelayBindPacket(token []byte) []byte { return relayTokenPacket(RelayBind, token) }

// RelayReleasePacket builds a release packet for token.
func RelayReleasePacket(token []byte) []byte { return relayTokenPacket(RelayRelease, token) }

func relayTokenPacket(typ byte, token []byte) []byte {
	b := make([]byte, 0, 4+len(token))
	b = append(b, typ)
	b = append(b, relayMagic...)
	return append(b, token...)
}

// ParseRelayToken returns the type (RelayBind or RelayRelease) and token of a
// host packet.
func ParseRelayToken(b []byte) (byte, []byte, bool) {
	if len(b) != 4+RelayTokenLen || (b[0] != RelayBind && b[0] != RelayRelease) || !bytes.Equal(b[1:4], relayMagic) {
		return 0, nil, false
	}
	return b[0], b[4:], true
}

// RelayBoundPacket is the gateway's answer to a valid bind.
func RelayBoundPacket() []byte { return []byte{RelayBound, 'R', 'L', 'Y'} }

// IsRelayBound reports whether b is a bound packet.
func IsRelayBound(b []byte) bool {
	return len(b) == 4 && b[0] == RelayBound && bytes.Equal(b[1:4], relayMagic)
}
