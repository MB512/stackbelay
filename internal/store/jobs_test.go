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
		Trigger: TriggerManual, RequestedBy: uid, QueuedAt: t0, Options: JobOptions{PreStop: true}}); err != nil {
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
		!j.Options.PreStop || !j.QueuedAt.Equal(t0) || !j.StartedAt.IsZero() {
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
