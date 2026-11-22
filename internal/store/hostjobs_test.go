package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

// TestHostJobsMigration applies migrations up to 0010 by hand, fills the
// tables that 0011 rebuilds, and checks that Open keeps every row,
// including the compose content of a job (which a careless rebuild would
// lose through ON DELETE CASCADE).
func TestHostJobsMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`)
	for _, m := range ms {
		if m.version > 10 {
			break
		}
		exec(m.sql)
		exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-11-01T00:00:00Z')`, m.version, m.name)
	}
	exec(`INSERT INTO stacks (id, name, enabled) VALUES (1, 'web', 1)`)
	exec(`INSERT INTO jobs (id, stack_id, type, trigger, state, options, queued_at, started_at, finished_at, result)
	      VALUES ('01AAAAAAAAAAAAAAAAAAAAAAA1', 1, 'compose-apply', 'manual', 'succeeded', '{"compose_file":"compose.yaml"}',
	              '2026-11-01T10:00:00Z', '2026-11-01T10:00:01Z', '2026-11-01T10:01:00Z', '{"unchanged":true}')`)
	exec(`INSERT INTO job_compose (job_id, content) VALUES ('01AAAAAAAAAAAAAAAAAAAAAAA1', x'7365727669636573')`)
	exec(`INSERT INTO schedules (id, stack_id, cron_expr, pre_stop, enabled, job_type, rollback_in_place, no_backup)
	      VALUES (7, 1, '@daily', 1, 0, 'update', 1, 0)`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.JobByID(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1")
	if err != nil || j.StackID != 1 || j.StackName != "web" || j.Type != jobs.TypeComposeApply ||
		j.State != jobs.StateSucceeded || j.Options.ComposeFile != "compose.yaml" || !j.Result.Unchanged || j.FinishedAt.IsZero() {
		t.Fatalf("job after migration: %+v, %v", j, err)
	}
	if c, err := s.JobComposeContent(ctx, j.ID); err != nil || string(c) != "services" {
		t.Fatalf("compose content after migration: %q, %v", c, err)
	}
	sc, err := s.ScheduleByID(ctx, 7)
	if err != nil || sc.StackID != 1 || sc.CronExpr != "@daily" || sc.Enabled || sc.Type != jobs.TypeUpdate {
		t.Fatalf("schedule after migration: %+v, %v", sc, err)
	}
	var fkProblems int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fkProblems); err != nil || fkProblems != 0 {
		t.Fatalf("foreign key check: %d, %v", fkProblems, err)
	}
	// The cascade still works on the rebuilt tables.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobComposeContent(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("compose content not deleted with its job: %v", err)
	}
}

