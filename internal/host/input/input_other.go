//go:build !windows

package input

import (
	"encoding/json"
	"os"
	"sync"
)

// logBackend records injected events as JSON lines (to $RECON_INPUT_LOG when
// set). Non-Windows hosts are used for development and automated tests.
type logBackend struct {
	mu sync.Mutex
	f  *os.File
}

// NewBackend returns the logging backend.
func NewBackend() (Backend, error) {
	b := &logBackend{}
	if p := os.Getenv("RECON_INPUT_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		b.f = f
	}
	return b, nil
}

func (b *logBackend) write(v map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f == nil {
		return nil
	}
	line, _ := json.Marshal(v)
	_, err := b.f.Write(append(line, '\n'))
	return err
}

func (b *logBackend) Key(sc uint16, ext, down bool) error {
	return b.write(map[string]any{"ev": "key", "sc": sc, "ext": ext, "down": down})
}
func (b *logBackend) Button(btn uint8, down bool) error {
	return b.write(map[string]any{"ev": "button", "b": btn, "down": down})
}
func (b *logBackend) MoveRel(dx, dy int32) error {
	return b.write(map[string]any{"ev": "rel", "dx": dx, "dy": dy})
}
func (b *logBackend) MoveAbs(x, y uint16, t Rect) error {
	return b.write(map[string]any{"ev": "abs", "x": x, "y": y})
}
func (b *logBackend) Wheel(dy, dx int16) error {
	return b.write(map[string]any{"ev": "wheel", "dy": dy, "dx": dx})
}
func (b *logBackend) Text(s string) error {
	return b.write(map[string]any{"ev": "text", "s": s})
}
func (b *logBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f != nil {
		b.f.Close()
		b.f = nil
	}
}
