package host

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/fec"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The "datagram + FEC" video mode (GUIDE 2.5; wire format internal/proto/fec.go,
// codec internal/fec, client web/static/js/fec.js).
//
// On a frame stream a lost packet costs the frame (and every frame decoded
// after it) a retransmission: the loss is detected after about a round trip
// and the packet resent, so above ~15 ms of round trip each loss is a visible
// stall. In this mode a frame instead goes out as datagram shards with
// Reed-Solomon parity sized from the loss the client measures: most losses are
// rebuilt from parity on arrival, the rest the client asks for again (NACK,
// answered with fresh parity rows) while the frame still has time.
//
// When: host config "fec" auto (the default) sends frames as shards to
// clients that reassemble them (hello.fec) on the paths where the host's QUIC
// connection ends at the browser (direct WebTransport and the UDP relay; the
// splice ends at the gateway, WebSocket has no datagrams) while the client's
// minimum round trip is above fecRTTOn (below fecRTTOff it goes back to
// streams: a LAN keeps per-frame streams) and the video bitrate is at most
// fecMaxKbps (what the browser's datagram path sustains: docs/VENDOR_NOTES.md
// 2.5); "on" regardless of the round trip (tests, measurements), "off" never.
// A datagram that cannot be sent (the peer's datagram limit below the
// smallest shard, datagrams not negotiated) ends the mode for the session:
// streams again. The switch is per frame and seamless: the client handles
// both at once (a frame is a frame, however it came).
//
// Send priorities (GUIDE 2.7, window.go): quic-go sends datagrams from one
// FIFO queue (32 deep, its SendDatagram blocks while full), so audio, cursor
// and pong datagrams wait behind every shard queued before them. The shard
// writer keeps at most fecQueueAhead of sending time queued, but against its
// model of the pacer, not against acknowledgements: when the path carries
// less than the pacing rate (the congestion window full, an outage) the
// queue fills with a frame's shards, where frame streams fill it only with
// audio. So a frame sent as shards goes through the video window as a frame
// stream does: sendFEC holds it in admit() before it is cut (a placeholder
// outFrame without a stream: the frame, opened, a deadline from its bytes
// with the shards' overhead; a hold re-stamps h.SendUs, which the data
// shards carry, so before Cut), its deadline (rung 1, writeShards) starts
// after the hold, a frame the client would discard meanwhile is not sent,
// and writeShards records it in the window after its last shard
// (sentDatagrams), so the window counts shard frames in flight as it counts
// streams. For the window's shortfall gate the shards' "write" is their
// hand-over, from the first shard's SendDatagram to the last one's return
// (the writer's pacing waits, waits on quic-go's full datagram queue), less
// the time the congestion window held the sender meanwhile: a send loop
// late on a busy CPU is the sender's own time, not the path's. The queue a single frame's shards fill while the path falls short
// stays (at most 32 shards, ~39 KB: about 31 ms at 10 Mbit/s): the writer
// does not also wait between shards for acknowledgements. Pongs leave from
// pongSender, never from the datagram loop, and the client sends NACKs on
// its input-class datagram writer where it has one (sendInputDatagram), not
// on the telemetry sender that drops while its queue stands still. Thinned
// frames (thin.go) are left out before useFEC: the shards' frame header
// carries the thinned mask like a frame stream's. Partial delivery (GUIDE
// 2.4) is a frame stream's: shard frames have no reliable boundary, and a
// session switches between the modes per frame.

