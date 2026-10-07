//go:build unix

package gateway

import (
	"os"
	"syscall"
)

// keepOwner gives a file written by root (for example by `recon-gateway user
// passwd` run as root) the owner of ref, so the service account can still read it.
func keepOwner(path, ref string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(ref)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
