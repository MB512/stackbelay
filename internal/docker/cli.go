package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CLI implements Client by running the docker CLI. All commands are run with
// exec.CommandContext and explicit argument lists, never through a shell.
type CLI struct {
	// Binary is the docker executable; defaults to "docker" (looked up in PATH).
	Binary string
	// PollInterval for WaitHealthy; defaults to 2s.
	PollInterval time.Duration
}

// Compile-time check that *CLI satisfies Client (like an explicit
// "class CLI : IClient" in C#; Go interfaces are otherwise implicit).
var _ Client = (*CLI)(nil)

func (c *CLI) binary() string {
	if c.Binary == "" {
		return "docker"
	}
	return c.Binary
}

// run executes docker with args in dir and returns stdout. On failure the
// error contains (a bounded part of) stderr.
func (c *CLI) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 4)], " "), err, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (c *CLI) compose(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"compose", "--project-directory", dir}, args...)
	return c.run(ctx, dir, full...)
}

// Services implements Client. It first makes sure that the compose project
// really belongs to dir (see checkProjectOwner).
func (c *CLI) Services(ctx context.Context, dir string) ([]ServiceState, error) {
	if err := c.checkProjectOwner(ctx, dir); err != nil {
		return nil, err
	}
	out, err := c.compose(ctx, dir, "config", "--services")
	if err != nil {
		return nil, err
	}
	names := parseLines(out)

	out, err = c.compose(ctx, dir, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	containers, err := parsePS(out)
	if err != nil {
		return nil, err
	}

	byService := map[string]ServiceState{}
	for _, n := range names {
		byService[n] = ServiceState{Name: n}
	}
	for _, ct := range containers {
		cur, seen := byService[ct.Service]
		// With scaled services keep the first running container.
		if seen && cur.ContainerID != "" && (cur.Running() || ct.State != "running") {
			continue
		}
		byService[ct.Service] = ServiceState{
			Name:        ct.Service,
			State:       ct.State,
			Health:      ct.Health,
			ContainerID: ct.ID,
			ImageRef:    ct.Image,
		}
	}

	// Resolve image IDs and repo digests for every container.
	imageCache := map[string]imageInfo{}
	for name, s := range byService {
		if s.ContainerID == "" {
			continue
		}
		id, err := c.containerImageID(ctx, dir, s.ContainerID)
		if err != nil {
			return nil, err
		}
		s.ImageID = id
		info, ok := imageCache[id]
		if !ok {
			info, err = c.imageInfo(ctx, dir, id)
			if err != nil {
				return nil, err
			}
			imageCache[id] = info
		}
		s.RepoDigests = info.RepoDigests
		byService[name] = s
	}

	result := make([]ServiceState, 0, len(byService))
	for _, s := range byService {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *CLI) containerImageID(ctx context.Context, dir, containerID string) (string, error) {
	out, err := c.run(ctx, dir, "container", "inspect", "--format", "{{.Image}}", containerID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type imageInfo struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

// imageInfo returns the image's repo digests. A missing image (removed after
// the container was created) is not an error: it simply has no digests.
func (c *CLI) imageInfo(ctx context.Context, dir, imageID string) (imageInfo, error) {
	out, err := c.run(ctx, dir, "image", "inspect", imageID)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return imageInfo{ID: imageID}, nil
		}
		return imageInfo{}, err
	}
	var infos []imageInfo
	if err := json.Unmarshal(out, &infos); err != nil {
		return imageInfo{}, fmt.Errorf("parsing image inspect: %w", err)
	}
	if len(infos) != 1 {
		return imageInfo{}, fmt.Errorf("image inspect returned %d entries", len(infos))
	}
	return infos[0], nil
}

// checkProjectOwner refuses a stack whose compose project name is also used
// by containers created from another directory. Compose identifies a
// project only by its name (the directory name, or "name:" in compose.yml);
// a stack of the same name elsewhere, or one started with "-p", would
// otherwise make stop/start/up act on someone else's containers.
func (c *CLI) checkProjectOwner(ctx context.Context, dir string) error {
	out, err := c.compose(ctx, dir, "config", "--format", "json")
	if err != nil {
		return err
	}
	project, err := parseProjectName(out)
	if err != nil {
		return err
	}
	out, err = c.run(ctx, dir, "ps", "--all", "--filter", "label=com.docker.compose.project="+project,
		"--format", `{{.Names}}`+"\t"+`{{.Label "com.docker.compose.project.working_dir"}}`)
	if err != nil {
		return err
	}
	if foreign := foreignContainers(out, dir); len(foreign) > 0 {
		return fmt.Errorf("compose project %q is also used by containers from another directory (%s); "+
			"refusing to touch it. Give the stacks different names, or start this stack from its own "+
			"directory without -p", project, strings.Join(foreign, ", "))
	}
	return nil
}

func parseProjectName(out []byte) (string, error) {
	var cfg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return "", fmt.Errorf("parsing compose config: %w", err)
	}
	if cfg.Name == "" {
		return "", errors.New("compose config has no project name")
	}
	return cfg.Name, nil
}

// foreignContainers returns "name (working dir)" for every container in
// "docker ps" output (name<TAB>working_dir lines) that was not created from
// dir.
func foreignContainers(out []byte, dir string) []string {
	var foreign []string
	for _, line := range parseLines(out) {
		name, wd, _ := strings.Cut(line, "\t")
		if filepath.Clean(wd) != filepath.Clean(dir) {
			foreign = append(foreign, fmt.Sprintf("%s (%s)", name, wd))
		}
	}
	return foreign
}

// Stop implements Client.
func (c *CLI) Stop(ctx context.Context, dir string) error {
	_, err := c.compose(ctx, dir, "stop")
	return err
}

// Start implements Client.
func (c *CLI) Start(ctx context.Context, dir string, services []string) error {
	if len(services) == 0 {
		return nil // "compose start" without services would start everything
	}
	_, err := c.compose(ctx, dir, append([]string{"start", "--"}, services...)...)
	return err
}

// Up implements Client.
func (c *CLI) Up(ctx context.Context, dir string, composeFiles, services []string) error {
	var args []string
	for _, f := range composeFiles {
		args = append(args, "--file", f)
	}
	args = append(args, "up", "--detach", "--force-recreate", "--")
	_, err := c.compose(ctx, dir, append(args, services...)...)
	return err
}

// EnsureImage implements Client.
func (c *CLI) EnsureImage(ctx context.Context, ref string) (bool, error) {
	if strings.HasPrefix(ref, "-") { // never let a reference become a flag
		return false, fmt.Errorf("invalid image reference %q", ref)
	}
	// "image inspect" fails for missing images; any failure is answered by
	// pulling, which reports the real problem if there is one.
	if _, err := c.run(ctx, "", "image", "inspect", "--format", "{{.Id}}", "--", ref); err == nil {
		return false, nil
	}
	if _, err := c.run(ctx, "", "pull", "--quiet", "--", ref); err != nil {
		return false, err
	}
	return true, nil
}

// WaitHealthy implements Client.
func (c *CLI) WaitHealthy(ctx context.Context, dir string, services []string) error {
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		out, err := c.compose(ctx, dir, "ps", "--all", "--format", "json")
		if err != nil {
			return err
		}
		containers, err := parsePS(out)
		if err != nil {
			return err
		}
		pending, err := healthStatus(containers, services)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("services not healthy: %s: %w", strings.Join(pending, ", "), ctx.Err())
		case <-ticker.C:
		}
	}
}

