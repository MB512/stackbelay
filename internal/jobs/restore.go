package jobs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

// RestoreOptions control a restore job.
type RestoreOptions struct {
	SnapshotID string
	// SkipSafetySnapshot disables the pre-restore snapshot (admin only).
	SkipSafetySnapshot bool
	// NoStart leaves the stack stopped after a successful restore.
	NoStart bool
}

// RestoreResult reports what a restore job did.
type RestoreResult struct {
	Snapshot       string // full id of the restored snapshot
	SafetySnapshot string // pre-restore snapshot, "" if skipped
	OldDir         string // previous stack directory, kept for manual rollback
}

// Restore runs the restore pipeline for an existing stack (see
// docs/architecture.md): validate snapshot -> stop -> safety snapshot ->
// restore into staging -> swap directories -> start.
//
// Until the swap has happened, any failure rolls back: the staging directory
// is removed and the services that were running before are started again.
// The original stack directory is never modified before the swap.
func (p *Pipeline) Restore(ctx context.Context, stack stacks.Stack, opts RestoreOptions) (res RestoreResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", "restore", "snapshot", opts.SnapshotID)

	// 1. Preflight: validate and resolve the snapshot before touching anything.
	if !restic.SnapshotIDPattern.MatchString(opts.SnapshotID) {
		return res, fmt.Errorf("invalid snapshot id %q", opts.SnapshotID)
	}
	if err := p.Restic.CheckRepo(ctx, p.Repo); err != nil {
		return res, fmt.Errorf("repository not reachable: %w", err)
	}
	snaps, err := p.Restic.Snapshots(ctx, p.Repo, opts.SnapshotID)
	if err != nil {
		return res, fmt.Errorf("looking up snapshot: %w", err)
	}
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0].ID, opts.SnapshotID) {
		return res, fmt.Errorf("snapshot %s not found (or ambiguous)", opts.SnapshotID)
	}
	snap := snaps[0]
	res.Snapshot = snap.ID
	srcPath, err := snapshotStackPath(snap, stack.Name)
	if err != nil {
		return res, err
	}
	log.Info("snapshot verified", "id", snap.ID, "host", snap.Hostname, "path", srcPath, "time", snap.Time)

	staging := stack.Dir + ".restore-tmp"
	if _, err := os.Lstat(staging); err == nil {
		return res, fmt.Errorf("staging directory %s already exists (left over from a failed restore?); remove it manually", staging)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("checking staging directory: %w", err)
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
	log.Info("recorded stack state", "running", running)

	// Rollback for everything before the swap.
	swapped := false
	defer func() {
		if swapped {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			log.Error("could not remove staging directory", "dir", staging, "error", rmErr)
		}
		log.Warn("restore did not complete; restarting previous state")
		if startErr := p.bringUp(ctx, log, stack.Dir, running, false); startErr != nil {
			err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
		}
	}()

	// 2./3. Stop, then safety snapshot of the stopped (consistent) stack.
	log.Info("stopping stack")
	if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
		return res, fmt.Errorf("stopping stack: %w", err)
	}
	if opts.SkipSafetySnapshot {
		log.Warn("safety snapshot disabled")
	} else {
		log.Info("taking pre-restore safety snapshot")
		id, err := p.Restic.Backup(ctx, p.Repo, stack.Dir, restic.BackupOptions{Host: p.Host, Tags: []string{TagPreRestore}})
		if err != nil {
			return res, fmt.Errorf("safety snapshot: %w", err)
		}
		res.SafetySnapshot = id
		log.Info("safety snapshot done", "snapshot", id)
	}

	// 4. Restore into staging (same filesystem, so the swap is atomic).
	log.Info("restoring into staging directory", "dir", staging)
	if err := p.Restic.Restore(ctx, p.Repo, snap.ID, srcPath, staging); err != nil {
		return res, fmt.Errorf("restoring snapshot: %w", err)
	}
	if ok, err := stacks.HasComposeFile(staging); err != nil {
		return res, fmt.Errorf("checking restored compose file: %w", err)
	} else if !ok {
		return res, errors.New("restored snapshot does not contain compose.yml/compose.yaml")
	}
	if err := p.applyDirMetadata(ctx, log, snap.ID, srcPath, staging); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 5. Swap.
	oldDir := fmt.Sprintf("%s.old-%s", stack.Dir, p.now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(stack.Dir, oldDir); err != nil {
		return res, fmt.Errorf("moving current stack directory aside: %w", err)
	}
	if err := os.Rename(staging, stack.Dir); err != nil {
		if rbErr := os.Rename(oldDir, stack.Dir); rbErr != nil {
			log.Error("ROLLBACK FAILED: original stack directory is at "+oldDir, "error", rbErr)
			return res, errors.Join(fmt.Errorf("activating restored directory: %w", err),
				fmt.Errorf("moving %s back: %w", oldDir, rbErr))
		}
		return res, fmt.Errorf("activating restored directory: %w", err)
	}
	swapped = true
	res.OldDir = oldDir
	log.Info("stack directory replaced", "previous", oldDir)

	// 6./7. Start and verify.
	if opts.NoStart {
		log.Info("not starting stack (requested)")
		return res, nil
	}
	toStart, ok := p.servicesToStart(log, stack)
	if ok && len(toStart) == 0 {
		log.Info("no services were running at snapshot time; leaving stack stopped")
		return res, nil
	}
	if err := p.bringUp(ctx, log, stack.Dir, toStart, true); err != nil {
		return res, fmt.Errorf("starting restored stack (previous version kept at %s): %w", oldDir, err)
	}
	return res, nil
}

