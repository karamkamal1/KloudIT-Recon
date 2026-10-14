package host

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The CPU budget of software encoding (host config "cpuBudget", default
// auto). On a PC or VM without a GPU every frame is captured (GDI),
// converted and encoded (libx264) on the CPU. Where the CPU cannot keep up
// with what the client asked for (1920x1080 at 60 fps on 2-4 vCPUs) FFmpeg's
// queues fill and every frame waits behind them: hundreds of ms of lag at a
// fraction of the frame rate (media/cpubench.go). Such a session streams at
// a rung of cpuRungs the CPU sustains instead, says so in the overlay
// (VideoConfig.CPULimit) and once in an informational notice:
//   - the first rung from the agent's measurements (startCPUBench): of
//     libx264 (media.BenchSoftware: the rung whose frames per second at its
//     size fit cpuShare of what the CPU encoded, the rest left for the
//     desktop and the program being used) and, for GDI capture, of the grab
//     itself (media.BenchGrab: at most cpuGrabShare of the frames per second
//     GDI grabbed the monitor, which it pays at the monitor's size whatever
//     the rung; cpuNoGrabFPS where that was not measured). The agent
//     measures at its start (logon on a VM, when its CPU is busiest) and
//     again while no session streams, and the best of the measurements
//     counts;
//   - then from the stream (cpuLoop): a rung whose generation falls behind
//     (its output below cpuBehindFPS of its frame rate where the capture
//     delivers every frame interval, or its frames waiting from capture to
//     encoded longer than a few frame intervals) for cpuBehindWindows in a
//     row moves one rung down; one at ease for cpuEaseWindows tries the rung
//     above after a wait that doubles each time that fails soon after
//     (cpuUpWait up to cpuUpWaitMax).
//
// A step down stops the generation that fell behind first (urgent: its
// frames are late already, and a second capture and encoder beside it on a
// CPU that does not keep up would only delay the new one); a step up
// restarts the video overlapped (no gap; one key frame).

// cpuRung is a step of the budget: at most fps frames per second and height
// lines (0: the session's), with the encoder's fastest preset where speed
// (Params.Quality "speed": libx264 ultrafast, about a third faster than
// superfast at 1080p).
type cpuRung struct {
	fps, height int
	speed       bool
}

// cpuRungs, from the session's settings down: the fastest preset first
// (the least visible), then the frame rate to 30 fps, then the size, then
// the frame rate below 30.
var cpuRungs = []cpuRung{
	{},
	{speed: true},
	{fps: 45, speed: true},
	{fps: 30, speed: true},
	{fps: 30, height: 900, speed: true},
	{fps: 30, height: 720, speed: true},
	{fps: 30, height: 540, speed: true},
	{fps: 20, height: 540, speed: true},
}

// Budget tuning. Tests shorten the times.
var (
	cpuShare         = 0.65            // of the measured encode rate a session's first rung may need
	cpuWindow        = 2 * time.Second // one look at the stream
	cpuBehindFPS     = 0.85            // output below this share of the frame rate: behind
	cpuBehindWindows = 2               // looks behind in a row before a step down
	cpuEaseWindows   = 15              // looks at ease in a row before a step up
	cpuUpWait        = time.Minute     // after a step down, before a step up is tried
	cpuUpWaitMax     = 10 * time.Minute
	cpuUpFailWithin  = 30 * time.Second // a step down this soon after a step up doubles the wait
	cpuGap           = 500 * time.Millisecond
	cpuGrabShare     = 0.8             // of the measured GDI grab rate a session's first rung may stream
	cpuNoGrabFPS     = 30              // a GDI session's first rung without a grab measurement
	cpuBenchAgain    = 3 * time.Minute // after a measurement, the next one (while no session streams)
	cpuBenchRuns     = 3               // measurements in all
)

// Why the budget restarts the video ("restarting video" reason).
const (
	cpuWhyDown = "the CPU does not keep up"
	cpuWhyUp   = "the CPU has room"
)

// cpuBudgetOn: host config "cpuBudget" auto ("" = auto), not off.
func (c *Config) cpuBudgetOn() bool { return c != nil && c.CPUBudget != settingOff }

// cpuBench is the agent's measurement of software encoding (startCPUBench).
type cpuBench struct {
	mu   sync.Mutex
	res  media.SoftwareBench
	ok   bool
	done chan struct{} // closed when it has finished (nil: none runs)
}

// benchSoftware is media.BenchSoftware and benchGrab media.BenchGrab (tests
// replace them).
var (
	benchSoftware = media.BenchSoftware
	benchGrab     = media.BenchGrab
)

// measureSoftware measures what a session of this PC does on its CPU at
// monitor mon: libx264 with encoder e (benchSoftware) and GDI's grab of the
// monitor (benchGrab; b.GrabErr says why not, e.g. a locked desktop).
func measureSoftware(ctx context.Context, ff string, e media.EncoderInfo, mon platform.Monitor) (media.SoftwareBench, error) {
	b, err := benchSoftware(ctx, ff, e, mon.W, mon.H)
	if err != nil {
		return b, err
	}
	start := time.Now()
	if b.Grab, err = benchGrab(ctx, ff, mon.X, mon.Y, mon.W, mon.H); err != nil {
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		b.Grab, b.GrabErr = 0, err.Error()
	}
	b.Took += time.Since(start)
	return b, nil
}

// startCPUBench measures software encoding in the background where sessions
// encode on the CPU and capture the desktop (no hardware encoder in FFmpeg,
// a build with gdigrab: Windows), at the main monitor's size (bench.done
// closes when the first measurement has finished; sessions that start
// before begin at their settings, and the stream then decides). Then it
// measures again every cpuBenchAgain while no session streams, given up
// when one starts, up to cpuBenchRuns measurements: one at logon, when a VM
// is busiest, can be a fraction of what the CPU does (5 times lower under
// load in this sandbox), and the best of them counts (cpuBench.merge).
func (a *Agent) startCPUBench(ctx context.Context) {
	if !a.cfg.cpuBudgetOn() || a.caps == nil || a.caps.FFmpeg == "" || !a.caps.CanGDIGrab() {
		return
	}
	var enc media.EncoderInfo
	for _, e := range a.caps.Encoders {
		if e.HW {
			return // a GPU encoder: the budget is for PCs without one
		}
		if e.Name == "libx264" && enc.Name == "" {
			enc = e
		}
	}
	if enc.Name == "" {
		return
	}
	a.bench.done = make(chan struct{})
	go func() {
		defer close(a.bench.done)
		a.measureCPU(ctx, enc)
	}()
	go func() {
		<-a.bench.done
		for runs := 1; runs < cpuBenchRuns; {
			select {
			case <-ctx.Done():
				return
			case <-time.After(cpuBenchAgain):
			}
			if a.streaming() {
				continue // the next wait
			}
			mctx, cancel := context.WithCancel(ctx)
			go func() { // given up when a session starts: it would share the CPU with it
				for t := time.NewTicker(250 * time.Millisecond); ; {
					select {
					case <-mctx.Done():
						t.Stop()
						return
					case <-t.C:
						if a.streaming() {
							cancel()
						}
					}
				}
			}()
			if a.measureCPU(mctx, enc) {
				runs++
			}
			cancel()
		}
	}()
}

// streaming reports whether a session is active (also a paused one).
func (a *Agent) streaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active != nil
}

