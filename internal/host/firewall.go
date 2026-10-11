package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The direct path's firewall check (docs/VENDOR_NOTES.md "First hardware run:
// the direct path on the LAN"). The owner's first run streamed every session
// through the gateway's relay although the browser was on the PC's own
// network and the agent listened on UDP 48100: install-host.ps1 opens that
// port for Private and Domain networks only (Windows treats a Public network
// as untrusted), and warns at install time when a network is Public, but
// nothing said so later, where the owner looks. At start (and in `recon-host
// probe`) the agent asks Windows which networks are connected with which
// profile, whether the firewall is on for them, and which inbound rules
// cover the direct port for this program, and logs per network whether the
// direct path is allowed, and the command that fixes it where not.

// fwRuleName is the installer's rule (install-host.ps1 $RuleName).
const fwRuleName = "KloudIT Recon host (direct path)"

// fwNetwork is a connected network (Get-NetConnectionProfile).
type fwNetwork struct {
	Name     string `json:"Name"`
	Alias    string `json:"Alias"`
	Index    int    `json:"Index"`
	Category string `json:"Category"` // Public | Private | DomainAuthenticated
}

// fwProfileState is a Windows Firewall profile (Get-NetFirewallProfile).
type fwProfileState struct {
	Name    string `json:"Name"` // Domain | Private | Public
	Enabled bool   `json:"Enabled"`
	Inbound string `json:"Inbound"` // DefaultInboundAction: Block | Allow | NotConfigured
}

// fwRule is an enabled inbound rule that covers the direct port over UDP.
type fwRule struct {
	Name    string `json:"Name"`
	Profile string `json:"Profile"` // Any, or a comma-separated list of Domain, Private, Public
	Action  string `json:"Action"`  // Allow | Block
	Program string `json:"Program"` // Any, or the program the rule is for
}

// fwState is what Windows reports (readFirewall).
type fwState struct {
	Networks []fwNetwork      `json:"Profiles"`
	Firewall []fwProfileState `json:"Firewall"`
	Rules    []fwRule         `json:"Rules"`
}

// fwFinding is the direct path's state on one connected network.
type fwFinding struct {
	Network fwNetwork
	Allowed bool
	Why     string // what allows or blocks it
	Fix     string // a command that fixes it (blocked only)
}

// fwProfileOf is the firewall profile of a network category.
func fwProfileOf(category string) string {
	if strings.EqualFold(category, "DomainAuthenticated") {
		return "Domain"
	}
	return category
}

// covers reports whether a rule applies to the firewall profile and to the
// program exe.
func (r fwRule) covers(profile, exe string) bool {
	p := strings.TrimSpace(r.Profile)
	inProfile := p == "" || strings.EqualFold(p, "Any")
	for _, s := range strings.Split(p, ",") {
		if strings.EqualFold(strings.TrimSpace(s), profile) {
			inProfile = true
		}
	}
	return inProfile && fwSameProgram(r.Program, exe)
}

// fwSameProgram reports whether a rule's program (Any, or a path that may
// hold %VARIABLES%) is exe.
func fwSameProgram(program, exe string) bool {
	p := strings.TrimSpace(program)
	if p == "" || strings.EqualFold(p, "Any") || exe == "" {
		return true
	}
	p = fwEnvVar.ReplaceAllStringFunc(p, func(m string) string {
		k := m[1 : len(m)-1]
		for _, e := range os.Environ() { // Windows variables are case-insensitive
			if i := strings.IndexByte(e, '='); i > 0 && strings.EqualFold(e[:i], k) {
				return e[i+1:]
			}
		}
		return m
	})
	return strings.EqualFold(filepath.Clean(p), filepath.Clean(exe))
}

// fwEnvVar is a %VARIABLE% in a rule's program path.
var fwEnvVar = regexp.MustCompile(`%[^%\\/]+%`)

