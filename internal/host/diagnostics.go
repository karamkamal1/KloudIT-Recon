package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// "recon-host report": one text file with what a problem on a PC needs (the
// second hardware report had only "the VM is still giving me an error"):
// versions, Windows and the machine (a VM names its hypervisor), the
// session's desktop (locked, remote, disconnected), the GPUs and the
// Direct3D 11 video device GPU capture needs, the monitors, the sound
// device, the host config, the probe's output (FFmpeg, encoders and their
// command lines, the native helper, the direct path), what the CPU encodes
// where there is no GPU encoder, and the end of host.log. It never contains
// tokens, keys or passwords: the config's secrets are left out, and the
// whole text is scrubbed of their values, of pairing codes, of
// "token=..."-style fields and of long opaque strings (scrubSecrets).

// DiagnosticsOptions says what WriteDiagnostics reports.
type DiagnosticsOptions struct {
	Config     *Config // nil: the config could not be read (ConfigErr)
	ConfigPath string
	ConfigErr  error
	LogPath    string // host.log ("": not found)
	LogLines   int    // how many of its last lines (0: 400)
	// Probe runs FFmpeg's probe, the native helper and the direct path
	// check, and measures software encoding where there is no GPU encoder
	// (a few seconds to a minute).
	Probe bool
}

// WriteDiagnostics writes the report to w, scrubbed of secrets.
func WriteDiagnostics(ctx context.Context, o DiagnosticsOptions, w io.Writer) error {
	var b bytes.Buffer
	writeDiagnostics(ctx, o, &b)
	_, err := io.WriteString(w, scrubSecrets(b.String(), o.Config))
	return err
}

