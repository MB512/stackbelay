package snapshots

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/secrets"
	"github.com/MB512/stackvault/internal/store"
)

// fakeRestic implements restic.Runner. Embedding the interface satisfies all
// of its methods; only Snapshots is overridden. Calling any other method
// would panic (nil interface), which is what we want in this test.
type fakeRestic struct {
	restic.Runner
	snaps []restic.Snapshot
	err   error
	calls int
	repo  restic.Repo
}

func (f *fakeRestic) Snapshots(ctx context.Context, repo restic.Repo, ids ...string) ([]restic.Snapshot, error) {
	f.calls++
	f.repo = repo
	return f.snaps, f.err
}

func setup(t *testing.T) (*Cache, *fakeRestic, int64, *time.Time) {
	t.Helper()
	ctx := context.Background()
	box, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	web, _ := st.StackByName(ctx, "web")
	if _, err := st.SetRepository(ctx, web.ID, "/repos/web", "pw", map[string]string{"AWS_ACCESS_KEY_ID": "ak"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	f := &fakeRestic{snaps: []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: now.Add(-time.Hour), Hostname: "h", Paths: []string{"/s/web"}},
	}}
	return &Cache{Store: st, Restic: f, Now: func() time.Time { return now }}, f, web.ID, &now
}

func TestRefreshUsesDecryptedCredentials(t *testing.T) {
	c, f, web, _ := setup(t)
	snaps, err := c.Refresh(context.Background(), web)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].ID != "aaaaaaaa11111111" {
		t.Fatalf("unexpected snapshots %+v", snaps)
	}
	if f.repo.Location != "/repos/web" || !slices.Contains(f.repo.Env, "RESTIC_PASSWORD=pw") ||
		!slices.Contains(f.repo.Env, "AWS_ACCESS_KEY_ID=ak") {
		t.Fatalf("restic called with %+v", f.repo)
	}
}

func TestGetUsesCacheWhileFresh(t *testing.T) {
	c, f, web, now := setup(t)
	ctx := context.Background()

	// Never refreshed: refreshes.
	_, at, err := c.Get(ctx, web, 5*time.Minute)
	if err != nil || f.calls != 1 || !at.Equal(*now) {
		t.Fatalf("first Get: calls=%d at=%v err=%v", f.calls, at, err)
	}
	// Fresh: served from cache, even if restic would now fail.
	f.err = errors.New("network down")
	*now = now.Add(4 * time.Minute)
	snaps, at, err := c.Get(ctx, web, 5*time.Minute)
	if err != nil || f.calls != 1 || len(snaps) != 1 || !at.Equal(now.Add(-4*time.Minute)) {
		t.Fatalf("cached Get: calls=%d at=%v snaps=%d err=%v", f.calls, at, len(snaps), err)
	}
	// Stale: refreshes, and the restic error is returned.
	*now = now.Add(2 * time.Minute)
	if _, _, err := c.Get(ctx, web, 5*time.Minute); err == nil || f.calls != 2 {
		t.Fatalf("stale Get: calls=%d err=%v", f.calls, err)
	}
	// maxAge 0 always refreshes.
	f.err = nil
	if _, _, err := c.Get(ctx, web, 0); err != nil || f.calls != 3 {
		t.Fatalf("forced Get: calls=%d err=%v", f.calls, err)
	}
}

func TestRefreshWithoutRepository(t *testing.T) {
	c, f, _, _ := setup(t)
	if _, err := c.Refresh(context.Background(), 999); !errors.Is(err, store.ErrNotFound) || f.calls != 0 {
		t.Fatalf("want ErrNotFound without restic call, got %v (calls %d)", err, f.calls)
	}
}
