package platform

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	ti   = TrustedInstallerSID
	user = "S-1-5-21-1-2-3-1001"
)

func sdCheck(t *testing.T, name string, check func(*windows.SECURITY_DESCRIPTOR) error, cases []struct{ sddl, why string }) {
	t.Helper()
	for _, c := range cases {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatalf("%s: %v", c.sddl, err)
		}
		err = check(sd)
		if c.why == "" && err != nil || c.why != "" && (err == nil || !strings.Contains(err.Error(), c.why)) {
			t.Errorf("%s %s: %v, want %q", name, c.sddl, err, c.why)
		}
	}
}

// TestCheckPrivateSD: the check of the Virtual Display Driver's settings
// folder and of the places the elevated agent runs code from accepts the ACL
// the agent and the installer set and refuses one that lets non-administrators
// change the folder or the files created in it.
func TestCheckPrivateSD(t *testing.T) {
	const vddDir = "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;FRFX;;;BU)" // vdisplay's vddDirSDDL, install-host.ps1's Protect-AdminFolder
	sdCheck(t, "private", CheckPrivateSD, []struct{ sddl, why string }{
		{"O:BA" + vddDir, ""},
		{"O:SY" + vddDir, ""},
		{"O:BAD:P(D;;FA;;;WD)(A;OICI;FA;;;BA)(A;OICIIO;GA;;;CO)(A;;FA;;;" + ti + ")", ""},
		// C:\Program Files and a folder an elevated installer created in it.
		{"O:" + ti + "D:PAI(A;;FA;;;" + ti + ")(A;CIIO;GA;;;" + ti + ")(A;;0x1301bf;;;SY)(A;OICIIO;GA;;;SY)(A;;0x1301bf;;;BA)(A;OICIIO;GA;;;BA)(A;;0x1200a9;;;BU)(A;OICIIO;GXGR;;;BU)(A;OICIIO;GA;;;CO)(A;;0x1200a9;;;AC)(A;OICIIO;GXGR;;;AC)", ""},
		{"O:BAD:AI(A;ID;FA;;;" + ti + ")(A;OICIIOID;GA;;;SY)(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;0x1200a9;;;BU)(A;OICIIOID;GXGR;;;BU)(A;OICIIOID;GA;;;CO)", ""},
		// A folder created under C:\ without an explicit ACL: Authenticated
		// Users' Modify inherited from the drive root.
		{"O:BAD:AI(A;OICIID;FA;;;BA)(A;OICIID;FA;;;SY)(A;OICIID;0x1200a9;;;BU)(A;ID;0x1301bf;;;AU)(A;OICIIOID;SDGXGWGR;;;AU)", "may change it"},
		// Inherit-only: the ACL of the files created in it.
		{"O:BAD:P(A;OICI;FA;;;BA)(A;OICIIO;GW;;;BU)", "may change it"},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;;WD;;;WD)", "may change it"},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;;0x4;;;BU)", "may change it"}, // may add folders (and plant DLLs) in it
		{"O:BUD:P(A;OICI;FA;;;BA)", "owned by"},
		{"O:" + user + vddDir, "owned by"},
		{"O:CO" + vddDir, "owned by"},
		{"O:BA", "no DACL"},
	})
}

// TestCheckAncestorSD: a folder above the checked one may let others add
// entries (C:\) and pass rights on to new children, but not rename or delete
// what is in it, or itself.
func TestCheckAncestorSD(t *testing.T) {
	sdCheck(t, "ancestor", checkAncestorSD, []struct{ sddl, why string }{
		// C:\: Authenticated Users may create folders, and Modify on what they create.
		{"O:" + ti + "D:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)(A;OICIIO;SDGXGWGR;;;AU)(A;;0x4;;;AU)", ""},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;OICIIO;FA;;;BU)", ""},
		// C:\Program Files.
		{"O:" + ti + "D:PAI(A;;FA;;;" + ti + ")(A;CIIO;GA;;;" + ti + ")(A;;0x1301bf;;;SY)(A;OICIIO;GA;;;SY)(A;;0x1301bf;;;BA)(A;OICIIO;GA;;;BA)(A;;0x1200a9;;;BU)(A;OICIIO;GXGR;;;BU)(A;OICIIO;GA;;;CO)", ""},
		// A folder a user created under C:\ (Modify includes DELETE: rename it).
		{"O:BAD:AI(A;OICIID;FA;;;BA)(A;OICIID;FA;;;SY)(A;OICIID;0x1200a9;;;BU)(A;ID;0x1301bf;;;AU)(A;OICIIOID;SDGXGWGR;;;AU)", "may change it"},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user + ")", "may change it"}, // FILE_DELETE_CHILD
		{"O:BAD:P(A;OICI;FA;;;BA)(A;;0x40;;;BU)", "may change it"},             // FILE_DELETE_CHILD alone
		{"O:BAD:P(A;OICI;FA;;;BA)(A;;WD;;;BU)", "may change it"},               // may grant itself both
		{"O:" + user + "D:P(A;OICI;FA;;;BA)", "owned by"},                      // the user's profile folder
	})
}

// TestAdminOnly: paths a non-administrator can change, or that are not there
// yet, are refused. (On Windows the user's temp folder is the user's own;
// under Wine the file system may keep no security descriptors at all.)
func TestAdminOnly(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ffmpeg.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{exe, dir, filepath.Join(dir, "missing"), `\\server\share\ffmpeg.exe`} {
		if err := AdminOnly(p); err == nil {
			t.Errorf("AdminOnly(%s) accepted", p)
		} else {
			t.Logf("%s: %v", p, err)
		}
	}
}

// TestAdminOnlyPaths: for ffmpeg.exe AdminOnly checks the file, its folder
// and the other files in that folder (the DLLs of a shared FFmpeg build,
// which Windows loads from the exe's folder first), as for a folder, but not
// the folders in it; then every folder above up to the drive's root. Before,
// a file's sibling DLLs were not checked: one whose own ACL let the user
// write it passed.
func TestAdminOnlyPaths(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ffmpeg.exe")
	for _, f := range []string{exe, filepath.Join(dir, "avcodec-62.dll"), filepath.Join(dir, "avutil-60.dll")} {
		if err := os.WriteFile(f, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "presets"), 0o755); err != nil {
		t.Fatal(err)
	}
	var above []string
	for a := filepath.Dir(dir); ; a = filepath.Dir(a) {
		above = append(above, a)
		if filepath.Dir(a) == a {
			break
		}
	}
	dlls := []string{filepath.Join(dir, "avcodec-62.dll"), filepath.Join(dir, "avutil-60.dll")}
	for _, c := range []struct {
		path    string
		private []string
	}{
		{exe, append([]string{exe, dir}, dlls...)},
		{dir, append([]string{dir}, append(dlls, exe)...)},
	} {
		private, ancestors, err := adminOnlyPaths(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(private)), slices.Sorted(slices.Values(c.private))) || private[0] != c.path {
			t.Errorf("%s: checks %q, want %q (the path first)", c.path, private, c.private)
		}
		if !slices.Equal(ancestors, above) {
			t.Errorf("%s: folders above %q, want %q", c.path, ancestors, above)
		}
	}
}
