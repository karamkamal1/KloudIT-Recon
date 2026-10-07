//go:build !unix

package gateway

func keepOwner(path, ref string) {}
