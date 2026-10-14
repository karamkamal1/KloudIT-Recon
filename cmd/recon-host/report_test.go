package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReportDir: the report goes to the user's desktop (on Windows the
// Desktop known folder first, which OneDrive's backup moves), else to the
// temporary folder, never the current directory (C:\Windows\System32 in an
// elevated PowerShell).
func TestReportDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		dirs := desktopDirs()
		if len(dirs) == 0 || dirs[0] == "" {
			t.Fatalf("desktop folders %q", dirs)
		}
		d := reportDir()
		if st, err := os.Stat(d); err != nil || !st.IsDir() || d == "." {
			t.Fatalf("report folder %q: %v", d, err)
		}
		t.Logf("desktop folders %q, report folder %q", dirs, d)
		return
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if d := reportDir(); d != os.TempDir() {
		t.Fatalf("no desktop: %q, want the temporary folder %q", d, os.TempDir())
	}
	desk := filepath.Join(home, "Desktop")
	if err := os.Mkdir(desk, 0o700); err != nil {
		t.Fatal(err)
	}
	if d := reportDir(); d != desk {
		t.Fatalf("with a desktop: %q, want %q", d, desk)
	}
}
