package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Stack is a stack record. Names come from discovery (internal/stacks); the
// database only mirrors them so other tables can reference a stack.
type Stack struct {
	ID      int64
	Name    string
	Enabled bool
	Present bool // false = directory missing (vanished, or imported but not restored yet)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, enabled, present FROM stacks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	// rows must be closed to release the connection (like disposing a
	// DbDataReader); defer guarantees it on every return path.
	defer rows.Close()
	var out []Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.ID, &st.Name, &st.Enabled, &st.Present); err != nil {
			return nil, fmt.Errorf("listing stacks: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StackByName returns the stack with the given name or ErrNotFound.
func (s *Store) StackByName(ctx context.Context, name string) (Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, enabled, present FROM stacks WHERE name = ?`, name,
	).Scan(&st.ID, &st.Name, &st.Enabled, &st.Present)
	if errors.Is(err, sql.ErrNoRows) {
		return Stack{}, fmt.Errorf("stack %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Stack{}, fmt.Errorf("loading stack %q: %w", name, err)
	}
	return st, nil
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
