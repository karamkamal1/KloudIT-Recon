package vdisplay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

var virtLUID = LUID{Low: 0x5a5a, High: 1}

// rig is a host with one 1920x1080@60 monitor and a fake SudoVDA.
func rig(t *testing.T, opts Options, mod func(*fakeDriver)) (*Manager, *fakeSys, *fakeDriver, *fakeTarget) {
	t.Helper()
	sys := newFakeSys()
	phys := sys.addPhysical(1, 1920, 1080, 0, 0, 60)
	drv := &fakeDriver{sys: sys, name: DriverSudoVDA, adapter: virtLUID}
	if mod != nil {
		mod(drv)
	}
	if opts.Policy == "" {
		opts.Policy = PolicyAuto
	}
	m := newManager(opts, sys, []driver{drv})
	m.poll, m.activateAt, m.appearWait, m.departWait, m.monitorWait = time.Millisecond, 20*time.Millisecond, time.Second, time.Second, time.Second
	return m, sys, drv, phys
}

var mode1440 = Mode{Width: 2560, Height: 1440, Hz: 120}

func hasFlag(fs []uint32, f uint32) bool {
	for _, x := range fs {
		if x&f == f {
			return true
		}
	}
	return false
}

func TestCreatePrimaryAndRestore(t *testing.T) {
	dir := t.TempDir()
	m, sys, drv, phys := rig(t, Options{StateDir: dir}, nil)
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	info := d.Info()
	t.Logf("info %+v", info)
	if info.Driver != DriverSudoVDA || info.Mode != mode1440 || info.RefreshHz != 120 || info.Layout != LayoutPrimary {
		t.Fatalf("info %+v", info)
	}
	if mon := info.Monitor; !mon.Primary || mon.X != 0 || mon.Y != 0 || mon.W != 2560 || mon.H != 1440 || mon.HMonitor == 0 || mon.Name != info.Name {
		t.Fatalf("monitor %+v", mon)
	}
	// The physical monitor keeps its place relative to the virtual one: left of it.
	if st, _ := sys.state(phys.t); !st.active || st.x != -1920 || st.y != 0 || st.w != 1920 {
		t.Fatalf("physical monitor %+v", st)
	}
	if !hasFlag(sys.flags(), SDCSaveToDatabase) {
		t.Fatalf("a SudoVDA session layout is saved to the display database: flags %#x", sys.flags())
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); err != nil {
		t.Fatalf("journal while the display exists: %v", err)
	}
	if found, ok := info.Find([]platform.Monitor{{Name: `\\.\DISPLAY1`}, {Name: strings.ToLower(info.Name), W: 1}}); !ok || found.W != 1 {
		t.Fatalf("Find: %+v %v", found, ok)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 || st.y != 0 || st.w != 1920 || st.h != 1080 {
		t.Fatalf("physical monitor after restore %+v", st)
	}
	if _, ok := sys.state(drv.target); ok {
		t.Fatal("the virtual monitor is still connected")
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after restore: %v", err)
	}
	last := sys.flags()[len(sys.flags())-1]
	if last&SDCSaveToDatabase != 0 || last&SDCUseSuppliedDisplayConf == 0 {
		t.Fatalf("restore flags %#x: the saved layout must be applied without saving", last)
	}
	if err := d.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if p, u, _ := drv.count(); p != 1 || u != 1 {
		t.Fatalf("plugs %d unplugs %d", p, u)
	}
}

func TestCreateExtend(t *testing.T) {
	m, sys, _, phys := rig(t, Options{Layout: LayoutExtend}, nil)
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if mon := d.Info().Monitor; mon.Primary || mon.X != 1920 || mon.Y != 0 {
		t.Fatalf("extended display %+v", mon)
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor moved: %+v", st)
	}
}

func TestCreateOnly(t *testing.T) {
	m, sys, _, phys := rig(t, Options{Layout: LayoutOnly}, nil)
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := sys.state(phys.t); st.active {
		t.Fatal("the physical monitor stays on with layout only")
	}
	if mon := d.Info().Monitor; !mon.Primary || mon.W != 2560 {
		t.Fatalf("display %+v", mon)
	}
	if hasFlag(sys.flags(), SDCSaveToDatabase) {
		t.Fatal("layout only must never be saved to the display database (the physical monitors would stay dark)")
	}
	d.Close()
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 || st.w != 1920 {
		t.Fatalf("physical monitor after restore %+v", st)
	}
}

func TestNeverRotated(t *testing.T) {
	m, sys, drv, _ := rig(t, Options{}, func(d *fakeDriver) { d.rotation = 2 }) // arrives rotated 90 degrees
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if st, _ := sys.state(drv.target); st.rotation != RotationIdentity || st.w != 2560 || st.h != 1440 {
		t.Fatalf("virtual monitor %+v", st)
	}
}

func TestPortraitClientGetsTallMode(t *testing.T) {
	m, sys, drv, _ := rig(t, Options{}, nil)
	d, err := m.Create(Mode{Width: 1080, Height: 2400, Hz: 60})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if st, _ := sys.state(drv.target); st.rotation != RotationIdentity || st.w != 1080 || st.h != 2400 {
		t.Fatalf("virtual monitor %+v", st)
	}
}

func TestInactiveMonitorIsSwitchedOn(t *testing.T) {
	m, _, _, _ := rig(t, Options{}, func(d *fakeDriver) { d.inactive = true })
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if mon := d.Info().Monitor; !mon.Primary || mon.W != 2560 {
		t.Fatalf("display %+v", mon)
	}
}

func TestClonedMonitorGetsOwnSource(t *testing.T) {
	m, sys, _, phys := rig(t, Options{}, func(d *fakeDriver) { d.cloned = true })
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if st, _ := sys.state(phys.t); !st.active || st.x != -1920 {
		t.Fatalf("physical monitor %+v", st)
	}
}

func TestTargetFoundWhenLUIDDiffers(t *testing.T) {
	dir := t.TempDir()
	m, sys, drv, _ := rig(t, Options{StateDir: dir}, func(d *fakeDriver) {
		d.reportLUID = LUID{Low: 0xdead}
		d.departIn = 30 * time.Millisecond
	})
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	if d.Info().Target.Adapter != virtLUID {
		t.Fatalf("target %v", d.Info().Target)
	}
	// The journal (Recover) and the departure wait use CCD's target.
	b, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil || j.Plug.Target != d.Info().Target {
		t.Fatalf("journal plug %+v (%v)", j.Plug, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Restored after the monitor left, not while it was still connected.
	time.Sleep(60 * time.Millisecond)
	ev := sys.eventLog()
	if len(ev) < 2 || ev[len(ev)-1] != "apply" || ev[len(ev)-2] != fmt.Sprintf("depart %v", drv.target) {
		t.Fatalf("events %v", ev)
	}
}

func TestUnsupportedRefreshIsAccepted(t *testing.T) {
	m, _, _, _ := rig(t, Options{}, func(d *fakeDriver) { d.hzs = []float64{60} })
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if info := d.Info(); info.RefreshHz != 60 || info.Mode != (Mode{2560, 1440, 60}) {
		t.Fatalf("info %+v", info)
	}
}

func TestFailedSetupRollsBack(t *testing.T) {
	dir := t.TempDir()
	m, sys, drv, phys := rig(t, Options{StateDir: dir}, nil)
	sys.fail = map[uint32]int{sdcSupplied | SDCSaveToDatabase | SDCAllowChanges: 1, sdcSupplied | SDCSaveToDatabase: 1}
	_, err := m.Create(mode1440)
	if err == nil || !strings.Contains(err.Error(), "SetDisplayConfig: ERROR_INVALID_PARAMETER") {
		t.Fatalf("err %v", err)
	}
	if _, ok := sys.state(drv.target); ok {
		t.Fatal("the virtual monitor was not removed after the failure")
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left: %v", err)
	}
	if m.cur != nil {
		t.Fatal("manager keeps a failed display")
	}
}

func TestRestoreFallsBackToDatabase(t *testing.T) {
	m, sys, _, phys := rig(t, Options{}, nil)
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	sys.mu.Lock()
	sys.fail = map[uint32]int{sdcSupplied: 1}
	sys.mu.Unlock()
	// The supplied restore fails once; the retry with SDC_ALLOW_CHANGES works.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor %+v", st)
	}
	// Both supplied attempts fail: Windows' database layout.
	d, err = m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.restore(Config{Paths: []PathInfo{{Flags: pathActive}}}); err != nil {
		t.Fatal(err)
	}
	if fs := sys.flags(); fs[len(fs)-1] != SDCApply|SDCUseDatabaseCurrent {
		t.Fatalf("last flags %#x", fs[len(fs)-1])
	}
	d.Close()
}

func TestLingerReuse(t *testing.T) {
	m, sys, drv, phys := rig(t, Options{Linger: 100 * time.Millisecond}, nil)
	d1, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	d1.Close()
	d2, err := m.Create(mode1440) // a reconnect within the linger time
	if err != nil {
		t.Fatal(err)
	}
	if p, u, _ := drv.count(); p != 1 || u != 0 {
		t.Fatalf("reconnect re-plugged: plugs %d unplugs %d", p, u)
	}
	if d2.Info().Name != d1.Info().Name {
		t.Fatal("different display")
	}
	d1.Close() // the old session's handle no longer owns it
	time.Sleep(200 * time.Millisecond)
	if _, ok := sys.state(drv.target); !ok {
		t.Fatal("an owned display was removed")
	}
	d2.Close()
	time.Sleep(300 * time.Millisecond)
	if _, ok := sys.state(drv.target); ok {
		t.Fatal("the display outlived the linger time")
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor %+v", st)
	}
}

func TestDecideReusesOwnDisplay(t *testing.T) {
	m, _, drv, phys := rig(t, Options{Linger: time.Hour}, nil)
	d1, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	virt := d1.Info().Monitor // primary: the monitor a reconnecting session picks
	physMon := platform.Monitor{Name: phys.name, W: 1920, H: 1080, Hz: 60}
	for _, owned := range []bool{true, false} {
		if !owned {
			d1.Close() // lingering
		}
		if use, why := m.Decide(mode1440, &virt); !use || why != "the monitor is the previous session's virtual display" {
			t.Fatalf("owned=%v: decide %v %q", owned, use, why)
		}
		// The physical monitor is still judged on its own.
		if use, _ := m.Decide(Mode{1920, 1080, 60}, &physMon); use {
			t.Fatalf("owned=%v: a matching physical monitor got a virtual display", owned)
		}
	}
	d2, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	if p, u, _ := drv.count(); p != 1 || u != 0 || d2.Info().Name != virt.Name {
		t.Fatalf("not reused: plugs %d unplugs %d, %s", p, u, d2.Info().Name)
	}
	m.Close()
	if use, why := m.Decide(mode1440, &virt); use {
		t.Fatalf("a removed display is still treated as the agent's: %q", why)
	}
}

func TestTakeoverWithOtherMode(t *testing.T) {
	m, sys, drv, _ := rig(t, Options{Linger: time.Hour}, nil)
	d1, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	first := drv.target
	d2, err := m.Create(Mode{Width: 1920, Height: 1080, Hz: 144})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sys.state(first); ok {
		t.Fatal("the first display was not removed")
	}
	if err := d1.Close(); err != nil { // no-op: replaced
		t.Fatal(err)
	}
	if _, ok := sys.state(drv.target); !ok {
		t.Fatal("closing the replaced handle removed the new display")
	}
	m.Close() // agent shutdown: no linger
	if _, ok := sys.state(drv.target); ok {
		t.Fatal("Manager.Close left the display")
	}
	d2.Close()
	if p, u, _ := drv.count(); p != 2 || u != 2 {
		t.Fatalf("plugs %d unplugs %d", p, u)
	}
}

func TestKeepaliveLost(t *testing.T) {
	m, _, drv, _ := rig(t, Options{}, func(d *fakeDriver) { d.every = 2 * time.Millisecond })
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	select {
	case <-d.Lost():
		t.Fatal("lost while the driver answers")
	case <-time.After(30 * time.Millisecond):
	}
	drv.pingErr.Store(true)
	select {
	case <-d.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("not reported lost")
	}
	// A lost display is not reused.
	d2, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if p, _, _ := drv.count(); p != 2 {
		t.Fatalf("plugs %d", p)
	}
}

func TestPersistentDriverRestoresFirst(t *testing.T) {
	m, sys, drv, phys := rig(t, Options{}, func(d *fakeDriver) {
		d.name, d.persistent, d.vddLike = DriverVDD, true, true
	})
	d, err := m.Create(mode1440)
	if err != nil {
		t.Fatal(err)
	}
	if d.Info().Driver != DriverVDD || !d.Info().Monitor.Primary {
		t.Fatalf("info %+v", d.Info())
	}
	if hasFlag(sys.flags(), SDCSaveToDatabase) {
		t.Fatal("a persistent virtual monitor's layout must not be saved (it would be primary after a reboot)")
	}
	n := len(sys.flags())
	d.Close()
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor %+v", st)
	}
	if st, ok := sys.state(drv.target); !ok || st.active {
		t.Fatalf("VDD monitor after restore: %+v connected=%v (stays connected, switched off)", st, ok)
	}
	if len(sys.flags()) <= n {
		t.Fatal("no restore")
	}
	if c := drv.calls; c[len(c)-1] != "unplug" {
		t.Fatalf("calls %v", c)
	}
}

func TestRecoverAfterCrash(t *testing.T) {
	dir := t.TempDir()
	m, sys, drv, phys := rig(t, Options{StateDir: dir}, nil)
	if _, err := m.Create(mode1440); err != nil {
		t.Fatal(err)
	}
	// The agent dies: a new one starts with the same state directory.
	m2 := newManager(Options{StateDir: dir}, sys, []driver{drv})
	m2.poll, m2.departWait = time.Millisecond, time.Second
	if err := m2.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, _, r := drv.count(); r != 1 {
		t.Fatalf("recovers %d", r)
	}
	if _, ok := sys.state(drv.target); ok {
		t.Fatal("virtual monitor left")
	}
	if st, _ := sys.state(phys.t); !st.active || st.x != 0 {
		t.Fatalf("physical monitor %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal: %v", err)
	}
	if err := m2.Recover(); err != nil { // nothing to do
		t.Fatal(err)
	}
	// A corrupt journal is reported and removed.
	os.WriteFile(filepath.Join(dir, journalName), []byte("{"), 0o600)
	if err := m2.Recover(); err == nil {
		t.Fatal("corrupt journal accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal: %v", err)
	}
}

func TestNoDriver(t *testing.T) {
	m, _, _, _ := rig(t, Options{}, func(d *fakeDriver) { d.detectErr = ErrNoDriver })
	if st := m.Detect(); st.Driver != "" || !errors.Is(st.Err, ErrNoDriver) {
		t.Fatalf("status %+v", st)
	}
	if _, err := m.Create(mode1440); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("err %v", err)
	}
	if use, why := m.Decide(mode1440, &platform.Monitor{W: 1920, H: 1080, Hz: 60}); use || why != "no virtual display driver" {
		t.Fatalf("decide %v %q", use, why)
	}
	// A broken driver is reported, not hidden as "not installed".
	m2, _, _, _ := rig(t, Options{}, func(d *fakeDriver) { d.detectErr = errors.New("protocol 1.0 is not compatible") })
	if st := m2.Detect(); st.Driver != "" || !strings.Contains(st.Err.Error(), "sudovda: protocol 1.0") {
		t.Fatalf("status %+v", st)
	}
	if _, err := m.Create(Mode{Width: 100, Height: 100, Hz: 60}); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestPlugFailure(t *testing.T) {
	dir := t.TempDir()
	m, _, _, _ := rig(t, Options{StateDir: dir}, func(d *fakeDriver) { d.plugErr = errors.New("STATUS_TOO_MANY_NODES") })
	if _, err := m.Create(mode1440); err == nil || !strings.Contains(err.Error(), "sudovda: STATUS_TOO_MANY_NODES") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal: %v", err)
	}
}

func TestNewOutsideWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("New uses the real drivers on Windows")
	}
	m := New(Options{Policy: PolicyOn})
	if st := m.Detect(); !errors.Is(st.Err, ErrUnsupported) || st.String() == "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := m.Create(mode1440); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.Close()
}

func TestDecide(t *testing.T) {
	phys := &platform.Monitor{W: 2560, H: 1440, Hz: 60}
	for _, c := range []struct {
		policy string
		driver bool
		req    Mode
		phys   *platform.Monitor
		want   bool
		why    string
	}{
		{PolicyOff, true, mode1440, phys, false, ""},
		{"", true, mode1440, phys, false, ""},
		{PolicyOn, false, mode1440, phys, true, "virtualDisplay is on"},
		{PolicyOn, true, Mode{2560, 1440, 60}, phys, true, "virtualDisplay is on"},
		{PolicyAuto, false, mode1440, phys, false, "no virtual display driver"},
		{PolicyAuto, true, Mode{2560, 1440, 60}, phys, false, "the monitor matches the client"},
		{PolicyAuto, true, Mode{2560, 1440, 30}, phys, false, "the monitor matches the client"},
		{PolicyAuto, true, mode1440, phys, true, "client wants 120 fps, monitor refreshes at 60 Hz"},
		{PolicyAuto, true, Mode{1920, 1080, 60}, phys, true, "client wants 1920x1080, monitor is 2560x1440"},
		{PolicyAuto, true, Mode{0, 0, 60}, phys, false, "the monitor matches the client"},
		{PolicyAuto, true, Mode{0, 0, 60}, nil, true, "no physical monitor to capture"},
		{PolicyAuto, true, Mode{2560, 1440, 144}, &platform.Monitor{W: 2560, H: 1440}, false, "the monitor matches the client"},
	} {
		use, why := Decide(c.policy, c.driver, c.req, c.phys)
		if use != c.want || why != c.why {
			t.Errorf("Decide(%q, %v, %v, %+v) = %v %q, want %v %q", c.policy, c.driver, c.req, c.phys, use, why, c.want, c.why)
		}
	}
}