// directFindings judges the direct path on every connected network: allowed
// where the firewall is off for its profile, its default lets inbound
// traffic in, or an allow rule covers the profile and this program, and no
// block rule does; else blocked, with the fix (a Public network the
// installer's Private/Domain rule does not cover: make it Private; no rule
// at all: add one).
func directFindings(st fwState, port int, exe string) []fwFinding {
	fw := map[string]fwProfileState{}
	for _, p := range st.Firewall {
		fw[strings.ToLower(p.Name)] = p
	}
	var out []fwFinding
	for _, n := range st.Networks {
		prof := fwProfileOf(n.Category)
		f := fwFinding{Network: n}
		var allow, block, other []string
		for _, r := range st.Rules {
			switch {
			case !r.covers(prof, exe):
				if strings.EqualFold(r.Action, "Allow") && fwSameProgram(r.Program, exe) {
					other = append(other, fmt.Sprintf("%q (%s)", r.Name, r.Profile))
				}
			case strings.EqualFold(r.Action, "Block"):
				block = append(block, fmt.Sprintf("%q", r.Name))
			default:
				allow = append(allow, fmt.Sprintf("%q", r.Name))
			}
		}
		p, known := fw[strings.ToLower(prof)]
		switch {
		case known && !p.Enabled:
			f.Allowed, f.Why = true, fmt.Sprintf("Windows Firewall is off for %s networks", prof)
		case len(block) > 0:
			f.Why = fmt.Sprintf("the firewall rule %s blocks inbound UDP %d", strings.Join(block, ", "), port)
			f.Fix = "remove or disable that rule (wf.msc, Inbound Rules)"
		case len(allow) > 0:
			f.Allowed, f.Why = true, "the firewall rule "+strings.Join(allow, ", ")+" allows it"
		case known && strings.EqualFold(p.Inbound, "Allow"):
			f.Allowed, f.Why = true, fmt.Sprintf("the %s profile lets inbound traffic in by default", prof)
		case len(other) > 0:
			f.Why = fmt.Sprintf("the network is %s, and the firewall rule %s does not cover %s networks", n.Category, strings.Join(other, ", "), prof)
			f.Fix = fmt.Sprintf("Set-NetConnectionProfile -InterfaceIndex %d -NetworkCategory Private", n.Index)
		default:
			f.Why = fmt.Sprintf("no inbound firewall rule allows UDP %d for %s", port, exeOr(exe))
			f.Fix = fmt.Sprintf("New-NetFirewallRule -DisplayName '%s' -Direction Inbound -Action Allow -Protocol UDP -LocalPort %d -Program '%s' -Profile Private,Domain"+
				" (or run install-host.ps1 again)", fwRuleName, port, exeOr(exe))
			if !strings.EqualFold(n.Category, "Private") && !strings.EqualFold(n.Category, "DomainAuthenticated") {
				f.Fix += fmt.Sprintf("; and, as the network is %s: Set-NetConnectionProfile -InterfaceIndex %d -NetworkCategory Private", n.Category, n.Index)
			}
		}
		out = append(out, f)
	}
	return out
}

func exeOr(exe string) string {
	if exe == "" {
		return "recon-hostw.exe"
	}
	return exe
}

// parseFirewall reads readFirewall's JSON.
func parseFirewall(b []byte) (fwState, error) {
	var st fwState
	if err := json.Unmarshal(b, &st); err != nil {
		return fwState{}, fmt.Errorf("firewall state: %w", err)
	}
	return st, nil
}

// errNoFirewallCheck: not on Windows.
var errNoFirewallCheck = errors.New("the firewall check is for Windows")

