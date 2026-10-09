package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// Sessions on a virtual display (GUIDE 3.7) against a simulated PC
// (vdisplay.Sim: Windows' display configuration and a SudoVDA-like driver):
// the policy's decision, the capture of exactly that display 1:1 with
// Desktop Duplication, input mapped to its rectangle, the topology restored
// when the session ends, its size changes, the display is lost, the agent
// stops or crashed, and reconnects within the linger.

// recInput records absolute mouse moves (the input backend).
type recInput struct {
	mu  sync.Mutex
	abs []input.Rect
}

func (r *recInput) Key(uint16, bool, bool) error { return nil }
func (r *recInput) Button(uint8, bool) error     { return nil }
func (r *recInput) MoveRel(int32, int32) error   { return nil }
func (r *recInput) MoveAbs(_, _ uint16, t input.Rect) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abs = append(r.abs, t)
	return nil
}
func (r *recInput) Wheel(int16, int16) error { return nil }
func (r *recInput) Text(string) error        { return nil }
func (r *recInput) Close()                   {}

// target returns the rectangle the last absolute move was mapped into.
func (r *recInput) target() input.Rect {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.abs) == 0 {
		return input.Rect{}
	}
	return r.abs[len(r.abs)-1]
}

// vdRig is an agent on a simulated PC.
type vdRig struct {
	sim  *vdisplay.Sim
	a    *Agent
	in   *recInput
	logs *lockedLog
	dir  string // the config's directory: the restore journal
}

// The FFmpeg build of the rigs: hardware HEVC and H.264, ddagrab and
// gfxcapture.
func vdCaps() *media.Caps {
	return &media.Caps{Filters: map[string]bool{"ddagrab": true, "gfxcapture": true},
		Encoders: []media.EncoderInfo{
			{Name: "hevc_amf", Family: "hevc", Vendor: "amd", HW: true},
			{Name: "h264_amf", Family: "h264", Vendor: "amd", HW: true},
		}}
}

// newVDRig returns an agent with cfg (capture ddagrab unless set) on a PC
// with sim's monitors, its virtual display manager set up as NewAgent does.
func newVDRig(t *testing.T, sim *vdisplay.Sim, cfg Config) *vdRig {
	t.Helper()
	if cfg.Capture == "" {
		cfg.Capture = "ddagrab"
	}
	cfg.Defaults()
	r := &vdRig{sim: sim, in: &recInput{}, logs: &lockedLog{}, dir: t.TempDir()}
	cfg.path = filepath.Join(r.dir, "host.json")
	r.a = &Agent{cfg: &cfg, caps: vdCaps(), inj: input.NewInjector(r.in), hostClock: media.NewHostClock(),
		log: slog.New(slog.NewTextHandler(r.logs, nil)), listMonitors: sim.Monitors}
	r.a.setupVirtualDisplays(sim.Manager(r.a.virtualDisplayOptions()))
	t.Cleanup(r.a.closeVirtualDisplays)
	return r
}

// linger returns a host config virtualDisplayLinger of d.
func linger(d time.Duration) *int {
	s := int(d / time.Second)
	if d > 0 && s == 0 {
		s = 1
	}
	return &s
}

// session returns an active session of a client with hello on the rig.
func (r *vdRig) session(t *testing.T, hello proto.Hello) (*Session, *fakeCtrl) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctrl := &fakeCtrl{}
	if hello.V == 0 {
		hello.V = proto.HelloVersionFrameExt
	}
	if hello.Decoders == nil {
		hello.Decoders = []proto.DecoderInfo{{Family: "hevc", HW: true}, {Family: "h264", HW: true}}
	}
	s := &Session{a: r.a, hello: hello, prefs: hello.Prefs, tried: map[string]bool{}, usage: map[string]string{},
		encFails: map[string]int{}, ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 64),
		pipeSwap: make(chan struct{}, 1), log: r.a.log}
	r.a.mu.Lock()
	r.a.active = s
	r.a.mu.Unlock()
	return s, ctrl
}

// client returns a hello of a client whose screen is w x h device pixels
// and that streams at fps.
func client(w, h, fps int) proto.Hello {
	return proto.Hello{Client: proto.ClientInfo{Width: w, Height: h, DPR: 1, Hz: float64(fps)}, Prefs: proto.Prefs{FPS: fps}}
}

// virtualMonitor returns the simulated PC's virtual display: its displays
// get HMONITORs in the order they were connected, the physical ones first.
func virtualMonitor(t *testing.T, sim *vdisplay.Sim, physical int) platform.Monitor {
	t.Helper()
	for _, m := range sim.Monitors() {
		if m.HMonitor >= uint64(0x10000+physical) {
			return m
		}
	}
	t.Fatalf("no virtual display in %+v", sim.Monitors())
	return platform.Monitor{}
}

// restored checks that the PC shows its physical monitors as before: one
// 1920x1080 monitor at (0, 0), and no restore journal.
func (r *vdRig) restored(t *testing.T) {
	t.Helper()
	mons := r.sim.Monitors()
	if len(mons) != 1 || mons[0].X != 0 || mons[0].Y != 0 || mons[0].W != 1920 || mons[0].H != 1080 || !mons[0].Primary {
		t.Fatalf("displays not restored: %+v", mons)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "vdisplay-restore.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore journal left: %v", err)
	}
}

