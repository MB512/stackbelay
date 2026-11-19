package jobs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Downtime slots (issue #36): only the part of a job that keeps a stack
// stopped (stop -> final snapshot -> start) is limited by
// STACKVAULT_MAX_DOWNTIMES; the pre-stop snapshot before it and restores
// are not. The worker asks the manager for a slot through its job
// directory, the only channel between them: it writes DowntimeRequestFile
// and waits until the manager wrote DowntimeGrantFile. Both live as long
// as the job directory, so a manager that was restarted (or stopped by
// its own backup) finds the state again. A slot is held until the job
// ends.
const (
	DowntimeRequestFile = "downtime-request"
	DowntimeGrantFile   = "downtime-granted"
)

// DowntimeGate gives a job the right to stop its stack. Acquire blocks
// until the right is granted or ctx ends.
type DowntimeGate interface {
	Acquire(ctx context.Context) error
}

// FileDowntimeGate is the worker's side of the downtime slots.
type FileDowntimeGate struct {
	Dir  string        // the job directory
	Poll time.Duration // how often the grant is checked; default 1s
}

// Acquire implements DowntimeGate.
func (g *FileDowntimeGate) Acquire(ctx context.Context) error {
	request := filepath.Join(g.Dir, DowntimeRequestFile)
	if err := os.WriteFile(request, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil { //nolint:gosec // G306: readable by the manager, no secrets
		return fmt.Errorf("requesting a downtime slot: %w", err)
	}
	if err := MatchDirOwner(request); err != nil {
		return fmt.Errorf("requesting a downtime slot: %w", err)
	}
	poll := g.Poll
	if poll <= 0 {
		poll = time.Second
	}
	// A ticker is like a System.Threading.Timer that fires every poll;
	// select waits for whichever channel is ready first.
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if requested, granted := DowntimeState(g.Dir); granted {
			return nil
		} else if !requested {
			return errors.New("the downtime request disappeared from the job directory")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// DowntimeState reports whether the job in dir asked for a downtime slot
// and whether it was granted (manager side; also used by the worker).
func DowntimeState(dir string) (requested, granted bool) {
	return isRegular(filepath.Join(dir, DowntimeRequestFile)), isRegular(filepath.Join(dir, DowntimeGrantFile))
}

func isRegular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// GrantDowntime gives the job in dir its downtime slot (manager side).
func GrantDowntime(dir string) error {
	err := os.WriteFile(filepath.Join(dir, DowntimeGrantFile), nil, 0o644) //nolint:gosec // G306: readable by the worker, no secrets
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("granting a downtime slot: %w", err)
	}
	return nil
}

// acquireDowntime waits for a downtime slot if the pipeline has a gate
// (jobs started by the manager; a worker run by hand has none).
func (p *Pipeline) acquireDowntime(ctx context.Context, log *slog.Logger) error {
	if p.Downtime == nil {
		return nil
	}
	log.Info("waiting for a downtime slot (STACKVAULT_MAX_DOWNTIMES)")
	p.phase("waiting for a downtime slot")
	if err := p.Downtime.Acquire(ctx); err != nil {
		return fmt.Errorf("waiting for a downtime slot (the stack was not stopped): %w", err)
	}
	log.Info("downtime slot granted")
	return nil
}

// downtimeContext returns the context for stop -> final snapshot: ctx
// bounded by p.DowntimeTimeout, if set.
func (p *Pipeline) downtimeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.DowntimeTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.DowntimeTimeout)
}

// downtimeErr explains err if it was caused by the downtime timeout (dctx
// ended, the job's ctx did not): the final snapshot was aborted and the
// stack is started again.
func (p *Pipeline) downtimeErr(ctx, dctx context.Context, err error) error {
	if ctx.Err() == nil && errors.Is(dctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("downtime timeout (%s, STACKVAULT_DOWNTIME_TIMEOUT) exceeded: the final snapshot was aborted "+
			"and the stack is started again: %w", p.DowntimeTimeout, err)
	}
	return err
}
