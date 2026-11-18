package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

// Default timeouts of jobs that change a stack (update, compose apply).
const (
	// DefaultUpdateHealthTimeout bounds waiting for the changed services
	// to run (and be healthy, if they have a healthcheck). Exceeding it
	// counts as a failed change.
	DefaultUpdateHealthTimeout = 5 * time.Minute
	// DefaultRollbackTimeout bounds the rollback (a restore of the final
	// snapshot). It runs even if the job was cancelled.
	DefaultRollbackTimeout = 2 * time.Hour
)

// ChangeResult reports what an update or compose apply did.
type ChangeResult struct {
	PreStopSnapshot string // "" if not taken or failed
	Snapshot        string // final snapshot: the state before the change
	RolledBack      bool   // the change failed and the final snapshot was restored
	OldDir          string // after a rollback with swap: directory of the failed version
	// Unchanged: there was nothing to change (update "only if new" found
	// no new image); the stack was not touched.
	Unchanged bool
	// ImagesChanged lists the services that ran before the change and run
	// another image (image id) after it. Only set when the change
	// succeeded.
	ImagesChanged []string
	// ReplacedImages are the image ids those services ran before. A host
	// cleanup keeps them for a while (issue #19), so the old version is
	// still there for a manual rollback.
	ReplacedImages []string
}

// errUnchanged is returned by change.prepare when there is nothing to
// change: the job ends successfully before any snapshot or stop.
var errUnchanged = errors.New("nothing to change")

// change describes a job that changes a stack the same way: back up, change,
// start, verify, and roll back to the backup if it does not come up.
type change struct {
	name string // job name for the log: "update", "compose-apply"
	// backup takes the pre-stop and final snapshots and enables the
	// rollback. False only for a stack without repository: then nothing is
	// backed up and a failed change is NOT rolled back (not even the old
	// version is started; it could damage data a new version already
	// migrated).
	backup    bool
	carryOver string // see RestoreOptions.CarryOver (for the rollback)
	// rollbackInPlace restores the final snapshot in place instead of
	// swapping directories (see RestoreOptions.InPlace).
	rollbackInPlace bool
	// noPreStop skips the pre-stop snapshot (the final snapshot then has no
	// parent and takes longer, during which the stack is stopped).
	noPreStop bool
	// requireRunning refuses a stack without running services.
	requireRunning bool
	// prepare (optional) runs while the stack still runs, before any
	// snapshot. A failure ends the job with nothing changed; errUnchanged
	// ends it successfully, also with nothing changed.
	prepare func(ctx context.Context, services []docker.ServiceState, running []string) error
	// apply runs with the stack stopped, after the final snapshot. It
	// returns the services to start and those to wait for. changed
	// reports whether the stack was modified: a failure with changed=false
	// starts the old containers again, one with changed=true is handled
	// like a failed start (rollback).
	apply func(ctx context.Context, before []docker.ServiceState, running []string) (start, verify []string, changed bool, err error)
}

