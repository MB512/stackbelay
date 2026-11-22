package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// The global log view (issue #21) follows the container logs of several
// stacks in one stream. Each stack is one "docker compose logs" process and
// counts as one stream against the limits, so a view has at most
// maxLogStreamsPerUser stacks.
const (
	maxGlobalLogStacks = maxLogStreamsPerUser
	globalLogTailLines = 100
)

// globalLogsPage is the data of the global log view.
type globalLogsPage struct {
	Stacks   []globalLogStack // stacks with the logs right, by name
	Selected []string         // chosen stacks (?stack=), in the order of Stacks
	Max      int
	Problems []string // per-stack problems, e.g. a failed container list
	Panel    logPanel // only with a selection
}

// globalLogStack is one stack in the selection form.
type globalLogStack struct {
	Name     string
	Present  bool // the stack directory exists
	Selected bool
}

// logStacks returns the stacks whose logs the user may read, by name.
func (s *Server) logStacks(ctx context.Context, user store.User) ([]store.Stack, error) {
	all, err := s.Store.ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	var perms map[int64]store.Permission
	if !user.IsAdmin {
		if perms, err = s.Store.PermissionsForUser(ctx, user.ID); err != nil {
			return nil, err
		}
	}
	var out []store.Stack
	for _, st := range all {
		if auth.Allowed(user, perms[st.ID], auth.ActionLogs) {
			out = append(out, st)
		}
	}
	return out, nil
}

// mayViewSomeLogs reports whether the user has the logs right on any stack
// (for the "Logs" link in the header).
func (s *Server) mayViewSomeLogs(ctx context.Context, user store.User) bool {
	if user.Disabled {
		return false
	}
	if user.IsAdmin {
		return true
	}
	list, err := s.logStacks(ctx, user)
	if err != nil {
		s.Logger.Warn("listing stacks for the header", "error", err)
		return false
	}
	return len(list) > 0
}

// logSelection is the result of checking ?stack= of a global log request.
type logSelection struct {
	allowed  []store.Stack // all stacks with the logs right
	selected []store.Stack // the chosen ones, in the order of allowed
}

// selectLogStacks checks ?stack= of r: every name must be a discovered
// stack the user may read the logs of (else 404, like the stack pages, so
// names are not revealed), at most maxGlobalLogStacks (else 400). On
// failure it has answered (plain text if plain, else a page) and returns
// false.
func (s *Server) selectLogStacks(w http.ResponseWriter, r *http.Request, plain bool) (logSelection, bool) {
	fail := func(status int, msg string) (logSelection, bool) {
		if plain {
			http.Error(w, msg, status)
		} else {
			s.renderError(w, r, status, msg)
		}
		return logSelection{}, false
	}
	a := authFrom(r.Context())
	allowed, err := s.logStacks(r.Context(), a.User)
	if err != nil {
		s.internalError(w, r, "listing stacks", err)
		return logSelection{}, false
	}
	names := r.URL.Query()["stack"]
	if len(names) > maxGlobalLogStacks {
		return fail(http.StatusBadRequest, fmt.Sprintf("At most %d stacks can be shown at once.", maxGlobalLogStacks))
	}
	sel := logSelection{allowed: allowed}
	for _, name := range names {
		if !stacks.ValidName(name) || !slices.ContainsFunc(allowed, func(st store.Stack) bool { return st.Name == name }) {
			return fail(http.StatusNotFound, "Stack not found.")
		}
	}
	for _, st := range allowed {
		if slices.Contains(names, st.Name) {
			sel.selected = append(sel.selected, st)
		}
	}
	return sel, true
}

// globalLogs shows the global log view: a form to choose stacks and,
// with ?stack=a&stack=b, their merged live logs (static/stacklog.js,
// stream from globalLogEvents).
func (s *Server) globalLogs(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.selectLogStacks(w, r, false)
	if !ok {
		return
	}
	page := globalLogsPage{Max: maxGlobalLogStacks}
	for _, st := range sel.allowed {
		_, present := s.stackDir(st)
		chosen := slices.ContainsFunc(sel.selected, func(c store.Stack) bool { return c.ID == st.ID })
		page.Stacks = append(page.Stacks, globalLogStack{Name: st.Name, Present: present, Selected: chosen})
	}
	if len(sel.selected) > 0 {
		// The stream gets the stacks whose directory exists; the others
		// are named above the view.
		var q []string
		page.Panel = logPanel{Tail: globalLogTailLines}
		for _, st := range sel.selected {
			page.Selected = append(page.Selected, st.Name)
			dir, found := s.stackDir(st)
			if !found {
				page.Problems = append(page.Problems, st.Name+": the stack directory is missing.")
				continue
			}
			q = append(q, "stack="+st.Name) // names are discovery names: no escaping needed
			if s.Logs == nil {
				continue
			}
			names, problem := s.listLogContainers(r.Context(), st.Name, dir)
			if problem != "" {
				page.Problems = append(page.Problems, st.Name+": "+problem)
			}
			for _, c := range names {
				page.Panel.Containers = append(page.Panel.Containers, containerLogLine{Stack: st.Name, Container: c}.Key())
			}
		}
		page.Panel.Events = "/logs/events?" + strings.Join(q, "&")
		switch {
		case s.Logs == nil:
			page.Panel.Unavailable = logsUnavailable
		case len(q) == 0:
			page.Panel.Unavailable = "None of the chosen stacks has a stack directory."
		}
	}
	s.render(w, r, http.StatusOK, "logs.html", pageData{Title: "Logs", Data: page})
}

// globalLogEvents streams the merged container logs of the stacks in
// ?stack= (Server-Sent Events, same events as stackLogEvents). Rights are
// checked on every (re)connect; the first connect (without ?since=) is
// audited as stack.logs per stack.
func (s *Server) globalLogEvents(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.selectLogStacks(w, r, true)
	if !ok {
		return
	}
	if len(sel.selected) == 0 {
		http.Error(w, "no stack chosen", http.StatusBadRequest)
		return
	}
	if s.Logs == nil {
		http.Error(w, "logs unavailable", http.StatusServiceUnavailable)
		return
	}
	sources := make([]logSource, 0, len(sel.selected))
	for _, st := range sel.selected {
		dir, found := s.stackDir(st)
		if !found {
			http.Error(w, "logs unavailable: stack directory of "+st.Name+" is missing", http.StatusServiceUnavailable)
			return
		}
		sources = append(sources, logSource{Stack: st.Name, Dir: dir})
	}
	since, ok := parseSince(w, r)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	if !s.logStreams.acquireN(a.User.ID, len(sources), maxLogStreamsPerUser, maxLogStreams) {
		http.Error(w, "too many open log views", http.StatusTooManyRequests)
		return
	}
	defer s.logStreams.releaseN(a.User.ID, len(sources))
	if since.IsZero() {
		for _, st := range sel.selected {
			s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "stack.logs", StackID: st.ID, Detail: "global log view"})
		}
	}
	s.streamLogs(w, r, sources, globalLogTailLines, since)
}
