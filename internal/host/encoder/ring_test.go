package encoder

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"unsafe"
)

// testWriter is a Go copy of the producer in native/recon-encoder/src/ring.cpp
// (RingWriter::write), used to test the reader without Windows.
type testWriter struct {
	mem            []byte
	slots          uint64
	slotSize       uint64
	written        uint64
	dropped        uint64
	droppedPending uint32
}

func newTestRing(t *testing.T, slots, slotSize int) (*Ring, *testWriter) {
	t.Helper()
	size, err := RingSize(slots, slotSize)
	if err != nil {
		t.Fatal(err)
	}
	mem := make([]byte, size)
	if err := InitRing(mem, slots, slotSize); err != nil {
		t.Fatal(err)
	}
	r, err := NewRing(mem, slots, slotSize)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(mem[offQPCFrequency:], 10_000_000)
	return r, &testWriter{mem: mem, slots: uint64(slots), slotSize: uint64(slotSize)}
}

func (w *testWriter) counter(off int) *uint64 { return (*uint64)(unsafe.Pointer(&w.mem[off])) }

// write publishes f; it returns false when the frame was dropped (ring full or too large).
func (w *testWriter) write(f *Frame) bool { return w.writeMangled(f, nil) }

// writeMangled is write with a hook that can corrupt the slot before it is published.
func (w *testWriter) writeMangled(f *Frame, mangle func(slot []byte)) bool {
	read := atomic.LoadUint64(w.counter(offReadCount))
	if uint64(len(f.Data)) > w.slotSize-slotHeaderSize || w.written-read == w.slots {
		w.dropped++
		w.droppedPending++
		atomic.StoreUint64(w.counter(offDropped), w.dropped)
		return false
	}
	s := w.mem[ringHeaderSize+(w.written%w.slots)*w.slotSize:]
	clear(s[:slotHeaderSize])
	le := binary.LittleEndian
	var flags uint32
	if f.Key {
		flags |= FlagKey
	}
	if f.Recovery {
		flags |= FlagRecovery
	}
	if f.Repeat {
		flags |= FlagRepeat
	}
	if w.droppedPending > 0 {
		flags |= FlagDroppedBefore
	}
	if f.Dirty >= 0 {
		flags |= FlagDirty
	}
	if f.Discardable {
		flags |= FlagDiscardable
	}
	le.PutUint64(s[slotSeq:], w.written)
	le.PutUint64(s[slotFrameID:], f.FrameID)
	le.PutUint32(s[slotFlags:], flags)
	le.PutUint32(s[slotGen:], f.Gen)
	le.PutUint32(s[slotPayloadOffset:], slotHeaderSize)
	le.PutUint32(s[slotPayloadSize:], uint32(len(f.Data)))
	le.PutUint64(s[slotPresentQPC:], uint64(f.PresentQPC))
	le.PutUint64(s[slotCaptureQPC:], uint64(f.CaptureQPC))
	le.PutUint64(s[slotSubmitQPC:], uint64(f.SubmitQPC))
	le.PutUint64(s[slotOutputQPC:], uint64(f.OutputQPC))
	le.PutUint64(s[slotRefFloor:], f.RefFloor)
	le.PutUint32(s[slotLTRSlot:], uint32(f.LTRSlot))
	le.PutUint32(s[slotTemporalLayer:], f.TemporalLayer)
	le.PutUint32(s[slotRefLTRMask:], f.RefLTRMask)
	le.PutUint32(s[slotDroppedBefore:], w.droppedPending)
	le.PutUint32(s[slotWidth:], f.Width)
	le.PutUint32(s[slotHeight:], f.Height)
	if f.Dirty >= 0 {
		le.PutUint32(s[slotDirtyPPM:], uint32(math.Round(min(1, f.Dirty)*1e6)))
	}
	copy(s[slotHeaderSize:], f.Data)
	if mangle != nil {
		mangle(s)
	}
	atomic.StoreUint64(w.counter(offWriteCount), w.written+1)
	w.written++
	w.droppedPending = 0
	return true
}

