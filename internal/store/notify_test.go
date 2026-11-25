package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

func TestNotifySettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	ns, err := s.NotifySettings(ctx)
	if err != nil || ns.ServerURL != "https://ntfy.sh" || ns.Topic != "" || ns.Auth != (NotifyAuth{}) || len(ns.Events) != 0 {
		t.Fatalf("defaults: %+v, %v", ns, err)
	}

	want := NotifySettings{ServerURL: "https://ntfy.example.com", Topic: "backups",
		Auth: NotifyAuth{Token: "tk_secret"}, Events: map[string]bool{"backup.succeeded": true}}
	if err := s.SaveNotifySettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.NotifySettings(ctx)
	if err != nil || got.ServerURL != want.ServerURL || got.Topic != want.Topic || got.Auth != want.Auth ||
		!got.Events["backup.succeeded"] {
		t.Fatalf("saved: %+v, %v", got, err)
	}
	var enc []byte
	if err := s.db.QueryRowContext(ctx, `SELECT auth_enc FROM notify_settings`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 || string(enc) == "tk_secret" {
		t.Fatalf("credentials not sealed: %q", enc)
	}

	// Removing the credentials stores NULL.
	want.Auth = NotifyAuth{}
	if err := s.SaveNotifySettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.NotifySettings(ctx); err != nil || got.Auth != (NotifyAuth{}) {
		t.Fatalf("without auth: %+v, %v", got, err)
	}
}

func TestNotifyEventsFromFinishedJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	t0 := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)

	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, t0)
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeRestore, t0)
	if evs, _ := s.NotifyEvents(ctx); len(evs) != 0 {
		t.Fatalf("queued jobs must not have events: %+v", evs)
	}

	if err := s.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1", jobs.StateFailed, "", "repository not reachable: x",
		JobResult{}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Finishing twice fails and adds no second event.
	if err := s.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1", jobs.StateFailed, "", "", JobResult{}, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second finish: %v", err)
	}
	// An imported final job gets an event, an imported running one does not.
	if err := s.ImportJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA3", StackID: web, Type: jobs.TypeUpdate,
		Trigger: TriggerSchedule, State: jobs.StateSucceeded, QueuedAt: t0, FinishedAt: t0.Add(2 * time.Minute)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportJob(ctx, Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA4", StackID: web, Type: jobs.TypeBackup,
		Trigger: TriggerSchedule, State: jobs.StateRunning, QueuedAt: t0}, ""); err != nil {
		t.Fatal(err)
	}

	evs, err := s.NotifyEvents(ctx)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %+v, %v", evs, err)
	}
	if e := evs[0]; e.Job.ID != "01AAAAAAAAAAAAAAAAAAAAAAA1" || e.Job.StackName != "web" || e.Job.State != jobs.StateFailed ||
		e.Job.Error != "repository not reachable: x" || !e.CreatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("event 1: %+v", e)
	}
	if e := evs[1]; e.Job.ID != "01AAAAAAAAAAAAAAAAAAAAAAA3" || e.Job.Type != jobs.TypeUpdate {
		t.Fatalf("event 2: %+v", e)
	}

	if err := s.DeleteNotifyEvents(ctx, []int64{evs[0].ID, evs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.NotifyEvents(ctx); len(evs) != 0 {
		t.Fatalf("after delete: %+v", evs)
	}
}

func TestStackNotifyMuted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	if err := s.SetStackNotifyMuted(ctx, web, true); err != nil {
		t.Fatal(err)
	}
	m, err := s.MutedStacks(ctx)
	if err != nil || !m[web] || m[db] {
		t.Fatalf("muted: %v, %v", m, err)
	}
	if err := s.SetStackNotifyMuted(ctx, web, false); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MutedStacks(ctx); len(m) != 0 {
		t.Fatalf("after unmute: %v", m)
	}
	if err := s.SetStackNotifyMuted(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown stack: %v", err)
	}
}
