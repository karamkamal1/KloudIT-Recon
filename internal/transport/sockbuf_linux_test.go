//go:build linux

package transport

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestSocketBuffers: the buffers the kernel accepted are read back, and a
// request over net.core.rmem_max is reported capped at it.
func TestSocketBuffers(t *testing.T) {
	b, err := os.ReadFile("/proc/sys/net/core/rmem_max")
	if err != nil {
		t.Skip(err)
	}
	rmax, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || rmax < 64<<10 {
		t.Skipf("rmem_max %q", b)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := SetSocketBuffers(c, 64<<10); got.Read != 64<<10 || got.Write != 64<<10 || got.Capped() || !got.Short() {
		t.Fatalf("64 kB asked: %+v capped %v short %v, want not capped, short", got, got.Capped(), got.Short())
	}
	if got := SetSocketBuffers(c, 2*rmax); got.Read != rmax || !got.Capped() || got.Short() != (rmax < SocketBufferMin) {
		t.Fatalf("twice rmem_max (%d) asked: %+v capped %v short %v, want the read buffer capped at rmem_max", rmax, got, got.Capped(), got.Short())
	}
}
