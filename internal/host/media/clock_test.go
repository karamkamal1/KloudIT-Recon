package media

import (
	"testing"
	"time"
)

// TestClock checks the host clock's resolution and rate, and that the wall
// clock offset used for FFmpeg's RTCTIME stamps is stable.
func TestClock(t *testing.T) {
	clock := NewClock()
	// Resolution: consecutive readings must advance in well under a timer tick.
	a := clock()
	b := a
	for spins := 0; b == a && spins < 1e7; spins++ {
		b = clock()
	}
	if b-a > 200 {
		t.Fatalf("clock advanced in a %d µs step (coarse timer?)", b-a)
	}
	off1 := wallOffset(clock)
	t0, c0 := time.Now(), clock()
	time.Sleep(200 * time.Millisecond)
	el, dc := time.Since(t0).Microseconds(), int64(clock()-c0)
	// The reference (Go's monotonic time) is itself tick-coarse on Windows.
	if d, tol := dc-el, el/20+16000; d < -tol || d > tol {
		t.Fatalf("clock rate off: %d µs over %d µs", dc, el)
	}
	if d := wallOffset(clock) - off1; d < -1000 || d > 1000 {
		t.Fatalf("wall clock offset moved by %d µs", d)
	}
	t.Logf("step %d µs, offset drift over 200 ms %d µs", b-a, wallOffset(clock)-off1)
}