func TestRequestedMode(t *testing.T) {
	for _, c := range []struct {
		client     proto.ClientInfo
		prefs      proto.Prefs
		def, maxFS int
		want       Mode
	}{
		{proto.ClientInfo{Width: 2560, Height: 1440, Hz: 143.9}, proto.Prefs{FPS: 120}, 60, 240, Mode{2560, 1440, 120}},
		{proto.ClientInfo{Width: 2560, Height: 1440, Hz: 143.9}, proto.Prefs{}, 60, 240, Mode{2560, 1440, 60}},
		{proto.ClientInfo{Width: 2560, Height: 1440}, proto.Prefs{Width: 1920, Height: 1080, FPS: 300}, 60, 240, Mode{1920, 1080, 240}},
		{proto.ClientInfo{Width: 2737, Height: 1541}, proto.Prefs{FPS: 90}, 60, 240, Mode{2736, 1540, 90}},
		{proto.ClientInfo{Width: 1080, Height: 2400}, proto.Prefs{FPS: 60}, 60, 240, Mode{1080, 2400, 60}},
		{proto.ClientInfo{Width: 9000, Height: 300}, proto.Prefs{FPS: 10}, 60, 240, Mode{7680, 360, 24}},
		{proto.ClientInfo{}, proto.Prefs{Width: 1280}, 60, 0, Mode{0, 0, 60}},
		{proto.ClientInfo{}, proto.Prefs{FPS: 1000}, 60, 0, Mode{0, 0, 500}},
	} {
		if got := RequestedMode(c.client, c.prefs, c.def, c.maxFS); got != c.want {
			t.Errorf("RequestedMode(%+v, %+v) = %v, want %v", c.client, c.prefs, got, c.want)
		}
	}
	if got := (Mode{Hz: 120}).Complete(&platform.Monitor{W: 3440, H: 1441}); got != (Mode{3440, 1440, 120}) {
		t.Errorf("Complete = %v", got)
	}
	if got := (Mode{Hz: 60}).Complete(nil); got != (Mode{1920, 1080, 60}) {
		t.Errorf("Complete(nil) = %v", got)
	}
	if got := mode1440.Complete(nil); got != mode1440 {
		t.Errorf("Complete keeps a known size: %v", got)
	}
}

func TestParseMode(t *testing.T) {
	m, err := ParseMode("2560x1440@120")
	if err != nil || m != mode1440 || m.String() != "2560x1440@120" {
		t.Fatalf("%v %v", m, err)
	}
	for _, s := range []string{"", "2560x1440", "2560@120", "axb@c", "100x100@60", "2560x1440@1000"} {
		if _, err := ParseMode(s); err == nil {
			t.Errorf("ParseMode(%q) accepted", s)
		}
	}
	if !ValidPolicy("") || !ValidPolicy(PolicyAuto) || ValidPolicy("yes") || !ValidLayout("") || !ValidLayout(LayoutOnly) || ValidLayout("mirror") {
		t.Fatal("validation")
	}
}

func TestMonitorID(t *testing.T) {
	a, b := newMonitorID("host-1"), newMonitorID("host-1")
	c := newMonitorID("host-2")
	if a != b || a.GUID == c.GUID || a.Serial == c.Serial {
		t.Fatal("monitor identity must be stable per host and differ between hosts")
	}
	if a.GUID[7]>>4 != 8 || a.GUID[8]>>6 != 2 || len(a.Serial) != 12 {
		t.Fatalf("GUID %x serial %q", a.GUID, a.Serial)
	}
}