// TestVirtualDisplayCapture: a 2560x1440 client streaming at 120 fps on a
// host whose monitor is 1920x1080 at 60 Hz ("auto") gets a virtual display
// at 2560x1440@120, primary or extended, that FFmpeg captures with ddagrab
// (its DXGI output) at its own size and frame rate; the welcome lists it
// alone; absolute input and the cursor map to its rectangle; AMD Direct
// Capture is turned down for it; ending the session restores the displays.
func TestVirtualDisplayCapture(t *testing.T) {
	for _, c := range []struct {
		layout string
		rect   input.Rect // the virtual display's
		physX  int        // the physical monitor's x meanwhile
	}{
		{vdisplay.LayoutPrimary, input.Rect{X: 0, Y: 0, W: 2560, H: 1440}, -1920},
		{vdisplay.LayoutExtend, input.Rect{X: 1920, Y: 0, W: 2560, H: 1440}, 0},
	} {
		t.Run(c.layout, func(t *testing.T) {
			sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
			sim.AddMonitor(1920, 1080, 0, 0, 60)
			r := newVDRig(t, sim, Config{Capture: "auto", VirtualDisplay: "auto", VirtualDisplayLayout: c.layout, VirtualDisplayLinger: linger(0)})
			hello := client(2560, 1440, 120)
			s, _ := r.session(t, hello)
			if n := s.openVirtualDisplay(hello.Prefs); n != "" {
				t.Fatalf("notice %q", n)
			}
			vm := virtualMonitor(t, sim, 1)
			if vm.W != 2560 || vm.H != 1440 || vm.Hz != 120 || vm.X != c.rect.X || vm.Y != c.rect.Y {
				t.Fatalf("virtual display %+v", vm)
			}
			for _, m := range sim.Monitors() {
				if m.HMonitor != vm.HMonitor && (m.X != c.physX || m.W != 1920) {
					t.Fatalf("physical monitor %+v", m)
				}
			}
			if l := r.logs.lines(`msg="streaming a virtual display"`); len(l) != 1 || !strings.Contains(l[0], `reason="client wants 2560x1440, monitor is 1920x1080"`) ||
				!strings.Contains(l[0], "mode=2560x1440@120") {
				t.Fatalf("log %q", l)
			}
			// The welcome lists the virtual display alone.
			if w := s.welcomeMonitors(); len(w) != 1 || !w[0].Virtual || w[0].Width != 2560 || w[0].Height != 1440 || w[0].Hz != 120 || w[0].X != c.rect.X {
				t.Fatalf("welcome monitors %+v", w)
			}
			// Captured whole with ddagrab at its size (no gfxcapture
			// scaling, also when the client names its size) and frame rate.
			for _, prefs := range []proto.Prefs{{FPS: 120}, {FPS: 120, Width: 2560, Height: 1440}} {
				p, err := s.buildParams(prefs)
				if err != nil {
					t.Fatal(err)
				}
				want := media.Source{Backend: "ddagrab", Output: vm.DXGIOutput, NativeW: 2560, NativeH: 1440}
				if p.Source != want || p.Width != 0 || p.Height != 0 || p.FPS != 120 || p.Encoder.Name != "hevc_amf" {
					t.Fatalf("prefs %+v: source %+v size %dx%d fps %d encoder %s, want %+v at its size, 120 fps", prefs, p.Source, p.Width, p.Height, p.FPS, p.Encoder.Name, want)
				}
			}
			// Absolute input lands in its rectangle; the cursor is mapped
			// from it.
			if err := r.a.inj.MoveAbs(65535, 65535); err != nil {
				t.Fatal(err)
			}
			if got := r.in.target(); got != c.rect {
				t.Fatalf("input target %+v, want %+v", got, c.rect)
			}
			s.prefsMu.Lock()
			cur := s.monitor
			s.prefsMu.Unlock()
			if cur.HMonitor != vm.HMonitor {
				t.Fatalf("cursor monitor %+v", cur)
			}
			s.closeVirtualDisplay()
			r.restored(t)
			if p, u, _ := sim.Counts(); p != 1 || u != 1 {
				t.Fatalf("plugs %d unplugs %d", p, u)
			}
		})
	}
}

// TestVirtualDisplayAMDDirectCapture: host config capture "amf" (AMD Direct
// Capture) is turned down for a virtual display, which the GPU's display
// engine never scans out: ddagrab, and the log says why.
func TestVirtualDisplayAMDDirectCapture(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{Capture: "amf", VirtualDisplay: "on", VirtualDisplayLinger: linger(0)})
	r.a.caps.Filters["vsrc_amf"] = true
	hello := client(1920, 1080, 60)
	s, _ := r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	vm := virtualMonitor(t, sim, 1)
	p, err := s.buildParams(hello.Prefs)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source.Backend != "ddagrab" || p.Source.Output != vm.DXGIOutput {
		t.Fatalf("source %+v", p.Source)
	}
	if l := r.logs.lines(`not used, capturing with ddagrab`); len(l) != 1 || !strings.Contains(l[0], "is a virtual display") {
		t.Fatalf("log %q", l)
	}
	if !strings.Contains(strings.Join(r.logs.lines(`msg="streaming a virtual display"`), ""), `reason="virtualDisplay is on"`) {
		t.Fatal("no decision logged")
	}
}

// TestVirtualDisplayOffAdapter0: a virtual display that is not an output of
// DXGI adapter 0 (ddagrab cannot address it) is captured with gfxcapture of
// its HMONITOR at its size; without gfxcapture (and no helper) the session
// does not use it.
func TestVirtualDisplayOffAdapter0(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	sim.OffAdapter0()
	r := newVDRig(t, sim, Config{VirtualDisplay: "on", VirtualDisplayLinger: linger(0)})
	hello := client(2560, 1440, 120)
	s, _ := r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	vm := virtualMonitor(t, sim, 1)
	p, err := s.buildParams(hello.Prefs)
	if err != nil {
		t.Fatal(err)
	}
	if want := (media.Source{Backend: "gfxcapture", HMonitor: vm.HMonitor, NativeW: 2560, NativeH: 1440}); p.Source != want || p.Width != 0 {
		t.Fatalf("source %+v size %dx%d, want %+v", p.Source, p.Width, p.Height, want)
	}
	s.closeVirtualDisplay()
	r.restored(t)

	delete(r.a.caps.Filters, "gfxcapture")
	s, _ = r.session(t, hello)
	if n := s.openVirtualDisplay(hello.Prefs); !strings.Contains(n, "FFmpeg cannot capture it") {
		t.Fatalf("notice %q", n)
	}
	r.restored(t) // removed at once, no linger
	if p, err := s.buildParams(hello.Prefs); err != nil || p.Source.Backend != "ddagrab" || p.Source.NativeW != 1920 {
		t.Fatalf("source %+v (%v)", p.Source, err)
	}
}

