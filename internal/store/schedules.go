package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Schedule is a cron schedule that queues backups of one stack. The cron
// expression is validated by the caller (internal/scheduler) before saving.
type Schedule struct {
	ID        int64
	StackID   int64
	StackName string // filled when reading
	CronExpr  string
	PreStop   bool // take a pre-stop snapshot first
	Enabled   bool
}

const scheduleSelect = `SELECT sc.id, sc.stack_id, s.name, sc.cron_expr, sc.pre_stop, sc.enabled
	FROM schedules sc JOIN stacks s ON s.id = sc.stack_id `

// CreateSchedule inserts an enabled schedule and returns its id.
func (s *Store) CreateSchedule(ctx context.Context, stackID int64, cronExpr string, preStop bool) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (stack_id, cron_expr, pre_stop, enabled) VALUES (?, ?, ?, 1)`,
		stackID, cronExpr, preStop)
	if err != nil {
		if isForeignKeyError(err) {
			return 0, fmt.Errorf("stack %d: %w", stackID, ErrNotFound)
		}
		return 0, fmt.Errorf("creating schedule: %w", err)
	}
	return res.LastInsertId()
}

// ListSchedules returns the schedules of a stack, or of all stacks if
// stackID is 0, ordered by stack name and id.
func (s *Store) ListSchedules(ctx context.Context, stackID int64) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, scheduleSelect+`WHERE (? = 0 OR sc.stack_id = ?) ORDER BY s.name, sc.id`,
		stackID, stackID)
	if err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		if err := rows.Scan(&sc.ID, &sc.StackID, &sc.StackName, &sc.CronExpr, &sc.PreStop, &sc.Enabled); err != nil {
			return nil, fmt.Errorf("listing schedules: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScheduleByID returns a schedule or ErrNotFound.
func (s *Store) ScheduleByID(ctx context.Context, id int64) (Schedule, error) {
	var sc Schedule
	err := s.db.QueryRowContext(ctx, scheduleSelect+`WHERE sc.id = ?`, id).
		Scan(&sc.ID, &sc.StackID, &sc.StackName, &sc.CronExpr, &sc.PreStop, &sc.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, fmt.Errorf("schedule %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("loading schedule %d: %w", id, err)
	}
	return sc, nil
}

// SetScheduleEnabled enables or disables a schedule of a stack. It returns
// ErrNotFound if the schedule does not belong to the stack.
func (s *Store) SetScheduleEnabled(ctx context.Context, stackID, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET enabled = ? WHERE id = ? AND stack_id = ?`,
		enabled, id, stackID)
	if err != nil {
		return fmt.Errorf("updating schedule %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("schedule %d of stack %d", id, stackID))
}

// DeleteSchedule deletes a schedule of a stack. It returns ErrNotFound if
// the schedule does not belong to the stack.
func (s *Store) DeleteSchedule(ctx context.Context, stackID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ? AND stack_id = ?`, id, stackID)
	if err != nil {
		return fmt.Errorf("deleting schedule %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("schedule %d of stack %d", id, stackID))
}
