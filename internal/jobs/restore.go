package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

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
	// PinImages starts the stack with the image versions recorded in the
	// snapshot's metadata.txt (by repo digest) instead of the tags in
	// compose.yml. See "Pinning image versions on restore".
	PinImages bool
	// InPlace restores directly into the stack directory instead of into a
	// staging directory that is then swapped in: only files that differ
	// from the snapshot are read from the repository and rewritten, files
	// that are not in the snapshot are deleted. Much faster for large
	// stacks and needs no second copy on disk, but there is no previous
	// directory to go back to: if the restore fails half-way, the stack is
	// left stopped and partly restored (the safety snapshot is the way
	// back). See "In-place restore" in docs/architecture.md.
	InPlace bool
	// CarryOver is a directory inside the stack (relative path) whose
	// current content must survive the restore. Used when the manager
	// restores its own stack: its job files live there (the restored copy
	// is older). With a swap it is copied from the previous version into
	// the restored stack before the stack starts; in place it is excluded
	// from the restore. Empty: nothing is carried over.
	CarryOver string
}

// RestoreResult reports what a restore job did.
type RestoreResult struct {
	Snapshot       string // full id of the restored snapshot
	SafetySnapshot string // pre-restore snapshot, "" if skipped
	OldDir         string // previous stack directory, kept for manual rollback ("" in place)
}

