package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/MB512/stackvault/internal/stacks"
)

// Stop stops all containers of the stack (compose stop), without backup.
// Containers, networks and volumes are kept (never compose down).
func (p *Pipeline) Stop(ctx context.Context, stack stacks.Stack) error {
	log := p.logger().With("stack", stack.Name, "job", "stop")
	log.Info("stopping stack")
	p.phase("stopping stack")
	if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
		return fmt.Errorf("stopping stack: %w", err)
	}
	log.Info("stack stopped")
	return nil
}

// Start starts the existing containers of all services that have one
// (compose start), like "docker compose start", and waits until they run.
// It creates nothing: a stack without containers fails.
func (p *Pipeline) Start(ctx context.Context, stack stacks.Stack) error {
	log := p.logger().With("stack", stack.Name, "job", "start")
	services, err := p.Docker.Services(ctx, stack.Dir)
	if err != nil {
		return fmt.Errorf("reading stack state: %w", err)
	}
	var names []string
	for _, s := range services {
		if s.ContainerID != "" {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return errors.New("the stack has no containers to start (create them once with \"docker compose up\")")
	}
	if err := p.bringUp(ctx, log, stack.Dir, nil, names, false); err != nil {
		return fmt.Errorf("starting stack: %w", err)
	}
	return nil
}

// Restart stops the stack and starts the services that were running
// before, without backup. The start is registered (defer) before the stop,
// so it also happens when the stop fails or the job is cancelled.
func (p *Pipeline) Restart(ctx context.Context, stack stacks.Stack) (err error) {
	log := p.logger().With("stack", stack.Name, "job", "restart")
	services, err := p.Docker.Services(ctx, stack.Dir)
	if err != nil {
		return fmt.Errorf("reading stack state: %w", err)
	}
	var running []string
	for _, s := range services {
		if s.Running() {
			running = append(running, s.Name)
		}
	}
	if len(running) == 0 {
		return errors.New("no service of the stack is running; use Start to start it")
	}
	log.Info("recorded running services", "running", running)
	if err := ctx.Err(); err != nil {
		return err // cancelled before anything was stopped
	}

	defer func() {
		if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
			err = errors.Join(err, fmt.Errorf("starting stack: %w", startErr))
		}
	}()
	log.Info("stopping stack")
	p.phase("stopping stack")
	if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
		return fmt.Errorf("stopping stack: %w", err)
	}
	return nil
}