// measureCPU makes one measurement (measureSoftware) at the main monitor and
// keeps the best of it and those before (cpuBench.merge); it reports
// whether it measured.
func (a *Agent) measureCPU(ctx context.Context, enc media.EncoderInfo) bool {
	mon := a.monitors()[0]
	b, err := measureSoftware(ctx, a.caps.FFmpeg, enc, mon)
	if err != nil {
		if ctx.Err() != nil {
			a.log.Info("software encoding measurement given up: a session started", "err", err)
			return false
		}
		a.log.Info("software encoding not measured: sessions start at their settings", "err", err)
		return false
	}
	best, again := a.bench.merge(b)
	grab := fmt.Sprintf("%.0f", b.Grab)
	if b.GrabErr != "" {
		grab = "not measured: " + trunc(b.GrabErr, 160)
	}
	attrs := []any{"encoder", b.Encoder, "size", fmt.Sprintf("%dx%d", b.W, b.H), "superfast_fps", fmt.Sprintf("%.0f", b.Superfast),
		"ultrafast_fps", fmt.Sprintf("%.0f", b.Ultrafast), "gdi_grab_fps", grab, "took", b.Took.Round(time.Millisecond)}
	if !again {
		a.log.Info("software encoding measured (no GPU encoder): sessions start at a frame rate and size this CPU sustains", attrs...)
		return true
	}
	a.log.Info("software encoding measured again (no session streaming): sessions start from the best measurement",
		append(attrs, "best", fmt.Sprintf("superfast %.0f fps, ultrafast %.0f fps, gdi grab %.0f fps", best.Superfast, best.Ultrafast, best.Grab))...)
	return true
}

