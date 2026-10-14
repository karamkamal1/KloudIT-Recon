//go:build linux

package transport

import "golang.org/x/sys/unix"

// socketBuffers reads a socket's SO_RCVBUF and SO_SNDBUF (0: unknown): what
// the kernel accepted, half what it reports (it doubles a request for its
// own bookkeeping).
func socketBuffers(fd uintptr) (read, write int) {
	read, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
	write, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	return read / 2, write / 2
}