const (
	FECAuto = "auto"
	FECOn   = "on"
	FECOff  = "off"

	// fecRTTOn / fecRTTOff: auto mode sends shards while the client's minimum
	// round trip (its pings, the last 30 s) is above fecRTTOn, and streams
	// again once it is below fecRTTOff.
	fecRTTOn  = 15 * time.Millisecond
	fecRTTOff = 12 * time.Millisecond
	// fecRTTReports: auto mode decides once this many of the client's pings
	// carried its minimum round trip (its first five come within 250 ms of
	// the start, then one a second): a loaded browser's first round trip
	// alone can be far above the path's, and once put a LAN on shards for a
	// frame.
	fecRTTReports = 5
	// fecMaxKbps is the most video bitrate sent as shards (auto and on): the
	// browser's datagram path, measured in headless Chromium 141 with
	// tools/dgbench (docs/VENDOR_NOTES.md 2.5).
	fecMaxKbps = 150000

	// fecKeepFrames / fecKeepFor: frames kept for repairs (the client asks
	// within its give-up time, at most a few round trips).
	fecKeepFrames = 120
	fecKeepFor    = time.Second
	// fecRepairShare: repairs take at most this share of the video bitrate
	// (bytes in the last second): a client that NACKs everything cannot
	// make the host send everything twice.
	fecRepairShare = 0.25
	// fecMaxRepair: the most shards one NACK entry gets.
	fecMaxRepair = 32

	// Shard loss estimate: the client's counts over fecLossWindow, once they
	// cover fecLossMinShards shards; fecInitialLoss until then (and the last
	// estimate meanwhile when frames go as shards again). The client counts
	// a frame's first transmission, received and lost alike, max(100 ms, 2 x
	// RTT) after it (web/static/js/fec.js accountMs), so the estimate is
	// that recent from the first frame on.
	fecLossWindow    = 4 * time.Second
	fecLossMinShards = 500
	fecInitialLoss   = 0.01
	// fecMaxLoss: above this shard loss (2 s window) the path loses more than
	// parity repairs (or the browser drops datagrams): streams for fecBackoff.
	fecMaxLoss = 0.2
	fecBackoff = 30 * time.Second

	// fecQueueAhead bounds how far the shard writer runs ahead of the pacer:
	// quic-go sends datagrams in order from one queue (32 deep) ahead of
	// stream data, so audio, cursor and pong datagrams wait behind every
	// shard queued before them. The writer keeps at most this much sending
	// time (and at least two shards) queued.
	fecQueueAhead = 3 * time.Millisecond
)

// validFEC reports whether s is a host config "fec" value ("" is auto).
func validFEC(s string) bool { return s == "" || s == FECAuto || s == FECOn || s == FECOff }

// fec returns the host's FEC mode.
func (c *Config) fec() string {
	if c.FEC == "" {
		return FECAuto
	}
	return c.FEC
}

// fecState is a session's "datagram + FEC" mode.
type fecState struct {
	enc fec.Encoder

	mu       sync.Mutex
	avail    bool      // the client and the path take shards, the config allows them
	on       bool      // frames go as shards now
	off      string    // why the mode ended for the session ("": it did not)
	pause    time.Time // streams until then (too much loss)
	maxShard int       // shard payload limit (proto.MaxShardPayload, or less for a peer's smaller max_datagram_frame_size)
	ring     []*fecFrame
	// rttReports: the client's pings that carried its minimum round trip
	// (the datagram loop counts them).
	rttReports atomic.Int32
	repairs    []timedBytes // repair bytes sent, the last second
	// Loss: the client's cumulative shard counters at its last report, and
	// the differences of the last fecLossWindow.
	lossHave           bool
	lastShards, lastLo uint32
	lossSamples        []lossSample
	loss               float64

	// ratio: the parity and header bytes of the frames sent as shards, per
	// frame byte (x 1e6), for the congestion target (applyCongestionTarget).
	ratio atomic.Int64

	// Counters for stream stats (since the last report).
	frames, shards, parity, frameBytes, wireBytes, repairShards, nacks, nackMisses, repairRefused atomic.Int64
}

type fecFrame struct {
	f  *fec.Frame
	at time.Time
}

type timedBytes struct {
	at time.Time
	n  int
}

type lossSample struct {
	at          time.Time
	shards, los uint32
}

// fecInit decides whether the session may send shards at all (logged once).
func (s *Session) fecInit() {
	fs := &s.fec
	fs.maxShard = proto.MaxShardPayload
	fs.loss = fecInitialLoss
	mode := s.a.cfg.fec()
	why := ""
	switch {
	case mode == FECOff:
		why = "off in host.json"
	case s.hello.FEC < proto.HelloFECVersion:
		why = "the client does not take shards"
	case s.meta.Path != "direct" && s.meta.Path != "relay":
		why = "the path ends at the gateway"
	}
	fs.avail = why == ""
	if fs.avail {
		s.log.Info("video transport: datagram + FEC available", "fec", mode, "rtt_on_ms", fecRTTOn.Milliseconds(),
			"max_kbps", fecMaxKbps)
	} else {
		s.log.Info("video transport: frame streams only", "why", why)
	}
}

