package proto

import "testing"

// The client's direct path report as the host logs it.
func TestDirectReportSummary(t *testing.T) {
	r := &DirectReport{Why: "no answer", Tried: []DirectAttempt{
		{URL: "https://192.168.5.10:48100/wt", Kind: "lan", Note: "the browser's network 192.168.5.0/24", Ms: 2500, Result: "no answer"},
		{URL: "https://[fd7a:115c:a1e0::1]:48100/wt", Kind: "tailscale", Ms: 12, Result: "refused: Opening handshake failed."},
		{URL: "https://100.101.102.103:48100/wt", Result: "not tried"},
	}}
	want := "192.168.5.10:48100 (lan, the browser's network 192.168.5.0/24): no answer after 2500 ms; " +
		"[fd7a:115c:a1e0::1]:48100 (tailscale): refused after 12 ms: Opening handshake failed.; 100.101.102.103:48100: not tried"
	if got := r.Summary(); got != want {
		t.Fatalf("summary\n%s\nwant\n%s", got, want)
	}
	if got := (&DirectReport{Why: "Network path is \"Relay via gateway\""}).Summary(); got != "none" {
		t.Fatalf("summary without attempts %q", got)
	}
	if got := (*DirectReport)(nil).Summary(); got != "" {
		t.Fatalf("nil summary %q", got)
	}
}
