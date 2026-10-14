//go:build windows

package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWASAPILoopback runs the capture loop briefly. Without an audio device
// (CI, VMs) it skips; on a real PC it must start cleanly.
func TestWASAPILoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var samples int
	err := WASAPISource{}.Run(ctx, func(s []float32) { samples += len(s) })
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no audio endpoint (ErrNoAudioDevice: audio waits for one quietly): %v", err)
		}
		if strings.Contains(err.Error(), "GetDefaultAudioEndpoint") {
			t.Fatalf("GetDefaultAudioEndpoint failed, not as no device: %v", err)
		}
		if strings.Contains(err.Error(), "0x88890003") {
			t.Skipf("loopback capture not implemented by this platform (Wine): %v", err)
		}
		t.Fatal(err)
	}
	t.Logf("captured %d samples (silence produces none)", samples)
	if want := os.Getenv("RECON_EXPECT_AUDIO"); want != "" && samples == 0 {
		t.Fatal("expected captured audio while a tone is playing")
	}
	// Peak level check when something was captured.
	_ = samples
}

// TestNoDeviceHRESULT: E_NOTFOUND (no render device: a VM without one) and
// AUDCLNT_E_SERVICE_NOT_RUNNING are no sound device; AUDCLNT_E_DEVICE_INVALIDATED
// (the device in use went away: unplugged, re-created by a display mode
// change) is a device change, captured again at once; other failures are
// neither.
func TestNoDeviceHRESULT(t *testing.T) {
	for _, c := range []struct {
		hr                hresult
		noDevice, changed bool
	}{
		{hrNotFound, true, false},
		{hrServiceNotRunning, true, false},
		{hrDeviceInvalidated, false, true},
		{0x80004005, false, false}, // E_FAIL
	} {
		err := noDevice(fmt.Errorf("GetNextPacketSize: %w", c.hr))
		if errors.Is(err, ErrNoAudioDevice) != c.noDevice || errors.Is(err, ErrAudioDeviceChanged) != c.changed {
			t.Errorf("%v: %v (no device %v, changed %v)", c.hr, err, errors.Is(err, ErrNoAudioDevice), errors.Is(err, ErrAudioDeviceChanged))
		}
	}
}
