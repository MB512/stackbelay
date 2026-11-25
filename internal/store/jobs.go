package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

// Job triggers.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

// JobOptions are the options of a job, stored as JSON.
type JobOptions struct {
	SnapshotID string `json:"snapshot_id,omitempty"` // restore: snapshot to restore
	NoStart    bool   `json:"no_start,omitempty"`    // restore
	PinImages  bool   `json:"pin_images,omitempty"`  // restore: image versions from metadata.txt
	// TagFallback (restore with PinImages, issue #12): services whose
	// recorded image version is gone start with their tag.
	TagFallback bool `json:"tag_fallback,omitempty"`
	// Import (restore): the stack directory does not exist yet (a stack
	// imported from another host), or only partly after an incomplete
	// import; nothing to stop or to protect.
	Import bool `json:"import,omitempty"`
	// LimitDownloadKiB (restore): restic --limit-download, KiB/s; 0 = none.
	LimitDownloadKiB int `json:"limit_download_kib,omitempty"`
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
	// NotifyMuted: an admin started the job with notifications muted
	// (issue #50); it sends no event message, whenever it finishes.
	NotifyMuted bool `json:"notify_muted,omitempty"`
}

// JobResult holds results besides the main snapshot, stored as JSON.
type JobResult struct {
	PreStopSnapshot string `json:"pre_stop_snapshot,omitempty"`
	SafetySnapshot  string `json:"safety_snapshot,omitempty"`
	// PreviousDir: the previous stack directory that restores kept before
	// they ran in place (issue #33); shown for old jobs only.
	PreviousDir string `json:"previous_dir,omitempty"`
	RolledBack  bool   `json:"rolled_back,omitempty"` // update: failed and restored to the job's snapshot
	Unchanged   bool   `json:"unchanged,omitempty"`   // update: no new image, nothing done
	// Unpinned (restore with TagFallback): services that started with their
	// tag because their recorded image version was gone.
	Unpinned []string `json:"unpinned,omitempty"`
	// ImagesChanged: update, compose apply: services whose running image
	// (image id) differs after the change.
	ImagesChanged []string `json:"images_changed,omitempty"`
	// ReplacedImages: update, compose apply: the image ids those services
	// ran before; a host cleanup keeps them for a while (issue #19).
	ReplacedImages []string `json:"replaced_images,omitempty"`
	// Cleanup (host job): what was removed; FreedBytes sums the sizes of
	// the removed images (shared layers counted per image).
	RemovedContainers int   `json:"removed_containers,omitempty"`
	RemovedImages     int   `json:"removed_images,omitempty"`
	FreedBytes        int64 `json:"freed_bytes,omitempty"`
	// ServicesDown: a failed job left these services down that ran before
	// it (see jobs.Status).
	ServicesDown []string `json:"services_down,omitempty"`
	// StackStoppedAt / StackStartedAt: when the job stopped the running
	// stack and started it again; the downtime of the summary (issue #48).
	// Nil if the job stopped no running stack, or did not start it again.
	StackStoppedAt *time.Time `json:"stack_stopped_at,omitempty"`
	StackStartedAt *time.Time `json:"stack_started_at,omitempty"`
}