// runChange runs c on stack: metadata.txt -> prepare -> pre-stop snapshot
// -> stop -> final snapshot -> apply -> up -> wait until running/healthy.
// Snapshots are only taken with c.backup. A stack whose services are all
// stopped (allowed without c.requireRunning) is not stopped and not
// started; its single snapshot is the final one.
//
// Before apply has changed the stack, any failure starts the old containers
// again. If the changed stack does not come up, the final snapshot is
// restored with the image versions recorded before the change (rollback);
// without backup it is left as it is.
func (p *Pipeline) runChange(ctx context.Context, stack stacks.Stack, c change) (res ChangeResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", c.name)

	// 1. Preflight and state.
	if c.backup {
		log.Info("checking repository")
		if err := p.Restic.CheckRepo(ctx, p.Repo); err != nil {
			return res, fmt.Errorf("repository not reachable: %w", err)
		}
	} else {
		log.Warn("the stack has no repository: changing it WITHOUT backup; a failure cannot be rolled back")
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
	if c.requireRunning && len(running) == 0 {
		return res, fmt.Errorf("no service of the stack is running; start the stack before the %s "+
			"(it recreates the running services)", c.name)
	}
	log.Info("recorded stack state", "services", len(services), "running", running)
	// The image digests recorded here are those of the running containers,
	// i.e. the versions before the change; the rollback pins them.
	if err := p.writeMetadata(log, stack, services); err != nil {
		return res, fmt.Errorf("writing %s: %w", MetadataFile, err)
	}

	// 2. Preparation while the stack still runs.
	if c.prepare != nil {
		err := c.prepare(ctx, services, running)
		if errors.Is(err, errUnchanged) {
			res.Unchanged = true
			log.Info("nothing to change; the stack was not touched")
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}

	// 3. Pre-stop snapshot (parent of the final one). Failure is not fatal.
	if c.backup && len(running) > 0 && c.noPreStop {
		log.Info("pre-stop snapshot disabled")
	}
	if c.backup && len(running) > 0 && !c.noPreStop {
		log.Info("taking pre-stop snapshot")
		p.phase("pre-stop snapshot")
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

	// Until the stack has been changed, every failure starts the old
	// containers again (they still use the old images and configuration).
	changed := false
	defer func() {
		if changed {
			return
		}
		if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
			err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
		}
	}()

	// 4. Stop.
	if len(running) > 0 {
		log.Info("stopping stack")
		p.phase("stopping stack")
		if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
			return res, fmt.Errorf("stopping stack: %w", err)
		}
	}

	// 5. Final snapshot: the state the rollback returns to.
	if c.backup {
		log.Info("taking final snapshot", "parent", res.PreStopSnapshot)
		p.phase("final snapshot")
		id, err := p.Restic.Backup(ctx, p.Repo, stack.Dir, restic.BackupOptions{
			Host: p.Host, Tags: []string{TagFinal}, Parent: res.PreStopSnapshot,
		})
		res.Snapshot = id
		if err != nil {
			return res, fmt.Errorf("final snapshot (the stack was not changed): %w", err)
		}
		log.Info("final snapshot done", "snapshot", id)
	}
	if err := ctx.Err(); err != nil {
		return res, err // cancelled: the old containers start again
	}

	// 6. Change, start and verify.
	start, verify, applied, failure := c.apply(ctx, services, running)
	if failure != nil && !applied {
		return res, failure // nothing changed: the old containers start again
	}
	changed = true
	if failure == nil {
		if len(start) == 0 {
			log.Info("no services to start; the stack stays stopped")
			return res, nil
		}
		if failure = p.upAndVerify(ctx, log, stack.Dir, start, verify); failure == nil {
			res.ImagesChanged, res.ReplacedImages = p.imagesChanged(ctx, log, stack.Dir, services)
			log.Info(c.name+" done", "images_changed", res.ImagesChanged, "replaced_images", res.ReplacedImages)
			return res, nil
		}
	}
	log.Error(c.name+" failed", "error", failure)

	// 7. Rollback.
	if !c.backup {
		log.Error("NOT ROLLED BACK: the stack has no repository, so there is no backup to return to. " +
			"The stack is left as it is; check it manually.")
		return res, fmt.Errorf("%s failed and was not rolled back (no repository): %w", c.name, failure)
	}
	log.Warn("rolling back: restoring the final snapshot with the previous image versions", "snapshot", res.Snapshot,
		"in_place", c.rollbackInPlace)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orDefault(p.RollbackTimeout, DefaultRollbackTimeout))
	defer cancel()
	// The restore's phases show as "rollback: ...".
	p.phase("rollback")
	p.phasePrefix = "rollback: "
	rres, rerr := p.Restore(rctx, stack, RestoreOptions{
		SnapshotID: res.Snapshot, SkipSafetySnapshot: true, PinImages: true, CarryOver: c.carryOver,
		InPlace: c.rollbackInPlace,
	})
	p.phasePrefix = ""
	res.OldDir = rres.OldDir
	if rerr != nil {
		log.Error("ROLLBACK FAILED; manual intervention required", "snapshot", res.Snapshot, "error", rerr)
		return res, errors.Join(fmt.Errorf("%s failed: %w", c.name, failure),
			fmt.Errorf("rollback to snapshot %s failed: %w", shortID(res.Snapshot), rerr))
	}
	res.RolledBack = true
	log.Info("rolled back to the state before the "+c.name, "snapshot", res.Snapshot, "failed_version_dir", rres.OldDir)
	if rres.OldDir == "" { // in place: nothing of the failed version is kept
		return res, fmt.Errorf("%s failed and was rolled back in place to snapshot %s: %w", c.name, shortID(res.Snapshot), failure)
	}
	return res, fmt.Errorf("%s failed and was rolled back to snapshot %s (the failed version is kept at %s): %w",
		c.name, shortID(res.Snapshot), rres.OldDir, failure)
}

// upAndVerify recreates the services in start with "compose up" and waits
// until those in verify run (and are healthy, if they have a
// healthcheck). Unlike bringUp, a health problem is an error: it decides
// whether the change is rolled back. Like bringUp it ignores the job's
// cancellation: the stack is stopped and must come up.
func (p *Pipeline) upAndVerify(ctx context.Context, log *slog.Logger, dir string, start, verify []string) error {
	base := context.WithoutCancel(ctx)
	uctx, cancel := context.WithTimeout(base, orDefault(p.StartTimeout, DefaultStartTimeout))
	defer cancel()
	log.Info("starting stack (compose up)", "services", start)
	p.phase("recreating services")
	if err := p.Docker.Up(uctx, dir, nil, start); err != nil {
		return fmt.Errorf("starting stack: %w", err)
	}
	if len(verify) == 0 {
		return nil
	}
	hctx, hcancel := context.WithTimeout(base, orDefault(p.UpdateHealthTimeout, DefaultUpdateHealthTimeout))
	defer hcancel()
	log.Info("waiting until the services run and are healthy", "services", verify)
	p.phase("waiting until healthy")
	if err := p.Docker.WaitHealthy(hctx, dir, verify); err != nil {
		return fmt.Errorf("stack is not healthy: %w", err)
	}
	return nil
}

// imagesChanged returns the services of before that were running and now
// run another image (compared by image id, i.e. content digest, so an
// image pulled earlier by another stack counts as well), and the image ids
// they ran before (each once). A failure to read the state only warns: the
// change itself succeeded.
func (p *Pipeline) imagesChanged(ctx context.Context, log *slog.Logger, dir string, before []docker.ServiceState) (services, replaced []string) {
	after, err := p.Docker.Services(context.WithoutCancel(ctx), dir)
	if err != nil {
		log.Warn("reading the stack state after the change; image changes are not recorded", "error", err)
		return nil, nil
	}
	now := make(map[string]string, len(after))
	for _, s := range after {
		if s.Running() {
			now[s.Name] = s.ImageID
		}
	}
	for _, s := range before {
		if id, ok := now[s.Name]; ok && s.Running() && s.ImageID != "" && id != "" && id != s.ImageID {
			services = append(services, s.Name)
			if docker.ValidImageID(s.ImageID) && !slices.Contains(replaced, s.ImageID) {
				replaced = append(replaced, s.ImageID)
			}
		}
	}
	return services, replaced
}

// shortID shortens a snapshot id for messages.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
