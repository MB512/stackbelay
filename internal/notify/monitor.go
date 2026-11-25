package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// The stack monitor (issue #49) reports stacks that degrade, stop or
// vanish outside a job. It alerts on transitions, not on states, so a
// stack that is stopped on purpose stays quiet.

// States the monitor tracks per stack (store.StackHealth.State).
const (
	stateStarted  = "started"
	stateDegraded = "degraded"
	stateStopped  = "stopped" // also "not created"
	stateUnknown  = "unknown" // reading the state failed
	stateMissing  = "missing" // the stack directory is gone
)

// Fixed timing of the monitor (issue #49).
const (
	monitorInterval = time.Minute
	alertAfter      = 3 // bad polls in a row before an alert
	recoverAfter    = 2 // good polls in a row before a recovery
	observeTimeout  = 30 * time.Second
)

// MonitorStore is what the monitor needs from the database (*store.Store).
type MonitorStore interface {
	ListStacks(ctx context.Context) ([]store.Stack, error)
	StackHealths(ctx context.Context) (map[int64]store.StackHealth, error)
	SaveStackHealth(ctx context.Context, h store.StackHealth) error
	LatestFinishedJobs(ctx context.Context) (map[int64]store.Job, error)
	JobsInState(ctx context.Context, state jobs.State) ([]store.Job, error)
	AddStackEvent(ctx context.Context, kind string, stackID int64, detail string, at time.Time) error
}

// StackReader reads a stack's services and containers (*docker.CLI,
// read-only).
type StackReader interface {
	StackOverview(ctx context.Context, dir string) (docker.Overview, error)
}

// Monitor polls the state of every stack and writes stack events into the
// notification outbox. Its view of each stack is in the database, so a
// restart neither repeats nor misses an alert; only the "Docker
// unreachable" state of the whole host is kept in memory.
type Monitor struct {
	Store      MonitorStore
	Docker     StackReader
	StacksRoot string
	Logger     *slog.Logger
	Now        func() time.Time
	// Mutes: stacks an admin is working on with notifications muted are
	// skipped like busy ones while the mute lasts (issue #50); nil: none.
	Mutes *Mutes

	hostBad     int  // polls in a row in which no stack could be read
	hostAlerted bool // the host alert went out
}

func (m *Monitor) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

// Run polls until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		m.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// observation is the state of a stack in one poll.
type observation struct {
	state  string
	detail StackDetail
}

// Step polls every stack once. Stacks with a running job are skipped:
// their state is the job's business.
func (m *Monitor) Step(ctx context.Context) {
	list, err := m.Store.ListStacks(ctx)
	if err != nil {
		m.Logger.Error("monitor: listing stacks", "error", err)
		return
	}
	healths, err := m.Store.StackHealths(ctx)
	if err != nil {
		m.Logger.Error("monitor: loading stack health", "error", err)
		return
	}
	latest, err := m.Store.LatestFinishedJobs(ctx)
	if err != nil {
		m.Logger.Error("monitor: listing jobs", "error", err)
		return
	}
	running, err := m.Store.JobsInState(ctx, jobs.StateRunning)
	if err != nil {
		m.Logger.Error("monitor: listing running jobs", "error", err)
		return
	}
	now := m.now()
	busy := map[int64]bool{}
	for _, j := range running {
		busy[j.StackID] = true
	}
	for _, st := range list {
		if m.Mutes.StackMuted(st.ID, now) {
			// An admin works on it with notifications muted; once the
			// mute ends, a bad state is reported after the usual polls.
			busy[st.ID] = true
		}
	}
	found, err := stacks.Discover(m.StacksRoot)
	if err != nil {
		m.Logger.Error("monitor: reading the stacks directory", "error", err)
		return
	}
	dirs := map[string]string{}
	for _, st := range found {
		dirs[st.Name] = st.Dir
	}

	obs := map[int64]observation{}
	readable := 0
	for _, st := range list {
		if !st.Present || busy[st.ID] {
			continue // vanished before the start, imported and not restored, or busy
		}
		o := m.observe(ctx, dirs, st.Name)
		obs[st.ID] = o
		if o.state != stateUnknown {
			readable++
		}
	}
	if m.hostCheck(ctx, len(obs), readable, now) {
		return // Docker does not answer: one host alert instead of one per stack
	}

	for _, st := range list {
		h, seen := healths[st.ID]
		o, polled := obs[st.ID]
		if !polled {
			if seen && h.Candidate != "" && busy[st.ID] {
				h.Candidate, h.CandidateSince, h.CandidateCount = "", time.Time{}, 0
				m.save(ctx, h)
			}
			continue
		}
		job := latest[st.ID]
		switch {
		case !seen:
			// First seen: the current state is the baseline, whatever it is.
			m.save(ctx, store.StackHealth{StackID: st.ID, State: o.state, Since: now, LastJobID: job.ID})
			continue
		case job.ID != "" && job.ID != h.LastJobID:
			h.LastJobID = job.ID
			if baseline, alerted := afterJob(job); baseline {
				h = store.StackHealth{StackID: st.ID, State: o.state, Since: now, LastJobID: job.ID,
					Alerted: alerted && o.state != stateStarted}
				m.save(ctx, h)
				continue
			}
		}
		next, ev := advance(h, o, now)
		if ev != nil {
			m.emit(ctx, ev.key, st.ID, ev.detail, now)
		}
		if next != h {
			m.save(ctx, next)
		}
	}
}

