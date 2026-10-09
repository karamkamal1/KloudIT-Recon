package proto

import (
	"encoding/binary"
	"errors"
)

// "Datagram + FEC" video mode (GUIDE 2.5). Where the path's round trip makes
// a lost packet's retransmission cost a visible stall (the host enables it
// above 15 ms: internal/host/fec.go), a video frame does not travel on its own
// stream: its bytes, exactly as a frame stream would carry them (FrameHeader,
// extension, payload), are cut into shards of at most MaxShardPayload bytes,
// grouped into Reed-Solomon blocks of at most MaxBlockData data shards, each
// with parity shards (GF(2^8), the systematic Vandermonde code of
// github.com/klauspost/reedsolomon's default matrix; web/static/js/fec.js
// decodes the same), and every shard is one DgVideoShard datagram. Any K of a
// block's shards rebuild its data. Shards a frame still needs when its
// shards stop coming the client asks for with a DgFECNack; the host answers
// with fresh parity shards (repairs). Clients announce support in their
// hello (Hello.FEC); WebSocket clients never do.

// HelloFECVersion is the shard format a client handles (Hello.FEC).
const HelloFECVersion = 1

// FeatureVideoFEC is the Welcome.Features entry of a host that may send this
// client its video frames as shards (it took Hello.FEC, and the path ends at
// the client: direct WebTransport or the UDP relay).
const FeatureVideoFEC = "video-fec"

// Shard layout limits.
const (
	VideoShardHeaderLen = 18
	// MaxShardPayload is the largest shard (GUIDE 2.5: <= 1200 bytes): with
	// the header and QUIC's and WebTransport's framing it fits the smallest
	// QUIC packet quic-go sends (1280 bytes).
	MaxShardPayload = 1200
	// MaxBlockData is the most data shards in one Reed-Solomon block: blocks
	// stay small enough for the client to rebuild in a fraction of a
	// millisecond, and data + parity within GF(2^8)'s 256 shards.
	MaxBlockData = 64
	// MaxBlockShards is the most shards (data + parity, repairs included) of
	// a block: GF(2^8) has 256 distinct evaluation points.
	MaxBlockShards = 256
)

// VideoShard flags.
const (
	ShardParity byte = 1 // a parity shard (Index >= K)
	ShardRepair byte = 2 // sent for a FECNack, not with the frame
)

// VideoShard is one DgVideoShard datagram:
//
//	u8 0x12 | u8 flags | u8 gen | u8 index | u32 seq | u32 frameLen |
//	u16 size | u16 base | u8 k | u8 m | payload
//
// The frame (FrameLen bytes) is cut into n = ceil(FrameLen / Size) data
// shards of Size bytes, the last one shorter (sent unpadded; zero-padded to
// Size for the code). A block is the data shards Base..Base+K-1 and their
// parity; Index is the shard's position in it (0..K-1 data shard Base+Index,
// K.. parity row Index-K), M the parity shards sent with the frame (repairs
// come after them). Gen and Seq are the frame's (FrameHeader).
type VideoShard struct {
	Flags    byte
	Gen      uint8
	Index    uint8
	Seq      uint32
	FrameLen uint32
	Size     uint16
	Base     uint16
	K, M     uint8
	Data     []byte
}

var ErrBadShard = errors.New("proto: malformed video shard")

// Append encodes the shard as a datagram onto b.
func (s *VideoShard) Append(b []byte) []byte {
	var h [VideoShardHeaderLen]byte
	le := binary.LittleEndian
	h[0], h[1], h[2], h[3] = DgVideoShard, s.Flags, s.Gen, s.Index
	le.PutUint32(h[4:], s.Seq)
	le.PutUint32(h[8:], s.FrameLen)
	le.PutUint16(h[12:], s.Size)
	le.PutUint16(h[14:], s.Base)
	h[16], h[17] = s.K, s.M
	b = append(b, h[:]...)
	return append(b, s.Data...)
}

