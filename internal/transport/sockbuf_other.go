//go:build !unix && !windows

package transport

// socketBuffers cannot tell on this platform.
func socketBuffers(uintptr) (read, write int) { return 0, 0 }
