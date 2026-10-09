package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
)

// qualifyCmd runs "recon-host qualify": the live-bitrate qualification of
// the native encoder helper (GUIDE 3.6, internal/host/qualify). Exit code 0:
// the results were recorded (whatever they say); 1: it could not run; 2: bad
// flags.
func qualifyCmd(cfg *host.Config, cfgPath string, args []string) int {
	fs := flag.NewFlagSet("qualify", flag.ContinueOnError)
	var o qualify.Options
	exe, _ := os.Executable()
	helper := fs.String("helper", filepath.Join(filepath.Dir(exe), "recon-encoder.exe"), "the native encoder helper")
	fs.StringVar(&o.Backend, "backend", "auto", "encoder backend: auto (as sessions start the helper) | amf | nvenc | lavc | mock")
	codecs := fs.String("codecs", "", "codecs to qualify, comma separated (default: every codec of the helper's encoder)")
	qualities := fs.String("quality", strings.Join(qualify.Qualities, ","), "encoder quality presets (the client's preset setting); "+
		"sessions with a preset not qualified get the helper's defaults")
	rcs := fs.String("rc", "", "rate-control modes: cbr, vbr (AMF LATENCY_CONSTRAINED_VBR), vbr_peak (AMF PEAK_CONSTRAINED_VBR); default AMF all three, NVENC and libavcodec cbr")
	modes := fs.String("modes", "seamless,flush", "live-bitrate modes")
	fs.StringVar(&o.Capture, "capture", "", "source: synthetic-gpu (default, high motion) | dda | amd-direct (with a high-motion game or video on the monitor)")
	fs.IntVar(&o.Monitor, "monitor", 0, "dda / amd-direct: output index on the primary adapter")
	fs.IntVar(&o.Width, "width", 0, "encoded width (default 1920 for synthetic-gpu, the desktop size for dda)")
	fs.IntVar(&o.Height, "height", 0, "encoded height")
	fs.IntVar(&o.FPS, "fps", 60, "frame rate")
	fs.IntVar(&o.HighKbps, "high", 50000, "start bitrate and every second step, kbps")
	fs.IntVar(&o.LowKbps, "low", 20000, "the other step, kbps")
	fs.DurationVar(&o.Step, "step", 0, "time between rate changes (default 2s)")
	fs.DurationVar(&o.Duration, "duration", 0, "length of each run (default 60s)")
	fs.BoolVar(&o.NoBarcode, "no-barcode", false, "do not draw the frame barcode (skips the barcode check)")
	out := fs.String("out", "", "results file (default "+qualify.FileName+" next to the config; for mock / test-double runs in -dir)")
	fs.StringVar(&o.WorkDir, "dir", "", "directory for the streams and logs (default qualify-<time> next to the config)")
	fs.BoolVar(&o.Keep, "keep", false, "keep the encoded streams (about 250 MB per run at the defaults)")
	ffmpeg := fs.String("ffmpeg", "", "ffmpeg (5.1+) for the decode and barcode checks (default: as the agent finds it)")
	lavcDir, refused := cfg.LibavcodecDir()
	if refused != nil {
		fmt.Fprintln(os.Stderr, `warning: host config "helperFFmpegDir" ignored:`, refused)
	}
	fs.StringVar(&o.FFmpegDir, "ffmpeg-dir", lavcDir, "where the libavcodec backend loads FFmpeg's shared libraries from (host config helperFFmpegDir)")
	fs.StringVar(&o.NvencTestDLL, "nvenc-test-dll", "", "tests: run the NVENC backend against this test double (recon-fake-nvenc.dll)")
	fs.StringVar(&o.LavcTestEncoder, "lavc-test-encoder", "", "tests: run the libavcodec backend (-backend lavc) with these FFmpeg software encoders, e.g. libx264")
	helperArgs := fs.String("helper-args", "", "tests: extra helper arguments for every run, space separated (e.g. --mock-rate-lag=5)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: recon-host [-config FILE] qualify [flags]

Runs one stream per codec x quality preset x rate-control mode x live-bitrate mode through the
native encoder helper, started as sessions start it (the preset, LTR slots where the codec
recovers from them), stepping the bitrate between -high and -low every -step for -duration, and
checks: no IDR on a change (seamless; flush must make one), P-frame sizes at the new target
within 3 frames, no frame-id or barcode gaps, a stream that decodes cleanly. Sessions then use
seamless where it passed, else flush (with less frequent changes), else a new encoder per
change. Stop streaming sessions first.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, ",")
	}
	o.Helper, o.Codecs, o.Qualities, o.RCModes, o.Modes = *helper, split(*codecs), split(*qualities), split(*rcs), split(*modes)
	o.HelperArgs = strings.Fields(*helperArgs)
	if _, err := os.Stat(o.Helper); err != nil {
		fmt.Fprintln(os.Stderr, "error: the native encoder helper:", err)
		return 1
	}
	o.FFmpeg = *ffmpeg
	if o.FFmpeg == "" {
		ff, skipped, err := cfg.FindFFmpeg()
		if skipped != nil {
			fmt.Fprintln(os.Stderr, `warning: host config "ffmpeg" ignored:`, skipped)
		}
		if err == nil {
			o.FFmpeg = ff
		} else {
			fmt.Fprintln(os.Stderr, "warning: no ffmpeg, the decode and barcode checks are skipped:", err)
		}
	}
	confDir := filepath.Dir(cfgPath)
	test := o.Backend == "mock" || o.NvencTestDLL != "" || o.LavcTestEncoder != ""
	if o.WorkDir == "" {
		o.WorkDir = filepath.Join(confDir, "qualify-"+timeStamp())
	}
	if *out == "" {
		*out = qualify.PathFor(cfgPath)
		if test {
			*out = filepath.Join(o.WorkDir, qualify.FileName) // never in place of a real GPU's results
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := qualify.Run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println()
	res.Print(os.Stdout)
	if err := res.Save(*out); err != nil {
		fmt.Fprintln(os.Stderr, "error: saving the results:", err)
		return 1
	}
	fmt.Printf("\nresults saved to %s (logs in %s)\n", *out, o.WorkDir)
	if !test {
		fmt.Println("sessions pick their live-bitrate mode from it from the next stream on")
	}
	return 0
}

func timeStamp() string { return time.Now().Format("20060102-150405") }
