package notify

import (
	"sync"
	"time"
)

// MuteDuration is how long an admin's mute lasts after the admin's last
// page request (issue #50).
const MuteDuration = 5 * time.Minute

// Mutes holds the temporary notification mutes of admins (issue #50): an
// admin doing manual work mutes the messages of the jobs they start. A
// mute ends MuteDuration after the admin's last page request, so a
// forgotten one switches itself off. Mutes live in memory only: a restart
// of the manager ends them all (the safe direction).
//
// The web server sets and extends mutes and marks the jobs started while
// muted (store.JobOptions.NotifyMuted); the monitor asks StackMuted. All
// methods are safe for concurrent use (a sync.Mutex, like lock in C#).
// The zero value is ready to use.
type Mutes struct {
	mu      sync.Mutex
	until   map[int64]time.Time // user id -> end of the mute
	touched map[int64]int64     // stack id -> admin whose muted job touched it
}

// Mute starts or resets the mute of user.
func (m *Mutes) Mute(user int64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.until == nil {
		m.until = map[int64]time.Time{}
	}
	m.until[user] = now.Add(MuteDuration)
}

// Unmute ends the mute of user.
func (m *Mutes) Unmute(user int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.until, user)
	for stack, u := range m.touched {
		if u == user {
			delete(m.touched, stack)
		}
	}
}

// Extend resets an active mute of user (a page request); it does nothing
// if user is not muted.
func (m *Mutes) Extend(user int64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeLocked(user, now) {
		m.until[user] = now.Add(MuteDuration)
	}
}

// Until returns the end of user's mute; zero if not muted.
func (m *Mutes) Until(user int64, now time.Time) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeLocked(user, now) {
		return time.Time{}
	}
	return m.until[user]
}

// Touch records that a muted job of user changes stack (0 = a host job).
func (m *Mutes) Touch(user, stack int64) {
	if stack == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.touched == nil {
		m.touched = map[int64]int64{}
	}
	m.touched[stack] = user
}

// StackMuted reports whether the monitor keeps quiet about stack: a muted
// job touched it and that admin's mute is still active.
func (m *Mutes) StackMuted(stack int64, now time.Time) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.touched[stack]
	if !ok {
		return false
	}
	if !m.activeLocked(user, now) {
		delete(m.touched, stack)
		return false
	}
	return true
}

// activeLocked reports whether user is muted at now; m.mu must be held.
// An expired mute is removed.
func (m *Mutes) activeLocked(user int64, now time.Time) bool {
	until, ok := m.until[user]
	if ok && !now.Before(until) {
		delete(m.until, user)
		return false
	}
	return ok
}
