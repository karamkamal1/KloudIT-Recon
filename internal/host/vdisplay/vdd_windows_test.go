//go:build windows

package vdisplay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestCheckPrivateSD: the settings folder check accepts the ACL the agent and
// the installer set and refuses one that lets non-administrators change the
// folder or the files created in it.
func TestCheckPrivateSD(t *testing.T) {
	for _, c := range []struct {
		sddl string
		why  string // "" = accepted
	}{
		{"O:BA" + vddDirSDDL, ""},
		{"O:SY" + vddDirSDDL, ""},
		{"O:BAD:P(D;;FA;;;WD)(A;OICI;FA;;;BA)(A;OICIIO;GA;;;CO)(A;;FA;;;" + trustedInstallerSID + ")", ""},
		// A folder created under C:\ without an explicit ACL: Authenticated
		// Users' Modify inherited from the drive root.
		{"O:BAD:AI(A;OICIID;FA;;;BA)(A;OICIID;FA;;;SY)(A;OICIID;0x1200a9;;;BU)(A;ID;0x1301bf;;;AU)(A;OICIIOID;SDGXGWGR;;;AU)", "may change it"},
		// Inherit-only: the ACL of the files the agent creates in it.
		{"O:BAD:P(A;OICI;FA;;;BA)(A;OICIIO;GW;;;BU)", "may change it"},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;;WD;;;WD)", "may change it"},
		{"O:BUD:P(A;OICI;FA;;;BA)", "owned by"},
		{"O:CO" + vddDirSDDL, "owned by"},
		{"O:BA", "no DACL"},
	} {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatalf("%s: %v", c.sddl, err)
		}
		err = checkPrivateSD(sd)
		if c.why == "" && err != nil || c.why != "" && (err == nil || !strings.Contains(err.Error(), c.why)) {
			t.Errorf("%s: %v, want %q", c.sddl, err, c.why)
		}
	}
}

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
