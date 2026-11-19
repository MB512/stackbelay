package jobs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/MB512/stackvault/internal/stacks"
)

// Import is the first restore of a stack from another host (issue #1, see
// "Restore from another host" in docs/architecture.md): the stack directory
// does not exist yet, so there is nothing to stop or to protect with a
// safety snapshot. The directory is created and the snapshot restored into
// it like any restore (issue #33).
//
// target comes from stacks.ImportTarget: no directory yet, or one left by
// an incomplete import (stacks.IncompleteMarker), which is continued: only
// missing and changed files are downloaded. A failed import leaves the
// directory with the marker for that. Only opts.SnapshotID, NoStart and
// PinImages apply. The image digests of metadata.txt are compared with the
// local images and missing ones are reported (a start pulls them; with
// PinImages by digest).
func (p *Pipeline) Import(ctx context.Context, target stacks.Stack, opts RestoreOptions) (res RestoreResult, err error) {
	log := p.logger().With("stack", target.Name, "job", "import", "snapshot", opts.SnapshotID)
	if opts.CarryOver != "" {
		return res, errors.New("an import restores into a new directory: no carry-over option")
	}
	snap, srcPath, err := p.resolveSnapshot(ctx, log, opts.SnapshotID, target.Name)
	if err != nil {
		return res, err
	}
	res.Snapshot = snap.ID

	if err := p.createImportDir(log, target.Dir, snap.Hostname); err != nil {
		return res, err
	}
	restored := false
	defer func() {
		switch {
		case err == nil:
		case !restored:
			err = fmt.Errorf("%w; the import is incomplete: run the restore again to continue it "+
				"(only missing and changed files are downloaded)", err)
		default:
			err = fmt.Errorf("%w; the stack directory is restored, but the stack was not started", err)
		}
	}()

	if err := p.restoreFiles(ctx, log, snap.ID, srcPath, target.Dir, ""); err != nil {
		return res, err
	}
	restored = true
	p.reportImages(ctx, log, target.Name, target.Dir)

	var composeFiles []string
	if opts.PinImages && !opts.NoStart {
		files, cleanup, err := p.preparePinning(ctx, log, target.Name, target.Dir, target.Dir)
		if err != nil {
			return res, err
		}
		defer cleanup()
		composeFiles = files
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := p.startRestored(ctx, log, target, composeFiles, opts.NoStart); err != nil {
		return res, fmt.Errorf("starting imported stack: %w", err)
	}
	return res, nil
}

// createImportDir creates the stack directory of an import, marked as
// incomplete, or accepts the directory of an incomplete import. Mkdir
// fails if anything appeared at that path in the meantime, so nothing is
// overwritten.
func (p *Pipeline) createImportDir(log *slog.Logger, dir, fromHost string) error {
	err := os.Mkdir(dir, 0o700)
	switch {
	case err == nil:
		if err := os.WriteFile(filepath.Join(dir, stacks.IncompleteMarker), nil, 0o600); err != nil {
			_ = os.Remove(dir) // empty: nothing to lose
			return fmt.Errorf("marking the import as started: %w", err)
		}
		log.Info("restoring into a new stack directory", "dir", dir, "from_host", fromHost)
		return nil
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("creating stack directory: %w", err)
	}
	if fi, lerr := os.Lstat(dir); lerr != nil || !fi.IsDir() || !stacks.RestoreIncomplete(dir) {
		return fmt.Errorf("creating stack directory: %w", err)
	}
	log.Info("continuing an incomplete import (only missing and changed files are downloaded)", "dir", dir, "from_host", fromHost)
	return nil
}

// reportImages compares the image digests recorded in metadata.txt with
// the local images and logs the missing ones. Informational only: a start
// pulls missing images (by digest with pinning), and an image may be
// gone from the registry, which the log should make visible early.
func (p *Pipeline) reportImages(ctx context.Context, log *slog.Logger, stackName, dir string) {
	f, err := os.Open(filepath.Join(dir, MetadataFile)) //nolint:gosec // G304: inside the restored directory
	if err != nil {
		log.Warn("the snapshot has no metadata.txt: the image versions it ran are unknown", "error", err)
		return
	}
	m, err := ParseMetadata(f, stackName)
	_ = f.Close()
	if err != nil {
		log.Warn("invalid metadata.txt in snapshot: image versions not compared", "error", err)
		return
	}
	var missing, present []string
	for _, s := range m.Services {
		if s.RepoDigest == "" {
			continue
		}
		ok, err := p.Docker.ImageExists(ctx, s.RepoDigest)
		if err != nil {
			log.Warn("checking a local image", "service", s.Name, "image", s.RepoDigest, "error", err)
			continue
		}
		if ok {
			present = append(present, s.Name)
		} else {
			missing = append(missing, s.Name+" ("+s.RepoDigest+")")
		}
	}
	if len(missing) > 0 {
		log.Warn("images of the snapshot are not on this host; starting pulls them (by digest with pinned image versions)",
			"missing", missing, "present", present)
		return
	}
	log.Info("all image versions of the snapshot are on this host", "services", present)
}
