//go:build windows

package host

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// readFirewall asks Windows (PowerShell's NetSecurity and NetConnection
// modules) for the connected networks' profiles, the firewall profiles and
// the enabled inbound rules that cover UDP port for exe (firewall.go).
func readFirewall(ctx context.Context, port int, exe string) (fwState, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", firewallScript(port, exe))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW} // no console window flashing up
	out, err := cmd.Output()
	if err != nil {
		return fwState{}, fmt.Errorf("powershell: %w", err)
	}
	return parseFirewall([]byte(strings.TrimSpace(string(out))))
}
