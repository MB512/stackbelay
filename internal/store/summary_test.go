package store

import (
	"context"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
)

func TestSummarySettingsAndSentAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ns, err := s.NotifySettings(ctx)
	if err != nil || ns.SummaryCron != "0 7 * * *" || ns.StaleDays != 2 {
		t.Fatalf("defaults: %+v, %v", ns, err)
	}
	ns.SummaryCron, ns.StaleDays = "0 8 * * 1", 8
	if err := s.SaveNotifySettings(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NotifySettings(ctx); got.SummaryCron != "0 8 * * 1" || got.StaleDays != 8 {
		t.Fatalf("saved: %+v", got)
	}

	if at, err := s.SummarySentAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("never sent: %v, %v", at, err)
	}
	t0 := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	if err := s.SetSummarySentAt(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if at, _ := s.SummarySentAt(ctx); !at.Equal(t0) {
		t.Fatalf("sent at %v", at)
	}
}

func TestSummaryPeriodQueries(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	from := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)

	// Jobs: one before, two inside, one running.
	finish := func(id string, stack int64, typ jobs.Type, state jobs.State, at time.Time) {
		t.Helper()
		newJob(t, s, id, stack, typ, at.Add(-time.Minute))
		if err := s.FinishJob(ctx, id, state, "", "", JobResult{}, at); err != nil {
			t.Fatal(err)
		}
	}
	finish("01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, jobs.StateSucceeded, from.Add(-time.Hour))
	finish("01AAAAAAAAAAAAAAAAAAAAAAA2", web, jobs.TypeBackup, jobs.StateFailed, from.Add(time.Hour))
	finish("01AAAAAAAAAAAAAAAAAAAAAAA3", db, jobs.TypeStop, jobs.StateSucceeded, from.Add(2*time.Hour))
	newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA4", db, jobs.TypeBackup, from.Add(3*time.Hour))
	got, err := s.JobsFinishedBetween(ctx, from, to)
	if err != nil || len(got) != 2 || got[0].ID != "01AAAAAAAAAAAAAAAAAAAAAAA2" || got[1].StackName != "db" {
		t.Fatalf("jobs: %+v, %v", got, err)
	}

	// Stopped on purpose: db's newest operation is a succeeded stop.
	if m, err := s.StoppedOnPurpose(ctx); err != nil || !m[db] || m[web] {
		t.Fatalf("stopped on purpose: %v, %v", m, err)
	}
	finish("01AAAAAAAAAAAAAAAAAAAAAAA5", db, jobs.TypeStart, jobs.StateSucceeded, from.Add(4*time.Hour))
	if m, _ := s.StoppedOnPurpose(ctx); m[db] {
		t.Fatal("started again: not stopped on purpose")
	}

	// Audit entries of the period.
	for _, at := range []time.Time{from.Add(-time.Minute), from.Add(time.Minute), to} {
		if err := s.AddAudit(ctx, AuditEntry{At: at, Action: "login.failure", Detail: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if a, err := s.AuditBetween(ctx, from, to); err != nil || len(a) != 1 || !a[0].At.Equal(from.Add(time.Minute)) {
		t.Fatalf("audit: %+v, %v", a, err)
	}

	// Backup schedules: only enabled backup schedules count.
	if _, err := s.CreateSchedule(ctx, Schedule{StackID: web, CronExpr: "0 3 * * *", Type: jobs.TypeBackup, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, Schedule{StackID: db, CronExpr: "0 4 * * *", Type: jobs.TypeUpdate, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.StacksWithBackupSchedule(ctx); err != nil || !m[web] || m[db] {
		t.Fatalf("schedules: %v, %v", m, err)
	}

	// Data added: this host's snapshots of the period with a summary.
	repo, err := s.CreateRepository(ctx, "nas-2026", "/srv/restic", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := func(id, host string, at time.Time, added uint64, withSummary bool) restic.Snapshot {
		sn := restic.Snapshot{ID: id + "00000000", ShortID: id, Time: at, Hostname: host, Paths: []string{"/home/docker/web"}}
		if withSummary {
			sn.Summary = &restic.SnapshotSummary{DataAddedPacked: added}
		}
		return sn
	}
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		snap("aaaaaaaa", "nas01", from.Add(time.Hour), 100, true),
		snap("bbbbbbbb", "nas01", from.Add(2*time.Hour), 50, true),
		snap("cccccccc", "other", from.Add(time.Hour), 999, true),
		snap("dddddddd", "nas01", from.Add(-time.Hour), 999, true),
		snap("eeeeeeee", "nas01", from.Add(time.Hour), 0, false),
	}, to); err != nil {
		t.Fatal(err)
	}
	added, err := s.DataAddedBetween(ctx, from, to, "nas01")
	if err != nil || len(added) != 2 || added[0] != (AddedData{"nas-2026", "web", 100}) || added[1].Bytes != 50 {
		t.Fatalf("added: %+v, %v", added, err)
	}
}
