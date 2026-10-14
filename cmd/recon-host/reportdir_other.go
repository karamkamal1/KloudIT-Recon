//go:build !windows

package main

// desktopDirs are the user's desktop folders to try: ~/Desktop.
func desktopDirs() []string { return homeDesktop() }
