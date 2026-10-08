package media

import "testing"

// The ACK ring (GUIDE 3.5): which acknowledged long-term reference a loss can
// be recovered from.
func TestAckRing(t *testing.T) {
	var r ackRing
	newest := func(lost uint64) uint64 {
		t.Helper()
		id, ok := r.newestAckedLTR(lost)
		if !ok {
			return 0
		}
		return id
	}
	// 1 key (slot 0 marked into it: key frames are never recovered from),
	// 2 LTR slot 0, 3, 4 LTR slot 1, 5; later 6 LTR slot 0 (overwrites 2).
	add := func(id uint64, ltr int, key bool) {
		t.Helper()
		r.add(id, ltr, key)
		if r.newest != id {
			t.Fatalf("newest %d after adding %d", r.newest, id)
		}
	}
	add(1, 0, true)
	add(2, 0, false)
	add(3, -1, false)
	add(4, 1, false)
	add(5, -1, false)
	// Every ACK of a marked frame goes on to the helper, but a marked key
	// frame is no recovery point (the helper's LTR policy never uses one).
	if ltr, ok := r.ack(1); !ltr || !ok {
		t.Fatalf("ack of the marked key frame: ltr %v known %v", ltr, ok)
	}
	if got := newest(10); got != 0 {
		t.Fatalf("only the key frame acknowledged: %d, want none", got)
	}
	if ltr, ok := r.ack(2); !ltr || !ok {
		t.Fatalf("first ack of LTR frame 2: ltr %v known %v", ltr, ok)
	}
	if ltr, _ := r.ack(2); ltr {
		t.Fatal("a second ack of frame 2 is passed on again")
	}
	if ltr, ok := r.ack(3); ltr || !ok {
		t.Fatalf("ack of P frame 3: ltr %v known %v", ltr, ok)
	}
	if ltr, ok := r.ack(99); ltr || ok {
		t.Fatalf("ack of an unknown frame: ltr %v known %v", ltr, ok)
	}
	if got := newest(5); got != 2 {
		t.Fatalf("loss at 5: recover from %d, want 2", got)
	}
	if got := newest(2); got != 0 {
		t.Fatalf("loss at 2: recover from %d, want none (only frames before the loss)", got)
	}
	r.ack(4)
	if got := newest(5); got != 4 {
		t.Fatalf("loss at 5 with 2 and 4 acknowledged: %d, want 4", got)
	}
	if got := newest(4); got != 2 {
		t.Fatalf("loss at 4: %d, want 2", got)
	}
	// Frame 6 goes into slot 0, where 2 was: 2 is gone, 6 not acknowledged.
	add(6, 0, false)
	add(7, -1, false)
	if got := newest(7); got != 4 {
		t.Fatalf("loss at 7: %d, want 4 (2's slot holds 6 now)", got)
	}
	r.ack(6)
	if got := newest(8); got != 6 {
		t.Fatalf("loss at 8 with 6 acknowledged: %d, want 6", got)
	}
	if got := newest(6); got != 4 {
		t.Fatalf("loss at 6: %d, want 4 (slot 0 holds 6, which the client lost)", got)
	}
	// Frame 8 overwrites slot 1 (frame 4) unacknowledged; a loss at 8 still
	// recovers from 6.
	add(8, 1, false)
	if got := newest(9); got != 6 {
		t.Fatalf("loss at 9: %d, want 6", got)
	}
	r.ack(8)
	// A key frame empties every slot.
	add(9, -1, true)
	add(10, -1, false)
	if got := newest(11); got != 0 {
		t.Fatalf("after a key frame: %d, want none", got)
	}
	add(11, 1, false)
	r.ack(11)
	if got := newest(12); got != 11 {
		t.Fatalf("an LTR after the key frame: %d, want 11", got)
	}
	// Frames out of order or 0 are ignored.
	add(12, -1, false)
	r.add(5, 0, false)
	r.add(0, 0, false)
	if r.newest != 12 || newest(13) != 11 {
		t.Fatalf("after stale adds: newest %d, recover from %d", r.newest, newest(13))
	}
	// Old frames fall out of the ring.
	for id := uint64(13); id < 13+ackRingSize; id++ {
		r.add(id, -1, false)
	}
	if got := newest(13 + ackRingSize); got != 0 {
		t.Fatalf("frame 11 is %d frames old: %d, want none", ackRingSize, got)
	}
	if ltr, ok := r.ack(11); ltr || ok {
		t.Fatalf("ack of a forgotten frame: ltr %v known %v", ltr, ok)
	}
	r.reset()
	if r.newest != 0 || r.lastKey != 0 || newest(100) != 0 {
		t.Fatal("reset")
	}
}
