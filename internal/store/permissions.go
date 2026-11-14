package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Permission holds what a (non-admin) user may do on one stack. The zero
// value means no access; a missing row is the same as a zero value.
type Permission struct {
	UserID     int64
	StackID    int64
	CanBackup  bool
	CanRestore bool
}

// None reports whether the permission grants nothing.
func (p Permission) None() bool { return !p.CanBackup && !p.CanRestore }

// SetPermission stores a permission. A permission that grants nothing
// deletes the row, so the table only holds actual grants.
func (s *Store) SetPermission(ctx context.Context, p Permission) error {
	var err error
	if p.None() {
		_, err = s.db.ExecContext(ctx, `DELETE FROM permissions WHERE user_id = ? AND stack_id = ?`, p.UserID, p.StackID)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO permissions (user_id, stack_id, can_backup, can_restore) VALUES (?, ?, ?, ?)
			 ON CONFLICT (user_id, stack_id) DO UPDATE SET
			   can_backup = excluded.can_backup, can_restore = excluded.can_restore`,
			p.UserID, p.StackID, p.CanBackup, p.CanRestore)
		if err != nil && isForeignKeyError(err) {
			return fmt.Errorf("permission for user %d on stack %d: %w", p.UserID, p.StackID, ErrNotFound)
		}
	}
	if err != nil {
		return fmt.Errorf("saving permission for user %d on stack %d: %w", p.UserID, p.StackID, err)
	}
	return nil
}

// Permission returns the permission of a user on a stack. Without a grant
// it returns a zero Permission (deny by default), not an error.
func (s *Store) Permission(ctx context.Context, userID, stackID int64) (Permission, error) {
	p := Permission{UserID: userID, StackID: stackID}
	err := s.db.QueryRowContext(ctx,
		`SELECT can_backup, can_restore FROM permissions WHERE user_id = ? AND stack_id = ?`, userID, stackID,
	).Scan(&p.CanBackup, &p.CanRestore)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Permission{}, fmt.Errorf("loading permission for user %d on stack %d: %w", userID, stackID, err)
	}
	return p, nil
}

// PermissionsForUser returns the grants of a user, keyed by stack id.
func (s *Store) PermissionsForUser(ctx context.Context, userID int64) (map[int64]Permission, error) {
	perms, err := s.queryPermissions(ctx, `SELECT `+permColumns+` FROM permissions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	byStack := make(map[int64]Permission, len(perms))
	for _, p := range perms {
		byStack[p.StackID] = p
	}
	return byStack, nil
}

// PermissionsForStack returns the grants on a stack, keyed by user id.
func (s *Store) PermissionsForStack(ctx context.Context, stackID int64) (map[int64]Permission, error) {
	perms, err := s.queryPermissions(ctx, `SELECT `+permColumns+` FROM permissions WHERE stack_id = ?`, stackID)
	if err != nil {
		return nil, err
	}
	byUser := make(map[int64]Permission, len(perms))
	for _, p := range perms {
		byUser[p.UserID] = p
	}
	return byUser, nil
}

const permColumns = `user_id, stack_id, can_backup, can_restore`

// queryPermissions runs a query that selects permColumns.
func (s *Store) queryPermissions(ctx context.Context, query string, arg int64) ([]Permission, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("listing permissions: %w", err)
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		var p Permission
		if err := rows.Scan(&p.UserID, &p.StackID, &p.CanBackup, &p.CanRestore); err != nil {
			return nil, fmt.Errorf("listing permissions: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
