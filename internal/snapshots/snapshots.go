// Package snapshots keeps the snapshot cache of a stack's repository up to
// date. Refreshing is a read-only "restic snapshots" call that the manager
// runs directly (not through a worker container), see docs/architecture.md.
package snapshots

import (
	"context"
	"fmt"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
)

// Cache reads and refreshes cached snapshot lists.
type Cache struct {
	Store  *store.Store
	Restic restic.Runner
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

func (c *Cache) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Refresh lists the snapshots in the stack's repository, updates the cache
// and returns the cached list (newest first, including missing snapshots).
func (c *Cache) Refresh(ctx context.Context, stackID int64) ([]store.CachedSnapshot, error) {
	repo, err := c.Store.RepositoryForStack(ctx, stackID)
	if err != nil {
		return nil, err
	}
	at := c.now()
	snaps, err := c.Restic.Snapshots(ctx, repo.Restic())
	if err != nil {
		return nil, fmt.Errorf("listing snapshots for stack %d: %w", stackID, err)
	}
	if err := c.Store.ReplaceSnapshots(ctx, repo.ID, snaps, at); err != nil {
		return nil, err
	}
	return c.Store.ListSnapshots(ctx, repo.ID)
}

// Get returns the cached snapshots and the time of the last refresh. If the
// cache has never been refreshed or is older than maxAge, it is refreshed
// first. maxAge <= 0 always refreshes.
func (c *Cache) Get(ctx context.Context, stackID int64, maxAge time.Duration) ([]store.CachedSnapshot, time.Time, error) {
	repo, err := c.Store.RepositoryForStack(ctx, stackID)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !repo.SnapshotsRefreshedAt.IsZero() && maxAge > 0 && c.now().Sub(repo.SnapshotsRefreshedAt) < maxAge {
		snaps, err := c.Store.ListSnapshots(ctx, repo.ID)
		return snaps, repo.SnapshotsRefreshedAt, err
	}
	at := c.now()
	snaps, err := c.Refresh(ctx, stackID)
	if err != nil {
		return nil, time.Time{}, err
	}
	// The store keeps second precision; report what it stored.
	return snaps, at.UTC().Truncate(time.Second), nil
}
