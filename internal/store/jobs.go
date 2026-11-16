package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
)

// Job triggers.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

// JobOptions are the options of a job, stored as JSON.
type JobOptions struct {
	PreStop            bool   `json:"pre_stop,omitempty"`             // backup
	SnapshotID         string `json:"snapshot_id,omitempty"`          // restore: snapshot to restore
	SkipSafetySnapshot bool   `json:"skip_safety_snapshot,omitempty"` // restore
	NoStart            bool   `json:"no_start,omitempty"`             // restore
	PinImages          bool   `json:"pin_images,omitempty"`           // restore: image versions from metadata.txt
	InPlace            bool   `json:"in_place,omitempty"`             // restore: over the stack directory, no swap
	// NoBackup (update): the user confirmed updating a stack without
	// repository, i.e. without backup and without rollback. It only takes
	// effect if the stack still has no repository when the job starts.
	NoBackup bool `json:"no_backup,omitempty"`
	// Compose editor (compose-apply): the stack's main compose file (base
	// name), the SHA-256 (hex) of the content the editor started from and
	// of the new content (stored in job_compose, see Job.ComposeContent).
	ComposeFile       string `json:"compose_file,omitempty"`
	ComposeBaseSHA256 string `json:"compose_base_sha256,omitempty"`
	ComposeSHA256     string `json:"compose_sha256,omitempty"`
}

// JobResult holds results besides the main snapshot, stored as JSON.
type JobResult struct {
	PreStopSnapshot string `json:"pre_stop_snapshot,omitempty"`
	SafetySnapshot  string `json:"safety_snapshot,omitempty"`
	PreviousDir     string `json:"previous_dir,omitempty"`
	RolledBack      bool   `json:"rolled_back,omitempty"` // update: failed and restored to the job's snapshot
}

// Job is a backup or restore job. State follows jobs.State: queued ->
// running -> succeeded | failed | cancelled.
type Job struct {
	ID          string
	StackID     int64
	StackName   string // filled when reading
	Type        jobs.Type
	Trigger     string
	RequestedBy int64  // 0 for scheduled jobs
	Requester   string // username, filled when reading
	State       jobs.State
	Options     JobOptions
	Snapshot    string // backup: final snapshot; restore: restored snapshot
	Error       string
	Result      JobResult
	QueuedAt    time.Time
	StartedAt   time.Time // zero until running
	FinishedAt  time.Time // zero until final
	// ComposeContent is the new compose file of a compose-apply job. Only
	// written by CreateJob; reading it needs JobComposeContent.
	ComposeContent []byte
}

// ErrJobQueued is returned by CreateJob when the stack already has a queued
// job of the same type.
var ErrJobQueued = errors.New("a job of this type is already queued for the stack")

// CreateJob inserts a queued job. j.ID, StackID, Type, Trigger and QueuedAt
// must be set.
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	opts, err := json.Marshal(j.Options)
	if err != nil {
		return fmt.Errorf("encoding job options: %w", err)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE stack_id = ? AND type = ? AND state = ?`,
			j.StackID, j.Type, jobs.StateQueued).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrJobQueued
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (id, stack_id, type, trigger, requested_by, state, options, queued_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			j.ID, j.StackID, j.Type, j.Trigger, nullID(j.RequestedBy), jobs.StateQueued, string(opts), formatTime(j.QueuedAt))
		if err != nil && isForeignKeyError(err) {
			return fmt.Errorf("stack %d or user %d: %w", j.StackID, j.RequestedBy, ErrNotFound)
		}
		if err != nil || j.ComposeContent == nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO job_compose (job_id, content) VALUES (?, ?)`, j.ID, j.ComposeContent)
		return err
	})
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}
	return nil
}

