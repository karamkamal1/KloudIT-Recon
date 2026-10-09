package gateway

import (
	"testing"
	"time"
)

// TestUserStreamsRevoke: revoking a user returns their open streams' ends,
// and refuses a stream whose ticket was issued before the revocation (taken
// before it, registered after) but not one issued after it.
func TestUserStreamsRevoke(t *testing.T) {
	var u userStreams
	issued := time.Now()
	removeA, ok := u.add("a", issued, func() {})
	if !ok {
		t.Fatal("refused before any revocation")
	}
	if _, ok := u.add("b", issued, func() {}); !ok {
		t.Fatal("refused before any revocation")
	}
	if ends := u.revoke("a"); len(ends) != 1 {
		t.Fatalf("%d ends for a, want 1", len(ends))
	}
	if _, ok := u.add("a", issued, func() {}); ok {
		t.Error("a stream from a ticket issued before the revocation was registered")
	}
	if _, ok := u.add("b", issued, func() {}); !ok {
		t.Error("another user's stream refused")
	}
	time.Sleep(time.Millisecond) // (coarse clocks)
	if _, ok := u.add("a", time.Now(), func() {}); !ok {
		t.Error("a stream from a ticket issued after the revocation refused")
	}
	removeA()
	if ends := u.revoke("a"); len(ends) != 1 {
		t.Fatalf("%d ends for a after one was removed, want 1", len(ends))
	}
}
