package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
)

func TestImportedStack(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	stackID(t, s, "web")
	if _, err := s.CreateImportedStack(ctx, "web", "host-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("import over an existing stack: %v", err)
	}
	st, err := s.CreateImportedStack(ctx, "cloud", "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "cloud" || st.Present || st.SourceHost != "host-a" {
		t.Fatalf("imported stack %+v", st)
	}
	// Discovery does not know it yet (no directory): it stays not present.
	if err := s.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StackByName(ctx, "cloud"); got.Present || got.SourceHost != "host-a" {
		t.Fatalf("after sync %+v", got)
	}
	if err := s.SetStackPresent(ctx, st.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStackSourceHost(ctx, st.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StackByName(ctx, "cloud"); !got.Present || got.SourceHost != "" {
		t.Fatalf("after first restore %+v", got)
	}
	sums, _ := s.StackSummaries(ctx)
	if i := slices.IndexFunc(sums, func(x StackSummary) bool { return x.Name == "cloud" }); i < 0 || !sums[i].Present {
		t.Fatalf("summaries %+v", sums)
	}
}

func TestStackSnapshotHosts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	repo, err := s.SetRepository(ctx, web, "/repos/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa1", ShortID: "aaaaaaaa", Time: t0, Hostname: "host-b", Paths: []string{"/home/docker/web"}},
		{ID: "bbbbbbbb1", ShortID: "bbbbbbbb", Time: t0, Hostname: "host-a", Paths: []string{"/srv/stacks/web"}},
		{ID: "cccccccc1", ShortID: "cccccccc", Time: t0, Hostname: "host-a", Paths: []string{"/srv/stacks/web"}},
		{ID: "dddddddd1", ShortID: "dddddddd", Time: t0, Hostname: "host-c", Paths: []string{"/srv/stacks/db"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	hosts, err := s.StackSnapshotHosts(ctx, repo, "web")
	if err != nil || !slices.Equal(hosts, []string{"host-a", "host-b"}) {
		t.Fatalf("hosts = %v, %v", hosts, err)
	}
}

// Every running job counts against the limit, restores and imports
// included (issue #36: the "background" exception is gone; only the
// downtime phase has its own, smaller limit).
func TestRestoresCountAgainstLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db := stackID(t, s, "web"), stackID(t, s, "db")
	cloud, err := s.CreateImportedStack(ctx, "cloud", "host-a")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	a := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA1", web, jobs.TypeBackup, t0)
	imp := Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA2", StackID: cloud.ID, Type: jobs.TypeRestore, Trigger: TriggerManual, QueuedAt: t0,
		Options: JobOptions{SnapshotID: "aaaaaaaa", Import: true, LimitDownloadKiB: 2048}}
	if err := s.CreateJob(ctx, imp); err != nil {
		t.Fatal(err)
	}
	b := newJob(t, s, "01AAAAAAAAAAAAAAAAAAAAAAA3", db, jobs.TypeBackup, t0)
	claim := func() string {
		t.Helper()
		j, ok, err := s.ClaimNextJob(ctx, 2, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return j.ID
	}
	if got := claim(); got != a.ID {
		t.Fatalf("first = %q", got)
	}
	if got := claim(); got != imp.ID {
		t.Fatalf("second = %q, want the import", got)
	}
	if got := claim(); got != "" {
		t.Fatalf("third = %q, want nothing (limit 2)", got)
	}
	if qs, _ := s.QueueState(ctx, b); qs.Running != 2 {
		t.Fatalf("running = %d, want 2", qs.Running)
	}
	if err := s.FinishJob(ctx, imp.ID, jobs.StateSucceeded, "", "", JobResult{}, t0); err != nil {
		t.Fatal(err)
	}
	if got := claim(); got != b.ID {
		t.Fatalf("after the import = %q, want b", got)
	}
	j, _ := s.JobByID(ctx, imp.ID)
	if !j.Options.Import || j.Options.LimitDownloadKiB != 2048 {
		t.Fatalf("options %+v", j.Options)
	}
}
