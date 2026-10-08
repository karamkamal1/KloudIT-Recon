//go:build !windows

package vdisplay

// New returns a Manager that has no drivers: Detect reports ErrUnsupported
// and Create fails with it.
func New(opts Options) *Manager { return newManager(opts, nil, nil) }
