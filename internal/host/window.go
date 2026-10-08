package host

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// The video window (GUIDE 2.7, send priorities). Audio, cursor and pong
// datagrams share the path with the video frames: quic-go packs a queued
// datagram ahead of stream data into its next packet, but that packet waits
// for congestion-window space and then queues behind every video byte already
// in the network. The media congestion controller's window, pacing rate x
// (min RTT + 2 frame intervals), bounds that queue to two frames only while
// the path carries the pacing rate; when the path's capacity drops below it
// (before the rate controller has lowered the bitrate) the window holds
// pacing / capacity times as long a queue (~100 ms when 20 Mbit/s of video
// meet 10 Mbit/s over a 10 ms round trip: TestDatagramLatencyBehindVideo).
// frameSender therefore holds the next frame while videoInFlight frames are
// in flight beyond those in transit for the round trip (windowLimit), as the
// controller's acknowledgements measure them; the backlog stays in the
// host's frame queue.
//
// "In flight": a frame whose stream write returned (quic-go returns once all
// but its last packet's worth has been packed and sent) and whose bytes up
// to that point the peer has not all acknowledged (cc.Media.Delivery: the
// connection's sent and acknowledged-or-lost positions, in order of sending).
// "In transit": in flight and sent within the last round trip (roundTrip),
// at most as many as the frame rate sends in it, rounded up: the frame just
// sent on a LAN, three at 60 fps over 40 ms. Counting the frames actually
// sent, not the frame rate's, keeps the allowance at the path's own rate
// when a capacity drop spaces the frames out. So on a LAN at most two frames
// are in flight (the frame on the wire and the next one), and a frame waits
// for the one two before it to be acknowledged.
//
// The window never makes a frame late: it holds a frame at most until a
// quarter of its deadline (ladder.go frameDeadline) is left, and then the
// frame goes to the transport as it did before the window, where the
// congestion window and rung 1 of the loss-recovery ladder deal with it. A
// receiver that acknowledges late (a busy browser) costs at most the
// deadline's worth of host queue on a frame, never a cancelled one. Paths
// without the media controller (congestion "reno") have no window: there the
// sequential frameSender's one frame in the transport is the only cap, as
// before.

const (
	// videoInFlight is how many frames may be in flight, the next one
	// included, beyond those in transit for the round trip. Measured
	// (TestDatagramLatencyBehindVideo, 20 Mbit/s into 10 Mbit/s over 10 ms):
	// audio datagrams' one-way delay p50 ~105 ms without the window, ~40 ms
	// with 1, ~73 ms with 2 (a hold then ends at its bound more often than
	// at an acknowledgement).
	videoInFlight = 1
	// windowLead: a held frame goes to the transport this share of its
	// deadline before the deadline at the latest, so rung 1 finds it written
	// or writing even when frameSender is scheduled late (a loaded machine).
	windowLead = 4 // a quarter
	// maxWindowHold bounds the hold of a frame with a long deadline (a key
	// frame larger than the pacer sends in a few frame intervals).
	maxWindowHold = 250 * time.Millisecond
	// windowPoll is how often a held frame looks again without an ACK: for
	// a path migration (a new controller, whose progress it does not hear).
	windowPoll = 5 * time.Millisecond
)

// deliveryMeter is what the window reads of the connection
// (transport.MediaControl's *cc.Media).
type deliveryMeter interface {
	// Delivery returns the ack-eliciting bytes sent and how many of them
	// have left the network (acknowledged or declared lost), cumulative.
	Delivery() (sent, done uint64)
	// Progress is signalled when done grows.
	Progress() <-chan struct{}
	MinRTT() time.Duration
}

// videoWindow is frameSender's record of the frames in flight.
type videoWindow struct {
	mu    sync.Mutex
	meter deliveryMeter // the controller the marks are positions of
	marks []windowMark  // the frames in flight, oldest first

	// For stream stats: frames held, and the longest hold (µs), since the
	// last report.
	held    atomic.Int64
	maxHold atomic.Int64
}

// windowMark is a frame in flight: the connection's sent position and the
// time when its write returned.
type windowMark struct {
	pos uint64
	at  time.Time
}

// roundTrip is how long a frame stays unacknowledged without any queue: the
// min RTT, a quarter added for the peer's delayed acknowledgements (Chrome
// acknowledges every 10th packet or after a quarter of the min RTT, quic-go
// every 2nd: a frame's last packet, which its mark leaves out anyway).
func roundTrip(minRTT time.Duration) time.Duration { return minRTT + minRTT/4 }

