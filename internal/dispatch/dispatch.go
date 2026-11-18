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
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
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
	// CleanupImageMinAge is the minimum age rule of host cleanups (see
	// jobs.CleanupOptions); images replaced by updates within it are kept.
	CleanupImageMinAge time.Duration
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
	d.Reconcile(ctx)
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

// Reconcile brings the database in line with the job directories. Run
// calls it once at start. The job files are the more recent record after
// the manager restored its own stack (the database comes from the
// snapshot, the job files were carried over from the previous version):
//
//   - a job the database still has as queued or running, whose directory
//     has a final status, is finished from that status;
//   - a job directory the database does not know is imported (e.g. the
//     restore itself, or jobs that ran after the restored snapshot).
func (d *Dispatcher) Reconcile(ctx context.Context) {
	entries, err := os.ReadDir(d.JobsDir)
	if err != nil {
		d.Logger.Error("reconciling jobs: reading jobs directory", "error", err)
		return
	}
	// Two passes: finishing known jobs first releases their stacks' locks,
	// so an imported running job does not collide with a stale one.
	for _, importing := range []bool{false, true} {
		for _, e := range entries {
			if e.IsDir() && jobs.ValidJobID(e.Name()) {
				d.reconcileDir(ctx, e.Name(), importing)
			}
		}
	}
}