// TestVirtualDisplayPolicy: whether a session gets a virtual display, from
// host config "virtualDisplay", the driver, the capture and the monitor's
// mode against the client's; the decision is logged once, a failure told
// to the user.
func TestVirtualDisplayPolicy(t *testing.T) {
	second := [5]int{2560, 1440, 1920, 0, 144}
	for _, c := range []struct {
		name    string
		policy  string
		capture string
		hello   proto.Hello
		second  bool // a 2560x1440@144 monitor right of the 1920x1080@60 one
		sim     func(*vdisplay.Sim)
		mode    string // the virtual display created, "" = none
		reason  string // logged
		notice  string
	}{
		{name: "matching monitor", policy: "auto", hello: client(1920, 1080, 60), reason: "the monitor matches the client"},
		{name: "faster than the monitor", policy: "auto", hello: client(1920, 1080, 120), mode: "1920x1080@120",
			reason: "client wants 120 fps, monitor refreshes at 60 Hz"},
		{name: "larger client", policy: "auto", hello: client(2560, 1440, 60), mode: "2560x1440@60", reason: "client wants 2560x1440, monitor is 1920x1080"},
		{name: "client size setting", policy: "auto", mode: "1280x720@60", reason: "client wants 1280x720",
			hello: proto.Hello{Client: proto.ClientInfo{Width: 2560, Height: 1440}, Prefs: proto.Prefs{FPS: 60, Width: 1280, Height: 720}}},
		{name: "default frame rate", policy: "auto", mode: "2560x1440@60",
			hello: proto.Hello{Client: proto.ClientInfo{Width: 2560, Height: 1440}}},
		{name: "second monitor matches", policy: "auto", second: true, reason: "the monitor matches the client",
			hello: proto.Hello{Client: proto.ClientInfo{Width: 2560, Height: 1440}, Prefs: proto.Prefs{FPS: 120, Monitor: 1}}},
		{name: "no driver", policy: "auto", hello: client(2560, 1440, 120), sim: (*vdisplay.Sim).RemoveDriver, reason: "no virtual display driver"},
		{name: "on without a driver", policy: "on", hello: client(1920, 1080, 60), sim: (*vdisplay.Sim).RemoveDriver,
			notice: "Virtual display unavailable: no virtual display driver installed (SudoVDA or Virtual Display Driver); streaming the monitor."},
		{name: "plug fails", policy: "on", hello: client(1920, 1080, 60),
			sim:    func(s *vdisplay.Sim) { s.FailPlug(errors.New("IOCTL_ADD failed")) },
			notice: "Virtual display unavailable: sudovda: IOCTL_ADD failed; streaming the monitor."},
		{name: "on", policy: "on", hello: client(1920, 1080, 60), mode: "1920x1080@60", reason: "virtualDisplay is on"},
		{name: "off", policy: "off", hello: client(2560, 1440, 120)},
		{name: "default off", hello: client(2560, 1440, 120)},
		{name: "test pattern", policy: "on", capture: "test", hello: client(2560, 1440, 120), reason: `capture \"test\" streams a test pattern`},
		{name: "window", policy: "on", reason: "the client captures a window",
			hello: proto.Hello{Client: proto.ClientInfo{Width: 2560, Height: 1440}, Prefs: proto.Prefs{Window: "Notepad"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
			sim.AddMonitor(1920, 1080, 0, 0, 60)
			if c.second {
				sim.AddMonitor(second[0], second[1], second[2], second[3], second[4])
			}
			if c.sim != nil {
				c.sim(sim)
			}
			r := newVDRig(t, sim, Config{VirtualDisplay: c.policy, Capture: c.capture, VirtualDisplayLinger: linger(0)})
			s, _ := r.session(t, c.hello)
			if n := s.openVirtualDisplay(c.hello.Prefs); n != c.notice {
				t.Fatalf("notice %q, want %q", n, c.notice)
			}
			mons := sim.Monitors()
			physical := 1
			if c.second {
				physical = 2
			}
			switch {
			case c.mode == "" && len(mons) != physical:
				t.Fatalf("a virtual display: %+v", mons)
			case c.mode != "":
				vm := virtualMonitor(t, sim, physical)
				if got := fmt.Sprintf("%dx%d@%d", vm.W, vm.H, vm.Hz); got != c.mode {
					t.Fatalf("virtual display %s, want %s", got, c.mode)
				}
			}
			all := strings.Join(r.logs.lines(""), "\n")
			switch {
			case c.policy == "" || c.policy == "off":
				if strings.Contains(all, "virtual display") {
					t.Fatalf("policy off logs:\n%s", all)
				}
			case c.reason != "" && !strings.Contains(all, `reason="`+c.reason):
				t.Fatalf("log without reason %q:\n%s", c.reason, all)
			}
			// Once per change: a session start decides again only when
			// asked to, and an unchanged reason is not logged twice.
			if c.mode == "" && c.notice == "" && c.policy == "auto" {
				s.vdMu.Lock()
				s.decideVirtualDisplay(c.hello.Prefs)
				s.vdMu.Unlock()
				if n := len(r.logs.lines(`msg="virtual display not used"`)); n != 1 {
					t.Fatalf("%d decision lines", n)
				}
			}
			s.closeVirtualDisplay()
			if len(sim.Monitors()) != physical {
				t.Fatalf("not restored: %+v", sim.Monitors())
			}
		})
	}
}

// recPipeline is a media.Pipeline that records the generations it starts and
// its suspensions.
type recPipeline struct {
	ladderPipeline
	params   []media.Params
	suspends int
}

func (p *recPipeline) Start(pr media.Params, urgent bool) error {
	p.mu.Lock()
	p.params = append(p.params, pr)
	p.mu.Unlock()
	return p.ladderPipeline.Start(pr, urgent)
}

func (p *recPipeline) Suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suspends++
}

func (p *recPipeline) last() (media.Params, bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.params) == 0 {
		return media.Params{}, false, p.suspends
	}
	return p.params[len(p.params)-1], p.starts[len(p.starts)-1], p.suspends
}

