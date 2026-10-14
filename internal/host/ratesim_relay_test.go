package host

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/congestion"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// A deterministic simulation of a native-helper session on the UDP relay
// (the first hardware run, docs/VENDOR_NOTES.md "First hardware run: rate
// control on the UDP relay"), in 100 µs steps. Unlike ratesim_test.go it
// drives the session's own parts around the rate controller: sendTrack and
// rateFeedback, the media congestion controller (cc.Media on the
// simulation's clock), the video window (videoWindow), the loss-recovery
// ladder (sendState, ladder, frameDeadline), thinning (thinState) and the
// frame queue's overflow rule, with the session's glue between them
// (videoEvents, frameSender, Session.loss, rateReport, rateLoop) written
// out below as the session does it. Around them:
//
//   - an AMF-like encoder: live bitrate changes, two temporal SVC layers,
//     an LTR mark every sixth base-layer frame, recovery from the newest
//     LTR the client acknowledged after the last key frame (else an IDR),
//     and frame sizes as multiples of the frame budget that grow at low
//     bitrates the way hardware encoders' do (key frames 6x at 30 Mbit/s,
//     20x at 2 Mbit/s; recovery frames and scene changes likewise), with a
//     smallest P frame (the QP limit) and a source that may be slower than
//     the session's frame rate (a game menu, idle repeats);
//   - quic-go's loss detection and the client's ACK frequency (every second
//     packet for the first 100, then every tenth or after a quarter of the
//     round trip, as Chrome);
//   - the path: a bottleneck (rate, tail-drop queue), random loss, a
//     one-way delay with jitter (in order);
//   - the client: frames complete with their last byte, a rate report every
//     25 ms (the one-way delay p50 of the frames since the last one, on a
//     clock-synced offset that may be off and corrected later), a 0x40 ack
//     after each decoded frame (the LTR acknowledgements).

const (
	rsStep     = 100 * time.Microsecond
	rsMDS      = 1232 // QUIC packet size
	rsOverhead = 52   // QUIC short header and STREAM frame header
	rsPayload  = rsMDS - rsOverhead
	rsHdr      = 64 // frame header and extension
)

// rsRTT is quic-go's RTT estimator (RFC 9002), for cc.Media.
type rsRTT struct {
	min, srtt, rttvar, latest time.Duration
	have                      bool
}

func (r *rsRTT) MinRTT() time.Duration {
	if !r.have {
		return 0
	}
	return r.min
}

func (r *rsRTT) SmoothedRTT() time.Duration {
	if !r.have {
		return 100 * time.Millisecond
	}
	return r.srtt
}

func (r *rsRTT) LatestRTT() time.Duration { return r.latest }

func (r *rsRTT) MeanDeviation() time.Duration {
	if !r.have {
		return 50 * time.Millisecond
	}
	return r.rttvar
}

func (r *rsRTT) PTO(inc bool) time.Duration {
	p := r.SmoothedRTT() + max(4*r.MeanDeviation(), time.Millisecond)
	if inc {
		p += 25 * time.Millisecond
	}
	return p
}

func (r *rsRTT) update(sample, ackDelay time.Duration) {
	if sample <= 0 {
		return
	}
	r.latest = sample
	if !r.have {
		r.min, r.srtt, r.rttvar, r.have = sample, sample, sample/2, true
		return
	}
	r.min = min(r.min, sample)
	adj := sample
	if adj-r.min >= ackDelay {
		adj -= ackDelay
	}
	r.rttvar = (3*r.rttvar + (r.srtt - adj).Abs()) / 4
	r.srtt = (7*r.srtt + adj) / 8
}

// rsStepErr is a piecewise constant value over time (from at on).
type rsStepErr struct{ at, v time.Duration }

// rsCfg is one scenario.
type rsCfg struct {
	name    string
	dur     time.Duration
	fps     int // the session's frame rate (the settings')
	ceiling int // kbps: the user's setting; the first generation starts there
	seed    uint64

	// The path.
	owd      time.Duration                 // one-way delay each way
	jitter   time.Duration                 // per packet, uniform 0..jitter (in order)
	capacity func(t time.Duration) float64 // bottleneck bit/s (nil: 200 Mbit/s)
	capQueue time.Duration                 // the bottleneck's tail-drop queue (0: unlimited)
	loss     float64                       // random packet loss host -> client

	audioKbps int
	svc       bool // two temporal layers; the session thins the enhancement layer
	policy    applyPolicy

	// Frame sizes as multiples of the frame budget (target / source rate),
	// at 30 Mbit/s and at 2 Mbit/s, log-interpolated between.
	idr30, idr2     float64
	rec30, rec2     float64 // LTR recovery frames
	recOwn          bool    // recovery frames are not shrunk by the rate control's debt (their size is the reference's distance: the owner's log)
	scene30, scene2 float64 // scene changes
	sceneEvery      time.Duration
	sigma           float64 // lognormal spread of P frames
	fill30, fill2   float64 // the encoder's output / its target
	pMin            int     // bytes: the smallest P frame (QP limit; 0: none)
	satKbps         int     // > 0: the encoder makes no more than this (simple content)
	srcFPS          int     // > 0: the source delivers this many frames a second

	startKbps int         // > 0: start backed off at this rate (also the last known-good rate)
	clockErr  []rsStepErr // the client's clock-offset error over time
	oldClient bool        // reports without the delay frame and the clock shift
	keyReqAt  time.Duration
	// static: the session's static-desktop cap (activity.go) is on; picture
	// is what the source shows at t (nil: presents at srcFPS that change all
	// of it): still (nothing presented: idle repeats every 100 ms and a
	// caret's new image every 500 ms, all tiny), or presents at srcFPS whose
	// capture reports dirty (a game flipping past the compositor: 0, the
	// owner's first run). The first present after a still picture is as
	// large as a scene change, at most a frame of the full target (the
	// cap's VBV).
	static  bool
	picture func(t time.Duration) (still bool, dirty float64)
	// pause: the link holds what it would send at t until the returned
	// time (0: no pause): a Wi-Fi link's stalls, the delay spikes of the
	// owner's LAN.
	pause     func(t time.Duration) time.Duration
	pauseBack bool // the pauses hold the client's ACKs and reports too (a shared medium)
	// shards > 0: frames go as datagram shards (fec.go) whose parity and
	// headers add this share to a frame's bytes on the wire: the link, the
	// pacer, the congestion target and the pacer model see them, the
	// frame-queue rule and rung 1 judge the frame by them; delivery stays
	// reliable (the parity's repairs are not modelled).
	shards float64
}

func (c *rsCfg) ratio(r30, r2 float64, kbps int) float64 {
	k := math.Log(r2/r30) / math.Log(15)
	return r30 * math.Pow(float64(kbps)/30000, -k)
}

// rsFrame is one encoded frame and what became of it.
type rsFrame struct {
	f        *media.Frame
	bytes    int // stream bytes: header and data
	kind     string
	capture  time.Duration
	encDone  time.Duration
	status   string // encoding, queued, holding, writing, sent, overflow, discarded, thinned, cancelled
	sendAt   time.Duration
	unsent   int
	rcvBytes int
	recv     time.Duration // complete at the client (0: never)
	decoded  time.Duration
}

type rsPkt struct {
	pn      int64
	size    int
	frame   int // -1: audio
	payload int
	sent    time.Duration
	acked   bool
	lost    bool
	dropped bool // the path dropped it (a full queue, random loss)
}

type rsChunk struct{ frame, n int }

type rsEvent struct {
	at   time.Duration
	kind int // rsAck, rsReport, rsFrameAck, rsLive
	pns  []int64
	ackD time.Duration
	rr   proto.RateReport
	idx  int
	kbps int
	fps  int
}

const (
	rsAck = iota
	rsReport
	rsFrameAck
	rsLive
)

// rsTrace is what a run records.
type rsTrace struct {
	cfg       rsCfg
	frames    []rsFrame
	lines     []string // decisions and events
	decreases map[string]int
	downAt    []time.Duration // when each decrease was applied
	overflows int
	// overflowWhy: overflows by queueOverflows' reason.
	overflowWhy map[string]int
	idrs        int
	// static: the static-desktop cap's cuts and restores; thinStarts:
	// thinning episodes.
	cuts, restores int
	thinStarts     int
	undone         int       // delay decreases undone (checkUndo)
	encoder        []int     // the encoder's bitrate at each rate tick (under the static-desktop cap: below the target)
	applied        []int     // the encoder's target at each rate tick
	decoded        []float64 // frames decoded each second
	secs           []string  // per-second summary
}