// Job is a backup or restore job. State follows jobs.State: queued ->
// running -> succeeded | failed | cancelled.
type Job struct {
	ID          string
	StackID     int64  // 0 for a host job (jobs.Type.Host)
	StackName   string // filled when reading; "" for a host job
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

// ErrJobQueued is returned by CreateJob when the stack (or, for a host job,
// the host) already has a queued job of the same type.
var ErrJobQueued = errors.New("a job of this type is already queued for the stack")

// CreateJob inserts a queued job. j.ID, Type, Trigger and QueuedAt must be
// set, and StackID for every type but host jobs (which have none).
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	if (j.StackID == 0) != j.Type.Host() {
		return fmt.Errorf("creating job: a %s job must have a stack exactly if it is not a host job", j.Type)
	}
	opts, err := json.Marshal(j.Options)
	if err != nil {
		return fmt.Errorf("encoding job options: %w", err)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		// "IS" also matches NULL (host jobs).
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE stack_id IS ? AND type = ? AND state = ?`,
			nullID(j.StackID), j.Type, jobs.StateQueued).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrJobQueued
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (id, stack_id, type, trigger, requested_by, state, options, queued_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			j.ID, nullID(j.StackID), j.Type, j.Trigger, nullID(j.RequestedBy), jobs.StateQueued, string(opts), formatTime(j.QueuedAt))
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

const jobColumns = `j.id, j.stack_id, COALESCE(s.name, ''), j.type, j.trigger, j.requested_by, COALESCE(u.username, ''), j.state,
	j.options, COALESCE(j.snapshot_id, ''), COALESCE(j.error, ''), j.result, j.queued_at,
	COALESCE(j.started_at, ''), COALESCE(j.finished_at, '')`

const jobFrom = ` FROM jobs j LEFT JOIN stacks s ON s.id = j.stack_id LEFT JOIN users u ON u.id = j.requested_by `

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var stackID, requestedBy sql.NullInt64
	var opts, result, queued, started, finished string
	if err := row.Scan(&j.ID, &stackID, &j.StackName, &j.Type, &j.Trigger, &requestedBy, &j.Requester, &j.State,
		&opts, &j.Snapshot, &j.Error, &result, &queued, &started, &finished); err != nil {
		return Job{}, err
	}
	j.StackID, j.RequestedBy = stackID.Int64, requestedBy.Int64
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
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (id, stack_id, type, trigger, requested_by, state, options, snapshot_id, error, result,
			                   queued_at, started_at, finished_at)
			 VALUES (?, ?, ?, ?, (SELECT id FROM users WHERE username = ? AND ? != ''), ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)`,
			j.ID, nullID(j.StackID), j.Type, j.Trigger, requesterName, requesterName, j.State, string(opts), j.Snapshot,
			truncateRunes(j.Error, 2000), string(res), formatTime(j.QueuedAt), opt(j.StartedAt), opt(j.FinishedAt))
		if err != nil || !j.State.Final() {
			return err
		}
		// A job that finished while the manager was down (e.g. after it
		// restored its own stack) is reported like any other.
		return addNotifyEvent(ctx, tx, j.ID, j.FinishedAt)
	})
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

// ListHostJobs returns the newest host jobs (cleanups).
func (s *Store) ListHostJobs(ctx context.Context, limit int) ([]Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+`WHERE j.stack_id IS NULL ORDER BY j.id DESC LIMIT ?`, limit)
}

// JobBrief is the short form of a job for overviews.
type JobBrief struct {
	ID    string // empty if there is no such job
	State jobs.State
	Time  time.Time // finished, else started, else queued
}

