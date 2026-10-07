//go:build !windows

package media

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

func raisePriority(cmd *exec.Cmd) {}