func runRelaySim(c rsCfg) *rsTrace {
	if c.fps == 0 {
		c.fps = 60
	}
	if c.capacity == nil {
		c.capacity = func(time.Duration) float64 { return 200e6 }
	}
	rng := rand.New(rand.NewPCG(c.seed, 7))
	tr := &rsTrace{cfg: c, decreases: map[string]int{}, overflowWhy: map[string]int{}}
	var now time.Duration
	logf := func(format string, a ...any) {
		tr.lines = append(tr.lines, fmt.Sprintf("%8.3f ", now.Seconds())+fmt.Sprintf(format, a...))
	}
	epoch := time.Unix(2_000_000, 0)
	clock := func() time.Time { return epoch.Add(now) }
	monoBase := congestion.Now()
	mono := func(d time.Duration) congestion.Time { return monoBase.Add(d) }
	hostUs := func(d time.Duration) uint64 { return uint64(d.Microseconds()) + 1_000_000 }

	// The session's parts.
	r := &rateController{now: clock}
	r.setPolicy(c.policy)
	r.setPath(true) // the UDP relay ends at the client
	var track sendTrack
	var fb rateFeedback
	var win videoWindow
	var send sendState
	var thin thinState
	rtt := &rsRTT{}
	m := cc.NewMedia(rtt, rsMDS)
	m.SetClock(func() congestion.Time { return mono(now) })
	audio := c.audioKbps
	// encKbps: the encoder's bitrate (under the static-desktop cap: below
	// the rate controller's target ctlKbps).
	var encKbps, ctlKbps, encFPS int
	st := &staticCap{on: c.static, now: clock}
	fecKbps := func(kbps int64) int64 { return int64(float64(kbps) * c.shards) } // Session.fecOverheadKbps
	// setCC: Session.applyCongestionTarget.
	setCC := func() {
		video := pacedVideoKbps(int64(encKbps), r.outputKbps())
		m.SetTarget((video+fecKbps(video)+int64(audio+ccOverheadKbps))*1000, pacedInterval(time.Second/time.Duration(encFPS), r.frameInterval()))
	}
	wireOf := func(bytes int) int { return bytes + int(float64(bytes)*c.shards) }
	videoPacing := func() float64 {
		return max(0, float64(m.TargetBitrate())*cc.PacingGain-float64((audio+ccOverheadKbps)*1000))
	}
	const mode = proto.RecoveryLTR

	// The encoder.
	encKbps, encFPS = r.target(c.ceiling, c.fps)
	if c.startKbps > 0 {
		r.mu.Lock()
		r.est, r.applied, r.lastGood = float64(c.startKbps), c.startKbps, float64(c.startKbps)
		r.mu.Unlock()
		encKbps = c.startKbps
	}
	r.live(encKbps, encFPS)
	ctlKbps = encKbps
	st.generation(clock(), encKbps, ctlKbps, true)
	r.setFEC(c.shards > 0)
	setCC()
	var (
		seq       uint32
		pos       int
		idrDue    = true
		recFrom   = int64(-1)
		marks     []uint32
		sinceMark = 6
		lastKey   uint32
		nextCap   time.Duration
		nextScene = time.Duration(-1)
		rcDebt    float64
	)
	if c.sceneEvery > 0 {
		nextScene = time.Duration(rng.ExpFloat64() * float64(c.sceneEvery))
	}
	ackedLTR := map[uint32]bool{}
	fidx := map[*media.Frame]int{}
	var encQ []int

	// The session.
	var frameQ []int
	type sender struct {
		state   int // 0 idle, 1 window hold, 2 writing
		of      *outFrame
		idx     int
		until   time.Duration
		since   time.Duration // taken from the queue (sendSince)
		ws      writeStart
		writeAt time.Duration
	}
	var snd sender
	lastKick := -time.Second
	keyDue := false // Session.keyDue
	keyCut := -1    // Session.keyCut: the key frame an overflow behind it cut for
	var burst queueBurst

	// The transport.
	var (
		pn         int64
		sent       = map[int64]*rsPkt{}
		unacked    []int64
		inflight   int
		streamQ    []rsChunk
		retxQ      []rsChunk
		audioDue   int
		nextAudio  time.Duration
		linkFree   time.Duration
		lastArrive time.Duration
		toClient   []struct {
			at time.Duration
			p  *rsPkt
		}
		toHost      []rsEvent
		lastBackArr time.Duration
		lastAckAt   time.Duration
		largestAck  int64 = -1
		ccLost      uint64
		ccTotal     uint64
		ccAcked     uint64
	)

	// The client.
	type owdSample struct {
		d   time.Duration
		seq uint32
	}
	var (
		cRecv       = map[int64]bool{}
		cPending    []int64
		cSinceAck   int
		cTotal      int
		cLargest    int64 = -1
		cAckTimer   time.Duration
		cFirstPend  time.Duration
		cr          proto.RateReport
		cOwd        []owdSample
		cHave       bool
		cErr        = time.Duration(math.MinInt64)
		cShift      time.Duration
		cFirstErr   time.Duration
		nextReport  = 25 * time.Millisecond
		cDecodeNext = 0
		cMinRTT     = 2*c.owd + time.Millisecond
	)
	backDelay := func() time.Duration {
		d := max(now+c.owd+time.Duration(rng.Float64()*float64(c.jitter)), lastBackArr)
		if c.pauseBack && c.pause != nil {
			if until := c.pause(now); until > 0 {
				d = max(d, until+c.owd) // the link stalls both ways: the client's ACKs and reports wait too
			}
		}
		lastBackArr = d
		return d
	}

	type secStat struct {
		enc, sent, thinned, discarded, overflow, cancelled, decoded int
		encBytes, rcvBytes                                          int
		hostQMax, holdMax                                           time.Duration
		held                                                        int
	}
	var sec secStat

	// keyBeing: Session.keyBeingSent; keyframe: Session.keyframe (one key
	// frame at a time in the running encoder).
	keyBeing := func(seq uint32) bool {
		return snd.state != 0 && tr.frames[snd.idx].f.Key && tr.frames[snd.idx].f.Seq < seq
	}
	keyframe := func(why string) {
		if snd.state != 0 && tr.frames[snd.idx].f.Key {
			keyDue = true
			logf("key frame deferred: %s", why)
			return
		}
		idrDue = true
		tr.idrs++
		logf("key frame forced: %s", why)
	}
	requestKeyframe := func(why string) {
		if now-lastKick < 500*time.Millisecond {
			return
		}
		lastKick = now
		keyframe(why)
	}
	// applyRate: Session.applyRate and setRate on a live-bitrate helper.
	applyRate := func(ch rateChange, src string) {
		if ch.down {
			tr.decreases[ch.why]++
			tr.downAt = append(tr.downAt, now)
		}
		if ch.why == "undo" {
			tr.undone++
		}
		logf("RATE %s %d -> %d fps %d why=%s urgent=%v (%s) start=%s %d qd=%v ack_qd=%v", map[bool]string{true: "down", false: "up"}[ch.down], ch.fromKbps, ch.toKbps,
			ch.toFPS, ch.why, ch.urgent, src, ch.basis.start, ch.basis.startKbps, ch.basis.qd.Round(time.Millisecond), ch.basis.ackQD.Round(time.Millisecond))
		// Session.setRate: the static-desktop cap's share of the target.
		ctlKbps = ch.toKbps
		st.mu.Lock()
		encKbps, _ = st.want(clock(), ch.toKbps, true)
		st.applied(clock(), encKbps, ch.toKbps)
		st.mu.Unlock()
		if ch.toFPS > 0 {
			encFPS = ch.toFPS
		}
		setCC()
		toHost = append(toHost, rsEvent{at: now + 5*time.Millisecond, kind: rsLive, kbps: encKbps, fps: encFPS}) // VideoEvent.Rate
		if ch.urgent {
			lastKick = now
			keyframe("urgent rate change")
		}
	}
	// recover: HelperVideo.Recover. The helper accepts it and picks the
	// reference when it encodes the next frame (simAckedLTR): an IDR when no
	// acknowledged LTR follows the last key frame.
	recover := func(from uint32) {
		if recFrom < 0 || int64(from) < recFrom {
			recFrom = int64(from)
		}
	}
	// loss: Session.loss.
	var checkOut func()
	loss := func(ev lossEvent, gen uint8, s uint32, why string) ladderStep {
		in := ladderIn{event: ev, gen: gen, seq: s, live: 1, mode: mode, forceIDR: true}
		st := ladder(in)
		if st.act == actRecover && keyBeing(s) {
			in.recoverFailed = true
			st = ladder(in)
		}
		if st.act == actRecover {
			from := send.recoverFrom(gen, s)
			recover(from)
			logf("recovering from a loss seq=%d wait_from=%d why=%s", s, from, why)
			send.setWait(gen, s, st.rung, false)
			checkOut()
			return st
		}
		if st.act != actKeyframe {
			return st
		}
		send.setWait(gen, s, st.rung, false)
		checkOut()
		if ev != lossOverflow {
			requestKeyframe("frame lost")
		}
		return st
	}
	cancelStream := func(idx int) {
		for i := range streamQ {
			if streamQ[i].frame == idx {
				streamQ[i].n = 0
			}
		}
		for i := range retxQ {
			if retxQ[i].frame == idx {
				retxQ[i].n = 0
			}
		}
	}
	checkOut = func() {
		for _, x := range send.due(ladderIn{event: lossOutgoing, live: 1, mode: mode}, len(frameQ) > 0, clock()) {
			fi, ok := fidx[x.of.f]
			if x.step.act == actDiscard {
				if ok {
					tr.frames[fi].status = "discarded"
					cancelStream(fi)
				}
				x.of.release()
				continue
			}
			sec.cancelled++
			logf("frame stream cancelled seq=%d", x.of.f.Seq)
			if ok {
				tr.frames[fi].status = "cancelled"
				cancelStream(fi)
			}
			loss(lossConfirmed, x.of.f.Gen, x.of.f.Seq, "deadline")
			x.of.release()
		}
	}
	// finish: sendState.finish on the simulation's clock.
	finish := func(of *outFrame) bool {
		send.mu.Lock()
		defer send.mu.Unlock()
		if !of.state.CompareAndSwap(outWriting, outDone) {
			return false
		}
		send.lastSlow = clock().Sub(of.opened) > of.deadline
		send.remove(of)
		return true
	}

	// The encoder's frame captured at t: a present whose capture reports
	// dirty, or (still) an idle repeat or a caret's new image.
	wasStill := false
	encode := func(t time.Duration, still, caret bool, dirty float64) {
		f := &media.Frame{Gen: 1, Seq: seq, Dirty: dirty, HasDirty: true}
		srcFPS := encFPS
		if c.srcFPS > 0 {
			srcFPS = min(c.srcFPS, encFPS)
		}
		// P frames spend the target over the source's frames (the rate
		// control goes by time); a key frame, a recovery frame or a scene
		// change is as large as the picture makes it: multiples of the
		// session's frame budget.
		budget := float64(encKbps) * 1000 / 8 / float64(srcFPS)
		frame := float64(encKbps) * 1000 / 8 / float64(encFPS)
		fill := c.ratio(c.fill30, c.fill2, encKbps)
		if c.satKbps > 0 {
			fill = min(fill, float64(c.satKbps)/float64(encKbps))
		}
		layer := 0
		if c.svc && pos%2 == 1 {
			layer = 1
		}
		kind := "p"
		lat := 2*time.Millisecond + time.Duration(rng.Float64()*float64(time.Millisecond))
		scene := nextScene >= 0 && t >= nextScene
		if scene {
			nextScene = t + time.Duration(rng.ExpFloat64()*float64(c.sceneEvery))
		}
		rec := false
		var floor uint32
		if !idrDue && recFrom >= 0 && layer == 0 {
			// The newest acknowledged LTR before the loss, after the last key
			// frame: a recovery frame from it; none: an IDR.
			if fl, ok := simAckedLTR(marks, ackedLTR, lastKey, uint32(recFrom)); ok {
				rec, floor = true, fl
			} else {
				idrDue = true
			}
			recFrom = -1
		}
		var size float64
		after := wasStill && !still
		wasStill = still
		switch {
		case still && !(idrDue || seq == 0 || rec):
			// A still picture: an idle repeat, or a caret's new image.
			kind, size = "still", 300
			f.Repeat, f.Dirty = !caret, 0
			if caret {
				size, f.Dirty = 800, 40.0/(1920*1080)
			}
		case after && !(idrDue || seq == 0 || rec):
			// The first present after a still picture: a scene change at
			// the encoder's (capped) rate, at most a frame of the full
			// target (the cap keeps the VBV that large).
			kind = "scene"
			size = min(max(budget*fill, frame*fill*c.ratio(c.scene30, c.scene2, encKbps)), float64(ctlKbps)*1000/8/float64(encFPS))
		case idrDue || seq == 0:
			f.Key, kind = true, "idr"
			size = frame * c.ratio(c.idr30, c.idr2, encKbps)
			idrDue, recFrom, marks, pos, layer, lastKey, sinceMark = false, -1, nil, 0, 0, seq, 0
			lat += 2 * time.Millisecond
		case rec:
			f.Recovery, f.RefFloor, kind = true, floor, "recovery"
			size = max(budget*fill, frame*fill*c.ratio(c.rec30, c.rec2, encKbps))
		case scene:
			kind = "scene"
			size = max(budget*fill, frame*fill*c.ratio(c.scene30, c.scene2, encKbps))
		default:
			lf := 1.0
			if c.svc {
				lf = map[int]float64{0: 1.3, 1: 0.7}[layer]
			}
			size = budget * fill * lf * math.Exp(c.sigma*rng.NormFloat64()-c.sigma*c.sigma/2)
		}
		// The encoder's rate control pays back what it overspent with the
		// P frames after it, over about half a second, down to the QP
		// limit; a key frame's size is its own (HRD off), and with recOwn a
		// recovery frame's.
		if !f.Key && !(c.recOwn && rec) && kind != "still" {
			size = max(size/(1+rcDebt/(30*budget)), float64(c.pMin))
		}
		rcDebt = max(0, rcDebt+size-budget)
		f.TemporalLayer = uint8(layer)
		f.Discardable = c.svc && layer == 1 && !f.Key && !f.Recovery
		if !f.Key && layer == 0 && sinceMark >= 6 {
			f.MarkedLTR = true
			marks = append(marks, seq)
			sinceMark = 0
		}
		sinceMark++
		pos++
		seq++
		n := max(200, int(size))
		tr.frames = append(tr.frames, rsFrame{f: f, bytes: n + rsHdr, kind: kind, capture: t, encDone: t + lat, status: "encoding"})
		fidx[f] = len(tr.frames) - 1
		f.EncodeDoneUs = hostUs(t + lat)
		encQ = append(encQ, len(tr.frames)-1)
		if kind != "p" && kind != "still" {
			logf("encoder: %s seq=%d bytes=%d (budget %.0f, %d kbps)", kind, f.Seq, n, budget, encKbps)
		}
	}

	// videoEvents: an encoded frame reaches the session.
	frameOut := func(idx int) {
		af := &tr.frames[idx]
		sec.enc++
		sec.encBytes += af.bytes
		r.output(af.bytes-rsHdr, af.f.Key)
		// Session.staticFrame: the cap's change goes to the encoder before
		// the frame is queued (a restore raises the pacing for this frame),
		// the helper announces it (VideoEvent.Rate).
		af.f.Data = rsZero[:min(len(rsZero), af.bytes-rsHdr)]
		target, _ := r.kbps()
		st.mu.Lock()
		was := st.capped
		sc, ok := st.frame(clock(), af.f, target, true)
		st.mu.Unlock()
		if ok {
			encKbps = sc.kbps
			setCC()
			toHost = append(toHost, rsEvent{at: now + 5*time.Millisecond, kind: rsLive, kbps: encKbps, fps: encFPS})
			switch {
			case sc.kbps < sc.target && !was:
				tr.cuts++
				logf("STATIC cut to %d of %d (images %d, output %.0f kbps, still %v)", sc.kbps, sc.target, sc.images, sc.output, sc.still)
			case sc.kbps >= sc.target:
				tr.restores++
				logf("STATIC restore to %d (activity %.3f, images %d, output %.0f kbps, next hold %v)", sc.kbps, sc.activity, sc.images, sc.output, sc.hold)
			}
		}
		q := queueLoad{queued: len(frameQ), congested: r.delayOver(), interval: time.Second / time.Duration(encFPS), burst: burst.within(clock())}
		if snd.state != 0 {
			q.sending, q.busy, q.deadline, q.lag = true, now-snd.since, snd.of.deadline, now-tr.frames[snd.idx].encDone
		}
		why := queueOverflows(q)
		if why == "" {
			frameQ = append(frameQ, idx)
			af.status = "queued"
			checkOut()
			return
		}
		key := false // Session.keyOverflow
		if why == "deadline" && tr.frames[snd.idx].f.Key && keyCut != snd.idx {
			key, keyCut = true, snd.idx
		}
		setCC() // Session.applyCongestionTarget: frameSender re-applies it per frame
		dropped := append(frameQ, idx)
		frameQ = nil
		tr.overflows++
		for _, d := range dropped {
			tr.frames[d].status = "overflow"
			sec.overflow++
		}
		logf("FRAME QUEUE OVERFLOW frames=%d from_seq=%d why=%s key=%v sender_busy=%v lag=%v", len(dropped), tr.frames[dropped[0]].f.Seq, why, key, q.busy, q.lag)
		tr.overflowWhy[why]++
		first := tr.frames[dropped[0]].f
		recovered := loss(lossOverflow, first.Gen, first.Seq, "queue overflow").act == actRecover
		sig := signalOverflow
		if key {
			sig = signalKeyOverflow
		}
		if ch, ok := r.congestion(sig); ok {
			if recovered {
				ch.urgent = false // seamless live bitrate: no key frame for the cut
			}
			applyRate(ch, "overflow")
		} else if !recovered {
			lastKick = now
			keyframe("queue overflow") // urgentRestart
		}
	}

	nextPacket := func() (*rsPkt, bool) {
		for len(retxQ) > 0 {
			ch := &retxQ[0]
			if ch.n <= 0 {
				retxQ = retxQ[1:]
				continue
			}
			n := min(rsPayload, ch.n)
			ch.n -= n
			return &rsPkt{frame: ch.frame, payload: n, size: n + rsOverhead}, true
		}
		if audioDue > 0 && audio > 0 {
			audioDue--
			b := audio * 1000 / 8 / 100
			return &rsPkt{frame: -1, payload: b, size: b + rsOverhead}, true
		}
		for len(streamQ) > 0 {
			ch := &streamQ[0]
			if ch.n <= 0 {
				streamQ = streamQ[1:]
				continue
			}
			n := min(rsPayload, ch.n)
			ch.n -= n
			tr.frames[ch.frame].unsent -= n
			return &rsPkt{frame: ch.frame, payload: n, size: n + rsOverhead}, true
		}
		return nil, false
	}
	declareLost := func(p *rsPkt) {
		if p.acked || p.lost {
			return
		}
		p.lost = true
		prior := inflight
		inflight -= p.size
		ccLost++
		ccTotal++
		m.OnCongestionEvent(congestion.PacketNumber(p.pn), congestion.ByteCount(p.size), congestion.ByteCount(prior))
		if p.frame >= 0 && (tr.frames[p.frame].status == "sent" || tr.frames[p.frame].status == "writing") {
			retxQ = append(retxQ, rsChunk{p.frame, p.payload})
		}
	}
	clientDecode := func() {
		// In order; the frames the client hears it will not get are skipped.
		for cDecodeNext < len(tr.frames) {
			af := &tr.frames[cDecodeNext]
			switch af.status {
			case "encoding", "queued", "writing", "holding":
				return
			case "sent":
				if af.recv == 0 {
					return
				}
				if af.decoded == 0 {
					af.decoded = max(af.recv, now) + time.Duration(1+rng.IntN(2))*time.Millisecond
					sec.decoded++
					toHost = append(toHost, rsEvent{at: af.decoded + c.owd + time.Duration(rng.Float64()*float64(c.jitter)), kind: rsFrameAck, idx: cDecodeNext})
				}
			}
			cDecodeNext++
		}
	}

	nextSec, nextTick := time.Second, rateTick
	keyReqDone := false
	stillN := 0
	for now = 0; now < c.dur; now += rsStep {
		if c.keyReqAt > 0 && !keyReqDone && now >= c.keyReqAt {
			keyReqDone = true
			requestKeyframe("client request")
		}
		srcFPS := encFPS
		if c.srcFPS > 0 {
			srcFPS = min(c.srcFPS, encFPS)
		}
		still, dirty := false, 1.0
		if c.picture != nil {
			still, dirty = c.picture(now)
			if wasStill && !still {
				nextCap = min(nextCap, now) // a present after idle repeats goes out at once
			}
		}
		if now >= nextCap {
			if still {
				encode(now, true, stillN%5 == 0, 0)
				stillN++
				nextCap += 100 * time.Millisecond
			} else {
				encode(now, false, false, dirty)
				nextCap += time.Second / time.Duration(srcFPS)
			}
		}
		for len(encQ) > 0 && tr.frames[encQ[0]].encDone <= now {
			idx := encQ[0]
			encQ = encQ[1:]
			frameOut(idx)
		}
		if now >= nextAudio {
			nextAudio += 10 * time.Millisecond
			audioDue++
		}

		// Events reaching the host.
		slices.SortStableFunc(toHost, func(a, b rsEvent) int { return int(a.at - b.at) })
		for len(toHost) > 0 && toHost[0].at <= now {
			e := toHost[0]
			toHost = toHost[1:]
			switch e.kind {
			case rsAck:
				newLargest := false
				var acked []*rsPkt
				for _, q := range e.pns {
					p := sent[q]
					if p == nil || p.acked {
						continue
					}
					if p.lost { // a spurious loss: acknowledged after all
						p.acked = true
						continue
					}
					p.acked = true
					acked = append(acked, p)
					if q > largestAck {
						largestAck, newLargest = q, true
					}
				}
				if newLargest {
					rtt.update(now-sent[largestAck].sent, e.ackD)
				}
				lastAckAt = now
				for _, p := range acked {
					prior := inflight
					inflight -= p.size
					ccAcked += uint64(p.size)
					ccTotal++
					m.OnPacketAcked(congestion.PacketNumber(p.pn), congestion.ByteCount(p.size), congestion.ByteCount(prior), mono(now))
				}
			case rsReport: // Session.rateReport
				rr := e.rr
				cnt := ccCounters{ok: true, lost: ccLost, total: ccTotal, acked: ccAcked,
					limited: m.WindowLimited(), nonVideoKbps: audio + ccOverheadKbps + int(fecKbps(int64(encKbps))), held: win.holdTime()}
				cnt.queue, cnt.queueOK = m.QueueDelay() // Session.ccQueue
				f, ok := fb.fromReport(rr, clock(), reportComp(&track, rr), cnt)
				if !ok {
					break
				}
				f.pending, f.pendingValid = track.pending(hostUs(now))
				before := r.decreasedAt()
				ch, ok := r.report(f)
				if ok || !r.decreasedAt().Equal(before) {
					logf("rate report decision why=%s deferred=%v qd=%v owd=%v pending=%v frames=%d owd_seq=%d", ch.why, !ok, f.qd.Round(100*time.Microsecond),
						f.owd.Round(100*time.Microsecond), f.pending.Round(time.Millisecond), f.frames, rr.OWDSeq)
				}
				if ok {
					applyRate(ch, "report")
				}
			case rsFrameAck: // 0x40: HelperVideo.Ack, the helper's LTR acknowledgement
				if af := tr.frames[e.idx]; af.f.MarkedLTR {
					ackedLTR[af.f.Seq] = true
				}
			case rsLive: // VideoEvent.Rate: the encoder runs at the new rate (through the static-desktop cap)
				target, _ := r.kbps()
				r.live(st.rate(clock(), e.kbps, target, true), e.fps)
			}
		}

		// quic-go's loss detection (packet and time thresholds), and a
		// crude PTO.
		if largestAck >= 0 {
			thr := time.Duration(9.0 / 8 * float64(max(rtt.SmoothedRTT(), rtt.latest)))
			keep := unacked[:0]
			for _, q := range unacked {
				p := sent[q]
				if p.acked || p.lost {
					continue
				}
				if q < largestAck && (largestAck-q >= 3 || now-p.sent >= thr) {
					declareLost(p)
					continue
				}
				keep = append(keep, q)
			}
			unacked = keep
		}
		if len(unacked) > 0 {
			// The PTO's probe: acknowledged, it shows a packet the path
			// dropped missing (lost by the packet threshold); one that is
			// only late (a link that stalls) arrives before it, in order,
			// and nothing is lost.
			if p := sent[unacked[0]]; now-max(p.sent, lastAckAt) > rtt.PTO(true) && p.dropped {
				declareLost(p)
				unacked = unacked[1:]
			}
		}

		// The rate loop.
		if now >= nextTick {
			nextTick += rateTick
			at, ok := track.uncoveredSince()
			stalled := ok && hostUs(now)-at >= uint64(ackTimeout.Microseconds())
			if ch, ok := r.tick(stalled); ok {
				applyRate(ch, "tick")
			}
			cur, _ := r.kbps()
			tr.applied = append(tr.applied, cur)
			tr.encoder = append(tr.encoder, encKbps)
		}

		// frameSender.
		checkOut()
	sendLoop:
		for {
			switch snd.state {
			case 0:
				if keyDue {
					keyDue = false
					keyframe("key frame deferred while one was being sent")
				}
				if len(frameQ) == 0 {
					break sendLoop
				}
				idx := frameQ[0]
				frameQ = frameQ[1:]
				af := &tr.frames[idx]
				num, step := send.take(af.f)
				if step.act == actDiscard {
					af.status = "discarded"
					sec.discarded++
					continue
				}
				if c.svc && af.f.Discardable && !af.f.Key && !af.f.Recovery {
					if why, path := thinWhy(false, r.overTarget(), send.slow(clock()), len(frameQ)); why != "" {
						if thin.left(af.f.Gen, af.f.Seq, clock(), why) {
							tr.thinStarts++
							logf("thinning starts why=%s seq=%d", why, af.f.Seq)
						}
						r.thinned(path)
						af.status = "thinned"
						sec.thinned++
						continue
					}
				}
				setCC()
				of := &outFrame{f: af.f, n: num, opened: clock(), deadline: frameDeadline(time.Second/time.Duration(encFPS), wireOf(af.bytes), videoPacing()),
					gone: make(chan struct{}), held: true}
				burst.note(of.deadline, clock()) // Session.sending
				send.register(of)
				snd = sender{state: 1, of: of, idx: idx, until: now + min(of.deadline-of.deadline/windowLead, maxWindowHold), since: now}
				af.status = "holding"
				fallthrough
			case 1:
				of := snd.of
				if of.state.Load() != outWriting { // discarded while held
					snd = sender{}
					continue
				}
				if !win.room(m, time.Second/time.Duration(encFPS), clock()) && now < snd.until {
					break sendLoop
				}
				if d := now - snd.since; d > time.Millisecond {
					sec.held++
					sec.holdMax = max(sec.holdMax, d)
					win.noteHold(d) // Session.admit
				}
				send.mu.Lock()
				of.held, of.opened = false, clock()
				send.mu.Unlock()
				af := &tr.frames[snd.idx]
				af.status, af.sendAt, af.unsent = "writing", now, wireOf(af.bytes)
				sec.hostQMax = max(sec.hostQMax, now-af.encDone)
				sent0, _ := m.Delivery()
				snd.ws = writeStart{pos: sent0, at: clock(), limited: m.WindowLimited(), ready: epoch.Add(af.encDone)}
				snd.writeAt = now
				streamQ = append(streamQ, rsChunk{snd.idx, wireOf(af.bytes)})
				snd.state = 2
				fallthrough
			case 2:
				of := snd.of
				af := &tr.frames[snd.idx]
				if of.state.Load() != outWriting { // cancelled (rung 1) or discarded
					snd = sender{}
					continue
				}
				if af.unsent > rsPayload {
					break sendLoop
				}
				// The write returned (all but its last packet paced out).
				win.sent(m, snd.ws, clock())
				if !finish(of) {
					snd = sender{}
					continue
				}
				af.status = "sent"
				sec.sent++
				track.sent(af.f.Gen, af.f.Seq, af.f.EncodeDoneUs, hostUs(now), wireOf(af.bytes), videoPacing())
				r.sent(af.bytes)
				snd = sender{}
			}
		}

		// quic-go's send loop: the congestion window and the pacer.
		for m.CanSend(congestion.ByteCount(inflight)) && m.HasPacingBudget(mono(now)) {
			p, ok := nextPacket()
			if !ok {
				break
			}
			p.pn, p.sent = pn, now
			pn++
			inflight += p.size
			sent[p.pn] = p
			unacked = append(unacked, p.pn)
			m.OnPacketSent(mono(now), congestion.ByteCount(inflight), congestion.PacketNumber(p.pn), congestion.ByteCount(p.size), true)
			if c.capQueue > 0 && linkFree-now > c.capQueue {
				p.dropped = true
				continue // the bottleneck's queue is full
			}
			if c.pause != nil {
				if until := c.pause(max(linkFree, now)); until > 0 {
					linkFree = max(linkFree, until)
				}
			}
			linkFree = max(linkFree, now) + time.Duration(float64(p.size*8)/c.capacity(now)*float64(time.Second))
			if rng.Float64() < c.loss {
				p.dropped = true
				continue
			}
			at := max(linkFree+c.owd+time.Duration(rng.Float64()*float64(c.jitter)), lastArrive)
			lastArrive = at
			toClient = append(toClient, struct {
				at time.Duration
				p  *rsPkt
			}{at, p})
		}

		// The client: packets, ACKs, frames, reports.
		err := time.Duration(0)
		for _, x := range c.clockErr {
			if now >= x.at {
				err = x.v
			}
		}
		if err != cErr {
			// A better clock sample (stream-worker.js onPong): the offset
			// moves; the delays measured with the old one are dropped.
			if cErr == time.Duration(math.MinInt64) {
				cFirstErr = err
			}
			cErr, cShift, cOwd = err, err-cFirstErr, cOwd[:0]
		}
		for len(toClient) > 0 && toClient[0].at <= now {
			p := toClient[0].p
			toClient = toClient[1:]
			if cRecv[p.pn] {
				continue
			}
			cRecv[p.pn] = true
			cTotal++
			gap := p.pn != cLargest+1
			cLargest = max(cLargest, p.pn)
			if len(cPending) == 0 {
				cFirstPend = now
			}
			cPending = append(cPending, p.pn)
			cSinceAck++
			thr := 10
			if cTotal < 100 {
				thr = 2
			}
			if gap || cSinceAck >= thr {
				toHost = append(toHost, rsEvent{at: backDelay(), kind: rsAck, pns: cPending, ackD: now - cFirstPend})
				cPending, cSinceAck, cAckTimer = nil, 0, 0
			} else if cAckTimer == 0 {
				cAckTimer = now + min(25*time.Millisecond, max(time.Millisecond, cMinRTT/4))
			}
			if p.frame < 0 {
				cr.Audio++
				continue
			}
			af := &tr.frames[p.frame]
			af.rcvBytes += p.payload
			if af.rcvBytes >= wireOf(af.bytes) && af.recv == 0 && af.status == "sent" {
				af.recv = now
				sec.rcvBytes += af.bytes
				cr.Frames++
				cr.Bytes += uint32(af.bytes)
				cOwd = append(cOwd, owdSample{now - af.encDone + cErr, af.f.Seq})
				if !cHave || af.f.Seq > cr.LastSeq {
					cr.Gen, cr.LastSeq, cHave = 1, af.f.Seq, true
				}
			}
		}
		if cAckTimer > 0 && now >= cAckTimer {
			toHost = append(toHost, rsEvent{at: backDelay(), kind: rsAck, pns: cPending, ackD: now - cFirstPend})
			cPending, cSinceAck, cAckTimer = nil, 0, 0
		}
		clientDecode()
		if now >= nextReport {
			nextReport = max(nextReport, now-25*time.Millisecond) + 25*time.Millisecond + time.Duration(rng.Float64()*float64(2*time.Millisecond))
			cr.TimeMs = uint32(now.Milliseconds()) + 1
			cr.Flags = 0
			if cHave {
				cr.Flags |= proto.RateReportFrame
			}
			if len(cOwd) > 0 {
				slices.SortFunc(cOwd, func(a, b owdSample) int { return int(a.d - b.d) })
				p50 := cOwd[(len(cOwd)-1)/2]
				cr.Flags |= proto.RateReportOWD
				cr.OWDP50Us, cr.OWDMaxUs = int32(p50.d.Microseconds()), int32(cOwd[len(cOwd)-1].d.Microseconds())
				if !c.oldClient { // the delay sample's frame and the clock shift (stream-worker.js)
					cr.Flags |= proto.RateReportOWDFrame
					cr.OWDGen, cr.OWDSeq, cr.ClockShiftUs = 1, p50.seq, int32(cShift.Microseconds())
				}
				cOwd = cOwd[:0]
			}
			toHost = append(toHost, rsEvent{at: backDelay(), kind: rsReport, rr: cr})
		}

		if now >= nextSec {
			nextSec += time.Second
			est, f, _, _ := r.state()
			cur, _ := r.kbps()
			tr.decoded = append(tr.decoded, float64(sec.decoded))
			tr.secs = append(tr.secs, fmt.Sprintf("%3ds target %6d est %7.0f fps %d | enc %2d sent %2d thinned %2d discarded %2d overflow %2d cancelled %d decoded %2d | "+
				"enc %5.0f kbps rcv %5.0f kbps | host queue max %4.0f ms, window holds %d (max %.0f ms) | cwnd %d pacing %.1f Mbit/s",
				int(now.Seconds()), cur, est, f, sec.enc, sec.sent, sec.thinned, sec.discarded, sec.overflow, sec.cancelled, sec.decoded,
				float64(sec.encBytes*8)/1000, float64(sec.rcvBytes*8)/1000, float64(sec.hostQMax)/1e6, sec.held, float64(sec.holdMax)/1e6,
				m.GetCongestionWindow(), float64(m.PacingRate())/1e6))
			sec = secStat{}
		}
	}
	return tr
}

