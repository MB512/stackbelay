package store

import (
	"context"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

func TestStackHealthAndStackEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	t0 := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)

	h := StackHealth{StackID: web, State: "started", Since: t0, Candidate: "degraded", CandidateSince: t0.Add(time.Minute),
		CandidateCount: 2, LastJobID: "01AAAAAAAAAAAAAAAAAAAAAAA1"}
	if err := s.SaveStackHealth(ctx, h); err != nil {
		t.Fatal(err)
	}
	h.Alerted, h.State, h.Candidate, h.CandidateSince, h.CandidateCount = true, "degraded", "", time.Time{}, 0
	if err := s.SaveStackHealth(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := s.StackHealths(ctx)
	if err != nil || len(got) != 1 || got[web] != h {
		t.Fatalf("health: %+v, %v", got, err)
	}

	// Stack events and job events share the outbox, in order.
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA1", db, jobs.TypeBackup, t0)
	if err := s.AddStackEvent(ctx, "stack.degraded", web, `{"problems":["db is not running"]}`, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1", jobs.StateFailed, "", "x", JobResult{}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStackEvent(ctx, "docker.unreadable", 0, `{}`, t0); err != nil {
		t.Fatal(err)
	}
	evs, err := s.NotifyEvents(ctx)
	if err != nil || len(evs) != 3 {
		t.Fatalf("events: %+v, %v", evs, err)
	}
	if e := evs[0]; e.Kind != "stack.degraded" || e.StackID != web || e.StackName != "web" || e.Job.ID != "" {
		t.Errorf("stack event: %+v", e)
	}
	if e := evs[1]; e.Kind != "" || e.Job.ID != "01AAAAAAAAAAAAAAAAAAAAAAA1" || e.StackID != db {
		t.Errorf("job event: %+v", e)
	}
	if e := evs[2]; e.Kind != "docker.unreadable" || e.StackID != 0 || e.StackName != "" {
		t.Errorf("host event: %+v", e)
	}
	if err := s.DeleteNotifyEvents(ctx, []int64{evs[0].ID, evs[1].ID, evs[2].ID}); err != nil {
		t.Fatal(err)
	}

	latest, err := s.LatestFinishedJobs(ctx)
	if err != nil || latest[db].ID != "01AAAAAAAAAAAAAAAAAAAAAAA1" || len(latest) != 1 {
		t.Fatalf("latest: %+v, %v", latest, err)
	}
}
