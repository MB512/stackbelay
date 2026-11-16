package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/stacks"
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

// updateTab is the data of the "Update" tab.
type updateTab struct {
	stackHeader
	CSRF    string
	HasRepo bool
	// Unavailable says why jobs cannot be started right now.
	Unavailable string
	// Problem is a form error to show above the form.
	Problem string

	// Update check (after "Check for updates").
	Checked    bool
	CheckedAt  time.Time
	CheckError string
	Checks     []docker.ImageCheck
	// NewImages counts running services with an available update.
	NewImages int
	// Pinned lists running services that run a version pinned by digest
	// (restore, rollback); an update switches them back to their tag.
	Pinned []string
	// CanCheck is false if this installation cannot check (no checker).
	CanCheck bool
}

// loadUpdateTab checks the update right and builds the tab. On failure the
// response has been written and ok is false.
func (s *Server) loadUpdateTab(w http.ResponseWriter, r *http.Request) (updateTab, store.Stack, bool) {
	st, perm, ok := s.loadStack(w, r, auth.ActionUpdate)
	if !ok {
		return updateTab{}, st, false
	}
	_, err := s.Store.RepositoryInfoForStack(r.Context(), st.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "loading repository", err)
		return updateTab{}, st, false
	}
	a := authFrom(r.Context())
	return updateTab{
		stackHeader: s.newStackHeader(a.User, perm, st, tabUpdate),
		CSRF:        a.Session.CSRFToken,
		HasRepo:     err == nil,
		Unavailable: s.jobsUnavailable(),
		CanCheck:    s.Updates != nil,
	}, st, true
}

func (s *Server) renderUpdate(w http.ResponseWriter, r *http.Request, status int, page updateTab) {
	s.render(w, r, status, "stack_update.html", pageData{Title: "Update " + page.Stack.Name, Data: page})
}

// stackUpdate shows the "Update" tab: what an update does, the update
// check and the form to start one.
func (s *Server) stackUpdate(w http.ResponseWriter, r *http.Request) {
	page, _, ok := s.loadUpdateTab(w, r)
	if !ok {
		return
	}
	s.renderUpdate(w, r, http.StatusOK, page)
}

// checkUpdates asks the registry whether the stack's images have newer
// versions and shows the result in the tab. It changes nothing (no pull),
// but it is a POST with CSRF token: every check sends requests to the
// registries, which may count them against a rate limit.
func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	page, st, ok := s.loadUpdateTab(w, r)
	if !ok {
		return
	}
	page.Checked, page.CheckedAt = true, s.now()
	found, err := stacks.Lookup(s.StacksRoot, st.Name)
	switch {
	case s.Updates == nil:
		page.CheckError = "Update checks are not available in this installation."
	case s.StacksRoot == "" || err != nil:
		page.CheckError = "The stack directory is missing."
	}
	if page.CheckError != "" {
		s.renderUpdate(w, r, http.StatusServiceUnavailable, page)
		return
	}
	user := authFrom(r.Context()).User
	if !s.updateChecks.acquire(user.ID, updateChecksPerUser, updateChecksTotal) {
		page.CheckError = "Another update check is running; try again in a moment."
		s.renderUpdate(w, r, http.StatusTooManyRequests, page)
		return
	}
	defer s.updateChecks.release(user.ID)

	ctx, cancel := context.WithTimeout(r.Context(), updateCheckTimeout)
	checks, err := s.Updates.CheckUpdates(ctx, found.Dir)
	cancel()
	if err != nil {
		s.Logger.Warn("update check failed", "stack", st.Name, "error", err)
		page.CheckError = "The check failed: " + err.Error()
		s.renderUpdate(w, r, http.StatusOK, page)
		return
	}
	page.Checks = checks
	for _, c := range checks {
		if c.Running && c.Status == docker.UpdateAvailable {
			page.NewImages++
		}
		if c.Running && c.Pinned {
			page.Pinned = append(page.Pinned, c.Service)
		}
	}
	s.Logger.Info("update check", "stack", st.Name, "user", user.Username, "services", len(checks), "new_images", page.NewImages)
	s.renderUpdate(w, r, http.StatusOK, page)
}

// startUpdate queues an update. A stack without repository is only
// updated if the user explicitly confirmed that there is no backup and no
// rollback.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	page, st, ok := s.loadUpdateTab(w, r)
	if !ok {
		return
	}
	opts := store.JobOptions{OnlyIfNew: r.PostFormValue("only_if_new") == "on"}
	if page.HasRepo {
		opts.RollbackInPlace = r.PostFormValue("rollback") == "in-place"
		opts.NoPreStop = r.PostFormValue("pre_stop") != "on"
	} else {
		if r.PostFormValue("confirm_no_backup") != "on" {
			page.Problem = "Confirm that the stack is updated without backup and without rollback."
			s.renderUpdate(w, r, http.StatusBadRequest, page)
			return
		}
		opts.NoBackup = true
	}
	s.submitJob(w, r, st, store.Job{Type: jobs.TypeUpdate, Options: opts},
		fmt.Sprintf("update, backup: %t, only if new: %t, pre-stop snapshot: %t, rollback in place: %t",
			page.HasRepo, opts.OnlyIfNew, page.HasRepo && !opts.NoPreStop, opts.RollbackInPlace),
		page.HasRepo)
}