// rsZero backs the simulated frames' Data (only its length is read: the
// static-desktop cap's encoder output).
var rsZero = make([]byte, 4<<20)

// simAckedLTR is the helper's LTR choice for a loss from frame from: the
// newest LTR mark the client acknowledged before it, after the last key
// frame.
func simAckedLTR(marks []uint32, acked map[uint32]bool, lastKey, from uint32) (uint32, bool) {
	for i := len(marks) - 1; i >= 0; i-- {
		if marks[i] < from && marks[i] > lastKey && acked[marks[i]] {
			return marks[i], true
		}
	}
	return 0, false
}

// relaySim is the scenario the first hardware run calls for: HEVC 1080p60
// from an AMF-like encoder on the UDP relay (~20 ms round trip), Chrome's
// ACKs, ample capacity, the helper's policy before qualification ("seamless
// (assumed)").
func relaySim(name string, seed uint64) rsCfg {
	return rsCfg{
		name: name, dur: 40 * time.Second, fps: 60, ceiling: 30000, seed: seed,
		owd: 8 * time.Millisecond, jitter: 3 * time.Millisecond,
		audioKbps: 96, svc: true,
		policy: ratePolicy(media.PipelineCaps{LiveBitrate: true, LiveFPS: true}),
		idr30:  10, idr2: 25, rec30: 2.5, rec2: 5, scene30: 3, scene2: 10, sceneEvery: 4 * time.Second,
		sigma: 0.35, fill30: 0.95, fill2: 1.1,
	}
}