// settings plays the client's settings messages through controlLoop.
func settings(t *testing.T, s *Session, prefs ...proto.Prefs) {
	t.Helper()
	var in bytes.Buffer
	for i := range prefs {
		b, _ := json.Marshal(proto.ClientMsg{T: "settings", Prefs: &prefs[i]})
		if err := proto.WriteMsg(&in, b); err != nil {
			t.Fatal(err)
		}
	}
	s.ctrl = &scriptedCtrl{r: &in}
	if err := s.controlLoop(); !errors.Is(err, io.EOF) {
		t.Fatalf("control loop: %v", err)
	}
}

// TestVirtualDisplayResolutionChange: a settings change of the stream's size
// or frame rate replaces the virtual display by one at the new mode (the
// video suspended first, the next generation started at once on the new
// display); a bitrate change leaves it alone. A session without one decides
// again: a frame rate the monitor cannot show gets one.
func TestVirtualDisplayResolutionChange(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{VirtualDisplay: "auto", VirtualDisplayLinger: linger(time.Hour)})
	hello := client(2560, 1440, 120)
	s, _ := r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	pl := &recPipeline{}
	s.video = pl
	check := func(what string, urgent bool, suspends, plugs int, w, h, fps int) {
		t.Helper()
		p, u, n := pl.last()
		vm := virtualMonitor(t, sim, 1)
		if u != urgent || n != suspends || p.Source.NativeW != w || p.Source.NativeH != h || p.FPS != fps ||
			p.Source.Output != vm.DXGIOutput || vm.W != w || vm.H != h || vm.Hz != fps {
			t.Fatalf("%s: start urgent %v (suspends %d) %+v at %d fps, display %+v; want urgent %v, %d suspends, %dx%d@%d",
				what, u, n, p.Source, p.FPS, vm, urgent, suspends, w, h, fps)
		}
		if p, _, _ := sim.Counts(); p != plugs {
			t.Fatalf("%s: %d plugs, want %d", what, p, plugs)
		}
		if len(sim.Monitors()) != 2 {
			t.Fatalf("%s: displays %+v", what, sim.Monitors())
		}
	}
	settings(t, s, proto.Prefs{FPS: 120, BitrateKbps: 50000})
	check("bitrate", false, 0, 1, 2560, 1440, 120)
	settings(t, s, proto.Prefs{FPS: 120, BitrateKbps: 50000, Width: 1920, Height: 1080})
	check("1080p", true, 1, 2, 1920, 1080, 120)
	settings(t, s, proto.Prefs{FPS: 60, BitrateKbps: 50000, Width: 1920, Height: 1080})
	check("60 fps", true, 2, 3, 1920, 1080, 60)
	// Back to the client's screen at 120 fps.
	settings(t, s, proto.Prefs{FPS: 120, BitrateKbps: 50000})
	check("native", true, 3, 4, 2560, 1440, 120)
	if l := r.logs.lines(`msg="virtual display changed"`); len(l) != 3 || !strings.Contains(l[0], "mode=1920x1080@120 was=2560x1440@120") {
		t.Fatalf("log %q", l)
	}
	if _, u, _ := sim.Counts(); u != 3 {
		t.Fatalf("unplugs %d: each replaced display removed", u)
	}
	s.closeVirtualDisplay()

	// A session without one (the client matched the monitor) gets one when
	// it asks for more frames than the monitor shows.
	r.a.closeVirtualDisplays() // the linger
	r.restored(t)
	hello = client(1920, 1080, 60)
	s, _ = r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	pl = &recPipeline{}
	s.video = pl
	settings(t, s, proto.Prefs{FPS: 60, BitrateKbps: 20000})
	if p, u, _ := pl.last(); u || p.Source.NativeW != 1920 || len(sim.Monitors()) != 1 {
		t.Fatalf("bitrate change: urgent %v %+v %+v", u, p.Source, sim.Monitors())
	}
	settings(t, s, proto.Prefs{FPS: 120, BitrateKbps: 20000})
	if p, u, _ := pl.last(); !u || p.FPS != 120 || p.Source.Output != virtualMonitor(t, sim, 1).DXGIOutput {
		t.Fatalf("120 fps: urgent %v %+v at %d fps", u, p.Source, p.FPS)
	}
	s.closeVirtualDisplay()
	r.a.closeVirtualDisplays()
	r.restored(t)
}

