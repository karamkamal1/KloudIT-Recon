//go:build windows

package media

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Go's monotonic clock on Windows reads the interrupt time, which advances only
// with the timer tick (0.5-15.6 ms): too coarse for per-stage latency.
// QueryPerformanceCounter has sub-microsecond resolution.
var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	qpc      = kernel32.NewProc("QueryPerformanceCounter")
	qpcFreq  = func() int64 {
		var f int64
		if r, _, _ := kernel32.NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
			return 0
		}
		return f
	}()
	monoBase = time.Now()
)

func monoMicros() int64 {
	if qpcFreq == 0 {
		return time.Since(monoBase).Microseconds()
	}
	var c int64
	qpc.Call(uintptr(unsafe.Pointer(&c)))
	return qpcMicros(c, qpcFreq)
}

// wallMicros reads the same clock as FFmpeg's av_gettime() in the MinGW-w64
// builds the installer downloads (gettimeofday, backed by
// GetSystemTimePreciseAsFileTime).
func wallMicros() int64 {
	var ft windows.Filetime
	windows.GetSystemTimePreciseAsFileTime(&ft)
	return ft.Nanoseconds() / 1000
}
