package host

import (
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Temporal SVC thinning (GUIDE 9 order 2, Phase 5). A frame that no other
// frame references (media.Frame.Discardable: the enhancement layer of the
// native helper's two-layer SVC stream, Frame.Droppable; on the FFmpeg path a
// non-reference frame, codec.Params.Discardable) can be left out without
// breaking the decoding of any other frame. Under congestion frameSender
// leaves such frames out before they are sent (thin): the frame rate halves
// at once, with no corruption, no key frame and no encoder change, and comes
// back with the next frame sent once the pressure is gone. Pressure is any
// of: the rate controller's last report over its delay target (overTarget),
// the frame queue building (thinQueued frames wait behind the one taken),
// the loss-recovery ladder's deadline pressure (a frame written past its
// deadline, on a stream or as shards: sendState.slow), or the test hook's
// simulated congestion (thin=every:N:for:M).
//
// A thinned frame is no loss: it gets no "dropped" report, no Recover and no
// ladder rung, and the client acknowledges nothing for it. Every frame sent
// after it carries the frame extension's ExtThinned mask of the frames the
// host left out among the 32 before it, so the client skips their seqs at
// once instead of waiting for them or recovering; a loss the client reports
// from a thinned seq ({"t":"lost"}) is moved to the first frame that was
// sent. Only clients with hello v >= proto.HelloVersionThinned are thinned
// (older ones would take the gap for a loss) and only with host config "svc"
// auto. The rate controller holds its increases while frames are thinned and
// decreases when thinning lasts (bitrate.go): thinning answers a short spike,
// the bitrate a lasting shortage.

// thinQueued is how many newer frames waiting behind the one frameSender
// takes count as a building frame queue.
const thinQueued = 2

// thinKeep is how many thinned seqs of the current generation are kept: the
// ExtThinned mask reaches back 32 frames, the client's loss reports a little
// further.
const thinKeep = 64

// thinState is the session's thinning bookkeeping: the live generation's
// frames left out recently (the frame header mask, the client's loss
// reports) and the current episode (frames thinned at most thinEpisodeGap
// apart), for the log. Safe for concurrent use.
type thinState struct {
	mu    sync.Mutex
	gen   uint8
	seqs  []uint32 // thinned seqs of gen, oldest first
	since time.Time
	last  time.Time
	n     int    // frames thinned in the episode
	why   string // what started it
	open  bool   // an episode is under way (not yet logged as ended)
}

// left records frame (gen, seq) as left out at now because of why; it
// reports whether that began a new episode.
func (t *thinState) left(gen uint8, seq uint32, now time.Time, why string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		t.gen, t.seqs = gen, t.seqs[:0]
	}
	if len(t.seqs) == thinKeep {
		t.seqs = append(t.seqs[:0], t.seqs[1:]...)
	}
	t.seqs = append(t.seqs, seq)
	start := !t.open
	if start {
		t.open, t.since, t.n, t.why = true, now, 0, why
	}
	t.last = now
	t.n++
	return start
}

// ended returns the episode that ended by now (no frame thinned for
// thinEpisodeGap), once.
func (t *thinState) ended(now time.Time) (frames int, dur time.Duration, why string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open || now.Sub(t.last) <= thinEpisodeGap {
		return 0, 0, "", false
	}
	t.open = false
	return t.n, t.last.Sub(t.since), t.why, true
}

// mask returns frame (gen, seq)'s ExtThinned mask: bit i set when seq - 1 - i
// was left out.
func (t *thinState) mask(gen uint8, seq uint32) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var m uint32
	if gen != t.gen {
		return 0
	}
	for _, s := range t.seqs {
		if s < seq && seq-s <= 32 {
			m |= 1 << (seq - 1 - s)
		}
	}
	return m
}

// firstSent returns the first seq from seq on of generation gen that was not
// left out (a loss the client reports from a thinned frame is a loss of the
// next frame sent, if any).
func (t *thinState) firstSent(gen uint8, seq uint32) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return seq
	}
	for again := true; again; {
		again = false
		for _, s := range t.seqs {
			if s == seq {
				seq++
				again = true
			}
		}
	}
	return seq
}

// thinOK reports whether the session thins at all: host config "svc" auto
// and a client that reads ExtThinned.
func (s *Session) thinOK() bool {
	return s.a.cfg != nil && s.a.cfg.svc() && s.hello.V >= proto.HelloVersionThinned
}

// thinPressure returns why the path counts as congested for the nth frame
// frameSender takes (thinning), or "".
func (s *Session) thinPressure(n uint64, now time.Time) string {
	switch {
	case s.a.faults.thinAt(n):
		return "test fault"
	case s.rate.overTarget():
		return "delay"
	case len(s.frameQ) >= thinQueued:
		return "queue"
	case s.send.slow(now):
		return "deadline"
	}
	return ""
}

// thin decides about the nth frame frameSender takes (before its stream is
// opened) and reports whether it leaves it out: a discardable frame of a
// session that thins, under pressure. It also logs the end of an episode.
func (s *Session) thin(f *media.Frame, n uint64) bool {
	now := time.Now()
	if frames, dur, why, ok := s.thinning.ended(now); ok {
		s.log.Info("thinning ended", "frames", frames, "duration_ms", dur.Milliseconds(), "why", why)
	}
	if !f.Discardable || f.Key || f.Recovery || !s.thinOK() {
		return false
	}
	why := s.thinPressure(n, now)
	if why == "" {
		return false
	}
	if s.thinning.left(f.Gen, f.Seq, now, why) {
		s.log.Info("thinning: leaving out discardable frames under congestion", "gen", f.Gen, "seq", f.Seq, "why", why,
			"temporal_layer", f.TemporalLayer)
	}
	s.rate.thinned()
	s.stats.thinned.Add(1)
	return true
}
