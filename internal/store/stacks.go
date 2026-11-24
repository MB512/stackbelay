package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

// Stack is a stack record. Names come from discovery (internal/stacks) or,
// for an import from another host, from a repository's snapshot paths
// (CreateImportedStack); the database mirrors them so other tables can
// reference a stack.
type Stack struct {
	ID      int64
	Name    string
	Present bool // false = directory missing (vanished, or imported but not restored yet)
	// SeenPresent: the directory existed at some point (issue #27). False
	// only for a stack imported from another host that was never restored
	// here; a stack with SeenPresent and not Present has vanished.
	SeenPresent bool
	// SourceHost is the host whose snapshots the stack lists and restores
	// (restic --host); "" = this host. Backups always use this host's name.
	SourceHost string
}

// SyncStacks mirrors the result of a discovery run: discovered names are
// inserted or marked present, all other stacks are marked not present.
// Stacks are never deleted here, so their repositories, permissions and job
// history survive a directory that is temporarily missing.
func (s *Store) SyncStacks(ctx context.Context, discovered []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE stacks SET present = 0`); err != nil {
			return err
		}
		for _, name := range discovered {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO stacks (name, present, seen_present) VALUES (?, 1, 1)
				 ON CONFLICT (name) DO UPDATE SET present = 1, seen_present = 1`, name); err != nil {
				return fmt.Errorf("syncing stack %q: %w", name, err)
			}
		}
		return nil
	})
}