// ParseVideoShard decodes a DgVideoShard datagram (Data aliases b) and
// checks that it is consistent: a block within the frame, a payload of the
// shard's size (the frame's last data shard: what is left of the frame).
func ParseVideoShard(b []byte) (VideoShard, error) {
	if len(b) < VideoShardHeaderLen || b[0] != DgVideoShard {
		return VideoShard{}, ErrBadShard
	}
	le := binary.LittleEndian
	s := VideoShard{
		Flags: b[1], Gen: b[2], Index: b[3],
		Seq:      le.Uint32(b[4:]),
		FrameLen: le.Uint32(b[8:]),
		Size:     le.Uint16(b[12:]),
		Base:     le.Uint16(b[14:]),
		K:        b[16], M: b[17],
		Data: b[VideoShardHeaderLen:],
	}
	if s.Size == 0 || s.FrameLen == 0 || s.FrameLen > MaxFrameSize || s.K == 0 || int(s.K) > MaxBlockData {
		return s, ErrBadShard
	}
	switch n := s.DataShards(); {
	case int(s.Base)+int(s.K) > n:
		return s, ErrBadShard
	case int(s.Index) < int(s.K) && len(s.Data) != s.dataLen(int(s.Base)+int(s.Index)):
		return s, ErrBadShard
	case int(s.Index) >= int(s.K) && len(s.Data) != int(s.Size):
		return s, ErrBadShard
	}
	return s, nil
}

// DataShards is the number of data shards of the frame.
func (s *VideoShard) DataShards() int {
	return int((uint64(s.FrameLen) + uint64(s.Size) - 1) / uint64(s.Size))
}

// dataLen is the length of data shard i of the frame.
func (s *VideoShard) dataLen(i int) int {
	return int(min(uint64(s.Size), uint64(s.FrameLen)-uint64(i)*uint64(s.Size)))
}

// FECNack asks for the shards a frame still needs (DgFECNack):
//
//	u8 0x42 | u8 gen | u8 count | u8 0 | u32 seq | count x (u16 base | u8 need | u8 0)
//
// Each entry names a block (its Base) and how many more of its shards would
// rebuild it. count 0: the client has none of the frame's shards (it saw the
// frames around it), so it does not know its blocks: send the whole frame.
type FECNack struct {
	Gen    uint8
	Seq    uint32
	Blocks []NackBlock
}

// NackBlock is one FECNack entry.
type NackBlock struct {
	Base uint16
	Need uint8
}

// MaxNackBlocks bounds a FECNack's entries (a frame of 64 blocks of 64
// shards is 4.9 MB).
const MaxNackBlocks = 64

// Marshal encodes the NACK as a datagram.
func (n FECNack) Marshal() []byte {
	k := min(len(n.Blocks), MaxNackBlocks)
	b := make([]byte, 8+4*k)
	b[0], b[1], b[2] = DgFECNack, n.Gen, byte(k)
	binary.LittleEndian.PutUint32(b[4:], n.Seq)
	for i, e := range n.Blocks[:k] {
		binary.LittleEndian.PutUint16(b[8+4*i:], e.Base)
		b[10+4*i] = e.Need
	}
	return b
}

// ParseFECNack decodes a DgFECNack datagram.
func ParseFECNack(b []byte) (FECNack, bool) {
	if len(b) < 8 || b[0] != DgFECNack || int(b[2]) > MaxNackBlocks || len(b) < 8+4*int(b[2]) {
		return FECNack{}, false
	}
	n := FECNack{Gen: b[1], Seq: binary.LittleEndian.Uint32(b[4:])}
	for i := 0; i < int(b[2]); i++ {
		n.Blocks = append(n.Blocks, NackBlock{Base: binary.LittleEndian.Uint16(b[8+4*i:]), Need: b[10+4*i]})
	}
	return n, true
}
