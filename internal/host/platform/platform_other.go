//go:build !windows

package platform

// EnableDPIAwareness is a no-op outside Windows.
func EnableDPIAwareness() {}

// Monitors returns a single virtual display on non-Windows hosts (test source
// or x11grab of the configured display).
func Monitors() ([]Monitor, error) {
	return []Monitor{{Index: 0, Name: "Display", W: 1280, H: 720, Primary: true, Hz: 60, DXGIOutput: -1}}, nil
}

// PrimaryAdapter is unsupported outside Windows.
func PrimaryAdapter() (Adapter, error) { return Adapter{}, ErrUnsupported }

// GetCursor is unsupported outside Windows.
func GetCursor() (CursorState, error) { return CursorState{}, ErrUnsupported }

// CursorImage is unsupported outside Windows.
func CursorImage(uint64) (*CursorShape, error) { return nil, ErrUnsupported }

// Gamepads is a stub outside Windows.
type Gamepads struct{}

// OpenGamepads is unsupported outside Windows.
func OpenGamepads(func(idx int, large, small uint8)) (*Gamepads, error) { return nil, ErrUnsupported }

func (*Gamepads) Update(int, Pad) error { return ErrUnsupported }
func (*Gamepads) Unplug(int)            {}
func (*Gamepads) Close()                {}

// RaisePriority is a no-op outside Windows.
func RaisePriority() {}

// Elevated is false outside Windows: the agent runs elevated only from the
// Windows logon task.
func Elevated() bool { return false }

// AdminOnly is unsupported outside Windows.
func AdminOnly(string) error { return ErrUnsupported }

// AgentStateDir is unsupported outside Windows (the agent is never elevated
// there).
func AgentStateDir() (string, error) { return "", ErrUnsupported }

// DisplayRequest keeps the display on while set: Windows only.
type DisplayRequest struct{}

// NewDisplayRequest is unsupported outside Windows.
func NewDisplayRequest(string) (*DisplayRequest, error) { return nil, ErrUnsupported }

// Set does nothing outside Windows.
func (*DisplayRequest) Set(bool) error { return nil }

// Close does nothing outside Windows.
func (*DisplayRequest) Close() {}