// ListStacks returns all stacks ordered by name.
func (s *Store) ListStacks(ctx context.Context) ([]Stack, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, present, seen_present, source_host FROM stacks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	// rows must be closed to release the connection (like disposing a
	// DbDataReader); defer guarantees it on every return path.
	defer rows.Close()
	var out []Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.ID, &st.Name, &st.Present, &st.SeenPresent, &st.SourceHost); err != nil {
			return nil, fmt.Errorf("listing stacks: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StackSummary is a stack with overview data for the stack list.
type StackSummary struct {
	Stack
	HasRepository  bool
	RepositoryName string   // "" if the stack has no repository
	LastBackup     JobBrief // newest backup job in any state; zero if there is none
	// LastFinal is the newest job that took a final snapshot (stack
	// stopped, consistent) that is known to be good: a succeeded backup,
	// update or compose apply, or one rolled back to that snapshot. Zero if
	// there is none.
	LastFinal  FinalBackup
	LastUpdate JobBrief // newest update job in any state; zero if there is none
	// LastImageUpdate is the finish of the newest succeeded update or
	// compose apply that changed the image of a running service; zero if
	// there is none.
	LastImageUpdate time.Time
}

// StackSummaries returns all stacks with their overview data, ordered by
// name.
func (s *Store) StackSummaries(ctx context.Context) ([]StackSummary, error) {
	lastBackup, err := s.latestJobs(ctx, jobs.TypeBackup)
	if err != nil {
		return nil, err
	}
	lastFinal, err := s.lastFinalSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	lastUpdate, err := s.latestJobs(ctx, jobs.TypeUpdate)
	if err != nil {
		return nil, err
	}
	lastImageUpdate, err := s.lastImageChanges(ctx, jobs.TypeUpdate, jobs.TypeComposeApply)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.present, s.seen_present, s.source_host, r.id IS NOT NULL, COALESCE(r.name, '')
		 FROM stacks s
		 LEFT JOIN repositories r ON r.id = s.repository_id
		 ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	defer rows.Close()
	var out []StackSummary
	for rows.Next() {
		var ss StackSummary
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.Present, &ss.SeenPresent, &ss.SourceHost, &ss.HasRepository, &ss.RepositoryName); err != nil {
			return nil, fmt.Errorf("listing stacks: %w", err)
		}
		ss.LastBackup = lastBackup[ss.ID]
		ss.LastFinal = lastFinal[ss.ID]
		ss.LastUpdate, ss.LastImageUpdate = lastUpdate[ss.ID], lastImageUpdate[ss.ID]
		out = append(out, ss)
	}
	return out, rows.Err()
}

// StackByName returns the stack with the given name or ErrNotFound.
func (s *Store) StackByName(ctx context.Context, name string) (Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, present, seen_present, source_host FROM stacks WHERE name = ?`, name,
	).Scan(&st.ID, &st.Name, &st.Present, &st.SeenPresent, &st.SourceHost)
	if errors.Is(err, sql.ErrNoRows) {
		return Stack{}, fmt.Errorf("stack %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Stack{}, fmt.Errorf("loading stack %q: %w", name, err)
	}
	return st, nil
}

// CreateImportedStack creates the record of a stack imported from a
// repository (restore from another host): not present until its first
// restore (it has no schedules until an admin adds some), listing the
// snapshots of sourceHost. The caller has validated the name
// (stacks.ValidName) and checked that no such directory exists. A record
// of that name is reused if it is unused (see StackReusable, issue #38);
// otherwise it returns ErrConflict.
func (s *Store) CreateImportedStack(ctx context.Context, name, sourceHost string) (Stack, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, present, seen_present, source_host) VALUES (?, 0, 0, ?) ON CONFLICT (name) DO NOTHING`,
			name, sourceHost)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM stacks WHERE name = ?`, name).Scan(&id); err != nil {
			return err
		}
		reusable, err := stackReusable(ctx, tx, id)
		if err != nil {
			return err
		}
		if !reusable {
			return fmt.Errorf("stack %q: %w", name, ErrConflict)
		}
		_, err = tx.ExecContext(ctx, `UPDATE stacks SET seen_present = 0, source_host = ? WHERE id = ?`, sourceHost, id)
		return err
	})
	if err != nil {
		return Stack{}, fmt.Errorf("creating stack %q: %w", name, err)
	}
	return s.StackByName(ctx, name)
}

// ErrStackBusy is returned by ForgetStack while the stack has a queued or
// running job, or while its directory exists.
var ErrStackBusy = errors.New("the stack has a directory or queued or running jobs")

// ForgetStack resets the record of a stack whose directory is gone (issue
// #38): its permissions, schedules, repository choice and source host go,
// so a later "Import stack" of the name starts clean and reuses the
// record. Job history and audit entries stay with it. It refuses with
// ErrStackBusy while the stack is present or has a queued or running job;
// the caller checks the disk as well.
func (s *Store) ForgetStack(ctx context.Context, id int64) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var present bool
		err := tx.QueryRowContext(ctx, `SELECT present FROM stacks WHERE id = ?`, id).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("stack %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE stack_id = ? AND state IN (?, ?)`,
			id, jobs.StateQueued, jobs.StateRunning).Scan(&active); err != nil {
			return err
		}
		if present || active > 0 {
			return ErrStackBusy
		}
		for _, q := range []string{
			`DELETE FROM permissions WHERE stack_id = ?`,
			`DELETE FROM schedules WHERE stack_id = ?`,
			`UPDATE stacks SET repository_id = NULL, source_host = '' WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("forgetting stack %d: %w", id, err)
	}
	return nil
}

// StackReusable reports whether an import may reuse the record of a stack
// (issue #38): not present, and nothing left of its old setup: no
// repository, permissions, schedules, queued or running jobs (the state
// ForgetStack leaves).
func (s *Store) StackReusable(ctx context.Context, id int64) (bool, error) {
	ok, err := stackReusable(ctx, s.db, id)
	if err != nil {
		return false, fmt.Errorf("checking stack %d: %w", id, err)
	}
	return ok, nil
}

func stackReusable(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT present = 0 AND repository_id IS NULL
		   AND NOT EXISTS (SELECT 1 FROM permissions WHERE stack_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM schedules WHERE stack_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM jobs WHERE stack_id = ?1 AND state IN (?2, ?3))
		 FROM stacks WHERE id = ?1`, id, jobs.StateQueued, jobs.StateRunning).Scan(&ok)
	return ok, err
}

// SetStackPresent records that a stack's directory exists (after the first
// restore of an imported stack) or not.
func (s *Store) SetStackPresent(ctx context.Context, id int64, present bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stacks SET present = ?, seen_present = seen_present OR ? WHERE id = ?`,
		present, present, id)
	if err != nil {
		return fmt.Errorf("updating stack %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("stack %d", id))
}

// SetStackSourceHost sets the host whose snapshots the stack lists and
// restores ("" = this host).
func (s *Store) SetStackSourceHost(ctx context.Context, id int64, host string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stacks SET source_host = ? WHERE id = ?`, host, id)
	if err != nil {
		return fmt.Errorf("updating stack %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("stack %d", id))
}

// expectOneRow turns "0 rows affected" into ErrNotFound.
func expectOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}
