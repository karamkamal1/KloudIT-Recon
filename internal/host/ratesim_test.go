package host

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// A deterministic simulation of a session's video path, millisecond by
// millisecond, for the rate controller's tests (GUIDE 2.2): the encoder (its
// pipeline's policy: restarts with key frames, or live changes), the host's
// frame queue (6 frames) and frameSender, the media congestion controller
// (pacing 1.2 x the target with its burst allowance, window pacing rate x
// (min RTT + 2 frame intervals), loss detection and retransmission), a
// bottleneck link (rate, tail-drop queue sized in milliseconds as netem.sh's
// capdrop, random loss), propagation, an optional Wi-Fi gate (netem slot: the
// link opens after a random 0.5-15 ms and releases everything queued) and the
// client (frames complete with their last packet; a rate report every 25 ms,
// travelling back with the propagation delay). The controller, sendTrack and
// rateFeedback are the session's own.

type simConfig struct {
	dur         time.Duration
	fps         int
	ceiling     int                         // kbps: the settings' bitrate
	capacity    func(t time.Duration) int64 // bottleneck bit/s at t
	queue       time.Duration               // bottleneck queue limit at the current rate
	prop        time.Duration               // one-way propagation delay
	wifi        bool                        // the slot gate (netem "slot 0ms 15ms")
	loss        float64                     // random packet loss at the bottleneck
	policy      applyPolicy                 // the pipeline's
	applyDelay  time.Duration               // restart: from the decision to the new generation's first frame
	keyFactor   float64                     // a generation's first frame (and a flush) is this many average frames
	fill        float64                     // the encoder's output as a share of its target
	keyEvery    time.Duration               // > 0: a key-frame restart this often (loss recovery on FFmpeg)
	noComp      bool                        // attribute nothing to the pacer (to show why sendTrack exists)
	relay       bool                        // the acknowledgements are not the client's (a relay leg)
	audioKbps   int
	reportEvery time.Duration
	seed        uint64
}

type simPkt struct {
	frame int // index into frames; -1 audio
	bytes int
	sent  time.Duration
}

type simFrame struct {
	gen        uint8
	seq        uint32
	bytes      int
	encodeDone time.Duration
	pkts       int // packets not yet delivered
	unsent     int // bytes not yet handed to the pacer
	recv       time.Duration
	done       bool
}

type simTimed struct {
	at  time.Duration
	pkt simPkt
}

// simPoint is the controller's state at a moment.
type simPoint struct {
	at      time.Duration
	applied int
	est     float64
	fps     int
}

type simResult struct {
	cfg       simConfig
	frames    []simFrame
	points    []simPoint
	overflows []time.Duration
	changes   []rateChange
	changeAt  []time.Duration
}

const simPktBytes = 1200