// Restore runs the restore pipeline for an existing stack (see
// docs/architecture.md): validate snapshot -> stop -> safety snapshot ->
// restore into staging -> swap directories -> start. With opts.InPlace the
// snapshot is restored over the stack directory itself instead (no staging,
// no swap).
//
// Until the stack directory has been changed (swap, or the start of an
// in-place restore), any failure rolls back: the staging directory is
// removed and the services that were running before are started again.
// After that, a failure leaves the stack as it is: with a swap the previous
// directory is kept next to it; in place, the stack stays stopped, because
// starting a partly restored stack could damage its data further.
func (p *Pipeline) Restore(ctx context.Context, stack stacks.Stack, opts RestoreOptions) (res RestoreResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", "restore", "snapshot", opts.SnapshotID, "in_place", opts.InPlace)

	// 1. Preflight: validate and resolve the snapshot before touching anything.
	if !restic.SnapshotIDPattern.MatchString(opts.SnapshotID) {
		return res, fmt.Errorf("invalid snapshot id %q", opts.SnapshotID)
	}
	if opts.CarryOver != "" && !ValidCarryOver(opts.CarryOver) {
		return res, fmt.Errorf("invalid carry-over directory %q", truncate(opts.CarryOver))
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

	// target is the directory restic restores into.
	target := stack.Dir
	if !opts.InPlace {
		target = stacks.StagingDir(stack)
		if _, err := os.Lstat(target); err == nil {
			return res, fmt.Errorf("staging directory %s already exists (left over from a failed restore?); remove it manually", target)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("checking staging directory: %w", err)
		}
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

	// changed: the stack directory is no longer the one the stack ran with
	// (swapped, or an in-place restore has started writing into it).
	// restored: an in-place restore has completed. starting: the restored
	// stack is being started.
	changed, restored, starting := false, false, false
	defer func() {
		switch {
		case err == nil && changed:
			return
		case changed && opts.InPlace && !restored:
			log.Error("RESTORE INCOMPLETE: the stack directory may be partly restored; the stack is left stopped",
				"safety_snapshot", res.SafetySnapshot)
			err = fmt.Errorf("%w; %s", err, partialRestoreAdvice(res.SafetySnapshot))
		case changed && opts.InPlace && !starting:
			err = fmt.Errorf("%w; the stack directory is restored, but the stack was not started", err)
		case changed:
			return
		default:
			if !opts.InPlace {
				if rmErr := os.RemoveAll(target); rmErr != nil {
					log.Error("could not remove staging directory", "dir", target, "error", rmErr)
				}
			}
			log.Warn("restore did not complete; restarting previous state")
			if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
				err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
			}
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
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 4. Restore: into staging (same filesystem, so the swap is atomic), or
	// over the stack directory.
	if opts.InPlace {
		var exclude []string
		if opts.CarryOver != "" {
			exclude = []string{opts.CarryOver}
		}
		log.Info("restoring in place (unchanged files are kept, files not in the snapshot are deleted)",
			"dir", stack.Dir, "kept_unchanged", exclude)
		changed = true
		if err := p.Restic.RestoreInPlace(ctx, p.Repo, snap.ID, srcPath, stack.Dir, exclude); err != nil {
			return res, fmt.Errorf("restoring snapshot in place: %w", err)
		}
	} else {
		log.Info("restoring into staging directory", "dir", target)
		if err := p.Restic.Restore(ctx, p.Repo, snap.ID, srcPath, target); err != nil {
			return res, fmt.Errorf("restoring snapshot: %w", err)
		}
	}
	if ok, err := stacks.HasComposeFile(target); err != nil {
		return res, fmt.Errorf("checking restored compose file: %w", err)
	} else if !ok {
		return res, fmt.Errorf("restored snapshot does not contain a compose file (%s)", strings.Join(stacks.MainFileNames(), ", "))
	}
	if err := p.applyDirMetadata(ctx, log, snap.ID, srcPath, target); err != nil {
		return res, err
	}
	restored = opts.InPlace
	// Pinning prepares everything (metadata, images) before the swap, so a
	// missing image aborts with the old stack still in place (in place:
	// with the restored stack left stopped).
	var composeFiles []string
	switch {
	case opts.PinImages && opts.NoStart:
		log.Info("image pinning skipped: the stack is not started")
	case opts.PinImages:
		files, cleanup, err := p.preparePinning(ctx, log, stack.Name, target, stack.Dir)
		if err != nil {
			return res, err
		}
		defer cleanup()
		composeFiles = files
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 5. Swap.
	if opts.InPlace {
		log.Info("stack directory restored in place")
	} else {
		oldDir := stacks.OldDir(stack, p.now())
		if err := os.Rename(stack.Dir, oldDir); err != nil {
			return res, fmt.Errorf("moving current stack directory aside: %w", err)
		}
		if err := os.Rename(target, stack.Dir); err != nil {
			if rbErr := os.Rename(oldDir, stack.Dir); rbErr != nil {
				log.Error("ROLLBACK FAILED: original stack directory is at "+oldDir, "error", rbErr)
				return res, errors.Join(fmt.Errorf("activating restored directory: %w", err),
					fmt.Errorf("moving %s back: %w", oldDir, rbErr))
			}
			return res, fmt.Errorf("activating restored directory: %w", err)
		}
		changed = true
		res.OldDir = oldDir
		log.Info("stack directory replaced", "previous", oldDir)

		if opts.CarryOver != "" {
			// Not fatal: the restore itself succeeded; only job history is lost.
			if err := CopyTree(filepath.Join(oldDir, opts.CarryOver), filepath.Join(stack.Dir, opts.CarryOver)); err != nil {
				log.Error("could not carry job files over into the restored stack", "dir", opts.CarryOver, "error", err)
			} else {
				log.Info("job files carried over into the restored stack", "dir", opts.CarryOver)
			}
		}
	}

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
	starting = true
	if err := p.bringUp(ctx, log, stack.Dir, composeFiles, toStart, true); err != nil {
		if opts.InPlace {
			return res, fmt.Errorf("starting restored stack: %w", err)
		}
		return res, fmt.Errorf("starting restored stack (previous version kept at %s): %w", res.OldDir, err)
	}
	return res, nil
}

// partialRestoreAdvice tells how to get out of a failed in-place restore.
func partialRestoreAdvice(safety string) string {
	if safety == "" {
		return "the stack directory may be partly restored (no safety snapshot was taken) and the stack was left stopped; " +
			"run the restore again"
	}
	return fmt.Sprintf("the stack directory may be partly restored and the stack was left stopped; run the restore again, "+
		"or restore the safety snapshot %s to return to the state before", shortID(safety))
}

// ValidCarryOver reports whether dir may be used as RestoreOptions.CarryOver:
// a clean relative path inside the stack.
func ValidCarryOver(dir string) bool {
	return dir != "" && dir != "." && filepath.IsLocal(dir) && filepath.Clean(dir) == dir
}

// CopyTree copies the directories and regular files below src into dst,
// overwriting existing files and keeping mode, owner (when root) and
// modification time. Symlinks and other special files are skipped. A
// missing src is not an error.
func CopyTree(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(p, target, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			return nil // symlinks, sockets, ...: job directories have none
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			if err := os.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: paths inside the stack's job directory
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // G304: see above
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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
