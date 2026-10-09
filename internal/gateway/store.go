package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// User is a gateway account.
type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Admin        bool      `json:"admin"`
	TOTPSecret   string    `json:"totpSecret,omitempty"`
	TOTPLast     uint64    `json:"totpLast,omitempty"`
	Created      time.Time `json:"created"`
	LastLogin    time.Time `json:"lastLogin,omitempty"`
	// Recovered is when the offline CLI last reset the password or the 2FA
	// (RecoverUser). A host not connected since then is sent an "end" for
	// the user when it registers (revoke.go).
	Recovered time.Time `json:"recovered,omitzero"`
}

// Host is a registered gaming PC.
type Host struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash"`
	MACs      []string  `json:"macs,omitempty"`
	LastSeen  time.Time `json:"lastSeen,omitempty"`
	LastIP    string    `json:"lastIp,omitempty"`
	OS        string    `json:"os,omitempty"`
	Version   string    `json:"version,omitempty"`
	Encoders  []string  `json:"encoders,omitempty"`
	Created   time.Time `json:"created"`
	// WakeBroadcast overrides the broadcast address used for Wake-on-LAN.
	WakeBroadcast string `json:"wakeBroadcast,omitempty"`
}

// LoginSession is a browser login.
type LoginSession struct {
	TokenHash string    `json:"tokenHash"`
	Username  string    `json:"username"`
	CSRF      string    `json:"csrf"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
	IP        string    `json:"ip"`
	UA        string    `json:"ua"`
}

type state struct {
	Users    map[string]*User         `json:"users"`
	Hosts    map[string]*Host         `json:"hosts"`
	Sessions map[string]*LoginSession `json:"sessions"`
}

// Store persists gateway state to a JSON file (0600).
type Store struct {
	path string
	mu   sync.Mutex
	st   state

	dirty     bool
	saveTimer *time.Timer
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if s.st.Users == nil {
		s.st.Users = map[string]*User{}
	}
	if s.st.Hosts == nil {
		s.st.Hosts = map[string]*Host{}
	}
	if s.st.Sessions == nil {
		s.st.Sessions = map[string]*LoginSession{}
	}
	return s, nil
}

// Update runs f under the lock and persists immediately.
func (s *Store) Update(f func(st *state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := f(&s.st); err != nil {
		return err
	}
	return s.saveLocked()
}

// UpdateLazy runs f under the lock and persists within a few seconds (used
// for frequent, low-value changes such as session last-seen times).
func (s *Store) UpdateLazy(f func(st *state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
	s.dirty = true
	if s.saveTimer == nil {
		s.saveTimer = time.AfterFunc(5*time.Second, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.saveTimer = nil
			if s.dirty {
				_ = s.saveLocked()
			}
		})
	}
}

// View runs f under the lock without saving.
func (s *Store) View(f func(st *state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	// O_EXCL after removing any leftover: never write through a planted symlink.
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keepOwner(f, filepath.Dir(s.path))
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Flush writes pending lazy updates.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		_ = s.saveLocked()
	}
}

func (s *Store) UserCount() int {
	n := 0
	s.View(func(st *state) { n = len(st.Users) })
	return n
}

func (s *Store) GetUser(name string) (User, bool) {
	var u User
	ok := false
	s.View(func(st *state) {
		if p := st.Users[name]; p != nil {
			u, ok = *p, true
		}
	})
	return u, ok
}

func (s *Store) GetHost(id string) (Host, bool) {
	var h Host
	ok := false
	s.View(func(st *state) {
		if p := st.Hosts[id]; p != nil {
			h, ok = *p, true
		}
	})
	return h, ok
}

func (s *Store) Hosts() []Host {
	var out []Host
	s.View(func(st *state) {
		for _, h := range st.Hosts {
			out = append(out, *h)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Users() []User {
	var out []User
	s.View(func(st *state) {
		for _, u := range st.Users {
			out = append(out, *u)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// RecoverUser changes an existing user with f (the offline CLI's account
// recovery: a new password, 2FA reset) and in the same save signs the user
// out everywhere and records the recovery (User.Recovered). It returns how
// many login sessions it ended.
func (s *Store) RecoverUser(name string, f func(u *User)) (int, error) {
	n := 0
	err := s.Update(func(st *state) error {
		u := st.Users[name]
		if u == nil {
			return fmt.Errorf("no user %s", name)
		}
		f(u)
		u.Recovered = time.Now().UTC()
		for k, x := range st.Sessions {
			if x.Username == name {
				delete(st.Sessions, k)
				n++
			}
		}
		return nil
	})
	return n, err
}

// UpdateUser creates or modifies a user (used by the offline CLI).
func (s *Store) UpdateUser(name string, f func(u *User, exists bool) error) error {
	return s.Update(func(st *state) error {
		u := st.Users[name]
		exists := u != nil
		if !exists {
			u = &User{Username: name}
		}
		if err := f(u, exists); err != nil {
			return err
		}
		st.Users[name] = u
		return nil
	})
}
