// Package qualify is the live-bitrate qualification of the native encoder
// helper (GUIDE 3.6, `recon-host qualify`): for every codec x quality preset x
// rate-control mode x live-bitrate mode (seamless, flush) of the helper's
// encoder it runs one stream, started as a session starts it (the preset, the
// LTR slots of encoder.Caps.LTRSlots), on a high-motion source with the
// bitrate stepping between a high and a low target every 2 s for 60 s, and
// judges whether the encoder follows without an IDR (seamless), within 3
// frames, with no frame-id or barcode gaps and a stream that decodes cleanly.
// The results go to live-bitrate.json next to host.json; sessions read it to
// pick the live-bitrate mode per codec, preset and rate-control mode
// (Results.Choose): seamless where it passed, else flush (with less frequent
// rate changes), else a new helper per change.
//
// Each run is the helper's encode test (recon-encoder.exe --encode-test,
// docs/HELPER_PROTOCOL.md) with --rate-schedule, which sets each new rate on
// the capture thread right before the first frame that should have it, and
// --frame-log, the per-frame flags, sizes and targets; FFmpeg decodes the
// written stream for the frame types and the frame barcode (GUIDE 0.2) of
// every frame.
package qualify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Options configures Run. Zero values take the defaults (GUIDE 3.6: 50 ->
// 20 -> 50 Mbit/s every 2 s for 60 s; 1920x1080 at 60 fps on the helper's
// synthetic GPU source in its high-motion mode).
type Options struct {
	Helper     string   // recon-encoder.exe
	HelperArgs []string // extra helper arguments for every run (tests: --mock-rate-lag=5)
	Backend    string   // auto (default) | amf | nvenc | lavc | mock
	// NvencTestDLL runs the NVENC backend against this test double of the
	// NVIDIA runtime (recon-fake-nvenc.dll; tests only). Its bitstream does
	// not decode: the decode and barcode checks are skipped.
	NvencTestDLL string
	// FFmpegDir is where the libavcodec backend (GUIDE 3.8) loads FFmpeg's
	// shared libraries from (the helper's --ffmpeg-dir; recon-host qualify
	// passes the host config's, as sessions run the helper); "" = the
	// helper's default.
	FFmpegDir string
	// LavcTestEncoder runs the libavcodec backend with these FFmpeg
	// encoders instead of Quick Sync Video (the helper's
	// --lavc-test-encoder, e.g. libx264 of an FFmpeg GPL shared build; tests
	// only, with Backend lavc). Software encoders: the results never apply
	// to sessions (Results.TestEncoder).
	LavcTestEncoder string
	// FFmpeg decodes the streams (frame types, barcodes); "" skips those
	// checks (noted in every cell).
	FFmpeg string

	Codecs []string // default: every codec of the helper's caps
	// Qualities: start's quality presets (default Qualities: every preset
	// a session may ask for; Choose uses only the presets measured).
	Qualities []string
	RCModes   []string // default: amf cbr, vbr, vbr_peak; nvenc, lavc and others cbr
	Modes     []string // default: seamless, flush

	// Capture: synthetic-gpu (the default; its high-motion mode) | dda |
	// amd-direct | synthetic (the mock backend's default: its canned
	// pictures ignore the source).
	Capture string
	Monitor int // dda / amd-direct: output index on adapter 0
	Width   int // default 1920 (0 with dda: the desktop size)
	Height  int
	FPS     int // default 60

	HighKbps int           // default 50000 (the start rate)
	LowKbps  int           // default 20000
	Step     time.Duration // default 2 s
	Duration time.Duration // default 60 s
	// NoBarcode leaves the frame barcode out (e.g. a game that needs the
	// corner); the barcode check is then skipped.
	NoBarcode bool

	WorkDir string    // streams, frame logs and helper logs (default: a new temporary directory)
	Keep    bool      // keep the streams (otherwise deleted after their check; logs stay)
	Out     io.Writer // progress (default os.Stdout)
}

// barcodeAt is where the runs draw the frame barcode: away from the picture
// edge, cells of proto.BarcodeCell.
var barcodeAt = BarcodeArea{X: 16, Y: 16, Cell: 16}

// mockMaxFrames: the mock backend's canned clip loops back to its IDR every
// 60 frames, so a mock run stays within one loop.
const mockMaxFrames = 59

func (o *Options) defaults() {
	if o.Backend == "" {
		o.Backend = "auto"
	}
	if o.Width == 0 && o.Height == 0 && (o.Capture == "" || o.Capture == "synthetic-gpu") {
		o.Width, o.Height = 1920, 1080
	}
	if o.FPS <= 0 {
		o.FPS = 60
	}
	if o.HighKbps <= 0 {
		o.HighKbps = 50000
	}
	if o.LowKbps <= 0 {
		o.LowKbps = 20000
	}
	if o.Step <= 0 {
		o.Step = 2 * time.Second
	}
	if o.Duration <= 0 {
		o.Duration = 60 * time.Second
	}
	if len(o.Qualities) == 0 {
		o.Qualities = Qualities
	}
	if len(o.Modes) == 0 {
		o.Modes = []string{ModeSeamless, ModeFlush}
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
}

// DefaultRCModes returns the rate-control modes qualified for a backend
// (GUIDE 3.6): AMF's CBR, LATENCY_CONSTRAINED_VBR (rc vbr) and
// PEAK_CONSTRAINED_VBR (rc vbr_peak); NVENC, libavcodec (Quick Sync's
// low-delay VBR with the peak at the target, which rc cbr and vbr_peak both
// are) and the mock CBR.
func DefaultRCModes(backend string) []string {
	if backend == "amf" {
		return []string{"cbr", "vbr", "vbr_peak"}
	}
	return []string{"cbr"}
}

// backendArgs are the helper arguments that pick and set up its encoder
// backend, for --print-caps and every run alike.
func (o Options) backendArgs(backend string) []string {
	args := []string{"--backend=" + backend}
	if o.NvencTestDLL != "" {
		args = append(args, "--nvenc-test-dll="+o.NvencTestDLL)
	}
	if o.FFmpegDir != "" {
		args = append(args, "--ffmpeg-dir="+o.FFmpegDir)
	}
	if o.LavcTestEncoder != "" {
		args = append(args, "--lavc-test-encoder="+o.LavcTestEncoder)
	}
	return args
}

// Caps runs the helper's --print-caps with o's backend.
func Caps(ctx context.Context, o Options) (encoder.Caps, error) {
	o.defaults()
	helper := o.Helper
	args := append([]string{"--print-caps", "--log-level=error"}, o.backendArgs(o.Backend)...)
	var c encoder.Caps
	out, err := exec.CommandContext(ctx, helper, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return c, fmt.Errorf("%s --print-caps: %w: %s", helper, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return c, fmt.Errorf("%s --print-caps: %w", helper, err)
	}
	if err := json.Unmarshal(out, &c); err != nil {
		return c, fmt.Errorf("%s --print-caps: %w", helper, err)
	}
	return c, nil
}

// Run runs the whole matrix and returns the results (the caller saves them).
// A cell that cannot run (the encoder refuses a mode) is recorded with
// verdict "error"; Run itself fails only when the helper cannot run at all.
func Run(ctx context.Context, o Options) (*Results, error) {
	o.defaults()
	caps, err := Caps(ctx, o)
	if err != nil {
		return nil, err
	}
	if !caps.Usable() {
		var why []string
		for k, v := range caps.Unavailable {
			why = append(why, k+": "+v)
		}
		slices.Sort(why)
		return nil, fmt.Errorf("the helper has no usable encoder (%s)", strings.Join(why, "; "))
	}
	host, _ := os.Hostname()
	r := &Results{Version: ResultsVersion, Time: time.Now().UTC().Truncate(time.Second), Host: host,
		HelperVersion: caps.HelperVersion, Backend: caps.Backend, Vendor: caps.Vendor, AdapterName: caps.AdapterName,
		AdapterLUID: caps.AdapterLUID, TestDouble: o.NvencTestDLL != "", TestEncoder: o.LavcTestEncoder, Criteria: DefaultCriteria()}

	if o.LavcTestEncoder != "" {
		r.Notes = append(r.Notes, "libavcodec test encoder "+o.LavcTestEncoder+" (software, not Quick Sync Video): sessions never use these results")
	}
	mock := caps.Backend == "mock"
	capture := o.Capture
	if capture == "" {
		capture = "synthetic-gpu"
		if mock {
			capture = "synthetic" // no GPU needed: the canned pictures ignore the source
		}
	}
	motion := capture == "synthetic-gpu"
	frames := max(1, int(o.Duration.Seconds()*float64(o.FPS)+0.5))
	step := max(1, int(o.Step.Seconds()*float64(o.FPS)+0.5))
	if mock && frames > mockMaxFrames {
		frames, step = mockMaxFrames, min(step, 10)
		r.Notes = append(r.Notes, fmt.Sprintf("mock backend: its canned clip has an IDR every 60 frames, so each run is %d frames with a change every %d", frames, step))
	}
	o.Step, o.Duration = time.Duration(step)*time.Second/time.Duration(o.FPS), time.Duration(frames)*time.Second/time.Duration(o.FPS)
	barcode := !o.NoBarcode && capture != "synthetic"
	r.Source = Source{Capture: capture, Motion: motion, Monitor: o.Monitor, Width: o.Width, Height: o.Height, FPS: o.FPS, Barcode: barcode}
	r.Schedule = Schedule{HighKbps: o.HighKbps, LowKbps: o.LowKbps, StepMs: int(o.Step / time.Millisecond),
		DurationMs: int(o.Duration / time.Millisecond), StepFrames: step, Frames: frames}

	codecs := o.Codecs
	if len(codecs) == 0 {
		for _, c := range codecOrder {
			if _, ok := caps.Codecs[c]; ok {
				codecs = append(codecs, c)
			}
		}
	}
	rcs := o.RCModes
	if len(rcs) == 0 {
		rcs = DefaultRCModes(caps.Backend)
	}
	dir := o.WorkDir
	if dir == "" {
		if dir, err = os.MkdirTemp("", "recon-qualify-"); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// Decode and barcode checks: why they are skipped, if they are.
	decodeSkip, barcodeSkip := "", ""
	switch {
	case r.TestDouble:
		decodeSkip = "the NVENC test double's bitstream does not decode"
	case o.FFmpeg == "":
		decodeSkip = "no ffmpeg"
	default:
		if err := ffmpegWorks(ctx, o.FFmpeg); err != nil {
			decodeSkip = err.Error()
		}
	}
	switch {
	case !barcode && o.NoBarcode:
		barcodeSkip = "turned off (-no-barcode)"
	case !barcode:
		barcodeSkip = "the " + capture + " source has no picture to draw it into"
	case mock:
		barcodeSkip = "the mock backend's stream is a canned clip"
	case decodeSkip != "":
		barcodeSkip = decodeSkip
	}

	total := len(codecs) * len(o.Qualities) * len(rcs) * len(o.Modes)
	fmt.Fprintf(o.Out, "qualifying %s on %s (%s): %d runs of %.0f s, logs in %s\n", caps.Backend, caps.AdapterName, caps.Vendor, total,
		float64(frames)/float64(o.FPS), dir)
	n := 0
	for _, codec := range codecs {
		for _, quality := range o.Qualities {
			for _, rc := range rcs {
				for _, mode := range o.Modes {
					n++
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					// Started as a session starts this codec: its preset, LTR
					// slots and intra refresh (encoder.Caps.LTRSlots and
					// IntraRefreshFrames, as media.HelperVideo).
					c := Cell{Codec: codec, Quality: quality, LTRSlots: caps.LTRSlots(codec), RC: rc, LiveBitrate: mode,
						IntraRefresh: caps.IntraRefreshFrames(codec, o.FPS)}
					if _, ok := caps.Codecs[codec]; !ok {
						c.Verdict = VerdictError
						c.Failures = []string{"the helper's encoder has no " + codec + " (" + caps.Unavailable[caps.Backend+"-"+codec] + ")"}
					} else {
						runCell(ctx, o, cellRun{dir: dir, backend: caps.Backend, capture: capture, motion: motion, barcode: barcode,
							mock: mock, frames: frames, step: step, decodeSkip: decodeSkip, barcodeSkip: barcodeSkip}, &c)
					}
					fmt.Fprintf(o.Out, "[%d/%d] %s %s %s %s: %s", n, total, codec, quality, rc, mode, strings.ToUpper(c.Verdict))
					if len(c.Failures) > 0 {
						fmt.Fprintf(o.Out, " (%s)", strings.Join(c.Failures, "; "))
					}
					fmt.Fprintln(o.Out)
					r.Cells = append(r.Cells, c)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err // the last run was cut short: not a verdict
	}
	sortCells(r.Cells)
	for _, c := range r.Cells {
		if c.Width > 0 {
			r.Source.Width, r.Source.Height = c.Width, c.Height // what was encoded (the mock: its clip's size)
			break
		}
	}
	r.fillChoice()
	return r, nil
}

type cellRun struct {
	dir, backend, capture   string
	motion, barcode, mock   bool
	frames, step            int
	decodeSkip, barcodeSkip string
}

// cellArgs returns the encode test's arguments for a cell: its stream
// started like a session's (codec, quality preset, LTR slots, whose frames the
// encode test acknowledges after --ack-delay frames), rc and live-bitrate
// mode, with the rate schedule.
func cellArgs(o Options, cr cellRun, c *Cell, stream, frameLog string) []string {
	args := append([]string{"--encode-test=" + stream, "--frame-log=" + frameLog}, o.backendArgs(cr.backend)...)
	args = append(args, "--codec="+c.Codec, "--capture="+cr.capture, "--fps="+fmt.Sprint(o.FPS), "--kbps="+fmt.Sprint(o.HighKbps),
		"--quality="+c.Quality, "--rc="+c.RC, "--live-bitrate="+c.LiveBitrate, "--frames="+fmt.Sprint(cr.frames),
		fmt.Sprintf("--rate-schedule=%d,%d:%d", o.LowKbps, o.HighKbps, cr.step))
	if c.LTRSlots > 0 {
		args = append(args, fmt.Sprintf("--ltr-slots=%d", c.LTRSlots))
	}
	if c.IntraRefresh > 0 {
		args = append(args, fmt.Sprintf("--intra-refresh=%d", c.IntraRefresh))
	}
	if o.Width > 0 && o.Height > 0 {
		args = append(args, fmt.Sprintf("--width=%d", o.Width), fmt.Sprintf("--height=%d", o.Height))
	}
	if cr.motion {
		args = append(args, "--motion=1")
	}
	if cr.capture == "dda" || cr.capture == "amd-direct" {
		args = append(args, fmt.Sprintf("--monitor=%d", o.Monitor))
	}
	if cr.barcode {
		args = append(args, fmt.Sprintf("--barcode=%d,%d,%d", barcodeAt.X, barcodeAt.Y, barcodeAt.Cell))
	}
	if cr.mock {
		args = append(args, "--mock-follow-rate") // frame sizes that follow the rate
	}
	return append(args, o.HelperArgs...)
}

// runCell runs one encode test and judges it.
func runCell(ctx context.Context, o Options, cr cellRun, c *Cell) {
	name := fmt.Sprintf("%s-%s-%s-%s", c.Codec, c.Quality, c.RC, c.LiveBitrate)
	ext := map[string]string{"h264": ".h264", "hevc": ".hevc", "av1": ".ivf"}[c.Codec]
	stream := filepath.Join(cr.dir, name+ext)
	frameLog := filepath.Join(cr.dir, name+".jsonl")
	logPath := filepath.Join(cr.dir, name+".log")
	c.Log = logPath
	_ = os.Remove(frameLog)
	args := cellArgs(o, cr, c, stream, frameLog)

	// The encode test gives up by itself after 4 x the planned time + 15 s.
	limit := time.Duration(cr.frames*4/max(o.FPS, 1)+60) * time.Second
	rctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	out, runErr := exec.CommandContext(rctx, o.Helper, args...).CombinedOutput()
	c.Seconds = time.Since(start).Round(100 * time.Millisecond).Seconds()
	_ = os.WriteFile(logPath, append([]byte(o.Helper+" "+strings.Join(args, " ")+"\n\n"), out...), 0o600)
	defer func() {
		if !o.Keep {
			_ = os.Remove(stream)
		}
	}()

	var ee *exec.ExitError
	if errors.As(runErr, &ee) && ee.ExitCode() == 2 {
		startFailed(lastLine(out, "encode-test: "), c)
		return
	}
	log, err := ReadRunLog(frameLog)
	if err != nil {
		why := err.Error()
		if runErr != nil {
			why = runErr.Error() + ": " + lastLine(out, "")
		}
		noFrameLog(out, why, c)
		return
	}
	st := log.Started
	c.RateControl, c.StartedLiveBitrate, c.Width, c.Height, c.FPS = st.RateControl, st.LiveBitrate, st.Width, st.Height, st.FPS
	fps := o.FPS
	if st.FPS > 0 {
		fps = st.FPS
	}
	in := Input{Mode: c.LiveBitrate, FPS: fps, PlannedFrames: cr.frames, Log: log, DecodeSkipped: cr.decodeSkip,
		Barcode: cr.barcodeSkip == "" && st.Barcode, BarcodeSkipped: cr.barcodeSkip}
	if in.BarcodeSkipped == "" && !st.Barcode {
		in.BarcodeSkipped = "the helper drew no barcode (started.barcode false)"
	}
	if cr.decodeSkip == "" {
		var bc *BarcodeArea
		if in.Barcode {
			bc = &barcodeAt
		}
		d, err := Decode(ctx, o.FFmpeg, stream, c.Codec, bc)
		if err != nil {
			in.DecodeSkipped, in.Barcode, in.BarcodeSkipped = "ffmpeg failed: "+err.Error(), false, "not decoded"
		} else {
			in.Decoded = d
		}
	}
	if st.LiveBitrate != "" && st.LiveBitrate != c.LiveBitrate {
		c.Notes = append(c.Notes, fmt.Sprintf("the encoder runs liveBitrate %s, not %s", st.LiveBitrate, c.LiveBitrate))
	}
	if log.HasStarted && st.LTRSlots != c.LTRSlots {
		c.Notes = append(c.Notes, fmt.Sprintf("the encoder runs %d LTR slots, not %d", st.LTRSlots, c.LTRSlots))
	}
	Judge(in, c)
	if runErr != nil && c.Verdict == VerdictPass {
		c.Verdict = VerdictFail
		c.Failures = append(c.Failures, "the encode test failed: "+lastLine(out, "encode-test: FAIL"))
	}
}

// startFailed records a run whose stream did not start (the encode test's
// exit code 2): an error, except where the encoder refuses the live-bitrate
// mode itself (NVENC without NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE answers
// start with unsupported "liveBitrate ..."): that mode does not work on this
// encoder, a fail, so sessions restart rather than fall back to a default.
func startFailed(line string, c *Cell) {
	const failed = "encode-test: start failed: "
	if why, ok := strings.CutPrefix(line, failed+"unsupported: "); ok && strings.HasPrefix(why, "liveBitrate ") {
		c.Verdict, c.Failures = VerdictFail, []string{"the encoder refuses the mode: " + why}
		return
	}
	c.Verdict, c.Failures = VerdictError, []string{"the stream did not start: " + line}
}

// noFrameLog records a run that left no frame log (the encode test writes it
// at the end): a fail when its stream had started (the encode test printed
// started), i.e. the helper crashed, hung or was killed during the run in
// this mode; else an error (it never got that far).
func noFrameLog(out []byte, why string, c *Cell) {
	if bytes.Contains(out, []byte(`encode-test: {"t":"started"`)) {
		c.Verdict, c.Failures = VerdictFail, []string{"the helper crashed or hung during the run (no frame log): " + why}
		return
	}
	c.Verdict, c.Failures = VerdictError, []string{"no frame log: " + why}
}

// lastLine returns the last line of out starting with prefix (any line for "").
func lastLine(out []byte, prefix string) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}