// latestJobs returns, per stack id, the newest job of type typ in any
// state. Stacks without such a job are missing from the map.
func (s *Store) latestJobs(ctx context.Context, typ jobs.Type) (map[int64]JobBrief, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_id, id, state, COALESCE(finished_at, started_at, queued_at) FROM jobs
		 WHERE id IN (SELECT MAX(id) FROM jobs WHERE type = ? GROUP BY stack_id)`, typ)
	if err != nil {
		return nil, fmt.Errorf("listing latest %s jobs: %w", typ, err)
	}
	defer rows.Close()
	out := make(map[int64]JobBrief)
	for rows.Next() {
		var stackID int64
		var b JobBrief
		var at string
		if err := rows.Scan(&stackID, &b.ID, &b.State, &at); err != nil {
			return nil, fmt.Errorf("listing latest %s jobs: %w", typ, err)
		}
		if b.Time, err = parseTime(at); err != nil {
			return nil, err
		}
		out[stackID] = b
	}
	return out, rows.Err()
}

// FinalBackup is a job that took a good final snapshot (see
// StackSummary.LastFinal).
type FinalBackup struct {
	JobBrief
	Type jobs.Type // backup, update or compose-apply
}

// lastFinalSnapshots returns, per stack id, the newest job with a good
// final snapshot: a backup, update or compose apply that succeeded, or an
// update or compose apply that failed and was rolled back to it (the
// rollback restored it, so it is usable). A failed backup may still have a
// snapshot (restic reported an incomplete one), which does not count.
func (s *Store) lastFinalSnapshots(ctx context.Context) (map[int64]FinalBackup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_id, id, type, state, finished_at FROM jobs WHERE id IN (
		   SELECT MAX(id) FROM jobs
		   WHERE type IN (?, ?, ?) AND snapshot_id IS NOT NULL AND finished_at IS NOT NULL
		     AND (state = ? OR COALESCE(json_extract(result, '$.rolled_back'), 0))
		   GROUP BY stack_id)`,
		jobs.TypeBackup, jobs.TypeUpdate, jobs.TypeComposeApply, jobs.StateSucceeded)
	if err != nil {
		return nil, fmt.Errorf("listing final snapshots: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]FinalBackup)
	for rows.Next() {
		var stackID int64
		var f FinalBackup
		var at string
		if err := rows.Scan(&stackID, &f.ID, &f.Type, &f.State, &at); err != nil {
			return nil, fmt.Errorf("listing final snapshots: %w", err)
		}
		if f.Time, err = parseTime(at); err != nil {
			return nil, err
		}
		out[stackID] = f
	}
	return out, rows.Err()
}

// lastImageChanges returns, per stack id, when the newest succeeded job of
// one of the types finished that changed the image of a running service
// (result.images_changed, recorded by updates and compose applies, see
// "Update pipeline" in docs/architecture.md). Stacks without such a job
// are missing from the map.
func (s *Store) lastImageChanges(ctx context.Context, types ...jobs.Type) (map[int64]time.Time, error) {
	typeList, err := json.Marshal(types)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_id, MAX(finished_at) FROM jobs
		 WHERE type IN (SELECT value FROM json_each(?)) AND state = ? AND finished_at IS NOT NULL
		   AND json_array_length(result, '$.images_changed') > 0
		 GROUP BY stack_id`,
		string(typeList), jobs.StateSucceeded)
	if err != nil {
		return nil, fmt.Errorf("listing last succeeded %v jobs: %w", types, err)
	}
	defer rows.Close()
	out := make(map[int64]time.Time)
	for rows.Next() {
		var stackID int64
		var at string
		if err := rows.Scan(&stackID, &at); err != nil {
			return nil, fmt.Errorf("listing last succeeded %v jobs: %w", types, err)
		}
		if out[stackID], err = parseTime(at); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

// JobsFinishedBefore returns the ids of the jobs that finished (any final
// state) before t, oldest first. Used to delete old job directories (issue
// #16); the rows themselves stay.
func (s *Store) JobsFinishedBefore(ctx context.Context, t time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state IN (?, ?, ?) AND finished_at IS NOT NULL AND finished_at < ? ORDER BY id`,
		jobs.StateSucceeded, jobs.StateFailed, jobs.StateCancelled, formatTime(t))
	if err != nil {
		return nil, fmt.Errorf("listing old jobs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing old jobs: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ActiveJobs returns the running jobs of all stacks, then the queued ones,
// each oldest first.
func (s *Store) ActiveJobs(ctx context.Context) ([]Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+`WHERE j.state IN (?, ?) ORDER BY j.state = ? DESC, j.id`,
		jobs.StateRunning, jobs.StateQueued, jobs.StateRunning)
}

// ReplacedImagesSince returns the image ids that update and compose-apply
// jobs finished at or after since replaced (result.replaced_images), each
// once. A host cleanup keeps these images (issue #19).
func (s *Store) ReplacedImagesSince(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.value FROM jobs j, json_each(j.result, '$.replaced_images') r
		 WHERE j.finished_at >= ? ORDER BY r.value`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("listing replaced images: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing replaced images: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// ClaimNextJob marks the oldest queued job that may start as running and
// returns it. ok is false if there is nothing to start: maxRunning jobs
// (STACKBELAY_MAX_JOBS) run already, or no queued job may start.
//
// A stack job may start when its stack has no running job, no host job
// runs and no host job was queued before it (so a host job is not starved
// by a stream of stack jobs). A host job may start when no job at all runs.
// The unique indexes jobs_one_running_per_stack and jobs_one_running_host
// back the locks in the database; claims are serialized by BEGIN IMMEDIATE.
func (s *Store) ClaimNextJob(ctx context.Context, maxRunning int, now time.Time) (job Job, ok bool, err error) {
	var id string
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var running int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE state = ?`,
			jobs.StateRunning).Scan(&running); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx,
			`SELECT q.id FROM jobs q
			 WHERE q.state = :queued AND :free AND (
			   (q.stack_id IS NULL AND NOT EXISTS (SELECT 1 FROM jobs r WHERE r.state = :running))
			   OR (q.stack_id IS NOT NULL
			       AND NOT EXISTS (SELECT 1 FROM jobs r WHERE r.state = :running AND (r.stack_id = q.stack_id OR r.stack_id IS NULL))
			       AND NOT EXISTS (SELECT 1 FROM jobs h WHERE h.state = :queued AND h.stack_id IS NULL AND h.id < q.id)))
			 ORDER BY q.id LIMIT 1`,
			sql.Named("queued", jobs.StateQueued), sql.Named("running", jobs.StateRunning),
			sql.Named("free", running < maxRunning)).Scan(&id)
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
	// The notification event is written in the same transaction, so a
	// finished job always gets its event (issue #47).
	return s.inTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, snapshot_id = NULLIF(?, ''), error = NULLIF(?, ''), result = ?, finished_at = ?
			 WHERE id = ? AND state IN (?, ?)`,
			state, snapshot, truncateRunes(errMsg, 2000), string(res), formatTime(at), id, jobs.StateQueued, jobs.StateRunning)
		if err != nil {
			return fmt.Errorf("finishing job %s: %w", id, err)
		}
		if err := expectOneRow(r, fmt.Sprintf("unfinished job %s", id)); err != nil {
			return err
		}
		return addNotifyEvent(ctx, tx, id, at)
	})
}

// truncateRunes makes s valid UTF-8 and cuts it to at most n characters.
func truncateRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// QueueState explains why a queued job has not started yet, following the
// rules of ClaimNextJob: the oldest queued job whose stack has no running
// job starts next, as long as fewer than the limit are running; host jobs
// run alone.
type QueueState struct {
	// Blocking is the job the job waits for: a running job of the same
	// stack or a running host job (for a host job: any running job), else
	// the oldest job of the same stack or host job queued before it. Nil if
	// there is none.
	Blocking *Job
	// Ahead counts the jobs of all stacks queued before this one.
	Ahead int
	// Running counts the jobs running now (all stacks).
	Running int
}

// QueueState returns the queue state of the queued job j.
func (s *Store) QueueState(ctx context.Context, j Job) (QueueState, error) {
	var qs QueueState
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM jobs WHERE state = ? AND id < ?),
		        (SELECT COUNT(*) FROM jobs WHERE state = ?)`,
		jobs.StateQueued, j.ID, jobs.StateRunning).Scan(&qs.Ahead, &qs.Running); err != nil {
		return qs, fmt.Errorf("queue state of job %s: %w", j.ID, err)
	}
	blocking, err := s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+
		`WHERE j.id <> :id AND (
		   (j.state = :running AND (:stack IS NULL OR j.stack_id = :stack OR j.stack_id IS NULL))
		   OR (j.state = :queued AND j.id < :id AND (j.stack_id IS :stack OR j.stack_id IS NULL)))
		 ORDER BY (j.state = :running) DESC, j.id LIMIT 1`,
		sql.Named("id", j.ID), sql.Named("stack", nullID(j.StackID)),
		sql.Named("running", jobs.StateRunning), sql.Named("queued", jobs.StateQueued))
	if err != nil {
		return qs, fmt.Errorf("queue state of job %s: %w", j.ID, err)
	}
	if len(blocking) == 1 {
		qs.Blocking = &blocking[0]
	}
	return qs, nil
}
