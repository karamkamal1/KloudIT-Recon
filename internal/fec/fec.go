// Package fec cuts video frames into the "datagram + FEC" mode's shards and
// rebuilds them (GUIDE 2.5; the wire format is proto.VideoShard).
//
// A frame of L bytes becomes n = ceil(L / MaxShardPayload) data shards of
// equal size (the last one shorter, zero-padded for the code), grouped into
// consecutive blocks of at most proto.MaxBlockData data shards (as equal as
// they divide). Each block is a systematic Reed-Solomon code over GF(2^8)
// (github.com/klauspost/reedsolomon's default matrix: a Vandermonde matrix
// made systematic by the inverse of its top square; polynomial 0x11d):
// parity row r of a block of K data shards is row K+r of that matrix, the
// same whatever the number of parity shards, so a repair can send rows the
// frame did not (web/static/js/fec.js builds the same rows to decode).
package fec

import (
	"errors"
	"math"
	"sync"

	"github.com/klauspost/reedsolomon"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Block is a Reed-Solomon block of a frame: its data shards Base..Base+K-1
// and the M parity shards sent with the frame.
type Block struct{ Base, K, M int }

// Layout returns the shard size of a frame of frameLen bytes with shards of
// at most maxShard bytes (as equal as the frame divides), and its blocks (M
// 0): Blocks of its ceil(frameLen / size) data shards, the count a receiver
// derives from a shard's header.
func Layout(frameLen, maxShard int) (size int, blocks []Block) {
	if frameLen <= 0 || maxShard <= 0 {
		return 0, nil
	}
	n := (frameLen + maxShard - 1) / maxShard
	size = (frameLen + n - 1) / n
	return size, Blocks((frameLen + size - 1) / size)
}

// Blocks returns the blocks of a frame of n data shards: ceil(n /
// proto.MaxBlockData) blocks, block i the data shards floor(i*n/nb) ..
// floor((i+1)*n/nb) - 1 (web/static/js/fec.js blockLayout).
func Blocks(n int) []Block {
	nb := (n + proto.MaxBlockData - 1) / proto.MaxBlockData
	blocks := make([]Block, 0, nb)
	for i := 0; i < nb; i++ {
		lo, hi := i*n/nb, (i+1)*n/nb
		blocks = append(blocks, Block{Base: lo, K: hi - lo})
	}
	return blocks
}

// Parity bounds (GUIDE 2.5: parity 5 % below 0.5 % loss up to 30 % at 3-5 %).
const (
	MinParityRatio = 0.05
	MaxParityRatio = 0.30
	// Residual is the share of blocks the parity may leave short of K
	// shards at the measured loss (a binomial tail): those are repaired by a
	// NACK while the frame has time, or lost.
	Residual = 0.01
)

// RatioCap is the most parity a block gets at shard loss rate p, as a share
// of its data shards: the guide's ramp, 5 % below 0.5 % loss rising linearly
// to 30 % at 3 % and above.
func RatioCap(p float64) float64 {
	return min(MaxParityRatio, max(MinParityRatio, MinParityRatio+(p-0.005)*10))
}

// Parity is how many parity shards a block of k data shards gets at shard
// loss rate p: the fewest that leave at most Residual of such blocks short
// of k shards, with independent losses, but at least 5 % of k (and one) and
// at most RatioCap(p) of k. Small blocks need relatively more (a frame of
// one shard needs a whole copy); large ones less than the cap: at 3 % loss
// a 35-shard block (20 Mbit/s at 60 fps) gets 4 (11 %).
func Parity(k int, p float64) int {
	if k <= 0 {
		return 0
	}
	lo := max(1, int(math.Ceil(MinParityRatio*float64(k)-1e-9)))
	hi := max(lo, int(math.Ceil(RatioCap(p)*float64(k)-1e-9)))
	m := lo
	for m < hi && binomTail(k+m, m, p) > Residual {
		m++
	}
	return min(m, proto.MaxBlockShards-k)
}

// binomTail is P(X > m) for X ~ Binomial(n, p).
func binomTail(n, m int, p float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	q := 1 - p
	term := math.Pow(q, float64(n)) // P(X = 0)
	cdf := term
	for i := 1; i <= m; i++ {
		term *= float64(n-i+1) / float64(i) * p / q
		cdf += term
	}
	return max(0, 1-cdf)
}

// Encoder makes frames' shards. It caches the Reed-Solomon codes per block
// shape; safe for concurrent use.
type Encoder struct {
	mu    sync.Mutex
	codes map[[2]int]reedsolomon.Encoder
}

func (e *Encoder) code(k, m int) (reedsolomon.Encoder, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.codes[[2]int{k, m}]; c != nil {
		return c, nil
	}
	c, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, err
	}
	if e.codes == nil {
		e.codes = map[[2]int]reedsolomon.Encoder{}
	}
	if len(e.codes) > 4096 {
		clear(e.codes) // shapes of long ago (sizes vary with the bitrate)
	}
	e.codes[[2]int{k, m}] = c
	return c, nil
}

