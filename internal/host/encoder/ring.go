package encoder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

// Shared-memory frame ring, layout version 1 (docs/HELPER_PROTOCOL.md; the
// producer is native/recon-encoder/src/ring.cpp). recon-host creates the
// mapping and writes the static header; the helper writes slots and
// writeCount; recon-host writes readCount. Everything read back from shared
// memory is bounds-checked: the helper is a separate, restartable process and
// must not be able to make recon-host read outside the mapping.
const (
	ringMagic       = 0x474E524E4F434552 // "RECONRNG" little-endian
	ringVersion     = 1
	ringHeaderSize  = 4096
	slotHeaderSize  = 128
	minSlotSize     = 64 << 10
	maxSlots        = 1024
	DefaultSlots    = 8
	DefaultSlotSize = 4 << 20

	// Ring header.
	offMagic          = 0  // u64
	offVersion        = 8  // u32
	offHeaderSize     = 12 // u32
	offSlotCount      = 16 // u32
	offSlotSize       = 20 // u32
	offTotalSize      = 24 // u64
	offSlotHeaderSize = 32 // u32
	offQPCFrequency   = 40 // i64, helper
	offHelperPID      = 48 // u32, helper
	offWriteCount     = 64 // u64 atomic, helper
	offReadCount      = 128
	offDropped        = 192

	// Slot header.
	slotSeq           = 0
	slotFrameID       = 8
	slotFlags         = 16
	slotGen           = 20
	slotPayloadOffset = 24
	slotPayloadSize   = 28
	slotPresentQPC    = 32
	slotCaptureQPC    = 40
	slotSubmitQPC     = 48
	slotOutputQPC     = 56
	slotRefFloor      = 64
	slotLTRSlot       = 72
	slotTemporalLayer = 76
	slotRefLTRMask    = 80
	slotDroppedBefore = 84
	slotWidth         = 88
	slotHeight        = 92
	slotDirtyPPM      = 96 // u32, valid with FlagDirty (Phase 5)

	FlagKey           = 1 << 0
	FlagRecovery      = 1 << 1
	FlagDroppedBefore = 1 << 2
	FlagRepeat        = 1 << 3
	FlagSeqStart      = 1 << 4
	// Phase 5 (additive: older helpers leave them unset).
	FlagDirty       = 1 << 5 // the slot's dirty share is valid
	FlagDiscardable = 1 << 6 // no later frame references this one
)

// ErrRingCorrupt means the shared memory holds something the helper cannot
// have written correctly; the helper must be restarted.
var ErrRingCorrupt = errors.New("encoder ring corrupt")

// Frame is one encoded frame (an Annex-B access unit or AV1 temporal unit),
// copied out of the ring.
type Frame struct {
	FrameID       uint64 // helper frame counter, from 1; gaps are frames the helper dropped
	Gen           uint32 // encoder generation inside the helper
	Key           bool   // IDR / key frame with parameter sets
	Recovery      bool   // references only acknowledged frames (RefFloor valid)
	Repeat        bool   // idle re-submit of the previous image (nothing new on screen)
	SeqStart      bool   // key frame starting a sequence: the stream's first, or ForceIDR's (barcodes count from it)
	DroppedBefore uint32 // frames the helper dropped right before this one (ring full / too large)
	RefFloor      uint64
	LTRSlot       int32 // LTR slot this frame was marked into, -1 = none
	TemporalLayer uint32
	// Discardable: no later frame references this one (the top temporal
	// layer of an SVC stream, a non-reference frame): it can be left out
	// without breaking the decoding of any other (see Droppable).
	Discardable bool
	// Dirty is the share of the picture the capture reported as changed since
	// the previous frame (0..1, from the dirty rects; 0 for an idle repeat),
	// -1 = unknown (no dirty rects from this capture method, older helpers).
	Dirty         float64
	RefLTRMask    uint32
	Width, Height uint32
	// QPC ticks (Ring.QPCFrequency per second); PresentQPC is 0 when unknown.
	PresentQPC, CaptureQPC, SubmitQPC, OutputQPC int64
	Data                                         []byte
}

// RingSize returns the mapping size for a ring geometry.
func RingSize(slots, slotSize int) (int, error) {
	if slots < 2 || slots > maxSlots {
		return 0, fmt.Errorf("encoder ring: %d slots out of range 2..%d", slots, maxSlots)
	}
	if slotSize < minSlotSize || slotSize > 1<<30 || slotSize%4096 != 0 {
		return 0, fmt.Errorf("encoder ring: slot size %d must be a multiple of 4096 in 64 KiB..1 GiB", slotSize)
	}
	total := uint64(ringHeaderSize) + uint64(slots)*uint64(slotSize)
	if total > 16<<30 {
		return 0, fmt.Errorf("encoder ring: %d bytes is too large", total)
	}
	return int(total), nil
}

