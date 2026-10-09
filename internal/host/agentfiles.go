package host

import (
	"fmt"
	"path/filepath"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// The files the agent writes. On Windows the logon task runs the agent
// elevated, but the folder of host.json (%APPDATA%\KlouditRecon) belongs to
// the user: any program the user runs could turn it or its files into links
// (a junction to \RPC Control and object manager symbolic links) that make the
// elevated agent create, append to, replace (a rename over) or delete files
// anywhere, a silent way past UAC as for the code paths in codepath.go. So an
// elevated agent writes its files, host.log with its rotation (cmd/recon-host)
// and the virtual display's restore journal, only in a folder that only
// administrators can change (platform.AdminOnly): install-host.ps1 creates
// %ProgramData%\KlouditRecon\<user> for administrators, with read access for
// the user, and points the logon task's -log there; the journal goes to that
// folder too (platform.AgentStateDir). It only reads host.json and
// live-bitrate.json from the user's folder (reading changes nothing; the
// settings that pick code are checked where they are used). An agent that is
// not elevated keeps the journal next to host.json, as before.

// elevatedStateDir is platform.AgentStateDir (tests replace it).
var elevatedStateDir = platform.AgentStateDir

// CheckAgentDir returns why an elevated agent must not write its files in dir
// (naming the part of it that fails), or nil; an agent that is not elevated
// may write anywhere.
func CheckAgentDir(dir string) error {
	if !runsElevated() {
		return nil
	}
	if err := adminOnly(dir); err != nil {
		return fmt.Errorf("%w (the agent runs elevated: it writes its files only in a folder only administrators can change, "+
			"such as the %%ProgramData%%\\KlouditRecon\\<user> that install-host.ps1 creates)", err)
	}
	return nil
}

// AgentFilesDir returns the folder for the files the agent of cfg writes
// besides its log (the virtual display's restore journal), or why it has
// none: an elevated agent's platform.AgentStateDir once CheckAgentDir accepts
// it, else the folder of the config file ("" without one; not created here).
func AgentFilesDir(cfg *Config) (string, error) {
	if !runsElevated() {
		if cfg.path == "" {
			return "", nil
		}
		return filepath.Dir(cfg.path), nil
	}
	dir, err := elevatedStateDir()
	if err != nil {
		return "", fmt.Errorf("the agent runs elevated and has no folder of its own: %w", err)
	}
	if err := CheckAgentDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}
