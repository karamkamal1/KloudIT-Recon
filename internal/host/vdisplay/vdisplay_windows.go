//go:build windows

package vdisplay

import "log/slog"

// New returns a Manager for the drivers installed on this PC (SudoVDA
// preferred over the Virtual Display Driver).
func New(opts Options) *Manager {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return newManager(opts, winSystem{}, []driver{newSudoVDA(log), newVDD(log)})
}
