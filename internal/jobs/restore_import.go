package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/MB512/stackvault/internal/stacks"
)

// Import is the first restore of a stack from another host (issue #1, see
// "Restore from another host" in docs/architecture.md): the stack directory
// does not exist yet, so there is nothing to stop, to protect with a safety
// snapshot or to swap. The snapshot is restored into the staging directory,
// which is then renamed to the stack directory. Later syncs of the same
// stack are normal restores (Restore), usually in place.
//
// target comes from stacks.ImportTarget (valid name, no directory yet).
// Only opts.SnapshotID, NoStart and PinImages apply. On any failure before
// the rename the staging directory is removed and nothing is left behind.
// The image digests of metadata.txt are compared with the local images and
// missing ones are reported (a start pulls them; with PinImages by digest).
func (p *Pipeline) Import(ctx context.Context, target stacks.Stack, opts RestoreOptions) (res RestoreResult, err error) {
	log := p.logger().With("stack", target.Name, "job", "import", "snapshot", opts.SnapshotID)
	if opts.SkipSafetySnapshot || opts.InPlace || opts.CarryOver != "" {
		return res, errors.New("an import restores into a new directory: no safety snapshot, in-place or carry-over options")
	}
	snap, srcPath, err := p.resolveSnapshot(ctx, log, opts.SnapshotID, target.Name)
	if err != nil {
		return res, err
	}
	res.Snapshot = snap.ID

	staging := stacks.StagingDir(target)
	created := false // the stack directory exists (renamed from staging)
	defer func() {
		if err != nil && !created {
			if rmErr := os.RemoveAll(staging); rmErr != nil {
				log.Error("could not remove staging directory", "dir", staging, "error", rmErr)
			}
		}
	}()

	log.Info("restoring into a new stack directory", "dir", target.Dir, "staging", staging, "from_host", snap.Hostname)
	p.phase("restoring")
	if err := p.Restic.Restore(ctx, p.Repo, snap.ID, srcPath, staging); err != nil {
		return res, fmt.Errorf("restoring snapshot: %w", err)
	}
	if ok, err := stacks.HasComposeFile(staging); err != nil {
		return res, fmt.Errorf("checking restored compose file: %w", err)
	} else if !ok {
		return res, fmt.Errorf("restored snapshot does not contain a compose file (%s)", strings.Join(stacks.MainFileNames(), ", "))
	}
	if err := p.applyDirMetadata(ctx, log, snap.ID, srcPath, staging); err != nil {
		return res, err
	}
	p.reportImages(ctx, log, target.Name, staging)

	var composeFiles []string
	if opts.PinImages && !opts.NoStart {
		files, cleanup, err := p.preparePinning(ctx, log, target.Name, staging, target.Dir)
		if err != nil {
			return res, err
		}
		defer cleanup()
		composeFiles = files
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Rename fails if the directory appeared in the meantime (rename onto a
	// non-empty directory is refused), so nothing is overwritten.
	if err := os.Rename(staging, target.Dir); err != nil {
		return res, fmt.Errorf("creating stack directory: %w", err)
	}
	created = true
	log.Info("stack directory created", "dir", target.Dir)

	if opts.NoStart {
		log.Info("not starting stack (requested)")
		return res, nil
	}
	toStart, ok := p.servicesToStart(log, target)
	if ok {
		toStart = p.definedOnly(context.WithoutCancel(ctx), log, target.Dir, toStart)
	}
	if ok && len(toStart) == 0 {
		log.Info("no services were running at snapshot time; leaving stack stopped")
		return res, nil
	}
	if err := p.bringUp(ctx, log, target.Dir, composeFiles, toStart, true); err != nil {
		return res, fmt.Errorf("starting imported stack: %w", err)
	}
	return res, nil
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
