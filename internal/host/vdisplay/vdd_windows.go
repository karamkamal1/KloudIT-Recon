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
	state := "running"
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
		} else {
			modes = err.Error()
		}
	}
	return fmt.Sprintf("device %s %s, %s", dev.instance, state, modes), nil
}

// ensureMode makes vdd_settings.xml offer m; changed reports a rewrite.
func (d *vdd) ensureMode(m Mode) (changed bool, err error) {
	path := vddSettingsPath()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	if _, err := os.Stat(path + vddBackupSuffix); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(path+vddBackupSuffix, b, 0o644)
	}
	d.log.Info("Virtual Display Driver: adding the client's mode to its settings", "path", path, "mode", m)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, nb, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
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