// hostCheck handles polls in which no stack could be read (at least two
// stacks, so a single broken stack is not taken for Docker being down).
// It reports whether the poll is such a one.
func (m *Monitor) hostCheck(ctx context.Context, polled, readable int, now time.Time) bool {
	if polled >= 2 && readable == 0 {
		m.hostBad++
		if m.hostBad >= alertAfter && !m.hostAlerted {
			m.hostAlerted = true
			m.emit(ctx, KeyStackUnreadable, 0, StackDetail{}, now)
		}
		return true
	}
	if m.hostAlerted && readable > 0 {
		m.emit(ctx, KeyStackRecovered, 0, StackDetail{}, now)
	}
	m.hostBad, m.hostAlerted = 0, false
	return false
}

// observe reads the state of one stack.
func (m *Monitor) observe(ctx context.Context, dirs map[string]string, name string) observation {
	dir, ok := dirs[name]
	if !ok {
		return observation{state: stateMissing}
	}
	octx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	ov, err := m.Docker.StackOverview(octx, dir)
	if err != nil {
		m.Logger.Debug("monitor: reading stack state", "stack", name, "error", err)
		return observation{state: stateUnknown}
	}
	cond, problems := ov.Condition()
	o := observation{detail: StackDetail{Problems: problems, Total: len(ov.Services)}}
	running := map[string]bool{}
	for _, c := range ov.Containers {
		if c.State == "running" {
			running[c.Service] = true
		}
	}
	for _, svc := range ov.Services {
		if running[svc] {
			o.detail.Running++
		}
	}
	switch cond {
	case docker.ConditionStarted:
		o.state = stateStarted
	case docker.ConditionDegraded:
		o.state = stateDegraded
	default: // stopped, not created
		o.state = stateStopped
	}
	return o
}

// afterJob decides how a stack's newest finished job sets the monitor's
// expectation. baseline: the state after the job is the new normal, with
// no message: a stop or down, a restore without start, or a failed job
// that left services down (its own message reported it; alerted makes the
// monitor send the recovery). Other jobs leave the expectation as it was,
// so a stack that does not come back after a successful job is reported.
func afterJob(j store.Job) (baseline, alerted bool) {
	switch {
	case j.State == jobs.StateSucceeded && (j.Type == jobs.TypeStop || j.Type == jobs.TypeDown):
		return true, false
	case j.State == jobs.StateSucceeded && j.Type == jobs.TypeRestore && j.Options.NoStart:
		return true, false
	case j.State == jobs.StateFailed && len(j.Result.ServicesDown) > 0:
		return true, true
	}
	return false, false
}

// stackEvent is a message advance decided to send.
type stackEvent struct {
	key    Key
	detail StackDetail
}

// advance applies one observation to a stack's health: a different state
// becomes the confirmed one after alertAfter polls in a row (recoverAfter
// for "started"), and the confirmation may raise an event. No reminders:
// a stack that stays bad raises one alert.
func advance(h store.StackHealth, o observation, now time.Time) (store.StackHealth, *stackEvent) {
	if o.state == h.State {
		h.Candidate, h.CandidateSince, h.CandidateCount = "", time.Time{}, 0
		return h, nil
	}
	if o.state != h.Candidate {
		h.Candidate, h.CandidateSince, h.CandidateCount = o.state, now, 0
	}
	h.CandidateCount++
	need := alertAfter
	if o.state == stateStarted {
		need = recoverAfter
	}
	if h.CandidateCount < need {
		return h, nil
	}

	prev, prevSince := h.State, h.Since
	h.State, h.Since = o.state, h.CandidateSince
	h.Candidate, h.CandidateSince, h.CandidateCount = "", time.Time{}, 0
	var ev *stackEvent
	alert := func(key Key) {
		if !h.Alerted {
			ev, h.Alerted = &stackEvent{key: key, detail: o.detail}, true
		}
	}
	switch o.state {
	case stateStarted:
		if h.Alerted {
			ev = &stackEvent{key: KeyStackRecovered,
				detail: StackDetail{Was: prev, ForSeconds: int64(h.Since.Sub(prevSince).Seconds())}}
		}
		h.Alerted = false
	case stateDegraded:
		alert(KeyStackDegraded)
	case stateMissing:
		alert(KeyStackMissing)
	case stateStopped:
		if prev == stateStarted {
			alert(KeyStackStopped)
		}
	case stateUnknown:
		if prev == stateStarted {
			alert(KeyStackUnreadable)
		}
	}
	return h, ev
}

func (m *Monitor) emit(ctx context.Context, key Key, stackID int64, d StackDetail, now time.Time) {
	detail, err := json.Marshal(d)
	if err != nil {
		m.Logger.Error("monitor: encoding event", "error", err)
		return
	}
	if err := m.Store.AddStackEvent(ctx, string(key), stackID, string(detail), now); err != nil {
		m.Logger.Error("monitor: writing event", "event", key, "error", err)
	}
}

func (m *Monitor) save(ctx context.Context, h store.StackHealth) {
	if err := m.Store.SaveStackHealth(ctx, h); err != nil {
		m.Logger.Error("monitor: saving stack health", "stack_id", h.StackID, "error", err)
	}
}
