package media

import "math"

// NewClock returns the host clock: monotonic microseconds since the call.
// Every host timestamp on the wire (frame send and stage times, pongs) uses it.
func NewClock() func() uint64 {
	start := monoMicros()
	return func() uint64 { return uint64(monoMicros() - start) }
}

// wallOffset returns FFmpeg's wall clock (av_gettime(): µs since the Unix
// epoch, the domain of CaptureClockFilter) minus clock(), from the tightest of a
// few back-to-back readings.
func wallOffset(clock func() uint64) int64 {
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
