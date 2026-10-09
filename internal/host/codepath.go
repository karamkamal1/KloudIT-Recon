package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// The agent runs code from two places host.json can name: FFmpeg (host config
// "ffmpeg", else next to recon-host or on PATH) and the helper's FFmpeg
// libraries (helperFFmpegDir). On Windows the logon task runs the agent
// elevated, but host.json and the user's PATH belong to the user, and any
// program the user runs can change them without elevation: run from there,
// that program's code would get the elevated token (a silent UAC bypass). An
// elevated agent therefore runs code only from its install folder (which
// install-host.ps1 limits to administrators, like recon-host.exe itself) or
// from a place only administrators can change (platform.AdminOnly).

// The process's privileges and the ACL check (tests replace them).
var (
	runsElevated = platform.Elevated
	adminOnly    = platform.AdminOnly
)

// checkCodePath returns why an elevated agent installed in installDir must
// not run code from path (naming the part of it that fails), or nil.
func checkCodePath(path, installDir string) error {
	if !runsElevated() || installDir != "" && within(installDir, path) {
		return nil
	}
	if err := adminOnly(path); err != nil {
		return fmt.Errorf("%w (the agent runs elevated: it runs FFmpeg and loads its libraries only from its install folder or a folder only administrators can change)", err)
	}
	return nil
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// exeDir is the folder of the running executable ("" when unknown).
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// FindFFmpeg locates FFmpeg for c as media.FindFFmpeg does (host config
// "ffmpeg", next to recon-host, PATH). An elevated agent skips a configured
// path checkCodePath refuses (skipped says why) and searches the default
// places; one it refuses there is an error.
func (c *Config) FindFFmpeg() (ff string, skipped, err error) {
	dir := exeDir()
	if c.FFmpeg != "" {
		if ff, err = media.FindFFmpeg(c.FFmpeg); err != nil {
			return "", nil, err
		}
		if skipped = checkCodePath(ff, dir); skipped == nil {
			return ff, nil, nil
		}
	}
	if ff, err = media.FindFFmpeg(""); err != nil {
		return "", skipped, err
	}
	if err = checkCodePath(ff, dir); err != nil {
		return "", skipped, err
	}
	return ff, skipped, nil
}
