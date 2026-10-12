//go:build !windows

package host

import "context"

// readFirewall: only Windows has the direct path's firewall rule.
func readFirewall(context.Context, int, string) (fwState, error) {
	return fwState{}, errNoFirewallCheck
}