// merge keeps measurement b with those before: the best of each rate (a
// busy CPU only measures lower) at one size (a measurement at another size,
// the monitor's resolution changed, replaces them). It returns the result
// and whether there was one before.
func (c *cpuBench) merge(b media.SoftwareBench) (media.SoftwareBench, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	again := c.ok
	if c.ok && c.res.W == b.W && c.res.H == b.H {
		b.Superfast, b.Ultrafast, b.Grab = max(b.Superfast, c.res.Superfast), max(b.Ultrafast, c.res.Ultrafast), max(b.Grab, c.res.Grab)
		if b.Grab > 0 {
			b.GrabErr = ""
		}
	}
	c.res, c.ok = b, true
	return b, again
}

// cpuBenchResult returns the agent's measurement, if it has one.
func (a *Agent) cpuBenchResult() (media.SoftwareBench, bool) {
	a.bench.mu.Lock()
	defer a.bench.mu.Unlock()
	return a.bench.res, a.bench.ok
}

// cpuState is a session's CPU budget (cpubudget.go).
type cpuState struct {
	mu      sync.Mutex
	chosen  bool   // the first rung was chosen
	rung    int    // the rung the session's generations use
	applies bool   // the session's latest parameters are a software encode the budget covers
	limit   string // the overlay's text for them ("" when the rung changes nothing)
	noted   bool   // the notice went out
	// The stream: the live generation (gen, its frame rate), its frames'
	// intervals and capture->encoded times in the current look.
	gen    uint8
	genFPS int
	lastAt time.Time
	sumIv  time.Duration
	n      int
	lags   []float64
	// The decisions: looks behind and at ease in a row, when a step up may
	// be tried, the wait after the next failed one, the last step up.
	behind, easy int
	upAt         time.Time
	upWait       time.Duration
	steppedUp    time.Time
}

// cpuCovers reports whether the budget covers parameters p: a software
// encoder on FFmpeg of a desktop capture (GDI, or Desktop Duplication
// downloaded to the CPU: also on a GPU PC whose GPU cannot encode the codec,
// AV1 before RDNA3 or RTX 40), with the host config allowing it.
func (s *Session) cpuCovers(p media.Params) bool {
	switch {
	case !s.a.cfg.cpuBudgetOn(), p.Encoder.HW, p.Encoder.Helper:
		return false
	}
	return p.Source.Backend == "gdigrab" || p.Source.Backend == "ddagrab" || p.Source.Backend == "test" && s.a.faults.vm
}

// vmFaults plays a VM without a GPU and a sound device (TestFaultsEnv "vm",
// TESTS ONLY): no sound device, and a measurement of a CPU that keeps up
// with the test pattern at 30 fps with the fastest preset.
func (a *Agent) vmFaults() {
	a.audioSource = noSoundSource{}
	w, h := a.cfg.TestWidth, a.cfg.TestHeight
	a.bench.res, a.bench.ok = media.SoftwareBench{Encoder: "libx264", W: w, H: h, Superfast: 40, Ultrafast: 50}, true
}

// noSoundSource is the audio source of a PC without a sound output device
// (TestFaultsEnv "vm").
type noSoundSource struct{}

func (noSoundSource) Name() string { return "test-no-device" }

func (noSoundSource) Run(context.Context, func([]float32)) error {
	return fmt.Errorf("%w: TEST fault vm", media.ErrNoAudioDevice)
}

// applyRung lowers p to rung r: its frame rate and picture height at most
// r's (the aspect kept: FFmpeg scales on the CPU), the fastest preset.
func applyRung(p *media.Params, r cpuRung) {
	if r.fps > 0 && p.FPS > r.fps {
		p.FPS = r.fps
	}
	if w, h := p.OutputSize(); r.height > 0 && h > r.height && p.Source.Backend == "gdigrab" {
		p.Width, p.Height = media.FitAspect(w, r.height, w, h)
	}
	if r.speed {
		p.Quality = "speed"
	}
}

