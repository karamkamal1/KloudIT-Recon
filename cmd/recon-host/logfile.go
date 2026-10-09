package main

import (
	"os"
	"sync"
)

// logMaxSize is where host.log is moved to host.log.old (replacing the one
// before) and started again: at the agent's start, and while it runs, since
// the logon task's agent runs for days.
var logMaxSize int64 = 20 << 20

// logFile appends to a log file and rotates it past logMaxSize. A slog
// handler writes each record in one Write, so a record is never split.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	next int64 // the size at which to rotate
}

func openLogFile(path string) (*logFile, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMaxSize {
		_ = os.Rename(path, path+".old")
	}
	l := &logFile{path: path, next: logMaxSize}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > l.next && l.size > 0 {
		l.f.Close()
		// A program that holds the file open without delete sharing can
		// make the rename fail on Windows: append to it then, and try again
		// after another tenth of the limit.
		renamed := os.Rename(l.path, l.path+".old") == nil
		if err := l.open(); err != nil {
			return 0, err
		}
		l.next = logMaxSize
		if !renamed {
			l.next = l.size + logMaxSize/10
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