// reconcileDir finishes a known job from its files, or (importing) imports
// an unknown one.
func (d *Dispatcher) reconcileDir(ctx context.Context, id string, importing bool) {
	log := d.Logger.With("job_id", id)
	dir := d.jobDir(id)
	status, statusErr := jobs.ReadStatus(dir)
	final := statusErr == nil && status.ID == id && status.State.Final()
	// A worker that is still running (it may have just written its final
	// status) is left to the regular check, which finishes the job once the
	// container has exited and then removes it.
	cs, err := d.Docker.InspectContainer(ctx, ContainerName(id))
	if err != nil {
		log.Warn("reconciling jobs: inspecting worker container", "error", err)
		return
	}
	if cs.Running() {
		final = false
	}

	known, err := d.Store.JobByID(ctx, id)
	switch {
	case err == nil:
		if !importing && !known.State.Final() && final {
			log.Info("reconciling jobs: finishing job from its job files", "state", status.State)
			d.removeContainer(ctx, ContainerName(id))
			d.finishWithStatus(ctx, known, status)
		}
		return
	case !errors.Is(err, store.ErrNotFound):
		log.Error("reconciling jobs: loading job", "error", err)
		return
	case !importing:
		return
	}

	spec, err := jobs.ReadSpec(dir)
	if err != nil {
		log.Warn("reconciling jobs: skipping job directory", "error", err)
		return
	}
	var st store.Stack // zero for a host job
	if !spec.Type.Host() {
		if st, err = d.Store.StackByName(ctx, spec.Stack); err != nil {
			log.Warn("reconciling jobs: skipping job of unknown stack", "stack", spec.Stack, "error", err)
			return
		}
	}
	queued, _ := jobs.IDTime(id)
	j := store.Job{
		ID: id, StackID: st.ID, StackName: st.Name, Type: spec.Type, Trigger: spec.Trigger, QueuedAt: queued,
		Options: store.JobOptions{PreStop: spec.PreStop, SnapshotID: spec.SnapshotID,
			SkipSafetySnapshot: spec.SkipSafetySnapshot, NoStart: spec.NoStart, PinImages: spec.PinImages,
			InPlace: spec.InPlace, Import: spec.Import, LimitDownloadKiB: spec.LimitDownloadKiB, NoBackup: spec.NoBackup, OnlyIfNew: spec.OnlyIfNew, RollbackInPlace: spec.RollbackInPlace,
			NoPreStop: spec.NoPreStop},
	}
	if j.Trigger == "" {
		j.Trigger = store.TriggerManual
	}
	switch {
	case statusErr == nil && status.ID == id:
		j.State, j.Snapshot, j.Error = status.State, status.Snapshot, status.Error
		j.Result = resultOf(status)
		if status.StartedAt != nil {
			j.StartedAt = *status.StartedAt
		}
		if status.FinishedAt != nil {
			j.FinishedAt = *status.FinishedAt
		}
	default: // the worker never wrote a status
		j.State, j.FinishedAt = jobs.StateFailed, d.now()
		j.Error = "found in the job files without a result (the worker did not start)"
	}
	if j.State == jobs.StateQueued { // workers never write "queued"
		j.State = jobs.StateFailed
	}
	switch {
	case cs.Running():
		j.State, j.FinishedAt, j.Snapshot, j.Error, j.Result = jobs.StateRunning, time.Time{}, "", "", store.JobResult{}
	case cs.Exists:
		d.removeContainer(ctx, ContainerName(id))
	}
	if err := d.Store.ImportJob(ctx, j, spec.Requester); err != nil {
		log.Warn("reconciling jobs: importing job", "error", err)
		return
	}
	log.Info("reconciling jobs: imported job from its job files", "stack", st.Name, "type", j.Type, "state", j.State)
	if err := d.Store.AddAudit(ctx, store.AuditEntry{At: d.now(), Action: "job.import", StackID: st.ID,
		Detail: fmt.Sprintf("job %s (%s, %s) rebuilt from its job files", id, j.Type, j.State)}); err != nil {
		d.Logger.Error("writing audit log", "error", err)
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
	// Every job needs the repository, except stop/start/restart (they get
	// no credentials at all) and an update or compose apply the user
	// confirmed to run without backup. Such a job on a stack that has a
	// repository always backs up, even if it had none when it was queued.
	changes := j.Type == jobs.TypeUpdate || j.Type == jobs.TypeComposeApply
	var resticRepo restic.Repo
	noBackup := false
	if !j.Type.Control() && !j.Type.Host() {
		repo, err := d.Store.RepositoryForStack(ctx, j.StackID)
		switch {
		case err == nil:
			resticRepo = repo.Restic()
		case errors.Is(err, store.ErrNotFound) && changes && j.Options.NoBackup:
			noBackup = true
			log.Warn("changing the stack without backup: it has no repository (confirmed when the job was queued)")
		case errors.Is(err, store.ErrNotFound):
			fail("starting job", errors.New("the stack has no repository"))
			return
		default:
			fail("loading repository", err)
			return
		}
	}
	dir := d.jobDir(j.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		fail("creating job directory", err)
		return
	}
	spec := jobs.Spec{
		ID: j.ID, Type: j.Type, Stack: j.StackName,
		PreStop: j.Options.PreStop, SnapshotID: j.Options.SnapshotID,
		SkipSafetySnapshot: j.Options.SkipSafetySnapshot, NoStart: j.Options.NoStart, PinImages: j.Options.PinImages,
		InPlace: j.Options.InPlace, Import: j.Options.Import, LimitDownloadKiB: j.Options.LimitDownloadKiB,
		OnlyIfNew: j.Options.OnlyIfNew, NoPreStop: j.Options.NoPreStop, Trigger: j.Trigger, Requester: j.Requester, NoBackup: noBackup,
		// Without backup there is no rollback, in place or not.
		RollbackInPlace: j.Options.RollbackInPlace && !noBackup,
	}
	if j.Type == jobs.TypeComposeApply {
		spec.ComposeSHA256, spec.ComposeBaseSHA256 = j.Options.ComposeSHA256, j.Options.ComposeBaseSHA256
		content, err := d.Store.JobComposeContent(ctx, j.ID)
		if err != nil {
			fail("loading the new compose file", err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, jobs.ComposeNewFile), content, 0o600); err != nil {
			fail("writing the new compose file", err)
			return
		}
	}
	if j.Type == jobs.TypeCleanup {
		if err := d.cleanupSpec(ctx, &spec, log); err != nil {
			fail("preparing cleanup", err)
			return
		}
	}
	if (j.Type == jobs.TypeRestore && !j.Options.Import) || (changes && !noBackup) {
		// Restoring the manager's own stack (also as an update's
		// rollback): keep the job files.
		spec.CarryOverJobs = d.Template.JobsDirInStack(j.StackName)
	}
	if err := jobs.WriteSpec(dir, spec); err != nil {
		fail("writing job spec", err)
		return
	}
	cs, err := d.Template.Spec(j.ID, resticRepo)
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

// cleanupSpec sets the rules of a host cleanup: the minimum age and the
// images updates and compose applies replaced within it, which the
// cleanup keeps (Docker records no "dangling since" time).
func (d *Dispatcher) cleanupSpec(ctx context.Context, spec *jobs.Spec, log *slog.Logger) error {
	spec.ImageMinAgeHours = int(d.CleanupImageMinAge / time.Hour)
	replaced, err := d.Store.ReplacedImagesSince(ctx, d.now().Add(-d.CleanupImageMinAge))
	if err != nil {
		return err
	}
	for _, id := range replaced {
		if !docker.ValidImageID(id) {
			continue
		}
		if len(spec.KeepImages) == jobs.MaxKeepImages {
			// More than a spec can carry: the rest is protected only by
			// its pull/tag time.
			log.Warn("too many recently replaced images to keep by id", "kept", jobs.MaxKeepImages, "total", len(replaced))
			break
		}
		spec.KeepImages = append(spec.KeepImages, id)
	}
	return nil
}

// check looks at a running job: a final status.json finishes it; a worker
// container that is gone or stopped without a final status fails it.
//
// The job is finished only once the container has stopped, and the
// container is removed before the result is recorded: if the manager
// stops in between (e.g. it is part of the stack being backed up), the job
// is still "running" and the next check finishes it. The other order could
// leave a container behind for a job that is never checked again.
func (d *Dispatcher) check(ctx context.Context, j store.Job) {
	name := ContainerName(j.ID)
	cs, err := d.Docker.InspectContainer(ctx, name)
	if err != nil {
		d.Logger.Warn("inspecting worker container", "job_id", j.ID, "error", err)
		return // Docker unreachable: try again later
	}
	if cs.Running() {
		// Even with a final status: the worker writes it right before it
		// exits, and a running container cannot be removed. The next round
		// finishes the job.
		return
	}
	// The worker may have written its final status right before exiting.
	st, ok, readErr := d.finalStatus(j)
	d.removeContainer(ctx, name)
	if ok {
		d.finishWithStatus(ctx, j, st)
		return
	}
	msg := "the worker container disappeared without reporting a result"
	if cs.Exists {
		msg = fmt.Sprintf("the worker container stopped (exit code %d) without reporting a result", cs.ExitCode)
	}
	if readErr != nil {
		msg += fmt.Sprintf(" (%v)", readErr)
	}
	d.Logger.Error("job lost", "job_id", j.ID, "stack", j.StackName, "reason", msg)
	d.finish(ctx, j, jobs.StateFailed, "", msg, store.JobResult{})
}

// finalStatus returns the worker's final status, if it wrote one. err is
// set if a status file exists but cannot be read.
func (d *Dispatcher) finalStatus(j store.Job) (st jobs.Status, ok bool, err error) {
	st, err = jobs.ReadStatus(d.jobDir(j.ID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, false, nil
	case err != nil:
		d.Logger.Warn("reading job status", "job_id", j.ID, "error", err)
		return st, false, err
	}
	return st, st.ID == j.ID && st.State.Final(), nil
}

func (d *Dispatcher) finishWithStatus(ctx context.Context, j store.Job, st jobs.Status) {
	d.finish(ctx, j, st.State, st.Snapshot, st.Error, resultOf(st))
}

// resultOf returns the job result a worker reported in its final status.
func resultOf(st jobs.Status) store.JobResult {
	return store.JobResult{
		PreStopSnapshot: st.PreStopSnapshot, SafetySnapshot: st.SafetySnapshot, PreviousDir: st.PreviousDir,
		RolledBack: st.RolledBack, Unchanged: st.Unchanged, ImagesChanged: st.ImagesChanged,
		ReplacedImages:    st.ReplacedImages,
		RemovedContainers: st.RemovedContainers, RemovedImages: st.RemovedImages, FreedBytes: st.FreedBytes,
	}
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
	if j.Options.Import {
		d.markImported(ctx, j)
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

// markImported records that an imported stack's directory exists after its
// first restore (it may also exist after a failed start). Discovery decides,
// as at startup.
func (d *Dispatcher) markImported(ctx context.Context, j store.Job) {
	if d.Template.StacksRoot == "" {
		return
	}
	if _, err := stacks.Lookup(d.Template.StacksRoot, j.StackName); err != nil {
		return
	}
	if err := d.Store.SetStackPresent(ctx, j.StackID, true); err != nil {
		d.Logger.Error("marking imported stack present", "stack", j.StackName, "error", err)
		return
	}
	d.Logger.Info("imported stack is on disk now", "stack", j.StackName)
}

func (d *Dispatcher) removeContainer(ctx context.Context, name string) {
	if err := d.Docker.RemoveContainer(ctx, name); err != nil {
		d.Logger.Warn("removing worker container", "container", name, "error", err)
	}
}
