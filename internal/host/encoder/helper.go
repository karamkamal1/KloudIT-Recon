package encoder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// ErrNotSupported is returned by Launch on platforms without the helper.
var ErrNotSupported = errors.New("encoder helper: not supported on this platform")

// ErrClosed is returned by calls on a helper after Close.
var ErrClosed = errors.New("encoder helper: closed")

// ExitError reports that the helper process ended without being asked to.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("encoder helper exited with code %d", e.Code) }

// Options configures Launch.
type Options struct {
	Exe          string        // path to recon-encoder.exe (absolute: next to recon-host.exe)
	Backend      string        // auto (default) | amf | nvenc | mock
	Slots        int           // ring slots (default DefaultSlots)
	SlotSize     int           // bytes per slot, header included (default DefaultSlotSize)
	LogLevel     string        // helper log level: error | warn | info (default) | debug
	Args         []string      // extra helper arguments (tests: --mock-fatal-at=N)
	Log          *slog.Logger  // receives the helper's stderr; nil discards it
	CapsTimeout  time.Duration // how long Launch waits for caps (default 10 s)
	StartTimeout time.Duration // how long Start waits for "started" (default 10 s)
}

func (o Options) withDefaults() Options {
	if o.Backend == "" {
		o.Backend = "auto"
	}
	if o.Slots == 0 {
		o.Slots = DefaultSlots
	}
	if o.SlotSize == 0 {
		o.SlotSize = DefaultSlotSize
	}
	if o.LogLevel == "" {
		o.LogLevel = "info"
	}
	if o.CapsTimeout == 0 {
		o.CapsTimeout = 10 * time.Second
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = 10 * time.Second
	}
	return o
}

// conn is a running helper as seen by the portable code: its control pipes,
// the mapped ring, the frame-ready event and the process. helper_windows.go
// builds it from a real process; tests build it from pipes and a goroutine.
type conn struct {
	ctrlW    io.WriteCloser // helper stdin
	ctrlR    io.ReadCloser  // helper stdout
	logR     io.ReadCloser  // helper stderr (nil: none)
	ring     *Ring
	wait     func(d time.Duration) error // waits for the frame-ready event or d
	kill     func() error
	exited   <-chan struct{} // closed when the process has exited
	exitCode func() int      // valid once exited is closed
	release  func()          // frees the mapping and handles; called once nothing uses them
}

