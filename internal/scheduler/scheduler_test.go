package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/secrets"
	"github.com/MB512/stackbelay/internal/store"
)

func TestParse(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr string // "" = valid
	}{
		{"0 3 * * *", ""},
		{"  30  2 * * 1-5 ", ""},
		{"*/15 * * * *", ""},
		{"@daily", ""},
		{"@weekly", ""},
		{"@every 6h", ""},
		{"@every 1m", ""},
		{"", "empty"},
		{"   ", "empty"},
		{"0 3 * *", "invalid"},      // 4 fields
		{"0 0 3 * * *", "invalid"},  // 6 fields (seconds are not supported)
		{"61 * * * *", "invalid"},   // minute out of range
		{"0 3 * * mon-", "invalid"}, // broken range
		{"@sometimes", "invalid"},   // unknown descriptor
		{"@every 30s", "at least"},  // too often
		{"@every soon", "invalid"},  // bad duration
		{"0 0 30 2 *", "never"},     // 30 February
		{"CRON_TZ=UTC 0 3 * * *", "time zone"},
		{"TZ=Europe/Berlin", "time zone"}, // would panic in the cron parser
		{strings.Repeat("1,", 60) + "1 * * * *", "longer"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.expr)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("Parse(%q) = %v, want valid", tt.expr, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("Parse(%q) = %v, want error containing %q", tt.expr, err, tt.wantErr)
		}
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("  0   3 *\t* * "); got != "0 3 * * *" {
		t.Errorf("Normalize = %q", got)
	}
}

// fakeQueue records submitted jobs.
type fakeQueue struct {
	mu   sync.Mutex
	jobs []store.Job
	err  error
}

func (q *fakeQueue) Submit(_ context.Context, j store.Job) (store.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return store.Job{}, q.err
	}
	j.ID = "20261115-030000-abcdef"
	q.jobs = append(q.jobs, j)
	return j, nil
}

func (q *fakeQueue) submitted() []store.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]store.Job(nil), q.jobs...)
}

