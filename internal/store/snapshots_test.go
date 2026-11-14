package store

import (
	"context"
	"errors"
	"reflect"
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