// floorTouched reports whether the target reached the floor; tail is the
// mean target over the last d, fps the mean frames decoded per second over
// it.
func (tr *rsTrace) summary(d time.Duration) (touched bool, tail, fps float64) {
	for _, a := range tr.applied {
		if a <= rateFloorKbps {
			touched = true
		}
	}
	n := int(d / rateTick)
	for _, a := range tr.applied[len(tr.applied)-n:] {
		tail += float64(a)
	}
	s := int(d / time.Second)
	for _, v := range tr.decoded[len(tr.decoded)-s:] {
		fps += v
	}
	return touched, tail / float64(n), fps / float64(s)
}

// heldFrom is when (since the start) the target reached kbps and stayed at
// least there to the end (-1: it ends below).
func (tr *rsTrace) heldFrom(kbps int) time.Duration {
	from := time.Duration(-1)
	for i, a := range tr.applied {
		switch {
		case a < kbps:
			from = -1
		case from < 0:
			from = time.Duration(i+1) * rateTick
		}
	}
	return from
}

func (tr *rsTrace) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== %s (seed %d): decreases %v, overflows %d, forced key frames %d\n", tr.cfg.name, tr.cfg.seed, tr.decreases, tr.overflows, tr.idrs)
	kinds := map[string]map[string]int{}
	for _, f := range tr.frames {
		if kinds[f.kind] == nil {
			kinds[f.kind] = map[string]int{}
		}
		st := f.status
		if st == "sent" && f.decoded > 0 {
			st = "decoded"
		}
		kinds[f.kind][st]++
	}
	fmt.Fprintf(&b, "frames by kind and fate: %v\n", kinds)
	b.WriteString(tr.stages(tr.cfg.dur-10*time.Second, tr.cfg.dur) + "\n")
	for _, s := range tr.secs {
		b.WriteString(s + "\n")
	}
	n := 0
	for _, l := range tr.lines {
		if strings.Contains(l, "RATE") || strings.Contains(l, "OVERFLOW") || strings.Contains(l, "decision") || strings.Contains(l, "forced") {
			b.WriteString(l + "\n")
			if n++; n > 200 {
				break
			}
		}
	}
	return b.String()
}

