// Command recon-host is the KloudIT Recon agent for the gaming PC. It captures
// the screen and audio, encodes on the GPU, and injects remote input.
//
//	recon-host pair <code>   store the pairing code shown by the gateway
//	recon-host run           connect to the gateway and serve streams
//	recon-host probe         show ffmpeg, its encoders and their command lines
//	recon-host qualify       measure the native helper's live bitrate changes (GUIDE 3.6)
//	recon-host vdisplay      create a virtual display for a while (hardware test)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

func usage() {
	fmt.Fprintf(os.Stderr, `KloudIT Recon host agent %s

Usage:
  recon-host [flags] pair <pairing-code>   pair this PC with a gateway
  recon-host [flags] run                   run the agent (default)
  recon-host [flags] probe                 show ffmpeg, encoders (with their ffmpeg
                                           command lines), capture backends and monitors
  recon-host [flags] qualify [qualify flags]
                                           measure how the native encoder helper's encoder
                                           changes its bitrate while it runs (codec x rate
                                           control x live-bitrate mode); results go to
                                           live-bitrate.json next to the config, which
                                           sessions use (recon-host qualify -h for its flags)
  recon-host [flags] vdisplay [-mode 2560x1440@120] [-layout primary|extend|only] [-hold 30s]
                                           create a virtual display (SudoVDA or Virtual
                                           Display Driver), list the monitors, restore
  recon-host version

Flags:
`, host.Version)
	flag.PrintDefaults()
}