// useFEC decides whether the next frame goes as shards (and logs a switch).
func (s *Session) useFEC() bool {
	fs := &s.fec
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.avail || fs.off != "" {
		return false
	}
	kbps := 0
	if t := s.ccTarget.Load(); t != nil {
		kbps = int(t.videoKbps)
	}
	rtt := time.Duration(s.clientRTT.Load())
	want, why := fs.on, ""
	now := time.Now()
	switch {
	case now.Before(fs.pause):
		want, why = false, "too much shard loss"
	case kbps > fecMaxKbps:
		want, why = false, "bitrate above the datagram limit"
	case s.a.cfg.fec() == FECOn:
		want, why = true, "on in host.json"
	case rtt == 0 || fs.rttReports.Load() < fecRTTReports:
		// no round trip measured yet, or too few samples: as before
	case !fs.on && rtt > fecRTTOn:
		want, why = true, "round trip"
	case fs.on && rtt < fecRTTOff:
		want, why = false, "round trip"
	}
	if want != fs.on {
		fs.on = want
		s.rate.setFEC(want)
		mode := "frame streams"
		if want {
			mode = "datagram + FEC"
		}
		s.log.Info("video transport", "mode", mode, "why", why, "rtt_ms", fmt.Sprintf("%.1f", float64(rtt)/1e6), "kbps", kbps,
			"shard_loss_pct", fmt.Sprintf("%.2f", fs.loss*100))
	}
	return fs.on
}

// fecEnd ends the mode for the session (a datagram could not be sent).
func (s *Session) fecEnd(why string) {
	fs := &s.fec
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.off == "" {
		fs.off, fs.on = why, false
		s.rate.setFEC(false)
		s.log.Warn("video transport: datagrams failed, frame streams from now on", "why", why)
	}
}

// fecParity is the parity of a block of k data shards at the current loss
// estimate.
func (s *Session) fecParity(k int) int {
	s.fec.mu.Lock()
	p := s.fec.loss
	s.fec.mu.Unlock()
	return fec.Parity(k, p)
}

// fecOverheadKbps is what the shards add to a video bitrate of kbps (parity,
// headers) while frames go as shards: part of the congestion target, and of
// what the connection's acknowledgements carry besides video.
func (s *Session) fecOverheadKbps(kbps int64) int64 {
	if !s.fecActive() {
		return 0
	}
	return kbps * s.fec.ratio.Load() / 1e6
}

func (s *Session) fecActive() bool {
	s.fec.mu.Lock()
	defer s.fec.mu.Unlock()
	return s.fec.on
}

// fecReport takes the shard counters of a client's rate report: the shard
// loss estimate the parity is sized from.
func (s *Session) fecReport(r proto.RateReport, now time.Time) {
	if r.Flags&proto.RateReportShards == 0 {
		return
	}
	fs := &s.fec
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.lossHave {
		fs.lossHave, fs.lastShards, fs.lastLo = true, r.Shards, r.ShardsLost
		return
	}
	d := lossSample{at: now, shards: r.Shards - fs.lastShards, los: r.ShardsLost - fs.lastLo}
	if d.shards > 1<<24 || d.los > 1<<24 {
		return // a reordered report (the next one's difference covers it)
	}
	fs.lastShards, fs.lastLo = r.Shards, r.ShardsLost
	fs.lossSamples = append(fs.lossSamples, d)
	var got, lost, got2, lost2 uint64
	keep := fs.lossSamples[:0]
	for _, x := range fs.lossSamples {
		if now.Sub(x.at) > fecLossWindow {
			continue
		}
		keep = append(keep, x)
		got += uint64(x.shards)
		lost += uint64(x.los)
		if now.Sub(x.at) <= fecLossWindow/2 {
			got2 += uint64(x.shards)
			lost2 += uint64(x.los)
		}
	}
	fs.lossSamples = keep
	if got+lost >= fecLossMinShards {
		fs.loss = float64(lost) / float64(got+lost)
	}
	if got2+lost2 >= fecLossMinShards && float64(lost2)/float64(got2+lost2) > fecMaxLoss && fs.on {
		fs.on, fs.pause = false, now.Add(fecBackoff)
		s.rate.setFEC(false)
		s.log.Warn("video transport", "mode", "frame streams", "why", "too much shard loss",
			"shard_loss_pct", fmt.Sprintf("%.1f", 100*float64(lost2)/float64(got2+lost2)), "for_s", fecBackoff.Seconds())
	}
}

