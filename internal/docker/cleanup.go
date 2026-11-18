package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Host cleanup (issue #19): read-only listing of dangling images and
// orphan containers (manager preview and worker), and removal by id
// (worker only). Every command has a fixed argument list; ids are
// validated before they reach the command line.

// DanglingImage is an untagged image that no container (running or
// stopped) uses.
type DanglingImage struct {
	ID          string    // "sha256:<64 hex>"
	Created     time.Time // build time
	LastTagTime time.Time // last pull/tag on this host; zero if docker has none
	Size        int64     // bytes
	RepoDigests []string  // e.g. "nextcloud@sha256:..."
}

// OrphanContainer is a container of a stack's compose project whose
// service is no longer in the compose file.
type OrphanContainer struct {
	ID      string // full id, 64 hex
	Name    string
	Service string
	State   string // "running", "exited", ...
}

// Running reports whether the container is running (or about to), i.e.
// must not be removed by a cleanup.
func (o OrphanContainer) Running() bool {
	return o.State == "running" || o.State == "restarting" || o.State == "paused"
}

var (
	imageIDPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	fullContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidImageID reports whether id is a full image id ("sha256:<64 hex>").
func ValidImageID(id string) bool { return imageIDPattern.MatchString(id) }

// inspectBatch bounds the ids per "inspect" command line.
const inspectBatch = 100

// DanglingImages lists the dangling images that no container uses.
func (c *CLI) DanglingImages(ctx context.Context) ([]DanglingImage, error) {
	out, err := c.run(ctx, "", "image", "ls", "--filter", "dangling=true", "--no-trunc", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	ids := parseLines(out)
	for _, id := range ids {
		if !ValidImageID(id) {
			return nil, fmt.Errorf("unexpected image id %q from docker image ls", id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	used, err := c.usedImages(ctx)
	if err != nil {
		return nil, err
	}
	var unused []string
	for _, id := range ids {
		if !used[id] {
			unused = append(unused, id)
		}
	}
	var images []DanglingImage
	for chunk := range slices.Chunk(unused, inspectBatch) {
		out, err := c.run(ctx, "", append([]string{"image", "inspect", "--"}, chunk...)...)
		if err != nil {
			return nil, err
		}
		list, err := parseDanglingImages(out)
		if err != nil {
			return nil, err
		}
		images = append(images, list...)
	}
	return images, nil
}

// usedImages returns the image ids of all containers, running or not.
func (c *CLI) usedImages(ctx context.Context) (map[string]bool, error) {
	out, err := c.run(ctx, "", "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}
	containers := parseLines(out)
	used := map[string]bool{}
	for chunk := range slices.Chunk(containers, inspectBatch) {
		out, err := c.run(ctx, "", append([]string{"container", "inspect", "--format", "{{.Image}}", "--"}, chunk...)...)
		if err != nil {
			// A container removed between ps and inspect: retry once.
			if !strings.Contains(err.Error(), "No such container") {
				return nil, err
			}
			return c.usedImagesRetry(ctx)
		}
		for _, id := range parseLines(out) {
			used[id] = true
		}
	}
	return used, nil
}

// usedImagesRetry is usedImages after a race with a removed container: one
// container at a time, skipping vanished ones.
func (c *CLI) usedImagesRetry(ctx context.Context) (map[string]bool, error) {
	out, err := c.run(ctx, "", "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, id := range parseLines(out) {
		img, err := c.run(ctx, "", "container", "inspect", "--format", "{{.Image}}", "--", id)
		if err != nil {
			if strings.Contains(err.Error(), "No such container") {
				continue
			}
			return nil, err
		}
		used[strings.TrimSpace(string(img))] = true
	}
	return used, nil
}

// parseDanglingImages parses "docker image inspect" output.
func parseDanglingImages(out []byte) ([]DanglingImage, error) {
	var raw []struct {
		ID          string   `json:"Id"`
		Created     string   `json:"Created"`
		Size        int64    `json:"Size"`
		RepoDigests []string `json:"RepoDigests"`
		Metadata    struct {
			LastTagTime string `json:"LastTagTime"`
		} `json:"Metadata"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing image inspect: %w", err)
	}
	images := make([]DanglingImage, 0, len(raw))
	for _, r := range raw {
		if !ValidImageID(r.ID) {
			return nil, fmt.Errorf("unexpected image id %q from docker image inspect", r.ID)
		}
		img := DanglingImage{ID: r.ID, Size: r.Size, RepoDigests: r.RepoDigests}
		img.Created = parseDockerTime(r.Created)
		img.LastTagTime = parseDockerTime(r.Metadata.LastTagTime)
		images = append(images, img)
	}
	return images, nil
}

// parseDockerTime parses an RFC 3339 time from docker; zero for docker's
// "0001-01-01T00:00:00Z" and for anything unparsable.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t
}

// orphanFormat prints id, name, state and compose service of a container.
const orphanFormat = `{{.ID}}` + "\t" + `{{.Names}}` + "\t" + `{{.State}}` + "\t" + `{{.Label "com.docker.compose.service"}}`

// StackOrphans lists the containers of the compose project in dir whose
// service is no longer in the compose file. Like every stack command it
// first makes sure the project belongs to dir (checkProjectOwner).
func (c *CLI) StackOrphans(ctx context.Context, dir string) ([]OrphanContainer, error) {
	cfg, err := c.projectConfig(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := c.checkProjectContainers(ctx, dir, cfg.Name); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, dir, "ps", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project="+cfg.Name,
		"--format", orphanFormat)
	if err != nil {
		return nil, err
	}
	return parseOrphans(out, cfg.Services)
}

// parseOrphans keeps the containers whose service is not in services.
func parseOrphans(out []byte, services []string) ([]OrphanContainer, error) {
	var orphans []OrphanContainer
	for _, line := range parseLines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected docker ps line %q", line)
		}
		o := OrphanContainer{ID: f[0], Name: f[1], State: f[2], Service: f[3]}
		if !fullContainerID.MatchString(o.ID) {
			return nil, fmt.Errorf("unexpected container id %q from docker ps", o.ID)
		}
		if o.Service == "" || slices.Contains(services, o.Service) {
			continue
		}
		orphans = append(orphans, o)
	}
	return orphans, nil
}

// RemoveImage removes an unused image by id ("docker image rm" without
// --force: docker refuses an image a container uses or that has tags in
// several repositories).
func (c *CLI) RemoveImage(ctx context.Context, id string) error {
	if !ValidImageID(id) {
		return fmt.Errorf("invalid image id %q", id)
	}
	_, err := c.run(ctx, "", "image", "rm", "--", id)
	return err
}

// RemoveStoppedContainer removes a container by full id ("docker container
// rm" without --force: docker refuses a running container). Its volumes
// are kept.
func (c *CLI) RemoveStoppedContainer(ctx context.Context, id string) error {
	if !fullContainerID.MatchString(id) {
		return fmt.Errorf("invalid container id %q", id)
	}
	_, err := c.run(ctx, "", "container", "rm", "--", id)
	return err
}