// slot returns the bytes of the slot that write index i lands in.
func (w *testWriter) slot(i uint64) []byte {
	return w.mem[ringHeaderSize+(i%w.slots)*w.slotSize:]
}

func TestRingGeometry(t *testing.T) {
	for _, c := range []struct {
		slots, slotSize int
		ok              bool
	}{
		{8, 4 << 20, true},
		{2, 64 << 10, true},
		{1, 64 << 10, false},
		{1025, 64 << 10, false},
		{8, 60 << 10, false},
		{8, 64<<10 + 1, false},
		{8, 2 << 30, false},
		{1024, 1 << 30, false}, // 1 TiB
	} {
		_, err := RingSize(c.slots, c.slotSize)
		if (err == nil) != c.ok {
			t.Errorf("RingSize(%d, %d): err=%v, want ok=%v", c.slots, c.slotSize, err, c.ok)
		}
	}
	size, _ := RingSize(DefaultSlots, DefaultSlotSize)
	if size != 4096+8*4<<20 {
		t.Fatalf("default ring size %d", size)
	}
	mem := make([]byte, size)
	if err := InitRing(mem, DefaultSlots, DefaultSlotSize); err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if string(mem[:8]) != "RECONRNG" || le.Uint32(mem[offVersion:]) != 1 || le.Uint32(mem[offHeaderSize:]) != 4096 ||
		le.Uint32(mem[offSlotCount:]) != 8 || le.Uint32(mem[offSlotSize:]) != 4<<20 ||
		le.Uint64(mem[offTotalSize:]) != uint64(size) || le.Uint32(mem[offSlotHeaderSize:]) != 128 {
		t.Fatalf("bad header % x", mem[:40])
	}
	if err := InitRing(mem[:size-1], DefaultSlots, DefaultSlotSize); err == nil {
		t.Fatal("InitRing accepted a short mapping")
	}
	if _, err := NewRing(mem[:size-4096], DefaultSlots, DefaultSlotSize); err == nil {
		t.Fatal("NewRing accepted a short mapping")
	}
}

func TestRingRoundTrip(t *testing.T) {
	r, w := newTestRing(t, 4, 64<<10)
	if f, err := r.Next(); f != nil || err != nil {
		t.Fatalf("empty ring: %v %v", f, err)
	}
	if r.QPCFrequency() != 10_000_000 {
		t.Fatalf("qpc frequency %d", r.QPCFrequency())
	}
	want := []*Frame{
		{FrameID: 1, Key: true, LTRSlot: -1, Width: 320, Height: 180, PresentQPC: 10, CaptureQPC: 11, SubmitQPC: 12, OutputQPC: 13, Data: []byte{0, 0, 0, 1, 0x65}},
		{FrameID: 2, Recovery: true, RefFloor: 1, LTRSlot: 1, TemporalLayer: 1, RefLTRMask: 2, Gen: 3, Width: 320, Height: 180, Data: bytes.Repeat([]byte{7}, 1000)},
		{FrameID: 3, LTRSlot: -1, Data: nil},
		{FrameID: 4, LTRSlot: -1, Repeat: true, Data: []byte{0, 0, 0, 1, 0x41}},
	}
	for _, f := range want {
		if !w.write(f) {
			t.Fatal("write dropped")
		}
	}
	if n, err := r.Pending(); n != 4 || err != nil {
		t.Fatalf("pending %d %v", n, err)
	}
	for i, wf := range want {
		f, err := r.Next()
		if err != nil || f == nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Data == nil {
			f.Data = []byte{}
		}
		if wf.Data == nil {
			wf.Data = []byte{}
		}
		if f.FrameID != wf.FrameID || f.Key != wf.Key || f.Recovery != wf.Recovery || f.Repeat != wf.Repeat || f.RefFloor != wf.RefFloor ||
			f.LTRSlot != wf.LTRSlot || f.TemporalLayer != wf.TemporalLayer || f.RefLTRMask != wf.RefLTRMask ||
			f.Gen != wf.Gen || f.Width != wf.Width || f.Height != wf.Height || f.PresentQPC != wf.PresentQPC ||
			f.CaptureQPC != wf.CaptureQPC || f.SubmitQPC != wf.SubmitQPC || f.OutputQPC != wf.OutputQPC ||
			!bytes.Equal(f.Data, wf.Data) || f.DroppedBefore != 0 {
			t.Fatalf("frame %d: got %+v, want %+v", i, f, wf)
		}
	}
	// The slot was released and the data copied: overwriting it does not change the frame.
	if got := atomic.LoadUint64(w.counter(offReadCount)); got != 4 {
		t.Fatalf("readCount %d", got)
	}
}

