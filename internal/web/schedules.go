package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/scheduler"
	"github.com/MB512/stackvault/internal/store"
)

// ScheduleReloader reloads the scheduler after a schedule was changed
// (*scheduler.Scheduler).
type ScheduleReloader interface {
	Reload(ctx context.Context) error
}

// maxSchedulesPerStack limits the schedules of one stack.
const maxSchedulesPerStack = 20

// scheduleRow is one schedule as shown in the UI.
type scheduleRow struct {
	store.Schedule
	Next time.Time // next run; zero if the schedule or stack is disabled
}

// scheduleRows lists the schedules of st with their next run.
func (s *Server) scheduleRows(ctx context.Context, st store.Stack) ([]scheduleRow, error) {
	list, err := s.Store.ListSchedules(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	// Cron expressions are evaluated in the manager's time zone, like the
	// scheduler does it.
	now := s.now().In(s.loc())
	rows := make([]scheduleRow, 0, len(list))
	for _, sc := range list {
		row := scheduleRow{Schedule: sc}
		if sc.Enabled && st.Enabled {
			if sched, err := scheduler.Parse(sc.CronExpr); err == nil {
				row.Next = sched.Next(now)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// nextScheduled returns the earliest next run of the rows of job type t
// (zero if none).
func nextScheduled(rows []scheduleRow, t jobs.Type) time.Time {
	var next time.Time
	for _, r := range rows {
		if r.Type == t && !r.Next.IsZero() && (next.IsZero() || r.Next.Before(next)) {
			next = r.Next
		}
	}
	return next
}

// createSchedule adds a schedule to a stack (admin only).
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	in := newSchedule{
		CronExpr: scheduler.Normalize(r.PostFormValue("cron_expr")),
		Update:   r.PostFormValue("job_type") == string(jobs.TypeUpdate),
		PreStop:  r.PostFormValue("pre_stop") == "on",
	}
	in.RollbackInPlace = in.Update && r.PostFormValue("rollback_in_place") == "on" // updates only
	in.ConfirmNoBackup = r.PostFormValue("confirm_no_backup") == "on"
	expr := in.CronExpr
	fail := func(msg string) {
		s.renderSettings(w, r, st, settingsView{Status: http.StatusBadRequest, Error: msg, NewSchedule: in})
	}
	if t := r.PostFormValue("job_type"); t != "" && t != string(jobs.TypeBackup) && t != string(jobs.TypeUpdate) {
		fail("Unknown job type.")
		return
	}
	if _, err := scheduler.Parse(expr); err != nil {
		fail(err.Error())
		return
	}
	existing, err := s.Store.ListSchedules(ctx, st.ID)
	if err != nil {
		s.internalError(w, r, "listing schedules", err)
		return
	}
	if len(existing) >= maxSchedulesPerStack {
		fail(fmt.Sprintf("A stack can have at most %d schedules.", maxSchedulesPerStack))
		return
	}
	sc := store.Schedule{StackID: st.ID, CronExpr: expr, Type: jobs.TypeBackup, PreStop: in.PreStop,
		RollbackInPlace: in.RollbackInPlace}
	if in.Update {
		sc.Type = jobs.TypeUpdate
		hasRepo, err := s.hasRepository(ctx, st)
		if err != nil {
			s.internalError(w, r, "loading repository", err)
			return
		}
		if !hasRepo {
			// Like a manual update: without repository only after the
			// explicit confirmation, and then without snapshots and
			// rollback.
			if !in.ConfirmNoBackup {
				fail("This stack has no repository: confirm that its scheduled updates run without backup and without rollback.")
				return
			}
			sc.NoBackup, sc.PreStop, sc.RollbackInPlace = true, false, false
		}
	}
	id, err := s.Store.CreateSchedule(ctx, sc)
	if err != nil {
		s.internalError(w, r, "creating schedule", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "schedule.create", StackID: st.ID,
		Detail: fmt.Sprintf("schedule %d: %s %s, pre-stop snapshot: %t, rollback in place: %t, without backup: %t",
			id, sc.Type, expr, sc.PreStop, sc.RollbackInPlace, sc.NoBackup)})
	s.reloadSchedules(ctx)
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}

// setScheduleEnabled enables or disables a schedule (admin only).
func (s *Server) setScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	st, id, ok := s.loadSchedule(w, r)
	if !ok {
		return
	}
	enabled := r.PostFormValue("enabled") == "true"
	if !s.scheduleChanged(w, r, st, id, s.Store.SetScheduleEnabled(r.Context(), st.ID, id, enabled)) {
		return
	}
	action := "schedule.disable"
	if enabled {
		action = "schedule.enable"
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: action, StackID: st.ID,
		Detail: fmt.Sprintf("schedule %d", id)})
	s.reloadSchedules(r.Context())
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}

// deleteSchedule deletes a schedule (admin only).
func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	st, id, ok := s.loadSchedule(w, r)
	if !ok {
		return
	}
	if !s.scheduleChanged(w, r, st, id, s.Store.DeleteSchedule(r.Context(), st.ID, id)) {
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "schedule.delete", StackID: st.ID,
		Detail: fmt.Sprintf("schedule %d", id)})
	s.reloadSchedules(r.Context())
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}

// loadSchedule resolves the stack and the schedule id in the URL.
func (s *Server) loadSchedule(w http.ResponseWriter, r *http.Request) (store.Stack, int64, bool) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return st, 0, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, http.StatusNotFound, "Schedule not found.")
		return st, 0, false
	}
	return st, id, true
}

// scheduleChanged handles the error of a schedule update. The store only
// changes schedules of the given stack, so ErrNotFound also covers ids of
// other stacks.
func (s *Server) scheduleChanged(w http.ResponseWriter, r *http.Request, st store.Stack, id int64, err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Schedule not found.")
		return false
	}
	if err != nil {
		s.internalError(w, r, fmt.Sprintf("updating schedule %d of stack %s", id, st.Name), err)
		return false
	}
	return true
}

// reloadSchedules tells the scheduler about a change. The change is saved
// either way; a failed reload is logged and fixed by the next change or a
// restart.
func (s *Server) reloadSchedules(ctx context.Context) {
	if s.Schedules == nil {
		return
	}
	if err := s.Schedules.Reload(ctx); err != nil {
		s.Logger.Error("reloading schedules", "error", err)
	}
}
