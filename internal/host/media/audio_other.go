//go:build !windows

package media

// DefaultAudioSource returns the platform capture source. Non-Windows hosts
// stream a test tone (system audio capture is Windows-only for now).
func DefaultAudioSource() AudioSource { return ToneSource{} }
