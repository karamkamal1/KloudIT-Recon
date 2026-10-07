//go:build windows

package media

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow keeps child processes from flashing a console window and gives the
// encoder elevated CPU priority so capture is not starved by the game.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.ABOVE_NORMAL_PRIORITY_CLASS,
	}
}

func raisePriority(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.SetPriorityClass(h, windows.HIGH_PRIORITY_CLASS)
}