// TestVirtualDisplayLost: a virtual display whose driver stops answering
// (SudoVDA's watchdog removes it) is left: the video suspended, the displays
// restored, the user told, the stream restarted at once on the physical
// monitor (input mapped there), and the session creates no other one. A
// display Windows no longer lists (VDD, no keepalive) is left the same way:
// by the next generation, which captures the monitor where the restore put
// it, and by a running one whose capture only reports it lost (the native
// helper's DDA retries a vanished output).
func TestVirtualDisplayLost(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{VirtualDisplay: "auto", VirtualDisplayLinger: linger(time.Hour)})
	hello := client(2560, 1440, 120)
	s, ctrl := r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	pl := &recPipeline{}
	s.video = pl
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	sim.Lose()
	waitMsg(t, ctrl, `"t":"notice"`, "The virtual display is gone (its driver stopped answering); streaming the monitor.")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if p, u, n := pl.last(); u && n == 1 && p.Source.NativeW == 1920 {
			break
		}
		if time.Now().After(deadline) {
			p, u, n := pl.last()
			t.Fatalf("no restart on the monitor: urgent %v suspends %d %+v", u, n, p.Source)
		}
	}
	r.restored(t) // at once, despite the linger
	if got := (func() input.Rect { r.a.inj.MoveAbs(0, 0); return r.in.target() })(); got != (input.Rect{W: 1920, H: 1080}) {
		t.Fatalf("input target %+v", got)
	}
	settings(t, s, proto.Prefs{FPS: 144, Width: 2560, Height: 1440})
	if len(sim.Monitors()) != 1 {
		t.Fatal("a session that lost its virtual display created another one")
	}
	if l := r.logs.lines(`msg="virtual display not used"`); len(l) != 1 || !strings.Contains(l[0], "was lost") {
		t.Fatalf("log %q", l)
	}

	// VDD: nothing pings it; the next generation finds it gone, and maps
	// input to the monitor where the restore put it (back at (0, 0)).
	sim = vdisplay.NewSim(vdisplay.DriverVDD)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r = newVDRig(t, sim, Config{VirtualDisplay: "on", VirtualDisplayLinger: linger(time.Hour)})
	s, ctrl = r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	s.video = &recPipeline{}
	if p, err := s.buildParams(hello.Prefs); err != nil || p.Source.NativeW != 2560 {
		t.Fatalf("on the virtual display: %+v (%v)", p.Source, err)
	}
	sim.Lose()
	if p, err := s.buildParams(hello.Prefs); err != nil || p.Source.NativeW != 1920 || p.FPS != 60 {
		t.Fatalf("after it left: %+v at %d fps (%v)", p.Source, p.FPS, err)
	}
	r.restored(t)
	if got := (func() input.Rect { r.a.inj.MoveAbs(0, 0); return r.in.target() })(); got != (input.Rect{W: 1920, H: 1080}) {
		t.Fatalf("input target %+v", got)
	}
	waitMsg(t, ctrl, `"t":"notice"`, "The virtual display is gone (Windows no longer lists it)")

	// VDD under a running generation that only reports its capture lost (the
	// native helper's DDA): the session notices that Windows no longer lists
	// the display and restarts at once on the monitor.
	sim = vdisplay.NewSim(vdisplay.DriverVDD)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r = newVDRig(t, sim, Config{VirtualDisplay: "on", VirtualDisplayLinger: linger(time.Hour)})
	s, ctrl = r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	pl = &recPipeline{}
	pl.events = make(chan media.VideoEvent, 1)
	s.video = pl
	go s.videoEvents()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	sim.Lose()
	pl.events <- media.VideoEvent{Capture: &media.CaptureChange{Reason: "lost", Width: 2560, Height: 1440, Text: "no output"}}
	waitMsg(t, ctrl, `"t":"notice"`, "The virtual display is gone (Windows no longer lists it); streaming the monitor.")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if p, u, n := pl.last(); u && n == 1 && p.Source.NativeW == 1920 && p.FPS == 60 {
			break
		}
		if time.Now().After(deadline) {
			p, u, n := pl.last()
			t.Fatalf("no restart on the monitor: urgent %v suspends %d %+v", u, n, p.Source)
		}
	}
	if s.onVirtualDisplay() {
		t.Fatal("the session still streams the virtual display")
	}
	r.restored(t)
}

// TestVirtualDisplayWindow: a session on a virtual display that switches to
// a window capture streams the window (FFmpeg gfxcapture), the display
// removed at once; a monitor capture afterwards decides again.
func TestVirtualDisplayWindow(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{Capture: "auto", VirtualDisplay: "auto", VirtualDisplayLinger: linger(time.Hour)})
	hello := client(2560, 1440, 120)
	s, _ := r.session(t, hello)
	s.openVirtualDisplay(hello.Prefs)
	pl := &recPipeline{}
	s.video = pl
	// A window is FFmpeg's gfxcapture also while the session still has one
	// (a restart before the settings reach updateVirtualDisplay).
	if b := s.captureBackend(proto.Prefs{FPS: 120, Window: "Notepad"}, virtualMonitor(t, sim, 1), true); b != "gfxcapture" {
		t.Fatalf("window on a virtual display: %s", b)
	}
	settings(t, s, proto.Prefs{FPS: 120, Window: "Notepad"})
	if p, u, n := pl.last(); !u || n != 1 || p.Source.Backend != "gfxcapture" || p.Source.Window != "Notepad" {
		t.Fatalf("window: urgent %v suspends %d %+v", u, n, p.Source)
	}
	if s.onVirtualDisplay() {
		t.Fatal("the session kept its virtual display for a window capture")
	}
	r.restored(t) // at once, despite the linger
	settings(t, s, proto.Prefs{FPS: 120})
	if p, u, _ := pl.last(); !u || p.Source.Backend != "ddagrab" || p.Source.Window != "" || p.Source.NativeW != 2560 || !s.onVirtualDisplay() {
		t.Fatalf("back to the monitor: urgent %v %+v, virtual %v", u, p.Source, s.onVirtualDisplay())
	}
	s.closeVirtualDisplay()
	r.a.closeVirtualDisplays()
	r.restored(t)
}

// TestVirtualDisplayReconnect: one virtual display per session; a client
// that reconnects within the linger gets the same one back (the policy sees
// the agent's own display as the monitor), else it is removed when the
// linger ends; a new session that takes over a running one with another
// mode replaces it, and the old session's end leaves the new display alone.
func TestVirtualDisplayReconnect(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{VirtualDisplay: "auto", VirtualDisplayLinger: linger(time.Second)})
	hello := client(2560, 1440, 120)
	s1, _ := r.session(t, hello)
	s1.openVirtualDisplay(hello.Prefs)
	name := virtualMonitor(t, sim, 1).Name
	s1.closeVirtualDisplay() // the connection dropped
	s2, _ := r.session(t, hello)
	s2.openVirtualDisplay(hello.Prefs)
	if p, u, _ := sim.Counts(); p != 1 || u != 0 || virtualMonitor(t, sim, 1).Name != name {
		t.Fatalf("reconnect: plugs %d unplugs %d", p, u)
	}
	if !strings.Contains(strings.Join(r.logs.lines(`msg="streaming a virtual display"`), "\n"), `reason="the monitor is the previous session's virtual display"`) {
		t.Fatalf("log %q", r.logs.lines(`msg="streaming a virtual display"`))
	}
	// Another device takes over (the old session still runs) at another mode.
	s3, _ := r.session(t, client(1920, 1080, 144))
	s3.openVirtualDisplay(s3.prefs)
	if p, u, _ := sim.Counts(); p != 2 || u != 1 {
		t.Fatalf("takeover: plugs %d unplugs %d", p, u)
	}
	s2.closeVirtualDisplay()
	if vm := virtualMonitor(t, sim, 1); vm.W != 1920 || vm.Hz != 144 {
		t.Fatalf("the replaced session's end touched the new display: %+v", vm)
	}
	if !s3.onVirtualDisplay() || s2.onVirtualDisplay() {
		t.Fatal("ownership")
	}
	s3.closeVirtualDisplay()
	if len(sim.Monitors()) != 2 {
		t.Fatal("removed before the linger")
	}
	time.Sleep(1500 * time.Millisecond)
	r.restored(t)
	if p, u, _ := sim.Counts(); p != 2 || u != 2 {
		t.Fatalf("plugs %d unplugs %d", p, u)
	}
}

