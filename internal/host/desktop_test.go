package host

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
)

// fakeDesktop stands in for platform.DesktopState and ConnectToConsole: the
// state is what set last; a move to the console connects the session (when
// moves work) and counts.
type fakeDesktop struct {
	mu      sync.Mutex
	d       platform.Desktop
	moves   atomic.Int32
	moveErr error
}

func (f *fakeDesktop) set(d platform.Desktop) {
	f.mu.Lock()
	f.d = d
	f.mu.Unlock()
}

func (f *fakeDesktop) state() platform.Desktop {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.d
}

func (f *fakeDesktop) connect() error {
	f.moves.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moveErr != nil {
		return f.moveErr
	}
	f.d = platform.Desktop{Session: f.d.Session, Console: f.d.Session, Known: true, Input: "Default"}
	return nil
}

// proxmoxVM is the firmware's machine name of a Proxmox VM (OSInfo).
const proxmoxVM = "QEMU Standard PC (Q35 + ICH9, 2009)"

// useFakeDesktop installs f for the test, with short intervals, on a
// virtual machine (proxmoxVM).
func useFakeDesktop(t *testing.T, f *fakeDesktop) {
	t.Helper()
	ds, cc, oi, every, retry, settle := desktopState, connectConsole, osInfo, desktopEvery, desktopRetry, consoleSettle
	desktopState, connectConsole = f.state, f.connect
	osInfo = func() (string, string) { return "Windows 11 Pro 24H2 (build 26100.1)", proxmoxVM }
	desktopEvery, desktopRetry, consoleSettle = 10*time.Millisecond, time.Hour, 2*time.Second
	t.Cleanup(func() {
		desktopState, connectConsole, osInfo, desktopEvery, desktopRetry, consoleSettle = ds, cc, oi, every, retry, settle
	})
}

// gdiFail fails the session's current generation as gdigrab fails where the
// session has no desktop on a screen.
func gdiFail(t *testing.T, s *Session) {
	t.Helper()
	p, _ := s.vid().Current()
	const stderr = "[in#0/gdigrab @ 000001c4] Failed to capture image (error 5) | Error during demuxing: I/O error"
	s.handleEncoderFailure(media.VideoEvent{Err: errors.New("encoder libx264 exited: " + stderr), Failed: &p,
		CaptureFailure: media.ClassifyCapture(stderr)})
}

// TestDesktopLockedWaits: on a VM without a GPU whose session is locked (or
// shows a UAC prompt), every GDI generation fails. The failures do not count
// (the session used to end after the seventh, about 10 s, with "Video
// encoder keeps failing"); the user gets one informational notice saying
// why and what brings the picture back; the video restarts as soon as the
// desktop is back.
func TestDesktopLockedWaits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in encoder is a shell script")
	}
	f := &fakeDesktop{}
	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Input: "Default"})
	useFakeDesktop(t, f)
	ff, args := sleepingFFmpeg(t)
	caps := gdiTestCaps(noDevice, true)
	caps.FFmpeg = ff
	a := gdiAgent(Config{Capture: "auto"}, caps)
	logs := &lockedLog{}
	s, ctrl := gdiSession(a, logs)
	s.video = media.NewVideo(caps, s.log, a.clock)
	defer stopGDISession(s)
	s.watchDesktop()
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, logs, "no first generation", func() bool { return len(ffmpegRuns(args)) == 1 })

	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Locked: true, Secure: true})
	for i := 0; i < 2*maxVideoFailures; i++ {
		gdiFail(t, s)
	}
	if s.failures != 0 || s.ctx.Err() != nil || len(logs.lines(`msg="session ended"`)) != 0 {
		t.Fatalf("the failures counted: %d failures, ctx %v", s.failures, s.ctx.Err())
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(ffmpegRuns(args)); n != 1 {
		t.Fatalf("%d generations started while the desktop was away", n)
	}
	want := []string{"info: No GPU screen capture on this PC (" + noDevice + "): capturing with GDI, which uses more CPU and adds latency.",
		"info: The PC is locked: Windows lets no program show or control its sign-in screen. Sign in at the PC itself " +
			"(on a VM: its console in the hypervisor); the stream comes back by itself."}
	if got := noticeLines(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices %q, want %q", got, want)
	}

	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Input: "Default"})
	waitUntil(t, logs, "no restart once the desktop was back", func() bool { return len(ffmpegRuns(args)) == 2 })
	if l := logs.lines(`msg="restarting video" reason="the desktop is back"`); len(l) != 1 {
		t.Fatalf("restart log %q", l)
	}
	if l := logs.lines(`msg="the session's desktop is back" was=locked waiting=true`); len(l) != 1 {
		t.Fatalf("back log %q", l)
	}
	// A second spell is told again.
	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Secure: true})
	gdiFail(t, s)
	if got := noticeLines(t, ctrl); len(got) != 3 || !strings.HasPrefix(got[2], "info: The PC shows a secure screen (a UAC prompt") {
		t.Fatalf("notices %q", got)
	}
}

