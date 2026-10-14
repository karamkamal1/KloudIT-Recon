//go:build windows

package transport

import "golang.org/x/sys/windows"

// socketBuffers reads a socket's SO_RCVBUF and SO_SNDBUF (0: unknown).
func socketBuffers(fd uintptr) (read, write int) {
	read, _ = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_RCVBUF)
	write, _ = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_SNDBUF)
	return read, write
}
