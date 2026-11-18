package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
)

// Stack is a stack record. Names come from discovery (internal/stacks) or,
// for an import from another host, from a repository's snapshot paths
// (CreateImportedStack); the database mirrors them so other tables can
// reference a stack.
type Stack struct {
	ID      int64
	Name    string
	Enabled bool
	Present bool // false = directory missing (vanished, or imported but not restored yet)
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
				`INSERT INTO stacks (name, present) VALUES (?, 1)
				 ON CONFLICT (name) DO UPDATE SET present = 1`, name); err != nil {
				return fmt.Errorf("syncing stack %q: %w", name, err)
			}
		}
		return nil
	})
}

// ListStacks returns all stacks ordered by name.
func (s *Store) ListStacks(ctx context.Context) ([]Stack, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, enabled, present, source_host FROM stacks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	// rows must be closed to release the connection (like disposing a
	// DbDataReader); defer guarantees it on every return path.
	defer rows.Close()
	var out []Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.ID, &st.Name, &st.Enabled, &st.Present, &st.SourceHost); err != nil {
			return nil, fmt.Errorf("listing stacks: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StackSummary is a stack with overview data for the stack list.
type StackSummary struct {
	Stack
	HasRepository        bool
	ActiveSchedules      int       // enabled backup and update schedules (they run only if the stack is enabled)
	LastBackup           JobBrief  // newest backup job in any state; zero if there is none
	LastSuccessfulBackup time.Time // finish of the newest succeeded backup; zero if there is none
	LastUpdate           JobBrief  // newest update job in any state; zero if there is none
	// LastImageUpdate is the finish of the newest succeeded update job
	// that changed the image of a running service; zero if there is none.
	LastImageUpdate time.Time
}

// StackSummaries returns all stacks with their overview data, ordered by
// name.
func (s *Store) StackSummaries(ctx context.Context) ([]StackSummary, error) {
	lastBackup, err := s.latestJobs(ctx, jobs.TypeBackup)
	if err != nil {
		return nil, err
	}
	lastSuccess, err := s.lastSucceeded(ctx, jobs.TypeBackup, false)
	if err != nil {
		return nil, err
	}
	lastUpdate, err := s.latestJobs(ctx, jobs.TypeUpdate)
	if err != nil {
		return nil, err
	}
	lastImageUpdate, err := s.lastSucceeded(ctx, jobs.TypeUpdate, true)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.enabled, s.present, s.source_host, r.id IS NOT NULL,
		   (SELECT COUNT(*) FROM schedules sc WHERE sc.stack_id = s.id AND sc.enabled)
		 FROM stacks s
		 LEFT JOIN repositories r ON r.stack_id = s.id
		 ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	defer rows.Close()
	var out []StackSummary
	for rows.Next() {
		var ss StackSummary
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.Enabled, &ss.Present, &ss.SourceHost, &ss.HasRepository, &ss.ActiveSchedules); err != nil {
			return nil, fmt.Errorf("listing stacks: %w", err)
		}
		ss.LastBackup = lastBackup[ss.ID]
		ss.LastSuccessfulBackup = lastSuccess[ss.ID]
		ss.LastUpdate, ss.LastImageUpdate = lastUpdate[ss.ID], lastImageUpdate[ss.ID]
		out = append(out, ss)
	}
	return out, rows.Err()
}

// StackByName returns the stack with the given name or ErrNotFound.
func (s *Store) StackByName(ctx context.Context, name string) (Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, enabled, present, source_host FROM stacks WHERE name = ?`, name,
	).Scan(&st.ID, &st.Name, &st.Enabled, &st.Present, &st.SourceHost)
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
// restore, disabled for schedules until the admin enables it, and listing
// the snapshots of sourceHost. The caller has validated the name
// (stacks.ValidName) and checked that no such directory exists. It returns
// ErrConflict if a stack of that name exists.
func (s *Store) CreateImportedStack(ctx context.Context, name, sourceHost string) (Stack, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO stacks (name, enabled, present, source_host) VALUES (?, 0, 0, ?) ON CONFLICT (name) DO NOTHING`,
		name, sourceHost)
	if err != nil {
		return Stack{}, fmt.Errorf("creating stack %q: %w", name, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return Stack{}, fmt.Errorf("stack %q: %w", name, ErrConflict)
	}
	return s.StackByName(ctx, name)
}

// SetStackPresent records that a stack's directory exists (after the first
// restore of an imported stack) or not.
func (s *Store) SetStackPresent(ctx context.Context, id int64, present bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stacks SET present = ? WHERE id = ?`, present, id)
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

// SetStackEnabled enables or disables a stack.
func (s *Store) SetStackEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stacks SET enabled = ? WHERE id = ?`, enabled, id)
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
