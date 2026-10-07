//go:build !windows

package media

import "time"

var monoBase = time.Now()

func monoMicros() int64 { return time.Since(monoBase).Microseconds() }

// wallMicros reads the same clock as FFmpeg's av_gettime() (gettimeofday).
func wallMicros() int64 { return time.Now().UnixMicro() }
