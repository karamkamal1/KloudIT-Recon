package host

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/host/vdisplay"
)

// elevatedState makes the agent elevated for the test, with an ACL check that
// accepts only paths inside admin that exist (as platform.AdminOnly), and the
// given folder of its own.
func elevatedState(t *testing.T, admin, own string) {
	t.Helper()
	elevatedWith(t, admin)
	inAdmin := adminOnly
	adminOnly = func(p string) error {
		if _, err := os.Stat(p); err != nil {
			return err
		}
		return inAdmin(p)
	}
	sd := elevatedStateDir
	t.Cleanup(func() { elevatedStateDir = sd })
	elevatedStateDir = func() (string, error) { return own, nil }
}

// TestAgentFilesDir: an agent that is not elevated writes its files anywhere
// and keeps the restore journal next to host.json; an elevated one only in a
// folder only administrators can change, its own (platform.AgentStateDir),
// never in host.json's folder, which belongs to the user.
func TestAgentFilesDir(t *testing.T) {
	user, admin := t.TempDir(), t.TempDir()
	cfg := &Config{path: filepath.Join(user, "host.json")}
	notElevated(t)
	if err := CheckAgentDir(user); err != nil {
		t.Fatalf("not elevated: %v", err)
	}
	if dir, err := AgentFilesDir(cfg); dir != user || err != nil {
		t.Fatalf("not elevated: %q %v", dir, err)
	}
	elevatedState(t, admin, admin)
	if CheckAgentDir(user) == nil || CheckAgentDir(admin) != nil {
		t.Fatal("elevated: the user's folder accepted or the admin folder refused")
	}
	if dir, err := AgentFilesDir(cfg); dir != admin || err != nil {
		t.Fatalf("elevated: %q %v, want %q", dir, err, admin)
	}
	elevatedStateDir = func() (string, error) { return user, nil } // a folder users may change
	if dir, err := AgentFilesDir(cfg); dir != "" || err == nil {
		t.Fatalf("elevated with a folder users may change: %q %v", dir, err)
	}
}

// TestElevatedRestoreJournal: an elevated agent neither writes its restore
// journal next to host.json nor acts on one there (a program the user runs
// may have put it there, or made it a link): the journal goes to its own
// folder, the startup's recovery and "recon-host vdisplay -restore" read it
// only from there, and without that folder there is no journal, with a
// warning while virtual displays are on.
func TestElevatedRestoreJournal(t *testing.T) {
	newRig := func(t *testing.T) (*vdisplay.Sim, string, string, *Config) {
		sim := vdisplay.NewSim(vdisplay.DriverSudoVDA)
		sim.AddMonitor(1920, 1080, 0, 0, 60)
		user, admin := t.TempDir(), t.TempDir()
		cfg := &Config{VirtualDisplay: "auto", VirtualDisplayLayout: "only", HostID: "h1", path: filepath.Join(user, "host.json")}
		f := newVirtualDisplays
		t.Cleanup(func() { newVirtualDisplays = f })
		newVirtualDisplays = sim.Manager
		return sim, user, admin, cfg
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	journal := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, vdisplay.JournalName))
		return err == nil
	}
	t.Run("in the user's folder", func(t *testing.T) {
		sim, user, admin, cfg := newRig(t)
		// A journal next to host.json (an agent from before, or forged).
		if _, err := sim.Manager(vdisplay.Options{Policy: vdisplay.PolicyOn, Layout: "only", StateDir: user}).Create(vdisplay.Mode{Width: 2560, Height: 1440, Hz: 120}); err != nil {
			t.Fatal(err)
		}
		elevatedState(t, admin, admin)
		if found, err := RestoreVirtualDisplays(cfg, log); found || err != nil {
			t.Fatalf("found %v, err %v", found, err)
		}
		a := &Agent{cfg: cfg, log: log}
		a.setupVirtualDisplays(sim.Manager(a.virtualDisplayOptions()))
		if _, _, rec := sim.Counts(); rec != 0 || !journal(user) {
			t.Fatalf("the elevated agent acted on the journal in the user's folder (recovers %d)", rec)
		}
		if o := a.virtualDisplayOptions(); o.StateDir != admin {
			t.Fatalf("journal folder %q, want the agent's own %q", o.StateDir, admin)
		}
	})
	t.Run("in the agent's folder", func(t *testing.T) {
		sim, _, admin, cfg := newRig(t)
		elevatedState(t, admin, admin)
		if _, err := sim.Manager((&Agent{cfg: cfg, log: log}).virtualDisplayOptions()).Create(vdisplay.Mode{Width: 2560, Height: 1440, Hz: 120}); err != nil {
			t.Fatal(err)
		}
		if !journal(admin) {
			t.Fatal("no journal in the agent's folder")
		}
		if found, err := RestoreVirtualDisplays(cfg, log); !found || err != nil {
			t.Fatalf("found %v, err %v", found, err)
		}
		(&vdRig{sim: sim, dir: admin}).restored(t)
	})
	t.Run("no folder of its own", func(t *testing.T) {
		sim, user, admin, cfg := newRig(t)
		logs := &lockedLog{}
		missing := filepath.Join(admin, "missing")
		elevatedState(t, admin, missing)
		if found, err := RestoreVirtualDisplays(cfg, log); found || err != nil {
			t.Fatalf("no folder: found %v, err %v", found, err)
		}
		a := &Agent{cfg: cfg, log: slog.New(slog.NewTextHandler(logs, nil))}
		if o := a.virtualDisplayOptions(); o.StateDir != "" {
			t.Fatalf("journal folder %q without one of its own", o.StateDir)
		}
		if l := logs.lines("virtual display: no restore journal"); len(l) != 1 {
			t.Fatalf("log %q", l)
		}
		if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the elevated agent created its folder itself")
		}
		// Its folder made by someone else (users may change it): refused.
		elevatedStateDir = func() (string, error) { return user, nil }
		if found, err := RestoreVirtualDisplays(cfg, log); found || err == nil {
			t.Fatalf("a folder users may change: found %v, err %v", found, err)
		}
		if _, _, rec := sim.Counts(); rec != 0 {
			t.Fatalf("recovers %d", rec)
		}
	})
}
