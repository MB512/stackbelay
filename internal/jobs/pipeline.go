// Package jobs implements the backup and restore pipelines.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
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

	hctx, hcancel := context.WithTimeout(ctx, orDefault(p.HealthTimeout, DefaultHealthTimeout))
	defer hcancel()
	if err := p.Docker.WaitHealthy(hctx, dir, services); err != nil {
		log.Warn("stack started but not all services are healthy", "error", err)
	} else {
		log.Info("stack is up")
	}
	return nil
}
