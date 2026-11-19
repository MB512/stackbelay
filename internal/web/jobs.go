package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/dispatch"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
	"github.com/MB512/stackvault/internal/store"
)

// JobQueue queues and cancels jobs (*dispatch.Dispatcher).
type JobQueue interface {
	Submit(ctx context.Context, j store.Job) (store.Job, error)
	Cancel(ctx context.Context, id string) error
}

// recentJobs is the number of jobs shown on the Backups tab.
const recentJobs = 10

// jobsUnavailable returns why jobs cannot be started, or "".
func (s *Server) jobsUnavailable() string {
	if s.Jobs != nil {
		return ""
	}
	if s.JobsDisabledReason != "" {
		return s.JobsDisabledReason
	}
	return "Backups and restores are not available."
}

// startBackup queues a manual backup of a stack.
func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionBackup)
	if !ok {
		return
	}
	s.submitJob(w, r, st, store.Job{Type: jobs.TypeBackup}, "backup", true)
}

// submitJob checks the preconditions shared by all job types, queues the
// job and redirects to its page. job carries the type, options and (for
// compose-apply) the new compose file; stack, trigger and requester are set
// here. needRepo: the job needs the stack's repository (all but a change
// confirmed to run without backup).
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request, st store.Stack, job store.Job, detail string, needRepo bool) {
	ctx := r.Context()
	if reason := s.jobsUnavailable(); reason != "" {
		s.renderError(w, r, http.StatusServiceUnavailable, reason)
		return
	}
	if !st.Present && !job.Options.Import {
		s.renderError(w, r, http.StatusConflict, "The stack directory is missing.")
		return
	}
	if needRepo {
		if _, err := s.Store.RepositoryInfoForStack(ctx, st.ID); errors.Is(err, store.ErrNotFound) {
			s.renderError(w, r, http.StatusConflict, "This stack has no repository.")
			return
		} else if err != nil {
			s.internalError(w, r, "loading repository", err)
			return
		}
	}
	a := authFrom(ctx)
	job.StackID, job.Trigger, job.RequestedBy = st.ID, store.TriggerManual, a.User.ID
	j, err := s.Jobs.Submit(ctx, job)
	if errors.Is(err, store.ErrJobQueued) {
		s.renderError(w, r, http.StatusConflict, fmt.Sprintf("A %s of this stack is already waiting to run.", job.Type))
		return
	}
	if err != nil {
		s.internalError(w, r, "queueing job", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "job.create", StackID: st.ID,
		Detail: fmt.Sprintf("job %s: %s", j.ID, detail)})
	http.Redirect(w, r, jobURL(j.ID), http.StatusSeeOther)
}

// restorePage is the data of the restore confirmation page.
type restorePage struct {
	Stack    store.Stack
	Snapshot store.CachedSnapshot
	// Import: the stack directory does not exist (a stack imported from
	// another host, or one whose directory vanished): the restore creates
	// it, with nothing to stop, protect or switch over.
	Import bool
	// Incomplete: the last restore into the stack directory did not
	// complete; this one continues it.
	Incomplete bool
	// PreStop: the snapshot was taken while the stack ran (admins only).
	PreStop bool
	// ForeignHost: the snapshot is from another host (the stack's source
	// host); host-specific settings may not fit this host.
	ForeignHost bool
	ThisHost    string
}

// restorableSnapshot returns the cached snapshot id of the stack, if it
// exists in the cache, belongs to this stack and host (a repository may be
// shared) and was not removed from the repository. The worker
// verifies it against the repository again before restoring.
func (s *Server) restorableSnapshot(ctx context.Context, st store.Stack, id string) (store.CachedSnapshot, bool, error) {
	if !restic.SnapshotIDPattern.MatchString(id) {
		return store.CachedSnapshot{}, false, nil
	}
	repo, err := s.Store.RepositoryInfoForStack(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.CachedSnapshot{}, false, nil
	}
	if err != nil {
		return store.CachedSnapshot{}, false, err
	}
	snaps, err := s.Store.StackSnapshots(ctx, repo.ID, st.Name, s.snapshotHost(st))
	if err != nil {
		return store.CachedSnapshot{}, false, err
	}
	for _, sn := range snaps {
		if sn.ID == id && !sn.Missing {
			return sn, true, nil
		}
	}
	return store.CachedSnapshot{}, false, nil
}

// restoreForm asks for confirmation and options before a restore.
func (s *Server) restoreForm(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadRestoreStack(w, r)
	if !ok {
		return
	}
	sn, found, err := s.restorableSnapshot(r.Context(), st, r.URL.Query().Get("snapshot"))
	if err != nil {
		s.internalError(w, r, "loading snapshot", err)
		return
	}
	if !found {
		s.renderError(w, r, http.StatusNotFound, "Snapshot not found. Refresh the snapshot list and try again.")
		return
	}
	if !s.mayRestoreSnapshot(w, r, sn) {
		return
	}
	s.render(w, r, http.StatusOK, "restore.html", pageData{Title: "Restore " + st.Name,
		Error: s.jobsUnavailable(), Data: restorePage{Stack: st, Snapshot: sn, Import: !st.Present,
			Incomplete:  stacks.RestoreIncomplete(filepath.Join(s.StacksRoot, st.Name)),
			PreStop:     isPreStop(sn),
			ForeignHost: s.foreignSource(st), ThisHost: s.Hostname}})
}

