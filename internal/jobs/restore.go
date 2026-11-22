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
	"slices"
	"strings"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/stacks"
)

// RestoreOptions control a restore job.
type RestoreOptions struct {
	SnapshotID string
	// skipSafetySnapshot: no pre-restore snapshot. Only for the automatic
	// rollback (its final snapshot was taken minutes before); every other
	// restore takes one (issue #33).
	skipSafetySnapshot bool
	// NoStart leaves the stack stopped after a successful restore.
	NoStart bool
	// PinImages starts the stack with the image versions recorded in the
	// snapshot's metadata.txt (by repo digest) instead of the tags in
	// compose.yml. See "Pinning image versions on restore".
	PinImages bool
	// CarryOver is a directory inside the stack (relative path) whose
	// current content must survive the restore. Used when the manager
	// restores its own stack: its job files live there (the restored copy
	// is older). It is excluded from the restore. Empty: nothing is
	// carried over.
	CarryOver string
}

// RestoreResult reports what a restore job did.
type RestoreResult struct {
	Snapshot       string // full id of the restored snapshot
	SafetySnapshot string // pre-restore snapshot, "" if skipped
}

// Restore runs the restore pipeline for an existing stack (see
// docs/architecture.md): validate snapshot -> stop -> safety snapshot ->
// restore over the stack directory -> start. Only files that differ from
// the snapshot are read from the repository and rewritten; files that are
// not in the snapshot are deleted (issue #33: there is no restore into a
// second directory).
//
// Before restic starts writing, any failure starts the services that were
// running before again. After that, a failure leaves the stack stopped:
// starting a partly restored stack could damage its data further. The
// directory then keeps stacks.IncompleteMarker, so nothing starts, backs up
// or changes the stack until a restore completes; the next restore
// continues the incomplete one (without a safety snapshot of the partly
// restored directory).
func (p *Pipeline) Restore(ctx context.Context, stack stacks.Stack, opts RestoreOptions) (res RestoreResult, err error) {
	log := p.logger().With("stack", stack.Name, "job", "restore", "snapshot", opts.SnapshotID)

	// 1. Preflight: validate and resolve the snapshot before touching anything.
	if opts.CarryOver != "" && !ValidCarryOver(opts.CarryOver) {
		return res, fmt.Errorf("invalid carry-over directory %q", truncate(opts.CarryOver))
	}
	snap, srcPath, err := p.resolveSnapshot(ctx, log, opts.SnapshotID, stack.Name)
	if err != nil {
		return res, err
	}
	res.Snapshot = snap.ID
	incomplete := stacks.RestoreIncomplete(stack.Dir)

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
	log.Info("recorded stack state", "running", running)

	// changed: restic may have started writing into the stack directory.
	// restored: the directory is completely restored. starting: the
	// restored stack is being started.
	changed, restored, starting := false, false, false
	defer func() {
		switch {
		case err == nil || starting:
			return
		case changed && !restored:
			log.Error("RESTORE INCOMPLETE: the stack directory may be partly restored; the stack is left stopped",
				"safety_snapshot", res.SafetySnapshot)
			err = fmt.Errorf("%w; %s", err, partialRestoreAdvice(res.SafetySnapshot))
		case changed:
			err = fmt.Errorf("%w; the stack directory is restored, but the stack was not started", err)
		default:
			log.Warn("restore did not start; restarting previous state")
			if startErr := p.bringUp(ctx, log, stack.Dir, nil, running, false); startErr != nil {
				err = errors.Join(err, fmt.Errorf("restarting stack: %w", startErr))
			}
		}
	}()

	// 2./3. Stop, then safety snapshot of the stopped (consistent) stack.
	log.Info("stopping stack")
	p.phase("stopping stack")
	if err := p.Docker.Stop(ctx, stack.Dir); err != nil {
		return res, fmt.Errorf("stopping stack: %w", err)
	}
	switch {
	case opts.skipSafetySnapshot:
		log.Info("no safety snapshot: rolling back to the snapshot taken just before")
	case incomplete:
		log.Warn("the last restore of this stack did not complete: continuing it, without a safety snapshot " +
			"of the partly restored directory (the safety snapshot of that restore is the way back)")
	default:
		log.Info("taking pre-restore safety snapshot")
		p.phase("safety snapshot")
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

	// 4. Restore over the stack directory.
	changed = true
	if err := p.restoreFiles(ctx, log, snap.ID, srcPath, stack.Dir, opts.CarryOver); err != nil {
		return res, err
	}
	restored = true
	var composeFiles []string
	switch {
	case opts.PinImages && opts.NoStart:
		log.Info("image pinning skipped: the stack is not started")
	case opts.PinImages:
		files, cleanup, err := p.preparePinning(ctx, log, stack.Name, stack.Dir, stack.Dir)
		if err != nil {
			return res, err
		}
		defer cleanup()
		composeFiles = files
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 5./6. Start and verify.
	starting = true
	if err := p.startRestored(ctx, log, stack, composeFiles, opts.NoStart); err != nil {
		return res, fmt.Errorf("starting restored stack: %w", err)
	}
	return res, nil
}

// restoreFiles restores the snapshot's srcPath over dir and applies the
// metadata of the stack directory itself. stacks.IncompleteMarker is
// created first and removed only when everything is restored; restic
// leaves it (and carryOver) alone.
func (p *Pipeline) restoreFiles(ctx context.Context, log *slog.Logger, snapshotID, srcPath, dir, carryOver string) error {
	marker := filepath.Join(dir, stacks.IncompleteMarker)
	if err := os.WriteFile(marker, []byte("restore of snapshot "+snapshotID+" started\n"), 0o600); err != nil {
		return fmt.Errorf("marking the restore as started: %w", err)
	}
	exclude := []string{stacks.IncompleteMarker}
	if carryOver != "" {
		exclude = append(exclude, carryOver)
	}
	log.Info("restoring (unchanged files are kept, files not in the snapshot are deleted)", "dir", dir, "kept", exclude)
	p.phase("restoring")
	if err := p.Restic.Restore(ctx, p.Repo, snapshotID, srcPath, dir, exclude); err != nil {
		return fmt.Errorf("restoring snapshot: %w", err)
	}
	if ok, err := stacks.HasComposeFile(dir); err != nil {
		return fmt.Errorf("checking restored compose file: %w", err)
	} else if !ok {
		return fmt.Errorf("restored snapshot does not contain a compose file (%s)", strings.Join(stacks.MainFileNames(), ", "))
	}
	if err := p.applyDirMetadata(ctx, log, snapshotID, srcPath, dir, marker); err != nil {
		return err
	}
	log.Info("stack directory restored", "dir", dir)
	return nil
}

// startRestored starts the services that were running at snapshot time
// (all of them if metadata.txt is missing or invalid), unless noStart.
func (p *Pipeline) startRestored(ctx context.Context, log *slog.Logger, stack stacks.Stack, composeFiles []string, noStart bool) error {
	if noStart {
		log.Info("not starting stack (requested)")
		return nil
	}
	toStart, ok := p.servicesToStart(log, stack)
	if ok {
		// The start must happen even after a cancellation, like bringUp.
		toStart = p.definedOnly(context.WithoutCancel(ctx), log, stack.Dir, toStart)
	}
	if ok && len(toStart) == 0 {
		log.Info("no services were running at snapshot time; leaving stack stopped")
		return nil
	}
	return p.bringUp(ctx, log, stack.Dir, composeFiles, toStart, true)
}

// resolveSnapshot checks the repository and looks up the snapshot by id,
// and returns it with the path to restore from (validated against the
// snapshot's own paths and the stack name).
func (p *Pipeline) resolveSnapshot(ctx context.Context, log *slog.Logger, id, stackName string) (restic.Snapshot, string, error) {
	if !restic.SnapshotIDPattern.MatchString(id) {
		return restic.Snapshot{}, "", fmt.Errorf("invalid snapshot id %q", id)
	}
	if err := p.Restic.CheckRepo(ctx, p.Repo); err != nil {
		return restic.Snapshot{}, "", fmt.Errorf("repository not reachable: %w", err)
	}
	snaps, err := p.Restic.Snapshots(ctx, p.Repo, id)
	if err != nil {
		return restic.Snapshot{}, "", fmt.Errorf("looking up snapshot: %w", err)
	}
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0].ID, id) {
		return restic.Snapshot{}, "", fmt.Errorf("snapshot %s not found (or ambiguous)", id)
	}
	snap := snaps[0]
	srcPath, err := snapshotStackPath(snap, stackName)
	if err != nil {
		return restic.Snapshot{}, "", err
	}
	log.Info("snapshot verified", "id", snap.ID, "host", snap.Hostname, "path", srcPath, "snapshot_time", snap.Time)
	return snap, srcPath, nil
}