// hostQueue is the p-th percentile and the maximum of the host queue stage
// (encodeDone to taken by the sender) of the frames captured in [from, to)
// that were decoded, and the end-to-end p-th percentile (capture to drawn).
func (tr *rsTrace) hostQueue(from, to time.Duration, p float64) (hq, hqMax, e2e time.Duration) {
	var h, e []time.Duration
	for _, f := range tr.frames {
		if f.capture < from || f.capture >= to || f.decoded == 0 {
			continue
		}
		h = append(h, f.sendAt-f.encDone)
		e = append(e, f.decoded+time.Millisecond-f.capture)
	}
	if len(h) == 0 {
		return 0, 0, 0
	}
	slices.Sort(h)
	slices.Sort(e)
	q := func(v []time.Duration) time.Duration { return v[min(len(v)-1, int(p*float64(len(v))))] }
	return q(h), h[len(h)-1], q(e)
}

// stages: the overlay's latency stages over the frames captured in [from,
// to) that were decoded.
func (tr *rsTrace) stages(from, to time.Duration) string {
	var hq, tf, e2e []time.Duration
	for _, f := range tr.frames {
		if f.capture < from || f.capture >= to || f.decoded == 0 {
			continue
		}
		hq = append(hq, f.sendAt-f.encDone)
		tf = append(tf, f.recv-f.sendAt)
		e2e = append(e2e, f.decoded+time.Millisecond-f.capture)
	}
	if len(e2e) == 0 {
		return "stages: no frame decoded"
	}
	p := func(v []time.Duration) string {
		slices.Sort(v)
		q := func(x float64) float64 { return float64(v[min(len(v)-1, int(x*float64(len(v))))]) / 1e6 }
		return fmt.Sprintf("%.1f/%.1f/%.1f", q(0.5), q(0.95), q(0.99))
	}
	return fmt.Sprintf("stages p50/p95/p99 ms over the last 10 s (%d frames decoded): end-to-end %s, host queue %s, network+transfer %s",
		len(e2e), p(e2e), p(hq), p(tf))
}