// rungParams are p at rung i (cpuRungs).
func rungParams(p media.Params, i int) media.Params {
	q := p
	applyRung(&q, cpuRungs[i])
	return q
}

// sameEffect reports whether two rungs' parameters encode the same.
func sameEffect(a, b media.Params) bool {
	aw, ah := a.OutputSize()
	bw, bh := b.OutputSize()
	return a.FPS == b.FPS && aw == bw && ah == bh && (a.Quality == "speed") == (b.Quality == "speed")
}

// nextRung returns the rung below (down) or above i that changes what p
// encodes, or i where none does.
func nextRung(p media.Params, i int, down bool) int {
	cur := rungParams(p, i)
	for j := i; ; {
		if down {
			j++
		} else {
			j--
		}
		if j < 0 || j >= len(cpuRungs) {
			return i
		}
		if q := rungParams(p, j); !sameEffect(q, cur) {
			// Up, the first rung of those with this effect.
			for !down && j > 0 && sameEffect(rungParams(p, j-1), q) {
				j--
			}
			return j
		}
	}
}

// benchRung is the first rung for a session asking for p, from the agent's
// measurement b: the first whose frames per second at its size, at the
// measurement's size, fit cpuShare of what the CPU encoded with its preset,
// and, for GDI capture, whose frame rate is within what GDI grabs
// (grabFPS).
func benchRung(b media.SoftwareBench, p media.Params) int {
	for i := range cpuRungs {
		q := rungParams(p, i)
		w, h := q.OutputSize()
		have := b.Superfast
		if q.Quality == "speed" {
			have = b.Ultrafast
		}
		if b.W > 0 && b.H > 0 && float64(q.FPS)*float64(w*h)/float64(b.W*b.H) <= cpuShare*have &&
			(p.Source.Backend != "gdigrab" || float64(q.FPS) <= grabFPS(b, p)) {
			return i
		}
	}
	return len(cpuRungs) - 1
}

// grabFPS is the frame rate a GDI session's first rung may have:
// cpuGrabShare of the frames per second GDI grabbed (b.Grab), from the
// measured monitor's size to the size p captures (GDI grabs the whole
// monitor whatever the rung's size), or cpuNoGrabFPS without a measurement
// (the stream then finds out: one rung up after cpuEaseWindows at ease).
func grabFPS(b media.SoftwareBench, p media.Params) float64 {
	if b.Grab <= 0 || b.W <= 0 || b.H <= 0 {
		return float64(cpuNoGrabFPS)
	}
	area := float64(p.Source.NativeW * p.Source.NativeH)
	if area <= 0 {
		area = float64(b.W * b.H)
	}
	return cpuGrabShare * b.Grab * float64(b.W*b.H) / area
}

// describeParams is "WxH at F fps".
func describeParams(p media.Params) string {
	w, h := p.OutputSize()
	return fmt.Sprintf("%d×%d at %d fps", w, h, p.FPS)
}