// partialRestoreAdvice tells how to get out of an incomplete restore.
func partialRestoreAdvice(safety string) string {
	if safety == "" {
		return "the stack directory may be partly restored and the stack was left stopped; " +
			"run the restore again to continue it"
	}
	return fmt.Sprintf("the stack directory may be partly restored and the stack was left stopped; run the restore again "+
		"to continue it, or restore the safety snapshot %s to return to the state before", shortID(safety))
}

// ValidCarryOver reports whether dir may be used as RestoreOptions.CarryOver:
// a clean relative path inside the stack.
func ValidCarryOver(dir string) bool {
	return dir != "" && dir != "." && filepath.IsLocal(dir) && filepath.Clean(dir) == dir
}

// applyDirMetadata sets owner, mode and mtime of the restored directory to
// those of the stack directory in the snapshot. "restic restore <id>:<path>"
// restores everything below path but creates the target itself with default
// permissions (0700, current user). marker (the incomplete-restore marker)
// is removed in between: removing a file changes the directory's mtime.
func (p *Pipeline) applyDirMetadata(ctx context.Context, log *slog.Logger, snapshotID, srcPath, dir, marker string) error {
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
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("marking the restore as complete: %w", err)
	}
	// Left by an incomplete restore of StackVault 0.4 or older (issue #44).
	if err := os.Remove(filepath.Join(dir, stacks.LegacyIncompleteMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("marking the restore as complete: %w", err)
	}
	if err := os.Chtimes(dir, node.ModTime, node.ModTime); err != nil {
		log.Warn("could not restore mtime of stack directory", "error", err)
	}
	return nil
}

// definedOnly keeps the services of names that the restored compose file
// defines: a metadata.txt written before issue #26 may list an orphan
// (a container whose service was no longer in the file) as running, and
// compose refuses to start it. If the stack cannot be read, names is kept.
func (p *Pipeline) definedOnly(ctx context.Context, log *slog.Logger, dir string, names []string) []string {
	if len(names) == 0 {
		return names
	}
	defined, err := p.services(ctx, log, dir)
	if err != nil {
		log.Warn("reading the restored stack's services; starting the recorded ones", "error", err)
		return names
	}
	var out []string
	for _, n := range names {
		if slices.ContainsFunc(defined, func(s docker.ServiceState) bool { return s.Name == n }) {
			out = append(out, n)
		} else {
			log.Warn("not starting a service that ran at snapshot time but is not in its compose file", "service", n)
		}
	}
	return out
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