// applyDirMetadata sets owner, mode and mtime of the staging directory to
// those of the stack directory in the snapshot. "restic restore <id>:<path>"
// restores everything below path but creates the target itself with default
// permissions (0700, current user).
func (p *Pipeline) applyDirMetadata(ctx context.Context, log *slog.Logger, snapshotID, srcPath, dir string) error {
	node, err := p.Restic.Node(ctx, p.Repo, snapshotID, srcPath)
	if err != nil {
		return fmt.Errorf("reading stack directory metadata from snapshot: %w", err)
	}
	if node.Type != "dir" {
		return fmt.Errorf("snapshot path %s is a %q, not a directory", srcPath, node.Type)
	}
	// Owner first: chown clears setuid/setgid bits, chmod sets them again.
	if err := os.Lchown(dir, int(node.UID), int(node.GID)); err != nil {
		if os.Geteuid() == 0 {
			return fmt.Errorf("setting owner of restored stack directory: %w", err)
		}
		log.Warn("not running as root; cannot restore owner of stack directory",
			"uid", node.UID, "gid", node.GID, "error", err)
	}
	mode := node.Mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("setting mode of restored stack directory: %w", err)
	}
	if err := os.Chtimes(dir, node.ModTime, node.ModTime); err != nil {
		log.Warn("could not restore mtime of stack directory", "error", err)
	}
	return nil
}

// servicesToStart reads metadata.txt from the restored stack and returns the
// services that were running at backup time. ok=false means the metadata is
// missing or invalid and all services should be started (nil slice).
func (p *Pipeline) servicesToStart(log *slog.Logger, stack stacks.Stack) (services []string, ok bool) {
	f, err := os.Open(filepath.Join(stack.Dir, MetadataFile))
	if err != nil {
		log.Warn("no usable metadata.txt in snapshot; starting all services", "error", err)
		return nil, false
	}
	defer f.Close()
	m, err := ParseMetadata(f, stack.Name)
	if err != nil {
		log.Warn("invalid metadata.txt in snapshot; starting all services", "error", err)
		return nil, false
	}
	return m.RunningServices(), true
}

// snapshotStackPath validates the snapshot's own path list and returns the
// path to restore from. The snapshot must contain exactly one absolute,
// clean path whose last element is the stack name.
func snapshotStackPath(snap restic.Snapshot, stackName string) (string, error) {
	if len(snap.Paths) != 1 {
		return "", fmt.Errorf("snapshot %s has %d paths, expected exactly 1", snap.ShortID, len(snap.Paths))
	}
	p := snap.Paths[0]
	switch {
	case !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/":
		return "", fmt.Errorf("snapshot %s has an invalid path %q", snap.ShortID, truncate(p))
	case strings.ContainsAny(p, "\x00\n"):
		return "", fmt.Errorf("snapshot %s path contains control characters", snap.ShortID)
	case path.Base(p) != stackName:
		return "", fmt.Errorf("snapshot %s belongs to %q, not to stack %q", snap.ShortID, truncate(path.Base(p)), stackName)
	}
	return p, nil
}
