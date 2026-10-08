//go:build windows

package platform

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// vigemIoctl is CTL_CODE(FILE_DEVICE_BUS_EXTENDER, IOCTL_VIGEM_BASE + n,
// METHOD_BUFFERED, access) as ViGEmClient's BusShared.h defines the codes.
func vigemIoctl(n, access uint32) uint32 { return 0x2A<<16 | access<<14 | (0x801+n)<<2 }

func TestViGEmIoctlCodes(t *testing.T) {
	const write, readWrite = 2, 3 // FILE_WRITE_DATA, FILE_READ_DATA | FILE_WRITE_DATA
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"PLUGIN_TARGET", ioctlPluginTarget, vigemIoctl(0x000, write)},
		{"UNPLUG_TARGET", ioctlUnplugTarget, vigemIoctl(0x001, write)},
		{"CHECK_VERSION", ioctlCheckVersion, vigemIoctl(0x002, write)},
		{"WAIT_DEVICE_READY", ioctlWaitDeviceReady, vigemIoctl(0x003, write)},
		{"XUSB_REQUEST_NOTIFICATION", ioctlXusbRequestNotification, vigemIoctl(0x200, readWrite)},
		{"XUSB_SUBMIT_REPORT", ioctlXusbSubmit, vigemIoctl(0x201, write)},
	} {
		if c.got != c.want {
			t.Errorf("IOCTL_%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

// fsctlPipeListen = CTL_CODE(FILE_DEVICE_NAMED_PIPE, 2, METHOD_BUFFERED,
// FILE_ANY_ACCESS): ConnectNamedPipe's request, which stays pending until a
// client connects, like a notification request until a game sets the motors.
const fsctlPipeListen = 0x110008

func pipeServer(t *testing.T, name string) windows.Handle {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE, 1, 512, 512, 0, nil)
	if err != nil {
		t.Skipf("CreateNamedPipe: %v", err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

// TestPadListener runs the force-feedback listener's overlapped request loop
// on a named pipe (no ViGEmBus here): a pending request that completes is
// passed to the callback, and end() cancels a pending one promptly, as an
// unplug or Close needs it.
func TestPadListener(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\recon-padtest-%d`, windows.GetCurrentProcessId())
	h := pipeServer(t, name)
	var calls atomic.Int32
	g := &Gamepads{h: h, onRumble: func(idx int, large, small uint8) {
		if idx == 2 && large == 0 && small == 0 {
			calls.Add(1)
		}
	}}
	io, err := newOverlappedIO()
	if err != nil {
		t.Fatal(err)
	}
	n := &padNotify{code: fsctlPipeListen, io: io, done: make(chan struct{})}
	go g.listen(2, 7, n)
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("callback before the request completed")
	}
	select {
	case <-n.done:
		t.Fatal("listener returned while its request should be pending")
	default:
	}
	// A client connects: the pending request completes once, the callback
	// runs; the next request on the connected pipe is refused at once.
	p, _ := windows.UTF16PtrFromString(name)
	c, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer windows.CloseHandle(c)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("callback ran %d times after the request completed, want 1", calls.Load())
	}
	start := time.Now()
	n.end(h)
	select {
	case <-n.done:
	default:
		t.Fatal("end returned before the listener did")
	}
	t.Logf("listener ended in %v after the refusals", time.Since(start))

	// A pending request is cancelled by end() at once, without a callback.
	h2 := pipeServer(t, name+"-2")
	g2 := &Gamepads{h: h2, onRumble: func(int, uint8, uint8) { t.Error("callback for a cancelled request") }}
	io2, err := newOverlappedIO()
	if err != nil {
		t.Fatal(err)
	}
	n2 := &padNotify{code: fsctlPipeListen, io: io2, done: make(chan struct{})}
	go g2.listen(0, 1, n2)
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	n2.end(h2)
	took := time.Since(start)
	select {
	case <-n2.done:
	default:
		t.Fatal("end returned before the listener did")
	}
	if took > 500*time.Millisecond {
		t.Fatalf("cancelling the pending request took %v", took)
	}
	t.Logf("pending request cancelled in %v", took)

	// overlappedIO.do reports a cancelled request as ERROR_OPERATION_ABORTED.
	h3 := pipeServer(t, name+"-3")
	io3, err := newOverlappedIO()
	if err != nil {
		t.Fatal(err)
	}
	defer io3.close()
	errc := make(chan error, 1)
	go func() { errc <- io3.do(h3, fsctlPipeListen, nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	_ = windows.CancelIoEx(h3, &io3.ov)
	select {
	case err := <-errc:
		if !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			t.Fatalf("cancelled request: %v, want ERROR_OPERATION_ABORTED", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request never returned")
	}
}