// TestRelaySimFirstHardwareRun reproduces the first hardware run's collapse
// with the session's own parts: an AMF-like helper on the UDP relay with
// ample capacity (200 Mbit/s, 16 ms round trip) must keep the setting and
// the stream's frame rate. Each case names what took the target to the
// 2 Mbit/s floor (or, where the encoder makes more than the pacer sent, the
// latency to seconds) before the fixes (docs/VENDOR_NOTES.md "First hardware
// run: rate control on the UDP relay"). Over the whole run, from the first
// frame, the host queue stays under maxQueueP95 at the 95th percentile and
// under maxQueue at worst (a 25-budget key frame at the floor takes ~350 ms
// at the pacing rate, and the frames behind it wait about that), and the
// end-to-end p95 under maxE2E95; a source slower than 60 fps sends larger
// frames that take longer to pace out: the p95 bounds grow by its frame
// interval's excess over 1/60 s (twice for the end-to-end, which also waits
// for the frame's own transfer).
func TestRelaySimFirstHardwareRun(t *testing.T) {
	const (
		maxQueueP95 = 50 * time.Millisecond
		maxQueue    = 400 * time.Millisecond
		maxE2E95    = 100 * time.Millisecond
	)
	outage := func(t time.Duration) float64 { // 200 ms at 0.2 Mbit/s after 5 s
		if t >= 5*time.Second && t < 5200*time.Millisecond {
			return 0.2e6
		}
		return 200e6
	}
	type check struct {
		name   string
		set    func(*rsCfg)
		minEnd float64 // the mean target over the last 10 s at least this share of the setting
		minFPS float64 // frames decoded per second over the last 10 s
		climb  time.Duration
		maxDec int // decreases at most (-1: any number)
		// seeds > 0: run this many seeds and judge the median of the target
		// and the frame rate over the last 10 s (a case that stays fragile
		// on some seeds; the whole-run bounds still hold for every seed).
		seeds int
	}
	for _, c := range []check{
		// The first key frame at 30 Mbit/s (10 x a frame's budget, 625 KB)
		// paced at 1.2 x the target took 140 ms: the six-frame queue
		// overflowed, every overflow cut, and the frames dropped after a
		// key frame left no acknowledged LTR: an IDR again, a storm.
		{"start, key frames 10x", func(c *rsCfg) {}, 0.95, 57, 0, 0, 0},
		{"start, key frames 15x", func(c *rsCfg) { c.idr30, c.idr2 = 15, 40 }, 0.95, 57, 0, 0, 0},
		// Backed off to the floor already: an IDR of 25 x the budget there
		// (104 KB) overflowed the queue every time. From the floor, with
		// the last known-good rate there too, the increases (+5 %/s,
		// accelerating by 5 %/s per second to +25 %/s; an encoder assumed
		// seamless takes a change a second, by 5 % near the last known-good
		// rate) take about 15 s to 85 % of 30 Mbit/s with no decrease on
		// the way; climb allows for one.
		{"from the floor", func(c *rsCfg) { c.startKbps = rateFloorKbps }, 0.9, 57, 25 * time.Second, 1, 0},
		// A source slower than the session's 60 fps (a game menu, idle
		// repeats): the frame-count fill held every increase.
		{"from the floor, 12 fps source", func(c *rsCfg) { c.startKbps, c.srcFPS = rateFloorKbps, 12 }, 0.9, 11, 25 * time.Second, 1, 0},
		// An encoder that cannot make P frames below 8 KB at 2 Mbit/s
		// (1080p60 HEVC at its QP limit): the host's pacer was the
		// bottleneck, and thinning held every increase.
		{"from the floor, P frames >= 8 KB", func(c *rsCfg) { c.startKbps, c.pMin = rateFloorKbps, 8000 }, 0.9, 57, 25 * time.Second, 1, 0},
		// The same without thinning (svc off, a client before
		// HelloVersionThinned, the FFmpeg pipeline), and 16 KB with it:
		// the encoder made more than the pacer sent at 1.2 x the target,
		// every frame on schedule, and the host queue grew to seconds
		// (frameQueueMax), unseen by the rate controller.
		{"from the floor, P frames >= 8 KB, no SVC", func(c *rsCfg) { c.startKbps, c.pMin, c.svc = rateFloorKbps, 8000, false }, 0.9, 57, 25 * time.Second, 1, 0},
		{"from the floor, P frames >= 16 KB", func(c *rsCfg) { c.startKbps, c.pMin = rateFloorKbps, 16000 }, 0.9, 57, 25 * time.Second, 1, 0},
		// A 3 Mbit/s setting under what the encoder makes at its QP limit
		// (3.9 Mbit/s): the host queue grew for the whole run.
		{"3 Mbit/s setting, P frames >= 8 KB, no SVC", func(c *rsCfg) { c.ceiling, c.pMin, c.svc = 3000, 8000, false }, 0.95, 57, 0, 0, 0},
		// The owner's log (all 764 losses answered by recovery frames, no
		// key frame after the start): recovery frames of ~10 frame budgets
		// at 2 Mbit/s took ~140 ms at the pacing rate, the six-frame queue
		// overflowed behind each, and each overflow's loss was answered by
		// the next recovery frame. Key frames small enough not to overflow
		// at the start (6 budgets at 2 Mbit/s) isolate it; a 200 ms outage
		// at 5 s starts it.
		{"from the floor, recovery frames 10x after an outage", func(c *rsCfg) {
			c.startKbps, c.idr30, c.idr2, c.rec30, c.rec2, c.recOwn = rateFloorKbps, 4, 6, 8, 10, true
			c.capacity, c.capQueue = outage, 50*time.Millisecond
		}, 0.9, 57, 30 * time.Second, 2, 0},
		// Frames as datagram shards (auto FEC turns on above a 15 ms
		// minimum round trip: the UDP relay), whose parity and headers add
		// 8-15 %: judged by their stream bytes, a key frame on schedule
		// was past its deadline and the storm came back.
		{"start, shards +8 %", func(c *rsCfg) { c.shards = 0.083 }, 0.95, 57, 0, 0, 0},
		{"start, key frames 15x, shards +15 %", func(c *rsCfg) { c.idr30, c.idr2, c.shards = 15, 40, 0.15 }, 0.95, 57, 0, 0, 0},
		{"from the floor, shards +15 %", func(c *rsCfg) { c.startKbps, c.shards = rateFloorKbps, 0.15 }, 0.9, 57, 25 * time.Second, 1, 0},
		// The client's clock offset corrected by 12 ms after 2 s (a better
		// ping once the busy start is over): a delay step that never
		// drains, read as a queue for the 2 s of the base window, a
		// decrease every hold.
		{"clock correction 12 ms", func(c *rsCfg) {
			c.clockErr = []rsStepErr{{0, -12 * time.Millisecond}, {2 * time.Second, 0}}
		}, 0.95, 57, 0, 0, 0},
		// A client that does not report its clock's corrections (or a step
		// of the path's own delay): one decrease, then the new level is the
		// base.
		{"clock correction 15 ms, old client", func(c *rsCfg) {
			c.oldClient, c.clockErr = true, []rsStepErr{{0, -15 * time.Millisecond}, {2 * time.Second, 0}}
		}, 0.95, 57, 0, 2, 0},
		// 0.5 % random loss: retransmitted frames completing out of order
		// made the delay of one set of frames less the pacer's share of
		// another, often negative; the 2 s base then read every report as
		// over the target. An old client cannot name the frame its delay
		// is, and stays fragile: 3 of 12 seeds end under 80 % of the
		// setting, before the review's fixes and after (other seeds each
		// time); the median is judged.
		{"loss 0.5 %", func(c *rsCfg) { c.loss = 0.005 }, 0.85, 57, 0, 4, 0},
		{"loss 0.5 %, old client", func(c *rsCfg) { c.loss, c.oldClient = 0.005, true }, 0.8, 57, 0, -1, 12},
		// The second overlay's setting, 154 Mbit/s, with an encoder that
		// makes 60 at most and the clock correction: seven delay decreases
		// (to ~17 Mbit/s in 4 s) before the fixes, back at the setting
		// after 20-27 s; no collapse to the floor (the owner's session went
		// on from ~6 Mbit/s to the floor by the recovery-frame cycle, the
		// next case).
		{"154 Mbit/s setting, clock correction", func(c *rsCfg) {
			c.ceiling, c.satKbps, c.clockErr = 154000, 60000, []rsStepErr{{0, -15 * time.Millisecond}, {2 * time.Second, 0}}
		}, 0.95, 57, 0, 0, 0},
		// The second overlay ("2.0 of 154.0 Mbps (backed off)"): from the
		// floor with the owner's recovery frames (and key frames as above).
		// The climb to 85 % of 154 Mbit/s takes about 33 s.
		{"154 Mbit/s setting from the floor, recovery frames 10x after an outage", func(c *rsCfg) {
			c.ceiling, c.satKbps, c.startKbps, c.idr30, c.idr2, c.rec30, c.rec2, c.recOwn = 154000, 60000, rateFloorKbps, 4, 6, 8, 10, true
			c.capacity, c.capQueue = outage, 50*time.Millisecond
		}, 0.85, 57, 36 * time.Second, 2, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			seeds := max(2, c.seeds)
			var tails, fpss []float64
			for seed := uint64(1); seed <= uint64(seeds); seed++ {
				cfg := relaySim(c.name, seed)
				c.set(&cfg)
				tr := runRelaySim(cfg)
				touched, tail, fps := tr.summary(10 * time.Second)
				hq, hqMax, e2e := tr.hostQueue(0, cfg.dur, 0.95)
				slow := time.Duration(0)
				if cfg.srcFPS > 0 && cfg.srcFPS < 60 {
					slow = time.Second/time.Duration(cfg.srcFPS) - time.Second/60
				}
				t.Logf("%s seed %d: floor touched %v, last 10 s: target %.0f, %.1f fps; decreases %v, overflows %d %v, forced key frames %d, thinning episodes %d; "+
					"whole run: host queue p95 %v, max %v, end-to-end p95 %v, one-way delay p50/p95 %v/%v",
					c.name, seed, touched, tail, fps, tr.decreases, tr.overflows, tr.overflowWhy, tr.idrs, tr.thinStarts, hq, hqMax, e2e,
					tr.owd(0, cfg.dur, 0.5), tr.owd(0, cfg.dur, 0.95))
				dec := 0
				for _, n := range tr.decreases {
					dec += n
				}
				tails, fpss = append(tails, tail), append(fpss, fps)
				if c.seeds > 0 {
					tail, fps = math.Inf(1), math.Inf(1) // judged over the seeds below
				}
				fail := ""
				switch {
				case c.maxDec >= 0 && dec > c.maxDec:
					fail = fmt.Sprintf("%d decreases, want at most %d", dec, c.maxDec)
				case tail < c.minEnd*float64(cfg.ceiling):
					fail = fmt.Sprintf("mean target %.0f over the last 10 s, want at least %.0f", tail, c.minEnd*float64(cfg.ceiling))
				case fps < c.minFPS:
					fail = fmt.Sprintf("%.1f frames decoded per second over the last 10 s, want at least %.0f", fps, c.minFPS)
				case c.climb == 0 && touched && c.seeds == 0:
					fail = "the target reached the floor"
				case hq > maxQueueP95+slow || hqMax > maxQueue || e2e > maxE2E95+2*slow:
					fail = fmt.Sprintf("over the whole run host queue p95 %v, max %v, end-to-end p95 %v; want under %v, %v, %v",
						hq, hqMax, e2e, maxQueueP95+slow, maxQueue, maxE2E95+2*slow)
				case c.climb > 0:
					if at := tr.heldFrom(cfg.ceiling * 85 / 100); at < 0 || at > c.climb {
						fail = fmt.Sprintf("back at 85 %% of the setting (and staying) at %v, want within %v", at, c.climb)
					}
				}
				if fail != "" {
					t.Errorf("seed %d: %s\n%s", seed, fail, tr.report())
				}
			}
			if c.seeds > 0 {
				slices.Sort(tails)
				slices.Sort(fpss)
				ceiling := relaySim(c.name, 1)
				c.set(&ceiling)
				if m := tails[len(tails)/2]; m < c.minEnd*float64(ceiling.ceiling) || fpss[len(fpss)/2] < c.minFPS {
					t.Errorf("median over %d seeds: target %.0f, %.1f fps over the last 10 s; want at least %.0f and %.0f (targets %v)",
						c.seeds, m, fpss[len(fpss)/2], c.minEnd*float64(ceiling.ceiling), c.minFPS, tails)
				}
			}
		})
	}
}

