//go:build !windows

package media

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

// raisePriority: process and GPU scheduling priority are Windows-only ("" =
// not supported, nothing is logged).
func raisePriority(cmd *exec.Cmd, vendor, gpuMode string) (gpu string, host gpuHost, err error) {
	return "", gpuHost{}, nil
}

// gpuHostInfo: nothing is detected outside Windows.
func gpuHostInfo() gpuHost { return gpuHost{} }

// EnableGPUPriorityPrivilege is a no-op outside Windows.
func EnableGPUPriorityPrivilege() error { return nil }