func runSim(c simConfig) simResult {
	if c.reportEvery == 0 {
		c.reportEvery = 25 * time.Millisecond
	}
	if c.fill == 0 {
		c.fill = 1
	}
	if c.keyFactor == 0 {
		c.keyFactor = 2
	}
	rng := rand.New(rand.NewPCG(c.seed, 2))
	epoch := time.Unix(1_000_000, 0)
	var now time.Duration
	clock := func() time.Time { return epoch.Add(now) }
	r := &rateController{now: clock}
	r.setPolicy(c.policy)
	r.setPath(!c.relay)
	var track sendTrack
	var rf rateFeedback
	res := simResult{cfg: c}

	// Encoder.
	kbps, fps := r.target(c.ceiling, c.fps)
	pacerKbps := kbps // the media congestion controller's video target
	type pendingStart struct {
		at        time.Duration
		kbps, fps int
		on        bool
	}
	var pend pendingStart
	gen, seq := uint8(1), uint32(0)
	liveKbps, liveFPS := kbps, fps
	r.live(liveKbps, liveFPS)
	encOn, key := true, true
	nextFrame := time.Duration(0)
	nextKey := c.keyEvery
	var frameQ []int
	cur := -1

	// Transport.
	var tokens float64
	var inflight int
	var retx []simPkt
	var lostPkts, ackedPkts, ackedBytes uint64
	var acks, detect, wire, gate []simTimed
	var bq []simPkt
	var bqBytes int
	var credit float64
	nextOpen := time.Duration(0)
	audioDue := 0
	nextAudio := time.Duration(0)

	// Client.
	var cr proto.RateReport
	var owds []time.Duration
	haveFrame := false
	nextReport := c.reportEvery
	var reports []simTimed // report index in pkt.frame, arriving at the host
	var sentReports []proto.RateReport
	nextTick := rateTick

	apply := func(ch rateChange) {
		res.changes = append(res.changes, ch)
		res.changeAt = append(res.changeAt, now)
		pacerKbps = ch.toKbps
		if c.policy.name == "restart" && !ch.urgent {
			pacerKbps = max(ch.toKbps, liveKbps) // the active generation streams on (session.startVideo)
		}
		switch c.policy.name {
		case "restart":
			pend = pendingStart{at: now + c.applyDelay, kbps: ch.toKbps, fps: ch.toFPS, on: true}
			r.target(c.ceiling, c.fps)
			if ch.urgent {
				encOn = false
			}
		default:
			liveKbps, liveFPS = ch.toKbps, ch.toFPS
			// A flush (or an urgent change) is a key frame.
			key = key || c.policy.name == "flush" || ch.urgent
			r.live(liveKbps, liveFPS)
		}
	}

	for ; now < c.dur; now += time.Millisecond {
		capBps := c.capacity(now)

		// Encoder: restarts going live, key-frame restarts, frames.
		if pend.on && now >= pend.at {
			pend.on = false
			gen++
			seq, key, encOn = 0, true, true
			liveKbps, liveFPS = pend.kbps, pend.fps
			pacerKbps = liveKbps
			r.live(liveKbps, liveFPS)
			nextFrame = now
		}
		if c.keyEvery > 0 && now >= nextKey {
			nextKey += c.keyEvery
			if c.policy.name == "restart" {
				pend = pendingStart{at: now + c.applyDelay, kbps: liveKbps, fps: liveFPS, on: true}
				encOn = false
			} else {
				key = true // the helper's forced IDR
			}
		}
		if encOn && now >= nextFrame {
			nextFrame += time.Second / time.Duration(liveFPS)
			size := float64(liveKbps) * 1000 / 8 / float64(liveFPS) * c.fill * (0.8 + 0.4*rng.Float64())
			if key {
				size *= c.keyFactor
				key = false
			}
			f := simFrame{gen: gen, seq: seq, bytes: int(size), encodeDone: now}
			f.unsent = f.bytes
			f.pkts = (f.bytes + simPktBytes - 1) / simPktBytes
			seq++
			res.frames = append(res.frames, f)
			r.output(f.bytes)
			if len(frameQ) >= 6 {
				// The session drops the queue, cuts (an emergency) or
				// hurries a starting generation: the old one stops.
				res.overflows = append(res.overflows, now)
				frameQ = frameQ[:0]
				if ch, ok := r.congestion(signalOverflow); ok {
					apply(ch)
				} else if pend.on {
					encOn = false
				}
			} else {
				frameQ = append(frameQ, len(res.frames)-1)
			}
		}
		if now >= nextAudio {
			nextAudio += 10 * time.Millisecond
			audioDue++
		}

		// Sender: frameSender hands the next frame to QUIC when the write of
		// the last returned (all but its last packet paced out).
		paceBps := 1.2 * float64(pacerKbps+c.audioKbps+ccOverheadKbps) * 1000
		rate := paceBps / 8
		burst := max(rate*0.002, 10*simPktBytes)
		tokens = min(tokens+rate/1000, burst)
		minRTT := 2 * c.prop
		window := max(int(rate*(minRTT+2*time.Second/time.Duration(liveFPS)).Seconds()), 32*simPktBytes)
		for {
			if cur >= 0 && !res.frames[cur].done && res.frames[cur].unsent <= simPktBytes {
				res.frames[cur].done = true // the write returned: the session records the frame
				f := res.frames[cur]
				video := 1.2*float64(pacerKbps+c.audioKbps+ccOverheadKbps)*1000 - float64((c.audioKbps+ccOverheadKbps)*1000)
				if c.noComp {
					video = 0
				}
				track.sent(f.gen, f.seq, uint64(f.encodeDone.Microseconds())+1, uint64(now.Microseconds())+1, f.bytes, video)
			}
			if (cur < 0 || res.frames[cur].unsent == 0) && len(frameQ) > 0 {
				cur, frameQ = frameQ[0], frameQ[1:]
				continue
			}
			if tokens < simPktBytes || inflight >= window {
				break
			}
			var p simPkt
			switch {
			case len(retx) > 0:
				p, retx = retx[0], retx[1:]
			case audioDue > 0 && c.audioKbps > 0:
				audioDue--
				p = simPkt{frame: -1, bytes: c.audioKbps * 1000 / 8 / 100}
			case cur >= 0 && res.frames[cur].unsent > 0:
				n := min(simPktBytes, res.frames[cur].unsent)
				res.frames[cur].unsent -= n
				p = simPkt{frame: cur, bytes: n}
			default:
				p.bytes = 0
			}
			if p.bytes == 0 {
				break
			}
			p.sent = now
			tokens -= float64(p.bytes)
			inflight += p.bytes
			// Bottleneck: random loss, then the tail-drop queue.
			if rng.Float64() < c.loss || float64(bqBytes+p.bytes) > float64(capBps)/8*c.queue.Seconds() {
				qd := time.Duration(float64(bqBytes) / (float64(capBps) / 8) * float64(time.Second))
				detect = append(detect, simTimed{now + time.Duration(1.125*float64(2*c.prop+qd)) + 2*time.Millisecond, p})
				continue
			}
			bq = append(bq, p)
			bqBytes += p.bytes
		}

		// Bottleneck link.
		credit += float64(capBps) / 8 / 1000
		for len(bq) > 0 && credit >= float64(bq[0].bytes) {
			p := bq[0]
			bq = bq[1:]
			bqBytes -= p.bytes
			credit -= float64(p.bytes)
			wire = append(wire, simTimed{now + c.prop, p})
		}
		if len(bq) == 0 {
			credit = min(credit, simPktBytes)
		}

		// Propagation, the Wi-Fi gate, delivery to the client.
		deliver := func(p simPkt) {
			acks = append(acks, simTimed{now + c.prop, p})
			if p.frame < 0 {
				cr.Audio++
				return
			}
			f := &res.frames[p.frame]
			if f.pkts--; f.pkts == 0 {
				f.recv = now
				cr.Frames++
				cr.Bytes += uint32(f.bytes)
				owds = append(owds, now-f.encodeDone)
				if !haveFrame || f.gen != cr.Gen && (f.gen-cr.Gen)&0x80 == 0 || f.gen == cr.Gen && f.seq > cr.LastSeq {
					cr.Gen, cr.LastSeq, haveFrame = f.gen, f.seq, true
				}
			}
		}
		for len(wire) > 0 && wire[0].at <= now {
			if c.wifi {
				gate = append(gate, wire[0])
			} else {
				deliver(wire[0].pkt)
			}
			wire = wire[1:]
		}
		if c.wifi && now >= nextOpen {
			for _, g := range gate {
				deliver(g.pkt)
			}
			gate = gate[:0]
			nextOpen = now + time.Duration(500+rng.IntN(14500))*time.Microsecond
		}

		// Acknowledgements and loss detection at the host.
		for len(acks) > 0 && acks[0].at <= now {
			inflight -= acks[0].pkt.bytes
			ackedPkts++
			ackedBytes += uint64(acks[0].pkt.bytes)
			acks = acks[1:]
		}
		slices.SortFunc(detect, func(a, b simTimed) int { return int(a.at - b.at) })
		for len(detect) > 0 && detect[0].at <= now {
			inflight -= detect[0].pkt.bytes
			lostPkts++
			if detect[0].pkt.frame >= 0 {
				retx = append(retx, detect[0].pkt)
			}
			detect = detect[1:]
		}

		// Client reports, arriving one propagation delay later.
		if now >= nextReport {
			nextReport += c.reportEvery
			cr.TimeMs = uint32(now.Milliseconds())
			cr.Flags = 0
			if haveFrame {
				cr.Flags |= proto.RateReportFrame
			}
			if len(owds) > 0 {
				slices.Sort(owds)
				cr.Flags |= proto.RateReportOWD
				cr.OWDP50Us, cr.OWDMaxUs = int32(owds[(len(owds)-1)/2].Microseconds()), int32(owds[len(owds)-1].Microseconds())
				owds = owds[:0]
			}
			sentReports = append(sentReports, cr)
			reports = append(reports, simTimed{now + c.prop, simPkt{frame: len(sentReports) - 1}})
		}
		for len(reports) > 0 && reports[0].at <= now {
			rr := sentReports[reports[0].pkt.frame]
			reports = reports[1:]
			var comp time.Duration
			if rr.Flags&proto.RateReportFrame != 0 {
				comp, _ = track.cover(rr.Gen, rr.LastSeq)
			}
			fb, ok := rf.fromReport(rr, clock(), comp, ccCounters{ok: true, lost: lostPkts, total: lostPkts + ackedPkts, acked: ackedBytes,
				nonVideoKbps: c.audioKbps + ccOverheadKbps})
			if !ok {
				continue
			}
			fb.pending, fb.pendingValid = track.pending(uint64(now.Microseconds()) + 1)
			if ch, ok := r.report(fb); ok {
				apply(ch)
			}
		}

		// The session's rate loop.
		if now >= nextTick {
			nextTick += rateTick
			at, ok := track.uncoveredSince()
			stalled := ok && uint64(now.Microseconds())+1-at >= uint64(ackTimeout.Microseconds())
			if ch, ok := r.tick(stalled); ok {
				apply(ch)
			}
			est, f, _, _ := r.state()
			cur, _ := r.kbps()
			res.points = append(res.points, simPoint{at: now, applied: cur, est: est, fps: f})
		}
	}
	return res
}

