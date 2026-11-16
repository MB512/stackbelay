package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/secrets"
	"github.com/MB512/stackvault/internal/store"
)

// fakeDocker plays the worker containers.
type fakeDocker struct {
	mu         sync.Mutex
	started    []docker.ContainerSpec
	containers map[string]docker.ContainerState
	removed    []string
	signals    []string
	runErr     error
}

func (f *fakeDocker) RunContainer(_ context.Context, spec docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return "", f.runErr
	}
	f.started = append(f.started, spec)
	f.containers[spec.Name] = docker.ContainerState{Exists: true, Status: "running"}
	return "cid-" + spec.Name, nil
}

func (f *fakeDocker) InspectContainer(_ context.Context, name string) (docker.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[name], nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeDocker) SignalContainer(_ context.Context, name, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, name+" "+signal)
	return nil
}

func (f *fakeDocker) set(name string, st docker.ContainerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !st.Exists {
		delete(f.containers, name)
		return
	}
	f.containers[name] = st
}

type fakeRefresher struct {
	mu     sync.Mutex
	stacks []int64
}

func (f *fakeRefresher) Refresh(_ context.Context, stackID int64) ([]store.CachedSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stacks = append(f.stacks, stackID)
	return nil, nil
}

type env struct {
	t       *testing.T
	st      *store.Store
	d       *Dispatcher
	docker  *fakeDocker
	refresh *fakeRefresher
	web, db int64
	jobsDir string
	clock   time.Time
	clockMu sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	box, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SyncStacks(ctx, []string{"web", "db", "norepo"}); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, docker: &fakeDocker{containers: map[string]docker.ContainerState{}},
		refresh: &fakeRefresher{}, jobsDir: t.TempDir(), clock: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
	for _, name := range []string{"web", "db"} {
		s, _ := st.StackByName(ctx, name)
		if _, err := st.SetRepository(ctx, s.ID, "rest:https://backup.example/"+name, "repo-pw-"+name,
			map[string]string{"RESTIC_REST_USERNAME": "u", "RESTIC_REST_PASSWORD": "rest-pw-" + name}); err != nil {
			t.Fatal(err)
		}
		if name == "web" {
			e.web = s.ID
		} else {
			e.db = s.ID
		}
	}
	e.d = e.newDispatcher()
	return e
}

func (e *env) now() time.Time {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	e.clock = e.clock.Add(time.Second) // every call moves on, so ids differ
	return e.clock
}

// newDispatcher returns a dispatcher on the same store and Docker, like a
// restarted manager.
func (e *env) newDispatcher() *Dispatcher {
	return &Dispatcher{
		Store: e.st, Docker: e.docker, JobsDir: e.jobsDir, Snapshots: e.refresh, MaxRunning: 2, Now: e.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Template: Template{Image: "stackvault:test", StacksRoot: "/home/docker", Socket: "/var/run/docker.sock",
			Hostname: "host-a", Jobs: docker.Mount{Type: "bind", Source: "/srv/stackvault/data/jobs"}},
	}
}

