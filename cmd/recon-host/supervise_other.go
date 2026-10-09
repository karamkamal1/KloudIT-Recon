//go:build !windows

package main

import (
	"os"
	"syscall"
)

// childJob has nothing to do outside Windows: a stopped supervisor sends the
// child SIGTERM (stopChild).
type childJob struct{}

func newChildJob() (childJob, error)   { return childJob{}, nil }
func (childJob) add(*os.Process) error { return nil }
func stopChild(p *os.Process) error    { return p.Signal(syscall.SIGTERM) }
