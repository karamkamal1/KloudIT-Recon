//go:build windows

package vdisplay

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// guidDevClassDisplay is GUID_DEVCLASS_DISPLAY (the Display adapters class
// MttVDD.inf installs into).
var guidDevClassDisplay = windows.GUID{Data1: 0x4d36e968, Data2: 0xe325, Data3: 0x11ce, Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18}}

const cmProbDisabled = 22 // CM_PROB_DISABLED (cfg.h)

// vdd drives the Virtual Display Driver through PnP (vdd.go has why).
// Enabling, disabling and restarting the device needs the elevated agent.
type vdd struct {
	log *slog.Logger
}

func newVDD(log *slog.Logger) *vdd { return &vdd{log: log} }

func (d *vdd) Name() string { return DriverVDD }

// vddDevice is the present VDD device node.
type vddDevice struct {
	set      windows.DevInfo
	data     *windows.DevInfoData
	instance string
	running  bool
	disabled bool
	problem  uint32
}

func (v *vddDevice) close() { v.set.Close() }

// findVDD returns the first present device with VDD's hardware id.
func findVDD() (*vddDevice, error) {
	set, err := windows.SetupDiGetClassDevsEx(&guidDevClassDisplay, "", 0, windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil, fmt.Errorf("listing display devices: %w", err)
	}
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			break
		}
		if err != nil {
			continue
		}
		v, err := set.DeviceRegistryProperty(data, windows.SPDRP_HARDWAREID)
		ids, _ := v.([]string)
		if err != nil || !isVDDHardwareID(ids) {
			continue
		}
		dev := &vddDevice{set: set, data: data}
		dev.instance, _ = set.DeviceInstanceID(data)
		var status, problem uint32
		if err := windows.CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0); err == nil {
			dev.running = status&windows.DN_STARTED != 0
			if status&windows.DN_HAS_PROBLEM != 0 {
				dev.problem = problem
				dev.disabled = problem == cmProbDisabled
			}
		}
		return dev, nil
	}
	set.Close()
	return nil, ErrNoDriver
}

// change applies a DIF_PROPERTYCHANGE (enable, disable or restart).
func (v *vddDevice) change(state windows.DICS_STATE) error {
	params := windows.PropChangeParams{
		ClassInstallHeader: *windows.MakeClassInstallHeader(windows.DIF_PROPERTYCHANGE),
		StateChange:        state,
		Scope:              windows.DICS_FLAG_GLOBAL,
	}
	if err := v.set.SetClassInstallParams(v.data, &params.ClassInstallHeader, uint32(unsafe.Sizeof(params))); err != nil {
		return fmt.Errorf("device %s: %w", v.instance, err)
	}
	if err := v.set.CallClassInstaller(windows.DIF_PROPERTYCHANGE, v.data); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("device %s: %w (the agent must run elevated: the logon task does)", v.instance, err)
		}
		return fmt.Errorf("device %s: %w", v.instance, err)
	}
	if p, err := v.set.DeviceInstallParams(v.data); err == nil && p.Flags&(windows.DI_NEEDREBOOT|windows.DI_NEEDRESTART) != 0 {
		return fmt.Errorf("device %s needs a reboot to change state", v.instance)
	}
	return nil
}

// vddSettingsPath is vdd_settings.xml in the driver's directory (VDDPATH).
func vddSettingsPath() string {
	dir := vddDefaultDir
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, vddRegistryKey, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue(vddRegistryValue); err == nil && s != "" {
			dir = s
		}
		k.Close()
	}
	return filepath.Join(dir, vddSettingsFile)
}

func (d *vdd) Detect() (string, error) {
	dev, err := findVDD()
	if err != nil {
		return "", err
	}
	defer dev.close()
	// install-host.ps1 leaves the device disabled: a running one keeps its
	// monitor connected (and the desktop extended onto it) between sessions.
	state := "running (its monitor stays connected outside sessions: disable the device so sessions enable it)"
	switch {
	case dev.disabled:
		state = "disabled (enabled for sessions)"
	case dev.problem != 0:
		return "", fmt.Errorf("device %s has problem code %d (see Device Manager)", dev.instance, dev.problem)
	case !dev.running:
		state = "stopped"
	}
	path := vddSettingsPath()
	modes := "no settings file (created on first use)"
	if b, err := os.ReadFile(path); err == nil {
		if s, err := parseVDDSettings(b); err == nil {
			modes = fmt.Sprintf("%d modes, %d monitor(s) in %s", len(s.Modes), s.Count, path)
			if err := vddCheckPrivate(filepath.Dir(path), path); err != nil {
				modes += "; new modes cannot be added: " + err.Error()
			}
		} else {
			modes = err.Error()
		}
	}
	return fmt.Sprintf("device %s %s, %s", dev.instance, state, modes), nil
}

