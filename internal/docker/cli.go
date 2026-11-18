package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// CLI implements Client by running the docker CLI. All commands are run with
// exec.CommandContext and explicit argument lists, never through a shell.
type CLI struct {
	// Binary is the docker executable; defaults to "docker" (looked up in PATH).
	Binary string
	// PollInterval for WaitHealthy (and for watching a stop); defaults to 2s.
	PollInterval time.Duration
	// Pending, if set, receives the services Stop and WaitHealthy are
	// still waiting for (sorted; empty when done). The worker shows them
	// as the detail of the job's phase.
	Pending func(services []string)
	// Pulling, if set, receives the progress of Pull (about once per
	// second); Pull then runs "compose --progress json pull" and reads its
	// output while it runs.
	Pulling func(PullStatus)
	// Now defaults to time.Now (tests override it).
	Now func() time.Time
}

func (c *CLI) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *CLI) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return 2 * time.Second
	}
	return c.PollInterval
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

	byService := mergeServices(names, containers)

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

// mergeServices combines the services of the compose file (names, from
// "compose config --services") with the project's containers ("compose ps
// --all"). A container whose service is not in the file (an orphan: the
// service was removed from the file) is listed with Defined false.
func mergeServices(names []string, containers []psEntry) map[string]ServiceState {
	byService := map[string]ServiceState{}
	for _, n := range names {
		byService[n] = ServiceState{Name: n, Defined: true}
	}
	for _, ct := range containers {
		cur, seen := byService[ct.Service]
		// With scaled services keep the first running container.
		if seen && cur.ContainerID != "" && (cur.Running() || ct.State != "running") {
			continue
		}
		byService[ct.Service] = ServiceState{
			Name:        ct.Service,
			Defined:     slices.Contains(names, ct.Service),
			State:       ct.State,
			Health:      ct.Health,
			ContainerID: ct.ID,
			ImageRef:    ct.Image,
		}
	}
	return byService
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

// ErrLegacyCompose means that containers of the stack were created by the
// Python docker-compose v1. They lack the labels StackVault relies on (see
// checkProjectOwner), so it refuses to manage the stack until it is
// recreated with "docker compose up".
var ErrLegacyCompose = errors.New("created by docker-compose v1")

// projectConfig is the part of "compose config --format json" StackVault
// needs.
type projectConfig struct {
	Name     string
	Services []string // sorted
}

// projectConfig reads the compose project of dir.
func (c *CLI) projectConfig(ctx context.Context, dir string) (projectConfig, error) {
	out, err := c.compose(ctx, dir, "config", "--format", "json")
	if err != nil {
		return projectConfig{}, err
	}
	return parseProjectConfig(out)
}

// checkProjectOwner refuses a stack whose compose project name is also used
// by containers created from another directory. Compose identifies a
// project only by its name (the directory name, or "name:" in compose.yml);
// a stack of the same name elsewhere, or one started with "-p", would
// otherwise make stop/start/up act on someone else's containers.
func (c *CLI) checkProjectOwner(ctx context.Context, dir string) error {
	cfg, err := c.projectConfig(ctx, dir)
	if err != nil {
		return err
	}
	return c.checkProjectContainers(ctx, dir, cfg.Name)
}

// containerLabelsFormat prints name, working directory and compose version
// of each container (see classifyContainers).
const containerLabelsFormat = `{{.Names}}` + "\t" + `{{.Label "com.docker.compose.project.working_dir"}}` +
	"\t" + `{{.Label "com.docker.compose.version"}}`

// checkProjectContainers is checkProjectOwner for a known project name. It
// returns an error wrapping ErrLegacyCompose if docker-compose v1 created
// containers of the stack: under the project name itself or under the
// name older v1 versions derived from it (without "-" and "_"), which a
// directory like "my-app" gives.
func (c *CLI) checkProjectContainers(ctx context.Context, dir, project string) error {
	names := []string{project}
	if v1 := legacyProjectName(project); v1 != project {
		names = append(names, v1)
	}
	for i, name := range names {
		out, err := c.run(ctx, dir, "ps", "--all", "--filter", "label=com.docker.compose.project="+name,
			"--format", containerLabelsFormat)
		if err != nil {
			return err
		}
		foreign, legacy := classifyContainers(out, dir)
		if len(legacy) > 0 {
			return fmt.Errorf("%w: containers %s were created by the old docker-compose v1 and cannot be "+
				"managed; recreate them once with \"docker compose up -d\" in the stack directory",
				ErrLegacyCompose, strings.Join(legacy, ", "))
		}
		if i == 0 && len(foreign) > 0 {
			return fmt.Errorf("compose project %q is also used by containers from another directory (%s); "+
				"refusing to touch it. Give the stacks different names, or start this stack from its own "+
				"directory without -p", project, strings.Join(foreign, ", "))
		}
	}
	return nil
}

// legacyProjectName is the project name docker-compose v1 before 1.21
// derived from name: only lower-case letters and digits.
func legacyProjectName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(name))
}

