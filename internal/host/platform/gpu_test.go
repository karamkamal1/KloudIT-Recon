package platform

import "testing"

// TestNoGPU: a PC or VM without a GPU lists only the Microsoft Basic Render
// Driver (a software adapter); a GPU anywhere in the list, also after it (a
// VM with a passthrough GPU next to its display-only adapter), is a GPU.
func TestNoGPU(t *testing.T) {
	warp := Adapter{Name: "Microsoft Basic Render Driver", VendorID: 0x1414, Vendor: "other", Software: true}
	amd := Adapter{Name: "AMD Radeon RX 7900 XT", VendorID: 0x1002, Vendor: "amd"}
	for _, c := range []struct {
		name string
		as   []Adapter
		want string
	}{
		{"vm", []Adapter{warp}, "Windows lists no GPU, only Microsoft Basic Render Driver"},
		{"none", nil, "Windows lists no graphics adapter"},
		{"gpu", []Adapter{amd, warp}, ""},
		{"passthrough second", []Adapter{warp, amd}, ""},
	} {
		if got := NoGPU(c.as); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
