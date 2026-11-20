package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// UpdateChecker compares a stack's running images with the registry
// without pulling (*docker.CLI: read-only "docker compose config/ps",
// "docker image inspect", "docker buildx imagetools inspect").
type UpdateChecker interface {
	CheckUpdates(ctx context.Context, dir string) ([]docker.ImageCheck, error)
}

// Limits of update checks: each asks the registry once per image.
const (
	updateCheckTimeout  = 2 * time.Minute
	updateChecksPerUser = 1
	updateChecksTotal   = 3
)

// updateResult is the outcome of the last update check of a stack. It is
// kept in memory (lost on restart) and shown to everyone who may view the
// stack: the Status tab and the update page show it until the next check.
type updateResult struct {
	At     time.Time
	Checks []docker.ImageCheck
	Error  string // the check failed; Checks is empty
	// NewImages counts running services with an available update.
	NewImages int
	// Pinned lists running services that run a version pinned by digest
	// (restore, rollback); an update switches them back to their tag.
	Pinned []string
}

// updateResults holds the last update check per stack. The zero value is
// ready to use; it is safe for concurrent use.
type updateResults struct {
	mu sync.Mutex
	m  map[int64]*updateResult
}

func (u *updateResults) get(stackID int64) *updateResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.m[stackID]
}

func (u *updateResults) set(stackID int64, res *updateResult) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[int64]*updateResult{}
	}
	u.m[stackID] = res
}

// newUpdateResult summarizes checks.
func newUpdateResult(at time.Time, checks []docker.ImageCheck) *updateResult {
	res := &updateResult{At: at, Checks: checks}
	for _, c := range checks {
		if c.Running && c.Status == docker.UpdateAvailable {
			res.NewImages++
		}
		if c.Running && c.Pinned {
			res.Pinned = append(res.Pinned, c.Service)
		}
	}
	return res
}

// ForService returns the check of a service, or nil.
func (u *updateResult) ForService(service string) *docker.ImageCheck {
	if u == nil {
		return nil
	}
	for i := range u.Checks {
		if u.Checks[i].Service == service {
			return &u.Checks[i]
		}
	}
	return nil
}

// updatePage is the data of the update page (GET /stacks/{name}/update):
// what an update does and the form to start one. It has no tab; the Status
// tab links to it.
type updatePage struct {
	Stack   store.Stack
	CSRF    string
	HasRepo bool
	// Unavailable says why jobs cannot be started right now.
	Unavailable string
	// Problem is a form error to show above the form.
	Problem string
	// Check is the last update check, or nil.
	Check *updateResult
}

// loadUpdatePage checks the update right and builds the page. On failure
// the response has been written and ok is false.
func (s *Server) loadUpdatePage(w http.ResponseWriter, r *http.Request) (updatePage, store.Stack, bool) {
	st, _, ok := s.loadStack(w, r, auth.ActionUpdate)
	if !ok {
		return updatePage{}, st, false
	}
	hasRepo, err := s.hasRepository(r.Context(), st)
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return updatePage{}, st, false
	}
	return updatePage{
		Stack:       st,
		CSRF:        authFrom(r.Context()).Session.CSRFToken,
		HasRepo:     hasRepo,
		Unavailable: s.jobsUnavailable(),
		Check:       s.updateResults.get(st.ID),
	}, st, true
}

func (s *Server) renderUpdate(w http.ResponseWriter, r *http.Request, status int, page updatePage) {
	s.render(w, r, status, "stack_update.html", pageData{Title: "Update " + page.Stack.Name, Data: page})
}

// stackUpdate shows the update page.
func (s *Server) stackUpdate(w http.ResponseWriter, r *http.Request) {
	page, _, ok := s.loadUpdatePage(w, r)
	if !ok {
		return
	}
	s.renderUpdate(w, r, http.StatusOK, page)
}

// checkUpdates asks the registry whether the stack's images have newer
// versions and keeps the result for the Status tab. It changes nothing (no
// pull), but it is a POST with CSRF token and needs the update right: every
// check sends requests to the registries, which may count them against a
// rate limit. HTMX requests get the Status tab's "status-live" fragment,
// plain posts a redirect to the Status tab.
func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionUpdate)
	if !ok {
		return
	}
	msg, status := s.runUpdateCheck(r, st)
	if !isHTMX(r) {
		if msg != "" {
			s.renderError(w, r, status, msg)
			return
		}
		http.Redirect(w, r, "/stacks/"+url.PathEscape(st.Name), http.StatusSeeOther)
		return
	}
	live, err := s.loadStatusLive(r, st, perm)
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	live.CheckMessage = msg
	s.renderPartial(w, "stack_status.html", "status-live", live)
}

// runUpdateCheck runs the check and stores its result. If the check could
// not run at all, it returns a message and HTTP status for the user and
// leaves the last result in place.
func (s *Server) runUpdateCheck(r *http.Request, st store.Stack) (string, int) {
	dir, found := s.stackDir(st)
	switch {
	case s.Updates == nil:
		return "Update checks are not available in this installation.", http.StatusServiceUnavailable
	case !found:
		return "The stack directory is missing.", http.StatusServiceUnavailable
	}
	user := authFrom(r.Context()).User
	if !s.updateChecks.acquire(user.ID, updateChecksPerUser, updateChecksTotal) {
		return "Another update check is running; try again in a moment.", http.StatusTooManyRequests
	}
	defer s.updateChecks.release(user.ID)

	ctx, cancel := context.WithTimeout(r.Context(), updateCheckTimeout)
	checks, err := s.Updates.CheckUpdates(ctx, dir)
	cancel()
	if err != nil {
		s.Logger.Warn("update check failed", "stack", st.Name, "error", err)
		s.updateResults.set(st.ID, &updateResult{At: s.now(), Error: "The check failed: " + err.Error()})
		return "", http.StatusOK
	}
	res := newUpdateResult(s.now(), checks)
	s.updateResults.set(st.ID, res)
	s.Logger.Info("update check", "stack", st.Name, "user", user.Username, "services", len(checks), "new_images", res.NewImages)
	return "", http.StatusOK
}

// startUpdate queues an update. A stack without repository is only
// updated if the user explicitly confirmed that there is no backup and no
// rollback.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	page, st, ok := s.loadUpdatePage(w, r)
	if !ok {
		return
	}
	var opts store.JobOptions
	if !page.HasRepo {
		if r.PostFormValue("confirm_no_backup") != "on" {
			page.Problem = "Confirm that the stack is updated without backup and without rollback."
			s.renderUpdate(w, r, http.StatusBadRequest, page)
			return
		}
		opts.NoBackup = true
	}
	s.submitJob(w, r, st, store.Job{Type: jobs.TypeUpdate, Options: opts},
		fmt.Sprintf("update, backup: %t", page.HasRepo),
		page.HasRepo)
}
