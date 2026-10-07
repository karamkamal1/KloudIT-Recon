//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// consoleAttach lets the GUI-subsystem build (recon-hostw.exe, used for the
// background task) print to the parent console when run from a terminal.
func consoleAttach() {
	if _, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE); err == nil {
		if fi, err := os.Stderr.Stat(); err == nil && fi != nil {
			return // already have a console or redirected output
		}
	}
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	attach := k32.NewProc("AttachConsole")
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS
	if r, _, _ := attach.Call(attachParent); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
}
