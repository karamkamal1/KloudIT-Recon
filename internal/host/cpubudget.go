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
//   - the first rung from the agent's measurement of libx264 at its start
//     (media.BenchSoftware: the rung whose frames per second at its size fit
//     cpuShare of what the CPU encoded, the rest left for the capture, the
//     desktop and the program being used);
//   - then from the stream (cpuLoop): a rung whose generation falls behind
//     (its output below cpuBehindFPS of its frame rate, or its frames
//     waiting from capture to encoded longer than a few frame intervals) for
//     cpuBehindWindows in a row moves one rung down; one at ease for
//     cpuEaseWindows tries the rung above after a wait that doubles each
//     time that fails soon after (cpuUpWait up to cpuUpWaitMax).
//
// Each move restarts the video overlapped (no gap; one key frame).

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

// benchSoftware is media.BenchSoftware (tests replace it).
var benchSoftware = media.BenchSoftware

// startCPUBench measures software encoding in the background where sessions
// encode on the CPU and capture the desktop (no hardware encoder in FFmpeg,
// a build with gdigrab: Windows), at the main monitor's size. Sessions that
// start before it has finished begin at their settings (the stream then
// decides).
func (a *Agent) startCPUBench() {
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
	mon := a.monitors()[0]
	a.bench.done = make(chan struct{})
	go func() {
		defer close(a.bench.done)
		b, err := benchSoftware(context.Background(), a.caps.FFmpeg, enc, mon.W, mon.H)
		if err != nil {
			a.log.Info("software encoding not measured: sessions start at their settings", "err", err)
			return
		}
		a.bench.mu.Lock()
		a.bench.res, a.bench.ok = b, true
		a.bench.mu.Unlock()
		a.log.Info("software encoding measured (no GPU encoder): sessions start at a frame rate and size this CPU sustains",
			"encoder", b.Encoder, "size", fmt.Sprintf("%dx%d", b.W, b.H), "superfast_fps", fmt.Sprintf("%.0f", b.Superfast),
			"ultrafast_fps", fmt.Sprintf("%.0f", b.Ultrafast), "took", b.Took.Round(time.Millisecond))
	}()
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
// downloaded to the CPU), with the host config allowing it.
func (s *Session) cpuCovers(p media.Params) bool {
	switch {
	case !s.a.cfg.cpuBudgetOn(), p.Encoder.HW, p.Encoder.Helper:
		return false
	}
	return p.Source.Backend == "gdigrab" || p.Source.Backend == "ddagrab"
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
// measurement's size, fit cpuShare of what the CPU encoded with its preset.
func benchRung(b media.SoftwareBench, p media.Params) int {
	for i := range cpuRungs {
		q := rungParams(p, i)
		w, h := q.OutputSize()
		have := b.Superfast
		if q.Quality == "speed" {
			have = b.Ultrafast
		}
		if b.W > 0 && b.H > 0 && float64(q.FPS)*float64(w*h)/float64(b.W*b.H) <= cpuShare*have {
			return i
		}
	}
	return len(cpuRungs) - 1
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
		limit += " (asked: " + describeParams(asked) + "): what this PC's CPU encodes"
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
			attrs = append(attrs, "measured", fmt.Sprintf("%dx%d superfast %.0f fps, ultrafast %.0f fps", b.W, b.H, b.Superfast, b.Ultrafast))
		}
		s.log.Info("cpu budget: software encoding", attrs...)
	} else if changed {
		s.log.Info("cpu budget: software encoding", "rung", rung, "asked", describeParams(asked), "streams", describeParams(*p),
			"preset_speed", p.Quality == "speed")
	}
	if notify {
		s.notice("info", fmt.Sprintf("This PC encodes video on its CPU (no GPU encoder), which keeps up with %s: streaming at that "+
			"instead of %s. The overlay shows the limit; it rises again when the CPU has room.", describeParams(*p), describeParams(asked)))
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
			if err := s.startVideo(false, why); err != nil {
				s.log.Warn("restart for the cpu budget failed", "err", err)
			}
		}
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
	behind := out < cpuBehindFPS*float64(fps) || lagOver(2.5, 50*time.Millisecond)
	ease := out >= 0.95*float64(fps) && !lagOver(1.5, 35*time.Millisecond)
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
		return "the CPU does not keep up"
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
		return "the CPU has room"
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