type env struct {
	st    *store.Store
	queue *fakeQueue
	s     *Scheduler
	web   store.Stack
	db    store.Stack
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	box, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SyncStacks(ctx, []string{"web", "db"}); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, queue: &fakeQueue{}}
	e.s = &Scheduler{Store: st, Queue: e.queue, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if e.web, err = st.StackByName(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if e.db, err = st.StackByName(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) schedule(t *testing.T, st store.Stack, expr string) int64 {
	t.Helper()
	return e.scheduleOf(t, store.Schedule{StackID: st.ID, CronExpr: expr, Type: jobs.TypeBackup})
}

func (e *env) scheduleOf(t *testing.T, sc store.Schedule) int64 {
	t.Helper()
	id, err := e.st.CreateSchedule(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFire(t *testing.T) {
	ctx := context.Background()

	t.Run("queues a scheduled backup and audits it", func(t *testing.T) {
		e := newEnv(t)
		id := e.schedule(t, e.web, "0 3 * * *")
		e.s.Fire(ctx, id)
		got := e.queue.submitted()
		if len(got) != 1 {
			t.Fatalf("submitted %d jobs, want 1", len(got))
		}
		j := got[0]
		if j.StackID != e.web.ID || j.Type != jobs.TypeBackup || j.Trigger != store.TriggerSchedule ||
			j.RequestedBy != 0 {
			t.Errorf("job = %+v", j)
		}
		audit, err := e.st.ListAudit(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(audit) != 1 || audit[0].Action != "job.create" || audit[0].StackID != e.web.ID || audit[0].UserID != 0 ||
			!strings.Contains(audit[0].Detail, "scheduled backup") {
			t.Errorf("audit = %+v", audit)
		}
	})

	t.Run("queues a scheduled update: only if new, with its options", func(t *testing.T) {
		e := newEnv(t)
		for _, sc := range []store.Schedule{
			{StackID: e.web.ID, CronExpr: "@daily", Type: jobs.TypeUpdate},
			{StackID: e.db.ID, CronExpr: "@daily", Type: jobs.TypeUpdate},
		} {
			e.s.Fire(ctx, e.scheduleOf(t, sc))
		}
		got := e.queue.submitted()
		if len(got) != 2 {
			t.Fatalf("submitted %d jobs, want 2", len(got))
		}
		want := []store.JobOptions{{}, {}}
		for i, j := range got {
			if j.Type != jobs.TypeUpdate || j.Trigger != store.TriggerSchedule || j.Options != want[i] {
				t.Errorf("job %d = %+v, options want %+v", i, j, want[i])
			}
		}
		audit, _ := e.st.ListAudit(ctx, 10)
		if len(audit) != 2 || !strings.Contains(audit[0].Detail+audit[1].Detail, "scheduled update") {
			t.Errorf("audit = %+v", audit)
		}
	})

	t.Run("queues a host cleanup", func(t *testing.T) {
		e := newEnv(t)
		e.s.Fire(ctx, e.scheduleOf(t, store.Schedule{CronExpr: "@weekly", Type: jobs.TypeCleanup}))
		got := e.queue.submitted()
		if len(got) != 1 || got[0].StackID != 0 || got[0].Type != jobs.TypeCleanup || got[0].Trigger != store.TriggerSchedule {
			t.Fatalf("submitted %+v", got)
		}
		audit, _ := e.st.ListAudit(ctx, 10)
		if len(audit) != 1 || audit[0].StackID != 0 || !strings.Contains(audit[0].Detail, "scheduled cleanup") {
			t.Errorf("audit = %+v", audit)
		}
	})

	t.Run("queues a confirmed update without backup", func(t *testing.T) {
		e := newEnv(t)
		e.s.Fire(ctx, e.scheduleOf(t, store.Schedule{StackID: e.db.ID, CronExpr: "@daily", Type: jobs.TypeUpdate, NoBackup: true}))
		got := e.queue.submitted()
		if want := (store.JobOptions{NoBackup: true}); len(got) != 1 || got[0].Options != want {
			t.Fatalf("submitted %+v, want options %+v", got, want)
		}
		audit, _ := e.st.ListAudit(ctx, 10)
		if len(audit) != 1 || !strings.Contains(audit[0].Detail, "without backup") {
			t.Errorf("audit = %+v", audit)
		}
	})

	skips := []struct {
		name  string
		setup func(t *testing.T, e *env) int64
	}{
		{"disabled schedule", func(t *testing.T, e *env) int64 {
			id := e.schedule(t, e.web, "@daily")
			if err := e.st.SetScheduleEnabled(ctx, e.web.ID, id, false); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"all schedules of the stack disabled", func(t *testing.T, e *env) int64 {
			id := e.schedule(t, e.web, "@daily")
			if _, err := e.st.SetAllSchedulesEnabled(ctx, e.web.ID, false); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"deleted schedule", func(t *testing.T, e *env) int64 {
			id := e.schedule(t, e.web, "@daily")
			if err := e.st.DeleteSchedule(ctx, e.web.ID, id); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"backup already queued", func(t *testing.T, e *env) int64 {
			e.queue.err = store.ErrJobQueued
			return e.schedule(t, e.web, "@daily")
		}},
	}
	for _, tt := range skips {
		t.Run("skips "+tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.s.Fire(ctx, tt.setup(t, e))
			if got := e.queue.submitted(); len(got) != 0 {
				t.Errorf("submitted %+v, want nothing", got)
			}
			if audit, _ := e.st.ListAudit(ctx, 10); len(audit) != 0 {
				t.Errorf("audit = %+v, want nothing", audit)
			}
		})
	}
}

func TestRunAndReload(t *testing.T) {
	e := newEnv(t)
	e.schedule(t, e.web, "0 3 * * *")
	off := e.schedule(t, e.db, "@hourly")
	if err := e.st.SetScheduleEnabled(context.Background(), e.db.ID, off, false); err != nil {
		t.Fatal(err)
	}

	// Reload before Run does nothing.
	if err := e.s.Reload(context.Background()); err != nil || e.s.Active() != 0 {
		t.Fatalf("Reload before Run: active = %d, err = %v", e.s.Active(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.s.Run(ctx)
		close(done)
	}()
	waitActive(t, e.s, 1) // the disabled schedule is not loaded

	if err := e.st.SetScheduleEnabled(context.Background(), e.db.ID, off, true); err != nil {
		t.Fatal(err)
	}
	e.schedule(t, e.db, "@every 2h")
	if err := e.s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitActive(t, e.s, 3)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func waitActive(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.Active() != want {
		if time.Now().After(deadline) {
			t.Fatalf("active schedules = %d, want %d", s.Active(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
