package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// NotifyAuth is how StackBelay authenticates at the ntfy server: an access
// token, or a user with password, or nothing (both empty).
type NotifyAuth struct {
	Token    string `json:"token,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

// NotifySettings are the notification settings (issue #47). Notifications
// are off while Topic is empty.
type NotifySettings struct {
	ServerURL string
	Topic     string
	Auth      NotifyAuth
	// Events maps event keys (internal/notify) to on/off. A key that is
	// missing has its default.
	Events map[string]bool
	// SummaryCron is when the summary is sent (issue #48), a standard cron
	// expression in the manager's time zone.
	SummaryCron string
	// StaleDays: a stack without a consistent backup for longer is listed
	// as stale in the summary.
	StaleDays int
}

// notifyAAD binds the sealed ntfy credentials to their row (see repoAAD).
var notifyAAD = []byte("notify_settings:id=1")

// NotifySettings returns the notification settings with decrypted
// credentials.
func (s *Store) NotifySettings(ctx context.Context) (NotifySettings, error) {
	var ns NotifySettings
	var authEnc []byte
	var events string
	err := s.db.QueryRowContext(ctx,
		`SELECT server_url, topic, auth_enc, events, summary_cron, stale_days FROM notify_settings WHERE id = 1`).
		Scan(&ns.ServerURL, &ns.Topic, &authEnc, &events, &ns.SummaryCron, &ns.StaleDays)
	if err != nil {
		return NotifySettings{}, fmt.Errorf("loading notification settings: %w", err)
	}
	if err := json.Unmarshal([]byte(events), &ns.Events); err != nil {
		return NotifySettings{}, fmt.Errorf("decoding notification events: %w", err)
	}
	if authEnc != nil {
		plain, err := s.box.Open(authEnc, notifyAAD)
		if err != nil {
			return NotifySettings{}, fmt.Errorf("decrypting notification credentials: %w", err)
		}
		if err := json.Unmarshal(plain, &ns.Auth); err != nil {
			return NotifySettings{}, fmt.Errorf("decoding notification credentials: %w", err)
		}
	}
	return ns, nil
}

// SaveNotifySettings replaces the notification settings. The caller has
// validated them.
func (s *Store) SaveNotifySettings(ctx context.Context, ns NotifySettings) error {
	events, err := json.Marshal(ns.Events)
	if err != nil {
		return fmt.Errorf("encoding notification events: %w", err)
	}
	if ns.Events == nil {
		events = []byte("{}")
	}
	var authEnc []byte // NULL without credentials
	if ns.Auth != (NotifyAuth{}) {
		// The JSON is encrypted right below and never stored or logged in plain text.
		plain, err := json.Marshal(ns.Auth) //nolint:gosec // G117: see above
		if err != nil {
			return fmt.Errorf("encoding notification credentials: %w", err)
		}
		if authEnc, err = s.box.Seal(plain, notifyAAD); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE notify_settings SET server_url = ?, topic = ?, auth_enc = ?, events = ?, summary_cron = ?, stale_days = ?
		 WHERE id = 1`,
		ns.ServerURL, ns.Topic, authEnc, string(events), ns.SummaryCron, ns.StaleDays)
	if err != nil {
		return fmt.Errorf("saving notification settings: %w", err)
	}
	return nil
}

// NotifyEvent is a row of the notification outbox: a job that finished.
type NotifyEvent struct {
	ID        int64
	CreatedAt time.Time
	Job       Job
}

// addNotifyEvent writes the outbox row of a finished job. It runs in the
// transaction that records the job's result.
func addNotifyEvent(ctx context.Context, tx *sql.Tx, jobID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notify_events (created_at, job_id) VALUES (?, ?)`,
		formatTime(at), jobID); err != nil {
		return fmt.Errorf("writing notification event of job %s: %w", jobID, err)
	}
	return nil
}

// NotifyEvents returns the outbox, oldest first.
func (s *Store) NotifyEvents(ctx context.Context) ([]NotifyEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.created_at, `+jobColumns+jobFrom+
		`JOIN notify_events e ON e.job_id = j.id ORDER BY e.id`)
	if err != nil {
		return nil, fmt.Errorf("listing notification events: %w", err)
	}
	defer rows.Close()
	var out []NotifyEvent
	for rows.Next() {
		var ev NotifyEvent
		var created string
		// scanJob reads the job columns; the event's own columns come first.
		j, err := scanJob(prefixScanner{rows, []any{&ev.ID, &created}})
		if err != nil {
			return nil, fmt.Errorf("listing notification events: %w", err)
		}
		if ev.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		ev.Job = j
		out = append(out, ev)
	}
	return out, rows.Err()
}

// prefixScanner scans extra leading columns before the ones its caller
// passes, so scanJob can be reused for queries with additional columns.
type prefixScanner struct {
	row    interface{ Scan(...any) error }
	prefix []any
}

func (p prefixScanner) Scan(dest ...any) error {
	return p.row.Scan(append(append([]any{}, p.prefix...), dest...)...)
}

// DeleteNotifyEvents removes sent or dropped events from the outbox.
func (s *Store) DeleteNotifyEvents(ctx context.Context, ids []int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM notify_events WHERE id = ?`, id); err != nil {
				return fmt.Errorf("deleting notification event %d: %w", id, err)
			}
		}
		return nil
	})
}

// SetStackNotifyMuted mutes or unmutes the event messages of a stack.
func (s *Store) SetStackNotifyMuted(ctx context.Context, stackID int64, muted bool) error {
	r, err := s.db.ExecContext(ctx, `UPDATE stacks SET notify_muted = ? WHERE id = ?`, muted, stackID)
	if err != nil {
		return fmt.Errorf("muting notifications of stack %d: %w", stackID, err)
	}
	return expectOneRow(r, fmt.Sprintf("stack %d", stackID))
}

// MutedStacks returns the ids of the stacks whose notifications are muted.
func (s *Store) MutedStacks(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM stacks WHERE notify_muted = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing muted stacks: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing muted stacks: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