// Phase 5 slot fields: the dirty share (parts per million, valid with
// FlagDirty) and FlagDiscardable; a slot from an older helper (neither set,
// the reserved bytes 0) reads as unknown / not discardable.
func TestRingDirtyAndDiscardable(t *testing.T) {
	r, w := newTestRing(t, 4, 64<<10)
	for _, f := range []*Frame{
		{FrameID: 1, Key: true, Dirty: 1, Data: []byte{1}},
		{FrameID: 2, Dirty: 0.25, Discardable: true, TemporalLayer: 1, Data: []byte{2}},
		{FrameID: 3, Dirty: 40.0 / (1920 * 1080), Data: []byte{3}}, // a text caret: 19 ppm
		{FrameID: 4, Dirty: -1, Data: []byte{4}},
	} {
		w.write(f)
	}
	want := []struct {
		dirty       float64
		discardable bool
	}{{1, false}, {0.25, true}, {0.000019, false}, {-1, false}}
	for i, x := range want {
		f, err := r.Next()
		if err != nil || f == nil || f.Dirty != x.dirty || f.Discardable != x.discardable {
			t.Fatalf("frame %d: %+v %v, want dirty %v discardable %v", i+1, f, err, x.dirty, x.discardable)
		}
	}
	w.write(&Frame{FrameID: 5, Data: []byte{5}})
	le := binary.LittleEndian
	s := w.slot(4)
	le.PutUint32(s[slotFlags:], 0) // an older helper's slot
	le.PutUint32(s[slotDirtyPPM:], 0)
	if f, _ := r.Next(); f.Dirty != -1 || f.Discardable {
		t.Fatalf("older slot: %+v", f)
	}
}

func TestRingCopiesData(t *testing.T) {
	r, w := newTestRing(t, 2, 64<<10)
	w.write(&Frame{FrameID: 1, Data: []byte{1, 2, 3}})
	f, _ := r.Next()
	w.slot(0)[slotHeaderSize] = 9
	if f.Data[0] != 1 {
		t.Fatal("frame data aliases the ring")
	}
}

func TestRingWrapAndDrop(t *testing.T) {
	r, w := newTestRing(t, 2, 64<<10)
	id := uint64(0)
	next := func() *Frame { id++; return &Frame{FrameID: id, Data: []byte{byte(id)}} }
	for i := 0; i < 10; i++ { // wrap around several times
		w.write(next())
		f, err := r.Next()
		if err != nil || f.FrameID != id || f.Data[0] != byte(id) {
			t.Fatalf("round %d: %+v %v", i, f, err)
		}
	}
	// Fill the ring, then two more frames are dropped (the newest ones).
	if !w.write(next()) || !w.write(next()) || w.write(next()) || w.write(next()) {
		t.Fatal("expected two writes then two drops")
	}
	// Oversized frames are dropped too (frame 15).
	r.Next()
	id++
	if w.write(&Frame{FrameID: id, Data: make([]byte, 64<<10)}) {
		t.Fatal("oversized frame written")
	}
	w.write(next())
	got := []*Frame{}
	for {
		f, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			break
		}
		got = append(got, f)
	}
	if len(got) != 2 || got[0].FrameID != 12 || got[0].DroppedBefore != 0 || got[1].FrameID != 16 || got[1].DroppedBefore != 3 {
		t.Fatalf("got %+v %+v", got[0], got[1])
	}
	if r.Dropped() != 3 {
		t.Fatalf("dropped counter %d", r.Dropped())
	}
}

