package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// VersionLabel is the OCI image label with the software version. Many
// images set it (linuxserver.io, Bitnami, most images built with
// docker/metadata-action); for others only date and digest can be shown.
const VersionLabel = "org.opencontainers.image.version"

// configuredService is a service as the compose file defines it.
type configuredService struct {
	Image string // image reference, "" if none
	Build bool   // built locally (build:)
}

// configuredServices returns the services of the compose file in dir with
// their image references ("docker compose config --format json").
func (c *CLI) configuredServices(ctx context.Context, dir string) (map[string]configuredService, error) {
	out, err := c.compose(ctx, dir, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseConfiguredServices(out)
}

func parseConfiguredServices(out []byte) (map[string]configuredService, error) {
	var cfg struct {
		Services map[string]struct {
			Image string          `json:"image"`
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("parsing compose config: %w", err)
	}
	services := make(map[string]configuredService, len(cfg.Services))
	for name, s := range cfg.Services {
		services[name] = configuredService{Image: s.Image, Build: len(s.Build) > 0 && string(s.Build) != "null"}
	}
	return services, nil
}

// validImageRef rejects references that could be read as a flag or contain
// control characters; anything else is left to docker to judge.
func validImageRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && len(ref) <= 512 &&
		!strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// ImageIDs implements Client.
func (c *CLI) ImageIDs(ctx context.Context, dir string, services []string) (map[string]string, error) {
	configured, err := c.configuredServices(ctx, dir)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(services))
	byRef := map[string]string{}
	for _, name := range services {
		ref := configured[name].Image
		if !validImageRef(ref) {
			ids[name] = ""
			continue
		}
		id, seen := byRef[ref]
		if !seen {
			out, err := c.run(ctx, dir, "image", "inspect", "--format", "{{.Id}}", "--", ref)
			switch {
			case err == nil:
				id = strings.TrimSpace(string(out))
			case strings.Contains(err.Error(), "No such image"):
				id = ""
			default:
				return nil, err
			}
			byRef[ref] = id
		}
		ids[name] = id
	}
	return ids, nil
}

// UpdateStatus is the result of an update check for one service.
type UpdateStatus string

// Update check results.
const (
	UpToDate        UpdateStatus = "up-to-date"
	UpdateAvailable UpdateStatus = "available"
	UpdateUnknown   UpdateStatus = "unknown"     // the registry could not be asked, or nothing to compare
	NotChecked      UpdateStatus = "not-checked" // built locally, pinned by digest, no image
)

// ImageVersion describes one image version for display.
type ImageVersion struct {
	Digest  string    // registry digest "sha256:...", "" if unknown
	Version string    // VersionLabel, "" if not set
	Created time.Time // zero if unknown
}

// ImageCheck is the update check result of one service.
type ImageCheck struct {
	Service   string
	Image     string // image reference in the compose file
	Running   bool   // only running services are recreated by an update
	Current   ImageVersion
	Available ImageVersion
	Status    UpdateStatus
	Note      string // explanation for NotChecked/Unknown, or a remark
}

// remoteImage is what the registry reports for a reference.
type remoteImage struct {
	Digest string
	// Configs by platform ("linux/amd64", "linux/arm/v7"); a
	// single-platform image has one entry with key "".
	Configs map[string]imageConfig
}

type imageConfig struct {
	Created      time.Time `json:"created"`
	OS           string    `json:"os"`
	Architecture string    `json:"architecture"`
	Variant      string    `json:"variant"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// localImage is the part of "docker image inspect" the check needs.
type localImage struct {
	RepoDigests  []string  `json:"RepoDigests"`
	Created      time.Time `json:"Created"`
	OS           string    `json:"Os"`
	Architecture string    `json:"Architecture"`
	Variant      string    `json:"Variant"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (l localImage) platform() string {
	if l.OS == "" || l.Architecture == "" {
		return ""
	}
	p := l.OS + "/" + l.Architecture
	if l.Variant != "" {
		p += "/" + l.Variant
	}
	return p
}

// maxParallelChecks bounds the registry requests of one update check.
const maxParallelChecks = 4

// CheckUpdates compares the images of the stack's running containers with
// what the registry currently has for the tags in the compose file. It
// only reads: "docker compose config/ps", "docker image inspect" and
// "docker buildx imagetools inspect", which asks the registry without
// pulling anything. Registry errors are reported per service, not as an
// error of the whole check.
func (c *CLI) CheckUpdates(ctx context.Context, dir string) ([]ImageCheck, error) {
	services, err := c.Services(ctx, dir) // includes the project owner check
	if err != nil {
		return nil, err
	}
	configured, err := c.configuredServices(ctx, dir)
	if err != nil {
		return nil, err
	}

	locals := map[string]localImage{}
	for _, s := range services {
		if s.ImageID == "" {
			continue
		}
		if _, ok := locals[s.ImageID]; ok {
			continue
		}
		img, err := c.localImage(ctx, dir, s.ImageID)
		if err != nil {
			return nil, err
		}
		locals[s.ImageID] = img
	}

	// Ask the registry once per reference, a few at a time.
	type result struct {
		img remoteImage
		err error
	}
	remotes := map[string]*result{}
	for _, s := range services {
		if ref := configured[s.Name].Image; checkable(ref) {
			remotes[ref] = &result{}
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelChecks)
	for ref, res := range remotes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res.img, res.err = c.remoteImage(ctx, ref)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	checks := make([]ImageCheck, 0, len(services))
	for _, s := range services {
		cfg := configured[s.Name]
		var remote remoteImage
		var remoteErr error
		if res, ok := remotes[cfg.Image]; ok {
			remote, remoteErr = res.img, res.err
		}
		local, hasLocal := locals[s.ImageID]
		checks = append(checks, evaluateUpdate(s, cfg, local, hasLocal, remote, remoteErr))
	}
	return checks, nil
}

// checkable reports whether the registry can be asked about ref: a tag
// reference, not one pinned by digest.
func checkable(ref string) bool {
	return validImageRef(ref) && !strings.Contains(ref, "@")
}

// evaluateUpdate decides the status of one service from its state, its
// compose definition, its running image and the registry's answer.
func evaluateUpdate(s ServiceState, cfg configuredService, local localImage, hasLocal bool,
	remote remoteImage, remoteErr error) ImageCheck {
	check := ImageCheck{Service: s.Name, Image: cfg.Image, Running: s.Running()}
	if hasLocal {
		check.Current = ImageVersion{Version: local.Config.Labels[VersionLabel], Created: local.Created}
		check.Current.Digest = matchingDigest(local.RepoDigests, remote.Digest)
	}
	switch {
	case cfg.Image == "" && cfg.Build:
		check.Status, check.Note = NotChecked, "built locally (build:), not pulled by an update"
		return check
	case cfg.Image == "":
		check.Status, check.Note = NotChecked, "no image"
		return check
	case strings.Contains(cfg.Image, "@"):
		check.Status, check.Note = NotChecked, "pinned by digest"
		return check
	case !checkable(cfg.Image):
		check.Status, check.Note = NotChecked, "unusual image reference"
		return check
	case remoteErr != nil:
		check.Status, check.Note = UpdateUnknown, "registry: "+firstLine(remoteErr.Error())
		return check
	}

	check.Available.Digest = remote.Digest
	platform := local.platform()
	if platform == "" {
		platform = "linux/" + runtime.GOARCH // no container: assume the host's platform
	}
	if rc, ok := remote.config(platform); ok {
		check.Available.Version, check.Available.Created = rc.Config.Labels[VersionLabel], rc.Created
	}

	switch {
	case s.ContainerID == "":
		check.Status, check.Note = UpdateUnknown, "no container yet"
	case s.ImageRef != "" && s.ImageRef != cfg.Image && !pinnedByRestore(s.ImageRef, cfg.Image):
		check.Status, check.Note = UpdateAvailable, "the compose file names another image than the container runs ("+s.ImageRef+")"
	case !hasLocal || len(local.RepoDigests) == 0:
		check.Status, check.Note = UpdateUnknown, "the running image has no registry digest (built or loaded locally?)"
	case check.Current.Digest != "":
		check.Status = UpToDate
	default:
		check.Status = UpdateAvailable
		check.Current.Digest = digestOf(local.RepoDigests[0])
	}
	if pinnedByRestore(s.ImageRef, cfg.Image) {
		// Started by a restore or rollback with pinned image versions.
		check.Note = strings.TrimPrefix(check.Note+"; runs a version pinned by digest by a restore or rollback", "; ")
	}
	if !check.Running && s.ContainerID != "" {
		check.Note = strings.TrimPrefix(check.Note+"; not running: an update does not recreate it", "; ")
	}
	return check
}

// config returns the image config for platform: the entry of a
// multi-platform image, or the only config of a single-platform one.
func (r remoteImage) config(platform string) (imageConfig, bool) {
	if c, ok := r.Configs[platform]; ok {
		return c, true
	}
	if c, ok := r.Configs[""]; ok {
		return c, true
	}
	// "linux/arm64" is often listed as "linux/arm64/v8".
	for p, c := range r.Configs {
		if strings.HasPrefix(p, platform+"/") {
			return c, true
		}
	}
	return imageConfig{}, false
}

// matchingDigest returns want if one of repoDigests ("name@sha256:...")
// has that digest, else "".
func matchingDigest(repoDigests []string, want string) string {
	if want == "" {
		return ""
	}
	for _, rd := range repoDigests {
		if digestOf(rd) == want {
			return want
		}
	}
	return ""
}

func digestOf(repoDigest string) string {
	if _, d, ok := strings.Cut(repoDigest, "@"); ok {
		return d
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// localImage inspects a local image by id. A missing image is not an
// error: it has no digests and no labels.
func (c *CLI) localImage(ctx context.Context, dir, imageID string) (localImage, error) {
	out, err := c.run(ctx, dir, "image", "inspect", "--", imageID)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return localImage{}, nil
		}
		return localImage{}, err
	}
	var imgs []localImage
	if err := json.Unmarshal(out, &imgs); err != nil {
		return localImage{}, fmt.Errorf("parsing image inspect: %w", err)
	}
	if len(imgs) != 1 {
		return localImage{}, fmt.Errorf("image inspect returned %d entries", len(imgs))
	}
	return imgs[0], nil
}

// remoteImage asks the registry about ref without pulling it.
func (c *CLI) remoteImage(ctx context.Context, ref string) (remoteImage, error) {
	if !checkable(ref) {
		return remoteImage{}, fmt.Errorf("invalid image reference %q", ref)
	}
	out, err := c.run(ctx, "", "buildx", "imagetools", "inspect", "--format", "{{json .}}", "--", ref)
	if err != nil {
		return remoteImage{}, err
	}
	return parseImagetools(out)
}

// parseImagetools parses "docker buildx imagetools inspect --format
// '{{json .}}'": "manifest" is the index or manifest with its digest;
// "image" is a map platform -> config for a multi-platform image, or the
// config itself for a single-platform one.
func parseImagetools(out []byte) (remoteImage, error) {
	var raw struct {
		Manifest struct {
			Digest string `json:"digest"`
		} `json:"manifest"`
		Image json.RawMessage `json:"image"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return remoteImage{}, fmt.Errorf("parsing imagetools output: %w", err)
	}
	if !strings.HasPrefix(raw.Manifest.Digest, "sha256:") {
		return remoteImage{}, errors.New("the registry reported no digest")
	}
	img := remoteImage{Digest: raw.Manifest.Digest, Configs: map[string]imageConfig{}}
	if len(raw.Image) == 0 || string(raw.Image) == "null" {
		return img, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw.Image, &probe); err != nil {
		return remoteImage{}, fmt.Errorf("parsing image config: %w", err)
	}
	if _, single := probe["architecture"]; single {
		var cfg imageConfig
		if err := json.Unmarshal(raw.Image, &cfg); err != nil {
			return remoteImage{}, fmt.Errorf("parsing image config: %w", err)
		}
		img.Configs[""] = cfg
		return img, nil
	}
	for platform, msg := range probe {
		var cfg imageConfig
		if err := json.Unmarshal(msg, &cfg); err != nil {
			continue // e.g. attestation entries
		}
		img.Configs[platform] = cfg
	}
	return img, nil
}

// pinnedByRestore reports whether a container runs running (a digest
// reference) instead of the configured tag of the same repository, as a
// restore or rollback with pinned image versions starts it.
func pinnedByRestore(running, configured string) bool {
	return running != configured && strings.Contains(running, "@") && repository(running) == repository(configured)
}

// repository returns ref without tag and digest: "nextcloud:29" and
// "nextcloud@sha256:..." both give "nextcloud"; "host:5000/app:1" gives
// "host:5000/app".
func repository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	return ref
}
