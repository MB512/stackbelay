// Package jobs implements the backup, restore and update pipelines.
package jobs

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/stacks"
)

// Snapshot tags written by the pipelines.
const (
	TagPreStop    = "pre-stop"
	TagFinal      = "final"
	TagPreRestore = "pre-restore"
)

// Default timeouts for bringing a stack back up.
const (
	DefaultStartTimeout  = 5 * time.Minute
	DefaultHealthTimeout = 2 * time.Minute
)

// Pipeline runs backup and restore jobs for one repository.
type Pipeline struct {
	Docker docker.Client
	Restic restic.Runner
	Repo   restic.Repo
	// Host is passed to restic as --host. Required: inside the worker
	// container os.Hostname() is a random container id.
	Host   string
	Logger *slog.Logger
	// Now returns the current time; defaults to time.Now (tests override it).
	Now func() time.Time
	// StartTimeout bounds "compose start/up" after a backup or restore.
	StartTimeout time.Duration
	// HealthTimeout bounds waiting for services to become healthy.
	HealthTimeout time.Duration
	// Progress receives the phases of the job (nil: none are reported).
	// restic's transfer progress reaches it directly from restic.CLI.
	Progress ProgressReporter
	// phasePrefix marks the phases of a rollback ("rollback: ").
	phasePrefix string
	// UpdateHealthTimeout bounds the health check after an update; default
	// DefaultUpdateHealthTimeout. Exceeding it fails (and rolls back) the update.
	UpdateHealthTimeout time.Duration
	// RollbackTimeout bounds the rollback of a failed update; default
	// DefaultRollbackTimeout.
	RollbackTimeout time.Duration
	// Downtime gives a job the right to stop its stack (a downtime slot,
	// issue #36); nil: no limit (a worker run by hand).
	Downtime DowntimeGate
	// DowntimeTimeout bounds stop -> final snapshot of a backup, update or
	// compose apply; 0: none. When it runs out, the final snapshot is
	// aborted and the stack started again.
	DowntimeTimeout time.Duration

	// stoppedAt / startedAt: when the job first stopped the running stack
	// and when it last started it again (StackDowntime).
	stoppedAt, startedAt time.Time
}

// StackDowntime returns when the job stopped the running stack (first
// stop) and when it started it again (last successful start after that);
// stopped is zero if the job stopped no running stack, started is zero if
// the stack was not started again. The summary sums these (issue #48).
func (p *Pipeline) StackDowntime() (stopped, started time.Time) {
	return p.stoppedAt, p.startedAt
}

// markStopped records that the job stopped the running stack.
func (p *Pipeline) markStopped() {
	if p.stoppedAt.IsZero() {
		p.stoppedAt = p.now()
	}
}

// markStarted records that the stack runs again after the job stopped it.
func (p *Pipeline) markStarted() {
	if !p.stoppedAt.IsZero() {
		p.startedAt = p.now()
	}
}

func (p *Pipeline) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p *Pipeline) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// requireComplete refuses a stack whose last restore did not complete
// (stacks.IncompleteMarker): starting, backing up or changing a partly
// restored stack could damage its data further or record it as a valid
// backup. Only a restore (which continues the incomplete one), stop and
// down are allowed.
func requireComplete(stack stacks.Stack) error {
	if stacks.RestoreIncomplete(stack.Dir) {
		return fmt.Errorf("the last restore of stack %s did not complete, so its directory may be partly restored; "+
			"restore a snapshot first", stack.Name)
	}
	return nil
}

// services reads the stack's services (docker.Client.Services, which also
// checks that the compose project belongs to dir) and leaves out orphans:
// containers of services that are no longer in the compose file. Compose
// refuses to start, recreate or pull those ("no such service"), so no job
// touches them; they are logged once (issue #26). The host cleanup removes
// them when they are stopped.
func (p *Pipeline) services(ctx context.Context, log *slog.Logger, dir string) ([]docker.ServiceState, error) {
	all, err := p.Docker.Services(ctx, dir)
	if err != nil {
		return nil, err
	}
	defined := make([]docker.ServiceState, 0, len(all))
	var orphans []string
	for _, s := range all {
		if s.Defined {
			defined = append(defined, s)
		} else {
			orphans = append(orphans, s.Name+" ("+cmp.Or(s.State, "no container")+")")
		}
	}
	if len(orphans) > 0 {
		log.Warn("containers of services that are no longer in the compose file are left alone (orphans)", "orphans", orphans)
	}
	return defined, nil
}

// pullMissing pulls the images of services that are not present locally
// before a "compose up" (issue #28). Compose would pull them itself, but
// silently; Pull reports the progress to the job page (bytes, per-image
// status) like an update's pull. Only missing images are pulled, so a tag
// never moves to a newer image here. A failure is only logged: compose up
// tries again and reports the real error. A nil or empty services list
// does nothing.
func (p *Pipeline) pullMissing(ctx context.Context, log *slog.Logger, dir string, services []string) {
	if len(services) == 0 {
		return
	}
	images, err := p.Docker.ConfiguredImages(ctx, dir, services)
	if err != nil {
		log.Warn("could not check for missing images; compose up pulls them", "error", err)
		return
	}
	var missing, refs []string
	for _, name := range services {
		if img := images[name]; img.Ref != "" && img.ID == "" {
			missing, refs = append(missing, name), append(refs, img.Ref)
		}
	}
	if len(missing) == 0 {
		return
	}
	log.Info("pulling missing images", "services", missing, "images", refs)
	p.phase("pulling missing images")
	if err := p.Docker.Pull(ctx, dir, missing); err != nil {
		log.Warn("pulling missing images failed; compose up tries again", "error", err)
		return
	}
	log.Info("missing images pulled", "services", missing)
}

// bringUp starts services after a job. It must run even if the job's context
// was cancelled or timed out, so it derives a fresh context that ignores the
// parent's cancellation (context.WithoutCancel) and only applies its own
// timeout. up=false uses "compose start" (existing containers), up=true uses
// "compose up -d" (after a restore). A nil services slice with up=true means
// all services; composeFiles (up=true only) replace compose's file lookup
// if not empty. Health problems are logged, not returned.
func (p *Pipeline) bringUp(ctx context.Context, log *slog.Logger, dir string, composeFiles, services []string, up bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orDefault(p.StartTimeout, DefaultStartTimeout))
	defer cancel()

	var err error
	if up && len(composeFiles) == 0 {
		// With composeFiles (pinned versions) the images were made
		// available by digest before.
		p.pullMissing(ctx, log, dir, services)
	}
	p.phase("starting stack")
	if up {
		log.Info("starting stack (compose up)", "services", services, "compose_files", composeFiles)
		err = p.Docker.Up(ctx, dir, composeFiles, services)
	} else {
		if len(services) == 0 {
			log.Info("no services were running before the job; leaving stack stopped")
			return nil
		}
		log.Info("starting stack (compose start)", "services", services)
		err = p.Docker.Start(ctx, dir, services)
	}
	if err != nil {
		log.Error("STACK WAS NOT STARTED; manual intervention required", "error", err)
		return err
	}
	p.markStarted()

	hctx, hcancel := context.WithTimeout(ctx, orDefault(p.HealthTimeout, DefaultHealthTimeout))
	defer hcancel()
	p.phase("waiting until healthy")
	if err := p.Docker.WaitHealthy(hctx, dir, services); err != nil {
		log.Warn("stack started but not all services are healthy", "error", err)
	} else {
		log.Info("stack is up")
	}
	return nil
}
