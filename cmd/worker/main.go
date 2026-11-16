// Command worker executes a single backup, restore or update job and exits.
//
// Usage:
//
//	worker run <job-dir>
//	worker backup  [--pre-stop] <stack>
//	worker restore [--no-safety-snapshot] [--no-start] [--pin-images] <stack> <snapshot-id>
//	worker update  [--no-backup] <stack>
//
// "run" is how the manager starts a job: the job is described by
// <job-dir>/spec.json, and the worker writes status.json and log.txt there.
// "backup", "restore" and "update" are for manual use and tests.
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
	"strings"
	"syscall"
	"time"

	"github.com/MB512/stackvault/internal/buildinfo"
	"github.com/MB512/stackvault/internal/config"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

const usage = `usage:
  worker run <job-dir>
  worker backup  [--pre-stop] <stack>
  worker restore [--no-safety-snapshot] [--no-start] [--pin-images] <stack> <snapshot-id>
  worker update  [--no-backup] <stack>
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

	p := &jobs.Pipeline{
		Docker: &docker.CLI{},
		Restic: &restic.CLI{Logger: log},
		Repo:   restic.Repo{Location: cfg.RepoLocation, Env: cfg.RepoEnv},
		Host:   cfg.Hostname,
		Logger: log,
	}
	err = execute(ctx, p, cfg, spec, &status, log)

	finished := time.Now().UTC()
	status.FinishedAt = &finished
	code := 0
	switch {
	case err == nil:
		status.State = jobs.StateSucceeded
		log.Info("job succeeded", "type", spec.Type, "stack", spec.Stack, "snapshot", status.Snapshot,
			"pre_stop_snapshot", status.PreStopSnapshot, "safety_snapshot", status.SafetySnapshot,
			"previous_dir", status.PreviousDir)
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		status.State, status.Error, code = jobs.StateCancelled, redact(err.Error(), secrets), 1
		log.Error("job cancelled", "error", err)
	default:
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = fmt.Errorf("job timeout (%s) exceeded: %w", cfg.JobTimeout, err)
		}
		status.State, status.Error, code = jobs.StateFailed, redact(err.Error(), secrets), 1
		log.Error("job failed", "error", err)
	}
	if jobDir != "" && !writeFinalStatus(jobDir, status, log, stderr) {
		return 1
	}
	if jobDir != "" && spec.CarryOverJobs != "" && status.PreviousDir != "" {
		// The manager restored its own stack: our job directory moved away
		// with the previous version. The job files were carried over into
		// the restored stack before it started; complete that copy, so the
		// restarted manager sees the final result and the whole log.
		carried := filepath.Join(cfg.StacksRoot, spec.Stack, spec.CarryOverJobs, spec.ID)
		if err := finishCarriedOver(jobDir, carried, status); err != nil {
			fmt.Fprintf(stderr, "updating carried-over job files: %v\n", err)
			return 1
		}
	}
	return code
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
		preStop := fs.Bool("pre-stop", false, "take a snapshot before stopping the stack")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return jobs.Spec{}, "", fmt.Errorf("%w: backup expects [--pre-stop] <stack>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeBackup, Stack: fs.Arg(0), PreStop: *preStop}, "", nil

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		noSafety := fs.Bool("no-safety-snapshot", false, "skip the pre-restore safety snapshot")
		noStart := fs.Bool("no-start", false, "do not start the stack after restoring")
		pin := fs.Bool("pin-images", false, "start with the image versions recorded in the snapshot")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 2 {
			return jobs.Spec{}, "", fmt.Errorf("%w: restore expects [--no-safety-snapshot] [--no-start] [--pin-images] <stack> <snapshot-id>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeRestore, Stack: fs.Arg(0), SnapshotID: fs.Arg(1),
			SkipSafetySnapshot: *noSafety, NoStart: *noStart, PinImages: *pin}, "", nil

	case "update":
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		noBackup := fs.Bool("no-backup", false, "update without snapshots and without rollback (no repository)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return jobs.Spec{}, "", fmt.Errorf("%w: update expects [--no-backup] <stack>", errUsage)
		}
		return jobs.Spec{Type: jobs.TypeUpdate, Stack: fs.Arg(0), NoBackup: *noBackup}, "", nil

	default:
		return jobs.Spec{}, "", fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// execute runs the job and records its results in status.
func execute(ctx context.Context, p *jobs.Pipeline, cfg config.Worker, spec jobs.Spec, status *jobs.Status, log *slog.Logger) error {
	stack, err := stacks.Lookup(cfg.StacksRoot, spec.Stack)
	if err != nil {
		return err
	}
	// Only an update explicitly marked "no backup" runs without repository.
	if needsRepo := spec.Type != jobs.TypeUpdate || !spec.NoBackup; needsRepo && !cfg.HasRepo() {
		return errors.New("no repository configured (RESTIC_REPOSITORY and RESTIC_PASSWORD(_FILE) are required for this job)")
	}
	switch spec.Type {
	case jobs.TypeBackup:
		res, err := p.Backup(ctx, stack, jobs.BackupOptions{PreStop: spec.PreStop})
		status.Snapshot, status.PreStopSnapshot = res.Snapshot, res.PreStopSnapshot
		return err
	case jobs.TypeRestore:
		res, err := p.Restore(ctx, stack, jobs.RestoreOptions{
			SnapshotID: spec.SnapshotID, SkipSafetySnapshot: spec.SkipSafetySnapshot, NoStart: spec.NoStart,
			PinImages: spec.PinImages, CarryOver: spec.CarryOverJobs,
		})
		status.Snapshot, status.SafetySnapshot, status.PreviousDir = res.Snapshot, res.SafetySnapshot, res.OldDir
		if res.OldDir != "" {
			log.Info("previous stack directory kept", "dir", res.OldDir)
		}
		return err
	case jobs.TypeUpdate:
		res, err := p.Update(ctx, stack, jobs.UpdateOptions{Backup: !spec.NoBackup, CarryOver: spec.CarryOverJobs})
		status.Snapshot, status.PreStopSnapshot = res.Snapshot, res.PreStopSnapshot
		status.PreviousDir, status.RolledBack = res.OldDir, res.RolledBack
		return err
	default:
		return fmt.Errorf("unknown job type %q", spec.Type)
	}
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

// finishCarriedOver writes the final status and the complete log into the
// carried-over copy of the job directory.
func finishCarriedOver(jobDir, carried string, status jobs.Status) error {
	if err := os.MkdirAll(carried, 0o700); err != nil {
		return err
	}
	if err := jobs.MatchDirOwner(carried); err != nil {
		return err
	}
	// Log first: once the status is final the manager stops reading.
	if err := jobs.CopyTree(filepath.Join(jobDir, jobs.LogFile), filepath.Join(carried, jobs.LogFile)); err != nil {
		return err
	}
	return jobs.WriteStatus(carried, status)
}

func redact(s string, secrets []string) string {
	var b strings.Builder
	_, _ = jobs.NewRedactingWriter(&b, secrets).Write([]byte(s))
	return b.String()
}