// owdPct is the p-th percentile of the one-way delays of the frames
// encoded in [from, to) that arrived.
func (s simResult) owdPct(from, to time.Duration, p float64) time.Duration {
	var d []time.Duration
	for _, f := range s.frames {
		if f.encodeDone >= from && f.encodeDone < to && f.recv > 0 {
			d = append(d, f.recv-f.encodeDone)
		}
	}
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(p*float64(len(d))))]
}

// appliedAt is the encoder's target at t.
func (s simResult) appliedAt(t time.Duration) int {
	v := 0
	for _, p := range s.points {
		if p.at > t {
			break
		}
		v = p.applied
	}
	return v
}

// heldFrom is the moment from t on after which the target stays at least
// kbps to the end of the run (-1: it ends below).
func (s simResult) heldFrom(t time.Duration, kbps int) time.Duration {
	from := time.Duration(-1)
	for _, p := range s.points {
		switch {
		case p.at < t:
		case p.applied < kbps:
			from = -1
		case from < 0:
			from = p.at
		}
	}
	return from
}

// decreases counts the decreases by why in [from, to).
func (s simResult) decreases(from, to time.Duration) map[string]int {
	out := map[string]int{}
	for i, c := range s.changes {
		if c.down && s.changeAt[i] >= from && s.changeAt[i] < to {
			out[c.why]++
		}
	}
	return out
}

