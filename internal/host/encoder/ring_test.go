package encoder

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
)

// newTestRing returns a reader and the Go copy of the helper's producer
// (fake.go) on one ring.
func newTestRing(t *testing.T, slots, slotSize int) (*Ring, *ringWriter) {
	t.Helper()
	r, w, err := newFakeRing(slots, slotSize)
	if err != nil {
		t.Fatal(err)
	}
	return r, w
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

// TestRingSlicesAndReencoded: the Phase 5 wiring B slot fields (the parts of
// sub-frame output and when the first one was ready, at offsets 100 / 104,
// and the REENCODED flag, bit 7); an older helper's slot (zeros there) reads
// as a whole frame, an implausible part count is ignored.
func TestRingSlicesAndReencoded(t *testing.T) {
	r, w := newTestRing(t, 4, 64<<10)
	w.write(&Frame{FrameID: 1, Key: true, Dirty: -1, Slices: 4, SubmitQPC: 100, FirstSliceQPC: 130, OutputQPC: 160, Data: []byte{1}})
	w.write(&Frame{FrameID: 2, Dirty: -1, Reencoded: true, Data: []byte{2}})
	w.write(&Frame{FrameID: 3, Dirty: -1, Slices: 2, FirstSliceQPC: 7, Data: []byte{3}})
	for _, want := range []struct {
		slices    int
		first     int64
		reencoded bool
	}{{4, 130, false}, {0, 0, true}, {2, 7, false}} {
		f, err := r.Next()
		if err != nil || f.Slices != want.slices || f.FirstSliceQPC != want.first || f.Reencoded != want.reencoded {
			t.Fatalf("frame %+v %v, want slices %d first %d reencoded %v", f, err, want.slices, want.first, want.reencoded)
		}
	}
	le := binary.LittleEndian
	w.write(&Frame{FrameID: 4, Dirty: -1, Slices: 3, FirstSliceQPC: 9, Data: []byte{4}})
	le.PutUint32(w.slot(3)[slotSlices:], 0) // an older helper's slot: reserved bytes 0
	le.PutUint64(w.slot(3)[slotFirstSliceQPC:], 0)
	w.write(&Frame{FrameID: 5, Dirty: -1, Slices: 3, FirstSliceQPC: 9, Data: []byte{5}})
	le.PutUint32(w.slot(4)[slotSlices:], 1<<20)
	for _, id := range []uint64{4, 5} {
		if f, err := r.Next(); err != nil || f.FrameID != id || f.Slices != 0 || f.FirstSliceQPC != 0 || f.Reencoded {
			t.Fatalf("frame %d: %+v %v, want a whole frame", id, f, err)
		}
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
	// One of the three was too large for a slot: the flag says so.
	if got[0].DroppedTooLarge || !got[1].DroppedTooLarge {
		t.Fatalf("DroppedTooLarge %v %v, want false true", got[0].DroppedTooLarge, got[1].DroppedTooLarge)
	}
	if r.Dropped() != 3 {
		t.Fatalf("dropped counter %d", r.Dropped())
	}
	// Drops of a full ring alone leave it unset, and it is cleared with the
	// next written frame.
	w.write(next()) // 17
	w.write(next()) // 18
	w.write(next()) // 19: ring full
	r.Next()
	r.Next()
	w.write(next()) // 20
	w.write(next()) // 21
	f20, _ := r.Next()
	f21, _ := r.Next()
	if f20 == nil || f20.DroppedBefore != 1 || f20.DroppedTooLarge || f21 == nil || f21.DroppedBefore != 0 || f21.DroppedTooLarge {
		t.Fatalf("after a full ring: %+v %+v", f20, f21)
	}
}

// Slots hold an uncompressed picture of the largest stream (a key frame of a
// high-bitrate 4K stream does not fit the default 4 MiB).
func TestSlotSizeFor(t *testing.T) {
	sdr4k, hdr4k := SlotSizeFor(3840, 2160, false), SlotSizeFor(3840, 2160, true)
	for _, c := range []struct {
		name         string
		w, h         int
		tenBit       bool
		minPayload   int
		want, atMost int
	}{
		{"1080p keeps the default", 1920, 1080, false, 1920 * 1080 * 3 / 2, DefaultSlotSize, DefaultSlotSize},
		{"1080p HDR10 too", 1920, 1080, true, 1920 * 1080 * 15 / 8, DefaultSlotSize, DefaultSlotSize},
		{"1440p", 2560, 1440, false, 2560 * 1440 * 3 / 2, 0, 8 << 20},
		// 10 average frames at 250 Mbit/s and 60 fps (5.2 MB): the default
		// holds only 8.
		{"4K", 3840, 2160, false, 3840 * 2160 * 3 / 2, 0, 16 << 20},
		{"4K HDR10", 3840, 2160, true, 3840 * 2160 * 15 / 8, 0, 20 << 20},
		{"unknown size: 4K", 0, 0, false, 0, sdr4k, sdr4k},
		{"8K HDR10", 7680, 4320, true, 7680 * 4320 * 15 / 8, 0, 64 << 20},
		{"larger pictures are capped", 15360, 8640, false, 0, 64 << 20, 64 << 20},
	} {
		got := SlotSizeFor(c.w, c.h, c.tenBit)
		if got%4096 != 0 || got-slotHeaderSize < c.minPayload || got > c.atMost || (c.want != 0 && got != c.want) {
			t.Errorf("%s: SlotSizeFor(%d, %d, %v) = %d", c.name, c.w, c.h, c.tenBit, got)
		}
		if _, err := RingSize(DefaultSlots, got); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if sdr4k-slotHeaderSize < 10*250_000_000/8/60 || hdr4k <= sdr4k {
		t.Errorf("4K slots %d (HDR10 %d)", sdr4k, hdr4k)
	}
}

func TestRingCorrupt(t *testing.T) {
	le := binary.LittleEndian
	for _, c := range []struct {
		name   string
		mangle func(w *ringWriter)
	}{
		{"writeCount beyond the ring", func(w *ringWriter) { atomic.StoreUint64(w.counter(offWriteCount), 3) }},
		{"writeCount behind readCount", func(w *ringWriter) { atomic.StoreUint64(w.counter(offWriteCount), ^uint64(0)) }},
		{"slot sequence", func(w *ringWriter) { le.PutUint64(w.slot(0)[slotSeq:], 7) }},
		{"payload offset inside header", func(w *ringWriter) { le.PutUint32(w.slot(0)[slotPayloadOffset:], 64) }},
		{"payload offset past slot", func(w *ringWriter) { le.PutUint32(w.slot(0)[slotPayloadOffset:], 70<<10) }},
		{"payload size past slot", func(w *ringWriter) { le.PutUint32(w.slot(0)[slotPayloadSize:], 64<<10-127) }},
		{"payload size wraps", func(w *ringWriter) { le.PutUint32(w.slot(0)[slotPayloadSize:], ^uint32(0)) }},
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
