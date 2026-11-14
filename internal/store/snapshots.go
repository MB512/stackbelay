package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

// CachedSnapshot is a snapshot from the cache.
type CachedSnapshot struct {
	restic.Snapshot // embedded: its fields (ID, Time, Paths, ...) are promoted
	// Missing is true if the snapshot was not in the repository at the last
	// refresh (pruned by the repository owner). It cannot be restored.
	Missing bool
}

// ReplaceSnapshots stores the result of a "restic snapshots" refresh for a
// repository: listed snapshots are inserted/updated, cached snapshots that
// are no longer listed are marked missing (not deleted, so job history can
// still show them), and the refresh time is recorded.
func (s *Store) ReplaceSnapshots(ctx context.Context, repoID int64, snaps []restic.Snapshot, refreshedAt time.Time) error {
	for _, sn := range snaps {
		if !restic.SnapshotIDPattern.MatchString(sn.ID) {
			return fmt.Errorf("refusing to cache snapshot with invalid id %q", sn.ID)
		}
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE repositories SET snapshots_refreshed_at = ? WHERE id = ?`,
			formatTime(refreshedAt), repoID)
		if err != nil {
			return err
		}
		if err := expectOneRow(res, fmt.Sprintf("repository %d", repoID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET missing = 1 WHERE repo_id = ?`, repoID); err != nil {
			return err
		}
		for _, sn := range snaps {
			paths, err := json.Marshal(nonNil(sn.Paths))
			if err != nil {
				return err
			}
			tags, err := json.Marshal(nonNil(sn.Tags))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO snapshots (repo_id, snapshot_id, short_id, time, hostname, paths, tags, missing)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 0)
				 ON CONFLICT (repo_id, snapshot_id) DO UPDATE SET
				   short_id = excluded.short_id, time = excluded.time, hostname = excluded.hostname,
				   paths = excluded.paths, tags = excluded.tags, missing = 0`,
				repoID, sn.ID, sn.ShortID, formatTime(sn.Time), sn.Hostname, string(paths), string(tags)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("caching snapshots of repository %d: %w", repoID, err)
	}
	return nil
}

// ListSnapshots returns all cached snapshots of a repository, newest
// first, including missing ones and those of other stacks or hosts.
func (s *Store) ListSnapshots(ctx context.Context, repoID int64) ([]CachedSnapshot, error) {
	return s.querySnapshots(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE repo_id = ? ORDER BY time DESC, snapshot_id`, repoID)
}

// StackSnapshots returns the cached snapshots of one stack from one host,
// newest first, including missing ones. A repository may be shared by
// several stacks and hosts; a snapshot belongs to a stack if its single
// path ends in "/<stack>" (restic records the backed-up path in every
// snapshot), and to a host by restic's --host. An empty host matches all
// hosts.
func (s *Store) StackSnapshots(ctx context.Context, repoID int64, stack, host string) ([]CachedSnapshot, error) {
	// Numbered parameters: ?1 repository, ?2 "/<stack>", ?3 host.
	return s.querySnapshots(ctx, `SELECT `+snapshotColumns+` FROM snapshots
		 WHERE repo_id = ?1 AND json_array_length(paths) = 1
		   AND substr(json_extract(paths, '$[0]'), -length(?2)) = ?2
		   AND (?3 = '' OR hostname = ?3)
		 ORDER BY time DESC, snapshot_id`, repoID, "/"+stack, host)
}

const snapshotColumns = `snapshot_id, short_id, time, hostname, paths, tags, missing`

func (s *Store) querySnapshots(ctx context.Context, query string, args ...any) ([]CachedSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	defer rows.Close()
	var out []CachedSnapshot
	for rows.Next() {
		var cs CachedSnapshot
		var ts, paths, tags string
		if err := rows.Scan(&cs.ID, &cs.ShortID, &ts, &cs.Hostname, &paths, &tags, &cs.Missing); err != nil {
			return nil, fmt.Errorf("listing snapshots: %w", err)
		}
		if cs.Time, err = parseTime(ts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &cs.Paths); err != nil {
			return nil, fmt.Errorf("decoding snapshot paths: %w", err)
		}
		if err := json.Unmarshal([]byte(tags), &cs.Tags); err != nil {
			return nil, fmt.Errorf("decoding snapshot tags: %w", err)
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// nonNil makes sure a nil slice is stored as [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