func TestRingCorrupt(t *testing.T) {
	le := binary.LittleEndian
	for _, c := range []struct {
		name   string
		mangle func(w *testWriter)
	}{
		{"writeCount beyond the ring", func(w *testWriter) { atomic.StoreUint64(w.counter(offWriteCount), 3) }},
		{"writeCount behind readCount", func(w *testWriter) { atomic.StoreUint64(w.counter(offWriteCount), ^uint64(0)) }},
		{"slot sequence", func(w *testWriter) { le.PutUint64(w.slot(0)[slotSeq:], 7) }},
		{"payload offset inside header", func(w *testWriter) { le.PutUint32(w.slot(0)[slotPayloadOffset:], 64) }},
		{"payload offset past slot", func(w *testWriter) { le.PutUint32(w.slot(0)[slotPayloadOffset:], 70<<10) }},
		{"payload size past slot", func(w *testWriter) { le.PutUint32(w.slot(0)[slotPayloadSize:], 64<<10-127) }},
		{"payload size wraps", func(w *testWriter) { le.PutUint32(w.slot(0)[slotPayloadSize:], ^uint32(0)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, w := newTestRing(t, 2, 64<<10)
			w.write(&Frame{FrameID: 1, Data: []byte{1}})
			c.mangle(w)
			f, err := r.Next()
			if !errors.Is(err, ErrRingCorrupt) || f != nil {
				t.Fatalf("got %v, %v; want ErrRingCorrupt", f, err)
			}
		})
	}
	// After a successful read, a writeCount that goes backwards is corrupt too.
	r, w := newTestRing(t, 2, 64<<10)
	w.write(&Frame{FrameID: 1})
	r.Next()
	atomic.StoreUint64(w.counter(offWriteCount), 0)
	if _, err := r.Pending(); !errors.Is(err, ErrRingCorrupt) {
		t.Fatalf("backwards writeCount: %v", err)
	}
	// A payload that exactly fills the slot is fine.
	r, w = newTestRing(t, 2, 64<<10)
	w.write(&Frame{FrameID: 1, Data: make([]byte, 64<<10-slotHeaderSize)})
	if f, err := r.Next(); err != nil || len(f.Data) != 64<<10-slotHeaderSize {
		t.Fatalf("full slot: %v", err)
	}
}

// FuzzRingSlot feeds arbitrary slot headers to the reader: it may reject them
// but must never panic or read outside the slot.
func FuzzRingSlot(f *testing.F) {
	f.Add(make([]byte, slotHeaderSize), uint64(1))
	seed := make([]byte, slotHeaderSize)
	binary.LittleEndian.PutUint32(seed[slotPayloadOffset:], slotHeaderSize)
	binary.LittleEndian.PutUint32(seed[slotPayloadSize:], 100)
	f.Add(seed, uint64(1))
	f.Fuzz(func(t *testing.T, hdr []byte, writeCount uint64) {
		r, w := newTestRing(t, 2, 64<<10)
		copy(w.slot(0)[:slotHeaderSize], hdr)
		atomic.StoreUint64(w.counter(offWriteCount), writeCount)
		if fr, err := r.Next(); err == nil && fr != nil && len(fr.Data) > 64<<10-slotHeaderSize {
			t.Fatalf("payload of %d bytes", len(fr.Data))
		}
	})
}
