package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
)

func newJob(t *testing.T, s *Store, id string, stackID int64, typ jobs.Type, at time.Time) Job {
	t.Helper()
	j := Job{ID: id, StackID: stackID, Type: typ, Trigger: TriggerManual, QueuedAt: at}
	if typ == jobs.TypeRestore {
		j.Options.SnapshotID = "aaaaaaaa11111111"
	}
	if err := s.CreateJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestJobQueueAndPerStackLock(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	uid, _ := s.CreateUser(ctx, User{Username: "alice", PasswordHash: "h"})
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	// Ids sort by time, like real ULIDs.
	if err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", StackID: web, Type: jobs.TypeBackup,
		Trigger: TriggerManual, RequestedBy: uid, QueuedAt: t0, Options: JobOptions{OnlyIfNew: true}}); err != nil {
		t.Fatal(err)
	}
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeRestore, t0.Add(time.Second))
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA3", db, jobs.TypeBackup, t0.Add(2*time.Second))

	// A second queued backup of the same stack is refused.
	err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA4", StackID: web, Type: jobs.TypeBackup, Trigger: TriggerSchedule, QueuedAt: t0})
	if !errors.Is(err, ErrJobQueued) {
		t.Fatalf("duplicate queued backup: want ErrJobQueued, got %v", err)
	}

	j, err := s.JobByID(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1")
	if err != nil || j.StackName != "web" || j.Requester != "alice" || j.State != jobs.StateQueued ||
		!j.Options.OnlyIfNew || !j.QueuedAt.Equal(t0) || !j.StartedAt.IsZero() {
		t.Fatalf("JobByID: %+v, %v", j, err)
	}

	// Claim: oldest first, max 2 running, never two on one stack.
	j1, ok, err := s.ClaimNextJob(ctx, 2, t0.Add(time.Minute))
	if err != nil || !ok || j1.ID != "01AAAAAAAAAAAAAAAAAAAAAAA1" || j1.State != jobs.StateRunning || j1.StartedAt.IsZero() {
		t.Fatalf("claim 1: %+v %v %v", j1, ok, err)
	}
	j2, ok, err := s.ClaimNextJob(ctx, 2, t0.Add(time.Minute))
	if err != nil || !ok || j2.ID != "01AAAAAAAAAAAAAAAAAAAAAAA3" {
		t.Fatalf("claim 2 must skip the locked stack: %+v %v %v", j2, ok, err)
	}
	if _, ok, _ := s.ClaimNextJob(ctx, 5, t0); ok {
		t.Fatal("claimed a job on a stack with a running job")
	}
	// The database refuses a second running job on a stack even if asked directly.
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = 'running' WHERE id = '01AAAAAAAAAAAAAAAAAAAAAAA2'`); err == nil {
		t.Fatal("unique index did not prevent a second running job")
	}

	// Finish the first: now the restore on "web" can start (limit 2, one running).
	if err := s.FinishJob(ctx, j1.ID, jobs.StateSucceeded, "bbbbbbbb22222222", "",
		JobResult{PreStopSnapshot: "cccccccc33333333"}, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, j1.ID, jobs.StateFailed, "", "again", JobResult{}, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finishing twice: want ErrNotFound, got %v", err)
	}
	if err := s.FinishJob(ctx, j2.ID, jobs.StateRunning, "", "", JobResult{}, t0); err == nil {
		t.Fatal("non-final state accepted")
	}
	j1, _ = s.JobByID(ctx, j1.ID)
	if j1.State != jobs.StateSucceeded || j1.Snapshot != "bbbbbbbb22222222" || j1.Result.PreStopSnapshot != "cccccccc33333333" || j1.FinishedAt.IsZero() {
		t.Fatalf("finished job: %+v", j1)
	}
	j3, ok, err := s.ClaimNextJob(ctx, 2, t0.Add(3*time.Minute))
	if err != nil || !ok || j3.ID != "01AAAAAAAAAAAAAAAAAAAAAAA2" {
		t.Fatalf("claim 3: %+v %v %v", j3, ok, err)
	}
	if _, ok, _ := s.ClaimNextJob(ctx, 2, t0); ok {
		t.Fatal("limit of 2 running jobs exceeded")
	}

	// A queued job can be cancelled without having run.
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA5", web, jobs.TypeBackup, t0.Add(4*time.Minute))
	if err := s.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA5", jobs.StateCancelled, "", "cancelled by alice", JobResult{}, t0); err != nil {
		t.Fatal(err)
	}

	running, err := s.JobsInState(ctx, jobs.StateRunning)
	if err != nil || len(running) != 2 || running[0].ID != j3.ID || running[1].ID != j2.ID {
		t.Fatalf("running: %+v %v", running, err)
	}
	list, err := s.ListJobs(ctx, web, 10)
	if err != nil || len(list) != 3 || list[0].ID != "01AAAAAAAAAAAAAAAAAAAAAAA5" {
		t.Fatalf("ListJobs(web): %+v %v", list, err)
	}
	if all, _ := s.ListJobs(ctx, 0, 2); len(all) != 2 {
		t.Fatalf("ListJobs(all, 2): %d", len(all))
	}
	if _, err := s.JobByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
	if err := s.CreateJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA6", StackID: 999, Type: jobs.TypeBackup, Trigger: TriggerManual, QueuedAt: t0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown stack: %v", err)
	}
}

func TestJobComposeContent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	content := []byte("services:\n  app:\n    image: nginx:1.27\n")
	j := Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", StackID: web, Type: jobs.TypeComposeApply, Trigger: TriggerManual,
		QueuedAt: time.Now(), ComposeContent: content,
		Options: JobOptions{ComposeFile: "compose.yml", ComposeSHA256: "abc", ComposeBaseSHA256: "def"}}
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := s.JobComposeContent(ctx, j.ID)
	if err != nil || string(got) != string(content) {
		t.Fatalf("content %q, %v", got, err)
	}
	loaded, err := s.JobByID(ctx, j.ID)
	if err != nil || loaded.Options.ComposeFile != "compose.yml" || loaded.ComposeContent != nil {
		t.Fatalf("job %+v, %v", loaded, err)
	}
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeBackup, time.Now())
	if _, err := s.JobComposeContent(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job without content: %v", err)
	}
}

func TestQueueState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, t0)
	b := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeRestore, t0.Add(time.Second))
	c := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA3", db, jobs.TypeBackup, t0.Add(2*time.Second))
	d := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA4", web, jobs.TypeUpdate, t0.Add(3*time.Second))

	// Nothing runs: b waits for the older queued job a of its stack.
	qs, err := s.QueueState(ctx, b)
	if err != nil || qs.Blocking == nil || qs.Blocking.ID != a.ID || qs.Ahead != 1 || qs.Running != 0 {
		t.Fatalf("b: %+v %v", qs, err)
	}
	// The oldest job has nothing before it.
	if qs, _ := s.QueueState(ctx, a); qs.Blocking != nil || qs.Ahead != 0 {
		t.Fatalf("a: %+v", qs)
	}
	// a runs: d waits for the running job, not for the queued b.
	if _, ok, err := s.ClaimNextJob(ctx, 1, t0); !ok || err != nil {
		t.Fatal("claim")
	}
	if qs, _ := s.QueueState(ctx, d); qs.Blocking == nil || qs.Blocking.ID != a.ID || qs.Ahead != 2 || qs.Running != 1 {
		t.Fatalf("d: %+v", qs)
	}
	// c (db) only waits for the global limit.
	if qs, _ := s.QueueState(ctx, c); qs.Blocking != nil || qs.Ahead != 1 || qs.Running != 1 {
		t.Fatalf("c: %+v", qs)
	}
}

func TestStackSummariesLastJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	t0 := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	addResult := func(id string, stack int64, typ jobs.Type, state jobs.State, queued, started, finished time.Time, res JobResult) {
		t.Helper()
		if err := s.ImportJob(ctx, Job{ID: id, StackID: stack, Type: typ, Trigger: TriggerSchedule, State: state,
			QueuedAt: queued, StartedAt: started, FinishedAt: finished, Result: res}, ""); err != nil {
			t.Fatal(err)
		}
	}
	add := func(id string, stack int64, typ jobs.Type, state jobs.State, queued, started, finished time.Time) {
		t.Helper()
		addResult(id, stack, typ, state, queued, started, finished, JobResult{})
	}
	h := time.Hour
	add("01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, jobs.StateSucceeded, t0, t0, t0.Add(10*time.Minute))
	add("01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeBackup, jobs.StateFailed, t0.Add(24*h), t0.Add(24*h), t0.Add(25*h))
	// Newer jobs of other types do not count.
	add("01AAAAAAAAAAAAAAAAAAAAAAA3", web, jobs.TypeUpdate, jobs.StateSucceeded, t0.Add(48*h), t0.Add(48*h), t0.Add(49*h))
	// A running backup shows its start.
	add("01AAAAAAAAAAAAAAAAAAAAAAA4", db, jobs.TypeBackup, jobs.StateRunning, t0, t0.Add(h), time.Time{})
	// Updates of web: only the one that changed an image is a real update;
	// the newest update job (failed) is the last check.
	addResult("01AAAAAAAAAAAAAAAAAAAAAAA5", web, jobs.TypeUpdate, jobs.StateSucceeded, t0.Add(50*h), t0.Add(50*h), t0.Add(51*h),
		JobResult{ImagesChanged: []string{"app"}})
	addResult("01AAAAAAAAAAAAAAAAAAAAAAA6", web, jobs.TypeUpdate, jobs.StateSucceeded, t0.Add(60*h), t0.Add(60*h), t0.Add(61*h),
		JobResult{Unchanged: true})
	add("01AAAAAAAAAAAAAAAAAAAAAAA7", web, jobs.TypeUpdate, jobs.StateFailed, t0.Add(70*h), t0.Add(70*h), t0.Add(71*h))

	sums, err := s.StackSummaries(ctx)
	if err != nil || len(sums) != 2 {
		t.Fatalf("StackSummaries: %+v, %v", sums, err)
	}
	d, w := sums[0], sums[1]
	if want := (JobBrief{ID: "01AAAAAAAAAAAAAAAAAAAAAAA2", State: jobs.StateFailed, Time: t0.Add(25 * h)}); w.LastBackup != want {
		t.Errorf("web last backup: %+v, want %+v", w.LastBackup, want)
	}
	if !w.LastSuccessfulBackup.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("web last successful backup: %v", w.LastSuccessfulBackup)
	}
	if want := (JobBrief{ID: "01AAAAAAAAAAAAAAAAAAAAAAA4", State: jobs.StateRunning, Time: t0.Add(h)}); d.LastBackup != want {
		t.Errorf("db last backup: %+v, want %+v", d.LastBackup, want)
	}
	if !d.LastSuccessfulBackup.IsZero() {
		t.Errorf("db last successful backup: %v", d.LastSuccessfulBackup)
	}
	if want := (JobBrief{ID: "01AAAAAAAAAAAAAAAAAAAAAAA7", State: jobs.StateFailed, Time: t0.Add(71 * h)}); w.LastUpdate != want {
		t.Errorf("web last update: %+v, want %+v", w.LastUpdate, want)
	}
	if !w.LastImageUpdate.Equal(t0.Add(51 * h)) {
		t.Errorf("web last image update: %v", w.LastImageUpdate)
	}
	// db has no update jobs.
	if d.LastUpdate.ID != "" || !d.LastImageUpdate.IsZero() {
		t.Errorf("db updates: %+v %v", d.LastUpdate, d.LastImageUpdate)
	}
}

func TestStackSummariesActiveSchedules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	for _, typ := range []jobs.Type{jobs.TypeBackup, jobs.TypeUpdate} {
		if _, err := s.CreateSchedule(ctx, Schedule{StackID: web, CronExpr: "0 3 * * *", Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	off, err := s.CreateSchedule(ctx, Schedule{StackID: web, CronExpr: "0 4 * * *", Type: jobs.TypeBackup})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetScheduleEnabled(ctx, web, off, false); err != nil {
		t.Fatal(err)
	}
	sums, err := s.StackSummaries(ctx)
	if err != nil || len(sums) != 1 || sums[0].ActiveSchedules != 2 {
		t.Fatalf("StackSummaries (2 enabled schedules): %+v, %v", sums, err)
	}
}
