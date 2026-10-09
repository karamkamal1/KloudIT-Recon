package host

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// notElevated makes the agent not elevated for the test (Wine's processes
// are).
func notElevated(t *testing.T) {
	t.Helper()
	el := runsElevated
	t.Cleanup(func() { runsElevated = el })
	runsElevated = func() bool { return false }
}

// elevatedWith makes the agent elevated for the test, with an ACL check that
// accepts only paths inside admin.
func elevatedWith(t *testing.T, admin string) {
	t.Helper()
	el, ao := runsElevated, adminOnly
	t.Cleanup(func() { runsElevated, adminOnly = el, ao })
	runsElevated = func() bool { return true }
	adminOnly = func(p string) error {
		if within(admin, p) {
			return nil
		}
		return fmt.Errorf("%s: users may change it", p)
	}
}

// TestCheckCodePath: an elevated agent runs code from its install folder or
// from places only administrators can change; one that is not elevated from
// anywhere.
func TestCheckCodePath(t *testing.T) {
	root := t.TempDir()
	install, admin, user := filepath.Join(root, "KlouditRecon"), filepath.Join(root, "admin"), filepath.Join(root, "user")
	notElevated(t)
	for _, p := range []string{install, filepath.Join(admin, "ffmpeg"), filepath.Join(user, "x")} {
		if checkCodePath(p, install) != nil {
			t.Fatalf("not elevated: %s refused", p)
		}
	}
	elevatedWith(t, admin)
	for p, ok := range map[string]bool{
		install:                                 true,
		filepath.Join(install, "ffmpeg-lgpl"):   true,
		filepath.Join(install, "..", "user"):    false,
		filepath.Join(root, "KlouditRecon2"):    false,
		filepath.Join(admin, "ffmpeg", "bin"):   true,
		filepath.Join(user, "ffmpeg-lgpl"):      false,
		filepath.Join(user, "..", "admin", "x"): true,
	} {
		if err := checkCodePath(p, install); (err == nil) != ok {
			t.Errorf("%s: %v, want accepted %v", p, err, ok)
		}
	}
}

// TestHelperFFmpegDirElevated: an elevated agent ignores a helperFFmpegDir
// non-administrators could change, and the helper loads the libraries from
// the install folder's ffmpeg-lgpl instead.
func TestHelperFFmpegDirElevated(t *testing.T) {
	root := t.TempDir()
	install, admin := filepath.Join(root, "KlouditRecon"), filepath.Join(root, "admin")
	elevatedWith(t, admin)
	def := filepath.Join(install, "ffmpeg-lgpl")
	for _, c := range []struct {
		dir, want string
		refused   bool
	}{
		{"", def, false},
		{"libs", filepath.Join(install, "libs"), false},
		{filepath.Join(admin, "ffmpeg-8.1"), filepath.Join(admin, "ffmpeg-8.1"), false},
		{filepath.Join(root, "user", "x"), def, true},
		{filepath.Join("..", "user", "x"), def, true},
	} {
		got, refused := (&Config{HelperFFmpegDir: c.dir}).helperFFmpegDir(install)
		if got != c.want || (refused != nil) != c.refused {
			t.Errorf("helperFFmpegDir %q: %q (refused: %v), want %q", c.dir, got, refused, c.want)
		}
	}
}

// TestFindFFmpegElevated: an elevated agent skips a configured ffmpeg that
// non-administrators could change for the default search, and refuses one the
// search finds in such a place.
func TestFindFFmpegElevated(t *testing.T) {
	root := t.TempDir()
	admin, user := filepath.Join(root, "admin"), filepath.Join(root, "user")
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	for _, d := range []string{admin, user} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	elevatedWith(t, admin)
	t.Setenv("PATH", admin)
	c := &Config{FFmpeg: filepath.Join(user, name)}
	ff, skipped, err := c.FindFFmpeg()
	if err != nil || ff != filepath.Join(admin, name) || skipped == nil || !strings.Contains(skipped.Error(), user) {
		t.Fatalf("configured in a user folder: %q, skipped %v, err %v", ff, skipped, err)
	}
	c.FFmpeg = filepath.Join(admin, name)
	if ff, skipped, err = c.FindFFmpeg(); err != nil || skipped != nil || ff != c.FFmpeg {
		t.Fatalf("configured in an admin folder: %q, skipped %v, err %v", ff, skipped, err)
	}
	t.Setenv("PATH", user)
	c.FFmpeg = ""
	if ff, _, err = c.FindFFmpeg(); err == nil {
		t.Fatalf("found %q on a PATH entry users may change", ff)
	}
}
