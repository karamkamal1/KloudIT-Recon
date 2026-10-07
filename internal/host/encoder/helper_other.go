//go:build !windows

package encoder

// Launch is only implemented on Windows: the helper captures and encodes with
// DXGI, AMF and NVENC.
func Launch(opt Options) (*Helper, error) { return nil, ErrNotSupported }
