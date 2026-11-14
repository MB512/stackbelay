package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/dispatch"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
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
	opts := store.JobOptions{PreStop: r.PostFormValue("pre_stop") == "on"}
	s.submitJob(w, r, st, jobs.TypeBackup, opts, fmt.Sprintf("backup, pre-stop snapshot: %t", opts.PreStop))
}

// submitJob checks the preconditions shared by all job types, queues the
// job and redirects to its page.
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request, st store.Stack, typ jobs.Type, opts store.JobOptions, detail string) {
	ctx := r.Context()
	if reason := s.jobsUnavailable(); reason != "" {
		s.renderError(w, r, http.StatusServiceUnavailable, reason)
		return
	}
	if !st.Present {
		s.renderError(w, r, http.StatusConflict, "The stack directory is missing.")
		return
	}
	if _, err := s.Store.RepositoryInfoForStack(ctx, st.ID); errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusConflict, "This stack has no repository.")
		return
	} else if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}
	a := authFrom(ctx)
	j, err := s.Jobs.Submit(ctx, store.Job{StackID: st.ID, Type: typ, Trigger: store.TriggerManual, RequestedBy: a.User.ID, Options: opts})
	if errors.Is(err, store.ErrJobQueued) {
		s.renderError(w, r, http.StatusConflict, fmt.Sprintf("A %s of this stack is already waiting to run.", typ))
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
}

// restorableSnapshot returns the cached snapshot id of the stack, if it
// exists in the cache and was not removed from the repository. The worker
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
	snaps, err := s.Store.ListSnapshots(ctx, repo.ID)
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
	st, _, ok := s.loadStack(w, r, auth.ActionRestore)
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
	s.render(w, r, http.StatusOK, "restore.html", pageData{Title: "Restore " + st.Name,
		Error: s.jobsUnavailable(), Data: restorePage{Stack: st, Snapshot: sn}})
}

// startRestore queues a restore. Only admins may skip the safety snapshot.
func (s *Server) startRestore(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionRestore)
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
	opts := store.JobOptions{
		SnapshotID: sn.ID,
		NoStart:    r.PostFormValue("start") != "on",
		// The safety snapshot is the way back if the restore was a mistake.
		SkipSafetySnapshot: authFrom(r.Context()).User.IsAdmin && r.PostFormValue("safety_snapshot") != "on",
	}
	s.submitJob(w, r, st, jobs.TypeRestore, opts, fmt.Sprintf("restore of snapshot %s, safety snapshot: %t, start: %t",
		sn.ShortID, !opts.SkipSafetySnapshot, !opts.NoStart))
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

// jobAction is the right needed to start (and cancel) a job of this type.
func jobAction(t jobs.Type) auth.Action {
	if t == jobs.TypeRestore {
		return auth.ActionRestore
	}
	return auth.ActionBackup
}

// jobPage is the data of the job page.
type jobPage struct {
	Job       store.Job
	Log       []logLine
	LogCut    bool // only the end of the log is shown
	CanCancel bool
	CSRF      string
}

// jobDetail shows a job with its log. While the job is not final the page
// polls itself (HTMX) and replaces the job section.
func (s *Server) jobDetail(w http.ResponseWriter, r *http.Request) {
	j, perm, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	page := jobPage{Job: j, CSRF: a.Session.CSRFToken,
		CanCancel: !j.State.Final() && s.Jobs != nil && auth.Allowed(a.User, perm, jobAction(j.Type))}
	if s.JobsDir != "" {
		var err error
		page.Log, page.LogCut, err = readLog(filepath.Join(s.JobsDir, j.ID, jobs.LogFile))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Logger.Warn("reading job log", "job_id", j.ID, "error", err)
		}
	}
	if isHTMX(r) {
		s.renderPartial(w, "job.html", "job", page)
		return
	}
	s.render(w, r, http.StatusOK, "job.html", pageData{Title: fmt.Sprintf("%s of %s", capitalize(string(j.Type)), j.StackName), Data: page})
}

// cancelJob cancels a queued or running job. It needs the same right as
// starting a job of that type.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, perm, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	if !auth.Allowed(a.User, perm, jobAction(j.Type)) {
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

// logLine is one parsed line of a job log (JSON lines written by slog).
type logLine struct {
	Time  string
	Level string
	Msg   string
	Attrs string // remaining fields as "key=value ..."
}

// maxLogBytes is how much of the end of a job log is shown.
const maxLogBytes = 512 << 10

// readLog reads the end of a job log. Lines that are not JSON are shown as
// they are. The worker already redacted secrets.
func readLog(path string) (lines []logLine, cut bool, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is built from a validated job id
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() > maxLogBytes {
		if _, err := f.Seek(-maxLogBytes, io.SeekEnd); err != nil {
			return nil, false, err
		}
		cut = true
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLogBytes))
	if err != nil {
		return nil, false, err
	}
	if cut { // drop the partial first line
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxLogBytes)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, parseLogLine(line))
		}
	}
	return lines, cut, sc.Err()
}

func parseLogLine(line string) logLine {
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return logLine{Msg: line}
	}
	l := logLine{}
	if v, ok := rec["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			l.Time = t.UTC().Format("15:04:05")
		}
	}
	l.Level, _ = rec["level"].(string)
	l.Msg, _ = rec["msg"].(string)
	var attrs []string
	for _, k := range slices.Sorted(maps.Keys(rec)) {
		switch k {
		case "time", "level", "msg", "job_id":
			continue
		}
		attrs = append(attrs, fmt.Sprintf("%s=%v", k, rec[k]))
	}
	l.Attrs = strings.Join(attrs, " ")
	return l
}

// jobURL is the path of a job page.
func jobURL(id string) string { return "/jobs/" + url.PathEscape(id) }