// sendFEC sends frame f (the nth frameSender took) as shards: the bytes a
// frame stream would carry, cut, with parity. It returns false, having sent
// nothing, when the frame cannot go as shards (the caller sends it on a
// stream).
func (s *Session) sendFEC(f *media.Frame, n int) bool {
	s.sendSince.Store(time.Now().UnixNano())
	s.applyCongestionTarget()
	h, ext := videoHeader(f, s.hello.V, s.a.clock(), s.thinning.mask(f.Gen, f.Seq))
	buf := make([]byte, proto.FrameHeaderLen, proto.FrameHeaderLen+64+len(f.Data))
	h.Marshal(buf)
	if h.Flags&proto.FrameFlagExt != 0 {
		buf = ext.Append(buf)
	}
	buf = append(buf, f.Data...)
	s.fec.mu.Lock()
	maxShard := s.fec.maxShard
	s.fec.mu.Unlock()
	drop, delay := s.a.faults.at(n)
	held := false
	if !drop && delay == 0 {
		// The video window (GUIDE 2.7; the top of this file). The test
		// hooks bypass it, as on frame streams.
		ph := &outFrame{f: f, opened: time.Now(), deadline: s.frameDeadline(s.fecWireEstimate(len(buf)))}
		if held = s.admit(ph) > 0; held && h.Flags&proto.FrameFlagExt != 0 {
			h.SendUs = s.a.clock() // handed to the transport now: the wait is host queue
			h.Marshal(buf)
		}
	}
	fr, dgs, err := s.fec.enc.Cut(buf, f.Gen, f.Seq, maxShard, s.fecParity)
	if err != nil {
		s.log.Debug("frame not cut into shards", "gen", f.Gen, "seq", f.Seq, "bytes", len(buf), "err", err)
		return false
	}
	wire := 0
	for _, d := range dgs {
		wire += len(d)
	}
	of := &outFrame{f: f, opened: time.Now(), deadline: s.frameDeadline(wire)}
	if drop {
		s.lostFrame(f, "test fault") // as if every shard was lost
		return true
	} else if delay > 0 {
		s.log.Debug("test fault: delaying frame", "gen", f.Gen, "seq", f.Seq, "delay", delay)
		time.AfterFunc(delay, func() {
			if !s.writeShards(of, h, fr, dgs, wire, len(buf), false) {
				s.lostFrame(f, "datagrams failed")
			}
		})
		return true
	}
	return s.writeShards(of, h, fr, dgs, wire, len(buf), held)
}

// fecWireEstimate is what a frame stream's n bytes take as shards (parity
// and headers by the recent frames' overhead ratio), for the deadline of a
// frame the video window holds before it is cut.
func (s *Session) fecWireEstimate(n int) int {
	return n + int(int64(n)*s.fec.ratio.Load()/1e6)
}

