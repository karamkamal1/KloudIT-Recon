package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceFileNoFollow: ReplaceFile writes neither through a link
// planted at the old fixed temporary name (host.json.tmp) nor into a file
// host.json is a hard link of: the files those point at keep their content,
// host.json gets the new one, and no temporary file is left. Before, Save
// wrote host.json.tmp with os.WriteFile, which follows a symbolic link.
func TestReplaceFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "elsewhere.dll")
	other := filepath.Join(dir, "other.dll")
	for _, f := range []string{victim, other} {
		if err := os.WriteFile(f, []byte("MZ original"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "host.json")
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := os.Link(other, path); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	for i, want := range []string{`{"gateway":"a"}`, `{"gateway":"b"}`} {
		if err := ReplaceFile(path, []byte(want)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != want {
			t.Fatalf("write %d: host.json %q, want %q", i, b, want)
		}
	}
	for _, f := range []string{victim, other} {
		if b, _ := os.ReadFile(f); string(b) != "MZ original" {
			t.Errorf("%s was written: %q", filepath.Base(f), b)
		}
	}
	if fi, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("host.json mode %v, want 0600", fi.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "host.json.*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left: %q", left)
	}
}
