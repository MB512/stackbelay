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
// Services that are defined but have no container yet (added to the compose
// file while the stack was stopped) are then created and started with
// "compose up" for just those services, as "docker compose up -d" would;
// the existing containers are not recreated. A stack without any
// containers fails: it was never created (issue #18).
func (p *Pipeline) Start(ctx context.Context, stack stacks.Stack) error {
	log := p.logger().With("stack", stack.Name, "job", "start")
	if err := requireComplete(stack); err != nil {
		return err
	}
	services, err := p.services(ctx, log, stack.Dir)
	if err != nil {
		return fmt.Errorf("reading stack state: %w", err)
	}
	var existing, missing []string
	for _, s := range services {
		if s.ContainerID != "" {
			existing = append(existing, s.Name)
		} else {
			missing = append(missing, s.Name)
		}
	}
	if len(existing) == 0 {
		return errors.New("the stack has no containers to start (create them once with \"docker compose up\")")
	}
	if err := p.bringUp(ctx, log, stack.Dir, nil, existing, false); err != nil {
		return fmt.Errorf("starting stack: %w", err)
	}
	if len(missing) > 0 {
		log.Info("creating services that have no container yet (added while the stack was stopped)", "services", missing)
		if err := p.bringUp(ctx, log, stack.Dir, nil, missing, true); err != nil {
			return fmt.Errorf("creating new services: %w", err)
		}
	}
	return nil
}

// Restart stops the stack and starts the services that were running
// before, without backup. The start is registered (defer) before the stop,
// so it also happens when the stop fails or the job is cancelled.
func (p *Pipeline) Restart(ctx context.Context, stack stacks.Stack) (err error) {
	log := p.logger().With("stack", stack.Name, "job", "restart")
	if err := requireComplete(stack); err != nil {
		return err
	}
	services, err := p.services(ctx, log, stack.Dir)
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

// Up creates and starts the stack like "docker compose up --detach",
// without backup: missing containers are created, those whose
// configuration or image changed are recreated, the others are left alone
// (images that are missing are pulled by compose). Then the usual health
// wait for all services (a warning only: one-shot services exit).
func (p *Pipeline) Up(ctx context.Context, stack stacks.Stack) error {
	log := p.logger().With("stack", stack.Name, "job", "up")
	if err := requireComplete(stack); err != nil {
		return err
	}
	services, err := p.services(ctx, log, stack.Dir) // also the project owner check
	if err != nil {
		return fmt.Errorf("reading stack state: %w", err)
	}
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	log.Info("creating and starting stack (compose up)", "services", names)
	p.phase("starting stack")
	if err := p.Docker.UpAll(ctx, stack.Dir); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	hctx, cancel := context.WithTimeout(ctx, orDefault(p.HealthTimeout, DefaultHealthTimeout))
	defer cancel()
	p.phase("waiting until healthy")
	if err := p.Docker.WaitHealthy(hctx, stack.Dir, names); err != nil {
		log.Warn("stack is up but not all services run or are healthy", "error", err)
	} else {
		log.Info("stack is up")
	}
	return nil
}

// Down removes the stack's containers and networks like "docker compose
// down", without backup. Volumes, images and the stack directory are kept;
// Up (or "docker compose up") creates the containers again.
func (p *Pipeline) Down(ctx context.Context, stack stacks.Stack) error {
	log := p.logger().With("stack", stack.Name, "job", "down")
	if _, err := p.services(ctx, log, stack.Dir); err != nil { // the project owner check, before removing anything
		return fmt.Errorf("reading stack state: %w", err)
	}
	log.Info("removing containers and networks (compose down; volumes are kept)")
	p.phase("removing containers")
	if err := p.Docker.Down(ctx, stack.Dir); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	log.Info("stack is down")
	return nil
}
