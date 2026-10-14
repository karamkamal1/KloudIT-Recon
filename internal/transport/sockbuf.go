package transport

import (
	"log/slog"
	"net"
)

// SocketBuffers are a UDP socket's buffers as the kernel set them after a
// request for Want bytes each way (SetSocketBuffers): Read and Write, what
// it accepted (0: the platform cannot tell).
type SocketBuffers struct {
	Want, Read, Write int
}

// SocketBufferMin is the least receive and send buffer a video socket should
// have: a frame's burst at up to a gigabit (cc.Media) while its reader is
// descheduled for 10 ms, with the kernel's per-datagram bookkeeping
// (Linux charges about twice a datagram's size against the buffer).
const SocketBufferMin = 4 << 20

// Capped reports whether the kernel gave less than asked: Linux caps a
// process's request at net.core.rmem_max / wmem_max (212992 bytes by
// default, also inside an LXC container, whose limit is the node's;
// deploy/proxmox/create-lxc.sh raises the node's to 7.5 MB).
func (b SocketBuffers) Capped() bool {
	return b.Read > 0 && b.Read < b.Want || b.Write > 0 && b.Write < b.Want
}

// Short reports whether the kernel gave less than SocketBufferMin: a burst
// that arrives faster than the socket's reader takes it (a relay's goroutine
// descheduled for a few milliseconds) overflows the rest.
func (b SocketBuffers) Short() bool {
	return b.Read > 0 && b.Read < SocketBufferMin || b.Write > 0 && b.Write < SocketBufferMin
}

// SetSocketBuffers asks for want bytes of receive and send buffer on c and
// reads back what the kernel set.
func SetSocketBuffers(c *net.UDPConn, want int) SocketBuffers {
	_ = c.SetReadBuffer(want)
	_ = c.SetWriteBuffer(want)
	b := SocketBuffers{Want: want}
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { b.Read, b.Write = socketBuffers(fd) })
	}
	return b
}

// LogSocketBuffers logs b for the socket named what, as a warning where the
// kernel gave less than SocketBufferMin (docs/INSTALL.md, "UDP buffers").
func LogSocketBuffers(log *slog.Logger, what string, b SocketBuffers) {
	attrs := []any{"socket", what, "wanted_kb", b.Want >> 10, "rcvbuf_kb", b.Read >> 10, "sndbuf_kb", b.Write >> 10, "capped", b.Capped()}
	if b.Short() {
		log.Warn("UDP socket buffers capped by the kernel: bursts the socket's reader cannot take at once are dropped "+
			"(raise net.core.rmem_max and net.core.wmem_max on the machine, on a Proxmox node for its containers: docs/INSTALL.md, \"UDP buffers\")",
			attrs...)
		return
	}
	log.Info("UDP socket buffers", attrs...)
}
