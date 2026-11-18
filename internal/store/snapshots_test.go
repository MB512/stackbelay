package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

func snap(id string, ts time.Time, tags ...string) restic.Snapshot {
	return restic.Snapshot{ID: id, ShortID: id[:8], Time: ts, Hostname: "host-a", Paths: []string{"/s/web"}, Tags: tags}
}

func TestReplaceAndListSnapshots(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	repo, err := s.SetRepository(ctx, stackID(t, s, "web"), "/repos/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	a := snap("aaaaaaaa11111111", t0, "final")
	b := snap("bbbbbbbb22222222", t0.Add(time.Hour), "pre-stop")
	c := snap("cccccccc33333333", t0.Add(2*time.Hour))
	c.Tags = nil // stored as [] and read back as empty slice

	refreshed := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{a, b, c}, refreshed); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSnapshots(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != c.ID || got[2].ID != a.ID {
		t.Fatalf("not newest first: %+v", got)
	}
	if !got[2].Time.Equal(t0) || !reflect.DeepEqual(got[2].Paths, a.Paths) || !reflect.DeepEqual(got[2].Tags, a.Tags) ||
		got[2].Hostname != "host-a" || len(got[0].Tags) != 0 {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// b was pruned on the server, d is new.
	d := snap("dddddddd44444444", t0.Add(3*time.Hour), "final")
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{a, c, d}, refreshed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListSnapshots(ctx, repo)
	missing := map[string]bool{}
	for _, cs := range got {
		missing[cs.ID] = cs.Missing
	}
	want := map[string]bool{a.ID: false, b.ID: true, c.ID: false, d.ID: false}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing flags %v, want %v", missing, want)
	}

	r, _ := s.RepositoryForStack(ctx, stackID(t, s, "web"))
	if !r.SnapshotsRefreshedAt.Equal(refreshed.Add(time.Hour)) {
		t.Fatalf("refreshed at %v", r.SnapshotsRefreshedAt)
	}
}

func TestReplaceSnapshotsErrors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.ReplaceSnapshots(ctx, 999, nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown repo, got %v", err)
	}
	repo, _ := s.SetRepository(ctx, stackID(t, s, "web"), "/repos/web", "pw", nil)
	bad := restic.Snapshot{ID: "../../etc", ShortID: "x", Time: time.Now()}
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{bad}, time.Now()); err == nil {
		t.Fatal("invalid snapshot id accepted")
	}
}

func TestStackSnapshotsFilter(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	repo, err := s.SetRepository(ctx, web, "/repos/shared", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	mk := func(id string, i int, host string, paths ...string) restic.Snapshot {
		return restic.Snapshot{ID: id, ShortID: id[:8], Time: t0.Add(time.Duration(i) * time.Hour), Hostname: host, Paths: paths}
	}
	snaps := []restic.Snapshot{
		mk("aaaaaaaa11111111", 1, "host-a", "/s/web"),         // ours
		mk("bbbbbbbb22222222", 2, "host-b", "/other/web"),     // our stack, other host
		mk("cccccccc33333333", 3, "host-a", "/s/db"),          // other stack, shared repository
		mk("dddddddd44444444", 4, "host-a", "/s/myweb"),       // ends in "web", but is not "web"
		mk("eeeeeeee55555555", 5, "host-a", "/s/web", "/etc"), // two paths: not made by StackVault
	}
	if err := s.ReplaceSnapshots(ctx, repo, snaps, t0); err != nil {
		t.Fatal(err)
	}
	ids := func(cs []CachedSnapshot) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ShortID)
		}
		return strings.Join(out, ",")
	}
	got, err := s.StackSnapshots(ctx, repo, "web", "host-a")
	if err != nil || ids(got) != "aaaaaaaa" {
		t.Fatalf("web on host-a: %s %v", ids(got), err)
	}
	if got, _ := s.StackSnapshots(ctx, repo, "web", ""); ids(got) != "bbbbbbbb,aaaaaaaa" {
		t.Fatalf("web on all hosts: %s", ids(got))
	}
	if got, _ := s.ListSnapshots(ctx, repo); len(got) != 5 {
		t.Fatalf("all: %d", len(got))
	}
}

func TestSnapshotSummaryCached(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	repo, err := s.SetRepository(ctx, web, "/repos/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	sum := &restic.SnapshotSummary{BackupStart: t0, BackupEnd: t0.Add(42 * time.Second), DataAddedPacked: 1234,
		TotalBytesProcessed: 5 << 20, TotalFilesProcessed: 7}
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa1", ShortID: "aaaaaaaa", Time: t0, Hostname: "h", Paths: []string{"/s/web"}, Summary: sum},
		{ID: "bbbbbbbb1", ShortID: "bbbbbbbb", Time: t0.Add(-time.Hour), Hostname: "h", Paths: []string{"/s/web"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.StackSnapshots(ctx, repo, "web", "h")
	if err != nil || len(got) != 2 {
		t.Fatalf("snapshots %+v, %v", got, err)
	}
	if got[0].Summary == nil || *got[0].Summary != *sum || got[0].Summary.Duration() != 42*time.Second {
		t.Fatalf("summary %+v", got[0].Summary)
	}
	if got[1].Summary != nil {
		t.Fatalf("snapshot without summary got %+v", got[1].Summary)
	}
}
