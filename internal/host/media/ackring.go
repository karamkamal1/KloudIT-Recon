package media

// ackRing is recon-host's record of a helper generation's recent frames for
// ACK-based loss recovery (GUIDE 3.5): per frame id the long-term reference
// slot the encoder marked it into (-1: none) and whether the client
// acknowledged it (the frame ack datagram, which the client sends from its
// decoder's output: the frame decoded, from references it had). It answers
// which acknowledged long-term reference the encoder can recover from after a
// loss. Fixed size: frames more than ackRingSize ids older than the newest are
// forgotten. Not safe for concurrent use (HelperVideo.mu guards it).
type ackRing struct {
	e       [ackRingSize]ackEntry
	newest  uint64 // newest frame id added, 0: none
	lastKey uint64 // newest key frame: it emptied every LTR slot
}

type ackEntry struct {
	id    uint64 // 0: empty
	ltr   int    // LTR slot, -1: none
	acked bool
}

// ackRingSize covers about 2 s at 240 fps (8.5 s at 60 fps). ACKs arrive
// within a round trip, the helper marks an LTR about every 100 ms and keeps an
// unacknowledged mark at most 1 s, so recovery never needs older frames.
const ackRingSize = 512

func (r *ackRing) reset() { *r = ackRing{} }

// add records frame id as it came out of the encoder, in order: marked into
// LTR slot ltr (-1: none); key: a key frame, which clears every slot.
func (r *ackRing) add(id uint64, ltr int, key bool) {
	if id == 0 || id <= r.newest {
		return
	}
	r.e[id%ackRingSize] = ackEntry{id: id, ltr: ltr}
	r.newest = id
	if key {
		r.lastKey = id
	}
}

// ack marks frame id acknowledged. ltr: it is marked into an LTR slot and this
// is its first ACK (the helper needs those); known: the ring has the frame.
func (r *ackRing) ack(id uint64) (ltr, known bool) {
	e := &r.e[id%ackRingSize]
	if id == 0 || e.id != id {
		return false, false
	}
	first := !e.acked
	e.acked = true
	return first && e.ltr >= 0, true
}

// newestAckedLTR returns the newest acknowledged frame before frame id lost
// that the encoder still holds as a long-term reference: marked after the
// newest key frame, and its slot not marked again since (a slot holds the
// newest frame marked into it, also one after the loss, which the client does
// not have). ok false: none, a loss needs a key frame (or the helper's own
// choice).
func (r *ackRing) newestAckedLTR(lost uint64) (id uint64, ok bool) {
	var seen uint64 // slots whose current content was looked at
	for id := r.newest; id > r.lastKey && r.newest-id < ackRingSize; id-- {
		e := &r.e[id%ackRingSize]
		if e.id != id || e.ltr < 0 || e.ltr >= 64 || seen&(1<<e.ltr) != 0 {
			continue
		}
		seen |= 1 << e.ltr
		if e.acked && id < lost {
			return id, true
		}
	}
	return 0, false
}