// Frame is a frame cut into shards, kept for repairs.
type Frame struct {
	Gen    uint8
	Seq    uint32
	Len    int     // frame bytes
	Size   int     // shard size
	Blocks []Block // M: the parity shards sent with the frame
	// data: the data shards, each Size bytes (the last zero-padded).
	data [][]byte
	// next: per block, the next parity row a repair sends.
	next []int
}

// Shards returns the frame's data and parity shards sent with it.
func (f *Frame) Shards() int {
	n := 0
	for _, b := range f.Blocks {
		n += b.K + b.M
	}
	return n
}

// ErrTooLarge: a frame with more data shards than a shard header can number.
var ErrTooLarge = errors.New("fec: frame too large for shards")

// Cut cuts frame (the bytes a frame stream carries: header, extension,
// payload) of generation gen, sequence seq into shards of at most maxShard
// bytes, with parity(k) parity shards per block of k data shards, and
// returns it and its datagrams in sending order: each block's data shards,
// then its parity. The datagrams share one buffer; frame is copied.
func (e *Encoder) Cut(frame []byte, gen uint8, seq uint32, maxShard int, parity func(k int) int) (*Frame, [][]byte, error) {
	size, blocks := Layout(len(frame), min(maxShard, proto.MaxShardPayload))
	if size == 0 {
		return nil, nil, errors.New("fec: empty frame")
	}
	n := blocks[len(blocks)-1].Base + blocks[len(blocks)-1].K
	if n > math.MaxUint16 {
		return nil, nil, ErrTooLarge
	}
	f := &Frame{Gen: gen, Seq: seq, Len: len(frame), Size: size, Blocks: blocks, next: make([]int, len(blocks))}
	total := 0
	for i := range f.Blocks {
		b := &f.Blocks[i]
		b.M = min(max(0, parity(b.K)), proto.MaxBlockShards-b.K)
		f.next[i] = b.M
		total += b.K + b.M
	}
	slab := make([]byte, total*(proto.VideoShardHeaderLen+size))
	out := make([][]byte, 0, total)
	f.data = make([][]byte, n)
	at := 0
	for _, b := range f.Blocks {
		shards := make([][]byte, b.K+b.M)
		for i := range shards {
			dg := slab[at : at+proto.VideoShardHeaderLen+size]
			at += len(dg)
			s := proto.VideoShard{Gen: gen, Index: uint8(i), Seq: seq, FrameLen: uint32(len(frame)), Size: uint16(size),
				Base: uint16(b.Base), K: uint8(b.K), M: uint8(b.M)}
			payload := dg[proto.VideoShardHeaderLen:]
			if i < b.K {
				d := b.Base + i
				copy(payload, frame[d*size:min(len(frame), (d+1)*size)])
				f.data[d] = payload
				dg = dg[:proto.VideoShardHeaderLen+min(size, len(frame)-d*size)]
			} else {
				s.Flags = proto.ShardParity
			}
			s.Append(dg[:0])
			shards[i] = payload
			out = append(out, dg)
		}
		if b.M > 0 {
			c, err := e.code(b.K, b.M)
			if err != nil {
				return nil, nil, err
			}
			if err := c.Encode(shards); err != nil {
				return nil, nil, err
			}
		}
	}
	return f, out, nil
}

// Repair returns up to count parity shards of block bi that the frame has
// not sent yet (fresh rows: any K of a block's shards rebuild it), flagged
// as repairs; fewer when the block's rows run out (proto.MaxBlockShards).
func (e *Encoder) Repair(f *Frame, bi, count int) ([][]byte, error) {
	if bi < 0 || bi >= len(f.Blocks) {
		return nil, errors.New("fec: no such block")
	}
	b := f.Blocks[bi]
	from := f.next[bi]
	count = min(count, proto.MaxBlockShards-b.K-from)
	if count <= 0 {
		return nil, nil
	}
	c, err := e.code(b.K, from+count)
	if err != nil {
		return nil, err
	}
	shards := make([][]byte, b.K+from+count)
	copy(shards, f.data[b.Base:b.Base+b.K])
	for i := b.K; i < len(shards); i++ {
		shards[i] = make([]byte, f.Size)
	}
	if err := c.Encode(shards); err != nil {
		return nil, err
	}
	f.next[bi] = from + count
	out := make([][]byte, 0, count)
	for r := from; r < from+count; r++ {
		s := proto.VideoShard{Flags: proto.ShardParity | proto.ShardRepair, Gen: f.Gen, Index: uint8(b.K + r), Seq: f.Seq,
			FrameLen: uint32(f.Len), Size: uint16(f.Size), Base: uint16(b.Base), K: uint8(b.K), M: uint8(b.M), Data: shards[b.K+r]}
		out = append(out, s.Append(make([]byte, 0, proto.VideoShardHeaderLen+f.Size)))
	}
	return out, nil
}

