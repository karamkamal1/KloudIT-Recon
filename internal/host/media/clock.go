package media

import "math"

// Clock is the host clock: monotonic microseconds since it was created. Every
// host timestamp on the wire (frame send and stage times, pongs) uses it. On
// Windows it reads QueryPerformanceCounter, so the native helper's QPC
// timestamps convert to it exactly (FromQPC).
type Clock struct{ start int64 }

// NewHostClock starts a host clock.
func NewHostClock() *Clock { return &Clock{start: monoMicros()} }

// Now returns the host clock in µs.
func (c *Clock) Now() uint64 { return uint64(monoMicros() - c.start) }

// FromQPC converts a QueryPerformanceCounter reading taken on this machine
// (ticks at freq per second; another process, e.g. the native helper, reads
// the same counter) to the host clock, with the arithmetic monoMicros uses:
// on Windows the result is the host clock's own reading at that instant. ok is
// false for an unknown (0) or impossible reading (before the clock started).
func (c *Clock) FromQPC(ticks, freq int64) (us uint64, ok bool) {
	if ticks <= 0 || freq <= 0 {
		return 0, false
	}
	m := qpcMicros(ticks, freq) - c.start
	if m < 0 {
		return 0, false
	}
	return uint64(m), true
}

// qpcMicros converts QPC ticks to µs without overflowing int64 for any
// realistic uptime: whole seconds, then the remainder.
func qpcMicros(ticks, freq int64) int64 {
	return ticks/freq*1_000_000 + ticks%freq*1_000_000/freq
}

// NewClock returns the host clock as a function (NewHostClock().Now).
func NewClock() func() uint64 { return NewHostClock().Now }

// WallOffset returns the wall clock (µs since the Unix epoch: FFmpeg's
// av_gettime(), the domain of CaptureClockFilter, and the clock behind a
// browser's Date.now() on the same machine) minus clock(), from the tightest of
// a few back-to-back readings.
func WallOffset(clock func() uint64) int64 {
	best, span := int64(0), uint64(math.MaxUint64)
	for i := 0; i < 5; i++ {
		c0 := clock()
		w := wallMicros()
		c1 := clock()
		if c1-c0 < span {
			span = c1 - c0
			best = w - int64(c0+(c1-c0)/2)
		}
	}
	return best
}
