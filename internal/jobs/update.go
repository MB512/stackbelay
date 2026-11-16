package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

// Default timeouts of the update job.
const (
	// DefaultUpdateHealthTimeout bounds waiting for the updated services to
	// run (and be healthy, if they have a healthcheck). Exceeding it counts
	// as a failed update.
	DefaultUpdateHealthTimeout = 5 * time.Minute
	// DefaultRollbackTimeout bounds the rollback (a restore of the final
	// snapshot). It runs even if the job was cancelled.
	DefaultRollbackTimeout = 2 * time.Hour
)

// UpdateOptions control an update job.
type UpdateOptions struct {
	// Backup takes the pre-stop and final snapshots and enables the
	// automatic rollback. It is false only when the stack has no
	// repository: then nothing is backed up and a failed update is NOT
	// rolled back (not even the old images are started, they could damage
	// data the new version has already migrated).
	Backup bool
	// CarryOver is passed to the rollback restore (see
	// RestoreOptions.CarryOver).
	CarryOver string
}

// UpdateResult reports what an update job did.
type UpdateResult struct {
	PreStopSnapshot string // "" if not taken or failed
	Snapshot        string // final snapshot: the state before the update
	RolledBack      bool   // the update failed and the final snapshot was restored
	OldDir          string // after a rollback: directory of the failed version
}

