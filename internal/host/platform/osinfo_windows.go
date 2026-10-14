//go:build windows

package platform

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// OSInfo describes Windows (edition, version, build) and the machine as its
// firmware names it: a VM shows its hypervisor there (QEMU / Proxmox
// "Standard PC (Q35 + ICH9, 2009)", Hyper-V "Virtual Machine", VMware,
// VirtualBox), for diagnostics reports.
func OSInfo() (os, machine string) {
	str := func(k registry.Key, name string) string {
		v, _, err := k.GetStringValue(name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		ubr, _, _ := k.GetIntegerValue("UBR")
		os = fmt.Sprintf("%s %s (build %s.%d)", str(k, "ProductName"), str(k, "DisplayVersion"), str(k, "CurrentBuild"), ubr)
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE); err == nil {
		machine = strings.TrimSpace(str(k, "SystemManufacturer") + " " + str(k, "SystemProductName"))
		k.Close()
	}
	return os, machine
}