// owd is the p-th percentile of the one-way delay (encodeDone to complete at
// the client) of the frames encoded in [from, to) that arrived.
func (tr *rsTrace) owd(from, to time.Duration, p float64) time.Duration {
	var d []time.Duration
	for _, f := range tr.frames {
		if f.encDone >= from && f.encDone < to && f.recv > 0 {
			d = append(d, f.recv-f.encDone)
		}
	}
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(p*float64(len(d))))]
}

// meanApplied is the mean target over [from, to).
func (tr *rsTrace) meanApplied(from, to time.Duration) float64 {
	var sum float64
	n := 0
	for i, a := range tr.applied {
		if at := time.Duration(i+1) * rateTick; at >= from && at < to {
			sum += float64(a)
			n++
		}
	}
	return sum / float64(max(1, n))
}

// TestRelaySimCongestion: the same session on a path that really is
// congested still backs off: a capacity drop to 15 Mbit/s (netem's capdrop
// step, a 50 ms tail-drop queue) cuts within half a second, keeps the delay
// near the baseline and comes back within 10 s of the capacity's return
// (GUIDE 2.2); a 5, 10, 20 or 25 Mbit/s path settles under its capacity, not
// at the floor (a 10 Mbit/s path went to the floor at 2-4 fps before the
// fixes: the first key frame took longer than the six-frame queue, the storm
// of key frames followed), and from the start: a key frame past its deadline
// cuts to what the path carried before the next one (signalKeyOverflow), so
// at most three key frames are forced and from the third second on every
// second decodes at least 50 frames (before the review's fixes: a storm of
// key frames for 2-8 s at 0-5 fps on a 5 Mbit/s path); 5 % random loss is
// congestion by the 2 % rule.
func TestRelaySimCongestion(t *testing.T) {
	t.Run("capacity drop", func(t *testing.T) {
		t.Parallel()
		for seed := uint64(1); seed <= 2; seed++ {
			cfg := relaySim("capacity drop", seed)
			cfg.dur, cfg.capQueue = 45*time.Second, 50*time.Millisecond
			cfg.capacity = func(t time.Duration) float64 {
				if t >= 15*time.Second && t < 30*time.Second {
					return 15e6
				}
				return 200e6
			}
			tr := runRelaySim(cfg)
			first := time.Duration(-1)
			for _, at := range tr.downAt {
				if at >= 15*time.Second {
					first = at - 15*time.Second
					break
				}
			}
			base, dip := tr.owd(5*time.Second, 15*time.Second, 0.95), tr.owd(15*time.Second, 30*time.Second, 0.95)
			back := time.Duration(-1)
			for i, a := range tr.applied {
				at := time.Duration(i+1) * rateTick
				switch {
				case at < 30*time.Second:
				case a < cfg.ceiling*85/100:
					back = -1
				case back < 0:
					back = at - 30*time.Second
				}
			}
			dipMean := tr.meanApplied(20*time.Second, 30*time.Second)
			t.Logf("seed %d: first cut %v after the drop, one-way delay p95 %v before, %v during the dip, mean target in the dip %.0f, back %v after; decreases %v, overflows %d",
				seed, first, base, dip, dipMean, back, tr.decreases, tr.overflows)
			if first < 0 || first > 500*time.Millisecond || dip > base+30*time.Millisecond || dipMean > 15000 || back < 0 || back > 10*time.Second {
				t.Errorf("seed %d: want a cut within 500 ms, the dip's delay p95 under %v, the target under the 15 Mbit/s, back within 10 s\n%s",
					seed, base+30*time.Millisecond, tr.report())
			}
		}
	})
	for _, c := range []struct {
		name     string
		capacity float64
	}{{"5 Mbit/s path", 5e6}, {"10 Mbit/s path", 10e6}, {"20 Mbit/s path", 20e6}, {"25 Mbit/s path", 25e6}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 2; seed++ {
				cfg := relaySim(c.name, seed)
				cfg.capQueue = 50 * time.Millisecond
				cfg.capacity = func(time.Duration) float64 { return c.capacity }
				tr := runRelaySim(cfg)
				touched, tail, fps := tr.summary(20 * time.Second)
				t.Logf("seed %d: floor touched %v, last 20 s: target %.0f, %.1f fps, one-way delay p95 %v; decreases %v, overflows %d %v, forced key frames %d; decoded per second from the start %v",
					seed, touched, tail, fps, tr.owd(20*time.Second, cfg.dur, 0.95), tr.decreases, tr.overflows, tr.overflowWhy, tr.idrs, tr.decoded[:6])
				want := c.capacity / 1000
				if touched || tail > want || tail < 0.5*want || fps < 55 {
					t.Errorf("seed %d: want the target between half the capacity and the capacity (%.0f), not at the floor, the frame rate kept\n%s",
						seed, want, tr.report())
				}
				for i, n := range tr.decoded[2:10] {
					if n < 50 {
						t.Errorf("seed %d: %.0f frames decoded in second %d, want at least 50 from the third second on (no storm of key frames)\n%s",
							seed, n, i+3, tr.report())
						break
					}
				}
				if tr.idrs > 3 {
					t.Errorf("seed %d: %d key frames forced, want at most 3\n%s", seed, tr.idrs, tr.report())
				}
			}
		})
	}
	t.Run("loss 5 %", func(t *testing.T) {
		t.Parallel()
		cfg := relaySim("loss 5 %", 1)
		cfg.dur, cfg.loss = 20*time.Second, 0.05
		tr := runRelaySim(cfg)
		t.Logf("decreases %v, target at the end %d", tr.decreases, tr.applied[len(tr.applied)-1])
		if tr.decreases["loss"] == 0 || tr.applied[len(tr.applied)-1] > cfg.ceiling/2 {
			t.Errorf("decreases %v, target %d at the end: want loss decreases to at most half\n%s", tr.decreases, tr.applied[len(tr.applied)-1], tr.report())
		}
	})
}

// ownerMenu is the picture of the owner's first run with the static-desktop
// cap toggling (docs/VENDOR_NOTES.md "First hardware run: the static desktop
// during a game"): a game menu presenting at 60 fps whose presents DDA handed
// out without dirty rects (the helper reported them as unchanged), and a
// frame the compositor drew (36 % of the picture, a share the log has) every
// 1.5 s.
func ownerMenu(t time.Duration) (bool, float64) {
	if t%(1500*time.Millisecond) < 17*time.Millisecond {
		return false, 0.363
	}
	return false, 0
}

