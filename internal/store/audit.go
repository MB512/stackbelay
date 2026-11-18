package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuditEntry is one audit log record. Detail must never contain secrets.
type AuditEntry struct {
	ID      int64 // filled when reading
	At      time.Time
	UserID  int64 // 0 = no (known) user, e.g. a failed login for an unknown name
	Action  string
	StackID int64 // 0 = not stack-related
	Detail  string
	// Username and StackName are filled when reading (empty for 0).
	Username  string
	StackName string
}

// AddAudit appends an entry to the audit log.
func (s *Store) AddAudit(ctx context.Context, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, user_id, action, stack_id, detail) VALUES (?, ?, ?, ?, ?)`,
		formatTime(e.At), nullID(e.UserID), e.Action, nullID(e.StackID), e.Detail)
	if err != nil {
		return fmt.Errorf("writing audit log (%s): %w", e.Action, err)
	}
	return nil
}

// ListAudit returns the newest entries first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return s.ListAuditBefore(ctx, 0, limit)
}

// ListAuditBefore returns up to limit entries with an id below before
// (0 = from the newest), newest first. The id of the last entry is the
// before of the next page.
func (s *Store) ListAuditBefore(ctx context.Context, before int64, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.at, a.user_id, a.action, a.stack_id, a.detail, u.username, st.name
		 FROM audit_log a LEFT JOIN users u ON u.id = a.user_id LEFT JOIN stacks st ON st.id = a.stack_id
		 WHERE (? = 0 OR a.id < ?) ORDER BY a.id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, fmt.Errorf("listing audit log: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		var user, stack sql.NullInt64
		var detail, username, stackName sql.NullString
		if err := rows.Scan(&e.ID, &at, &user, &e.Action, &stack, &detail, &username, &stackName); err != nil {
			return nil, fmt.Errorf("listing audit log: %w", err)
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		e.UserID, e.StackID, e.Detail = user.Int64, stack.Int64, detail.String
		e.Username, e.StackName = username.String, stackName.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// nullID stores 0 as NULL, so foreign keys are not violated.
func nullID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id != 0}
}
