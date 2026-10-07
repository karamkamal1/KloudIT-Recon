//go:build !unix

package gateway

import "os"

func keepOwner(f *os.File, ref string) {}