// healthStatus returns the services that are not yet ready. It fails fast
// for services that are unhealthy or have exited.
func healthStatus(containers []psEntry, services []string) (pending []string, err error) {
	byService := map[string][]psEntry{}
	for _, c := range containers {
		byService[c.Service] = append(byService[c.Service], c)
	}
	var failed []string
	for _, name := range services {
		cs := byService[name]
		ready, broken := len(cs) > 0, false
		for _, c := range cs {
			switch {
			case c.State == "exited" || c.State == "dead":
				failed = append(failed, fmt.Sprintf("%s (%s)", name, c.State))
				broken = true
			case c.Health == "unhealthy":
				failed = append(failed, name+" (unhealthy)")
				broken = true
			case c.State != "running" || (c.Health != "" && c.Health != "healthy"):
				ready = false
			}
		}
		if !ready && !broken {
			pending = append(pending, name)
		}
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("services failed: %s", strings.Join(failed, ", "))
	}
	return pending, nil
}

// psEntry is one container from "docker compose ps --format json".
type psEntry struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
}

// parsePS accepts both output styles of "compose ps --format json": a JSON
// array (Compose < 2.21) and one JSON object per line (newer versions).
func parsePS(out []byte) ([]psEntry, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] == '[' {
		var entries []psEntry
		if err := json.Unmarshal(out, &entries); err != nil {
			return nil, fmt.Errorf("parsing compose ps: %w", err)
		}
		return entries, nil
	}
	var entries []psEntry
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var e psEntry
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing compose ps: %w", err)
		}
		entries = append(entries, e)
	}
}

func parseLines(out []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// tail returns the last part of s, for error messages.
func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 2000
	if len(s) > max {
		return "..." + s[len(s)-max:]
	}
	return s
}