// TestDesktopDisconnected: a session a remote desktop client left
// disconnected (a VM someone reached over RDP) is moved to the console at
// the stream's start, and its first generation captures the console's
// desktop; with host config "consoleSession" "off", or where the move
// fails, the video waits and the notice says what to do.
func TestDesktopDisconnected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in encoder is a shell script")
	}
	disconnected := platform.Desktop{Session: 3, Console: 1, Known: true, Disconnected: true}

	t.Run("moved at the start", func(t *testing.T) {
		f := &fakeDesktop{}
		f.set(disconnected)
		useFakeDesktop(t, f)
		a := gdiAgent(Config{Capture: "auto"}, gdiTestCaps(noDevice, true))
		logs := &lockedLog{}
		s, ctrl := gdiSession(a, logs)
		defer s.cancel()
		s.prepareDesktop()
		if why, _ := s.desktopNow(); why != "" || f.moves.Load() != 1 {
			t.Fatalf("after the start: away %q, %d moves", why, f.moves.Load())
		}
		want := []string{"info: This PC's Windows session was disconnected (a remote desktop client closed): it was moved to the PC's console " +
			"(session 3) to stream it, where it shows unlocked (on a VM: in the hypervisor's console)."}
		if got := noticeLines(t, ctrl); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("notices %q, want %q", got, want)
		}
		l := logs.lines(`level=WARN msg="moved the disconnected session to the console (as tscon <session> /dest:console): the console shows it unlocked"`)
		if len(l) != 1 || !strings.Contains(l[0], `because="a virtual machine (`+proxmoxVM+`)"`) || !strings.Contains(l[0], "console_session=1") {
			t.Fatalf("log %q", l)
		}
	})

	for _, c := range []struct {
		name, consoleSession string
		gpuPC                bool
		moveErr              error
		moves                int32
		tail                 string
	}{
		{"off", "off", false, nil, 0, ` (Not moved to the console by itself: host config "consoleSession" is "off".)`},
		{"move fails", "", false, errors.New("alice is signed in at the console"), 1, " (Moving it to the console failed: alice is signed in at the console.)"},
		{"a PC with a GPU", "", true, nil, 0, ` (Not moved to the console by itself: on a PC with a GPU its own monitor would show the unlocked desktop; ` +
			`host config "consoleSession" "on" allows it.)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeDesktop{moveErr: c.moveErr}
			f.set(disconnected)
			useFakeDesktop(t, f)
			if c.gpuPC {
				usePhysicalGPU(t)
			}
			ff, args := sleepingFFmpeg(t)
			caps := gdiTestCaps(noDevice, true)
			caps.FFmpeg = ff
			a := gdiAgent(Config{Capture: "auto", ConsoleSession: c.consoleSession}, caps)
			logs := &lockedLog{}
			s, ctrl := gdiSession(a, logs)
			s.video = media.NewVideo(caps, s.log, a.clock)
			defer stopGDISession(s)
			s.prepareDesktop()
			if err := s.startVideo(false, ""); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, logs, "no first generation", func() bool { return len(ffmpegRuns(args)) == 1 })
			gdiFail(t, s)
			gdiFail(t, s)
			waitUntil(t, logs, "no notice", func() bool { return len(noticeLines(t, ctrl)) == 2 })
			time.Sleep(5 * desktopEvery)
			if f.moves.Load() != c.moves {
				t.Fatalf("%d moves to the console, want %d", f.moves.Load(), c.moves)
			}
			got := noticeLines(t, ctrl)
			want := "info: This PC's Windows session has no screen: a remote desktop client disconnected from it. Sign in at the PC " +
				"itself, or run \"tscon 3 /dest:console\" as administrator in that session; the stream comes back by itself." + c.tail
			if len(got) != 2 || got[1] != want {
				t.Fatalf("notices %q, want the GDI one and %q", got, want)
			}
			// Signed in at the console (or tscon by hand): the video restarts.
			f.set(platform.Desktop{Session: 3, Console: 3, Known: true, Input: "Default"})
			waitUntil(t, logs, "no restart once the session was at the console", func() bool { return len(ffmpegRuns(args)) == 2 })
		})
	}
}

// TestDesktopRetry: while the video waits for a desktop that stays away,
// it is restarted every desktopRetry anyway (a state Windows misreports must
// not leave the stream without a picture for good); a generation that fails
// again waits again, without counting.
func TestDesktopRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in encoder is a shell script")
	}
	f := &fakeDesktop{}
	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Input: "Default"})
	useFakeDesktop(t, f)
	desktopRetry = 100 * time.Millisecond
	ff, args := sleepingFFmpeg(t)
	caps := gdiTestCaps(noDevice, true)
	caps.FFmpeg = ff
	a := gdiAgent(Config{Capture: "auto"}, caps)
	logs := &lockedLog{}
	s, ctrl := gdiSession(a, logs)
	s.video = media.NewVideo(caps, s.log, a.clock)
	defer stopGDISession(s)
	if err := s.startVideo(false, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, logs, "no first generation", func() bool { return len(ffmpegRuns(args)) == 1 })
	f.set(platform.Desktop{Session: 2, Console: 2, Known: true, Secure: true})
	gdiFail(t, s)
	waitUntil(t, logs, "no retry while the desktop was away", func() bool { return len(ffmpegRuns(args)) == 2 })
	gdiFail(t, s)
	if l := logs.lines(`msg="restarting video" reason="retrying while the desktop is away (secure)"`); len(l) != 1 || s.failures != 0 {
		t.Fatalf("retry log %q, %d failures", l, s.failures)
	}
	if got := noticeLines(t, ctrl); len(got) != 2 {
		t.Fatalf("notices %q", got)
	}
}

// usePhysicalGPU makes the test's PC a physical one with a GPU (its
// firmware names no hypervisor, DXGI lists an AMD GPU).
func usePhysicalGPU(t *testing.T) {
	t.Helper()
	oi, ga := osInfo, gpuAdapters
	osInfo = func() (string, string) {
		return "Windows 11 Pro 24H2 (build 26100.1)", "Micro-Star International Co., Ltd. MS-7D25"
	}
	gpuAdapters = func() ([]platform.Adapter, error) {
		return []platform.Adapter{{Vendor: "amd", VendorID: 0x1002, Name: "AMD Radeon RX 7900 XT"}}, nil
	}
	t.Cleanup(func() { osInfo, gpuAdapters = oi, ga })
}

// TestDesktopConsoleMove: a disconnected session is moved to the console
// only while the stream plays (a hidden client's tab does not move a
// session nobody watches onto the console, unlocked) and only after
// consoleLooks looks in a row found it disconnected (not for a moment of an
// RDP client taking the session over); on a PC with a GPU only with host
// config "consoleSession" "on"; on a PC without a GPU (no hypervisor named)
// by default.
func TestDesktopConsoleMove(t *testing.T) {
	connected := platform.Desktop{Session: 2, Console: 2, Known: true, Input: "Default"}
	disconnected := platform.Desktop{Session: 2, Console: 5, Known: true, Disconnected: true}
	start := func(t *testing.T, cfg Config) (*fakeDesktop, *Session, *fakeCtrl) {
		f := &fakeDesktop{}
		f.set(connected)
		useFakeDesktop(t, f)
		a := gdiAgent(cfg, gdiTestCaps(noDevice, true))
		s, ctrl := gdiSession(a, nil)
		t.Cleanup(func() { s.cancel(); s.watches.Wait() })
		return f, s, ctrl
	}

	t.Run("paused", func(t *testing.T) {
		f, s, ctrl := start(t, Config{Capture: "auto"})
		s.paused.Store(true)
		s.watchDesktop()
		f.set(disconnected)
		time.Sleep(20 * desktopEvery)
		if f.moves.Load() != 0 || len(noticeLines(t, ctrl)) != 0 {
			t.Fatalf("while paused: %d moves, notices %q", f.moves.Load(), noticeLines(t, ctrl))
		}
		s.paused.Store(false) // resumed: the next looks move it
		deadline := time.Now().Add(5 * time.Second)
		for f.moves.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(desktopEvery)
		}
		if f.moves.Load() != 1 {
			t.Fatalf("after the resume: %d moves", f.moves.Load())
		}
	})

	t.Run("a moment disconnected", func(t *testing.T) {
		f, s, _ := start(t, Config{Capture: "auto"})
		s.lookAtDesktop()
		f.set(disconnected)
		s.lookAtDesktop() // one look disconnected
		f.set(connected)
		s.lookAtDesktop()
		f.set(disconnected)
		s.lookAtDesktop()
		if f.moves.Load() != 0 {
			t.Fatalf("%d moves after single looks disconnected", f.moves.Load())
		}
		s.lookAtDesktop() // the second in a row
		if f.moves.Load() != 1 {
			t.Fatalf("%d moves after two looks in a row", f.moves.Load())
		}
	})

	for _, c := range []struct {
		name, consoleSession string
		gpuPC, noGPU         bool
		moves                int32
	}{
		{"a PC with a GPU, on", "on", true, false, 1},
		{"a PC with a GPU, auto", "", true, false, 0},
		{"a PC without a GPU, auto", "", false, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, s, _ := start(t, Config{Capture: "auto", ConsoleSession: c.consoleSession})
			switch {
			case c.gpuPC:
				usePhysicalGPU(t)
			case c.noGPU:
				usePhysicalGPU(t)
				gpuAdapters = func() ([]platform.Adapter, error) {
					return []platform.Adapter{{Vendor: "other", Name: "Microsoft Basic Render Driver", Software: true}}, nil
				}
			}
			f.set(disconnected)
			s.lookAtDesktop()
			s.lookAtDesktop()
			if f.moves.Load() != c.moves {
				t.Fatalf("%d moves, want %d", f.moves.Load(), c.moves)
			}
		})
	}
}

// TestVirtualMachine: the firmware names of hypervisors' machines.
func TestVirtualMachine(t *testing.T) {
	for m, vm := range map[string]bool{
		proxmoxVM:                                    true,
		"QEMU Standard PC (i440FX + PIIX, 1996)":     true,
		"Microsoft Corporation Virtual Machine":      true,
		"VMware, Inc. VMware20,1":                    true,
		"innotek GmbH VirtualBox":                    true,
		"Xen HVM domU":                               true,
		"Micro-Star International Co., Ltd. MS-7D25": false,
		"ASUS System Product Name":                   false,
		"":                                           false,
	} {
		if virtualMachine(m) != vm {
			t.Errorf("%q: virtual machine %v, want %v", m, !vm, vm)
		}
	}
}

// TestConfigConsoleSession: host config "consoleSession" is auto (the
// default), on or off.
func TestConfigConsoleSession(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.json")
	for v, ok := range map[string]bool{"": true, "auto": true, "on": true, "off": true, "always": false} {
		if err := os.WriteFile(p, []byte(`{"consoleSession":"`+v+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); (err == nil) != ok || err != nil && !strings.Contains(err.Error(), "consoleSession") {
			t.Errorf("%q: %v", v, err)
		}
	}
}