// writeShards sends a frame's shards in order, no further ahead of the pacer
// than fecQueueAhead. Like a frame stream past its deadline, a frame whose
// shards are still going out past it while a newer frame is ready is stopped
// (rung 1 of the loss-recovery ladder; the shards already sent are gone), and
// one the client would discard (it waits for the answer to a loss before it)
// is not sent on; after a hold of the video window (held) that is also
// asked before the first shard. It returns false when the first shard could
// not be sent (nothing went out: the caller sends the frame on a stream).
// A frame whose shards all went out is recorded in the video window.
func (s *Session) writeShards(of *outFrame, h proto.FrameHeader, fr *fec.Frame, dgs [][]byte, wire, frameBytes int, held bool) bool {
	f := of.f
	pace := s.videoPacingBps()
	if pace <= 0 {
		if t := s.ccTarget.Load(); t != nil {
			pace = float64(t.videoKbps+s.fecOverheadKbps(t.videoKbps)) * 1000 * 1.2
		}
	}
	ahead := max(2*float64(proto.VideoShardHeaderLen+fr.Size), pace/8*fecQueueAhead.Seconds())
	var queued float64
	last := time.Now()
	loss := s.a.faults.fecLoss
	m := s.deliveryMeter()
	ws := startWrite(m) // the video window measures the frame's delivery (and the hand-over's time) from here
	sent := 0           // bytes handed to the connection
	for i, d := range dgs {
		if (i > 0 || held) && i%16 == 0 && s.ctx.Err() == nil {
			in := s.ladderIn(lossOutgoing, f.Gen, f.Seq)
			in.key, in.recovery, in.refFloor, in.age, in.deadline, in.newer = f.Key, f.Recovery, f.RefFloor, time.Since(of.opened), of.deadline, len(s.frameQ) > 0
			switch st := s.send.outgoing(in); st.act {
			case actCancel:
				s.stats.cancelled.Add(1)
				s.log.Info("frame shards stopped", "gen", f.Gen, "seq", f.Seq, "why", st.why, "sent", i, "of", len(dgs),
					"age_ms", in.age.Milliseconds(), "deadline_ms", of.deadline.Milliseconds())
				s.lostFrame(f, "deadline")
				return true
			case actDiscard:
				s.discard(f, st)
				return true
			}
		}
		if pace > 0 {
			now := time.Now()
			queued = max(0, queued-pace/8*now.Sub(last).Seconds())
			last = now
			if queued > ahead {
				wait := time.Duration((queued - ahead) / (pace / 8) * float64(time.Second))
				select {
				case <-s.ctx.Done():
					return true
				case <-time.After(wait):
				}
			}
			queued += float64(len(d))
		}
		if loss > 0 && rand.Float64() < loss {
			continue // test hook: the network lost it
		}
		if err := s.c.SendDatagram(d); err != nil {
			if s.ctx.Err() != nil {
				return true
			}
			var tooLarge *quic.DatagramTooLargeError
			if errors.As(err, &tooLarge) && tooLarge.MaxDatagramPayloadSize > 256+proto.VideoShardHeaderLen+8 {
				// A peer that takes smaller datagrams (its
				// max_datagram_frame_size; quic-go's own packet size
				// estimate starts at transport.InitialPacketSize, which
				// proto.MaxShardPayload fits, and only grows): smaller
				// shards from the next frame on (WebTransport adds up to 8
				// bytes).
				s.fec.mu.Lock()
				s.fec.maxShard = int(tooLarge.MaxDatagramPayloadSize) - proto.VideoShardHeaderLen - 8
				s.fec.mu.Unlock()
				s.log.Info("video transport: smaller shards", "max_datagram", tooLarge.MaxDatagramPayloadSize)
			} else {
				s.fecEnd(err.Error())
			}
			if i == 0 {
				return false
			}
			s.lostFrame(f, "datagrams failed")
			return true
		}
		sent += len(d)
	}
	if m2 := s.deliveryMeter(); m2 != nil {
		// In flight until the peer acknowledged its shards (GUIDE 2.7):
		// SendDatagram queues them, so the frame ends at least its bytes
		// after where its first shard started.
		end := uint64(0)
		if m2 != m {
			ws.pos = math.MaxUint64 // the path changed during the writes
		} else if ws.pos != math.MaxUint64 {
			end = ws.pos + uint64(sent)
		}
		s.win.sentDatagrams(m2, ws, end, time.Now())
	}
	s.fec.keep(fr)
	fs := &s.fec
	fs.frames.Add(1)
	fs.shards.Add(int64(len(dgs)))
	fs.parity.Add(int64(len(dgs) - (fr.Len+fr.Size-1)/fr.Size))
	fs.frameBytes.Add(int64(frameBytes))
	fs.wireBytes.Add(int64(wire))
	// The overhead ratio, smoothed over about 30 frames.
	r := int64(float64(wire-frameBytes) / float64(frameBytes) * 1e6)
	if old := fs.ratio.Load(); old != 0 {
		r = old + (r-old)/30
	}
	fs.ratio.Store(r)
	// The pacer model counts what went on the wire.
	s.track.sent(f.Gen, f.Seq, f.EncodeDoneUs, s.a.clock(), wire, s.videoPacingBps())
	s.stats.frames.Add(1)
	s.stats.bytes.Add(int64(frameBytes))
	if h.Flags&proto.FrameFlagExt != 0 {
		s.hostStages.sentFrame(f, h.SendUs)
	}
	return true
}

// keep stores a frame sent as shards for repairs.
func (fs *fecState) keep(f *fec.Frame) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := time.Now()
	drop := 0
	for drop < len(fs.ring) && (len(fs.ring)-drop >= fecKeepFrames || now.Sub(fs.ring[drop].at) > fecKeepFor) {
		drop++
	}
	fs.ring = append(fs.ring[drop:], &fecFrame{f: f, at: now})
}

// find returns a kept frame (nil: not sent as shards, or too old).
func (fs *fecState) find(gen uint8, seq uint32) *fec.Frame {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i := len(fs.ring) - 1; i >= 0; i-- {
		if k := fs.ring[i]; k.f.Gen == gen && k.f.Seq == seq {
			if time.Since(k.at) > fecKeepFor {
				return nil
			}
			return k.f
		}
	}
	return nil
}