// Helper is a running recon-encoder process.
type Helper struct {
	opt  Options
	log  *slog.Logger
	c    conn
	caps Caps

	writeMu sync.Mutex

	mu      sync.Mutex
	startCh chan startResult // pending Start
	err     error            // why the helper ended (first fatal error / exit)

	frames   chan *Frame
	stats    chan Stats
	errs     chan error
	ctrlDone chan struct{} // closed when the control reader ended
	done     chan struct{} // closed when the process has exited and its messages were read
	quit     chan struct{} // closed by Close
	wg       sync.WaitGroup

	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

type startResult struct {
	started Started
	err     error
}

// newHelper takes ownership of c, starts the reader goroutines and waits for
// the caps message.
func newHelper(opt Options, c conn) (*Helper, error) {
	h := &Helper{
		opt:      opt,
		log:      opt.Log,
		c:        c,
		frames:   make(chan *Frame, 4),
		stats:    make(chan Stats, 64),
		errs:     make(chan error, 16),
		ctrlDone: make(chan struct{}),
		done:     make(chan struct{}),
		quit:     make(chan struct{}),
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	capsCh := make(chan *Caps, 1)
	h.wg.Add(2)
	go h.controlLoop(capsCh)
	go h.waitLoop()
	if c.logR != nil {
		h.wg.Add(1)
		go h.logLoop()
	}

	var caps *Caps
	select {
	case caps = <-capsCh:
	case <-h.done:
	case <-time.After(opt.CapsTimeout):
	}
	if caps == nil {
		err := h.Err()
		if err == nil {
			err = errors.New("encoder helper: no capabilities message")
		}
		h.Close()
		return nil, err
	}
	if caps.V != ProtocolVersion {
		h.Close()
		return nil, fmt.Errorf("encoder helper speaks protocol %d, want %d", caps.V, ProtocolVersion)
	}
	if caps.QPCFrequency <= 0 || c.ring.QPCFrequency() != caps.QPCFrequency {
		h.Close()
		return nil, errors.New("encoder helper: did not attach the frame ring")
	}
	h.caps = *caps
	h.wg.Add(1)
	go h.ringLoop()
	return h, nil
}

// Caps returns the capabilities the helper reported at start-up.
func (h *Helper) Caps() Caps { return h.caps }

// QPCFrequency converts the QPC timestamps in Frame and Stats to seconds.
func (h *Helper) QPCFrequency() int64 { return h.caps.QPCFrequency }

// Frames delivers encoded frames in order. Each frame was copied out of the
// ring, so the helper can reuse the slot at once. If the receiver falls
// behind, the ring fills and the helper drops frames (never blocks the
// encoder); the next delivered frame has DroppedBefore > 0. Closed when the
// helper is gone and every published frame was delivered, or on Close.
func (h *Helper) Frames() <-chan *Frame { return h.frames }

// Stats delivers the per-frame reports, including dropped frames. Best
// effort: reports are discarded while nobody reads them.
func (h *Helper) Stats() <-chan Stats { return h.stats }

// Errors delivers *HelperError values (and transport errors), except a
// non-fatal error answering Start, which Start returns. A fatal error is
// followed by the helper exiting (Done). Best effort, like Stats; Err keeps
// the reason the helper ended.
func (h *Helper) Errors() <-chan error { return h.errs }

// Done is closed when the helper process has exited.
func (h *Helper) Done() <-chan struct{} { return h.done }

// Err returns why the helper ended: its fatal error, an *ExitError, or nil
// after a clean Close.
func (h *Helper) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *Helper) setErr(err error) {
	h.mu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.mu.Unlock()
}

func (h *Helper) send(v any) error {
	if h.closing.Load() {
		return ErrClosed
	}
	select {
	case <-h.done:
		if err := h.Err(); err != nil {
			return err
		}
		return ErrClosed
	default:
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return proto.WriteMsg(h.c.ctrlW, b)
}

// Start starts capture and encoding and waits for the helper's answer.
func (h *Helper) Start(p StartParams) (Started, error) {
	ch := make(chan startResult, 1)
	h.mu.Lock()
	if h.startCh != nil {
		h.mu.Unlock()
		return Started{}, errors.New("encoder helper: Start already in progress")
	}
	h.startCh = ch
	h.mu.Unlock()
	clear := func() {
		h.mu.Lock()
		if h.startCh == ch {
			h.startCh = nil
		}
		h.mu.Unlock()
	}
	if err := h.send(startMsg{T: "start", StartParams: p}); err != nil {
		clear()
		return Started{}, err
	}
	select {
	case r := <-ch:
		return r.started, r.err
	case <-h.done:
		clear()
		if err := h.Err(); err != nil {
			return Started{}, err
		}
		return Started{}, ErrClosed
	case <-time.After(h.opt.StartTimeout):
		clear()
		return Started{}, errors.New("encoder helper: start timed out")
	}
}

// ForceIDR makes the next frame an IDR / key frame.
func (h *Helper) ForceIDR() error { return h.send(simpleMsg{T: "forceIdr"}) }

// Recover reports that frames from lostFrom on were lost. ackedLTR is the
// newest acknowledged long-term reference frame, if any. Backends without a
// recovery mechanism (Caps recovery "none") send an IDR.
func (h *Helper) Recover(lostFrom uint64, ackedLTR *uint64) error {
	return h.send(recoverMsg{T: "recover", LostFromFrameID: lostFrom, AckedLTRFrameID: ackedLTR})
}

// SetRate changes the target bitrate (and optionally the VBV size in frame
// intervals and the frame rate; 0 = unchanged). How seamless this is depends
// on the codec's Caps liveBitrate.
func (h *Helper) SetRate(kbps int, vbvFrames float64, fps int) error {
	return h.send(setRateMsg{T: "setRate", Kbps: kbps, VBVFrames: vbvFrames, FPS: fps})
}

// SetROI replaces the regions of interest (nil clears them).
func (h *Helper) SetROI(rects []ROIRect) error {
	if rects == nil {
		rects = []ROIRect{}
	}
	return h.send(setROIMsg{T: "setRoi", Rects: rects})
}

// Close asks the helper to exit, kills it if it does not within two seconds,
// and frees the ring. It returns the helper's terminal error, if it ended on
// its own before Close.
func (h *Helper) Close() error {
	h.closeOnce.Do(func() {
		select {
		case <-h.done:
			h.closeErr = h.Err() // already gone: report why
		default:
		}
		h.closing.Store(true)
		h.writeMu.Lock()
		_ = proto.WriteMsg(h.c.ctrlW, []byte(`{"t":"shutdown"}`))
		_ = h.c.ctrlW.Close()
		h.writeMu.Unlock()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			h.log.Warn("encoder helper did not exit, killing it")
			_ = h.c.kill()
			<-h.done
		}
		close(h.quit)
		_ = h.c.ctrlR.Close()
		if h.c.logR != nil {
			_ = h.c.logR.Close()
		}
		h.wg.Wait()
		if h.c.release != nil {
			h.c.release()
		}
	})
	return h.closeErr
}

func (h *Helper) pushErr(err error) {
	select {
	case h.errs <- err:
	default:
		h.log.Warn("encoder helper error dropped (nobody reading)", "err", err)
	}
}

// fail ends the helper because of something it did (bad message, corrupt ring).
func (h *Helper) fail(err error) {
	h.setErr(err)
	h.pushErr(err)
	_ = h.c.kill()
}

func (h *Helper) controlLoop(capsCh chan<- *Caps) {
	defer h.wg.Done()
	defer close(h.ctrlDone)
	gotCaps := false
	for {
		b, err := proto.ReadMsg(h.c.ctrlR, MaxControlMsg)
		if err != nil {
			if !errors.Is(err, io.EOF) && !h.closing.Load() {
				// A torn message usually means the process died: then the
				// exit (recorded by waitLoop) is the better explanation.
				select {
				case <-h.c.exited:
				case <-time.After(time.Second):
					h.fail(fmt.Errorf("encoder helper: control channel: %w", err))
				}
			}
			return
		}
		m, err := decodeMessage(b)
		if errors.Is(err, errUnknownMessage) {
			h.log.Debug("encoder helper: ignoring message", "err", err)
			continue
		}
		if err != nil {
			h.fail(err)
			return
		}
		switch m := m.(type) {
		case *Caps:
			if !gotCaps {
				gotCaps = true
				capsCh <- m
			}
		case *Started:
			h.replyStart(startResult{started: *m})
		case *Stats:
			select {
			case h.stats <- *m:
			default:
			}
		case *HelperError:
			if m.Fatal {
				h.setErr(m)
				h.replyStart(startResult{err: m})
			} else if m.Re == "start" && h.replyStart(startResult{err: m}) {
				continue // Start returns it
			}
			h.pushErr(m)
		}
	}
}

// replyStart hands r to a pending Start and reports whether there was one.
func (h *Helper) replyStart(r startResult) bool {
	h.mu.Lock()
	ch := h.startCh
	h.startCh = nil
	h.mu.Unlock()
	if ch != nil {
		ch <- r
	}
	return ch != nil
}

func (h *Helper) waitLoop() {
	defer h.wg.Done()
	<-h.c.exited
	// Read what the helper said before it went (its fatal error explains the
	// exit better than the exit code). stdout reaches EOF when it exits.
	select {
	case <-h.ctrlDone:
	case <-time.After(2 * time.Second):
	}
	if code := h.c.exitCode(); !h.closing.Load() || code != 0 {
		h.setErr(&ExitError{Code: code})
	}
	close(h.done)
}

// ringLoop moves frames from the ring to the Frames channel.
func (h *Helper) ringLoop() {
	defer h.wg.Done()
	defer close(h.frames)
	for {
		exited := false
		select {
		case <-h.quit:
			return
		case <-h.done:
			exited = true // deliver what was published, then stop
		default:
			if err := h.c.wait(100 * time.Millisecond); err != nil {
				h.fail(fmt.Errorf("encoder helper: waiting for frames: %w", err))
				return
			}
		}
		for {
			f, err := h.c.ring.Next()
			if err != nil {
				h.fail(err)
				return
			}
			if f == nil {
				break
			}
			select {
			case h.frames <- f:
			case <-h.quit:
				return
			}
		}
		if exited {
			return
		}
	}
}

// logLoop forwards the helper's stderr ("level: message" lines) to the log.
func (h *Helper) logLoop() {
	defer h.wg.Done()
	sc := bufio.NewScanner(h.c.logR)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		level, msg := slog.LevelInfo, line
		if lv, rest, ok := strings.Cut(line, ": "); ok {
			switch lv {
			case "error":
				level, msg = slog.LevelError, rest
			case "warn":
				level, msg = slog.LevelWarn, rest
			case "info":
				level, msg = slog.LevelInfo, rest
			case "debug":
				level, msg = slog.LevelDebug, rest
			}
		}
		h.log.Log(context.Background(), level, "encoder helper: "+msg)
	}
	// Keep draining after a scanner error (e.g. an overlong line) so the
	// helper never blocks on a full stderr pipe.
	_, _ = io.Copy(io.Discard, h.c.logR)
}
