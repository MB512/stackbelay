package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

// BackupResult reports the snapshots created by a backup job.
type BackupResult struct {
	PreStopSnapshot string // "" if not taken or failed
	Snapshot        string // the final snapshot; set even on restic.ErrIncomplete
}

// Backup runs the backup pipeline for stack (see docs/architecture.md):
// metadata.txt -> pre-stop snapshot -> stop -> final snapshot -> start.
// The pre-stop snapshot (taken while the stack runs, issue #34: always) is
// the parent of the final one, so the stack is only down for the changes
// in between. Once the stack has been stopped it is always started again, even if
// the job fails or ctx is cancelled.
//
// The named return value err lets the deferred restart add its own error
// to the one being returned (a defer runs like a C# finally block, and can
// still modify named results).
func (p *Pipeline) Backup(ctx context.Context, stack stacks.Stack) (res BackupResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", "backup")

	// 1. Preflight.
	if err := requireComplete(stack); err != nil {
		return res, err
	}
	log.Info("checking repository")
	if err := p.Restic.CheckRepo(ctx, p.Repo); err != nil {
		return res, fmt.Errorf("repository not reachable: %w", err)
	}

	// 2. Record state.
	services, err := p.services(ctx, log, stack.Dir)
	if err != nil {
		return res, fmt.Errorf("reading stack state: %w", err)
	}
	var running []string
	for _, s := range services {
		if s.Running() {
			running = append(running, s.Name)
		}
	}
	log.Info("recorded stack state", "services", len(services), "running", running)
	if err := p.writeMetadata(log, stack, services); err != nil {
		return res, fmt.Errorf("writing %s: %w", MetadataFile, err)
	}

	// 3. Pre-stop snapshot while the stack runs (nothing running: the
	// final snapshot is the only one). Failure is not fatal.
	if len(running) > 0 {
		res.PreStopSnapshot = p.preStopSnapshot(ctx, log, stack)
	}
	if err := ctx.Err(); err != nil {
		return res, err // cancelled before anything was stopped
	}
	// Only stopping a running stack needs a downtime slot.
	if len(running) > 0 {
		if err := p.acquireDowntime(ctx, log); err != nil {
			return res, err
		}
	}
	// Stop and final snapshot are bounded by the downtime timeout; the
	// start below uses its own (bringUp ignores dctx's cancellation).
	dctx, cancel := p.downtimeContext(ctx)
	defer cancel()

	// 6. (registered before 4) Start again, always.
	defer func() {
		if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
			err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
		}
	}()

	// 4. Stop.
	log.Info("stopping stack")
	p.phase("stopping stack")
	if err := p.Docker.Stop(dctx, stack.Dir); err != nil {
		return res, p.downtimeErr(ctx, dctx, fmt.Errorf("stopping stack: %w", err))
	}

	// 5. Final snapshot.
	log.Info("taking final snapshot", "parent", res.PreStopSnapshot)
	p.phase("final snapshot")
	id, err := p.Restic.Backup(dctx, p.Repo, stack.Dir, restic.BackupOptions{
		Host: p.Host, Tags: []string{TagFinal}, Parent: res.PreStopSnapshot,
	})
	res.Snapshot = id
	if err != nil {
		return res, p.downtimeErr(ctx, dctx, fmt.Errorf("final snapshot: %w", err))
	}
	log.Info("final snapshot done", "snapshot", id)
	return res, nil
}

// writeMetadata writes metadata.txt atomically into the stack directory.
func (p *Pipeline) writeMetadata(log *slog.Logger, stack stacks.Stack, services []docker.ServiceState) error {
	m := &Metadata{CreatedAt: p.now().UTC(), Stack: stack.Name}
	if h, err := fileSHA256(stack.ComposeFile); err != nil {
		log.Warn("could not hash compose file", "error", err)
	} else {
		m.ComposeSHA256 = h
	}
	for _, s := range services {
		rec, ok := serviceRecord(log, s)
		if ok {
			m.Services = append(m.Services, rec)
		}
	}

	tmp := filepath.Join(stack.Dir, MetadataFile+".tmp")
	// tmp is inside a discovered stack directory; metadata.txt is meant to
	// be world-readable like the rest of the stack.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G302/G304: see comment above

	if err != nil {
		return err
	}
	if err := WriteMetadata(f, m); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what matters
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(stack.Dir, MetadataFile))
}

// serviceRecord converts a docker service state into a metadata record.
// Values that do not pass validation are dropped with a warning, so an
// unusual image reference never prevents a backup.
func serviceRecord(log *slog.Logger, s docker.ServiceState) (ServiceRecord, bool) {
	if !serviceNamePattern.MatchString(s.Name) {
		log.Warn("service name not representable in metadata; skipping", "service", s.Name)
		return ServiceRecord{}, false
	}
	rec := ServiceRecord{
		Name:        s.Name,
		State:       s.State,
		ImageRef:    s.ImageRef,
		RepoDigest:  pickRepoDigest(s.ImageRef, s.RepoDigests),
		ImageID:     s.ImageID,
		ContainerID: s.ContainerID,
	}
	if rec.State == "" || !validStates[rec.State] {
		rec.State = none
	}
	drop := func(field string, val *string, ok bool) {
		if *val != "" && !ok {
			log.Warn("dropping invalid value from metadata", "service", s.Name, "field", field, "value", *val)
			*val = ""
		}
	}
	drop("image_ref", &rec.ImageRef, imageRefPattern.MatchString(rec.ImageRef))
	drop("image_id", &rec.ImageID, imageIDPattern.MatchString(rec.ImageID))
	drop("container_id", &rec.ContainerID, containerIDPattern.MatchString(rec.ContainerID))
	if rec.RepoDigest == "" && s.ContainerID != "" {
		log.Info("no pullable repo digest for service (locally built or unsupported registry name)", "service", s.Name)
	}
	return rec, true
}

// pickRepoDigest chooses the repo digest that belongs to the configured image
// reference. Only digests matching RepoDigestPattern are considered.
func pickRepoDigest(ref string, digests []string) string {
	repo := refRepository(ref)
	var fallback string
	for _, d := range digests {
		if !RepoDigestPattern.MatchString(d) {
			continue
		}
		name, _, _ := strings.Cut(d, "@")
		if name == repo {
			return d
		}
		if fallback == "" {
			fallback = d
		}
	}
	return fallback
}

// refRepository strips tag and digest from an image reference:
// "nextcloud:29" -> "nextcloud", "ghcr.io/a/b:1@sha256:..." -> "ghcr.io/a/b".
func refRepository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is a discovered compose file
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// preStopSnapshot takes the snapshot of the running stack that becomes the
// parent of the final one. It returns "" if it failed: the backup then
// continues without parent (the final snapshot takes longer).
func (p *Pipeline) preStopSnapshot(ctx context.Context, log *slog.Logger, stack stacks.Stack) string {
	log.Info("taking pre-stop snapshot")
	p.phase("pre-stop snapshot")
	id, err := p.Restic.Backup(ctx, p.Repo, stack.Dir, restic.BackupOptions{Host: p.Host, Tags: []string{TagPreStop}})
	if err != nil {
		log.Warn("pre-stop snapshot failed; continuing without parent", "error", err)
		return ""
	}
	log.Info("pre-stop snapshot done", "snapshot", id)
	return id
}
