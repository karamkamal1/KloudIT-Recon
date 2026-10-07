//go:build windows

package encoder

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Launch starts recon-encoder.exe and waits for its capabilities.
//
// The frame ring is an unnamed file mapping and the frame-ready signal an
// unnamed auto-reset event, both created here and handed to the helper by
// handle inheritance (their values go on its command line). Go passes them in
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST, so no other child inherits them, and
// being unnamed no other process can open them. Control messages use the
// helper's stdin/stdout pipes for the same reason.
func Launch(opt Options) (*Helper, error) {
	opt = opt.withDefaults()
	if opt.Exe == "" {
		return nil, errors.New("encoder helper: no executable")
	}
	size, err := RingSize(opt.Slots, opt.SlotSize)
	if err != nil {
		return nil, err
	}

	mapping, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE,
		uint32(uint64(size)>>32), uint32(size), nil)
	if err != nil {
		return nil, fmt.Errorf("encoder helper: CreateFileMapping: %w", err)
	}
	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(mapping)
		return nil, fmt.Errorf("encoder helper: MapViewOfFile: %w", err)
	}
	// addr is a mapping that lives until release(), not Go memory.
	mem := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))), size)
	event, err := windows.CreateEvent(nil, 0, 0, nil) // auto-reset, not signalled
	if err != nil {
		windows.UnmapViewOfFile(addr)
		windows.CloseHandle(mapping)
		return nil, fmt.Errorf("encoder helper: CreateEvent: %w", err)
	}
	release := func() {
		windows.UnmapViewOfFile(addr)
		windows.CloseHandle(mapping)
		windows.CloseHandle(event)
	}
	if err := InitRing(mem, opt.Slots, opt.SlotSize); err != nil {
		release()
		return nil, err
	}
	ring, err := NewRing(mem, opt.Slots, opt.SlotSize)
	if err != nil {
		release()
		return nil, err
	}

	args := []string{
		fmt.Sprintf("--ring-handle=%#x", uintptr(mapping)),
		fmt.Sprintf("--ring-size=%d", size),
		fmt.Sprintf("--event-handle=%#x", uintptr(event)),
		"--backend=" + opt.Backend,
		"--log-level=" + opt.LogLevel,
	}
	args = append(args, opt.Args...)
	cmd := exec.Command(opt.Exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		CreationFlags:              windows.CREATE_NO_WINDOW | windows.ABOVE_NORMAL_PRIORITY_CLASS,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(mapping), syscall.Handle(event)},
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		release()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		release()
		return nil, err
	}

	// The handle list only accepts inheritable handles; keep them inheritable
	// just for the duration of CreateProcess.
	setInherit := func(on bool) {
		var flags uint32
		if on {
			flags = windows.HANDLE_FLAG_INHERIT
		}
		windows.SetHandleInformation(mapping, windows.HANDLE_FLAG_INHERIT, flags)
		windows.SetHandleInformation(event, windows.HANDLE_FLAG_INHERIT, flags)
	}
	setInherit(true)
	err = cmd.Start()
	setInherit(false)
	if err != nil {
		release()
		return nil, fmt.Errorf("encoder helper: starting %s: %w", opt.Exe, err)
	}

	exited := make(chan struct{})
	exitCode := -1
	go func() {
		if st, err := cmd.Process.Wait(); err == nil {
			exitCode = st.ExitCode()
		}
		close(exited)
	}()
	return newHelper(opt, conn{
		ctrlW:  stdin,
		ctrlR:  stdout,
		logR:   stderr,
		ring:   ring,
		wait:   func(d time.Duration) error { return waitEvent(event, d) },
		kill:   cmd.Process.Kill,
		exited: exited,
		exitCode: func() int {
			<-exited
			return exitCode
		},
		release: release,
	})
}

func waitEvent(event windows.Handle, d time.Duration) error {
	r, err := windows.WaitForSingleObject(event, uint32(d/time.Millisecond))
	switch r {
	case windows.WAIT_OBJECT_0, uint32(windows.WAIT_TIMEOUT):
		return nil
	default:
		if err == nil {
			err = fmt.Errorf("WaitForSingleObject returned %#x", r)
		}
		return err
	}
}
