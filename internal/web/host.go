package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/scheduler"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// The admin "Host" tab (issue #19): the host cleanup with a preview, a
// "Clean up now" button and host schedules. The preview only lists
// (jobs.CleanupLister has no removal methods); removing runs in a worker
// job, which recomputes everything itself.

const (
	// recentHostJobs is the number of cleanup jobs shown on the Host tab.
	recentHostJobs = 10
	// cleanupPreviewTimeout bounds the docker commands of a preview.
	cleanupPreviewTimeout = 2 * time.Minute
)

// hostPage is the data of the Host tab.
type hostPage struct {
	MinAge      time.Duration // STACKBELAY_CLEANUP_IMAGE_MIN_AGE
	Unavailable string        // why no cleanup can be queued, or ""
	Schedules   []scheduleRow
	NewCron     string // the schedule form's input after an error
	Jobs        []store.Job
	// Preview is set when the admin asked for it (?preview=1).
	Preview      *jobs.CleanupPlan
	PreviewError string
	NoLister     bool // this installation cannot list (no docker CLI)
}

// MinAgeLabel is the minimum age for the page, e.g. "7 days" or "36 hours".
func (p hostPage) MinAgeLabel() string {
	h := int(p.MinAge / time.Hour)
	switch {
	case h == 0:
		return "0 hours"
	case h%24 == 0 && h/24 == 1:
		return "1 day"
	case h%24 == 0:
		return fmt.Sprintf("%d days", h/24)
	case h == 1:
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// adminHost shows the Host tab; with ?preview=1 it also lists what a
// cleanup would remove now.
func (s *Server) adminHost(w http.ResponseWriter, r *http.Request) {
	s.renderHost(w, r, http.StatusOK, hostPage{}, "", r.URL.Query().Get("preview") == "1")
}

func (s *Server) renderHost(w http.ResponseWriter, r *http.Request, status int, page hostPage, errMsg string, preview bool) {
	ctx := r.Context()
	page.MinAge, page.Unavailable, page.NoLister = s.CleanupImageMinAge, s.jobsUnavailable(), s.Cleanup == nil
	list, err := s.Store.ListHostSchedules(ctx)
	if err != nil {
		s.internalError(w, r, "listing host schedules", err)
		return
	}
	page.Schedules = s.withNextRun(list)
	if page.Jobs, err = s.Store.ListHostJobs(ctx, recentHostJobs); err != nil {
		s.internalError(w, r, "listing host jobs", err)
		return
	}
	if preview && s.Cleanup != nil {
		plan, err := s.cleanupPreview(ctx)
		if err != nil {
			s.Logger.Warn("cleanup preview", "error", err)
			page.PreviewError = "Listing failed: " + truncate(err.Error(), 300)
		} else {
			page.Preview = &plan
		}
	}
	s.render(w, r, status, "admin_host.html", pageData{Title: "Host", Error: errMsg, Data: page})
}

// cleanupPreview lists what a cleanup would remove now, with the rules the
// dispatcher gives the job (see Dispatcher.cleanupSpec).
func (s *Server) cleanupPreview(ctx context.Context) (jobs.CleanupPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupPreviewTimeout)
	defer cancel()
	var list []stacks.Stack
	if s.StacksRoot != "" {
		var err error
		if list, err = stacks.Discover(s.StacksRoot); err != nil {
			return jobs.CleanupPlan{}, err
		}
	}
	now := s.now()
	keep, err := s.Store.ReplacedImagesSince(ctx, now.Add(-s.CleanupImageMinAge))
	if err != nil {
		return jobs.CleanupPlan{}, err
	}
	return jobs.ListCleanup(ctx, s.Cleanup, list, jobs.CleanupOptions{ImageMinAge: s.CleanupImageMinAge, KeepImages: keep}, now)
}

// startCleanup queues a host cleanup (admin only).
func (s *Server) startCleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if reason := s.jobsUnavailable(); reason != "" {
		s.renderError(w, r, http.StatusServiceUnavailable, reason)
		return
	}
	a := authFrom(ctx)
	j, err := s.Jobs.Submit(ctx, store.Job{Type: jobs.TypeCleanup, Trigger: store.TriggerManual, RequestedBy: a.User.ID,
		Options: store.JobOptions{NotifyMuted: s.markMuted(a.User, 0)}})
	if errors.Is(err, store.ErrJobQueued) {
		s.renderHost(w, r, http.StatusConflict, hostPage{}, "A cleanup is already waiting to run.", false)
		return
	}
	if err != nil {
		s.internalError(w, r, "queueing cleanup", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "job.create", Detail: fmt.Sprintf("job %s: host cleanup", j.ID)})
	http.Redirect(w, r, jobURL(j.ID), http.StatusSeeOther)
}

// createHostSchedule adds a cleanup schedule (admin only).
func (s *Server) createHostSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	expr := scheduler.Normalize(r.PostFormValue("cron_expr"))
	fail := func(status int, msg string) {
		s.renderHost(w, r, status, hostPage{NewCron: expr}, msg, false)
	}
	if _, err := scheduler.Parse(expr); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.Store.ListHostSchedules(ctx)
	if err != nil {
		s.internalError(w, r, "listing host schedules", err)
		return
	}
	if len(existing) >= maxSchedulesPerStack {
		fail(http.StatusBadRequest, fmt.Sprintf("The host can have at most %d schedules.", maxSchedulesPerStack))
		return
	}
	id, err := s.Store.CreateSchedule(ctx, store.Schedule{CronExpr: expr, Type: jobs.TypeCleanup})
	if err != nil {
		s.internalError(w, r, "creating host schedule", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "schedule.create",
		Detail: fmt.Sprintf("schedule %d: host cleanup %s", id, expr)})
	s.reloadSchedules(ctx)
	http.Redirect(w, r, "/admin/host", http.StatusSeeOther)
}

// setHostScheduleEnabled enables or disables a host schedule (admin only).
func (s *Server) setHostScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.hostScheduleID(w, r)
	if !ok {
		return
	}
	enabled := r.PostFormValue("enabled") == "true"
	if !s.hostScheduleChanged(w, r, id, s.Store.SetScheduleEnabled(r.Context(), 0, id, enabled)) {
		return
	}
	action := "schedule.disable"
	if enabled {
		action = "schedule.enable"
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: action,
		Detail: fmt.Sprintf("schedule %d (host cleanup)", id)})
	s.reloadSchedules(r.Context())
	http.Redirect(w, r, "/admin/host", http.StatusSeeOther)
}

// deleteHostSchedule deletes a host schedule (admin only).
func (s *Server) deleteHostSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.hostScheduleID(w, r)
	if !ok {
		return
	}
	if !s.hostScheduleChanged(w, r, id, s.Store.DeleteSchedule(r.Context(), 0, id)) {
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "schedule.delete",
		Detail: fmt.Sprintf("schedule %d (host cleanup)", id)})
	s.reloadSchedules(r.Context())
	http.Redirect(w, r, "/admin/host", http.StatusSeeOther)
}

func (s *Server) hostScheduleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, http.StatusNotFound, "Schedule not found.")
		return 0, false
	}
	return id, true
}

// hostScheduleChanged handles the error of a host schedule update; the
// store only changes host schedules here, so ErrNotFound also covers ids
// of stack schedules.
func (s *Server) hostScheduleChanged(w http.ResponseWriter, r *http.Request, id int64, err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Schedule not found.")
		return false
	}
	if err != nil {
		s.internalError(w, r, fmt.Sprintf("updating host schedule %d", id), err)
		return false
	}
	return true
}