func writeDiagnostics(ctx context.Context, o DiagnosticsOptions, w io.Writer) {
	section := func(title string) { fmt.Fprintf(w, "\n== %s\n", title) }
	now := time.Now()
	fmt.Fprintln(w, "KloudIT Recon diagnostics report")
	fmt.Fprintf(w, "created:    %s (%s)\n", now.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	exe, _ := os.Executable()
	fmt.Fprintf(w, "recon-host: %s (%s, %s/%s)\n", Version, exe, runtime.GOOS, runtime.GOARCH)
	osName, machine := platform.OSInfo()
	if osName != "" {
		fmt.Fprintf(w, "windows:    %s\n", osName)
	}
	if machine != "" {
		fmt.Fprintf(w, "machine:    %s\n", machine)
	}
	fmt.Fprintf(w, "cpus:       %d logical\n", runtime.NumCPU())
	fmt.Fprintf(w, "elevated:   %v\n", platform.Elevated())
	fmt.Fprintln(w, "This report has no tokens, keys or passwords: the config's secrets are left out and such values are replaced by [redacted].")

	section("Session and desktop")
	d := platform.DesktopState()
	if runtime.GOOS == "windows" {
		fmt.Fprintf(w, "session %d, console session %d, remote desktop %v, state known %v, disconnected %v, locked %v, input desktop %q, secure %v\n",
			d.Session, int32(d.Console), d.Remote, d.Known, d.Disconnected, d.Locked, d.Input, d.Secure)
		switch why := d.Away(); why {
		case "":
			fmt.Fprintln(w, "the desktop is on a screen: it can be captured")
		default:
			fmt.Fprintf(w, "the desktop is away (%s): streams wait for it\n", why)
		}
	} else {
		fmt.Fprintln(w, "not Windows: no session state")
	}

	section("Graphics")
	if as, err := platform.Adapters(); err != nil {
		fmt.Fprintf(w, "adapters: %v\n", err)
	} else {
		for i, a := range as {
			fmt.Fprintf(w, "adapter %d:  %q vendor %s (%#04x) software=%v\n", i, a.Name, a.Vendor, a.VendorID, a.Software)
		}
		if why := platform.NoGPU(as); why != "" {
			fmt.Fprintf(w, "no GPU:     %s: capture with GDI, encode on the CPU\n", why)
		}
	}
	switch err := platform.D3D11VideoDevice(); {
	case errors.Is(err, platform.ErrUnsupported):
	case err != nil:
		fmt.Fprintf(w, "Direct3D 11 video device: unavailable (%v): GPU screen capture (ddagrab, gfxcapture) cannot work\n", err)
	default:
		fmt.Fprintln(w, "Direct3D 11 video device: ok")
	}
	if mons, err := platform.Monitors(); err != nil {
		fmt.Fprintf(w, "monitors: %v\n", err)
	} else {
		for _, m := range mons {
			fmt.Fprintf(w, "monitor %d:  %s %dx%d@%dHz at (%d,%d) primary=%v dxgi=%d\n", m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.DXGIOutput)
		}
	}

	section("Sound")
	fmt.Fprintln(w, soundDevice(ctx))

	section("Configuration")
	fmt.Fprintf(w, "path: %s\n", o.ConfigPath)
	switch {
	case o.ConfigErr != nil:
		fmt.Fprintf(w, "not read: %v\n", o.ConfigErr)
	case o.Config != nil:
		fmt.Fprintln(w, redactedConfig(o.Config))
	}

	if o.Probe && o.Config != nil {
		section("Probe (recon-host probe)")
		probeReport(ctx, o.Config, w)
	}

	section("host.log")
	writeLogTail(w, o.LogPath, o.LogLines)
}

// soundDevice says whether the sound output device audio is captured from
// is there (Windows: WASAPI's default render endpoint).
func soundDevice(ctx context.Context) string {
	src := media.DefaultAudioSource()
	if src.Name() != "wasapi-loopback" {
		return "not checked (not Windows)"
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	switch err := src.Run(ctx, func([]float32) {}); {
	case errors.Is(err, media.ErrNoAudioDevice):
		return fmt.Sprintf("none (%v): streams have no sound until one is added", err)
	case err != nil:
		return fmt.Sprintf("capture failed: %v", err)
	}
	return "default output device: ok"
}

// secretKey matches config keys whose values are secrets.
var secretKey = regexp.MustCompile(`(?i)token|secret|password|passwd|pin$|key$|ticket`)

// redactedConfig is the config as JSON with its secrets left out.
func redactedConfig(c *Config) string {
	b, err := json.Marshal(c)
	if err != nil {
		return err.Error()
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err.Error()
	}
	for k, v := range m {
		if !secretKey.MatchString(k) {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			m[k] = "(not set)"
		} else {
			m[k] = "[redacted]"
		}
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return string(out)
}

// probeReport writes what "recon-host probe" prints, and what the CPU
// encodes where there is no GPU encoder.
func probeReport(ctx context.Context, cfg *Config, w io.Writer) {
	ff, skipped, err := cfg.FindFFmpeg()
	if skipped != nil {
		fmt.Fprintf(w, "host config \"ffmpeg\" ignored: %v\n", skipped)
	}
	if err != nil {
		fmt.Fprintf(w, "ffmpeg not found: %v\n", err)
		return
	}
	caps, err := media.Probe(ctx, ff, nil)
	if caps == nil {
		fmt.Fprintf(w, "cannot run %s: %v\n", ff, err)
		return
	}
	platform.EnableDPIAwareness()
	caps.WriteReport(w, ProbeSample(cfg, caps))
	if err != nil {
		fmt.Fprintln(w, "error:", err)
	}
	ProbeHelper(ctx, cfg, w)
	ProbeDirectPath(ctx, cfg, w)
	if slices.ContainsFunc(caps.Encoders, func(e media.EncoderInfo) bool { return e.HW }) || !caps.CanGDIGrab() {
		return
	}
	i := slices.IndexFunc(caps.Encoders, func(e media.EncoderInfo) bool { return e.Name == "libx264" })
	if i < 0 {
		return
	}
	a := &Agent{cfg: cfg, caps: caps}
	mon := a.monitors()[0]
	b, err := measureSoftware(ctx, ff, caps.Encoders[i], mon)
	if err != nil {
		fmt.Fprintf(w, "cpu:        software encoding not measured: %v\n", err)
		return
	}
	p := a.sessionParams(proto.Prefs{FPS: 60, Quality: "balanced"}, mon, "gdigrab", true)
	p.Encoder = caps.Encoders[i]
	r := benchRung(b, p)
	at, preset := rungParams(p, r), ""
	if at.Quality == "speed" {
		preset = ", fastest preset"
	}
	grab := fmt.Sprintf("%.0f fps", b.Grab)
	if b.GrabErr != "" {
		grab = "not measured (" + trunc(b.GrabErr, 160) + ")"
	}
	fmt.Fprintf(w, "cpu:        %s at %dx%d: superfast %.0f fps, ultrafast %.0f fps; GDI grab %s; a stream asking %s starts at %s%s "+
		"(rung %d of the CPU budget)\n", b.Encoder, b.W, b.H, b.Superfast, b.Ultrafast, grab, describeParams(p), describeParams(at), preset, r)
}

// writeLogTail writes the last lines of host.log (and of host.log.old
// before them where host.log has fewer).
func writeLogTail(w io.Writer, path string, n int) {
	if n <= 0 {
		n = 400
	}
	if path == "" {
		fmt.Fprintln(w, "not found")
		return
	}
	lines, err := lastLines(path, n)
	if err != nil {
		fmt.Fprintf(w, "%s: %v\n", path, err)
		return
	}
	if len(lines) < n {
		if old, err := lastLines(path+".old", n-len(lines)); err == nil && len(old) > 0 {
			fmt.Fprintf(w, "(last %d lines of %s.old)\n", len(old), path)
			for _, l := range old {
				fmt.Fprintln(w, l)
			}
		}
	}
	fmt.Fprintf(w, "(last %d lines of %s)\n", len(lines), path)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// lastLines returns a file's last n lines (reading at most its last 4 MiB).
func lastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 4<<20 {
		if _, err := f.Seek(st.Size()-4<<20, io.SeekStart); err != nil {
			return nil, err
		}
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 2*n {
			lines = slices.Clone(lines[len(lines)-n:])
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, sc.Err()
}

var (
	// pairingCode is a pairing code ("recon1:" and its payload).
	pairingCode = regexp.MustCompile(`recon1:[A-Za-z0-9+/=_.-]+`)
	// secretField is a "name=value" or JSON "name":"value" field whose name
	// says it is a secret.
	secretField = regexp.MustCompile(`(?i)\b((?:[a-z_]*token|ticket|secret|password|passwd|api_?key|gateway_?pin|pin)"?\s*[=:]\s*)("[^"]*"|[^\s,}]+)`)
	// longOpaque is an opaque string long enough to be a key or a token
	// (base64 or hex, 40 characters or more, letters and digits; not paths:
	// no slashes).
	longOpaque = regexp.MustCompile(`\b[A-Za-z0-9+_-]{40,}={0,2}`)
)

// scrubSecrets removes from a report the config's secret values wherever
// they appear, pairing codes, secret-named fields and long opaque strings.
func scrubSecrets(s string, cfg *Config) string {
	if cfg != nil {
		for _, v := range []string{cfg.Token, cfg.GatewayPin} {
			if len(v) >= 4 {
				s = strings.ReplaceAll(s, v, "[redacted]")
			}
		}
	}
	s = pairingCode.ReplaceAllString(s, "recon1:[redacted]")
	s = secretField.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretField.FindStringSubmatch(m)
		if strings.Contains(sub[2], "redacted") || sub[2] == `"(not set)"` || sub[2] == `""` {
			return m
		}
		return sub[1] + "[redacted]"
	})
	return longOpaque.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.IndexFunc(m, func(r rune) bool { return r >= 'A' && r <= 'z' }) >= 0 {
			return "[redacted]"
		}
		return m
	})
}