// meanApplied is the time average of the encoder's target in [from, to).
func (s simResult) meanApplied(from, to time.Duration) float64 {
	var sum float64
	n := 0
	for _, p := range s.points {
		if p.at >= from && p.at < to {
			sum += float64(p.applied)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// trace summarizes the target over time (every second) for failure messages.
func (s simResult) trace() string {
	var b []byte
	for _, p := range s.points {
		if p.at%time.Second < rateTick {
			b = fmt.Appendf(b, " %ds:%d", int(p.at.Seconds()), p.applied)
			if p.fps != s.cfg.fps {
				b = fmt.Appendf(b, "@%dfps", p.fps)
			}
		}
	}
	return string(b)
}

// capdropRates is netem.sh's capdrop profile: 50 -> 15 -> 50 Mbit/s, 20 s
// steps (scaled by k).
func capdropRates(k float64) func(time.Duration) int64 {
	return func(t time.Duration) int64 {
		if t >= 20*time.Second && t < 40*time.Second {
			return int64(15e6 * k)
		}
		return int64(50e6 * k)
	}
}

// Pipeline capabilities of the policies the simulations run.
var (
	seamlessCaps = media.PipelineCaps{LiveBitrate: true, LiveBitrateMeasured: true}
	flushCaps    = media.PipelineCaps{LiveBitrate: true, LiveBitrateFlush: true, LiveBitrateMeasured: true}
	restartCaps  = media.PipelineCaps{}
)
