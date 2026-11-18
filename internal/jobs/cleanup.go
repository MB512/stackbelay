package jobs

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/stacks"
)

// Host cleanup (issue #19, see "Host cleanup" in docs/architecture.md).
// The rules live here, so the manager's preview and the worker's job decide
// alike; the job recomputes them and trusts nothing from the preview.

// CleanupLister is the read-only part of the cleanup (*docker.CLI): the
// manager's preview needs only this.
type CleanupLister interface {
	DanglingImages(ctx context.Context) ([]docker.DanglingImage, error)
	StackOrphans(ctx context.Context, dir string) ([]docker.OrphanContainer, error)
}

// Cleaner is what the cleanup job needs from docker (*docker.CLI).
type Cleaner interface {
	CleanupLister
	RemoveImage(ctx context.Context, id string) error
	RemoveStoppedContainer(ctx context.Context, id string) error
}

// CleanupOptions are the rules of a cleanup.
type CleanupOptions struct {
	// ImageMinAge: a dangling image is only removed when it was last
	// pulled or tagged on this host (else built) at least this long ago.
	ImageMinAge time.Duration
	// KeepImages are image ids an update or compose apply replaced within
	// ImageMinAge (store.ReplacedImagesSince): Docker records no "dangling
	// since" time, so these are kept by id.
	KeepImages []string
}

// ImageCandidate is a dangling image and what the cleanup does with it.
type ImageCandidate struct {
	docker.DanglingImage
	// Since is when the image was last pulled or tagged on this host, or
	// built if docker has no such time.
	Since  time.Time
	Remove bool
	Reason string // why it is kept; "" if removed
}

// OrphanCandidate is an orphan container and what the cleanup does with it.
type OrphanCandidate struct {
	Stack string
	docker.OrphanContainer
	Remove bool
	Reason string // why it is kept; "" if removed
}

// CleanupPlan is what a cleanup would remove now.
type CleanupPlan struct {
	Orphans []OrphanCandidate
	Images  []ImageCandidate
	// Problems are stacks whose orphans could not be listed (e.g. a
	// project name used by another directory); they are skipped.
	Problems []string
}

// RemovableBytes sums the sizes of the images to remove. Images can share
// layers, so the space actually freed may be smaller.
func (pl CleanupPlan) RemovableBytes() int64 {
	var n int64
	for _, img := range pl.Images {
		if img.Remove {
			n += img.Size
		}
	}
	return n
}

// CleanupResult is what a cleanup job removed.
type CleanupResult struct {
	RemovedContainers int
	RemovedImages     int
	FreedBytes        int64 // sizes of the removed images (shared layers counted per image)
}

// PlanImages applies the image rules to dangling, unused images.
func PlanImages(images []docker.DanglingImage, opts CleanupOptions, now time.Time) []ImageCandidate {
	out := make([]ImageCandidate, 0, len(images))
	for _, img := range images {
		c := ImageCandidate{DanglingImage: img, Since: img.LastTagTime}
		if c.Since.IsZero() {
			c.Since = img.Created
		}
		switch {
		case slices.Contains(opts.KeepImages, img.ID):
			c.Reason = "replaced by an update or compose change recently"
		case now.Sub(c.Since) < opts.ImageMinAge:
			c.Reason = "pulled or tagged recently"
		default:
			c.Remove = true
		}
		out = append(out, c)
	}
	return out
}

// PlanOrphans applies the orphan rule: stopped orphans are removed,
// running ones are kept.
func PlanOrphans(stack string, orphans []docker.OrphanContainer) []OrphanCandidate {
	out := make([]OrphanCandidate, 0, len(orphans))
	for _, o := range orphans {
		c := OrphanCandidate{Stack: stack, OrphanContainer: o, Remove: !o.Running()}
		if !c.Remove {
			c.Reason = "running"
		}
		out = append(out, c)
	}
	return out
}

// listOrphans lists the orphans of every stack; a stack whose orphans
// cannot be listed becomes a problem and is skipped.
func listOrphans(ctx context.Context, l CleanupLister, list []stacks.Stack) (orphans []OrphanCandidate, problems []string) {
	for _, st := range list {
		found, err := l.StackOrphans(ctx, st.Dir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", st.Name, err))
			continue
		}
		orphans = append(orphans, PlanOrphans(st.Name, found)...)
	}
	return orphans, problems
}

// ListCleanup computes what a cleanup would remove now (read-only).
func ListCleanup(ctx context.Context, l CleanupLister, list []stacks.Stack, opts CleanupOptions, now time.Time) (CleanupPlan, error) {
	var plan CleanupPlan
	plan.Orphans, plan.Problems = listOrphans(ctx, l, list)
	images, err := l.DanglingImages(ctx)
	if err != nil {
		return plan, fmt.Errorf("listing dangling images: %w", err)
	}
	plan.Images = PlanImages(images, opts, now)
	return plan, nil
}

// Cleanup removes the stopped orphan containers of the stacks in list,
// then the dangling images the rules allow (listed after the containers
// are gone, so images only they used count too). Every removal is a single
// docker command without --force; one that fails is logged and the
// cleanup goes on, but the job fails at the end.
func (p *Pipeline) Cleanup(ctx context.Context, c Cleaner, list []stacks.Stack, opts CleanupOptions) (CleanupResult, error) {
	log := p.logger()
	var res CleanupResult
	failed := 0

	p.phase("removing orphan containers")
	orphans, problems := listOrphans(ctx, c, list)
	for _, pr := range problems {
		log.Warn("skipping a stack: listing its containers failed", "problem", pr)
	}
	for _, o := range orphans {
		if !o.Remove {
			log.Info("keeping orphan container", "stack", o.Stack, "container", o.Name, "service", o.Service, "reason", o.Reason)
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := c.RemoveStoppedContainer(ctx, o.ID); err != nil {
			log.Error("removing orphan container failed", "stack", o.Stack, "container", o.Name, "error", err)
			failed++
			continue
		}
		res.RemovedContainers++
		log.Info("removed orphan container", "stack", o.Stack, "container", o.Name, "service", o.Service, "state", o.State)
	}

	p.phase("removing dangling images")
	images, err := c.DanglingImages(ctx)
	if err != nil {
		return res, fmt.Errorf("listing dangling images: %w", err)
	}
	for _, img := range PlanImages(images, opts, p.now()) {
		if !img.Remove {
			log.Info("keeping image", "image", img.ID, "since", img.Since, "reason", img.Reason)
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := c.RemoveImage(ctx, img.ID); err != nil {
			log.Error("removing image failed", "image", img.ID, "error", err)
			failed++
			continue
		}
		res.RemovedImages++
		res.FreedBytes += img.Size
		log.Info("removed image", "image", img.ID, "digests", img.RepoDigests, "since", img.Since, "size", img.Size)
	}

	log.Info("cleanup done", "removed_containers", res.RemovedContainers, "removed_images", res.RemovedImages,
		"freed_bytes", res.FreedBytes, "failed", failed, "skipped_stacks", len(problems))
	if failed > 0 {
		return res, fmt.Errorf("%d removals failed (see the log)", failed)
	}
	return res, nil
}
