// Package scheduler queues scheduled backups and updates. It only creates queued jobs;
// they run through the same queue and worker containers as manual jobs
// (internal/dispatch). Runs missed while the manager was down are not
// replayed.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// MaxExprLen is the maximum length of a cron expression.
const MaxExprLen = 100

// MinInterval is the shortest interval allowed for "@every".
const MinInterval = time.Minute

// parser accepts the standard 5 fields (minute hour day-of-month month
// day-of-week) and descriptors such as @daily or @every 6h.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a cron expression and returns its schedule. Times are
// in the manager's local time zone (TZ); per-schedule time zones
// (CRON_TZ=) are not accepted. Expressions that never fire are rejected.
func Parse(expr string) (sched cron.Schedule, err error) {
	expr = strings.TrimSpace(expr)
	switch {
	case expr == "":
		return nil, errors.New("the cron expression is empty")
	case len(expr) > MaxExprLen:
		return nil, fmt.Errorf("the cron expression is longer than %d characters", MaxExprLen)
	case strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ="):
		return nil, errors.New("time zones in the expression are not supported; the manager's time zone (TZ) is used")
	}
	// The parser is third-party code fed with user input: turn a panic on
	// odd input into an error (recover in a deferred function works like a
	// catch block around the whole function).
	defer func() {
		if r := recover(); r != nil {
			sched, err = nil, fmt.Errorf("invalid cron expression %q", expr)
		}
	}()
	sched, err = parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	if every, ok := sched.(cron.ConstantDelaySchedule); ok && every.Delay < MinInterval {
		return nil, fmt.Errorf("@every must be at least %s", MinInterval)
	}
	if sched.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("the cron expression %q never matches a date", expr)
	}
	return sched, nil
}

// Normalize returns the expression as it is stored (trimmed, single spaces).
func Normalize(expr string) string { return strings.Join(strings.Fields(expr), " ") }

// Queue queues jobs (*dispatch.Dispatcher).
type Queue interface {
	Submit(ctx context.Context, j store.Job) (store.Job, error)
}

// Scheduler fires the enabled schedules of enabled stacks.
type Scheduler struct {
	Store  *store.Store
	Queue  Queue
	Logger *slog.Logger
	// Location is the time zone the cron expressions are evaluated in
	// (the manager's TZ); nil means time.Local.
	Location *time.Location

	mu      sync.Mutex // guards cron and ctx; Reload is called from HTTP handlers
	cron    *cron.Cron
	ctx     context.Context
	entries int
}

// Run loads the schedules and fires them until ctx is cancelled. It waits
// for schedules that are being fired to finish before returning.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	// SkipIfStillRunning: a schedule never fires twice at the same time.
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}
	s.cron = cron.New(cron.WithParser(parser), cron.WithLocation(loc),
		cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	s.ctx = ctx
	s.mu.Unlock()

	if err := s.Reload(ctx); err != nil {
		s.Logger.Error("loading schedules", "error", err)
	}
	s.cron.Start()
	<-ctx.Done()
	<-s.cron.Stop().Done() // Stop returns a context that is done when running fires have ended
}

// Reload replaces all cron entries with the current schedules from the
// database. Call it after a schedule was changed. Before Run it does
// nothing.
func (s *Scheduler) Reload(ctx context.Context) error {
	list, err := s.Store.ListSchedules(ctx, 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return nil
	}
	for _, e := range s.cron.Entries() {
		s.cron.Remove(e.ID)
	}
	s.entries = 0
	runCtx := s.ctx
	for _, sc := range list {
		if !sc.Enabled {
			continue
		}
		sched, err := Parse(sc.CronExpr)
		if err != nil { // validated on save, so only possible after manual DB edits
			s.Logger.Error("skipping invalid schedule", "schedule_id", sc.ID, "stack", sc.StackName, "error", err)
			continue
		}
		id := sc.ID
		s.cron.Schedule(sched, cron.FuncJob(func() { s.Fire(runCtx, id) }))
		s.entries++
	}
	s.Logger.Info("schedules loaded", "active", s.entries)
	return nil
}

// Fire queues the job of one schedule: a backup, an update, or (host
// schedule) a cleanup. It re-reads the schedule and its stack, so a
// schedule or stack disabled in the meantime is skipped. A job of the same type that is already queued
// for the stack is not queued twice. Problems are logged; the queued job
// itself reports its result.
//
// Scheduled updates always run "only if new": a schedule never stops or
// backs up a stack when no image changed. Without a repository they fail
// (there is no one to confirm an update without backup).
func (s *Scheduler) Fire(ctx context.Context, scheduleID int64) {
	log := s.Logger.With("schedule_id", scheduleID)
	sc, err := s.Store.ScheduleByID(ctx, scheduleID)
	if errors.Is(err, store.ErrNotFound) {
		return // deleted since the last reload
	}
	if err != nil {
		log.Error("scheduled job: loading schedule", "error", err)
		return
	}
	if !sc.Enabled {
		return // disabled since the last reload
	}
	what := "scheduled " + string(sc.Type) // "scheduled backup", "scheduled update", "scheduled cleanup"
	log = log.With("stack", sc.StackName, "type", sc.Type)
	var st store.Stack // zero for a host schedule
	if !sc.Type.Host() {
		if st, err = s.Store.StackByName(ctx, sc.StackName); err != nil {
			log.Error(what+": loading stack", "error", err)
			return
		}
	}
	job := store.Job{StackID: st.ID, Type: sc.Type, Trigger: store.TriggerSchedule}
	var detail string
	switch sc.Type {
	case jobs.TypeCleanup:
		detail = "host cleanup"
	case jobs.TypeBackup:
		detail = "pre-stop snapshot"
	case jobs.TypeUpdate:
		if sc.NoBackup {
			// Confirmed when the schedule was created: while the stack has
			// no repository, the update runs without backup and rollback
			// (the dispatcher still backs up if it has one by then).
			job.Options = store.JobOptions{OnlyIfNew: true, NoBackup: true}
			detail = "only if new, without backup if the stack has no repository"
			break
		}
		job.Options = store.JobOptions{OnlyIfNew: true}
		detail = "only if new"
	default:
		log.Error("skipping schedule with unknown job type")
		return
	}
	j, err := s.Queue.Submit(ctx, job)
	if errors.Is(err, store.ErrJobQueued) {
		log.Warn(what + " skipped: a job of this type is already waiting to run")
		return
	}
	if err != nil {
		log.Error(what+": queueing job", "error", err)
		return
	}
	log.Info(what+" queued", "job_id", j.ID)
	if err := s.Store.AddAudit(ctx, store.AuditEntry{Action: "job.create", StackID: st.ID,
		Detail: fmt.Sprintf("job %s: %s (schedule %d: %s), %s", j.ID, what, sc.ID, sc.CronExpr, detail)}); err != nil {
		log.Error("writing audit log", "error", err)
	}
}

// Active returns the number of active cron entries (for tests and logs).
func (s *Scheduler) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries
}