func main() {
	cfgPath := flag.String("config", host.DefaultConfigPath(), "config file")
	logPath := flag.String("log", "", "also write logs to this file")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Usage = usage
	flag.Parse()
	consoleAttach()

	cmd := "run"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	var out io.Writer = os.Stderr
	if *logPath != "" {
		if fi, err := os.Stat(*logPath); err == nil && fi.Size() > 20<<20 {
			_ = os.Rename(*logPath, *logPath+".old")
		}
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "log file:", err)
			os.Exit(1)
		}
		defer f.Close()
		out = tolerantMulti{f, os.Stderr}
		errOut = out // the background build has no console: errors must reach the log
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	switch cmd {
	case "version":
		fmt.Println(host.Version)
	case "pair":
		if flag.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "usage: recon-host pair <pairing-code>")
			os.Exit(2)
		}
		pc, err := proto.ParsePairingCode(strings.Join(flag.Args()[1:], ""))
		if err != nil {
			fatal(err)
		}
		cfg, err := host.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		cfg.Gateway, cfg.GatewayPin, cfg.HostID, cfg.Token = pc.Gateway, pc.Pin, pc.HostID, pc.Token
		if pc.Name != "" {
			cfg.Name = pc.Name
		}
		if err := cfg.Save(*cfgPath); err != nil {
			fatal(err)
		}
		fmt.Printf("Paired as %q with gateway %s.\nConfig saved to %s\nA running agent picks this up within a few seconds; otherwise start it (Start-ScheduledTask 'KloudIT Recon Host', or recon-host run).\n", cfg.Name, cfg.Gateway, *cfgPath)
	case "probe":
		cfg, err := host.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		ff, err := media.FindFFmpeg(cfg.FFmpeg)
		if err != nil {
			fatal(fmt.Errorf("ffmpeg not found: %w", err))
		}
		caps, err := media.Probe(context.Background(), ff, log)
		if caps == nil {
			fatal(fmt.Errorf("cannot run %s: %w", ff, err))
		}
		platform.EnableDPIAwareness()
		caps.WriteReport(os.Stdout, host.ProbeSample(cfg, caps))
		if err != nil {
			fmt.Println("error:", err)
		}
		mons, _ := platform.Monitors()
		for _, m := range mons {
			fmt.Printf("monitor %d:  %s %dx%d@%dHz at (%d,%d) primary=%v dxgi=%d\n", m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.DXGIOutput)
		}
		fmt.Println("virtual display:", vdisplay.New(vdisplay.Options{}).Detect())
		if _, err := platform.OpenGamepads(); err == nil {
			fmt.Println("gamepads:   ViGEmBus available")
		} else {
			fmt.Println("gamepads:  ", err)
		}
	case "qualify":
		cfg, err := host.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		os.Exit(qualifyCmd(cfg, *cfgPath, flag.Args()[1:]))
	case "vdisplay":
		cfg, err := host.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if err := vdisplayTest(cfg, flag.Args()[1:], log); err != nil {
			fatal(err)
		}
	case "run":
		cfg, err := host.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if cfg.LogLevel == "debug" && !*verbose {
			log = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		agent, err := host.NewAgent(ctx, cfg, log)
		if err != nil {
			log.Error("startup failed", "err", err)
			os.Exit(1)
		}
		if err := agent.Run(ctx); err != nil {
			log.Error("agent stopped", "err", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

// tolerantMulti writes to every writer, ignoring individual failures. The
// background (GUI-subsystem) build has no usable stderr, and io.MultiWriter
// would stop at the first failing writer and never reach the log file.
type tolerantMulti []io.Writer

func (t tolerantMulti) Write(p []byte) (int, error) {
	for _, w := range t {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

var errOut io.Writer = os.Stderr

func fatal(err error) {
	fmt.Fprintln(errOut, "error:", err)
	os.Exit(1)
}

// vdisplayTest creates a virtual display as a session would, prints where it
// is and the monitor list, waits, and restores the displays (docs/
// VENDOR_NOTES.md 3.7). It uses its own monitor identity and restore journal,
// but stop the agent first: two programs rearranging the displays at once
// undo each other's changes.
func vdisplayTest(cfg *host.Config, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("vdisplay", flag.ExitOnError)
	modeArg := fs.String("mode", "2560x1440@120", "virtual monitor WIDTHxHEIGHT@HZ")
	layout := fs.String("layout", cfg.VirtualDisplayLayout, "primary, extend or only (default: virtualDisplayLayout, else primary)")
	hold := fs.Duration("hold", 30*time.Second, "how long to keep it (Ctrl+C ends earlier)")
	_ = fs.Parse(args)
	mode, err := vdisplay.ParseMode(*modeArg)
	if err != nil {
		return err
	}
	if !vdisplay.ValidLayout(*layout) {
		return fmt.Errorf("layout %q: want primary, extend or only", *layout)
	}
	platform.EnableDPIAwareness()
	printMonitors := func(title string) {
		mons, _ := platform.Monitors()
		fmt.Println(title)
		for _, m := range mons {
			fmt.Printf("  monitor %d: %s %dx%d@%dHz at (%d,%d) primary=%v hmonitor=%#x dxgi=%d rotated=%v\n",
				m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.HMonitor, m.DXGIOutput, m.Rotated)
		}
	}
	opts := vdisplay.Options{Policy: vdisplay.PolicyOn, Layout: *layout, MonitorID: cfg.HostID + "/vdisplay-test",
		StateDir: filepath.Join(os.TempDir(), "kloudit-recon-vdisplay-test"), Log: log}
	if a, err := platform.PrimaryAdapter(); err == nil {
		opts.RenderAdapter = a.LUID
		fmt.Printf("render adapter: %s (DXGI adapter 0, luid %#x)\n", a.Name, a.LUID)
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return err
	}
	m := vdisplay.New(opts)
	if err := m.Recover(); err != nil {
		fmt.Println("recovering an earlier test run:", err)
	}
	st := m.Detect()
	fmt.Println("driver:", st)
	if st.Driver == "" {
		return st.Err
	}
	printMonitors("before:")
	start := time.Now()
	d, err := m.Create(mode)
	if err != nil {
		printMonitors("after the failure:")
		return err
	}
	info := d.Info()
	fmt.Printf("created in %v: %s %v (refresh %.3f Hz) layout %s target %v\n", time.Since(start).Round(time.Millisecond),
		info.Name, info.Mode, info.RefreshHz, info.Layout, info.Target)
	fmt.Printf("capture: ffmpeg %s output_idx=%d (on DXGI adapter 0; -1 = not on it), helper %q hmonitor=%#x\n",
		vdisplay.FFmpegCapture, info.Monitor.DXGIOutput, vdisplay.HelperCapture, info.Monitor.HMonitor)
	printMonitors("with the virtual display:")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("holding for %v (Ctrl+C to end)...\n", *hold)
	select {
	case <-ctx.Done():
	case <-time.After(*hold):
	case <-d.Lost():
		fmt.Println("the driver stopped answering: the virtual display is gone")
	}
	start = time.Now()
	err = d.Close()
	fmt.Printf("removed and restored in %v\n", time.Since(start).Round(time.Millisecond))
	printMonitors("after:")
	return err
}