// Resend returns block bi's data shards again, flagged as repairs (for a
// client that has none of the frame's shards: it does not know the blocks).
func (f *Frame) Resend(bi int) [][]byte {
	if bi < 0 || bi >= len(f.Blocks) {
		return nil
	}
	b := f.Blocks[bi]
	out := make([][]byte, 0, b.K)
	for i := 0; i < b.K; i++ {
		d := b.Base + i
		s := proto.VideoShard{Flags: proto.ShardRepair, Gen: f.Gen, Index: uint8(i), Seq: f.Seq, FrameLen: uint32(f.Len),
			Size: uint16(f.Size), Base: uint16(b.Base), K: uint8(b.K), M: uint8(b.M), Data: f.data[d][:min(f.Size, f.Len-d*f.Size)]}
		out = append(out, s.Append(make([]byte, 0, proto.VideoShardHeaderLen+f.Size)))
	}
	return out
}

// Block returns the index of the block whose first data shard is base, or -1.
func (f *Frame) Block(base int) int {
	for i, b := range f.Blocks {
		if b.Base == base {
			return i
		}
	}
	return -1
}

// Assembler rebuilds frames from their shards: the Go counterpart of the
// client's (web/static/js/fec.js), for tests and the Go reference client.
type Assembler struct {
	enc    Encoder
	frames map[[2]uint32]*partial
}

type partial struct {
	len, size int
	blocks    map[int]*partialBlock // by base
	done      bool
}

type partialBlock struct {
	k      int
	shards map[int][]byte // index -> payload (data shards padded to size)
	done   bool
}

// Add takes one shard; it returns the frame once its last missing block is
// rebuilt (once per frame).
func (a *Assembler) Add(s proto.VideoShard) ([]byte, error) {
	if a.frames == nil {
		a.frames = map[[2]uint32]*partial{}
	}
	key := [2]uint32{uint32(s.Gen), s.Seq}
	p := a.frames[key]
	if p == nil {
		p = &partial{len: int(s.FrameLen), size: int(s.Size), blocks: map[int]*partialBlock{}}
		a.frames[key] = p
	}
	if p.done || int(s.FrameLen) != p.len || int(s.Size) != p.size {
		return nil, nil
	}
	b := p.blocks[int(s.Base)]
	if b == nil {
		b = &partialBlock{k: int(s.K), shards: map[int][]byte{}}
		p.blocks[int(s.Base)] = b
	}
	if b.done || int(s.K) != b.k {
		return nil, nil
	}
	if _, dup := b.shards[int(s.Index)]; dup {
		return nil, nil
	}
	d := make([]byte, p.size)
	copy(d, s.Data)
	b.shards[int(s.Index)] = d
	if len(b.shards) >= b.k {
		if err := a.rebuild(b); err != nil {
			return nil, err
		}
	}
	return a.finish(p)
}

// rebuild fills in block b's missing data shards.
func (a *Assembler) rebuild(b *partialBlock) error {
	top := 0
	for i := range b.shards {
		top = max(top, i+1)
	}
	if top > b.k {
		shards := make([][]byte, top)
		for i, d := range b.shards {
			shards[i] = d
		}
		c, err := a.enc.code(b.k, top-b.k)
		if err != nil {
			return err
		}
		if err := c.ReconstructData(shards); err != nil {
			return err
		}
		for i := 0; i < b.k; i++ {
			b.shards[i] = shards[i]
		}
	}
	b.done = true
	return nil
}

// finish returns the frame once every block is done.
func (a *Assembler) finish(p *partial) ([]byte, error) {
	n := (p.len + p.size - 1) / p.size
	have := 0
	for _, b := range p.blocks {
		if !b.done {
			return nil, nil
		}
		have += b.k
	}
	if have < n {
		return nil, nil
	}
	out := make([]byte, 0, n*p.size)
	for d := 0; d < n; {
		var b *partialBlock
		if b = p.blocks[d]; b == nil {
			return nil, errors.New("fec: blocks do not cover the frame")
		}
		for i := 0; i < b.k; i++ {
			out = append(out, b.shards[i]...)
		}
		d += b.k
	}
	p.done = true
	return out[:p.len], nil
}

// Need returns the blocks of frame (gen, seq) still short of shards, with
// how many more each needs (a FECNack's entries); seen is false when no
// shard of the frame arrived (the client NACKs it whole), done true once it
// was rebuilt.
func (a *Assembler) Need(gen uint8, seq uint32) (blocks []proto.NackBlock, seen, done bool) {
	p := a.frames[[2]uint32{uint32(gen), seq}]
	if p == nil {
		return nil, false, false
	}
	if p.done {
		return nil, true, true
	}
	for _, b := range Blocks((p.len + p.size - 1) / p.size) {
		have := 0
		if pb := p.blocks[b.Base]; pb != nil {
			if pb.done {
				continue
			}
			have = len(pb.shards)
		}
		blocks = append(blocks, proto.NackBlock{Base: uint16(b.Base), Need: uint8(b.K - have)})
	}
	return blocks, true, false
}

// Forget drops what is kept of frame (gen, seq).
func (a *Assembler) Forget(gen uint8, seq uint32) { delete(a.frames, [2]uint32{uint32(gen), seq}) }
