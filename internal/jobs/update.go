package jobs

import (
	"context"
	"fmt"
	"slices"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/stacks"
)

// UpdateOptions control an update job.
type UpdateOptions struct {
	// Backup takes the snapshots and enables the automatic rollback. It is
	// false only when the stack has no repository (see change.backup).
	Backup bool
	// CarryOver is passed to the rollback restore (see
	// RestoreOptions.CarryOver).
	CarryOver string
	// OnlyIfNew ends the job after the pull, without snapshots and without
	// stopping anything, if no running service would get another image.
	OnlyIfNew bool
	// RollbackInPlace restores the final snapshot in place on a rollback
	// (see RestoreOptions.InPlace).
	RollbackInPlace bool
}

// Update runs the update pipeline for a running stack (see "Update
// pipeline" in docs/architecture.md): pull the images while the stack
// runs, back up, recreate the running services and wait until they run
// and are healthy; if they do not, roll back to the final snapshot.
func (p *Pipeline) Update(ctx context.Context, stack stacks.Stack, opts UpdateOptions) (ChangeResult, error) {
	log := p.logger().With("stack", stack.Name, "job", "update")
	return p.runChange(ctx, stack, change{
		name: "update", backup: opts.Backup, carryOver: opts.CarryOver, requireRunning: true,
		rollbackInPlace: opts.RollbackInPlace,
		prepare: func(ctx context.Context, services []docker.ServiceState, running []string) error {
			log.Info("pulling images", "services", running)
			if err := p.Docker.Pull(ctx, stack.Dir, running); err != nil {
				return fmt.Errorf("pulling images (the stack was not changed): %w", err)
			}
			if !opts.OnlyIfNew {
				return nil
			}
			changed, err := p.newImages(ctx, stack, services, running)
			if err != nil {
				return fmt.Errorf("comparing images (the stack was not changed): %w", err)
			}
			if len(changed) == 0 {
				log.Info("no new images: every running service already runs the image of its tag")
				return errUnchanged
			}
			log.Info("new images", "services", changed)
			return nil
		},
		// The change is in the pulled images: "up" recreates the running
		// services with them.
		apply: func(_ context.Context, _ []docker.ServiceState, running []string) ([]string, []string, bool, error) {
			return running, running, true, nil
		},
	})
}

// newImages returns the running services whose container image differs
// from the local image their compose file names now (after the pull).
// Services without such an image (built locally) do not count.
func (p *Pipeline) newImages(ctx context.Context, stack stacks.Stack, services []docker.ServiceState, running []string) ([]string, error) {
	ids, err := p.Docker.ImageIDs(ctx, stack.Dir, running)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, s := range services {
		if !slices.Contains(running, s.Name) || ids[s.Name] == "" {
			continue
		}
		if ids[s.Name] != s.ImageID {
			changed = append(changed, s.Name)
		}
	}
	return changed, nil
}