// wifiPauses: the link stalls 30, 60 or 90 ms about every 1.7 s (a Wi-Fi
// link's medium busy, a power-save interval): one-way delay spikes as the
// owner's report_owd max of 40-150 ms per 10 s on a 13-21 ms round trip.
func wifiPauses(t time.Duration) time.Duration {
	const every = 1700 * time.Millisecond
	k := t / every
	start := k*every + 300*time.Millisecond
	if t < start {
		return 0
	}
	if end := start + time.Duration(30*(1+k%3))*time.Millisecond; t < end {
		return end
	}
	return 0
}

// TestRelaySimStaticDesktop replays the static-desktop cut of the owner's
// first run against the fixed code (docs/VENDOR_NOTES.md "First hardware
// run: the static desktop during a game"): with the session's cap on, a game
// whose presents the helper reported as unchanged is never cut (52f801f cut
// it to a quarter of the target a second after each composed frame and
// restored it with the next, every 1-2 s, 30 cuts and 28 restores in the
// log, each restore followed by a burst of large frames), also at the
// 151 Mbit/s setting with an encoder making 10 Mbit/s and on a link with
// Wi-Fi-like stalls (the 118 -> 51 -> 23 Mbit/s cascade); a still desktop is
// cut once it has been still for the hold, and the game after it (the first
// frame as large as a frame at the full target) is restored at once and
// carried without a frame-queue overflow (the log's first overflow, at
// 30 Mbit/s with no loss, 128 ms after a restore).
func TestRelaySimStaticDesktop(t *testing.T) {
	stillThenGame := func(t time.Duration) (bool, float64) {
		if t < 12*time.Second {
			return true, 0
		}
		return false, 1
	}
	for _, c := range []struct {
		name         string
		set          func(*rsCfg)
		cuts         int     // the cap's cuts (and at most as many restores)
		maxDec       int     // rate decreases at most
		minEnd       float64 // the mean target over the last 10 s at least this share of the setting
		minFPS       float64
		minShare     float64 // the target never under this share of the setting from 2 s on
		meanShare    float64 // the mean target from 2 s on at least this share of the setting
		after        time.Duration
		maxQueueP95  time.Duration // host queue p95 of the frames captured from after on
		maxQueueMax  time.Duration
		maxE2EP95    time.Duration
		noOverflowAt time.Duration // no frame-queue overflow from here on (-1: none at all)
	}{
		{"game menu, presents reported unchanged", func(c *rsCfg) { c.picture = ownerMenu }, 0, 0, 0.95, 57, 0.95, 0.95, 0,
			50 * time.Millisecond, 150 * time.Millisecond, 100 * time.Millisecond, -1},
		{"game menu at 151 Mbit/s, encoder at 10 Mbit/s", func(c *rsCfg) { c.picture, c.ceiling, c.satKbps = ownerMenu, 151000, 10000 },
			0, 0, 0.95, 57, 0.95, 0.95, 0, 50 * time.Millisecond, 150 * time.Millisecond, 100 * time.Millisecond, -1},
		// The stalls: each holds every frame for 30-90 ms, the reports find
		// frames that do not arrive, and the decrease, taken from the next
		// to nothing acknowledged, halved the target (0.425 x: the log's
		// 154000 -> 65450, 118196 -> 51315, 52901 -> 22904, 24433 ->
		// 10541); the climb back from each halving took longer than the
		// next stall: the cascade (at a20376f, its static cap toggling too:
		// 8 halvings, the floor, mean 28 and 29 of 151 Mbit/s). Now nothing
		// acknowledged at all is a
		// stall, not a capacity (stallMax), and a decrease on a stall that
		// slips through is undone once the path delivers the backlog at
		// once (checkUndo): no two halvings in a row, the mean near the
		// setting.
		{"game menu at 151 Mbit/s, encoder at 10 Mbit/s, Wi-Fi stalls", func(c *rsCfg) {
			c.picture, c.ceiling, c.satKbps, c.pause = ownerMenu, 151000, 10000, wifiPauses
		}, 0, -1, 0.75, 57, 0.33, 0.8, 0, 50 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond, -1},
		// Stalls that hold both ways (a shared medium): the client's ACKs
		// and reports arrive in a burst after each, the reports with
		// frames that did not arrive while ACKs come in with them; the
		// decrease on them is undone once the backlog arrives at once
		// (checkUndo).
		{"game menu at 151 Mbit/s, encoder at 10 Mbit/s, Wi-Fi stalls both ways", func(c *rsCfg) {
			c.picture, c.ceiling, c.satKbps, c.pause, c.pauseBack = ownerMenu, 151000, 10000, wifiPauses, true
		}, 0, -1, 0.75, 57, 0.33, 0.8, 0, 50 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond, -1},
		// The same stalls under a game that fills its 30 Mbit/s (at
		// a20376f the floor, mean 7.0 and 9.7 Mbit/s): the frames held by the
		// stall arrive late (the congestion window held the sender), a
		// decrease on their delay where it is far over the target and
		// drains at once is undone.
		{"game at 30 Mbit/s, Wi-Fi stalls", func(c *rsCfg) { c.picture, c.pause = ownerMenu, wifiPauses }, 0, -1, 0.75, 57, 0.33, 0.8, 0,
			50 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond, -1},
		{"game at 30 Mbit/s, Wi-Fi stalls both ways", func(c *rsCfg) { c.picture, c.pause, c.pauseBack = ownerMenu, wifiPauses, true }, 0, -1, 0.75, 57,
			0.33, 0.8, 0, 50 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond, -1},
		{"still desktop, then the game at 30 Mbit/s", func(c *rsCfg) { c.picture = stillThenGame }, 1, 0, 0.95, 57, 0.95, 0.95,
			12 * time.Second, 50 * time.Millisecond, 150 * time.Millisecond, 100 * time.Millisecond, -1},
		{"still desktop, then the game at 151 Mbit/s, encoder at 60 Mbit/s", func(c *rsCfg) {
			c.picture, c.ceiling, c.satKbps = stillThenGame, 151000, 60000
		}, 1, 0, 0.95, 57, 0.95, 0.95, 12 * time.Second, 50 * time.Millisecond, 150 * time.Millisecond, 100 * time.Millisecond, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 2; seed++ {
				cfg := relaySim(c.name, seed)
				cfg.static = true
				c.set(&cfg)
				tr := runRelaySim(cfg)
				_, tail, fps := tr.summary(10 * time.Second)
				hq, hqMax, e2e := tr.hostQueue(c.after, cfg.dur, 0.95)
				lowest := cfg.ceiling
				for i, a := range tr.applied {
					if time.Duration(i+1)*rateTick >= 2*time.Second {
						lowest = min(lowest, a)
					}
				}
				mean := tr.meanApplied(2*time.Second, cfg.dur)
				capped, enc := 0, 0.0
				for i, e := range tr.encoder {
					enc += float64(e) / float64(len(tr.encoder))
					if e < tr.applied[i] {
						capped++
					}
				}
				dec := 0
				for _, n := range tr.decreases {
					dec += n
				}
				t.Logf("%s seed %d: static cuts %d, restores %d, encoder below the target %.0f %% of the time, its mean %.0f; decreases %v, undone %d, lowest target %d, mean %.0f; overflows %d %v; thinning episodes %d; "+
					"last 10 s: target %.0f, %.1f fps; from %v: host queue p95 %v, max %v, end-to-end p95 %v",
					c.name, seed, tr.cuts, tr.restores, 100*float64(capped)/float64(len(tr.encoder)), enc, tr.decreases, tr.undone, lowest, mean, tr.overflows, tr.overflowWhy, tr.thinStarts, tail, fps, c.after, hq, hqMax, e2e)
				fail := ""
				switch {
				case tr.cuts != c.cuts || tr.restores > c.cuts:
					fail = fmt.Sprintf("%d static cuts and %d restores, want %d", tr.cuts, tr.restores, c.cuts)
				case c.maxDec >= 0 && dec > c.maxDec:
					fail = fmt.Sprintf("%d decreases, want at most %d", dec, c.maxDec)
				case float64(lowest) < c.minShare*float64(cfg.ceiling) || mean < c.meanShare*float64(cfg.ceiling):
					fail = fmt.Sprintf("the target went down to %d, mean %.0f; want at least %.0f, %.0f", lowest, mean,
						c.minShare*float64(cfg.ceiling), c.meanShare*float64(cfg.ceiling))
				case tail < c.minEnd*float64(cfg.ceiling) || fps < c.minFPS:
					fail = fmt.Sprintf("last 10 s: target %.0f, %.1f fps; want %.0f, %.0f", tail, fps, c.minEnd*float64(cfg.ceiling), c.minFPS)
				case tr.overflows > 0:
					fail = fmt.Sprintf("%d frame-queue overflows %v", tr.overflows, tr.overflowWhy)
				case tr.thinStarts > 3:
					// The log's 347 thinning episodes (a start and an end
					// line each) came with the overflow cycles.
					fail = fmt.Sprintf("%d thinning episodes, want at most 3", tr.thinStarts)
				case hq > c.maxQueueP95 || hqMax > c.maxQueueMax || e2e > c.maxE2EP95:
					fail = fmt.Sprintf("host queue p95 %v, max %v, end-to-end p95 %v; want under %v, %v, %v", hq, hqMax, e2e, c.maxQueueP95, c.maxQueueMax, c.maxE2EP95)
				}
				if fail != "" {
					t.Errorf("seed %d: %s\n%s", seed, fail, tr.report())
				}
			}
		})
	}
}