func (e *env) submit(stackName string, typ jobs.Type, opts store.JobOptions) store.Job {
	e.t.Helper()
	s, err := e.st.StackByName(context.Background(), stackName)
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.d.Submit(context.Background(), store.Job{StackID: s.ID, Type: typ, Trigger: store.TriggerManual, Options: opts})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) job(id string) store.Job {
	e.t.Helper()
	j, err := e.st.JobByID(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// workerDone simulates a worker that wrote its final status and exited.
func (e *env) workerDone(id string, st jobs.Status) {
	e.t.Helper()
	st.ID = id
	if err := jobs.WriteStatus(filepath.Join(e.jobsDir, id), st); err != nil {
		e.t.Fatal(err)
	}
	e.docker.set(ContainerName(id), docker.ContainerState{Exists: true, Status: "exited"})
}

func TestDispatchLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	b1 := e.submit("web", jobs.TypeBackup, store.JobOptions{PreStop: true})
	r1 := e.submit("web", jobs.TypeRestore, store.JobOptions{SnapshotID: "aaaaaaaa11111111"})
	b2 := e.submit("db", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)

	// web's backup and db's backup run; web's restore waits for the stack.
	if got := e.job(b1.ID).State; got != jobs.StateRunning {
		t.Fatalf("b1: %s", got)
	}
	if got := e.job(r1.ID).State; got != jobs.StateQueued {
		t.Fatalf("r1 must wait for the per-stack lock: %s", got)
	}
	if got := e.job(b2.ID).State; got != jobs.StateRunning {
		t.Fatalf("b2: %s", got)
	}
	if len(e.docker.started) != 2 {
		t.Fatalf("started %d containers", len(e.docker.started))
	}

	// The worker container follows the template; credentials only as env.
	cs := e.docker.started[0]
	if cs.Name != ContainerName(b1.ID) || cs.Image != "stackvault:test" || cs.Labels[LabelJob] != b1.ID ||
		cs.Env["RESTIC_REPOSITORY"] != "rest:https://backup.example/web" || cs.Env["RESTIC_PASSWORD"] != "repo-pw-web" ||
		cs.Env["RESTIC_REST_PASSWORD"] != "rest-pw-web" || cs.Env["STACKS_ROOT"] != "/home/docker" ||
		cs.Env["STACKVAULT_HOSTNAME"] != "host-a" || strings.Join(cs.Cmd, " ") != "/app/worker run /job" {
		t.Fatalf("container spec: %+v", cs)
	}
	if cs.Mounts[2] != (docker.Mount{Type: "bind", Source: "/srv/stackvault/data/jobs/" + b1.ID, Target: "/job"}) {
		t.Fatalf("job mount: %+v", cs.Mounts[2])
	}
	spec, err := jobs.ReadSpec(filepath.Join(e.jobsDir, b1.ID))
	if err != nil || spec.Stack != "web" || spec.Type != jobs.TypeBackup || !spec.PreStop {
		t.Fatalf("spec.json: %+v %v", spec, err)
	}
	raw, _ := os.ReadFile(filepath.Join(e.jobsDir, b1.ID, jobs.SpecFile))
	if strings.Contains(string(raw), "repo-pw") || strings.Contains(string(raw), "rest-pw") {
		t.Fatal("spec.json contains credentials")
	}

	// Still running: nothing changes.
	e.d.Step(ctx)
	if got := e.job(b1.ID).State; got != jobs.StateRunning {
		t.Fatalf("b1 while running: %s", got)
	}

	// Final status written, but the container has not exited yet: wait,
	// so the container can be removed when the job is finished.
	if err := jobs.WriteStatus(filepath.Join(e.jobsDir, b1.ID), jobs.Status{ID: b1.ID, State: jobs.StateSucceeded}); err != nil {
		t.Fatal(err)
	}
	e.d.Step(ctx)
	if got := e.job(b1.ID).State; got != jobs.StateRunning || len(e.docker.removed) != 0 {
		t.Fatalf("b1 with final status but running container: %s, removed %v", got, e.docker.removed)
	}

	// The backup worker finishes: result recorded, container removed,
	// snapshots refreshed, and the restore on web starts.
	e.workerDone(b1.ID, jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222", PreStopSnapshot: "cccccccc33333333"})
	e.d.Step(ctx)
	j := e.job(b1.ID)
	if j.State != jobs.StateSucceeded || j.Snapshot != "bbbbbbbb22222222" || j.Result.PreStopSnapshot != "cccccccc33333333" || j.FinishedAt.IsZero() {
		t.Fatalf("b1 after finish: %+v", j)
	}
	if len(e.docker.removed) != 1 || e.docker.removed[0] != ContainerName(b1.ID) {
		t.Fatalf("removed: %v", e.docker.removed)
	}
	if len(e.refresh.stacks) != 1 || e.refresh.stacks[0] != e.web {
		t.Fatalf("refreshed: %v", e.refresh.stacks)
	}
	if got := e.job(r1.ID).State; got != jobs.StateRunning {
		t.Fatalf("r1 after the lock was released: %s", got)
	}

	// The db worker crashes without a final status.
	e.docker.set(ContainerName(b2.ID), docker.ContainerState{Exists: true, Status: "exited", ExitCode: 137})
	e.d.Step(ctx)
	if j := e.job(b2.ID); j.State != jobs.StateFailed || !strings.Contains(j.Error, "exit code 137") {
		t.Fatalf("crashed worker: %+v", j)
	}

	// The restore worker's container vanishes.
	e.docker.set(ContainerName(r1.ID), docker.ContainerState{})
	e.d.Step(ctx)
	if j := e.job(r1.ID); j.State != jobs.StateFailed || !strings.Contains(j.Error, "disappeared") {
		t.Fatalf("vanished worker: %+v", j)
	}

	actions := 0
	entries, _ := e.st.ListAudit(ctx, 100)
	for _, a := range entries {
		if a.Action == "job.finish" {
			actions++
		}
	}
	if actions != 3 {
		t.Fatalf("job.finish audit entries: %d", actions)
	}
}

func TestDispatchStartFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	j := e.submit("norepo", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	if got := e.job(j.ID); got.State != jobs.StateFailed || !strings.Contains(got.Error, "no repository") {
		t.Fatalf("no repository: %+v", got)
	}

	e.docker.runErr = errors.New("image not found")
	j = e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	if got := e.job(j.ID); got.State != jobs.StateFailed || !strings.Contains(got.Error, "image not found") {
		t.Fatalf("docker run failure: %+v", got)
	}
	// The lock is free again: the next job starts.
	e.docker.runErr = nil
	j = e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	if got := e.job(j.ID).State; got != jobs.StateRunning {
		t.Fatalf("next job: %s", got)
	}
}

func TestDispatchSurvivesManagerRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	j := e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)

	// Self-backup: the manager is stopped while the worker runs, then a
	// new manager process continues with the same database and files.
	restarted := e.newDispatcher()
	restarted.Step(ctx)
	if got := e.job(j.ID).State; got != jobs.StateRunning {
		t.Fatalf("after restart, still running: %s", got)
	}
	e.workerDone(j.ID, jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222"})
	restarted.Step(ctx)
	if got := e.job(j.ID); got.State != jobs.StateSucceeded || got.Snapshot != "bbbbbbbb22222222" {
		t.Fatalf("after restart, finished: %+v", got)
	}
	if len(e.docker.started) != 1 {
		t.Fatal("the job was started twice")
	}
}

// A manager that stopped after removing the container but before recording
// the result finishes the job on the next check.
func TestFinishAfterContainerAlreadyRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	j := e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	e.workerDone(j.ID, jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222"})
	e.docker.set(ContainerName(j.ID), docker.ContainerState{}) // already removed
	e.newDispatcher().Step(ctx)
	if got := e.job(j.ID); got.State != jobs.StateSucceeded || got.Snapshot != "bbbbbbbb22222222" {
		t.Fatalf("job: %+v", got)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	running := e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	queued := e.submit("web", jobs.TypeRestore, store.JobOptions{SnapshotID: "aaaaaaaa11111111"})

	if err := e.d.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.job(queued.ID).State; got != jobs.StateCancelled {
		t.Fatalf("queued cancel: %s", got)
	}
	if err := e.d.Cancel(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.docker.signals) != 1 || e.docker.signals[0] != ContainerName(running.ID)+" SIGTERM" {
		t.Fatalf("signals: %v", e.docker.signals)
	}
	// The worker reports "cancelled" itself.
	e.workerDone(running.ID, jobs.Status{State: jobs.StateCancelled, Error: "context canceled"})
	e.d.Step(ctx)
	if got := e.job(running.ID).State; got != jobs.StateCancelled {
		t.Fatalf("running cancel: %s", got)
	}
	if err := e.d.Cancel(ctx, running.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel finished job: %v", err)
	}
	if err := e.d.Cancel(ctx, "01AAAAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel unknown job: %v", err)
	}
}

func TestRunWakesOnSubmit(t *testing.T) {
	e := newEnv(t)
	e.d.PollInterval = time.Hour // only a wake-up can start the job quickly
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.d.Run(ctx); close(done) }()
	j := e.submit("web", jobs.TypeBackup, store.JobOptions{})
	deadline := time.Now().Add(5 * time.Second)
	for e.job(j.ID).State != jobs.StateRunning {
		if time.Now().After(deadline) {
			t.Fatal("job not started after Submit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// writeJobDir writes job files as the dispatcher and a worker would.
func (e *env) writeJobDir(spec jobs.Spec, status *jobs.Status) {
	e.t.Helper()
	dir := filepath.Join(e.jobsDir, spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := jobs.WriteSpec(dir, spec); err != nil {
		e.t.Fatal(err)
	}
	if status != nil {
		status.ID = spec.ID
		if err := jobs.WriteStatus(dir, *status); err != nil {
			e.t.Fatal(err)
		}
	}
}

// After the manager restored its own stack, the database comes from the
// snapshot, but the job files were carried over and are newer.
func TestReconcileAfterSelfRestore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.CreateUser(ctx, store.User{Username: "alice", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	newID := func(i int) string {
		id, err := jobs.NewID(t0.Add(time.Duration(i) * time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// The self-backup was running when its snapshot was taken ...
	backup := e.submit("web", jobs.TypeBackup, store.JobOptions{})
	e.d.Step(ctx)
	e.docker.set(ContainerName(backup.ID), docker.ContainerState{}) // long gone
	// ... and actually succeeded (its files were carried over).
	e.workerDone(backup.ID, jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222"})

	// The restore itself: unknown to the restored database, still running.
	restoreID := newID(1)
	started := t0.Add(90 * time.Second)
	e.writeJobDir(jobs.Spec{ID: restoreID, Type: jobs.TypeRestore, Stack: "web", SnapshotID: "bbbbbbbb22222222",
		CarryOverJobs: "data/jobs", Trigger: "manual", Requester: "alice"},
		&jobs.Status{State: jobs.StateRunning, StartedAt: &started})
	e.docker.set(ContainerName(restoreID), docker.ContainerState{Exists: true, Status: "running"})
	// A job whose worker never started, a job of a vanished stack, junk.
	neverID := newID(2)
	e.writeJobDir(jobs.Spec{ID: neverID, Type: jobs.TypeBackup, Stack: "db"}, nil)
	e.writeJobDir(jobs.Spec{ID: newID(3), Type: jobs.TypeBackup, Stack: "gone"}, &jobs.Status{State: jobs.StateSucceeded})
	if err := os.Mkdir(filepath.Join(e.jobsDir, "not-a-job"), 0o700); err != nil {
		t.Fatal(err)
	}

	restarted := e.newDispatcher()
	restarted.Reconcile(ctx)

	if j := e.job(backup.ID); j.State != jobs.StateSucceeded || j.Snapshot != "bbbbbbbb22222222" {
		t.Fatalf("self-backup: %+v", j)
	}
	r := e.job(restoreID)
	if r.State != jobs.StateRunning || r.Type != jobs.TypeRestore || r.Requester != "alice" || r.StackName != "web" ||
		r.Options.SnapshotID != "bbbbbbbb22222222" || !r.QueuedAt.Equal(t0.Add(time.Minute)) || !r.StartedAt.Equal(started) {
		t.Fatalf("imported restore: %+v", r)
	}
	if j := e.job(neverID); j.State != jobs.StateFailed || !strings.Contains(j.Error, "without a result") {
		t.Fatalf("job without status: %+v", j)
	}
	if all, _ := e.st.ListJobs(ctx, 0, 100); len(all) != 3 {
		t.Fatalf("jobs: %d, want 3 (the unknown stack is skipped)", len(all))
	}

	// The imported restore is followed like any running job.
	restarted.Step(ctx)
	if got := e.job(restoreID).State; got != jobs.StateRunning {
		t.Fatalf("restore while its worker runs: %s", got)
	}
	e.workerDone(restoreID, jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222", StartedAt: &started})
	restarted.Step(ctx)
	if got := e.job(restoreID).State; got != jobs.StateSucceeded {
		t.Fatalf("restore after its worker finished: %s", got)
	}

	// Reconciling again changes nothing.
	restarted.Reconcile(ctx)
	if all, _ := e.st.ListJobs(ctx, 0, 100); len(all) != 3 {
		t.Fatalf("second reconcile: %d jobs", len(all))
	}
}

func TestRestoreOfManagerStackCarriesJobsOver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.d.Template.Jobs = docker.Mount{Type: "bind", Source: "/home/docker/web/data/jobs"} // the manager lives in "web"
	own := e.submit("web", jobs.TypeRestore, store.JobOptions{SnapshotID: "aaaaaaaa11111111", InPlace: true})
	other := e.submit("db", jobs.TypeRestore, store.JobOptions{SnapshotID: "aaaaaaaa11111111"})
	e.d.Step(ctx)
	for id, want := range map[string]string{own.ID: "data/jobs", other.ID: ""} {
		spec, err := jobs.ReadSpec(filepath.Join(e.jobsDir, id))
		if err != nil {
			t.Fatal(err)
		}
		if spec.CarryOverJobs != want || spec.Trigger != store.TriggerManual {
			t.Errorf("spec of %s: carry-over %q, trigger %q", spec.Stack, spec.CarryOverJobs, spec.Trigger)
		}
		if spec.InPlace != (id == own.ID) { // in place, the carry-over directory is excluded from the restore
			t.Errorf("spec of %s: in place %t", spec.Stack, spec.InPlace)
		}
	}
}

// The restarted manager may find a job that already finished: imported as
// final, and its exited container is removed. One whose container still
// runs is imported as running and finished by the regular check.
func TestReconcileImportRemovesExitedContainer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	done, _ := jobs.NewID(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	busy, _ := jobs.NewID(time.Date(2026, 11, 14, 9, 1, 0, 0, time.UTC))
	e.writeJobDir(jobs.Spec{ID: done, Type: jobs.TypeBackup, Stack: "web"}, &jobs.Status{State: jobs.StateSucceeded, Snapshot: "aaaaaaaa11111111"})
	e.docker.set(ContainerName(done), docker.ContainerState{Exists: true, Status: "exited"})
	e.writeJobDir(jobs.Spec{ID: busy, Type: jobs.TypeBackup, Stack: "db"}, &jobs.Status{State: jobs.StateSucceeded, Snapshot: "bbbbbbbb22222222"})
	e.docker.set(ContainerName(busy), docker.ContainerState{Exists: true, Status: "running"}) // about to exit

	e.d.Reconcile(ctx)
	if j := e.job(done); j.State != jobs.StateSucceeded || j.Snapshot != "aaaaaaaa11111111" {
		t.Fatalf("finished job: %+v", j)
	}
	if j := e.job(busy); j.State != jobs.StateRunning {
		t.Fatalf("job with running container: %+v", j)
	}
	if len(e.docker.removed) != 1 || e.docker.removed[0] != ContainerName(done) {
		t.Fatalf("removed: %v", e.docker.removed)
	}
	e.docker.set(ContainerName(busy), docker.ContainerState{Exists: true, Status: "exited"})
	e.d.Step(ctx)
	if j := e.job(busy); j.State != jobs.StateSucceeded || j.Snapshot != "bbbbbbbb22222222" {
		t.Fatalf("after exit: %+v", j)
	}
	if len(e.docker.removed) != 2 {
		t.Fatalf("removed: %v", e.docker.removed)
	}
}

// An update needs the repository unless the user confirmed an update
// without backup; a stack with repository is always backed up.
func TestUpdateRepositoryHandling(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.d.Template.Jobs = docker.Mount{Type: "bind", Source: "/home/docker/web/data/jobs"} // the manager lives in "web"

	unconfirmed := e.submit("norepo", jobs.TypeUpdate, store.JobOptions{})
	e.d.Step(ctx)
	if got := e.job(unconfirmed.ID); got.State != jobs.StateFailed || !strings.Contains(got.Error, "no repository") {
		t.Fatalf("update without repository and confirmation: %+v", got)
	}

	// The rollback option is dropped without backup (there is no rollback).
	opts := store.JobOptions{NoBackup: true, OnlyIfNew: true, RollbackInPlace: true}
	confirmed := e.submit("norepo", jobs.TypeUpdate, opts)
	withRepo := e.submit("web", jobs.TypeUpdate, opts) // repository wins
	e.d.Step(ctx)
	for _, tc := range []struct {
		id           string
		wantNoBackup bool
		wantCarry    string
		wantRepoEnv  bool
	}{
		{confirmed.ID, true, "", false},
		{withRepo.ID, false, "data/jobs", true},
	} {
		if got := e.job(tc.id).State; got != jobs.StateRunning {
			t.Fatalf("job %s: %s", tc.id, got)
		}
		spec, err := jobs.ReadSpec(filepath.Join(e.jobsDir, tc.id))
		if err != nil {
			t.Fatal(err)
		}
		if spec.NoBackup != tc.wantNoBackup || spec.CarryOverJobs != tc.wantCarry {
			t.Errorf("spec of %s: no_backup %v, carry-over %q", spec.Stack, spec.NoBackup, spec.CarryOverJobs)
		}
		if !spec.OnlyIfNew || spec.RollbackInPlace != !tc.wantNoBackup {
			t.Errorf("spec of %s: only_if_new %v, rollback_in_place %v", spec.Stack, spec.OnlyIfNew, spec.RollbackInPlace)
		}
		var env map[string]string
		for _, cs := range e.docker.started {
			if cs.Name == ContainerName(tc.id) {
				env = cs.Env
			}
		}
		if _, ok := env["RESTIC_REPOSITORY"]; ok != tc.wantRepoEnv {
			t.Errorf("spec of %s: RESTIC_REPOSITORY set = %v", spec.Stack, ok)
		}
	}

	// A rolled-back update keeps its result.
	e.workerDone(withRepo.ID, jobs.Status{State: jobs.StateFailed, Snapshot: "aaaaaaaa11111111",
		PreviousDir: "/home/docker/web.old-x", RolledBack: true, Error: "update failed and was rolled back"})
	e.d.Step(ctx)
	if got := e.job(withRepo.ID); !got.Result.RolledBack || got.Result.PreviousDir == "" || got.Snapshot == "" {
		t.Fatalf("rolled-back update: %+v", got)
	}
}

// A compose-apply job gets its new file in the job directory and the
// checksums in the spec.
func TestComposeApplyHandoff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	content := []byte("services: {}\n")
	sum := jobs.SHA256Hex(content)
	base := jobs.SHA256Hex([]byte("old"))
	j, err := e.d.Submit(ctx, store.Job{StackID: e.web, Type: jobs.TypeComposeApply, Trigger: store.TriggerManual,
		ComposeContent: content, Options: store.JobOptions{ComposeFile: "compose.yml", ComposeSHA256: sum, ComposeBaseSHA256: base}})
	if err != nil {
		t.Fatal(err)
	}
	e.d.Step(ctx)
	if got := e.job(j.ID).State; got != jobs.StateRunning {
		t.Fatalf("state %s: %+v", got, e.job(j.ID))
	}
	spec, err := jobs.ReadSpec(filepath.Join(e.jobsDir, j.ID))
	if err != nil {
		t.Fatal(err)
	}
	if spec.ComposeSHA256 != sum || spec.ComposeBaseSHA256 != base || spec.NoBackup {
		t.Fatalf("spec %+v", spec)
	}
	got, err := os.ReadFile(filepath.Join(e.jobsDir, j.ID, jobs.ComposeNewFile))
	if err != nil || string(got) != string(content) {
		t.Fatalf("compose.new %q, %v", got, err)
	}

	// Without repository only with confirmation (like an update).
	n := e.submit("norepo", jobs.TypeComposeApply, store.JobOptions{ComposeSHA256: sum, ComposeBaseSHA256: base})
	e.d.Step(ctx)
	if got := e.job(n.ID); got.State != jobs.StateFailed || !strings.Contains(got.Error, "no repository") {
		t.Fatalf("unconfirmed without repository: %+v", got)
	}
}
