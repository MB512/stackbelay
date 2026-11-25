// Command worker executes a single backup, restore, update, compose-apply,
// stop, start, restart or cleanup job and exits (compose-apply only
// through "run": its new compose file is in the job directory).
//
// Usage:
//
//	worker run <job-dir>
//	worker backup  <stack>
//	worker restore [--no-start] [--pin-images [--tag-fallback]] [--import] [--limit-download <KiB/s>] <stack> <snapshot-id>
//	worker update  [--no-backup] <stack>
//	worker stop|start|restart|up|down <stack>
//	worker cleanup [--image-min-age <duration>]
//
// "run" is how the manager starts a job: the job is described by
// <job-dir>/spec.json, and the worker writes status.json and log.txt there.
// The other commands are for manual use and tests. stop, start, restart,
// up and down run without backup and need no repository. cleanup is a host
// job (stopped orphan containers of all stacks, old dangling images; no
// repository).
//
// Flags must come before positional arguments. Configuration and repository
// credentials are read from the environment (see internal/config). Exit
// codes: 0 success, 1 job failed, 2 usage or configuration error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/MB512/stackbelay/internal/buildinfo"
	"github.com/MB512/stackbelay/internal/config"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/stacks"
)

const usage = `usage:
  worker run <job-dir>
  worker backup  <stack>
  worker restore [--no-start] [--pin-images [--tag-fallback]] [--import] [--limit-download <KiB/s>] <stack> <snapshot-id>
  worker update  [--no-backup] <stack>
  worker stop|start|restart|up|down <stack>
  worker cleanup [--image-min-age <duration>]
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

// errUsage marks errors caused by wrong invocation (exit code 2).
var errUsage = errors.New("usage error")

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.LoadWorker(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configuration error:\n%v\n", err)
		return 2
	}
	secrets := restic.SecretValues(cfg.RepoEnv)

	spec, jobDir, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n%s", err, usage)
		if jobDir != "" {
			// Let the manager see why the job did not run.
			writeFinalStatus(jobDir, jobs.Status{ID: spec.ID, State: jobs.StateFailed, Error: redact(err.Error(), secrets)}, nil, stderr)
		}
		return 2
	}

	// Logs go to stderr and, for "run", also to <job-dir>/log.txt. Both are
	// filtered for secret values.
	out := stderr
	if jobDir != "" {
		// jobDir is fixed by the manager's worker container template (/job).
		f, err := os.OpenFile(filepath.Join(jobDir, jobs.LogFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: see above
		if err != nil {
			fmt.Fprintf(stderr, "opening job log: %v\n", err)
			return 2
		}
		defer f.Close()
		if err := jobs.MatchDirOwner(f.Name()); err != nil {
			fmt.Fprintf(stderr, "job log: %v\n", err)
			return 2
		}
		out = io.MultiWriter(stderr, f)
	}
	log := slog.New(slog.NewJSONHandler(jobs.NewRedactingWriter(out, secrets), &slog.HandlerOptions{Level: cfg.LogLevel}))
	if spec.ID != "" {
		log = log.With("job_id", spec.ID)
	}
	log.Debug("worker started", "version", buildinfo.Version)

	// SIGTERM/SIGINT cancel the context (like a CancellationToken in .NET);
	// the pipelines still restart the stack before returning.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.JobTimeout)
	defer cancel()

	started := time.Now().UTC()
	status := jobs.Status{ID: spec.ID, State: jobs.StateRunning, StartedAt: &started}
	if jobDir != "" {
		if err := jobs.WriteStatus(jobDir, status); err != nil {
			log.Error("writing job status", "error", err)
			return 1
		}
	}

	resticCLI := &restic.CLI{Logger: log}
	dockerCLI := &docker.CLI{}
	p := &jobs.Pipeline{
		Docker: dockerCLI,
		Restic: resticCLI,
		Repo:   restic.Repo{Location: cfg.RepoLocation, Env: cfg.RepoEnv},
		Host:   cfg.Hostname,
		Logger: log,
		// Bounds stop -> final snapshot (issue #36).
		DowntimeTimeout: cfg.DowntimeTimeout,
	}
	if jobDir != "" {
		// Started by the manager: stopping the stack needs a downtime slot,
		// which the manager grants through the job directory.
		p.Downtime = &jobs.FileDowntimeGate{Dir: jobDir}
		// progress.json for the job page: phases from the pipeline,
		// transfer numbers straight from restic's status lines, pending
		// services and pull progress from docker.
		progress := &jobs.FileProgress{Dir: jobDir}
		p.Progress = progress
		resticCLI.Progress = progress.Transfer
		// Services a stop or the health wait is still waiting for.
		dockerCLI.Pending = func(services []string) {
			if len(services) == 0 {
				progress.Detail("")
				return
			}
			progress.Detail("waiting for: " + strings.Join(services, ", "))
		}
		// Image pulls (update): bytes for the bar, per-image summary as
		// detail.
		// Without known sizes (nothing downloading yet, or images too
		// small to report any) there is no bar, only the summary.
		dockerCLI.Pulling = func(s docker.PullStatus) {
			if s.BytesTotal > 0 {
				progress.Transfer(restic.Progress{Percent: float64(s.BytesDone) / float64(s.BytesTotal),
					BytesDone: s.BytesDone, TotalBytes: s.BytesTotal, SecondsElapsed: s.SecondsElapsed})
			}
			progress.Detail(s.Summary())
		}
	}
	// Steps of compose up/start (issue #28): the current one as detail
	// (only with a job directory), the finished ones in the job log.
	var detail func(string)
	if p.Progress != nil {
		detail = p.Progress.Detail
	}
	dockerCLI.Steps = func(s docker.ComposeStep) {
		if detail != nil {
			detail(s.String())
		}
		switch s.Status {
		case "Done":
			log.Info("compose: "+s.Text, "object", s.Object)
		case "Error", "Warning":
			log.Warn("compose: "+s.Text, "object", s.Object, "status", s.Status)
		}
	}
	err = execute(ctx, p, cfg, spec, jobDir, &status, log)
	status.StackStoppedAt, status.StackStartedAt = downtimeTimes(p)

	finished := time.Now().UTC()
	status.FinishedAt = &finished
	code := 0
	state, err := outcome(ctx.Err(), err, cfg.JobTimeout)
	status.State = state
	switch state {
	case jobs.StateSucceeded:
		log.Info("job succeeded", "type", spec.Type, "stack", spec.Stack, "snapshot", status.Snapshot,
			"pre_stop_snapshot", status.PreStopSnapshot, "safety_snapshot", status.SafetySnapshot)
	case jobs.StateCancelled:
		status.Error, code = redact(err.Error(), secrets), 1
		log.Error("job cancelled", "error", err)
	default:
		status.Error, code = redact(err.Error(), secrets), 1
		log.Error("job failed", "error", err)
	}
	if jobDir != "" && !writeFinalStatus(jobDir, status, log, stderr) {
		return 1
	}
	return code
}

// outcome decides the final state of a job from the error of the pipeline
// and the state of the job's context (ctxErr = ctx.Err() after the job).
// The context decides, not the error: when the context ends, the running
// command (restic, docker) is killed and only reports "signal: killed",
// which does not wrap context.Canceled or context.DeadlineExceeded.
// A timeout is a failure; the returned error then says so first.
func outcome(ctxErr, err error, timeout time.Duration) (jobs.State, error) {
	switch {
	case err == nil:
		return jobs.StateSucceeded, nil
	case errors.Is(ctxErr, context.Canceled):
		return jobs.StateCancelled, fmt.Errorf("job cancelled: %w", err)
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return jobs.StateFailed, fmt.Errorf("job timeout (%s) exceeded: %w", timeout, err)
	default:
		return jobs.StateFailed, err
	}
}

// parseArgs turns the command line into a job spec. For "run" it also
// returns the job directory (even on error, so the failure can be recorded).
func parseArgs(args []string) (jobs.Spec, string, error) {
	if len(args) == 0 {
		return jobs.Spec{}, "", fmt.Errorf("%w: missing command", errUsage)
	}
	switch args[0] {
	case "run":
		if len(args) != 2 {
			return jobs.Spec{}, "", fmt.Errorf("%w: run expects <job-dir>", errUsage)
		}
		dir := args[1]
		spec, err := jobs.ReadSpec(dir)
		return spec, dir, err

	case "backup":
		fs := flag.NewFlagSet("backup", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return jobs.Spec{}, "", fmt.Errorf("%w: backup expects <stack>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeBackup, Stack: fs.Arg(0)}, "", nil

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		noStart := fs.Bool("no-start", false, "do not start the stack after restoring")
		pin := fs.Bool("pin-images", false, "start with the image versions recorded in the snapshot")
		fallback := fs.Bool("tag-fallback", false, "with --pin-images: start a service with its tag if its recorded version is gone")
		imp := fs.Bool("import", false, "first restore of a stack from another host: create its directory")
		limit := fs.Int("limit-download", 0, "limit downloads from the repository (KiB/s)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 2 {
			return jobs.Spec{}, "", fmt.Errorf("%w: restore expects [--no-start] [--pin-images [--tag-fallback]] [--import] [--limit-download <KiB/s>] <stack> <snapshot-id>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeRestore, Stack: fs.Arg(0), SnapshotID: fs.Arg(1),
			NoStart: *noStart, PinImages: *pin, TagFallback: *fallback,
			Import: *imp, LimitDownloadKiB: *limit}, "", nil

	case "update":
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		noBackup := fs.Bool("no-backup", false, "update without snapshots and without rollback (no repository)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return jobs.Spec{}, "", fmt.Errorf("%w: update expects [--no-backup] <stack>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeUpdate, Stack: fs.Arg(0), NoBackup: *noBackup}, "", nil

	case "cleanup":
		fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		minAge := fs.Duration("image-min-age", 7*24*time.Hour, "keep dangling images pulled or tagged more recently")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *minAge < 0 {
			return jobs.Spec{}, "", fmt.Errorf("%w: cleanup expects [--image-min-age <duration>]", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeCleanup, ImageMinAgeHours: int(minAge.Hours())}, "", nil

	case "stop", "start", "restart", "up", "down":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return jobs.Spec{}, "", fmt.Errorf("%w: %s expects <stack>", errUsage, args[0])
		}
		return jobs.Spec{Type: jobs.Type(args[0]), Stack: args[1]}, "", nil

	default:
		return jobs.Spec{}, "", fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// execute runs the job and records its results in status.
func execute(ctx context.Context, p *jobs.Pipeline, cfg config.Worker, spec jobs.Spec, jobDir string, status *jobs.Status, log *slog.Logger) error {
	if spec.Type == jobs.TypeCleanup {
		return cleanup(ctx, p, cfg, spec, status)
	}
	p.Repo.LimitDownloadKiB = spec.LimitDownloadKiB
	if spec.Import {
		// The first restore from another host: the stack is not on disk yet
		// (or only partly, after an incomplete import), so it cannot be
		// looked up; ImportTarget checks the name and the directory.
		if !cfg.HasRepo() {
			return errors.New("no repository configured (RESTIC_REPOSITORY and RESTIC_PASSWORD(_FILE) are required for this job)")
		}
		target, err := stacks.ImportTarget(cfg.StacksRoot, spec.Stack)
		if err != nil {
			return err
		}
		res, err := p.Import(ctx, target, jobs.RestoreOptions{SnapshotID: spec.SnapshotID, NoStart: spec.NoStart,
			PinImages: spec.PinImages, TagFallback: spec.TagFallback})
		status.Snapshot, status.Unpinned = res.Snapshot, res.Unpinned
		return err
	}
	stack, err := stacks.Lookup(cfg.StacksRoot, spec.Stack)
	if err != nil {
		return err
	}
	if needsRepository(spec) && !cfg.HasRepo() {
		return errors.New("no repository configured (RESTIC_REPOSITORY and RESTIC_PASSWORD(_FILE) are required for this job)")
	}
	if !restartsStack(spec) {
		return executeStack(ctx, p, spec, jobDir, stack, status)
	}
	// Which services ran before, to report those a failed job left down.
	before, berr := runningServices(ctx, p.Docker, stack.Dir)
	if berr != nil {
		log.Warn("reading stack state before the job", "error", berr)
	}
	err = executeStack(ctx, p, spec, jobDir, stack, status)
	if err != nil && berr == nil && len(before) > 0 {
		status.ServicesDown = servicesDown(p.Docker, stack.Dir, before, log)
	}
	return err
}

// downtimeTimes returns when the job stopped the running stack and started
// it again, as the status records them (nil = did not happen).
func downtimeTimes(p *jobs.Pipeline) (stopped, started *time.Time) {
	s, t := p.StackDowntime()
	if !s.IsZero() {
		s = s.UTC()
		stopped = &s
	}
	if !t.IsZero() {
		t = t.UTC()
		started = &t
	}
	return stopped, started
}

// restartsStack reports whether a job stops a running stack and starts it
// again: backup, update, compose apply and a restore that starts the
// stack. Only these can leave a stack down that ran before.
func restartsStack(spec jobs.Spec) bool {
	switch spec.Type {
	case jobs.TypeBackup, jobs.TypeUpdate, jobs.TypeComposeApply:
		return true
	case jobs.TypeRestore:
		return !spec.NoStart
	}
	return false
}

// serviceLister is the part of docker.Client the before/after check needs.
type serviceLister interface {
	Services(ctx context.Context, projectDir string) ([]docker.ServiceState, error)
}

// runningServices returns the names of the services of the stack in dir
// whose container runs.
func runningServices(ctx context.Context, dc serviceLister, dir string) ([]string, error) {
	services, err := dc.Services(ctx, dir)
	if err != nil {
		return nil, err
	}
	var running []string
	for _, s := range services {
		if s.Running() {
			running = append(running, s.Name)
		}
	}
	return running, nil
}

// servicesDown returns the services of before that do not run now. It has
// its own timeout: the job's context may have ended (cancel, timeout).
func servicesDown(dc serviceLister, dir string, before []string, log *slog.Logger) []string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now, err := runningServices(ctx, dc, dir)
	if err != nil {
		log.Warn("reading stack state after the job", "error", err)
		return nil
	}
	var down []string
	for _, name := range before {
		if !slices.Contains(now, name) {
			down = append(down, name)
		}
	}
	if len(down) > 0 {
		log.Error("the job left services down that ran before it", "services", down)
	}
	return down
}

// executeStack runs a job of an existing stack and records its results in
// status.
func executeStack(ctx context.Context, p *jobs.Pipeline, spec jobs.Spec, jobDir string, stack stacks.Stack, status *jobs.Status) error {
	switch spec.Type {
	case jobs.TypeBackup:
		res, err := p.Backup(ctx, stack)
		status.Snapshot, status.PreStopSnapshot = res.Snapshot, res.PreStopSnapshot
		return err
	case jobs.TypeRestore:
		res, err := p.Restore(ctx, stack, jobs.RestoreOptions{
			SnapshotID: spec.SnapshotID, NoStart: spec.NoStart,
			PinImages: spec.PinImages, TagFallback: spec.TagFallback, CarryOver: spec.CarryOverJobs,
		})
		status.Snapshot, status.SafetySnapshot, status.Unpinned = res.Snapshot, res.SafetySnapshot, res.Unpinned
		return err
	case jobs.TypeUpdate:
		res, err := p.Update(ctx, stack, jobs.UpdateOptions{Backup: !spec.NoBackup, CarryOver: spec.CarryOverJobs})
		recordChange(status, res)
		return err
	case jobs.TypeComposeApply:
		if jobDir == "" {
			return errors.New("compose-apply only runs from a job directory (worker run)")
		}
		content, err := readComposeNew(jobDir, spec.ComposeSHA256)
		if err != nil {
			return err
		}
		res, err := p.ComposeApply(ctx, stack, jobs.ComposeApplyOptions{
			Content: content, BaseSHA256: spec.ComposeBaseSHA256, Backup: !spec.NoBackup, CarryOver: spec.CarryOverJobs,
			PreviousCopy: filepath.Join(jobDir, jobs.ComposePreviousFile),
		})
		recordChange(status, res)
		return err
	case jobs.TypeStop:
		return p.Stop(ctx, stack)
	case jobs.TypeStart:
		return p.Start(ctx, stack)
	case jobs.TypeRestart:
		return p.Restart(ctx, stack)
	case jobs.TypeUp:
		return p.Up(ctx, stack)
	case jobs.TypeDown:
		return p.Down(ctx, stack)
	default:
		return fmt.Errorf("unknown job type %q", spec.Type)
	}
}

// cleanup runs the host cleanup over all discovered stacks.
func cleanup(ctx context.Context, p *jobs.Pipeline, cfg config.Worker, spec jobs.Spec, status *jobs.Status) error {
	list, err := stacks.Discover(cfg.StacksRoot)
	if err != nil {
		return err
	}
	cleaner, ok := p.Docker.(jobs.Cleaner)
	if !ok {
		return errors.New("the docker client cannot clean up")
	}
	res, err := p.Cleanup(ctx, cleaner, list, jobs.CleanupOptions{
		ImageMinAge: time.Duration(spec.ImageMinAgeHours) * time.Hour, KeepImages: spec.KeepImages})
	status.RemovedContainers, status.RemovedImages, status.FreedBytes = res.RemovedContainers, res.RemovedImages, res.FreedBytes
	return err
}

// needsRepository reports whether the job needs repository settings. Stop,
// start and restart never touch the repository; of the other jobs only an
// update or compose apply explicitly marked "no backup" runs without one.
func needsRepository(spec jobs.Spec) bool {
	if spec.Type.Control() || spec.Type.Host() {
		return false
	}
	changes := spec.Type == jobs.TypeUpdate || spec.Type == jobs.TypeComposeApply
	return !changes || !spec.NoBackup
}

// recordChange stores the result of an update or compose apply in status.
func recordChange(status *jobs.Status, res jobs.ChangeResult) {
	status.Snapshot, status.PreStopSnapshot = res.Snapshot, res.PreStopSnapshot
	status.RolledBack, status.Unchanged = res.RolledBack, res.Unchanged
	status.ImagesChanged, status.ReplacedImages = res.ImagesChanged, res.ReplacedImages
}

// readComposeNew reads the new compose file of a compose-apply job from its
// job directory and checks it against the checksum in the spec.
func readComposeNew(jobDir, wantSHA256 string) ([]byte, error) {
	f, err := os.Open(filepath.Join(jobDir, jobs.ComposeNewFile)) //nolint:gosec // G304: fixed name in the job directory
	if err != nil {
		return nil, fmt.Errorf("reading the new compose file: %w", err)
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, jobs.MaxComposeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the new compose file: %w", err)
	}
	if len(content) > jobs.MaxComposeBytes || jobs.SHA256Hex(content) != wantSHA256 {
		return nil, errors.New("the new compose file does not match the job (size or checksum)")
	}
	return content, nil
}

// writeFinalStatus writes the final status; failures are reported on log
// (if available) and stderr.
func writeFinalStatus(dir string, s jobs.Status, log *slog.Logger, stderr io.Writer) bool {
	if s.FinishedAt == nil {
		now := time.Now().UTC()
		s.FinishedAt = &now
	}
	if err := jobs.WriteStatus(dir, s); err != nil {
		if log != nil {
			log.Error("writing final job status", "error", err)
		}
		fmt.Fprintf(stderr, "writing final job status: %v\n", err)
		return false
	}
	return true
}

func redact(s string, secrets []string) string {
	var b strings.Builder
	_, _ = jobs.NewRedactingWriter(&b, secrets).Write([]byte(s))
	return b.String()
}