// InitRing writes the static header into a fresh, zeroed mapping of
// RingSize(slots, slotSize) bytes.
func InitRing(mem []byte, slots, slotSize int) error {
	size, err := RingSize(slots, slotSize)
	if err != nil {
		return err
	}
	if len(mem) != size {
		return fmt.Errorf("encoder ring: mapping is %d bytes, want %d", len(mem), size)
	}
	le := binary.LittleEndian
	le.PutUint64(mem[offMagic:], ringMagic)
	le.PutUint32(mem[offVersion:], ringVersion)
	le.PutUint32(mem[offHeaderSize:], ringHeaderSize)
	le.PutUint32(mem[offSlotCount:], uint32(slots))
	le.PutUint32(mem[offSlotSize:], uint32(slotSize))
	le.PutUint64(mem[offTotalSize:], uint64(size))
	le.PutUint32(mem[offSlotHeaderSize:], slotHeaderSize)
	return nil
}

// Ring is the consumer side. Its geometry and read position are kept in Go
// memory; shared memory is only trusted for slot contents, which are checked.
// Not safe for concurrent use.
type Ring struct {
	mem      []byte
	slots    uint64
	slotSize uint64
	read     uint64 // next write index to consume
}

// NewRing wraps a mapping initialised with InitRing(mem, slots, slotSize).
func NewRing(mem []byte, slots, slotSize int) (*Ring, error) {
	size, err := RingSize(slots, slotSize)
	if err != nil {
		return nil, err
	}
	if len(mem) != size {
		return nil, fmt.Errorf("encoder ring: mapping is %d bytes, want %d", len(mem), size)
	}
	if uintptr(unsafe.Pointer(&mem[0]))%8 != 0 {
		return nil, errors.New("encoder ring: mapping is not 8-byte aligned")
	}
	return &Ring{mem: mem, slots: uint64(slots), slotSize: uint64(slotSize)}, nil
}

func (r *Ring) counter(off int) *uint64 { return (*uint64)(unsafe.Pointer(&r.mem[off])) }

// QPCFrequency is the helper's QueryPerformanceFrequency (0 until it attached).
func (r *Ring) QPCFrequency() int64 {
	return int64(binary.LittleEndian.Uint64(r.mem[offQPCFrequency:]))
}

// HelperPID is the process id the helper wrote when it attached.
func (r *Ring) HelperPID() uint32 { return binary.LittleEndian.Uint32(r.mem[offHelperPID:]) }

// Dropped is the helper's count of frames it could not put into the ring.
func (r *Ring) Dropped() uint64 { return atomic.LoadUint64(r.counter(offDropped)) }

// Pending returns how many published frames have not been read yet.
func (r *Ring) Pending() (int, error) {
	w := atomic.LoadUint64(r.counter(offWriteCount))
	if w < r.read || w-r.read > r.slots {
		return 0, fmt.Errorf("%w: writeCount %d, readCount %d, %d slots", ErrRingCorrupt, w, r.read, r.slots)
	}
	return int(w - r.read), nil
}

// Next copies the oldest unread frame out of the ring and releases its slot
// at once. It returns (nil, nil) when the ring is empty.
func (r *Ring) Next() (*Frame, error) {
	n, err := r.Pending()
	if err != nil || n == 0 {
		return nil, err
	}
	base := uint64(ringHeaderSize) + (r.read%r.slots)*r.slotSize
	s := r.mem[base : base+r.slotSize : base+r.slotSize]
	le := binary.LittleEndian
	if seq := le.Uint64(s[slotSeq:]); seq != r.read {
		return nil, fmt.Errorf("%w: slot sequence %d, want %d", ErrRingCorrupt, seq, r.read)
	}
	off := uint64(le.Uint32(s[slotPayloadOffset:]))
	size := uint64(le.Uint32(s[slotPayloadSize:]))
	if off < slotHeaderSize || off > r.slotSize || size > r.slotSize-off {
		return nil, fmt.Errorf("%w: payload %d+%d outside a %d-byte slot", ErrRingCorrupt, off, size, r.slotSize)
	}
	flags := le.Uint32(s[slotFlags:])
	f := &Frame{
		FrameID:       le.Uint64(s[slotFrameID:]),
		Gen:           le.Uint32(s[slotGen:]),
		Key:           flags&FlagKey != 0,
		Recovery:      flags&FlagRecovery != 0,
		Repeat:        flags&FlagRepeat != 0,
		SeqStart:      flags&FlagSeqStart != 0,
		Discardable:   flags&FlagDiscardable != 0,
		Dirty:         -1,
		DroppedBefore: le.Uint32(s[slotDroppedBefore:]),
		LTRSlot:       int32(le.Uint32(s[slotLTRSlot:])),
		TemporalLayer: le.Uint32(s[slotTemporalLayer:]),
		RefLTRMask:    le.Uint32(s[slotRefLTRMask:]),
		Width:         le.Uint32(s[slotWidth:]),
		Height:        le.Uint32(s[slotHeight:]),
		PresentQPC:    int64(le.Uint64(s[slotPresentQPC:])),
		CaptureQPC:    int64(le.Uint64(s[slotCaptureQPC:])),
		SubmitQPC:     int64(le.Uint64(s[slotSubmitQPC:])),
		OutputQPC:     int64(le.Uint64(s[slotOutputQPC:])),
		Data:          append([]byte(nil), s[off:off+size]...),
	}
	if f.Recovery {
		f.RefFloor = le.Uint64(s[slotRefFloor:])
	}
	if flags&FlagDirty != 0 {
		f.Dirty = min(1, float64(le.Uint32(s[slotDirtyPPM:]))/1e6)
	}
	r.read++
	atomic.StoreUint64(r.counter(offReadCount), r.read)
	return f, nil
}