func TestHostJobsRunAlone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	// A host job has no stack, a stack job needs one.
	if err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA0", StackID: web, Type: jobs.TypeCleanup,
		Trigger: TriggerManual, QueuedAt: t0}); err == nil {
		t.Fatal("cleanup job with a stack accepted")
	}
	if err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA0", Type: jobs.TypeBackup,
		Trigger: TriggerManual, QueuedAt: t0}); err == nil {
		t.Fatal("backup job without a stack accepted")
	}

	a := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, t0)
	h := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA2", 0, jobs.TypeCleanup, t0.Add(time.Second))
	c := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA3", db, jobs.TypeBackup, t0.Add(2*time.Second))
	// One queued host job at a time.
	if err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA4", Type: jobs.TypeCleanup, Trigger: TriggerSchedule,
		QueuedAt: t0}); !errors.Is(err, ErrJobQueued) {
		t.Fatalf("second queued cleanup: %v", err)
	}
	claim := func() string {
		t.Helper()
		j, ok, err := s.ClaimNextJob(ctx, 5, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return j.ID
	}

	// a starts; the host job waits for it, and c (queued after the host
	// job) waits for the host job although its stack is free.
	if got := claim(); got != a.ID {
		t.Fatalf("first claim = %q, want a", got)
	}
	if got := claim(); got != "" {
		t.Fatalf("claimed %q while a runs and a host job waits", got)
	}
	if qs, _ := s.QueueState(ctx, h); qs.Blocking == nil || qs.Blocking.ID != a.ID {
		t.Fatalf("host job blocked by %+v, want a", qs.Blocking)
	}
	if qs, _ := s.QueueState(ctx, c); qs.Blocking == nil || qs.Blocking.ID != h.ID {
		t.Fatalf("c blocked by %+v, want the host job", qs.Blocking)
	}

	// a ends: the host job runs alone, c still waits.
	if err := s.FinishJob(ctx, a.ID, jobs.StateSucceeded, "", "", JobResult{}, t0); err != nil {
		t.Fatal(err)
	}
	if got := claim(); got != h.ID {
		t.Fatalf("claim after a = %q, want the host job", got)
	}
	if got := claim(); got != "" {
		t.Fatalf("claimed %q while the host job runs", got)
	}
	if qs, _ := s.QueueState(ctx, c); qs.Blocking == nil || qs.Blocking.ID != h.ID {
		t.Fatalf("c blocked by %+v, want the running host job", qs.Blocking)
	}
	running, err := s.JobByID(ctx, h.ID)
	if err != nil || running.StackID != 0 || running.StackName != "" || running.State != jobs.StateRunning {
		t.Fatalf("host job: %+v, %v", running, err)
	}

	// The database itself refuses a second running host job.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO jobs (id, type, trigger, state, queued_at) VALUES
		('01AAAAAAAAAAAAAAAAAAAAAAA9', 'cleanup', 'manual', 'running', '2026-11-18T10:00:00Z')`); err == nil {
		t.Fatal("second running host job accepted by the database")
	}

	if err := s.FinishJob(ctx, h.ID, jobs.StateSucceeded, "", "", JobResult{}, t0); err != nil {
		t.Fatal(err)
	}
	if got := claim(); got != c.ID {
		t.Fatalf("claim after the host job = %q, want c", got)
	}
}

func TestHostSchedules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	for _, bad := range []Schedule{
		{StackID: web, CronExpr: "@daily", Type: jobs.TypeCleanup},
		{CronExpr: "@daily", Type: jobs.TypeBackup},
		{CronExpr: "@daily", Type: jobs.TypeCleanup, NoBackup: true},
	} {
		if _, err := s.CreateSchedule(ctx, bad); err == nil {
			t.Errorf("schedule %+v accepted", bad)
		}
	}
	h, err := s.CreateSchedule(ctx, Schedule{CronExpr: "@weekly", Type: jobs.TypeCleanup})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, Schedule{StackID: web, CronExpr: "@daily", Type: jobs.TypeBackup}); err != nil {
		t.Fatal(err)
	}
	host, err := s.ListHostSchedules(ctx)
	if err != nil || len(host) != 1 || host[0].ID != h || host[0].StackID != 0 || host[0].StackName != "" || !host[0].Enabled {
		t.Fatalf("host schedules: %+v, %v", host, err)
	}
	if all, _ := s.ListSchedules(ctx, 0); len(all) != 2 || all[0].ID != h {
		t.Fatalf("all schedules: %+v", all)
	}
	if list, _ := s.ListSchedules(ctx, web); len(list) != 1 || list[0].Type != jobs.TypeBackup {
		t.Fatalf("web schedules: %+v", list)
	}
	// A host schedule is changed through the host (0), not a stack.
	if err := s.SetScheduleEnabled(ctx, web, h, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabling the host schedule through a stack: %v", err)
	}
	if err := s.SetScheduleEnabled(ctx, 0, h, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSchedule(ctx, 0, h); err != nil {
		t.Fatal(err)
	}
	if host, _ := s.ListHostSchedules(ctx); len(host) != 0 {
		t.Fatalf("host schedule not deleted: %+v", host)
	}
}

func TestReplacedImagesSince(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	finish := func(id string, at time.Time, replaced ...string) {
		t.Helper()
		newJob(t, s, id, web, jobs.TypeUpdate, at)
		if err := s.FinishJob(ctx, id, jobs.StateSucceeded, "", "", JobResult{ReplacedImages: replaced}, at); err != nil {
			t.Fatal(err)
		}
	}
	finish("01AAAAAAAAAAAAAAAAAAAAAAA1", t0.Add(-10*24*time.Hour), "sha256:old")
	finish("01AAAAAAAAAAAAAAAAAAAAAAA2", t0.Add(-2*24*time.Hour), "sha256:b", "sha256:a")
	finish("01AAAAAAAAAAAAAAAAAAAAAAA3", t0, "sha256:a")
	finish("01AAAAAAAAAAAAAAAAAAAAAAA4", t0)
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA5", web, jobs.TypeUpdate, t0) // still queued
	got, err := s.ReplacedImagesSince(ctx, t0.Add(-7*24*time.Hour))
	if err != nil || len(got) != 2 || got[0] != "sha256:a" || got[1] != "sha256:b" {
		t.Fatalf("replaced images = %v, %v", got, err)
	}
}
