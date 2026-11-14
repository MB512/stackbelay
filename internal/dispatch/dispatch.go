package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// Containers is what the dispatcher needs from Docker (*docker.CLI).
type Containers interface {
	RunContainer(ctx context.Context, spec docker.ContainerSpec) (string, error)
	InspectContainer(ctx context.Context, name string) (docker.ContainerState, error)
	RemoveContainer(ctx context.Context, name string) error
	SignalContainer(ctx context.Context, name, signal string) error
}

// SnapshotRefresher refreshes a stack's snapshot cache after a job.
type SnapshotRefresher interface {
	Refresh(ctx context.Context, stackID int64) ([]store.CachedSnapshot, error)
}

// Dispatcher starts queued jobs in worker containers and tracks them.
// All state lives in the database and the job directories, so a restarted
// manager (e.g. after its own stack was backed up) simply continues.
type Dispatcher struct {
	Store     *store.Store
	Docker    Containers
	Template  Template
	JobsDir   string // the jobs directory as the manager sees it
	Snapshots SnapshotRefresher
	Logger    *slog.Logger
	// MaxRunning limits jobs running at the same time (across stacks).
	MaxRunning int
	// PollInterval is how often running jobs are checked; default 2s.
	PollInterval time.Duration
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time

	// wake is signalled when a job was queued, so it starts without waiting
	// for the next poll. A buffered channel of size 1 works like an
	// AutoResetEvent in C#: many signals collapse into one wake-up.
	wake     chan struct{}
	wakeOnce sync.Once // creates wake exactly once, even from several goroutines (like Lazy<T>)
}

func (d *Dispatcher) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d *Dispatcher) wakeChan() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

// ErrNotCancellable is returned by Cancel for jobs that already finished.
var ErrNotCancellable = errors.New("the job has already finished")

// Submit queues a job and wakes the dispatcher. ID, State and QueuedAt are
// set here. The caller has checked permissions.
func (d *Dispatcher) Submit(ctx context.Context, j store.Job) (store.Job, error) {
	j.QueuedAt = d.now()
	id, err := jobs.NewID(j.QueuedAt)
	if err != nil {
		return store.Job{}, err
	}
	j.ID, j.State = id, jobs.StateQueued
	if err := d.Store.CreateJob(ctx, j); err != nil {
		return store.Job{}, err
	}
	d.Wake()
	return j, nil
}

// Wake makes the dispatcher look for work now. It never blocks.
func (d *Dispatcher) Wake() {
	select {
	case d.wakeChan() <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// Cancel cancels a job. A queued job is marked cancelled; a running job's
// worker gets SIGTERM, restarts the stack and reports "cancelled" itself.
func (d *Dispatcher) Cancel(ctx context.Context, id string) error {
	j, err := d.Store.JobByID(ctx, id)
	if err != nil {
		return err
	}
	switch j.State {
	case jobs.StateQueued:
		err := d.Store.FinishJob(ctx, id, jobs.StateCancelled, "", "cancelled before it started", store.JobResult{}, d.now())
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotCancellable // started or finished meanwhile
		}
		return err
	case jobs.StateRunning:
		// SIGTERM only, never SIGKILL: the worker must get the chance to
		// restart the stack it stopped.
		return d.Docker.SignalContainer(ctx, ContainerName(id), "SIGTERM")
	default:
		return ErrNotCancellable
	}
}

// Run processes jobs until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := d.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.wakeChan():
		}
	}
}

// Step checks running jobs and starts queued ones. Run calls it in a loop;
// tests call it directly.
func (d *Dispatcher) Step(ctx context.Context) {
	running, err := d.Store.JobsInState(ctx, jobs.StateRunning)
	if err != nil {
		d.Logger.Error("listing running jobs", "error", err)
		return
	}
	for _, j := range running {
		d.check(ctx, j)
	}
	maxRunning := max(d.MaxRunning, 1)
	for ctx.Err() == nil {
		j, ok, err := d.Store.ClaimNextJob(ctx, maxRunning, d.now())
		if err != nil {
			d.Logger.Error("claiming job", "error", err)
			return
		}
		if !ok {
			return
		}
		d.start(ctx, j)
	}
}

func (d *Dispatcher) jobDir(id string) string { return filepath.Join(d.JobsDir, id) }

