package gateway

import (
	"sync"
	"time"
)

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
type lockout struct {
	mu    sync.Mutex
	fails map[string]*failState
}

type failState struct {
	count int
	until time.Time
}

func newLockout() *lockout { return &lockout{fails: map[string]*failState{}} }

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

// Fail records a failure; after 5 failures the account locks for 1 minute,
// doubling up to 1 hour.
func (l *lockout) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	if f == nil {
		f = &failState{}
		l.fails[key] = f
	}
	f.count++
	if f.count >= 5 {
		d := time.Minute << uint(min(f.count-5, 6))
		if d > time.Hour {
			d = time.Hour
		}
		f.until = time.Now().Add(d)
	}
}

func (l *lockout) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