// JobComposeContent returns the new compose file of a compose-apply job.
func (s *Store) JobComposeContent(ctx context.Context, jobID string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM job_compose WHERE job_id = ?`, jobID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("compose content of job %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading compose content of job %s: %w", jobID, err)
	}
	return content, nil
}

const jobColumns = `j.id, j.stack_id, s.name, j.type, j.trigger, j.requested_by, COALESCE(u.username, ''), j.state,
	j.options, COALESCE(j.snapshot_id, ''), COALESCE(j.error, ''), j.result, j.queued_at,
	COALESCE(j.started_at, ''), COALESCE(j.finished_at, '')`

const jobFrom = ` FROM jobs j JOIN stacks s ON s.id = j.stack_id LEFT JOIN users u ON u.id = j.requested_by `

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var requestedBy sql.NullInt64
	var opts, result, queued, started, finished string
	if err := row.Scan(&j.ID, &j.StackID, &j.StackName, &j.Type, &j.Trigger, &requestedBy, &j.Requester, &j.State,
		&opts, &j.Snapshot, &j.Error, &result, &queued, &started, &finished); err != nil {
		return Job{}, err
	}
	j.RequestedBy = requestedBy.Int64
	if err := json.Unmarshal([]byte(opts), &j.Options); err != nil {
		return Job{}, fmt.Errorf("decoding job options: %w", err)
	}
	if err := json.Unmarshal([]byte(result), &j.Result); err != nil {
		return Job{}, fmt.Errorf("decoding job result: %w", err)
	}
	var err error
	if j.QueuedAt, err = parseTime(queued); err != nil {
		return Job{}, err
	}
	if j.StartedAt, err = parseOptionalTime(started); err != nil {
		return Job{}, err
	}
	if j.FinishedAt, err = parseOptionalTime(finished); err != nil {
		return Job{}, err
	}
	return j, nil
}

func parseOptionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseTime(s)
}

// ImportJob inserts a job rebuilt from its job directory, in any state and
// with its times and results. RequesterName is resolved to a user if one
// with that name exists. It fails with ErrConflict if the job exists or
// would be a second running job of its stack.
func (s *Store) ImportJob(ctx context.Context, j Job, requesterName string) error {
	opts, err := json.Marshal(j.Options)
	if err != nil {
		return fmt.Errorf("encoding job options: %w", err)
	}
	res, err := json.Marshal(j.Result)
	if err != nil {
		return fmt.Errorf("encoding job result: %w", err)
	}
	opt := func(t time.Time) sql.NullString {
		return sql.NullString{String: formatTime(t), Valid: !t.IsZero()}
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, stack_id, type, trigger, requested_by, state, options, snapshot_id, error, result,
		                   queued_at, started_at, finished_at)
		 VALUES (?, ?, ?, ?, (SELECT id FROM users WHERE username = ? AND ? != ''), ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)`,
		j.ID, j.StackID, j.Type, j.Trigger, requesterName, requesterName, j.State, string(opts), j.Snapshot,
		truncateRunes(j.Error, 2000), string(res), formatTime(j.QueuedAt), opt(j.StartedAt), opt(j.FinishedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("importing job %s: %w", j.ID, ErrConflict)
		}
		return fmt.Errorf("importing job %s: %w", j.ID, err)
	}
	return nil
}

// JobByID returns a job or ErrNotFound.
func (s *Store) JobByID(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+`WHERE j.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("loading job %q: %w", id, err)
	}
	return j, nil
}

// ListJobs returns the newest jobs of a stack, or of all stacks if stackID
// is 0.
func (s *Store) ListJobs(ctx context.Context, stackID int64, limit int) ([]Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+`WHERE (? = 0 OR j.stack_id = ?) ORDER BY j.id DESC LIMIT ?`,
		stackID, stackID, limit)
}

// JobsInState returns all jobs in a state, oldest first.
func (s *Store) JobsInState(ctx context.Context, state jobs.State) ([]Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+`WHERE j.state = ? ORDER BY j.id`, state)
}

func (s *Store) queryJobs(ctx context.Context, query string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("listing jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClaimNextJob marks the oldest queued job whose stack has no running job
// as running and returns it. ok is false if there is nothing to start or
// maxRunning jobs are already running. The unique index
// jobs_one_running_per_stack guarantees the per-stack lock even if two
// claims raced.
func (s *Store) ClaimNextJob(ctx context.Context, maxRunning int, now time.Time) (job Job, ok bool, err error) {
	var id string
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var running int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state = ?`, jobs.StateRunning).Scan(&running); err != nil {
			return err
		}
		if running >= maxRunning {
			return nil
		}
		err := tx.QueryRowContext(ctx,
			`SELECT q.id FROM jobs q
			 WHERE q.state = ? AND NOT EXISTS (SELECT 1 FROM jobs r WHERE r.stack_id = q.stack_id AND r.state = ?)
			 ORDER BY q.id LIMIT 1`, jobs.StateQueued, jobs.StateRunning).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			id = ""
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, started_at = ? WHERE id = ?`,
			jobs.StateRunning, formatTime(now), id)
		return err
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("claiming job: %w", err)
	}
	if id == "" {
		return Job{}, false, nil
	}
	job, err = s.JobByID(ctx, id)
	return job, err == nil, err
}

// FinishJob records the final state of a queued or running job. It returns
// ErrNotFound if the job does not exist or is already final, so a result
// is never recorded twice.
func (s *Store) FinishJob(ctx context.Context, id string, state jobs.State, snapshot, errMsg string, result JobResult, at time.Time) error {
	if !state.Final() {
		return fmt.Errorf("finishing job %s: %q is not a final state", id, state)
	}
	res, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encoding job result: %w", err)
	}
	r, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, snapshot_id = NULLIF(?, ''), error = NULLIF(?, ''), result = ?, finished_at = ?
		 WHERE id = ? AND state IN (?, ?)`,
		state, snapshot, truncateRunes(errMsg, 2000), string(res), formatTime(at), id, jobs.StateQueued, jobs.StateRunning)
	if err != nil {
		return fmt.Errorf("finishing job %s: %w", id, err)
	}
	return expectOneRow(r, fmt.Sprintf("unfinished job %s", id))
}

// truncateRunes makes s valid UTF-8 and cuts it to at most n characters.
func truncateRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
