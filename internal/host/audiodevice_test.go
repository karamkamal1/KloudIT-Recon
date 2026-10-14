package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// noSoundDevice is a capture source on a PC without a sound output device.
type noSoundDevice struct{}

func (noSoundDevice) Name() string { return "wasapi-loopback" }

func (noSoundDevice) Run(context.Context, func([]float32)) error {
	return fmt.Errorf("%w: GetDefaultAudioEndpoint: HRESULT 0x80070490", media.ErrNoAudioDevice)
}

// TestAudioNoSoundDevice: on a VM without a sound output device the client
// is told that audio is off and why (its overlay shows it), with no notice:
// audio is optional and the stream needs none.
func TestAudioNoSoundDevice(t *testing.T) {
	a := gdiAgent(Config{Capture: "auto", Audio: true}, gdiTestCaps(noDevice, true))
	a.audioSource = noSoundDevice{}
	s, ctrl := gdiSession(a, nil)
	defer s.cancel()
	s.hello.Audio = proto.AudioCaps{Opus: true}
	s.startAudio()
	defer s.stopAudio()
	var cfgs []proto.AudioConfig
	for deadline := time.Now().Add(5 * time.Second); len(cfgs) < 2 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		cfgs = audioConfigs(t, ctrl)
	}
	if len(cfgs) != 2 || !cfgs[0].Enabled || cfgs[1].Enabled || cfgs[1].Reason != "no sound output device on the PC" {
		t.Fatalf("audio configs %+v", cfgs)
	}
	if n := ctrlNotices(t, ctrl); len(n) != 0 {
		t.Fatalf("notices %+v", n)
	}
}

// audioConfigs returns the audio configs the session sent.
func audioConfigs(t *testing.T, c *fakeCtrl) []proto.AudioConfig {
	t.Helper()
	var out []proto.AudioConfig
	for _, b := range ctrlMessages(t, c) {
		var m proto.AudioConfig
		if json.Unmarshal(b, &m) == nil && m.T == "audio" {
			out = append(out, m)
		}
	}
	return out
}

// ctrlMessages returns the messages the session sent on its control stream.
func ctrlMessages(t *testing.T, c *fakeCtrl) [][]byte {
	t.Helper()
	c.mu.Lock()
	r := bytes.NewReader(bytes.Clone(c.buf.Bytes()))
	c.mu.Unlock()
	var out [][]byte
	for {
		b, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
}
