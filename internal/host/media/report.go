package media

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// WriteReport prints what Probe found, as "recon-host probe" shows it: the
// ffmpeg binary and version, the capture filters, every usable encoder with
// the ffmpeg command line BuildArgs builds for it in the sample session, and
// the encoders whose test encode failed.
func (c *Caps) WriteReport(w io.Writer, sample Params) {
	fmt.Fprintf(w, "ffmpeg:     %s\n", c.FFmpeg)
	version := c.VersionInfo
	if len(version) == 0 && c.Version != "" {
		version = []string{c.Version}
	}
	for _, l := range version {
		fmt.Fprintf(w, "            %s\n", l)
	}
	fmt.Fprintf(w, "capture:    ddagrab=%v gfxcapture=%v\n", c.Filters["ddagrab"], c.Filters["gfxcapture"])
	if len(c.Encoders) > 0 {
		fmt.Fprintf(w, "session:    %s (command line under each encoder)\n", describeSample(sample))
	}
	for _, e := range c.Encoders {
		fmt.Fprintf(w, "encoder:    %-12s %-5s %s\n", e.Name, e.Family, e.Vendor)
		p := sample
		p.Encoder = e
		if args, err := c.BuildArgs(p); err != nil {
			fmt.Fprintf(w, "            no command line: %v\n", err)
		} else {
			fmt.Fprintf(w, "            %s\n", commandLine("ffmpeg", args))
		}
	}
	rejected := make([]string, 0, len(c.Rejected))
	for name := range c.Rejected {
		rejected = append(rejected, name)
	}
	sort.Strings(rejected)
	for _, name := range rejected {
		fmt.Fprintf(w, "unusable:   %-12s %s\n", name, c.Rejected[name])
	}
}

// describeSample summarises the session WriteReport builds command lines for.
func describeSample(p Params) string {
	var src string
	switch p.Source.Backend {
	case "ddagrab":
		src = fmt.Sprintf("ddagrab output %d at its native size", p.Source.Output)
	case "test":
		src = fmt.Sprintf("test pattern %dx%d", p.Source.NativeW, p.Source.NativeH)
	default:
		src = p.Source.Backend
	}
	if p.Width > 0 && p.Height > 0 {
		src += fmt.Sprintf(" scaled to %dx%d", p.Width, p.Height)
	}
	quality := p.Quality
	if quality == "" {
		quality = "default"
	}
	s := fmt.Sprintf("%s, %d fps, %s Mbit/s, quality %s", src, p.FPS,
		strconv.FormatFloat(float64(p.BitrateKbps)/1000, 'f', -1, 64), quality)
	if p.CaptureClock {
		s += ", capture timestamps"
	}
	if p.DrawCursor {
		s += ", cursor drawn"
	}
	return s
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=+-]+$`)

// commandLine renders a command for pasting into PowerShell, cmd or a POSIX
// shell: arguments with characters other than letters, digits and _ . / : =
// + - are double-quoted. An argument containing a character double quotes do
// not protect (" $ ` \ !; BuildArgs only produces one for a gfxcapture window
// pattern) is single-quoted for POSIX shells instead.
func commandLine(name string, args []string) string {
	parts := []string{name}
	for _, a := range args {
		switch {
		case plainArg.MatchString(a):
			parts = append(parts, a)
		case !strings.ContainsAny(a, "\"$`\\!"):
			parts = append(parts, `"`+a+`"`)
		default:
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		}
	}
	return strings.Join(parts, " ")
}
