package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// supervisorTestEnv tells the test binary, started again as the supervised
// agent, what to do (TestSupervisorChild): "mode:dir".
const supervisorTestEnv = "RECON_SUPERVISOR_TEST"

// TestSupervisorChild is the agent the supervisor tests start: this test
// binary again, running only this test.
func TestSupervisorChild(t *testing.T) {
	spec := os.Getenv(supervisorTestEnv)
	if spec == "" {
		t.Skip("the supervisor tests' child process")
	}
	mode, dir, _ := strings.Cut(spec, ":")
	switch mode {
	case "crash-once": // a panic in a goroutine on the first run, a clean exit on the second
		runs, _ := os.ReadFile(filepath.Join(dir, "runs"))
		_ = os.WriteFile(filepath.Join(dir, "runs"), append(runs, 'x'), 0o600)
		if len(runs) == 0 {
			go func() { panic("test crash in a session goroutine") }()
			select {}
		}
	case "wait": // runs until stopped, and notes a clean stop
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		<-ctx.Done()
		_ = os.WriteFile(filepath.Join(dir, "stopped"), nil, 0o600)
	case "supervise": // a supervisor of a "wait" child, for a test to kill
		s := testSupervisor(dir, "wait", os.Stderr)
		s.run(context.Background())
	}
}

func testSupervisor(dir, mode string, out *os.File) *supervisor {
	s := newSupervisor(os.Args[0], []string{"-test.run=^TestSupervisorChild$"}, out,
		slog.New(slog.NewTextHandler(out, nil)))
	s.env = append(s.env, supervisorTestEnv+"="+mode+":"+dir)
	s.backoff, s.stopWait = 10*time.Millisecond, 2*time.Second
	return s
}

func waitFile(t *testing.T, path string, d time.Duration) []byte {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return b
		}
	}
	t.Fatalf("%s did not appear within %v", path, d)
	return nil
}

// An agent that crashes is started again, and the crash's trace (which the
// Go runtime writes to stderr, which the logon task's agent does not have)
// lands in host.log with the restart.
func TestSupervisorRestartsCrashedAgent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "host.log")
	out := tolerantMulti{appendFile(logPath)}
	s := newSupervisor(os.Args[0], []string{"-test.run=^TestSupervisorChild$"}, out, slog.New(slog.NewTextHandler(out, nil)))
	s.env = append(s.env, supervisorTestEnv+"=crash-once:"+dir)
	s.backoff = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if code := s.run(ctx); code != 0 || ctx.Err() != nil {
		t.Fatalf("run = %d, ctx %v: want 0 once the second run exits cleanly", code, ctx.Err())
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); len(runs) != 2 {
		t.Fatalf("the agent ran %d times, want 2 (started again after the crash)", len(runs))
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"panic: test crash in a session goroutine", "goroutine ",
		`msg="agent exited, starting it again" status="exit status 2"`, `msg="agent supervisor: the agent stopped"`} {
		if !strings.Contains(string(log), want) {
			t.Errorf("host.log lacks %q:\n%s", want, log)
		}
	}
}

// A supervisor that is stopped stops the agent; outside Windows it asks the
// agent to stop (SIGTERM), which then stops cleanly.
func TestSupervisorStopsAgent(t *testing.T) {
	dir := t.TempDir()
	s := testSupervisor(dir, "wait", os.Stderr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- s.run(ctx) }()
	waitFile(t, filepath.Join(dir, "pid"), 30*time.Second)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil && runtime.GOOS != "windows" {
		t.Fatal("the agent was not stopped cleanly:", err)
	}
}

// When the supervisor's process ends, as Stop-ScheduledTask ends it, the agent
// ends with it (Windows: the job object).
func TestSupervisorKilledEndsAgent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows job objects")
	}
	dir := t.TempDir()
	sup := exec.Command(os.Args[0], "-test.run=^TestSupervisorChild$")
	sup.Env = append(os.Environ(), supervisorTestEnv+"=supervise:"+dir)
	sup.Stdout, sup.Stderr = os.Stderr, os.Stderr
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(waitFile(t, filepath.Join(dir, "pid"), 30*time.Second)))
	agent, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _, _ = agent.Wait(); close(exited) }()
	_ = sup.Process.Kill()
	_ = sup.Wait()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = agent.Kill()
		t.Fatal("the agent kept running after its supervisor was killed")
	}
}