// ensureMode makes vdd_settings.xml offer m; changed reports a rewrite. The
// agent writes only into a folder that non-administrators cannot change
// (vddCheckPrivate): it runs elevated.
func (d *vdd) ensureMode(m Mode) (changed bool, err error) {
	path := vddSettingsPath()
	dir := filepath.Dir(path)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := vddCreateDir(dir); err != nil {
			return false, err
		}
		if err := vddCheckPrivate(dir, path); err != nil {
			return false, err
		}
		d.log.Info("Virtual Display Driver: creating its settings file", "path", path, "mode", m)
		return true, os.WriteFile(path, newVDDSettings(m), 0o644)
	case err != nil:
		return false, err
	}
	s, err := parseVDDSettings(b)
	if err != nil {
		return false, err
	}
	if s.has(m) {
		return false, nil
	}
	nb, err := addVDDMode(b, m)
	if err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := vddCheckPrivate(dir, path, path+vddBackupSuffix, tmp); err != nil {
		return false, err
	}
	if _, err := os.Stat(path + vddBackupSuffix); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(path+vddBackupSuffix, b, 0o644)
	}
	d.log.Info("Virtual Display Driver: adding the client's mode to its settings", "path", path, "mode", m)
	if err := os.WriteFile(tmp, nb, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// vddDirSDDL is the settings folder's access: administrators and SYSTEM full
// control, users (also the driver's LocalService host) read, not inherited from
// the drive root, whose "Authenticated Users: Modify" for new subfolders would
// let any signed-in user replace the files the elevated agent writes.
// install-host.ps1 sets the same.
const vddDirSDDL = "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;FRFX;;;BU)"

// vddCreateDir creates the settings folder with vddDirSDDL, owned by
// Administrators (its parent must exist). A folder that exists already is left
// to vddCheckPrivate.
func vddCreateDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString("O:BA" + vddDirSDDL)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, &sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}

// vddCheckPrivate refuses the settings folder (the first path) and the files
// in it the agent is about to write unless only administrators and the system
// can change them: no reparse point, owned by Administrators, SYSTEM or
// TrustedInstaller, and no ACE giving anyone else write, delete or permission
// rights. A path that does not exist is skipped (the folder's ACL decides who
// may create it).
func vddCheckPrivate(paths ...string) error {
	for _, path := range paths {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(p)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			continue
		}
		if err == nil && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			err = errors.New("it is a link (reparse point)")
		}
		if err == nil {
			var sd *windows.SECURITY_DESCRIPTOR
			sd, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err == nil {
				err = checkPrivateSD(sd)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w; the agent runs elevated and edits the Virtual Display Driver's settings only in a folder that only administrators can change: run install-host.ps1 -InstallVirtualDisplay again, or elevated: icacls \"%s\" /inheritance:r /grant:r *S-1-5-32-544:(OI)(CI)F *S-1-5-18:(OI)(CI)F *S-1-5-32-545:(OI)(CI)RX",
				path, err, paths[0])
		}
	}
	return nil
}

// fileWriteRights are the rights that let a holder change a file or folder,
// its contents (a folder's FILE_ADD_FILE/FILE_ADD_SUBDIRECTORY are
// FILE_WRITE_DATA/FILE_APPEND_DATA) or its ACL.
const fileWriteRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | fileDeleteChild |
	windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL

const fileDeleteChild = 0x40 // FILE_DELETE_CHILD

// trustedInstallerSID is NT SERVICE\TrustedInstaller.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// privilegedSID: Administrators, SYSTEM, TrustedInstaller, or a placeholder
// for the owner (CREATOR OWNER, OWNER RIGHTS), which must be one of these.
func privilegedSID(s *windows.SID) bool {
	return s.IsWellKnown(windows.WinBuiltinAdministratorsSid) || s.IsWellKnown(windows.WinLocalSystemSid) ||
		s.IsWellKnown(windows.WinCreatorOwnerSid) || s.IsWellKnown(windows.WinCreatorOwnerRightsSid) ||
		s.String() == trustedInstallerSID
}

// checkPrivateSD checks a file's or folder's owner and DACL for
// vddCheckPrivate. Inherit-only ACEs count too: they become the ACL of the
// files created in a folder.
func checkPrivateSD(sd *windows.SECURITY_DESCRIPTOR) error {
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
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if uint32(ace.Mask)&fileWriteRights != 0 && !privilegedSID(sid) {
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

func (d *vdd) Plug(m Mode, _ monitorID, _ LUID) (plug, error) {
	dev, err := findVDD()
	if err != nil {
		return plug{}, err
	}
	defer dev.close()
	if dev.problem != 0 && !dev.disabled {
		return plug{}, fmt.Errorf("device %s has problem code %d", dev.instance, dev.problem)
	}
	changed, err := d.ensureMode(m)
	if err != nil {
		return plug{}, fmt.Errorf("settings: %w", err)
	}
	p := plug{Instance: dev.instance}
	switch {
	case dev.disabled:
		// Enabling starts the device, which reads the settings.
		if err := dev.change(windows.DICS_ENABLE); err != nil {
			return plug{}, err
		}
		p.Departs = true
	case changed || !dev.running:
		// The driver reads its modes only when the device starts.
		if err := dev.change(windows.DICS_PROPCHANGE); err != nil {
			return plug{}, err
		}
	}
	return p, nil
}

func (d *vdd) Unplug(p plug) error {
	if !p.Departs {
		return nil
	}
	dev, err := findVDD()
	if err != nil {
		return nil // uninstalled meanwhile
	}
	defer dev.close()
	if dev.disabled {
		return nil
	}
	return dev.change(windows.DICS_DISABLE)
}

func (d *vdd) Keepalive() error              { return nil }
func (d *vdd) KeepaliveEvery() time.Duration { return 0 }
func (d *vdd) Persistent() bool              { return true }

func (d *vdd) Recover(p plug, _ monitorID) error { return d.Unplug(p) }

func (d *vdd) Close() {}
