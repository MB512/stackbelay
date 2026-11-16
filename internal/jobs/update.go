package jobs

import (
	"context"
	"fmt"

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
}

// Update runs the update pipeline for a running stack (see "Update
// pipeline" in docs/architecture.md): pull the images while the stack
// runs, back up, recreate the running services and wait until they run
// and are healthy; if they do not, roll back to the final snapshot.
func (p *Pipeline) Update(ctx context.Context, stack stacks.Stack, opts UpdateOptions) (ChangeResult, error) {
	log := p.logger().With("stack", stack.Name, "job", "update")
	return p.runChange(ctx, stack, change{
		name: "update", backup: opts.Backup, carryOver: opts.CarryOver, requireRunning: true,
		prepare: func(ctx context.Context, running []string) error {
			log.Info("pulling images", "services", running)
			if err := p.Docker.Pull(ctx, stack.Dir, running); err != nil {
				return fmt.Errorf("pulling images (the stack was not changed): %w", err)
			}
			return nil
		},
		// The change is in the pulled images: "up" recreates the running
		// services with them.
		apply: func(_ context.Context, _ []docker.ServiceState, running []string) ([]string, []string, bool, error) {
			return running, running, true, nil
		},
	})
}
