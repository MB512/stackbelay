package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
)

// Queries of the notification summary (issue #48). A period is [from, to).
// Times are stored as RFC 3339 UTC strings of fixed length, so they
// compare as text.

// SummarySentAt returns when the last summary was sent; zero if never.
func (s *Store) SummarySentAt(ctx context.Context) (time.Time, error) {
	var at sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT summary_sent_at FROM notify_settings WHERE id = 1`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("loading summary time: %w", err)
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return parseTime(at.String)
}

// SetSummarySentAt records when the last summary was sent (the start of
// the next period).
func (s *Store) SetSummarySentAt(ctx context.Context, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_settings SET summary_sent_at = ? WHERE id = 1`, formatTime(at)); err != nil {
		return fmt.Errorf("saving summary time: %w", err)
	}
	return nil
}

// JobsFinishedBetween returns the jobs that finished in the period, oldest
// first.
func (s *Store) JobsFinishedBetween(ctx context.Context, from, to time.Time) ([]Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+
		`WHERE j.finished_at >= ? AND j.finished_at < ? ORDER BY j.finished_at, j.id`, formatTime(from), formatTime(to))
}

// AuditBetween returns the audit entries of the period, oldest first.
func (s *Store) AuditBetween(ctx context.Context, from, to time.Time) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+auditColumns+auditFrom+
		`WHERE a.at >= ? AND a.at < ? ORDER BY a.id`, formatTime(from), formatTime(to))
	if err != nil {
		return nil, fmt.Errorf("listing audit log: %w", err)
	}
	return scanAudit(rows)
}

// AddedData is what one snapshot added to a repository.
type AddedData struct {
	Repository string // name
	Stack      string // last component of the snapshot path
	Bytes      uint64 // restic's data_added_packed
}

// DataAddedBetween returns, per snapshot of host taken in the period, what
// it added to its repository (from the snapshot cache). Snapshots without
// a backup summary (restic before 0.17) are left out.
func (s *Store) DataAddedBetween(ctx context.Context, from, to time.Time, host string) ([]AddedData, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.name, sn.paths, sn.summary FROM snapshots sn JOIN repositories r ON r.id = sn.repo_id
		 WHERE sn.time >= ? AND sn.time < ? AND sn.hostname = ? AND sn.summary IS NOT NULL
		 ORDER BY sn.time`, formatTime(from), formatTime(to), host)
	if err != nil {
		return nil, fmt.Errorf("listing added data: %w", err)
	}
	defer rows.Close()
	var out []AddedData
	for rows.Next() {
		var repo, pathsJSON, summaryJSON string
		if err := rows.Scan(&repo, &pathsJSON, &summaryJSON); err != nil {
			return nil, fmt.Errorf("listing added data: %w", err)
		}
		var paths []string
		var sum restic.SnapshotSummary
		if json.Unmarshal([]byte(pathsJSON), &paths) != nil || len(paths) == 0 ||
			json.Unmarshal([]byte(summaryJSON), &sum) != nil {
			continue // not a snapshot of a stack directory
		}
		out = append(out, AddedData{Repository: repo, Stack: path.Base(paths[0]), Bytes: sum.DataAddedPacked})
	}
	return out, rows.Err()
}

// StacksWithBackupSchedule returns the ids of the stacks that have an
// enabled backup schedule.
func (s *Store) StacksWithBackupSchedule(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT stack_id FROM schedules WHERE enabled = 1 AND job_type = ? AND stack_id IS NOT NULL`, jobs.TypeBackup)
	if err != nil {
		return nil, fmt.Errorf("listing backup schedules: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing backup schedules: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// StoppedOnPurpose returns the ids of the stacks whose newest stop, start,
// restart, up or down job is a succeeded stop or down: a user stopped them
// on purpose.
func (s *Store) StoppedOnPurpose(ctx context.Context) (map[int64]bool, error) {
	ops := []any{jobs.TypeStop, jobs.TypeStart, jobs.TypeRestart, jobs.TypeUp, jobs.TypeDown}
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_id, type, state FROM jobs
		 WHERE id IN (SELECT MAX(id) FROM jobs WHERE type IN (?, ?, ?, ?, ?) AND state IN (?, ?, ?) GROUP BY stack_id)`,
		append(ops, jobs.StateSucceeded, jobs.StateFailed, jobs.StateCancelled)...)
	if err != nil {
		return nil, fmt.Errorf("listing stack operations: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var typ jobs.Type
		var state jobs.State
		if err := rows.Scan(&id, &typ, &state); err != nil {
			return nil, fmt.Errorf("listing stack operations: %w", err)
		}
		if state == jobs.StateSucceeded && (typ == jobs.TypeStop || typ == jobs.TypeDown) {
			out[id] = true
		}
	}
	return out, rows.Err()
}