// cpuBudget applies the session's rung to p (buildParams, once the encoder
// is chosen), choosing the first one at the session's first covered
// generation, and keeps the overlay's text for it.
func (s *Session) cpuBudget(p *media.Params) {
	covers := s.cpuCovers(*p)
	s.cpu.mu.Lock()
	s.cpu.applies = covers
	if !covers {
		s.cpu.limit = ""
		s.cpu.mu.Unlock()
		return
	}
	asked := *p
	first := !s.cpu.chosen
	if first {
		s.cpu.chosen = true
		if b, ok := s.a.cpuBenchResult(); ok {
			s.cpu.rung = benchRung(b, asked)
		}
	}
	applyRung(p, cpuRungs[s.cpu.rung])
	limit := ""
	if !sameEffect(*p, asked) {
		limit = describeParams(*p)
		if p.Quality == "speed" && asked.Quality != "speed" {
			limit += ", fastest preset"
		}
		limit += " (asked: " + describeParams(asked) + "): what the CPU keeps up with"
	}
	changed := limit != s.cpu.limit
	s.cpu.limit = limit
	notify := limit != "" && !s.cpu.noted && !sameEffect(rungParams(asked, s.cpu.rung), asked)
	if notify {
		s.cpu.noted = true
	}
	rung := s.cpu.rung
	s.cpu.mu.Unlock()
	if first {
		b, ok := s.a.cpuBenchResult()
		attrs := []any{"rung", rung, "asked", describeParams(asked), "streams", describeParams(*p), "preset_speed", p.Quality == "speed"}
		if ok {
			attrs = append(attrs, "measured", fmt.Sprintf("%dx%d superfast %.0f fps, ultrafast %.0f fps, gdi grab %.0f fps", b.W, b.H,
				b.Superfast, b.Ultrafast, b.Grab))
		}
		s.log.Info("cpu budget: software encoding", attrs...)
	} else if changed {
		s.log.Info("cpu budget: software encoding", "rung", rung, "asked", describeParams(asked), "streams", describeParams(*p),
			"preset_speed", p.Quality == "speed")
	}
	if notify {
		s.notice("info", fmt.Sprintf("This stream encodes video on the CPU (%s: no GPU encoder for it), which keeps up with %s: "+
			"streaming at that instead of %s. The overlay shows the limit; it rises again when the CPU has room.", p.Encoder.Name,
			describeParams(*p), describeParams(asked)))
	}
}

// cpuConfig marks the start of generation c (videoEvents) and gives its
// config the overlay's text.
func (s *Session) cpuConfig(c *proto.VideoConfig) {
	s.cpu.mu.Lock()
	defer s.cpu.mu.Unlock()
	s.cpu.gen, s.cpu.genFPS, s.cpu.lastAt = c.Gen, c.FPS, time.Time{}
	if s.cpu.applies {
		c.CPULimit = s.cpu.limit
	}
}

// cpuFrame measures a frame of the live generation as it comes from the
// encoder: the interval since the generation's last one, and its
// capture->encoded time where it has a capture stamp.
func (s *Session) cpuFrame(f *media.Frame, now time.Time) {
	s.cpu.mu.Lock()
	defer s.cpu.mu.Unlock()
	if !s.cpu.applies || f.Gen != s.cpu.gen {
		return
	}
	// A gap of more than cpuGap is a stall of the capture (a desktop that
	// went away, a display change), not the CPU's pace: left out.
	if iv := now.Sub(s.cpu.lastAt); !s.cpu.lastAt.IsZero() && iv <= cpuGap {
		s.cpu.sumIv += iv
		s.cpu.n++
	}
	s.cpu.lastAt = now
	if f.CaptureUs != 0 && f.EncodeDoneUs > f.CaptureUs {
		s.cpu.lags = append(s.cpu.lags, float64(f.EncodeDoneUs-f.CaptureUs)/1000)
	}
}

// cpuLoop looks at the stream every cpuWindow while the session lasts and
// moves its rung (cpuLook).
func (s *Session) cpuLoop() {
	t := time.NewTicker(cpuWindow)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if s.paused.Load() {
			s.cpuReset()
			continue
		}
		p, ok := s.vid().Active()
		if why := s.cpuLook(time.Now(), p, ok); why != "" {
			s.cpuRestart(why)
		}
	}
}

// cpuRestart restarts the video for the budget's move why: urgent for a
// step down (the generation that fell behind stops first), overlapped for a
// step up.
func (s *Session) cpuRestart(why string) {
	if err := s.startVideo(why == cpuWhyDown, why); err != nil {
		s.log.Warn("restart for the cpu budget failed", "err", err)
	}
}