// start writes the job spec and starts the worker container. Failures
// finish the job as failed.
func (d *Dispatcher) start(ctx context.Context, j store.Job) {
	log := d.Logger.With("job_id", j.ID, "stack", j.StackName, "type", j.Type)
	fail := func(msg string, err error) {
		log.Error(msg, "error", err)
		d.finish(ctx, j, jobs.StateFailed, "", fmt.Sprintf("%s: %v", msg, err), store.JobResult{})
	}
	if !jobs.ValidJobID(j.ID) {
		fail("starting job", fmt.Errorf("invalid job id %q", j.ID))
		return
	}
	repo, err := d.Store.RepositoryForStack(ctx, j.StackID)
	if errors.Is(err, store.ErrNotFound) {
		fail("starting job", errors.New("the stack has no repository"))
		return
	}
	if err != nil {
		fail("loading repository", err)
		return
	}
	dir := d.jobDir(j.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		fail("creating job directory", err)
		return
	}
	spec := jobs.Spec{
		ID: j.ID, Type: j.Type, Stack: j.StackName,
		PreStop: j.Options.PreStop, SnapshotID: j.Options.SnapshotID,
		SkipSafetySnapshot: j.Options.SkipSafetySnapshot, NoStart: j.Options.NoStart,
	}
	if err := jobs.WriteSpec(dir, spec); err != nil {
		fail("writing job spec", err)
		return
	}
	cs, err := d.Template.Spec(j.ID, repo.Restic())
	if err != nil {
		fail("building worker container", err)
		return
	}
	if _, err := d.Docker.RunContainer(ctx, cs); err != nil {
		fail("starting worker container", err)
		return
	}
	log.Info("worker started", "container", cs.Name)
}

// check looks at a running job: a final status.json finishes it; a worker
// container that is gone or stopped without a final status fails it.
func (d *Dispatcher) check(ctx context.Context, j store.Job) {
	name := ContainerName(j.ID)
	if d.finishFromStatus(ctx, j) {
		d.removeContainer(ctx, name)
		return
	}
	st, err := d.Docker.InspectContainer(ctx, name)
	if err != nil {
		d.Logger.Warn("inspecting worker container", "job_id", j.ID, "error", err)
		return // Docker unreachable: try again later
	}
	if st.Running() {
		return
	}
	// The worker may have written its final status right before exiting.
	if d.finishFromStatus(ctx, j) {
		d.removeContainer(ctx, name)
		return
	}
	msg := "the worker container disappeared without reporting a result"
	if st.Exists {
		msg = fmt.Sprintf("the worker container stopped (exit code %d) without reporting a result", st.ExitCode)
	}
	d.Logger.Error("job lost", "job_id", j.ID, "stack", j.StackName, "reason", msg)
	d.finish(ctx, j, jobs.StateFailed, "", msg, store.JobResult{})
	d.removeContainer(ctx, name)
}

// finishFromStatus records the worker's final status, if there is one.
func (d *Dispatcher) finishFromStatus(ctx context.Context, j store.Job) bool {
	st, err := jobs.ReadStatus(d.jobDir(j.ID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false
	case err != nil:
		d.Logger.Warn("reading job status", "job_id", j.ID, "error", err)
		return false
	case st.ID != j.ID || !st.State.Final():
		return false
	}
	d.finish(ctx, j, st.State, st.Snapshot, st.Error, store.JobResult{
		PreStopSnapshot: st.PreStopSnapshot, SafetySnapshot: st.SafetySnapshot, PreviousDir: st.PreviousDir,
	})
	return true
}

// finish records the final state and refreshes the snapshot cache if the
// job created snapshots.
func (d *Dispatcher) finish(ctx context.Context, j store.Job, state jobs.State, snapshot, errMsg string, res store.JobResult) {
	if err := d.Store.FinishJob(ctx, j.ID, state, snapshot, errMsg, res, d.now()); err != nil {
		d.Logger.Error("recording job result", "job_id", j.ID, "error", err)
		return
	}
	d.Logger.Info("job finished", "job_id", j.ID, "stack", j.StackName, "type", j.Type, "state", state, "error", errMsg)
	if err := d.Store.AddAudit(ctx, store.AuditEntry{At: d.now(), Action: "job.finish", StackID: j.StackID,
		Detail: fmt.Sprintf("job %s (%s): %s", j.ID, j.Type, state)}); err != nil {
		d.Logger.Error("writing audit log", "error", err)
	}
	// Backups and restores (safety snapshot) add snapshots, even failed ones.
	if d.Snapshots != nil && (snapshot != "" || res.PreStopSnapshot != "" || res.SafetySnapshot != "") {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := d.Snapshots.Refresh(rctx, j.StackID); err != nil {
			d.Logger.Warn("refreshing snapshots after job", "job_id", j.ID, "error", err)
		}
	}
}

func (d *Dispatcher) removeContainer(ctx context.Context, name string) {
	if err := d.Docker.RemoveContainer(ctx, name); err != nil {
		d.Logger.Warn("removing worker container", "container", name, "error", err)
	}
}