func parseProjectConfig(out []byte) (projectConfig, error) {
	var cfg struct {
		Name     string                     `json:"name"`
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return projectConfig{}, fmt.Errorf("parsing compose config: %w", err)
	}
	if cfg.Name == "" {
		return projectConfig{}, errors.New("compose config has no project name")
	}
	return projectConfig{Name: cfg.Name, Services: slices.Sorted(maps.Keys(cfg.Services))}, nil
}

// classifyContainers sorts the containers in "docker ps" output
// (containerLabelsFormat lines) that were not created from dir into
// foreign ("name (working dir)") and legacy (name; created by
// docker-compose v1, whose containers have no working directory label).
func classifyContainers(out []byte, dir string) (foreign, legacy []string) {
	for _, line := range parseLines(out) {
		parts := strings.SplitN(line, "\t", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		name, wd, version := parts[0], parts[1], parts[2]
		switch {
		case wd != "" && filepath.Clean(wd) == filepath.Clean(dir):
		case wd == "" && strings.HasPrefix(version, "1."):
			legacy = append(legacy, name)
		default:
			foreign = append(foreign, fmt.Sprintf("%s (%s)", name, wd))
		}
	}
	return foreign, legacy
}

// Stop implements Client.
//
// With Pending set, a goroutine watches "compose ps" while compose stops
// the containers (each may take up to its stop timeout) and reports the
// services that still run.
func (c *CLI) Stop(ctx context.Context, dir string) error {
	if c.Pending == nil {
		_, err := c.compose(ctx, dir, "stop")
		return err
	}
	done := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		c.watchRunning(ctx, dir, done)
	}()
	_, err := c.compose(ctx, dir, "stop")
	close(done)
	<-watched // no report after Stop has returned
	c.Pending(nil)
	return err
}

// watchRunning reports the running services of the project in dir every
// poll interval until done is closed. Errors only skip a report.
func (c *CLI) watchRunning(ctx context.Context, dir string, done <-chan struct{}) {
	ticker := time.NewTicker(c.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		out, err := c.compose(ctx, dir, "ps", "--format", "json")
		if err != nil {
			continue
		}
		entries, err := parsePS(out)
		if err != nil {
			continue
		}
		var running []string
		for _, e := range entries {
			if e.State == "running" && !slices.Contains(running, e.Service) {
				running = append(running, e.Service)
			}
		}
		sort.Strings(running)
		select {
		case <-done: // stop finished meanwhile: this list is outdated
			return
		default:
			c.Pending(running)
		}
	}
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

// UpAll implements Client.
func (c *CLI) UpAll(ctx context.Context, dir string) error {
	_, err := c.compose(ctx, dir, "up", "--detach")
	return err
}

// Down implements Client. Never with --volumes: named volumes hold data.
func (c *CLI) Down(ctx context.Context, dir string) error {
	_, err := c.compose(ctx, dir, "down")
	return err
}

// Pull implements Client.
func (c *CLI) Pull(ctx context.Context, dir string, services []string) error {
	if len(services) == 0 {
		return nil // "compose pull" without services would pull everything
	}
	if c.Pulling != nil {
		return c.pullWithProgress(ctx, dir, services)
	}
	_, err := c.compose(ctx, dir, append([]string{"pull", "--ignore-buildable", "--quiet", "--"}, services...)...)
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

// ImageExists implements Client.
func (c *CLI) ImageExists(ctx context.Context, ref string) (bool, error) {
	if strings.HasPrefix(ref, "-") { // never let a reference become a flag
		return false, fmt.Errorf("invalid image reference %q", ref)
	}
	if _, err := c.run(ctx, "", "image", "inspect", "--format", "{{.Id}}", "--", ref); err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// WaitHealthy implements Client.
func (c *CLI) WaitHealthy(ctx context.Context, dir string, services []string) error {
	ticker := time.NewTicker(c.pollInterval())
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
		if c.Pending != nil {
			c.Pending(pending)
		}
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
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	Project  string `json:"Project"`
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	Status   string `json:"Status"`
	Image    string `json:"Image"`
	ExitCode int    `json:"ExitCode"`
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
