package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/MB512/stackvault/internal/jobs"
)

// Schedule is a cron schedule that queues backups or updates of one stack,
// or host jobs (cleanup; StackID 0). The cron expression is validated by
// the caller (internal/scheduler) before saving.
type Schedule struct {
	ID        int64
	StackID   int64  // 0 for a host schedule
	StackName string // filled when reading; "" for a host schedule
	CronExpr  string
	// Type is the job it queues: jobs.TypeBackup or jobs.TypeUpdate for a
	// stack, jobs.TypeCleanup for the host.
	Type    jobs.Type
	PreStop bool // take a pre-stop snapshot first
	// RollbackInPlace (updates): restore the final snapshot in place if
	// the update is rolled back.
	RollbackInPlace bool
	// NoBackup (updates): the admin confirmed that the update runs without
	// backup and without rollback while the stack has no repository. A
	// stack that has a repository when the job starts is backed up anyway.
	NoBackup bool
	Enabled  bool
}

const scheduleSelect = `SELECT sc.id, sc.stack_id, COALESCE(s.name, ''), sc.cron_expr, sc.job_type, sc.pre_stop, sc.rollback_in_place, sc.no_backup, sc.enabled
	FROM schedules sc LEFT JOIN stacks s ON s.id = sc.stack_id `

func scanSchedule(row interface{ Scan(...any) error }, sc *Schedule) error {
	var stackID sql.NullInt64
	err := row.Scan(&sc.ID, &stackID, &sc.StackName, &sc.CronExpr, &sc.Type, &sc.PreStop, &sc.RollbackInPlace, &sc.NoBackup, &sc.Enabled)
	sc.StackID = stackID.Int64
	return err
}

// CreateSchedule inserts sc (StackID, CronExpr, Type, PreStop,
// RollbackInPlace, NoBackup) as an enabled schedule and returns its id. A
// host schedule (cleanup) has StackID 0 and no options.
func (s *Store) CreateSchedule(ctx context.Context, sc Schedule) (int64, error) {
	switch {
	case sc.Type != jobs.TypeBackup && sc.Type != jobs.TypeUpdate && sc.Type != jobs.TypeCleanup:
		return 0, fmt.Errorf("creating schedule: invalid job type %q", sc.Type)
	case (sc.StackID == 0) != sc.Type.Host():
		return 0, fmt.Errorf("creating schedule: a %s schedule must have a stack exactly if it is not a host job", sc.Type)
	case sc.Type.Host() && (sc.PreStop || sc.RollbackInPlace || sc.NoBackup):
		return 0, errors.New("creating schedule: a host schedule has no options")
	case sc.RollbackInPlace && sc.Type != jobs.TypeUpdate:
		return 0, errors.New("creating schedule: rollback_in_place is only for updates")
	case sc.NoBackup && sc.Type != jobs.TypeUpdate:
		return 0, errors.New("creating schedule: no_backup is only for updates")
	case sc.NoBackup && (sc.RollbackInPlace || sc.PreStop):
		return 0, errors.New("creating schedule: an update without backup has no snapshots and no rollback")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (stack_id, cron_expr, job_type, pre_stop, rollback_in_place, no_backup, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		nullID(sc.StackID), sc.CronExpr, sc.Type, sc.PreStop, sc.RollbackInPlace, sc.NoBackup)
	if err != nil {
		if isForeignKeyError(err) {
			return 0, fmt.Errorf("stack %d: %w", sc.StackID, ErrNotFound)
		}
		return 0, fmt.Errorf("creating schedule: %w", err)
	}
	return res.LastInsertId()
}

// ListSchedules returns the schedules of a stack, or all schedules (also
// the host's) if stackID is 0, ordered by stack name (host first) and id.
func (s *Store) ListSchedules(ctx context.Context, stackID int64) ([]Schedule, error) {
	return s.listSchedules(ctx, `WHERE (? = 0 OR sc.stack_id = ?)`, stackID, stackID)
}

// ListHostSchedules returns the host schedules (cleanup), ordered by id.
func (s *Store) ListHostSchedules(ctx context.Context) ([]Schedule, error) {
	return s.listSchedules(ctx, `WHERE sc.stack_id IS NULL`)
}

func (s *Store) listSchedules(ctx context.Context, where string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, scheduleSelect+where+` ORDER BY s.name, sc.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		if err := scanSchedule(rows, &sc); err != nil {
			return nil, fmt.Errorf("listing schedules: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScheduleByID returns a schedule or ErrNotFound.
func (s *Store) ScheduleByID(ctx context.Context, id int64) (Schedule, error) {
	var sc Schedule
	err := scanSchedule(s.db.QueryRowContext(ctx, scheduleSelect+`WHERE sc.id = ?`, id), &sc)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, fmt.Errorf("schedule %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("loading schedule %d: %w", id, err)
	}
	return sc, nil
}

// SetScheduleEnabled enables or disables a schedule of a stack (stackID 0:
// of the host). It returns ErrNotFound if the schedule does not belong to
// the stack (host).
func (s *Store) SetScheduleEnabled(ctx context.Context, stackID, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET enabled = ? WHERE id = ? AND stack_id IS ?`,
		enabled, id, nullID(stackID))
	if err != nil {
		return fmt.Errorf("updating schedule %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("schedule %d of stack %d", id, stackID))
}

// DeleteSchedule deletes a schedule of a stack (stackID 0: of the host).
// It returns ErrNotFound if the schedule does not belong to the stack
// (host).
func (s *Store) DeleteSchedule(ctx context.Context, stackID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ? AND stack_id IS ?`, id, nullID(stackID))
	if err != nil {
		return fmt.Errorf("deleting schedule %d: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("schedule %d of stack %d", id, stackID))
}
