package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrConflict is returned when a unique value (e.g. a username) is taken.
var ErrConflict = errors.New("already exists")

// User is a user account. PasswordHash is an argon2id hash (internal/auth).
type User struct {
	ID                 int64
	Username           string
	PasswordHash       string
	IsAdmin            bool
	Disabled           bool
	MustChangePassword bool
	CreatedAt          time.Time
}

const userColumns = `id, username, password_hash, is_admin, disabled, must_change_password, created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.MustChangePassword, &created); err != nil {
		return User{}, err
	}
	t, err := parseTime(created)
	if err != nil {
		return User{}, err
	}
	u.CreatedAt = t
	return u, nil
}

// CreateUser inserts a user and returns its id. A taken username yields
// ErrConflict.
func (s *Store) CreateUser(ctx context.Context, u User) (int64, error) {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, disabled, must_change_password, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		u.Username, u.PasswordHash, u.IsAdmin, u.Disabled, u.MustChangePassword, formatTime(u.CreatedAt),
	).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, fmt.Errorf("user %q: %w", u.Username, ErrConflict)
		}
		return 0, fmt.Errorf("creating user %q: %w", u.Username, err)
	}
	return id, nil
}

// UserByUsername returns the user or ErrNotFound.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("user %q: %w", username, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("loading user %q: %w", username, err)
	}
	return u, nil
}

// UserByID returns the user or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("user %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("loading user %d: %w", id, err)
	}
	return u, nil
}

// CountActiveAdmins returns the number of admins that are not disabled.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting admins: %w", err)
	}
	return n, nil
}

// SetPassword replaces a user's password hash and must-change flag.
func (s *Store) SetPassword(ctx context.Context, userID int64, hash string, mustChange bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = ? WHERE id = ?`,
		hash, mustChange, userID)
	if err != nil {
		return fmt.Errorf("setting password of user %d: %w", userID, err)
	}
	return expectOneRow(res, fmt.Sprintf("user %d", userID))
}

// SetUserAdmin grants or revokes admin rights and enables the account.
func (s *Store) SetUserAdmin(ctx context.Context, userID int64, admin bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET is_admin = ?, disabled = 0 WHERE id = ?`, admin, userID)
	if err != nil {
		return fmt.Errorf("updating user %d: %w", userID, err)
	}
	return expectOneRow(res, fmt.Sprintf("user %d", userID))
}

// ErrLastAdmin is returned when a change would leave no active admin.
var ErrLastAdmin = errors.New("at least one active admin is required")

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("listing users: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserFlags sets a user's admin and disabled flags. Disabling a user
// also ends all of their sessions. If the change would leave no active
// admin, nothing is changed and ErrLastAdmin is returned; the check runs in
// the same transaction as the update, so two admins demoting each other at
// the same time cannot both succeed.
func (s *Store) SetUserFlags(ctx context.Context, userID int64, admin, disabled bool) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = ?, disabled = ? WHERE id = ?`, admin, disabled, userID)
		if err != nil {
			return err
		}
		if err := expectOneRow(res, fmt.Sprintf("user %d", userID)); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0`).Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return ErrLastAdmin // rolls the update back
		}
		if disabled {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("updating user %d: %w", userID, err)
	}
	return nil
}
