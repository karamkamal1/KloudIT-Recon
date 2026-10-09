package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// supervisedEnv marks the agent process a supervisor started, which runs the
// agent itself instead of supervising again.
const supervisedEnv = "RECON_SUPERVISED"

// supervisor runs the agent (`recon-host -restart run`, which the logon task
// starts) in a child process, the same program with the same arguments, and
// starts it again when it exits with an error. A panic or a fatal runtime
// error in any of the agent's goroutines ends its process, and Task Scheduler
// does not start a program again that ran and then exited with an error code.
// The child's stdout and stderr go to out: the Go runtime writes a crash's
// trace to stderr, which the background (GUI-subsystem) build otherwise does
// not have.
type supervisor struct {
	exe  string
	args []string
	env  []string
	out  io.Writer
	log  *slog.Logger
	// backoff is the wait before the first restart, doubled after each
	// failure up to maxBackoff; a child that ran for stable resets it.
	backoff, maxBackoff, stable time.Duration
	// stopWait is how long the child has to stop once ctx is done (and, after
	// it exited, how long its output may stay open) before it is killed.
	stopWait time.Duration
}

func newSupervisor(exe string, args []string, out io.Writer, log *slog.Logger) *supervisor {
	return &supervisor{
		exe: exe, args: args, env: append(os.Environ(), supervisedEnv+"=1"), out: out, log: log,
		backoff: time.Second, maxBackoff: time.Minute, stable: 5 * time.Minute, stopWait: 15 * time.Second,
	}
}

// run supervises until ctx is done or the agent exits without an error, and
// returns the supervisor's exit code.
func (s *supervisor) run(ctx context.Context) int {
	job, err := newChildJob()
	if err != nil {
		s.log.Warn("agent supervisor: no job object, so stopping the logon task leaves the agent running", "err", err)
	}
	backoff := s.backoff
	for {
		cmd := exec.CommandContext(ctx, s.exe, s.args...)
		cmd.Env = s.env
		cmd.Stdout, cmd.Stderr = s.out, s.out
		cmd.Cancel = func() error { return stopChild(cmd.Process) }
		cmd.WaitDelay = s.stopWait
		started := time.Now()
		if err := cmd.Start(); err != nil {
			s.log.Error("agent supervisor: cannot start the agent", "err", err, "in", backoff)
		} else {
			if err := job.add(cmd.Process); err != nil {
				s.log.Warn("agent supervisor: the agent is not in the job object, so stopping the logon task leaves it running", "err", err)
			}
			_ = cmd.Wait()
			if ctx.Err() != nil {
				return 0
			}
			if cmd.ProcessState.Success() {
				s.log.Info("agent supervisor: the agent stopped")
				return 0
			}
			if time.Since(started) >= s.stable {
				backoff = s.backoff
			}
			s.log.Error("agent exited, starting it again", "status", cmd.ProcessState.String(),
				"ran", time.Since(started).Round(time.Second), "in", backoff)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, s.maxBackoff)
	}
}

// appendFile appends each Write to the file at its path, opened and closed
// every time: the agent in the child process keeps the log open and rotates
// it by renaming, which a handle held open here would block on Windows.
type appendFile string

func (p appendFile) Write(b []byte) (int, error) {
	f, err := os.OpenFile(string(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}
