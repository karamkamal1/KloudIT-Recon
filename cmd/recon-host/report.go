package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// reportCmd writes "recon-host report": one text file with what a problem on
// this PC needs (host.WriteDiagnostics), by default on the user's desktop,
// and says where. It has no tokens, keys or passwords.
func reportCmd(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	out := fs.String("o", "", "the report file (default: recon-report-<PC>-<time>.txt on your desktop, else here)")
	lines := fs.Int("lines", 400, "how many of host.log's last lines")
	hostLog := fs.String("hostlog", "", "the agent's log (default: the one the logon task writes, %ProgramData%\\KlouditRecon\\<user>\\host.log)")
	quick := fs.Bool("quick", false, "leave out the probe (FFmpeg, encoders, helper, direct path, CPU measurement: up to a minute)")
	_ = fs.Parse(args)

	o := host.DiagnosticsOptions{ConfigPath: cfgPath, LogPath: *hostLog, LogLines: *lines, Probe: !*quick}
	o.Config, o.ConfigErr = host.LoadConfig(cfgPath)
	if o.LogPath == "" {
		o.LogPath = defaultHostLog()
	}
	path := *out
	if path == "" {
		name, _ := os.Hostname()
		path = filepath.Join(reportDir(), fmt.Sprintf("recon-report-%s-%s.txt", name, time.Now().Format("20060102-150405")))
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Writing the report (the probe takes up to a minute)...")
	err = host.WriteDiagnostics(context.Background(), o, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("Report written to %s\nIt has no tokens, keys or passwords. Send it with a description of the problem.\n", path)
	return 0
}

// defaultHostLog is where the logon task's agent writes host.log
// (install-host.ps1: %ProgramData%\KlouditRecon\<user>), else host.log next
// to this executable; "" when neither exists.
func defaultHostLog() string {
	var dirs []string
	if d, err := platform.AgentStateDir(); err == nil {
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		p := filepath.Join(d, "host.log")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// reportDir is the user's desktop where there is one, else the current
// directory.
func reportDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, "Desktop")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "."
}
