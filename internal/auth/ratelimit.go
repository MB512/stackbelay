package auth

import (
	"sync"
	"time"
)

// LoginLimiter slows down password guessing per key (the username): after
// a number of free failures, each further failure locks the key for an
// exponentially growing time, capped at a maximum. A successful login
// resets the key.
//
// Limiting per username (not per client IP) is deliberate: behind a
// reverse proxy all clients share one IP, and an IP limit would let one
// attacker lock out everyone. The state is in memory and resets on restart.
type LoginLimiter struct {
	FreeFailures int           // failures before locking starts (default 5)
	BaseDelay    time.Duration // first lock duration (default 1s)
	MaxDelay     time.Duration // cap (default 15m)
	Now          func() time.Time

	mu      sync.Mutex // protects entries; methods are called concurrently by HTTP handlers
	entries map[string]*limitEntry
}

type limitEntry struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

const maxLimiterEntries = 10_000

func (l *LoginLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *LoginLimiter) defaults() (free int, base, maxDelay time.Duration) {
	free, base, maxDelay = l.FreeFailures, l.BaseDelay, l.MaxDelay
	if free <= 0 {
		free = 5
	}
	if base <= 0 {
		base = time.Second
	}
	if maxDelay <= 0 {
		maxDelay = 15 * time.Minute
	}
	return free, base, maxDelay
}

// Allow reports whether a login attempt for key may be checked now, and if
// not, how long the caller has to wait.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	if wait := e.lockedUntil.Sub(l.now()); wait > 0 {
		return false, wait
	}
	return true, 0
}

// Failure records a failed attempt for key.
func (l *LoginLimiter) Failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	free, base, maxDelay := l.defaults()
	now := l.now()
	if l.entries == nil {
		l.entries = map[string]*limitEntry{}
	}
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= maxLimiterEntries {
			l.prune(now, maxDelay)
		}
		e = &limitEntry{}
		l.entries[key] = e
	}
	// Forget old failures after a quiet period.
	if !e.lastFailure.IsZero() && now.Sub(e.lastFailure) > 2*maxDelay {
		e.failures = 0
	}
	e.failures++
	e.lastFailure = now
	if over := e.failures - free; over > 0 {
		delay := maxDelay
		if over <= 30 { // avoid overflowing the shift
			delay = min(base<<(over-1), maxDelay)
		}
		e.lockedUntil = now.Add(delay)
	}
}

// Success resets key after a successful login.
func (l *LoginLimiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// prune drops entries that are no longer relevant, so that guessing many
// different usernames cannot grow the map without bound.
func (l *LoginLimiter) prune(now time.Time, maxDelay time.Duration) {
	for k, e := range l.entries {
		if now.Sub(e.lastFailure) > 2*maxDelay || now.After(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