// firewallScript is the PowerShell that reports the networks, the firewall
// profiles and the enabled inbound rules that cover UDP port (the UDP port
// rules, and the program's own rules for UDP or any protocol), as JSON.
func firewallScript(port int, exe string) string {
	q := strings.ReplaceAll(exe, "'", "''")
	return strings.NewReplacer("{{PORT}}", strconv.Itoa(port), "{{EXE}}", q).Replace(`$ErrorActionPreference = 'SilentlyContinue'
$port = {{PORT}}
$exe = '{{EXE}}'
function Covers($lp) {
  foreach ($x in @($lp)) {
    $s = "$x"
    if ($s -eq 'Any' -or $s -eq "$port") { return $true }
    if ($s -match '^(\d+)-(\d+)$' -and [int]$matches[1] -le $port -and $port -le [int]$matches[2]) { return $true }
  }
  return $false
}
$found = @{}
foreach ($f in @(Get-NetFirewallPortFilter -Protocol UDP)) { if (Covers $f.LocalPort) { foreach ($r in @($f | Get-NetFirewallRule)) { $found[$r.Name] = $r } } }
if ($exe) { foreach ($r in @(Get-NetFirewallApplicationFilter -Program $exe | Get-NetFirewallRule)) {
  $pf = $r | Get-NetFirewallPortFilter
  if ("$($pf.Protocol)" -in @('Any', 'UDP', '17') -and (Covers $pf.LocalPort)) { $found[$r.Name] = $r } } }
$rules = @($found.Values | Where-Object { "$($_.Enabled)" -eq 'True' -and "$($_.Direction)" -eq 'Inbound' } | ForEach-Object {
  $af = $_ | Get-NetFirewallApplicationFilter
  [pscustomobject]@{ Name = "$($_.DisplayName)"; Profile = "$($_.Profile)"; Action = "$($_.Action)"; Program = "$($af.Program)" } })
$profiles = @(Get-NetConnectionProfile | ForEach-Object {
  [pscustomobject]@{ Name = "$($_.Name)"; Alias = "$($_.InterfaceAlias)"; Index = [int]$_.InterfaceIndex; Category = "$($_.NetworkCategory)" } })
$fw = @(Get-NetFirewallProfile | ForEach-Object {
  [pscustomobject]@{ Name = "$($_.Name)"; Enabled = ("$($_.Enabled)" -eq 'True'); Inbound = "$($_.DefaultInboundAction)" } })
[pscustomobject]@{ Profiles = $profiles; Firewall = $fw; Rules = $rules } | ConvertTo-Json -Depth 4 -Compress
`)
}

// checkDirectFirewall logs, once at start, whether Windows Firewall lets the
// direct path's UDP port in on each connected network (a warning with the
// fix where it does not: streams from that network go through the gateway's
// relay).
func (a *Agent) checkDirectFirewall(ctx context.Context) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := readFirewall(ctx, a.cfg.DirectPort, exe)
	if err != nil {
		if !errors.Is(err, errNoFirewallCheck) {
			a.log.Info("direct path: cannot read the Windows Firewall state", "err", err)
		}
		return
	}
	logDirectFindings(a.log.Info, a.log.Warn, directFindings(st, a.cfg.DirectPort, exe), a.cfg.DirectPort)
}

// logDirectFindings logs each network's finding: allowed (info) or blocked
// (warn, with the fix).
func logDirectFindings(info, warn func(string, ...any), fs []fwFinding, port int) {
	if len(fs) == 0 {
		info("direct path: Windows reports no connected network", "port", port)
	}
	for _, f := range fs {
		attrs := []any{"port", port, "network", f.Network.Name, "interface", f.Network.Alias, "profile", f.Network.Category, "why", f.Why}
		if f.Allowed {
			info("direct path: Windows Firewall lets it in on this network", attrs...)
			continue
		}
		warn("direct path blocked by Windows Firewall on this network: browsers on it stream through the gateway's relay", append(attrs, "fix", f.Fix)...)
	}
}

// ProbeDirectPath writes the direct path's firewall findings for `recon-host
// probe` (nothing where the agent has no direct port; on other systems a
// note).
func ProbeDirectPath(ctx context.Context, cfg *Config, w io.Writer) {
	if cfg.DirectPort <= 0 {
		fmt.Fprintln(w, "direct path: off (directPort 0)")
		return
	}
	exe, _ := os.Executable()
	if dir := filepath.Dir(exe); dir != "" {
		// The logon task runs recon-hostw.exe, the rule the installer adds is for it.
		if w := filepath.Join(dir, "recon-hostw.exe"); fileExists(w) {
			exe = w
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := readFirewall(ctx, cfg.DirectPort, exe)
	if err != nil {
		fmt.Fprintf(w, "direct path: UDP %d (firewall not checked: %v)\n", cfg.DirectPort, err)
		return
	}
	fs := directFindings(st, cfg.DirectPort, exe)
	if len(fs) == 0 {
		fmt.Fprintf(w, "direct path: UDP %d; Windows reports no connected network\n", cfg.DirectPort)
	}
	for _, f := range fs {
		state := "allowed"
		if !f.Allowed {
			state = "BLOCKED (browsers on this network stream through the relay)"
		}
		fmt.Fprintf(w, "direct path: UDP %d on network %q (%s, %s): %s: %s\n", cfg.DirectPort, f.Network.Name, f.Network.Alias, f.Network.Category, state, f.Why)
		if f.Fix != "" {
			fmt.Fprintf(w, "  fix (administrator PowerShell): %s\n", f.Fix)
		}
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