// cpuLook is one look at the stream, whose live generation has parameters p
// (ok: there is one): it moves the rung where the stream asks for it and
// returns why the video restarts ("" when it does not).
func (s *Session) cpuLook(now time.Time, p media.Params, ok bool) string {
	s.cpu.mu.Lock()
	defer s.cpu.mu.Unlock()
	n, sum, lags, fps := s.cpu.n, s.cpu.sumIv, s.cpu.lags, s.cpu.genFPS
	s.cpu.n, s.cpu.sumIv, s.cpu.lags = 0, 0, s.cpu.lags[:0]
	if !ok || !s.cpu.applies || n < 10 || sum <= 0 || fps <= 0 || s.desktopAway() {
		return ""
	}
	out := float64(n) / sum.Seconds()
	interval := time.Second / time.Duration(fps)
	lag := -1.0
	if len(lags) > 0 {
		slices.Sort(lags)
		lag = lags[len(lags)/2]
	}
	lagOver := func(frames float64, floor time.Duration) bool {
		return lag >= 0 && lag > max(frames*float64(interval.Milliseconds()), float64(floor.Milliseconds()))
	}
	behind, ease := lagOver(2.5, 50*time.Millisecond), lag >= 0 && !lagOver(1.5, 35*time.Millisecond)
	if p.Source.Backend != "ddagrab" {
		// GDI and the test pattern deliver a frame every interval, changed
		// or not: an output below the frame rate is the CPU falling behind.
		// Desktop Duplication (dup_frames=0) delivers one only when the
		// screen changes (a 30 fps video, a game below the stream's rate, a
		// still desktop): its output says nothing about the CPU, only its
		// frames' capture->encoded times do.
		behind = behind || out < cpuBehindFPS*float64(fps)
		ease = out >= 0.95*float64(fps) && !lagOver(1.5, 35*time.Millisecond)
	}
	if behind {
		s.cpu.behind, s.cpu.easy = s.cpu.behind+1, 0
	} else {
		s.cpu.behind = 0
		if ease {
			s.cpu.easy++
		} else {
			s.cpu.easy = 0
		}
	}
	measured := fmt.Sprintf("%.0f of %d fps, capture→encoded p50 %s", out, fps, lagText(lag))
	switch {
	case s.cpu.behind >= cpuBehindWindows:
		next := nextRung(s.askedOf(p), s.cpu.rung, true)
		s.cpu.behind = 0
		if next == s.cpu.rung {
			return "" // the lowest rung
		}
		if !s.cpu.steppedUp.IsZero() && now.Sub(s.cpu.steppedUp) < cpuUpFailWithin {
			s.cpu.upWait = min(2*max(s.cpu.upWait, cpuUpWait), cpuUpWaitMax)
		} else {
			s.cpu.upWait = cpuUpWait
		}
		s.cpu.upAt, s.cpu.steppedUp, s.cpu.rung = now.Add(s.cpu.upWait), time.Time{}, next
		s.log.Info("cpu budget: the CPU does not keep up, stepping down", "rung", next, "measured", measured, "next_try_up", s.cpu.upWait)
		return cpuWhyDown
	case s.cpu.easy >= cpuEaseWindows && s.cpu.rung > 0 && !now.Before(s.cpu.upAt):
		asked := s.askedOf(p)
		next := nextRung(asked, s.cpu.rung, false)
		s.cpu.easy = 0
		if next == s.cpu.rung || p.FPS < rungParams(asked, s.cpu.rung).FPS {
			// Nothing above; or the rate controller holds the frame rate
			// below this rung's still (it raises it towards the settings
			// after a wait of its own): the rung is not measured yet.
			return ""
		}
		s.cpu.rung, s.cpu.steppedUp = next, now
		s.log.Info("cpu budget: the CPU keeps up, trying the rung above", "rung", next, "measured", measured)
		return cpuWhyUp
	}
	return ""
}

// askedOf returns the parameters the client asks for, from the live
// generation's (at its rung): its settings' frame rate, size and quality,
// as buildParams starts from them. Called with s.cpu.mu held.
func (s *Session) askedOf(live media.Params) media.Params {
	prefs := s.currentPrefs()
	p := s.a.sessionParams(prefs, s.monitorOf(), live.Source.Backend, s.hello.V >= proto.HelloVersionFrameExt)
	p.Encoder = live.Encoder
	return p
}

// monitorOf is the monitor the session's generations capture.
func (s *Session) monitorOf() platform.Monitor {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.monitor
}

// cpuReset drops the current look (the client is hidden: nothing streams).
func (s *Session) cpuReset() {
	s.cpu.mu.Lock()
	s.cpu.n, s.cpu.sumIv, s.cpu.lags, s.cpu.lastAt = 0, 0, s.cpu.lags[:0], time.Time{}
	s.cpu.behind, s.cpu.easy = 0, 0
	s.cpu.mu.Unlock()
}

// lagText is a capture->encoded time for the log ("-": no capture stamps).
func lagText(ms float64) string {
	if ms < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f ms", ms)
}
