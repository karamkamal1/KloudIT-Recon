package gateway

import (
	"net/netip"
	"sync"
	"time"
)

// rateKey is the client part of the per-client limits' keys (request rates,
// lockouts): an IPv4 address, or the /64 an IPv6 address is in. A /64 is
// what one subscriber (a home, a VPS) gets, and it holds more addresses than
// anyone needs, so a key per IPv6 address would give one client as many
// buckets and lockouts as it cares to use.
func rateKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// limiter is a per-key token bucket with lazy cleanup.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	ts     time.Time
}

func newLimiter(perMinute float64, burst int) *limiter {
	return &limiter{rate: perMinute / 60, burst: float64(burst), buckets: map[string]*bucket{}}
}

// Allow consumes one token for key.
func (l *limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.ts) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, ts: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.ts).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.ts = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// lockout tracks consecutive failures per account with exponential back-off.
// Its keys come from unauthenticated requests, so the map is bounded: a key
// that is not locked and has seen no failure for lockoutForget is forgotten,
// and at most max keys are kept.
type lockout struct {
	mu    sync.Mutex
	fails map[string]*failState
	max   int
	swept time.Time
}

type failState struct {
	count int
	until time.Time
	last  time.Time // the latest failure
}

// lockoutForget is how long a key that is not locked keeps its failure count
// after its latest failure (well past the 1 h longest lock, so waiting out a
// lock does not reset the back-off).
const lockoutForget = 24 * time.Hour

func newLockout() *lockout { return &lockout{fails: map[string]*failState{}, max: 10000} }

// Locked reports whether key is currently locked and for how long.
func (l *lockout) Locked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	if f == nil {
		return false, 0
	}
	if d := time.Until(f.until); d > 0 {
		return true, d
	}
	return false, 0
}

// sweep forgets the keys that are not locked and have had no failure for
// lockoutForget, or, with all, every key that is not locked.
func (l *lockout) sweep(now time.Time, all bool) {
	for k, f := range l.fails {
		if now.After(f.until) && (all || now.Sub(f.last) > lockoutForget) {
			delete(l.fails, k)
		}
	}
	l.swept = now
}

// Fail records a failure; after 5 failures the account locks for 1 minute,
// doubling up to 1 hour. It returns how long the key is now locked (0: not).
// A new key that finds the map full of locked keys is not recorded: the
// per-client rate limit still applies to it.
func (l *lockout) Fail(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.swept) > 10*time.Minute {
		l.sweep(now, false)
	}
	f := l.fails[key]
	if f == nil {
		if len(l.fails) >= l.max {
			l.sweep(now, true)
			if len(l.fails) >= l.max {
				return 0
			}
		}
		f = &failState{}
		l.fails[key] = f
	}
	f.count++
	f.last = now
	if f.count >= 5 {
		d := time.Minute << uint(min(f.count-5, 6))
		if d > time.Hour {
			d = time.Hour
		}
		f.until = now.Add(d)
		return d
	}
	return 0
}

func (l *lockout) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
