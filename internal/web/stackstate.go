package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

const (
	// stateCacheTTL is how long a stack's state is reused, so reloads and
	// several users do not multiply docker calls.
	stateCacheTTL = 5 * time.Second
	// maxStateLoads bounds the docker calls for stack states that run at
	// the same time.
	maxStateLoads = 4
)

// stackState is the State column of the stack list (issue #23).
type stackState struct {
	Label  string // "started", "stopped", ...
	Class  string // badge class: "ok", "warn", "err", "run" or ""
	Detail string // tooltip
	JobID  string // busy: the running job
}

// Badge labels of the State column.
var (
	stateStarted    = stackState{Label: "started", Class: "ok"}
	stateStopped    = stackState{Label: "stopped"}
	stateNotCreated = stackState{Label: "not created", Detail: "The stack has no containers; create them once with docker compose up."}
	stateMissing    = stackState{Label: "missing", Class: "warn", Detail: "The stack directory is missing."}
)

// classifyStack derives the state of a stack from its services and
// containers:
//   - started: every service runs (healthy, if it has a healthcheck);
//   - stopped: no container runs;
//   - degraded: some but not all services run, or a container is
//     unhealthy or restarting.
//
// Containers that exited with code 0 while others run are one-shot
// services (init, migrations) and are ignored.
func classifyStack(ov docker.Overview) stackState {
	if len(ov.Containers) == 0 {
		return stateNotCreated
	}
	type service struct{ running, done bool }
	services := map[string]*service{}
	for _, name := range ov.Services {
		services[name] = &service{}
	}
	var problems []string
	anyRunning := false
	for _, c := range ov.Containers {
		svc := services[c.Service]
		if svc == nil { // e.g. a service of an inactive profile
			svc = &service{}
			services[c.Service] = svc
		}
		switch {
		case c.State == "running" && c.Health == "unhealthy":
			problems = append(problems, c.Name+" is unhealthy")
			svc.running, anyRunning = true, true
		case c.State == "running":
			svc.running, anyRunning = true, true
		case c.State == "restarting":
			problems = append(problems, c.Name+" is restarting")
		case c.State == "exited" && c.ExitCode == 0:
			svc.done = true
		}
	}
	if !anyRunning && len(problems) == 0 {
		return stateStopped
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if svc := services[name]; !svc.running && !svc.done {
			problems = append(problems, name+" is not running")
		}
	}
	if len(problems) > 0 {
		return stackState{Label: "degraded", Class: "warn", Detail: strings.Join(problems, "; ")}
	}
	return stateStarted
}

// overviewState turns the result of StackOverview into a state.
func overviewState(ov docker.Overview, err error) stackState {
	switch {
	case errors.Is(err, docker.ErrLegacyCompose):
		return stackState{Label: "legacy (compose v1)", Class: "warn", Detail: truncate(err.Error(), 300)}
	case err != nil:
		return stackState{Label: "unknown", Class: "err", Detail: "Reading the state failed: " + truncate(err.Error(), 300)}
	}
	return classifyStack(ov)
}

// stateCache holds the last state per stack directory and merges
// concurrent loads of the same stack. The zero value is ready to use.
type stateCache struct {
	mu      sync.Mutex
	entries map[string]*stateEntry
	sem     chan struct{} // bounds concurrent loads; created on first use
}

type stateEntry struct {
	done  chan struct{} // closed when state is set
	at    time.Time
	state stackState
}

// get returns the state of dir: a cached one younger than stateCacheTTL,
// the result of a load already running, or a new load. load runs with at
// most maxStateLoads others at the same time.
func (c *stateCache) get(ctx context.Context, dir string, now func() time.Time, load func() stackState) stackState {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*stateEntry{}
		c.sem = make(chan struct{}, maxStateLoads)
	}
	e := c.entries[dir]
	if e != nil {
		select {
		case <-e.done:
			if now().Sub(e.at) >= stateCacheTTL {
				e = nil // expired
			}
		default: // still loading: wait for it below
		}
	}
	if e == nil {
		e = &stateEntry{done: make(chan struct{})}
		c.entries[dir] = e
		c.mu.Unlock()
		// A goroutine (like a Task.Run in C#) loads the state, so a load
		// another request waits for is not cancelled with this request.
		go func() {
			c.sem <- struct{}{} // acquire a slot (blocks if all are taken)
			defer func() { <-c.sem }()
			e.state, e.at = load(), now()
			close(e.done)
		}()
	} else {
		c.mu.Unlock()
	}
	select {
	case <-e.done:
		return e.state
	case <-ctx.Done():
		return stackState{Label: "unknown", Detail: "Cancelled."}
	}
}

// stackStateCell answers the lazy State cell of the stack list
// (GET /stacks/{name}/state, HTMX fragment).
func (s *Server) stackStateCell(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	state, err := s.stackStateOf(r.Context(), st)
	if err != nil {
		s.internalError(w, r, "loading stack state", err)
		return
	}
	s.renderPartial(w, "stacks.html", "stack-state", state)
}

// stackStateOf returns the State column of a stack: missing and busy come
// from the database, the rest from docker (cached).
func (s *Server) stackStateOf(ctx context.Context, st store.Stack) (stackState, error) {
	dir, found := s.stackDir(st)
	if !st.Present || !found {
		return stateMissing, nil
	}
	running, err := s.Store.JobsInState(ctx, jobs.StateRunning)
	if err != nil {
		return stackState{}, err
	}
	for _, j := range running {
		if j.StackID == st.ID {
			return stackState{Label: "busy: " + string(j.Type), Class: "run", JobID: j.ID,
				Detail: fmt.Sprintf("A %s job is running.", j.Type)}, nil
		}
	}
	if s.Containers == nil {
		return stackState{Label: "unknown", Detail: "The container state is not available in this installation."}, nil
	}
	return s.stackStates.get(ctx, dir, s.now, func() stackState {
		lctx, cancel := context.WithTimeout(context.Background(), containerListTimeout)
		defer cancel()
		ov, err := s.Containers.StackOverview(lctx, dir)
		if err != nil {
			s.Logger.Warn("reading stack state", "stack", st.Name, "error", err)
		}
		return overviewState(ov, err)
	}), nil
}
