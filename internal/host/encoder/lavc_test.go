package encoder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLavcMissing(t *testing.T) {
	dir := t.TempDir()
	if why := LavcMissing(dir); !strings.Contains(why, "has no avcodec-62.dll") || !strings.Contains(why, "-InstallLibavcodec") {
		t.Fatalf("empty directory: %q", why)
	}
	if err := os.WriteFile(filepath.Join(dir, "avcodec-62.dll"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if why := LavcMissing(dir); !strings.Contains(why, "has no avutil-60.dll") {
		t.Fatalf("without avutil: %q", why)
	}
	if err := os.Mkdir(filepath.Join(dir, "avutil-60.dll"), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := LavcMissing(dir); !strings.Contains(why, "has no avutil-60.dll") {
		t.Fatalf("a directory of that name: %q", why)
	}
	if err := os.Remove(filepath.Join(dir, "avutil-60.dll")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "avutil-60.dll"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if why := LavcMissing(dir); why != "" {
		t.Fatalf("installed: %q", why)
	}
	if why := LavcMissing(filepath.Join(dir, "nonexistent")); why == "" {
		t.Fatal("missing directory reported as installed")
	}
}
