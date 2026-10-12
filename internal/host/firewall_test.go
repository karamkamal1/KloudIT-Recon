package host

import (
	"strings"
	"testing"
)

// TestDirectFindings: the direct path's firewall check on what the
// PowerShell reports (firewallScript's JSON). The owner's first run: the
// network is Public, the installer's rule covers Private and Domain only,
// so the direct port is closed and every session went through the relay;
// the finding says so and names Set-NetConnectionProfile. A Private network,
// a rule for every profile, a firewall that is off, a rule for another
// program, a block rule and no rule at all.
func TestDirectFindings(t *testing.T) {
	const exe = `C:\Program Files\KlouditRecon\recon-hostw.exe`
	ours := `{"Name":"KloudIT Recon host (direct path)","Profile":"Domain, Private","Action":"Allow","Program":"C:\\Program Files\\KlouditRecon\\recon-hostw.exe"}`
	fwOn := `[{"Name":"Domain","Enabled":true,"Inbound":"NotConfigured"},{"Name":"Private","Enabled":true,"Inbound":"NotConfigured"},{"Name":"Public","Enabled":true,"Inbound":"NotConfigured"}]`
	home := func(cat string) string {
		return `[{"Name":"Network 2","Alias":"Ethernet","Index":12,"Category":"` + cat + `"}]`
	}
	for _, c := range []struct {
		name    string
		json    string
		allowed bool
		why     string // in the finding's Why
		fix     string // in its Fix
	}{
		{"the owner's: a Public network, the installer's rule", `{"Profiles":` + home("Public") + `,"Firewall":` + fwOn + `,"Rules":[` + ours + `]}`,
			false, `the network is Public, and the firewall rule "KloudIT Recon host (direct path)" (Domain, Private) does not cover Public networks`,
			"Set-NetConnectionProfile -InterfaceIndex 12 -NetworkCategory Private"},
		{"a Private network", `{"Profiles":` + home("Private") + `,"Firewall":` + fwOn + `,"Rules":[` + ours + `]}`,
			true, `the firewall rule "KloudIT Recon host (direct path)" allows it`, ""},
		{"a domain network", `{"Profiles":` + home("DomainAuthenticated") + `,"Firewall":` + fwOn + `,"Rules":[` + ours + `]}`,
			true, "allows it", ""},
		{"a rule for any profile and program", `{"Profiles":` + home("Public") + `,"Firewall":` + fwOn +
			`,"Rules":[{"Name":"mine","Profile":"Any","Action":"Allow","Program":"Any"}]}`, true, `"mine" allows it`, ""},
		{"the firewall off for Public", `{"Profiles":` + home("Public") + `,"Firewall":[{"Name":"Public","Enabled":false,"Inbound":"NotConfigured"}],"Rules":[]}`,
			true, "Windows Firewall is off for Public networks", ""},
		{"a rule for another program", `{"Profiles":` + home("Private") + `,"Firewall":` + fwOn +
			`,"Rules":[{"Name":"old","Profile":"Any","Action":"Allow","Program":"C:\\old\\recon-hostw.exe"}]}`,
			false, "no inbound firewall rule allows UDP 48100", "New-NetFirewallRule -DisplayName 'KloudIT Recon host (direct path)'"},
		{"a block rule", `{"Profiles":` + home("Private") + `,"Firewall":` + fwOn + `,"Rules":[` + ours +
			`,{"Name":"no","Profile":"Any","Action":"Block","Program":"Any"}]}`, false, `the firewall rule "no" blocks inbound UDP 48100`, "remove or disable"},
		{"no rule, a Public network", `{"Profiles":` + home("Public") + `,"Firewall":` + fwOn + `,"Rules":[]}`,
			false, "no inbound firewall rule allows UDP 48100", "-NetworkCategory Private"},
	} {
		st, err := parseFirewall([]byte(c.json))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		fs := directFindings(st, 48100, exe)
		if len(fs) != 1 {
			t.Fatalf("%s: %d findings", c.name, len(fs))
		}
		f := fs[0]
		if f.Allowed != c.allowed || !strings.Contains(f.Why, c.why) || !strings.Contains(f.Fix, c.fix) || (f.Allowed && f.Fix != "") {
			t.Errorf("%s: allowed %v, why %q, fix %q; want %v, %q, %q", c.name, f.Allowed, f.Why, f.Fix, c.allowed, c.why, c.fix)
		}
	}
	// A rule's program with an environment variable, matched case-insensitively.
	t.Setenv("ProgramFiles", `C:\Program Files`)
	if !fwSameProgram(`%programfiles%\KlouditRecon\RECON-HOSTW.exe`, exe) || fwSameProgram(`%ProgramFiles%\Other\recon-hostw.exe`, exe) {
		t.Fatal("a rule's program with %ProgramFiles%")
	}
	// The logged lines: a warning with the fix for a blocked network.
	var infos, warns []string
	logDirectFindings(func(m string, a ...any) { infos = append(infos, m) }, func(m string, a ...any) {
		warns = append(warns, m)
		for i := 0; i+1 < len(a); i += 2 {
			if a[i] == "fix" {
				warns = append(warns, a[i+1].(string))
			}
		}
	}, []fwFinding{{Network: fwNetwork{Name: "Network 2", Category: "Public", Index: 12}, Why: "x", Fix: "Set-NetConnectionProfile -InterfaceIndex 12 -NetworkCategory Private"}}, 48100)
	if len(infos) != 0 || len(warns) != 2 || !strings.Contains(warns[0], "stream through the gateway's relay") || !strings.HasPrefix(warns[1], "Set-NetConnectionProfile") {
		t.Fatalf("logged %q, %q", infos, warns)
	}
	if s := firewallScript(48100, `C:\it's\recon-hostw.exe`); !strings.Contains(s, "$port = 48100") || !strings.Contains(s, `$exe = 'C:\it''s\recon-hostw.exe'`) {
		t.Fatalf("script: %s", s)
	}
}