// mayRestoreSnapshot refuses a pre-stop snapshot to non-admins (issue
// #35): it was taken while the stack ran and is not consistent. On
// refusal the response has been written.
func (s *Server) mayRestoreSnapshot(w http.ResponseWriter, r *http.Request, sn store.CachedSnapshot) bool {
	if isPreStop(sn) && !authFrom(r.Context()).User.IsAdmin {
		s.renderError(w, r, http.StatusForbidden, "Pre-stop snapshots are not consistent: only admins may restore them.")
		return false
	}
	return true
}

// loadRestoreStack is loadStack for the restore right; another host's
// snapshots (the stack's source host) may only be restored by admins.
func (s *Server) loadRestoreStack(w http.ResponseWriter, r *http.Request) (store.Stack, store.Permission, bool) {
	st, perm, ok := s.loadStack(w, r, auth.ActionRestore)
	if ok && !s.mayRestore(authFrom(r.Context()).User, perm, st) {
		s.renderError(w, r, http.StatusForbidden, "Only admins may restore snapshots of another host.")
		return st, perm, false
	}
	return st, perm, ok
}

// parseDownloadLimit reads the "limit download" field (MiB/s, empty or 0 =
// no limit) and returns KiB/s for restic.
func parseDownloadLimit(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n*1024 > restic.MaxLimitKiB {
		return 0, fmt.Errorf("invalid download limit %q (MiB/s, a whole number)", truncate(v, 20))
	}
	return n * 1024, nil
}

// startRestore queues a restore. Only admins may skip the safety snapshot.
func (s *Server) startRestore(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadRestoreStack(w, r)
	if !ok {
		return
	}
	sn, found, err := s.restorableSnapshot(r.Context(), st, r.PostFormValue("snapshot_id"))
	if err != nil {
		s.internalError(w, r, "loading snapshot", err)
		return
	}
	if !found {
		s.renderError(w, r, http.StatusBadRequest, "Snapshot not found. Refresh the snapshot list and try again.")
		return
	}
	if !s.mayRestoreSnapshot(w, r, sn) {
		return
	}
	limit, err := parseDownloadLimit(r.PostFormValue("limit_download"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	opts := store.JobOptions{
		SnapshotID:       sn.ID,
		NoStart:          r.PostFormValue("start") != "on",
		PinImages:        r.PostFormValue("images") == "pinned",
		LimitDownloadKiB: limit,
	}
	if !st.Present {
		// No directory: the restore creates it (import from another host,
		// or a stack whose directory vanished).
		if _, err := stacks.ImportTarget(s.StacksRoot, st.Name); err != nil {
			s.renderError(w, r, http.StatusConflict, "The stack directory cannot be created: "+truncate(err.Error(), 200))
			return
		}
		opts.Import = true
	}
	s.submitJob(w, r, st, store.Job{Type: jobs.TypeRestore, Options: opts},
		fmt.Sprintf("restore of snapshot %s (host %s), import: %t, safety snapshot: %t, start: %t, pinned images: %t, download limit: %d KiB/s",
			sn.ShortID, sn.Hostname, opts.Import, !opts.Import, !opts.NoStart, opts.PinImages,
			opts.LimitDownloadKiB), true)
}

// loadJob resolves the job in the URL and checks that the user may view
// its stack. Jobs of stacks the user may not see answer 404.
func (s *Server) loadJob(w http.ResponseWriter, r *http.Request) (store.Job, store.Permission, bool) {
	id := chi.URLParam(r, "id")
	if !jobs.ValidJobID(id) {
		s.renderError(w, r, http.StatusNotFound, "Job not found.")
		return store.Job{}, store.Permission{}, false
	}
	j, err := s.Store.JobByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Job not found.")
		return store.Job{}, store.Permission{}, false
	}
	if err != nil {
		s.internalError(w, r, "loading job", err)
		return store.Job{}, store.Permission{}, false
	}
	a := authFrom(r.Context())
	// Host jobs belong to no stack: admins only (like the Host tab).
	if j.Type.Host() && (!a.User.IsAdmin || a.User.Disabled) {
		s.renderError(w, r, http.StatusNotFound, "Job not found.")
		return store.Job{}, store.Permission{}, false
	}
	perm, err := s.Store.Permission(r.Context(), a.User.ID, j.StackID)
	if err != nil {
		s.internalError(w, r, "loading permission", err)
		return store.Job{}, store.Permission{}, false
	}
	if !auth.Allowed(a.User, perm, auth.ActionView) {
		s.renderError(w, r, http.StatusNotFound, "Job not found.")
		return store.Job{}, store.Permission{}, false
	}
	return j, perm, true
}

// mayRunJob reports whether user may start (and cancel) a job of type t on
// a stack where the user has perm. Applying a compose file is admin-only:
// it is equivalent to root on the host. Host jobs (cleanup) are admin-only.
func mayRunJob(user store.User, perm store.Permission, t jobs.Type) bool {
	if t == jobs.TypeComposeApply || t.Host() {
		return user.IsAdmin && !user.Disabled
	}
	return auth.Allowed(user, perm, jobAction(t))
}

// jobAction is the right needed to start (and cancel) a job of this type
// (compose-apply: see mayRunJob).
func jobAction(t jobs.Type) auth.Action {
	switch t {
	case jobs.TypeRestore:
		return auth.ActionRestore
	case jobs.TypeUpdate:
		return auth.ActionUpdate
	case jobs.TypeStop, jobs.TypeStart, jobs.TypeRestart, jobs.TypeUp, jobs.TypeDown:
		return auth.ActionOperate
	}
	return auth.ActionBackup
}

// jobPage is the data of the job page.
type jobPage struct {
	Job store.Job
	// Progress is what a running job does right now (the worker's
	// progress.json); nil if the job is not running or reports nothing.
	Progress *jobs.Progress
	// Queue says why a queued job has not started yet; nil otherwise.
	Queue     *queueInfo
	Log       []logLine
	LogCut    bool  // only the end of the log is shown
	LogOffset int64 // byte offset in log.txt after the shown lines; the live stream continues there
	CanCancel bool
	CSRF      string
}

// jobDetail shows a job with its log. While the job is not final,
// static/joblog.js follows it live through jobEvents (SSE).
func (s *Server) jobDetail(w http.ResponseWriter, r *http.Request) {
	j, perm, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	page := s.newJobPage(r.Context(), j, perm)
	if s.JobsDir != "" {
		var err error
		page.Log, page.LogCut, page.LogOffset, err = readLogTail(s.logPath(j.ID), s.loc())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Logger.Warn("reading job log", "job_id", j.ID, "error", err)
		}
	}
	title := fmt.Sprintf("%s of %s", capitalize(string(j.Type)), j.StackName)
	if j.Type.Host() {
		title = "Host cleanup"
	}
	s.render(w, r, http.StatusOK, "job.html", pageData{Title: title, Data: page})
}

