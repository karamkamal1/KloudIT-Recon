package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Elevated reports whether this process runs with an elevated token (the
// logon task's RunLevel Highest for an administrator).
func Elevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// AdminOnly returns an error unless only administrators and the system can
// change path, a file or folder on a local drive, and what runs from it: path
// and the folder it is in pass CheckPrivateSD (a folder also with the files
// directly in it, which is where Windows looks for the DLLs they load), and
// no folder above lets anyone else rename or replace its entries
// (checkAncestorSD). No component may be a link (reparse point), and path
// must exist (a missing one could be created later). The error names the part
// of path that fails.
func AdminOnly(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if v := filepath.VolumeName(path); len(v) != 2 || v[1] != ':' {
		return fmt.Errorf("%s: not on a local drive", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir, check := path, []string{path}
	if fi.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				check = append(check, filepath.Join(path, e.Name()))
			}
		}
	} else {
		dir = filepath.Dir(path)
		check = append(check, dir)
	}
	for _, p := range check {
		if err := checkPath(p, CheckPrivateSD); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	for a := filepath.Dir(dir); ; a = filepath.Dir(a) {
		if err := checkPath(a, checkAncestorSD); err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		if filepath.Dir(a) == a {
			return nil
		}
	}
}

// checkPath refuses a reparse point, else applies check to path's owner and
// DACL.
func checkPath(path string, check func(*windows.SECURITY_DESCRIPTOR) error) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("it is a link (reparse point)")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return check(sd)
}

// fileWriteRights are the rights that let a holder change a file or folder,
// its contents (a folder's FILE_ADD_FILE/FILE_ADD_SUBDIRECTORY are
// FILE_WRITE_DATA/FILE_APPEND_DATA) or its ACL.
const fileWriteRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | fileDeleteChild |
	windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL

// replaceRights are the rights on a folder above that let a holder replace
// what is below it: rename the folder itself (DELETE), delete or rename its
// entries (FILE_DELETE_CHILD), or grant itself either. Adding new entries
// (Authenticated Users may create folders in C:\) replaces nothing.
const replaceRights = windows.DELETE | fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL

const fileDeleteChild = 0x40 // FILE_DELETE_CHILD

// TrustedInstallerSID is NT SERVICE\TrustedInstaller.
const TrustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// privilegedSID: Administrators, SYSTEM, TrustedInstaller, or a placeholder
// for the owner (CREATOR OWNER, OWNER RIGHTS), which must be one of these.
func privilegedSID(s *windows.SID) bool {
	return s.IsWellKnown(windows.WinBuiltinAdministratorsSid) || s.IsWellKnown(windows.WinLocalSystemSid) ||
		s.IsWellKnown(windows.WinCreatorOwnerSid) || s.IsWellKnown(windows.WinCreatorOwnerRightsSid) ||
		s.String() == TrustedInstallerSID
}

// CheckPrivateSD checks that only administrators and the system can change a
// file or folder: owned by Administrators, SYSTEM or TrustedInstaller, and no
// ACE gives anyone else write, delete or permission rights. Inherit-only ACEs
// count too: they become the ACL of the files created in a folder.
func CheckPrivateSD(sd *windows.SECURITY_DESCRIPTOR) error {
	return checkSD(sd, fileWriteRights, true)
}

// checkAncestorSD checks a folder above a checked path: owned as for
// CheckPrivateSD, and no ACE that applies to the folder itself gives anyone
// else replaceRights.
func checkAncestorSD(sd *windows.SECURITY_DESCRIPTOR) error {
	return checkSD(sd, replaceRights, false)
}

func checkSD(sd *windows.SECURITY_DESCRIPTOR, rights uint32, inheritOnly bool) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("no owner (%v)", err)
	}
	if owner.IsWellKnown(windows.WinCreatorOwnerSid) || owner.IsWellKnown(windows.WinCreatorOwnerRightsSid) || !privilegedSID(owner) {
		return fmt.Errorf("owned by %s", sidName(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("no DACL: everyone has full access")
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("ACE %d: %w", i, err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, aceTypeAllowedCallback:
		case windows.ACCESS_DENIED_ACE_TYPE, aceTypeDeniedObject, aceTypeDeniedCallback, aceTypeDeniedCallbackObject:
			continue
		default:
			return fmt.Errorf("ACE %d has type %d", i, ace.Header.AceType)
		}
		if !inheritOnly && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if uint32(ace.Mask)&rights != 0 && !privilegedSID(sid) {
			return fmt.Errorf("%s may change it (access mask %#x)", sidName(sid), uint32(ace.Mask))
		}
	}
	return nil
}

// ACE types beyond the two x/sys names (winnt.h).
const (
	aceTypeDeniedObject         = 6
	aceTypeAllowedCallback      = 9 // same layout as ACCESS_ALLOWED_ACE up to SidStart
	aceTypeDeniedCallback       = 10
	aceTypeDeniedCallbackObject = 12
)

// sidName is DOMAIN\name, else the SID string.
func sidName(s *windows.SID) string {
	if account, domain, _, err := s.LookupAccount(""); err == nil {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return s.String()
}
