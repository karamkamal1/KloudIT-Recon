//go:build windows

package vdisplay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestCreatePrivateDir: the folder the agent creates passes its own check
// (on Windows; under Wine only where the file system keeps security
// descriptors).
func TestCreatePrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "VirtualDisplayDriver")
	if err := vddCreateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := vddCreateDir(dir); err != nil { // exists: left alone
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sd.String(), "D:P") {
		t.Skipf("the file system did not keep the folder's security descriptor (%s)", sd)
	}
	file := filepath.Join(dir, vddSettingsFile)
	if err := os.WriteFile(file, newVDDSettings(mode1440), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := vddCheckPrivate(dir, file, file+".missing"); err != nil {
		t.Fatal(err)
	}
}
