package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/store"
)

// ContainerLister lists the containers of a stack (*docker.CLI: read-only
// "docker compose ps").
type ContainerLister interface {
	StackContainers(ctx context.Context, dir string) ([]docker.Container, error)
}

// pollHeader marks the periodic refresh of the Status tab. Such requests do
// not extend the session (see loadSession): an open tab alone must not keep
// a login alive forever.
const pollHeader = "X-Stackvault-Poll"

// statusTab is the data of the "Status" tab, the home of a stack.
type statusTab struct {
	stackHeader
	CSRF            string
	CanBackup       bool
	CanUpdate       bool
	CanCheck        bool // this installation can check for updates
	HasRepo         bool
	JobsUnavailable string // why jobs cannot be started, or ""
	Live            statusLive
}

// statusLive is the part of the Status tab that refreshes itself: the
// services with the last update check, and the recent jobs. It is also the
// answer to the poll and to "Check now".
type statusLive struct {
	StackName   string
	Containers  []docker.Container
	Unavailable string      // why the containers cannot be listed, or ""
	Jobs        []store.Job // recent jobs, newest first
	// Check is the last update check of the stack, or nil.
	Check *updateResult
	// CheckMessage says why "Check now" could not run (only in the answer
	// to it).
	CheckMessage string
	CanUpdate    bool
}

// ShowUpdates reports whether the services table has an update column: a
// successful update check exists.
func (l statusLive) ShowUpdates() bool { return l.Check != nil && l.Check.Error == "" }

// stackStatus shows the Status tab (GET /stacks/{name}).
func (s *Server) stackStatus(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	page := statusTab{
		stackHeader:     s.newStackHeader(a.User, perm, st, tabStatus),
		CSRF:            a.Session.CSRFToken,
		CanBackup:       auth.Allowed(a.User, perm, auth.ActionBackup),
		CanUpdate:       auth.Allowed(a.User, perm, auth.ActionUpdate),
		CanCheck:        s.Updates != nil,
		JobsUnavailable: s.jobsUnavailable(),
	}
	hasRepo, err := s.hasRepository(r.Context(), st)
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}
	page.HasRepo = hasRepo
	if page.Live, err = s.loadStatusLive(r, st, perm); err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	s.render(w, r, http.StatusOK, "stack_status.html", pageData{Title: st.Name, Data: page})
}

// stackStatusLive answers the periodic refresh of the Status tab with the
// "status-live" fragment.
func (s *Server) stackStatusLive(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	live, err := s.loadStatusLive(r, st, perm)
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	s.renderPartial(w, "stack_status.html", "status-live", live)
}

// loadStatusLive lists the containers and the recent jobs of st for the
// user of r, whose permission on st is perm. A failing container list is
// shown on the page, not returned as an error.
func (s *Server) loadStatusLive(r *http.Request, st store.Stack, perm store.Permission) (statusLive, error) {
	ctx := r.Context()
	live := statusLive{
		StackName: st.Name,
		Check:     s.updateResults.get(st.ID),
		CanUpdate: auth.Allowed(authFrom(ctx).User, perm, auth.ActionUpdate),
	}
	var err error
	if live.Jobs, err = s.Store.ListJobs(ctx, st.ID, recentJobs); err != nil {
		return live, err
	}
	dir, found := s.stackDir(st)
	switch {
	case s.Containers == nil:
		live.Unavailable = "The container list is not available in this installation."
	case !found:
		live.Unavailable = "The stack directory is missing."
	default:
		ctx, cancel := context.WithTimeout(ctx, containerListTimeout)
		defer cancel()
		list, err := s.Containers.StackContainers(ctx, dir)
		if err != nil {
			s.Logger.Warn("listing containers", "stack", st.Name, "error", err)
			live.Unavailable = "Listing the containers failed: " + truncate(err.Error(), 300)
		}
		live.Containers = list
	}
	return live, nil
}

// hasRepository reports whether st has a repository configured.
func (s *Server) hasRepository(ctx context.Context, st store.Stack) (bool, error) {
	_, err := s.Store.RepositoryInfoForStack(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
