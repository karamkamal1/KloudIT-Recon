package gateway

import (
	"errors"
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Revoking a user's access (the user is deleted, or their password changes,
// which signs out every other login session) also ends the streams they have
// open: signing out only stops new tickets, and a stream that is already up
// does not depend on the login session.
//
// The gateway ends what runs through it (splice relays, UDP relay
// allocations) and sends every online host an "end" for the user, with the
// host tickets it issued them in the last minute: the host ends the user's
// session with a bye and refuses those tickets and any session the gateway
// authorised before the end. A host that is offline at the time and comes
// back streaming for a deleted user gets the end then (handleHostControl).

// userStreams tracks the streams users have open through the gateway.
type userStreams struct {
	mu      sync.Mutex
	next    int
	open    map[string]map[int]func() // user -> stream -> end it
	revoked map[string]time.Time      // user -> when their access was last revoked
}

var errRevoked = errors.New("the user's access was revoked")

// userRemoved is what a deleted user's clients show.
const userRemoved = "Your account was removed from this gateway"

// revokeKeep is how long a revocation refuses streams authorised before it: a
// ticket lives 60 s, and opening the host's side of a stream takes 10 s at
// most after that.
const revokeKeep = 2 * time.Minute

// add registers end, which ends a stream of user that a ticket issued at
// issued authorised. It returns the function that unregisters it, or ok false
// (registering nothing) when the user's access was revoked since issued.
func (u *userStreams) add(user string, issued time.Time, end func()) (remove func(), ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if t, ok := u.revoked[user]; ok && !issued.After(t) {
		return nil, false
	}
	if u.open == nil {
		u.open = map[string]map[int]func(){}
	}
	if u.open[user] == nil {
		u.open[user] = map[int]func(){}
	}
	u.next++
	id := u.next
	u.open[user][id] = end
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		delete(u.open[user], id)
		if len(u.open[user]) == 0 {
			delete(u.open, user)
		}
	}, true
}

// revoke records that user's access was revoked now and returns the end
// functions of their open streams.
func (u *userStreams) revoke(user string) []func() {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if u.revoked == nil {
		u.revoked = map[string]time.Time{}
	}
	for k, t := range u.revoked {
		if now.Sub(t) > revokeKeep {
			delete(u.revoked, k)
		}
	}
	u.revoked[user] = now
	var ends []func()
	for _, end := range u.open[user] {
		ends = append(ends, end)
	}
	return ends
}

// issuedTicket is a host ticket the gateway signed (direct path or UDP relay).
type issuedTicket struct {
	user, nonce string
	exp         int64 // Unix seconds
}

// signHostTicket signs t with the host's direct key and remembers its nonce
// until it expires, so that revoking t.User can make the host refuse it.
func (hc *hostConn) signHostTicket(t proto.DirectTicket) (string, error) {
	tok, err := auth.SignTicket(hc.directKey, t)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	hc.mu.Lock()
	kept := hc.tickets[:0]
	for _, it := range hc.tickets {
		if it.exp >= now {
			kept = append(kept, it)
		}
	}
	hc.tickets = append(kept, issuedTicket{user: t.User, nonce: t.Nonce, exp: t.Exp})
	hc.mu.Unlock()
	return tok, nil
}

// endUser sends the host an "end" for user with the tickets issued to them
// that have not expired.
func (hc *hostConn) endUser(user, reason string) error {
	now := time.Now().Unix()
	tickets := map[string]int64{}
	hc.mu.Lock()
	for _, it := range hc.tickets {
		if it.user == user && it.exp >= now {
			tickets[it.nonce] = it.exp
		}
	}
	hc.mu.Unlock()
	return hc.send(proto.TunnelMsg{T: "end", User: user, Detail: reason, Tickets: tickets})
}

// revokeStreams ends user's streams and the tickets that could open new ones
// (see the top of this file). reason is what their clients show.
func (s *Server) revokeStreams(user, reason string) {
	ends := s.streams.revoke(user)
	s.ticketMu.Lock()
	for k, t := range s.tickets {
		if t.user == user {
			delete(s.tickets, k)
		}
	}
	s.ticketMu.Unlock()
	s.hosts.mu.Lock()
	hosts := make([]*hostConn, 0, len(s.hosts.hosts))
	for _, hc := range s.hosts.hosts {
		hosts = append(hosts, hc)
	}
	s.hosts.mu.Unlock()
	for _, hc := range hosts {
		go func() {
			if err := hc.endUser(user, reason); err != nil {
				s.log.Warn("could not tell a host to end a revoked user's session", "host", hc.id, "user", user, "err", err)
			}
		}()
	}
	if len(ends) == 0 {
		return
	}
	s.log.Info("ending a revoked user's relayed streams", "user", user, "streams", len(ends))
	// The host's bye (sent on its end) goes through these relays: give it
	// a moment to reach the client, which then does not reconnect. An older
	// host sends none, and the relays end the stream regardless.
	time.AfterFunc(time.Second, func() {
		for _, end := range ends {
			end()
		}
	})
}
