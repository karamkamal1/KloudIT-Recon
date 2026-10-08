package encoder

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Test double of recon-encoder.exe (tests only, here and in the packages that
// use a Helper): Fake plays the helper in process over pipes and a Go copy of
// the ring producer, so code that drives a Helper runs on any OS.

// ringWriter is a Go copy of the producer in native/recon-encoder/src/ring.cpp
// (RingWriter::write).
type ringWriter struct {
	mem            []byte
	slots          uint64
	slotSize       uint64
	written        uint64
	dropped        uint64
	droppedPending uint32
}

// newFakeRing returns a reader and the Go producer on one in-memory ring, as
// the helper leaves it after attaching (QPC frequency 10 MHz).
func newFakeRing(slots, slotSize int) (*Ring, *ringWriter, error) {
	size, err := RingSize(slots, slotSize)
	if err != nil {
		return nil, nil, err
	}
	// uint64s: the counters need 8-byte alignment.
	mem := unsafe.Slice((*byte)(unsafe.Pointer(&make([]uint64, (size+7)/8)[0])), size)
	if err := InitRing(mem, slots, slotSize); err != nil {
		return nil, nil, err
	}
	r, err := NewRing(mem, slots, slotSize)
	if err != nil {
		return nil, nil, err
	}
	binary.LittleEndian.PutUint64(mem[offQPCFrequency:], FakeQPCFrequency)
	return r, &ringWriter{mem: mem, slots: uint64(slots), slotSize: uint64(slotSize)}, nil
}

func (w *ringWriter) counter(off int) *uint64 { return (*uint64)(unsafe.Pointer(&w.mem[off])) }

// write publishes f; it returns false when the frame was dropped (ring full or too large).
func (w *ringWriter) write(f *Frame) bool { return w.writeMangled(f, nil) }

// writeMangled is write with a hook that can corrupt the slot before it is published.
func (w *ringWriter) writeMangled(f *Frame, mangle func(slot []byte)) bool {
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
	if f.SeqStart && f.Key {
		flags |= FlagSeqStart
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
func (w *ringWriter) slot(i uint64) []byte {
	return w.mem[ringHeaderSize+(i%w.slots)*w.slotSize:]
}

// FakeQPCFrequency is the QPC frequency of a Fake's ring; its caps must say
// the same.
const FakeQPCFrequency = 10_000_000

// FakeHandler sees every message the Go side sends a Fake (decoded JSON),
// except shutdown, before Fake.Messages delivers it.
type FakeHandler func(f *Fake, msg map[string]any)

// Fake plays recon-encoder.exe for a Helper: it sends caps, answers through
// its handler and publishes frames into the ring (tests only).
type Fake struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter

	ringMu sync.Mutex
	ring   *ringWriter
	event  chan struct{}

	exitOnce sync.Once
	exited   chan struct{}
	code     int
	released chan struct{}

	msgs chan map[string]any

	errMu sync.Mutex
	err   error
}

// LaunchFake starts a Fake that announces caps (a "caps" message, e.g.
// `{"t":"caps","v":1,"backend":"mock",...,"qpcFrequency":10000000}`) and
// returns the Helper talking to it, as Launch does for the real helper. The
// ring has 4 slots of 64 KiB; caps and start time out after 2 s.
func LaunchFake(caps string, handle FakeHandler) (*Helper, *Fake, error) {
	r, w, err := newFakeRing(4, 64<<10)
	if err != nil {
		return nil, nil, err
	}
	f := &Fake{ring: w, event: make(chan struct{}, 1), exited: make(chan struct{}),
		released: make(chan struct{}), msgs: make(chan map[string]any, 256)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go f.run(caps, handle)
	h, err := newHelper(Options{CapsTimeout: 2 * time.Second, StartTimeout: 2 * time.Second}.withDefaults(), conn{
		ctrlW: f.stdinW,
		ctrlR: f.stdoutR,
		ring:  r,
		wait: func(d time.Duration) error {
			select {
			case <-f.event:
			case <-time.After(d):
			}
			return nil
		},
		kill:     func() error { f.Exit(1); return nil },
		exited:   f.exited,
		exitCode: func() int { return f.code },
		release:  func() { close(f.released) },
	})
	return h, f, err
}

// Send sends a message to the Go side: Started, HelperError, Stats and
// CaptureChanged values get their "t"; anything else is sent as it marshals.
func (f *Fake) Send(v any) {
	b, _ := json.Marshal(v)
	// Tag Go structs with their message type, as the helper does.
	t := ""
	switch v.(type) {
	case Started:
		t = "started"
	case HelperError:
		t = "error"
	case Stats:
		t = "stats"
	case CaptureChanged:
		t = "captureChanged"
	}
	if t != "" {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["t"] = t
		b, _ = json.Marshal(m)
	}
	_ = proto.WriteMsg(f.stdoutW, b)
}

// Exit ends the fake helper process with an exit code (once).
func (f *Fake) Exit(code int) {
	f.exitOnce.Do(func() {
		f.code = code
		f.stdoutW.Close()
		f.stdinR.Close()
		close(f.exited)
	})
}

// Publish writes a frame into the ring and signals it; false: dropped (ring
// full or too large), so the next published frame has DroppedBefore > 0.
// Frame.Key and Frame.SeqStart become the slot flags (SeqStart only with Key).
func (f *Fake) Publish(fr *Frame) bool { return f.PublishMangled(fr, nil) }

// PublishMangled is Publish with a hook that can corrupt the slot.
func (f *Fake) PublishMangled(fr *Frame, mangle func(slot []byte)) bool {
	f.ringMu.Lock()
	ok := f.ring.writeMangled(fr, mangle)
	f.ringMu.Unlock()
	select {
	case f.event <- struct{}{}:
	default:
	}
	return ok
}

// Messages delivers the messages the Go side sent (after the handler saw them).
func (f *Fake) Messages() <-chan map[string]any { return f.msgs }

// Exited is closed when the fake helper process has exited.
func (f *Fake) Exited() <-chan struct{} { return f.exited }

// Code is the exit code, valid once Exited is closed.
func (f *Fake) Code() int { return f.code }

// Released is closed when the Helper freed the ring.
func (f *Fake) Released() <-chan struct{} { return f.released }

// Err reports a malformed message from the Go side, if any.
func (f *Fake) Err() error {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	return f.err
}

func (f *Fake) run(caps string, handle FakeHandler) {
	f.Send(json.RawMessage(caps))
	for {
		b, err := proto.ReadMsg(f.stdinR, MaxControlMsg)
		if err != nil {
			f.Exit(0) // stdin closed: exit like the real helper
			return
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			f.errMu.Lock()
			f.err = errors.Join(f.err, fmt.Errorf("fake helper: bad message %q", b))
			f.errMu.Unlock()
			continue
		}
		if m["t"] == "shutdown" {
			f.Exit(0)
			return
		}
		if handle != nil {
			handle(f, m)
		}
		select {
		case f.msgs <- m:
		default: // nobody reads them
		}
	}
}