// Update runs the update pipeline for a running stack (see "Update" in
// docs/architecture.md): metadata.txt -> pull -> pre-stop snapshot -> stop
// -> final snapshot -> up -> wait until running/healthy. Snapshots are
// only taken with opts.Backup.
//
// Before "up", any failure starts the old containers again (like a backup).
// If "up" or the health check fails, the final snapshot is restored with
// the image versions recorded before the update (rollback). Without backup
// a failed update is left as it is.
func (p *Pipeline) Update(ctx context.Context, stack stacks.Stack, opts UpdateOptions) (res UpdateResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", "update")

	// 1. Preflight and state.
	if opts.Backup {
		log.Info("checking repository")
		if err := p.Restic.CheckRepo(ctx, p.Repo); err != nil {
			return res, fmt.Errorf("repository not reachable: %w", err)
		}
	} else {
		log.Warn("the stack has no repository: updating WITHOUT backup; a failed update cannot be rolled back")
	}
	services, err := p.Docker.Services(ctx, stack.Dir)
	if err != nil {
		return res, fmt.Errorf("reading stack state: %w", err)
	}
	var running []string
	for _, s := range services {
		if s.Running() {
			running = append(running, s.Name)
		}
	}
	if len(running) == 0 {
		return res, errors.New("no service of the stack is running; start the stack before updating it " +
			"(an update recreates the running services)")
	}
	log.Info("recorded stack state", "services", len(services), "running", running)
	// The image digests recorded here are those of the running containers,
	// i.e. the versions before the update; the rollback pins them.
	if err := p.writeMetadata(log, stack, services); err != nil {
		return res, fmt.Errorf("writing %s: %w", MetadataFile, err)
	}

	// 2. Pull while the stack is still running.
	log.Info("pulling images", "services", running)
	if err := p.Docker.Pull(ctx, stack.Dir, running); err != nil {
		return res, fmt.Errorf("pulling images (the stack was not changed): %w", err)
	}

	// 3. Pre-stop snapshot (parent of the final one). Failure is not fatal.
	if opts.Backup {
		log.Info("taking pre-stop snapshot")
		id, err := p.Restic.Backup(ctx, p.Repo, stack.Dir, restic.BackupOptions{Host: p.Host, Tags: []string{TagPreStop}})
		if err != nil {
			log.Warn("pre-stop snapshot failed; continuing without parent", "error", err)
		} else {
			res.PreStopSnapshot = id
			log.Info("pre-stop snapshot done", "snapshot", id)
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err // cancelled before anything was stopped
	}

	// Until "up" has been attempted, every failure starts the old
	// containers again (they still use the old images).
	upAttempted := false
	defer func() {
		if upAttempted {
			return
		}
		if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
			err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
		}
	}()

	// 4. Stop.
	log.Info("stopping stack")
	if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
		return res, fmt.Errorf("stopping stack: %w", err)
	}

	// 5. Final snapshot: the state the rollback returns to.
	if opts.Backup {
		log.Info("taking final snapshot", "parent", res.PreStopSnapshot)
		id, err := p.Restic.Backup(ctx, p.Repo, stack.Dir, restic.BackupOptions{
			Host: p.Host, Tags: []string{TagFinal}, Parent: res.PreStopSnapshot,
		})
		res.Snapshot = id
		if err != nil {
			return res, fmt.Errorf("final snapshot (the stack was not updated): %w", err)
		}
		log.Info("final snapshot done", "snapshot", id)
	}
	if err := ctx.Err(); err != nil {
		return res, err // cancelled: the old containers start again
	}

	// 6. Recreate with the new images and verify.
	upAttempted = true
	upErr := p.upAndVerify(ctx, log, stack.Dir, running)
	if upErr == nil {
		log.Info("update done")
		return res, nil
	}
	log.Error("update failed", "error", upErr)

	// 7. Rollback.
	if !opts.Backup {
		log.Error("NOT ROLLED BACK: the stack has no repository, so there is no backup to return to. " +
			"The stack is left as it is; check it manually.")
		return res, fmt.Errorf("update failed and was not rolled back (no repository): %w", upErr)
	}
	log.Warn("rolling back: restoring the final snapshot with the previous image versions", "snapshot", res.Snapshot)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orDefault(p.RollbackTimeout, DefaultRollbackTimeout))
	defer cancel()
	rres, rerr := p.Restore(rctx, stack, RestoreOptions{
		SnapshotID: res.Snapshot, SkipSafetySnapshot: true, PinImages: true, CarryOver: opts.CarryOver,
	})
	res.OldDir = rres.OldDir
	if rerr != nil {
		log.Error("ROLLBACK FAILED; manual intervention required", "snapshot", res.Snapshot, "error", rerr)
		return res, errors.Join(fmt.Errorf("update failed: %w", upErr),
			fmt.Errorf("rollback to snapshot %s failed: %w", shortID(res.Snapshot), rerr))
	}
	res.RolledBack = true
	log.Info("rolled back to the state before the update", "snapshot", res.Snapshot, "failed_version_dir", rres.OldDir)
	return res, fmt.Errorf("update failed and was rolled back to snapshot %s (the failed version is kept at %s): %w",
		shortID(res.Snapshot), rres.OldDir, upErr)
}

// upAndVerify recreates the services with "compose up" and waits until
// they run (and are healthy, if they have a healthcheck). Unlike bringUp,
// a health problem is an error: it decides whether the update is rolled
// back. Like bringUp it ignores the job's cancellation: the stack is
// stopped and must come up.
func (p *Pipeline) upAndVerify(ctx context.Context, log *slog.Logger, dir string, services []string) error {
	base := context.WithoutCancel(ctx)
	uctx, cancel := context.WithTimeout(base, orDefault(p.StartTimeout, DefaultStartTimeout))
	defer cancel()
	log.Info("starting stack with the new images (compose up)", "services", services)
	if err := p.Docker.Up(uctx, dir, nil, services); err != nil {
		return fmt.Errorf("starting updated stack: %w", err)
	}
	hctx, hcancel := context.WithTimeout(base, orDefault(p.UpdateHealthTimeout, DefaultUpdateHealthTimeout))
	defer hcancel()
	log.Info("waiting until the services run and are healthy")
	if err := p.Docker.WaitHealthy(hctx, dir, services); err != nil {
		return fmt.Errorf("updated stack is not healthy: %w", err)
	}
	return nil
}

// shortID shortens a snapshot id for messages.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
