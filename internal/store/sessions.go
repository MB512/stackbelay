package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a login session. IDHash is the SHA-256 of the random session
// token in the cookie; the token itself is never stored, so a leaked
// database does not contain usable session ids.
type Session struct {
	IDHash    string
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES (?, ?, ?, ?)`,
		sess.IDHash, sess.UserID, sess.CSRFToken, formatTime(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// SessionWithUser returns a session that has not expired at now, together
// with its user. Expired or unknown sessions yield ErrNotFound.
func (s *Store) SessionWithUser(ctx context.Context, idHash string, now time.Time) (Session, User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.user_id, s.csrf_token, s.expires_at,
		        u.id, u.username, u.password_hash, u.is_admin, u.disabled, u.must_change_password, u.created_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = ? AND s.expires_at > ?`, idHash, formatTime(now))
	var sess Session
	var u User
	var expires, created string
	err := row.Scan(&sess.IDHash, &sess.UserID, &sess.CSRFToken, &expires,
		&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.MustChangePassword, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, fmt.Errorf("session: %w", ErrNotFound)
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("loading session: %w", err)
	}
	if sess.ExpiresAt, err = parseTime(expires); err != nil {
		return Session{}, User{}, err
	}
	if u.CreatedAt, err = parseTime(created); err != nil {
		return Session{}, User{}, err
	}
	return sess, u, nil
}

// ExtendSession sets a new expiry time (sliding expiration).
func (s *Store) ExtendSession(ctx context.Context, idHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id = ?`, formatTime(expires), idHash)
	if err != nil {
		return fmt.Errorf("extending session: %w", err)
	}
	return nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, idHash); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteUserSessions removes all sessions of a user except keepIDHash (pass
// "" to remove all), e.g. after a password change.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepIDHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id != ?`, userID, keepIDHash); err != nil {
		return fmt.Errorf("deleting sessions of user %d: %w", userID, err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that expired before now and
// returns how many were removed.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	return res.RowsAffected()
}
