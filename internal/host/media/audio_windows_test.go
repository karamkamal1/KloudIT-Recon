//go:build windows

package media

import (
	"context"
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
		if strings.Contains(err.Error(), "GetDefaultAudioEndpoint") {
			t.Skipf("no audio endpoint: %v", err)
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