// windowLimit is how many frames may be in flight, the next one included,
// on a path of the given min RTT at the given frame interval, recent of them
// sent within the last round trip: videoInFlight plus those in transit (the
// recent ones, at most as many as the frame rate sends in a round trip,
// rounded up).
func windowLimit(minRTT, interval time.Duration, recent int) int {
	if interval <= 0 {
		interval = time.Second / 60
	}
	rt := roundTrip(minRTT)
	return videoInFlight + min(recent, int((rt+interval-1)/interval))
}

// inFlight forgets the recorded frames m has delivered and returns how many
// remain, and how many of those were sent since recentFrom. Marks of another
// controller (a path migration replaced it) are dropped: its positions start
// over. Called with w.mu held.
func (w *videoWindow) inFlight(m deliveryMeter, recentFrom time.Time) (n, recent int) {
	if m != w.meter {
		w.meter, w.marks = m, w.marks[:0]
		return 0, 0
	}
	_, done := m.Delivery()
	keep := w.marks[:0]
	for _, k := range w.marks {
		if k.pos > done {
			keep = append(keep, k)
			if k.at.After(recentFrom) {
				recent++
			}
		}
	}
	w.marks = keep
	return len(keep), recent
}

// room reports whether another frame may go out at now on m's path.
func (w *videoWindow) room(m deliveryMeter, interval time.Duration, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	rtt := m.MinRTT()
	n, recent := w.inFlight(m, now.Add(-roundTrip(rtt)))
	return n < windowLimit(rtt, interval, recent)
}

// sent records a frame whose write returned at now (its data handed to m's
// path).
func (w *videoWindow) sent(m deliveryMeter, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight(m, now) // switches to m if the controller changed
	pos, _ := m.Delivery()
	w.marks = append(w.marks, windowMark{pos, now})
}

// noteHold records how long a frame was held.
func (w *videoWindow) noteHold(d time.Duration) {
	w.held.Add(1)
	us := d.Microseconds()
	for {
		m := w.maxHold.Load()
		if us <= m || w.maxHold.CompareAndSwap(m, us) {
			return
		}
	}
}

// stats returns and resets the frames held and the longest hold.
func (w *videoWindow) stats() (held int64, maxHold time.Duration) {
	return w.held.Swap(0), time.Duration(w.maxHold.Swap(0)) * time.Microsecond
}

// deliveryMeter returns the connection's meter (nil: none, no window; also
// with the test hook's no-window).
func (s *Session) deliveryMeter() deliveryMeter {
	if s.meter != nil {
		return s.meter()
	}
	if s.a.faults.noWindow {
		return nil
	}
	if m := transport.MediaControl(s.c); m != nil {
		return m
	}
	return nil
}

// frameInterval is the video's frame interval as the congestion target has it.
func (s *Session) frameInterval() time.Duration {
	if t := s.ccTarget.Load(); t != nil {
		return t.frameInterval
	}
	return time.Second / 60
}

// admit holds frame stream of (opened, nothing written yet) until the window
// has room for it, and returns how long it waited. It returns early when the
// ladder discarded the frame meanwhile (the client waits for the answer to a
// loss before it: of.gone, once checkOut recorded it), when the session ends,
// and a quarter of the frame's deadline before it (at most maxWindowHold).
func (s *Session) admit(of *outFrame) time.Duration {
	var start time.Time
	var timer *time.Timer
	until := of.opened.Add(min(of.deadline-of.deadline/windowLead, maxWindowHold))
hold:
	for {
		m := s.deliveryMeter()
		if m == nil || s.win.room(m, s.frameInterval(), time.Now()) || s.ctx.Err() != nil || !time.Now().Before(until) {
			break
		}
		select {
		case <-of.gone:
			break hold
		default:
		}
		wait := min(windowPoll, time.Until(until))
		if start.IsZero() {
			start = time.Now()
			s.windowSince.Store(start.UnixNano())
			timer = time.NewTimer(wait)
			defer timer.Stop()
		} else {
			timer.Reset(wait)
		}
		select {
		case <-s.ctx.Done():
		case <-of.gone:
		case <-m.Progress():
		case <-timer.C:
		}
	}
	if start.IsZero() {
		return 0
	}
	s.windowSince.Store(0)
	d := time.Since(start)
	s.win.noteHold(d)
	return d
}
