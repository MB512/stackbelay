// Command worker executes a single backup or restore job and exits.
//
// Usage:
//
//	worker backup  [--pre-stop] <stack>
//	worker restore [--no-safety-snapshot] [--no-start] <stack> <snapshot-id>
//
// Flags must come before positional arguments. Configuration is read from
// the environment (see internal/config). Exit codes: 0 success, 1 job
// failed, 2 usage or configuration error.
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
	"syscall"

	"github.com/MB512/stackvault/internal/config"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

const usage = `usage:
  worker backup  [--pre-stop] <stack>
  worker restore [--no-safety-snapshot] [--no-start] <stack> <snapshot-id>
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
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// SIGTERM/SIGINT cancel the context (like a CancellationToken in .NET);
	// the pipelines still restart the stack before returning.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.JobTimeout)
	defer cancel()

	p := &jobs.Pipeline{
		Docker: &docker.CLI{},
		Restic: &restic.CLI{Logger: log},
		Repo:   restic.Repo{Location: cfg.RepoLocation, Env: cfg.RepoSecretEnv},
		Host:   cfg.Hostname,
		Logger: log,
	}

	if err := dispatch(ctx, p, cfg, args, log); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintf(stderr, "%v\n%s", err, usage)
			return 2
		}
		log.Error("job failed", "error", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, p *jobs.Pipeline, cfg config.Worker, args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing command", errUsage)
	}
	switch args[0] {
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		preStop := fs.Bool("pre-stop", false, "take a snapshot before stopping the stack")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return fmt.Errorf("%w: backup expects [--pre-stop] <stack>", errUsage)
		}
		stack, err := stacks.Lookup(cfg.StacksRoot, fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := p.Backup(ctx, stack, jobs.BackupOptions{PreStop: *preStop})
		if err != nil {
			return err
		}
		log.Info("backup succeeded", "stack", stack.Name, "snapshot", res.Snapshot, "pre_stop_snapshot", res.PreStopSnapshot)
		return nil

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		noSafety := fs.Bool("no-safety-snapshot", false, "skip the pre-restore safety snapshot")
		noStart := fs.Bool("no-start", false, "do not start the stack after restoring")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 2 {
			return fmt.Errorf("%w: restore expects [--no-safety-snapshot] [--no-start] <stack> <snapshot-id>", errUsage)
		}
		stack, err := stacks.Lookup(cfg.StacksRoot, fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := p.Restore(ctx, stack, jobs.RestoreOptions{
			SnapshotID: fs.Arg(1), SkipSafetySnapshot: *noSafety, NoStart: *noStart,
		})
		if err != nil {
			return err
		}
		log.Info("restore succeeded", "stack", stack.Name, "snapshot", res.Snapshot,
			"safety_snapshot", res.SafetySnapshot, "previous_dir", res.OldDir)
		return nil

	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}