// repairBudget reports whether n more repair bytes fit the budget at a
// video bitrate of kbps (and records them).
func (fs *fecState) repairBudget(n int, kbps int64) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := time.Now()
	sum := 0
	keep := fs.repairs[:0]
	for _, r := range fs.repairs {
		if now.Sub(r.at) < time.Second {
			keep = append(keep, r)
			sum += r.n
		}
	}
	fs.repairs = keep
	if float64(sum+n) > max(fecRepairShare*float64(kbps)*1000/8, 64<<10) {
		return false
	}
	fs.repairs = append(fs.repairs, timedBytes{now, n})
	return true
}

// fecNack queues a client's NACK for fecRepairs (never blocks the datagram
// loop).
func (s *Session) fecNack(n proto.FECNack) {
	select {
	case s.fecNacks <- n:
	default:
		s.fec.nackMisses.Add(1)
	}
}

// fecRepairs answers NACKs: fresh parity rows for each block named (any K of
// a block's shards rebuild it), the data shards again for a frame the client
// has nothing of. Frames not sent as shards, or too old, get nothing (the
// client NACKs frames it never saw a shard of, which may have gone on a
// stream or been dropped).
func (s *Session) fecRepairs() {
	for {
		var n proto.FECNack
		select {
		case <-s.ctx.Done():
			return
		case n = <-s.fecNacks:
		}
		fs := &s.fec
		fs.nacks.Add(1)
		fr := fs.find(n.Gen, n.Seq)
		if fr == nil {
			fs.nackMisses.Add(1)
			continue
		}
		var dgs [][]byte
		if len(n.Blocks) == 0 {
			for bi := range fr.Blocks {
				dgs = append(dgs, fr.Resend(bi)...)
			}
		}
		for _, b := range n.Blocks {
			bi := fr.Block(int(b.Base))
			if bi < 0 || b.Need == 0 {
				continue
			}
			rep, err := fs.enc.Repair(fr, bi, min(int(b.Need), fecMaxRepair))
			if err != nil {
				s.log.Debug("repair", "gen", n.Gen, "seq", n.Seq, "err", err)
				continue
			}
			dgs = append(dgs, rep...)
		}
		bytes := 0
		for _, d := range dgs {
			bytes += len(d)
		}
		kbps := int64(0)
		if t := s.ccTarget.Load(); t != nil {
			kbps = t.videoKbps
		}
		if len(dgs) == 0 {
			fs.nackMisses.Add(1)
			continue
		}
		if !fs.repairBudget(bytes, kbps) {
			fs.repairRefused.Add(1)
			continue
		}
		for _, d := range dgs {
			if loss := s.a.faults.fecLoss; loss > 0 && rand.Float64() < loss {
				continue
			}
			if err := s.c.SendDatagram(d); err != nil {
				break
			}
			fs.repairShards.Add(1)
			fs.wireBytes.Add(int64(len(d)))
		}
	}
}

// fecStats returns the stream stats fields of the mode (nil: unavailable)
// and resets its counters.
func (s *Session) fecStats() []any {
	fs := &s.fec
	fs.mu.Lock()
	avail, on, loss := fs.avail && fs.off == "", fs.on, fs.loss
	fs.mu.Unlock()
	if !avail && fs.frames.Load() == 0 {
		return nil
	}
	frames, shards, parity := fs.frames.Swap(0), fs.shards.Swap(0), fs.parity.Swap(0)
	fb, wb := fs.frameBytes.Swap(0), fs.wireBytes.Swap(0)
	pct := func(a, b int64) string {
		if b <= 0 {
			return "0"
		}
		return fmt.Sprintf("%.1f", 100*float64(a)/float64(b))
	}
	return []any{"fec_on", on, "fec_frames", frames, "fec_shards", shards, "fec_parity_pct", pct(parity, shards-parity),
		"fec_overhead_pct", pct(wb-fb, fb), "fec_loss_pct", fmt.Sprintf("%.2f", loss*100), "fec_nacks", fs.nacks.Swap(0),
		"fec_repairs", fs.repairShards.Swap(0), "fec_nack_misses", fs.nackMisses.Swap(0), "fec_repair_refused", fs.repairRefused.Swap(0)}
}