// TestVirtualDisplayAgent: the agent restores the displays a crashed agent
// left (NewAgent replays the journal before any session), logs its policy
// and driver once, and removes a session's display when Run returns.
func TestVirtualDisplayAgent(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	dir := t.TempDir()
	path := filepath.Join(dir, "host.json")
	if err := os.WriteFile(path, []byte(`{"virtualDisplay":"auto","virtualDisplayLayout":"only","gpuPriority":"off","audio":false,"gamepad":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// The crashed agent: its virtual display and journal are left.
	if _, err := sim.Manager(cfg.virtualDisplayOptions()).Create(vdisplay.Mode{Width: 2560, Height: 1440, Hz: 120}); err != nil {
		t.Fatal(err)
	}
	if m := sim.Monitors(); len(m) != 1 || m[0].W != 2560 {
		t.Fatalf("crashed agent's displays %+v", m)
	}
	var opts vdisplay.Options
	defer func(f func(vdisplay.Options) *vdisplay.Manager) { newVirtualDisplays = f }(newVirtualDisplays)
	newVirtualDisplays = func(o vdisplay.Options) *vdisplay.Manager { opts = o; return sim.Manager(o) }
	logs := &lockedLog{}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := NewAgent(ctx, cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := &vdRig{sim: sim, dir: dir}
	r.restored(t)
	if _, _, rec := sim.Counts(); rec != 1 {
		t.Fatalf("recovers %d", rec)
	}
	if opts.Policy != "auto" || opts.Layout != "only" || opts.StateDir != dir || opts.Linger != defaultVirtualDisplayLinger || opts.Log == nil {
		t.Fatalf("options %+v", opts)
	}
	if l := logs.lines(`msg="virtual display"`); len(l) != 1 || !strings.Contains(l[0], "policy=auto layout=only linger=10s driver=") {
		t.Fatalf("log %q", l)
	}
	if l := logs.lines("restoring the displays after an unfinished virtual display session"); len(l) != 1 {
		t.Fatalf("log %q", l)
	}
	// A session's display (lingering for a reconnect) is removed at shutdown.
	d, err := a.vd.Create(vdisplay.Mode{Width: 2560, Height: 1440, Hz: 120})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	r.restored(t)
}

// TestRestoreVirtualDisplays: "recon-host vdisplay -restore", which
// uninstall-host.ps1 runs after it killed the agent and before it deletes
// the agent and its config: the virtual display a killed agent left (here
// with layout "only": the physical monitor off) is removed and the displays
// restored from the journal next to the config, whatever the policy says
// now; without a journal nothing happens.
func TestRestoreVirtualDisplays(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	dir := t.TempDir()
	cfg := &Config{VirtualDisplay: "auto", VirtualDisplayLayout: "only", HostID: "h1", path: filepath.Join(dir, "host.json")}
	defer func(f func(vdisplay.Options) *vdisplay.Manager) { newVirtualDisplays = f }(newVirtualDisplays)
	newVirtualDisplays = sim.Manager
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))

	if found, err := RestoreVirtualDisplays(cfg, log); found || err != nil {
		t.Fatalf("no journal: found %v, err %v", found, err)
	}
	if _, _, rec := sim.Counts(); rec != 0 {
		t.Fatalf("recovers %d without a journal", rec)
	}
	// The killed agent's display: neither the session nor the agent removed it.
	if _, err := sim.Manager(cfg.virtualDisplayOptions()).Create(vdisplay.Mode{Width: 2560, Height: 1440, Hz: 120}); err != nil {
		t.Fatal(err)
	}
	if m := sim.Monitors(); len(m) != 1 || m[0].W != 2560 {
		t.Fatalf("killed agent's displays %+v", m)
	}
	off := *cfg
	off.VirtualDisplay = "off" // turned off before uninstalling: restored all the same
	found, err := RestoreVirtualDisplays(&off, log)
	if !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	(&vdRig{sim: sim, dir: dir}).restored(t)
	if _, _, rec := sim.Counts(); rec != 1 {
		t.Fatalf("recovers %d", rec)
	}
	if l := logs.lines("restoring the displays after an unfinished virtual display session"); len(l) != 1 {
		t.Fatalf("log %q", l)
	}
	if found, err := RestoreVirtualDisplays(cfg, log); found || err != nil {
		t.Fatalf("again: found %v, err %v", found, err)
	}
}

// pipeConn is a transport.Conn whose only stream is the control stream a
// test client plays over a net.Pipe.
type pipeConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	ctrl   chan transport.BidiStream
}

type pipeStream struct{ net.Conn }

func (pipeStream) CancelRead()  {}
func (pipeStream) CancelWrite() {}

func (c *pipeConn) AcceptStream(ctx context.Context) (transport.BidiStream, error) {
	select {
	case st := <-c.ctrl:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *pipeConn) OpenStreamSync(context.Context) (transport.BidiStream, error) {
	return nil, errors.New("no streams")
}
func (c *pipeConn) AcceptUniStream(ctx context.Context) (transport.RecvStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *pipeConn) OpenUniStreamSync(context.Context) (transport.SendStream, error) {
	return &fakeStream{}, nil
}
func (c *pipeConn) SendDatagram([]byte) error { return nil }
func (c *pipeConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *pipeConn) Close(uint32, string) error { c.cancel(); return nil }
func (c *pipeConn) Context() context.Context   { return c.ctx }
func (c *pipeConn) RemoteAddr() net.Addr       { return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4433} }

// runClient runs a session of hello on the rig's agent over a pipeConn and
// returns the host's control messages as they arrive, a function that sends
// a client message and a channel closed when the session ended.
func runClient(t *testing.T, a *Agent, hello proto.Hello) (msgs func() []string, send func(any), done chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host, cl := net.Pipe()
	t.Cleanup(func() { host.Close(); cl.Close() })
	c := &pipeConn{ctx: ctx, cancel: cancel, ctrl: make(chan transport.BidiStream, 1)}
	c.ctrl <- pipeStream{host}
	done = make(chan struct{})
	go func() { a.HandleConn(c, SessionMeta{Path: "direct"}); close(done) }()
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			b, err := proto.ReadMsg(cl, proto.MaxControlMsg)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(b))
			mu.Unlock()
		}
	}()
	send = func(v any) {
		b, _ := json.Marshal(v)
		if err := proto.WriteMsg(cl, b); err != nil {
			t.Error(err)
		}
	}
	if _, err := cl.Write([]byte{proto.StreamKindControl}); err != nil {
		t.Fatal(err)
	}
	hello.T = "hello"
	send(hello)
	msgs = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	return msgs, send, done
}

// waitFor waits until a message of msgs contains all parts and returns it.
func waitFor(t *testing.T, msgs func() []string, parts ...string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		for _, m := range msgs() {
			if hasMsg([]string{m}, parts...) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message with %q in %q", parts, msgs())
		}
	}
}

// TestVirtualDisplaySessionRun: a whole session (Agent.HandleConn) on the
// native helper: the virtual display exists before the helper is launched
// (its caps list the display's output) and the welcome is sent (it lists
// the display alone); the helper captures it with DDA by its HMONITOR, also
// with host config capture "amf"; the client's bye ends the session and the
// displays are restored. A session whose video cannot start (no codec in
// common) restores them too.
func TestVirtualDisplaySessionRun(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	r := newVDRig(t, sim, Config{Capture: "amf", Pipeline: "helper", VirtualDisplay: "auto", VirtualDisplayLinger: linger(0)})
	starts := make(chan map[string]any, 4)
	var launchMons [][]platform.Monitor
	var mu sync.Mutex
	r.a.launchHelper = func(_ *slog.Logger, backend string) (*encoder.Helper, error) {
		mons := sim.Monitors()
		mu.Lock()
		launchMons = append(launchMons, mons)
		mu.Unlock()
		var outs []string
		for _, m := range mons {
			outs = append(outs, output(m.Name, m.HMonitor, "00000000:0000a1b2", "AMD Radeon RX 7900 XT", "amd"))
		}
		caps := onGPUs(capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeHEVC, noNVENC+","+noIntel), strings.Join(outs, ","))
		caps = strings.Replace(caps, `"capture":["dda"]`, `"capture":["dda","amd-direct"]`, 1)
		h, _, err := encoder.LaunchFake(caps, func(f *encoder.Fake, m map[string]any) {
			if m["t"] == "start" {
				starts <- m
			}
		})
		return h, err
	}
	msgs, send, done := runClient(t, r.a, proto.Hello{V: proto.HelloVersionFrameExt, Client: proto.ClientInfo{Width: 2560, Height: 1440, DPR: 1, Hz: 120},
		Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}}, Prefs: proto.Prefs{FPS: 120}})
	welcome := waitFor(t, msgs, `"t":"welcome"`)
	var w proto.Welcome
	if err := json.Unmarshal([]byte(welcome), &w); err != nil {
		t.Fatal(err)
	}
	vm := virtualMonitor(t, sim, 1)
	if len(w.Monitors) != 1 || !w.Monitors[0].Virtual || w.Monitors[0].Width != 2560 || w.Monitors[0].Hz != 120 {
		t.Fatalf("welcome monitors %+v", w.Monitors)
	}
	var start map[string]any
	select {
	case start = <-starts:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper got no start")
	}
	if start["capture"] != "dda" || start["hmonitor"] != float64(vm.HMonitor) || start["fps"] != float64(120) || start["codec"] != "hevc" {
		t.Fatalf("start %v, want dda of hmonitor %#x at 120 fps", start, vm.HMonitor)
	}
	mu.Lock()
	first := launchMons[0]
	mu.Unlock()
	if len(first) != 2 || first[0].HMonitor != vm.HMonitor {
		t.Fatalf("the helper was launched before the virtual display existed: %+v", first)
	}
	if l := r.logs.lines(`msg="video pipeline"`); len(l) != 1 || !strings.Contains(l[0], "pipeline=helper") {
		t.Fatalf("log %q", l)
	}
	send(proto.ClientMsg{T: "bye"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	r.restored(t)

	// The video cannot start: the displays are restored as the session ends.
	r.a.launchHelper = nil
	r.a.cfg.Pipeline = "ffmpeg"
	msgs, _, done = runClient(t, r.a, proto.Hello{V: proto.HelloVersionFrameExt, Client: proto.ClientInfo{Width: 2560, Height: 1440}, Prefs: proto.Prefs{FPS: 120}})
	waitFor(t, msgs, `"t":"notice"`, "Could not start video")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	r.restored(t)
	if p, u, _ := sim.Counts(); p != 2 || u != 2 {
		t.Fatalf("plugs %d unplugs %d", p, u)
	}
}

// TestVirtualDisplayHDR: HDR10 on a virtual display (3.7 wiring with
// 3.9/4.5). The session asks the helper for HDR10 of the virtual display
// (DDA by its HMONITOR, start hdr), and the display's own Windows HDR mode
// decides, as for a physical monitor: the agent does not switch Windows HDR
// on for it (docs/VENDOR_NOTES.md 3.9/4.5, "HDR on a virtual display"). A
// virtual display in SDR mode streams SDR with the reason; HDR turned on for
// it in Windows restarts the stream as HDR10; a new size replaces the display
// (a monitor of the same identity, whose HDR setting Windows keeps: the fake
// keeps it for every virtual display) and the next generation asks for HDR10
// again; an HDR-only settings change (the client's display left HDR mode)
// restarts the video without touching the virtual display.
func TestVirtualDisplayHDR(t *testing.T) {
	sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
	sim.AddMonitor(1920, 1080, 0, 0, 60)
	physical := sim.Monitors()[0].HMonitor
	r := newVDRig(t, sim, Config{Pipeline: "helper", HDR: proto.HDRAuto, VirtualDisplay: "auto", VirtualDisplayLinger: linger(0)})
	const hevc10 = `"hevc":{"maxW":8192,"maxH":4352,"tenBit":true,"hdr10":true,"forceIdr":true,"recovery":"ltr","maxLtr":2,"liveBitrate":"seamless","alignW":1,"alignH":1}`
	var mu sync.Mutex
	virtualHDR := false // Windows HDR of the virtual display (the physical monitor's is on)
	type start struct {
		f *encoder.Fake
		m map[string]any
	}
	starts := make(chan start, 8)
	r.a.launchHelper = func(_ *slog.Logger, backend string) (*encoder.Helper, error) {
		var outs []string
		for _, m := range sim.Monitors() {
			outs = append(outs, output(m.Name, m.HMonitor, "00000000:0000a1b2", "AMD Radeon RX 7900 XT", "amd"))
		}
		caps := onGPUs(capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", hevc10, noNVENC+","+noIntel), strings.Join(outs, ","))
		h, _, err := encoder.LaunchFake(caps, func(f *encoder.Fake, m map[string]any) {
			if m["t"] != "start" {
				return
			}
			mu.Lock()
			on := m["hdr"] == true && (virtualHDR || m["hmonitor"] == float64(physical))
			mu.Unlock()
			st := encoder.Started{Backend: "amf", Capture: "dda", Codec: "hevc", Width: 2560, Height: 1440, FPS: 120, Kbps: int(m["kbps"].(float64)),
				LiveBitrate: "seamless", BitDepth: 8, ColorSpace: "bt709"}
			if on {
				st.HDR, st.BitDepth, st.ColorSpace = true, 10, "bt2020-pq"
			}
			f.Send(st)
			starts <- start{f, m}
		})
		return h, err
	}
	key := []byte{0, 0, 0, 1, 0x26, 0x01, 0xaf} // HEVC IDR slice without parameter sets: the generic codec string
	next := func(what string) start {
		t.Helper()
		select {
		case s := <-starts:
			s.f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
			return s
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the helper got no start", what)
			return start{}
		}
	}
	hdr := &proto.HDRPrefs{Mode: proto.HDRAuto, Display: true, Canvas: true, Decoders: []string{"hevc"}}
	prefs := proto.Prefs{FPS: 120, HDR: hdr}
	msgs, send, done := runClient(t, r.a, proto.Hello{V: proto.HelloVersionRecovery, Client: proto.ClientInfo{Width: 2560, Height: 1440, DPR: 1, Hz: 120},
		Decoders: []proto.DecoderInfo{{Family: "hevc", HW: true}}, Prefs: prefs})

	// A new virtual display is SDR: asked for HDR10, streamed SDR with the reason.
	s := next("first generation")
	vm := virtualMonitor(t, sim, 1)
	if s.m["capture"] != "dda" || s.m["hmonitor"] != float64(vm.HMonitor) || s.m["hdr"] != true || s.m["codec"] != "hevc" {
		t.Fatalf("start %v, want HEVC with hdr, dda of the virtual display %#x", s.m, vm.HMonitor)
	}
	waitFor(t, msgs, `"t":"video"`, `"gen":1`, `"hdrNote":"the host display is not in Windows HDR mode"`)
	if hasMsg(msgs(), `"gen":1`, `"hdr":true`) {
		t.Fatal("an SDR virtual display's stream announced as HDR10")
	}

	// HDR turned on for the virtual display in Windows: HDR10.
	mu.Lock()
	virtualHDR = true
	mu.Unlock()
	s.f.Send(encoder.CaptureChanged{Reason: "hdr", Width: 2560, Height: 1440, HDR: true})
	s = next("Windows HDR on")
	if s.m["hmonitor"] != float64(vm.HMonitor) || s.m["hdr"] != true {
		t.Fatalf("start %v after Windows HDR turned on", s.m)
	}
	waitFor(t, msgs, `"t":"video"`, `"gen":2`, `"hdr":true`, `"bitDepth":10`)

	// A new size: a new virtual display, asked for HDR10 again.
	p2 := prefs
	p2.Width, p2.Height = 1920, 1080
	send(proto.ClientMsg{T: "settings", Prefs: &p2})
	s = next("1080p")
	vm2 := virtualMonitor(t, sim, 1)
	if vm2.HMonitor == vm.HMonitor || vm2.W != 1920 || s.m["hmonitor"] != float64(vm2.HMonitor) || s.m["hdr"] != true {
		t.Fatalf("start %v on %+v, want hdr on the new 1920x1080 virtual display", s.m, vm2)
	}
	waitFor(t, msgs, `"t":"video"`, `"gen":3`, `"hdr":true`)
	if l := r.logs.lines(`msg="virtual display changed"`); len(l) != 1 {
		t.Fatalf("log %q", l)
	}

	// The client's display leaves HDR mode: an HDR-only change restarts the
	// video (SDR, the reason) on the same virtual display.
	plugs, _, _ := sim.Counts()
	p3 := p2
	p3.HDR = &proto.HDRPrefs{Mode: proto.HDRAuto, Display: false, Canvas: true, Decoders: []string{"hevc"}}
	send(proto.ClientMsg{T: "settings", Prefs: &p3})
	s = next("client display SDR")
	if s.m["hmonitor"] != float64(vm2.HMonitor) || s.m["hdr"] != nil {
		t.Fatalf("start %v, want no hdr on the same virtual display %#x", s.m, vm2.HMonitor)
	}
	waitFor(t, msgs, `"t":"video"`, `"gen":4`, `"hdrNote":"the client's display is not in HDR mode"`)
	if p, _, _ := sim.Counts(); p != plugs {
		t.Fatalf("an HDR-only change plugged a virtual display (%d plugs, was %d)", p, plugs)
	}
	var why []string
	for _, l := range r.logs.lines(`msg="restarting video"`) {
		why = append(why, l[strings.Index(l, "reason="):])
	}
	if want := []string{`reason="Windows HDR turned on" urgent=false`, "reason=settings urgent=true", `reason="HDR settings" urgent=false`}; !slices.Equal(why, want) {
		t.Fatalf("restarts %q, want %q", why, want)
	}
	send(proto.ClientMsg{T: "bye"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	r.restored(t)
}
