//go:build windows

package platform

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestDisplayRequest creates, sets, clears and closes a display power
// request (REASON_CONTEXT as minwinbase.h lays it out on x64: 32 bytes, the
// reason at 8). Wine has no power requests (a stub that fails).
func TestDisplayRequest(t *testing.T) {
	var rc reasonContext
	if unsafe.Sizeof(rc) != 32 || unsafe.Offsetof(rc.reason) != 8 {
		t.Fatalf("REASON_CONTEXT layout: size %d, reason at %d", unsafe.Sizeof(rc), unsafe.Offsetof(rc.reason))
	}
	r, err := NewDisplayRequest("KloudIT Recon test")
	if errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
		t.Skipf("PowerCreateRequest: %v (Wine)", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Set(true); err != nil {
		t.Fatal(err)
	}
	if err := r.Set(false); err != nil {
		t.Fatal(err)
	}
}
