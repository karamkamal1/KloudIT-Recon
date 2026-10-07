// Command recon-host is the KloudIT Recon agent for the gaming PC. It captures
// the screen and audio, encodes on the GPU, and injects remote input.
//
//	recon-host pair <code>   store the pairing code shown by the gateway
//	recon-host run           connect to the gateway and serve streams
//	recon-host probe         show ffmpeg, its encoders and their command lines
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

func usage() {
	fmt.Fprintf(os.Stderr, `KloudIT Recon host agent %s

Usage:
  recon-host [flags] pair <pairing-code>   pair this PC with a gateway
  recon-host [flags] run                   run the agent (default)
  recon-host [flags] probe                 show ffmpeg, encoders (with their ffmpeg
                                           command lines), capture backends and monitors
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
		caps.WriteReport(os.Stdout, probeSample(cfg, caps))
		if err != nil {
			fmt.Println("error:", err)
		}
		platform.EnableDPIAwareness()
		mons, _ := platform.Monitors()
		for _, m := range mons {
			fmt.Printf("monitor %d:  %s %dx%d@%dHz at (%d,%d) primary=%v dxgi=%d\n", m.Index, m.Name, m.W, m.H, m.Hz, m.X, m.Y, m.Primary, m.DXGIOutput)
		}
		if _, err := platform.OpenGamepads(); err == nil {
			fmt.Println("gamepads:   ViGEmBus available")
		} else {
			fmt.Println("gamepads:  ", err)
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

// probeSample is the session "recon-host probe" shows each encoder's ffmpeg
// command line for: the browser client's defaults (60 fps, 30 Mbit/s,
// balanced quality) within the host's limits, a client that takes capture
// timestamps, ddagrab on the first output on Windows and the 1920x1080 test
// pattern elsewhere.
func probeSample(cfg *host.Config, caps *media.Caps) media.Params {
	p := media.Params{
		Source:       media.Source{Backend: "test", NativeW: 1920, NativeH: 1080},
		FPS:          min(60, cfg.MaxFPS),
		BitrateKbps:  min(30000, cfg.MaxKbps),
		Quality:      "balanced",
		DrawCursor:   cfg.DrawCursor,
		CaptureClock: cfg.CaptureTimestamps != "off" && caps.CanStampCapture(),
	}
	if runtime.GOOS == "windows" {
		p.Source = media.Source{Backend: "ddagrab", Output: 0}
	}
	return p
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