// newJobPage returns the job page data without the log.
func (s *Server) newJobPage(ctx context.Context, j store.Job, perm store.Permission) jobPage {
	a := authFrom(ctx)
	page := jobPage{Job: j, CSRF: a.Session.CSRFToken,
		CanCancel: !j.State.Final() && s.Jobs != nil && mayRunJob(a.User, perm, j.Type)}
	if j.State == jobs.StateQueued {
		if qs, err := s.Store.QueueState(ctx, j); err == nil {
			page.Queue = &queueInfo{QueueState: qs, MaxJobs: s.MaxJobs}
		} else {
			s.Logger.Warn("loading queue state", "job_id", j.ID, "error", err)
		}
	}
	if j.State == jobs.StateRunning && s.JobsDir != "" {
		// Informational: a missing or unreadable file shows no progress.
		if p, err := jobs.ReadProgress(filepath.Join(s.JobsDir, j.ID)); err == nil && p.Phase != "" {
			page.Progress = &p
		}
	}
	return page
}

// cancelJob cancels a queued or running job. It needs the same right as
// starting a job of that type.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, perm, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	if !mayRunJob(a.User, perm, j.Type) {
		s.renderError(w, r, http.StatusForbidden, "You are not allowed to do this.")
		return
	}
	if s.Jobs == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, s.jobsUnavailable())
		return
	}
	if err := s.Jobs.Cancel(r.Context(), j.ID); err != nil {
		if errors.Is(err, dispatch.ErrNotCancellable) {
			http.Redirect(w, r, jobURL(j.ID), http.StatusSeeOther)
			return
		}
		s.internalError(w, r, "cancelling job", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "job.cancel", StackID: j.StackID, Detail: "job " + j.ID})
	http.Redirect(w, r, jobURL(j.ID), http.StatusSeeOther)
}

// jobURL is the path of a job page.
func jobURL(id string) string { return "/jobs/" + url.PathEscape(id) }

// queueInfo is the queue state of a queued job for the job page.
type queueInfo struct {
	store.QueueState
	MaxJobs int // STACKVAULT_MAX_JOBS; 0 if unknown
}

// SlotsFull reports whether the job waits for a running job to finish
// because the limit of jobs running at the same time is reached.
func (q queueInfo) SlotsFull() bool { return q.MaxJobs > 0 && q.Running >= q.MaxJobs }
