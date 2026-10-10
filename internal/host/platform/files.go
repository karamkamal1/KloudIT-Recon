package platform

import (
	"os"
	"path/filepath"
)

// ReplaceFile writes data to a new file next to path, created exclusively
// under a random name, and renames it over path. It never writes into a file
// that is there already: host.json and live-bitrate.json live in the user's
// own folder, where any program the user runs can plant a link or a hard link
// at a fixed name (host.json.tmp) that an elevated "recon-host pair" (the
// installer runs it with -PairingCode) or "qualify" would otherwise write
// through, into a file elsewhere. The rename replaces the name, not a file a
// link points at. A new file is 0600 (on Windows: the folder's inherited ACL).
func ReplaceFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
