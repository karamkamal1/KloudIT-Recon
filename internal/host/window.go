package host

import (
	"math"
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
// frameSender therefore holds the next frame while the path falls short of
// the pacing rate (shortfall) and videoInFlight frames are in flight beyond
// those in transit for the round trip (windowLimit), as the controller's
// acknowledgements measure them; the backlog stays in the host's frame
// queue.
//
// "Falls short": the path took markedly longer (shortfallRatio) to deliver
// the recent frames' bytes than the sender took to send them, from the
// acknowledgement of a frame's first byte to that of its mark (below; the
// controller's DeliveredAt). Only a bottleneck slower than the sender
// stretches that: a longer or jittery round trip delays both ends alike, so
// a path that carries the video is never held back, whatever its round trip
// does (a relay fallback, Wi-Fi), and a capacity drop is seen within about
// three frames. Without that evidence frames go out as before the window.
// The sender's time is the pacer's at its rate, or the write's own where
// that was longer: on a host whose CPU is busy (a game) quic-go's send loop
// runs late, the frame's bytes leave late and arrive as late, and no queue
// builds that a hold would keep datagrams out of; holding would only add the
// wait for an acknowledgement. Time the congestion window held the write
// back is not the sender's own: that is the path's (a backlog the window
// should prevent), so it never hides a shortfall.
//
// "In flight": a frame whose stream write returned (quic-go returns once all
// but its last packet's worth has been packed and sent) and whose bytes up
// to that point (its mark) the peer has not all acknowledged
// (cc.Media.Delivery: the connection's sent and acknowledged-or-lost
// positions, in order of sending). "In transit": in flight and sent within
// the last round trip (roundTrip, from the path's recent min RTT: the
// smallest of the last 1.5-2 s, which follows a round trip that grows; kept
// from before a shortfall while it lasts, as the window's own backlog is in
// every round trip then), at most as many as the frame rate sends in it,
// rounded up: the frame just sent on a LAN, three at 60 fps over 40 ms. Counting the frames actually sent, not
// the frame rate's, keeps the allowance at the path's own rate when a
// capacity drop spaces the frames out. So on a LAN at most two frames are in
// flight (the frame on the wire and the next one), and a frame waits for the
// one two before it to be acknowledged.
//
// The hold is host queue, not part of the frame's transport time: the
// frame's deadline (ladder.go frameDeadline, rung 1) starts when the window
// releases it (sendState.start), so the window never makes rung 1 cancel a
// frame. It holds a frame at most until three quarters of its deadline have
// passed since its stream opened (at most maxWindowHold), which bounds the
// latency it adds to one frame when acknowledgements come late (a busy
// browser). Paths without the media controller (congestion "reno") have no
// window: there the sequential frameSender's one frame in the transport is
// the only cap, as before.

const (
	// videoInFlight is how many frames may be in flight, the next one
	// included, beyond those in transit for the round trip. Measured
	// (TestDatagramLatencyBehindVideo, 20 Mbit/s into 10 Mbit/s over 10 ms,
	// a window holding from the first frame on): audio datagrams' one-way
	// delay p50 ~105 ms without the window, ~40 ms with 1, ~73 ms with 2 (a
	// hold then ends at its bound more often than at an acknowledgement).
	// With the shortfall gate ~50 ms: the frames that go out before the
	// evidence is in leave one more in flight.
	videoInFlight = 1
	// windowLead: a held frame goes out at the latest when this share of its
	// deadline is left (counted from its stream's opening).
	windowLead = 4 // a quarter
	// maxWindowHold bounds the hold of a frame with a long deadline (a key
	// frame larger than the pacer sends in a few frame intervals).
	maxWindowHold = 250 * time.Millisecond
	// windowPoll is how often a held frame looks again without an ACK: for
	// a path migration (a new controller, whose progress it does not hear).
	windowPoll = 5 * time.Millisecond

	// shortfallFrames: the evidence of a shortfall is the last this many
	// frames' delivery (paceSample).
	shortfallFrames = 8
	// shortfallRatio: the path falls short when it took this many times as
	// long to deliver those frames' bytes as the pacer took to send them: a
	// capacity under 2/3 of the pacing rate (0.8 x the target bitrate). The
	// margin covers acknowledgement timing (a receiver acknowledges every
	// 2nd to 10th packet, Wi-Fi in bursts) and a path just under the pacing
	// rate (it still carries the bitrate: no backlog builds).
	shortfallRatio = 1.5
	// shortfallMin: less pacing time than this in the samples (small
	// frames: a still desktop) decides nothing; their acknowledgements'
	// timing would dominate.
	shortfallMin = 20 * time.Millisecond
)

// deliveryMeter is what the window reads of the connection
// (transport.MediaControl's *cc.Media).
type deliveryMeter interface {
	// Delivery returns the ack-eliciting bytes sent and how many of them
	// have left the network (acknowledged or declared lost), cumulative.
	Delivery() (sent, done uint64)
	// Progress is signalled when done grows.
	Progress() <-chan struct{}
	// DeliveredAt returns when done reached pos (false: not yet, or longer
	// ago than the meter remembers).
	DeliveredAt(pos uint64) (time.Time, bool)
	// RecentMinRTT is the path's smallest round trip of the last few
	// seconds (0: none measured yet).
	RecentMinRTT() time.Duration
	// PacingRate is the rate the controller paces at, in bit/s.
	PacingRate() int64
	// WindowLimited is how long in all the congestion window has kept the
	// sender from sending.
	WindowLimited() time.Duration
}

// videoWindow is frameSender's record of the frames in flight.
type videoWindow struct {
	mu    sync.Mutex
	meter deliveryMeter // the controller the marks are positions of
	marks []windowMark  // the frames in flight, oldest first
	// paced: the last shortfallFrames acknowledged frames' delivery, the
	// evidence of a shortfall (npaced written so far).
	paced  [shortfallFrames]paceSample
	npaced int
	// rtt: the round trip "in transit" is measured against (room).
	rtt time.Duration

	// For stream stats: frames held, and the longest hold (µs), since the
	// last report.
	held    atomic.Int64
	maxHold atomic.Int64
}

// windowMark is a frame in flight: the connection's sent positions before
// its write (start) and when it returned (pos), the time when it returned,
// the pacing rate (bit/s) it went out at, and the write's own time (its
// duration less the time the congestion window held it back).
type windowMark struct {
	start, pos uint64
	at         time.Time
	pacing     int64
	own        time.Duration
}

// paceSample is how one frame went through the path: took, from the
// acknowledgement of its first byte to that of its mark, against sent, the
// time the sender needed to send those bytes: paced, the pacer's at its
// rate, or the write's own time where that was longer. A bottleneck slower
// than the sender stretches took; a round trip that is longer or varies
// delays both acknowledgements alike, and a sender that is late itself
// delays both ends of the delivery as it delays the sending.
type paceSample struct {
	took, paced, sent time.Duration
}

// roundTrip is how long a frame stays unacknowledged without any queue: the
// recent min RTT, a quarter added for the peer's delayed acknowledgements (Chrome
// acknowledges every 10th packet or after a quarter of the min RTT, quic-go
// every 2nd: a frame's last packet, which its mark leaves out anyway).
func roundTrip(minRTT time.Duration) time.Duration { return minRTT + minRTT/4 }

// windowLimit is how many frames may be in flight, the next one included,
// on a path of the given recent min RTT at the given frame interval, recent of them
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

// inFlight forgets the recorded frames m has delivered (measuring how they
// went through: measure). Marks, samples and the round trip of another
// controller (a path migration replaced it) are dropped: its positions
// start over. Called with w.mu held.
func (w *videoWindow) inFlight(m deliveryMeter) {
	if m != w.meter {
		w.meter, w.marks, w.npaced, w.rtt = m, w.marks[:0], 0, 0
		return
	}
	_, done := m.Delivery()
	keep := w.marks[:0]
	for _, k := range w.marks {
		if k.pos > done {
			keep = append(keep, k)
			continue
		}
		w.measure(m, k)
	}
	w.marks = keep
}

// measure records how the acknowledged frame k went through the path
// (paceSample), where m still knows when its first byte and its mark were
// delivered. Called with w.mu held.
func (w *videoWindow) measure(m deliveryMeter, k windowMark) {
	if k.pos <= k.start || k.pacing <= 0 {
		return // nothing between the positions, or another controller's start
	}
	first, ok := m.DeliveredAt(k.start + 1)
	if !ok {
		return
	}
	last, ok := m.DeliveredAt(k.pos)
	if !ok {
		return
	}
	paced := time.Duration(float64(k.pos-k.start) * 8 / float64(k.pacing) * float64(time.Second))
	w.paced[w.npaced%shortfallFrames] = paceSample{took: last.Sub(first), paced: paced, sent: max(paced, k.own)}
	w.npaced++
}

// shortfall reports whether the path delivered the last frames' bytes
// shortfallRatio times slower than the sender sent them (see the top of the
// file). Called with w.mu held.
func (w *videoWindow) shortfall() bool {
	var took, paced, sent time.Duration
	for _, p := range w.paced[:min(w.npaced, shortfallFrames)] {
		took += p.took
		paced += p.paced
		sent += p.sent
	}
	return paced >= shortfallMin && float64(took) > shortfallRatio*float64(sent)
}

// room reports whether another frame may go out at now on m's path: yes
// unless the path falls short of the pacing rate and the frames in flight
// fill the window.
func (w *videoWindow) room(m deliveryMeter, interval time.Duration, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight(m)
	short := w.shortfall()
	// The path's round trip: its recent min RTT, but while the path falls
	// short the one from before (lower ones count): the backlog the window
	// leaves (about a frame) is in every round trip then, and would widen
	// "in transit" by itself every couple of seconds.
	if rtt := m.RecentMinRTT(); !short || w.rtt == 0 || (rtt > 0 && rtt < w.rtt) {
		w.rtt = rtt
	}
	if !short {
		return true
	}
	from, recent := now.Add(-roundTrip(w.rtt)), 0
	for _, k := range w.marks {
		if k.at.After(from) {
			recent++
		}
	}
	return len(w.marks) < windowLimit(w.rtt, interval, recent)
}

// sent records a frame whose write (begun as ws) returned at now, its data
// handed to m's path.
func (w *videoWindow) sent(m deliveryMeter, ws writeStart, now time.Time) {
	own := now.Sub(ws.at) - (m.WindowLimited() - ws.limited)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight(m) // switches to m if the controller changed
	pos, _ := m.Delivery()
	w.marks = append(w.marks, windowMark{start: ws.pos, pos: pos, at: now, pacing: m.PacingRate(), own: own})
}

// sentDatagrams records a frame sent as datagram shards (fec.go), handed to
// m's path from ws to now, as sent does, its mark at least end (0: none).
// quic-go queues datagrams (SendDatagram returns before they are packed), so
// m's sent position can still be short of the frame's last shard; ws.pos plus
// the shards' bytes is a lower bound of its end (packets add their headers).
// The write's own time is the shards' hand-over, from the first shard's
// SendDatagram to the last one's return: the writer's own pacing waits and
// any wait on quic-go's full datagram queue (its send loop late on a busy
// CPU), less the time the congestion window held the sender meanwhile (the
// path's), as for a frame stream's write.
func (w *videoWindow) sentDatagrams(m deliveryMeter, ws writeStart, end uint64, now time.Time) {
	own := now.Sub(ws.at) - (m.WindowLimited() - ws.limited)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight(m)
	pos, _ := m.Delivery()
	w.marks = append(w.marks, windowMark{start: ws.pos, pos: max(pos, end), at: now, pacing: m.PacingRate(), own: own})
}

// writeStart is where and when a frame's write began on a path, for sent:
// the path's sent position (math.MaxUint64: unknown, or another path's) and
// how long its congestion window had held the sender back by then.
type writeStart struct {
	pos     uint64
	at      time.Time
	limited time.Duration
}

// startWrite records the beginning of a frame's write on m's path (nil: no
// meter).
func startWrite(m deliveryMeter) writeStart {
	ws := writeStart{pos: math.MaxUint64, at: time.Now()}
	if m != nil {
		ws.pos, _ = m.Delivery()
		ws.limited = m.WindowLimited()
	}
	return ws
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
// and when three quarters of the frame's deadline have passed since its
// stream opened (at most maxWindowHold). The frame's deadline starts after
// (frameSender: sendState.start).
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
