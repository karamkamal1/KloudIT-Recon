package gateway

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry is one security-relevant event.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	User   string    `json:"user,omitempty"`
	IP     string    `json:"ip,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// auditMaxSize is where audit.log is moved to audit.log.1 (replacing the one
// before) and started again: at the gateway's start and while it runs, since
// failed logins from the internet add entries for as long as it runs.
var auditMaxSize int64 = 20 << 20

// Audit appends JSON lines to audit.log and keeps the latest entries in memory.
type Audit struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	size   int64
	recent []AuditEntry
}

func OpenAudit(dir string) (*Audit, error) {
	p := filepath.Join(dir, "audit.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > auditMaxSize {
		_ = os.Rename(p, p+".1")
	}
	a := &Audit{path: p}
	if f, err := os.Open(p); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e AuditEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				a.push(e)
			}
		}
		f.Close()
	}
	if err := a.open(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Audit) open() error {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	a.f, a.size = f, 0
	if fi, err := f.Stat(); err == nil {
		a.size = fi.Size()
	}
	return nil
}

func (a *Audit) push(e AuditEntry) {
	a.recent = append(a.recent, e)
	if len(a.recent) > 500 {
		a.recent = a.recent[len(a.recent)-500:]
	}
}

// Log records an event.
func (a *Audit) Log(event, user, ip, detail string) {
	e := AuditEntry{Time: time.Now().UTC(), Event: event, User: user, IP: ip, Detail: detail}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.push(e)
	if a.f == nil {
		return
	}
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	if a.size+int64(len(b)) > auditMaxSize && a.size > 0 {
		a.f.Close()
		a.f = nil
		_ = os.Rename(a.path, a.path+".1")
		if a.open() != nil {
			return
		}
	}
	n, _ := a.f.Write(b)
	a.size += int64(n)
}

// Recent returns up to n latest entries, newest first.
func (a *Audit) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEntry, 0, n)
	for i := len(a.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a.recent[i])
	}
	return out
}
