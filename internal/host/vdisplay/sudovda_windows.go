//go:build windows

package vdisplay

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// sudovda drives SudoVDA through its device interface (sudovda.go has the
// protocol). The handle stays open while a monitor exists, for the watchdog
// pings.
type sudovda struct {
	log      *slog.Logger
	mu       sync.Mutex
	h        windows.Handle // 0 = closed
	version  sudovdaVersion
	watchdog uint32 // seconds, 0 = off
	plugged  bool
	id       monitorID
}

func newSudoVDA(log *slog.Logger) *sudovda { return &sudovda{log: log} }

func (d *sudovda) Name() string { return DriverSudoVDA }

// open opens the first device interface that answers with a compatible
// protocol version.
func (d *sudovda) open() error {
	if d.h != 0 {
		return nil
	}
	iface := windows.GUID(sudovdaInterface)
	paths, err := windows.CM_Get_Device_Interface_List("", &iface, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil || len(paths) == 0 {
		return ErrNoDriver
	}
	var errs []error
	for _, p := range paths {
		name, err := windows.UTF16PtrFromString(p)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("opening %s: %w", p, err))
			continue
		}
		d.h = h
		if err := d.handshake(); err != nil {
			d.closeLocked()
			errs = append(errs, err)
			continue
		}
		return nil
	}
	return errors.Join(errs...)
}

func (d *sudovda) handshake() error {
	out, err := d.ioctl(ioctlGetProtocolVersion, nil, sudovdaVersionSize)
	if err != nil {
		return fmt.Errorf("protocol version: %w", err)
	}
	if d.version, err = decodeSudovdaVersion(out); err != nil {
		return err
	}
	if !d.version.compatible() {
		return fmt.Errorf("driver protocol %v is not compatible with %v (update SudoVDA)", d.version, sudovdaClient)
	}
	out, err = d.ioctl(ioctlGetWatchdog, nil, sudovdaWatchdogSize)
	if err != nil {
		return fmt.Errorf("watchdog: %w", err)
	}
	d.watchdog, _, err = decodeSudovdaWatchdog(out)
	return err
}

func (d *sudovda) closeLocked() {
	if d.h != 0 {
		windows.CloseHandle(d.h)
		d.h = 0
	}
}

func (d *sudovda) ioctl(code uint32, in []byte, outSize int) ([]byte, error) {
	if d.h == 0 {
		return nil, errors.New("SudoVDA device not open")
	}
	out := make([]byte, max(outSize, 1))
	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	var n uint32
	if err := windows.DeviceIoControl(d.h, code, inPtr, uint32(len(in)), &out[0], uint32(outSize), &n, nil); err != nil {
		return nil, err
	}
	return out[:n], nil
}

// Detect re-opens the device unless a monitor is plugged, so it notices a
// driver that was removed or updated.
func (d *sudovda) Detect() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.plugged {
		d.closeLocked()
	}
	if err := d.open(); err != nil {
		return "", err
	}
	return fmt.Sprintf("protocol %v, watchdog %d s", d.version, d.watchdog), nil
}

func (d *sudovda) Plug(m Mode, id monitorID, render LUID) (plug, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.open(); err != nil {
		return plug{}, err
	}
	d.id = id
	// A monitor with this GUID left over (the driver's watchdog off, and the
	// agent stopped) would be returned as is by ADD, at its old mode.
	if _, err := d.ioctl(ioctlRemoveVirtualDisplay, encodeSudovdaRemove(id), 0); err == nil {
		d.log.Info("removed a leftover SudoVDA monitor")
		time.Sleep(500 * time.Millisecond)
	}
	if render != (LUID{}) {
		if _, err := d.ioctl(ioctlSetRenderAdapter, encodeLUID(render), 0); err != nil {
			d.log.Warn("SudoVDA: could not set the render adapter, the driver picks one", "luid", render, "err", err)
		}
	}
	out, err := d.ioctl(ioctlAddVirtualDisplay, encodeSudovdaAdd(m, id), sudovdaAddOutSize)
	if err != nil {
		return plug{}, fmt.Errorf("adding a %v monitor: %w", m, err)
	}
	t, err := decodeSudovdaAddOut(out)
	if err != nil {
		return plug{}, err
	}
	d.plugged = true
	return plug{Target: t, Departs: true}, nil
}

func (d *sudovda) Unplug(plug) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.plugged {
		return nil
	}
	d.plugged = false
	if _, err := d.ioctl(ioctlRemoveVirtualDisplay, encodeSudovdaRemove(d.id), 0); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("removing the SudoVDA monitor: %w", err)
	}
	return nil
}

func (d *sudovda) Keepalive() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.ioctl(ioctlDriverPing, nil, 0)
	return err
}

func (d *sudovda) KeepaliveEvery() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return sudovdaPingEvery(d.watchdog)
}

func (d *sudovda) Persistent() bool { return false }

func (d *sudovda) Recover(_ plug, id monitorID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.open(); err != nil {
		if errors.Is(err, ErrNoDriver) {
			return nil
		}
		return err
	}
	if _, err := d.ioctl(ioctlRemoveVirtualDisplay, encodeSudovdaRemove(id), 0); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("removing the SudoVDA monitor: %w", err)
	}
	return nil
}

func (d *sudovda) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.plugged {
		d.closeLocked()
	}
}
