package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// Mount is one mount of a container.
type Mount struct {
	Type     string // "bind" or "volume"
	Source   string // host path (bind) or volume name (volume)
	Target   string // path inside the container
	SubPath  string // volume only: subdirectory of the volume to mount
	ReadOnly bool
}

// ContainerSpec is a container to create with "docker run -d". The manager
// builds it from a fixed template; nothing in it comes from user input.
type ContainerSpec struct {
	Name   string
	Image  string
	Labels map[string]string
	Mounts []Mount
	// Env is passed to the container. Only the names appear on the docker
	// command line (-e NAME); the values reach docker through its own
	// process environment, so they never show up in "ps" or logs.
	Env map[string]string
	Cmd []string
}

// ContainerState is the state of a container.
type ContainerState struct {
	Exists   bool
	Status   string // "created", "running", "exited", ...
	ExitCode int
}

// Running reports whether the container has not finished yet.
func (s ContainerState) Running() bool {
	return s.Exists && (s.Status == "created" || s.Status == "running" || s.Status == "restarting")
}

// SelfInfo describes the container the manager runs in.
type SelfInfo struct {
	ImageID string // "sha256:..."
	Mounts  []Mount
}

// dockerEnv are the variables the docker CLI inherits from the manager. The
// manager's own secrets (e.g. STACKVAULT_SECRET_KEY) must not reach it.
var dockerEnv = []string{
	"PATH", "HOME", "TMPDIR",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION",
}

var (
	envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	signalPattern  = regexp.MustCompile(`^SIG[A-Z0-9]+$`)
)

// runArgs builds the "docker run" arguments for spec.
func runArgs(spec ContainerSpec) ([]string, error) {
	if spec.Name == "" || spec.Image == "" || strings.HasPrefix(spec.Image, "-") {
		return nil, errors.New("container name and image are required")
	}
	args := []string{"run", "--detach", "--name", spec.Name}
	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	for _, m := range spec.Mounts {
		v, err := mountArg(m)
		if err != nil {
			return nil, err
		}
		args = append(args, "--mount", v)
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		if !envNamePattern.MatchString(k) || slices.Contains(dockerEnv, k) {
			return nil, fmt.Errorf("invalid container environment variable %q", k)
		}
		args = append(args, "--env", k) // name only, see ContainerSpec.Env
	}
	args = append(args, spec.Image)
	return append(args, spec.Cmd...), nil
}

// mountArg formats a --mount value. Commas and quotes separate --mount
// fields, so values containing them are refused.
func mountArg(m Mount) (string, error) {
	for _, v := range []string{m.Source, m.Target, m.SubPath} {
		if strings.ContainsAny(v, ",\"\n") {
			return "", fmt.Errorf("mount path %q contains a comma, quote or newline", v)
		}
	}
	if m.Source == "" || !strings.HasPrefix(m.Target, "/") {
		return "", fmt.Errorf("invalid mount %+v", m)
	}
	var parts []string
	switch m.Type {
	case "bind":
		if !strings.HasPrefix(m.Source, "/") || m.SubPath != "" {
			return "", fmt.Errorf("invalid bind mount %+v", m)
		}
		parts = []string{"type=bind", "source=" + m.Source, "target=" + m.Target}
	case "volume":
		parts = []string{"type=volume", "source=" + m.Source, "target=" + m.Target}
		if m.SubPath != "" {
			parts = append(parts, "volume-subpath="+m.SubPath)
		}
	default:
		return "", fmt.Errorf("unsupported mount type %q", m.Type)
	}
	if m.ReadOnly {
		parts = append(parts, "readonly")
	}
	return strings.Join(parts, ","), nil
}

// RunContainer creates and starts a container in the background and
// returns its id.
func (c *CLI) RunContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	args, err := runArgs(spec)
	if err != nil {
		return "", err
	}
	env := make([]string, 0, len(dockerEnv)+len(spec.Env))
	for _, k := range dockerEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range spec.Env {
		if strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("environment variable %s contains a NUL byte", k)
		}
		env = append(env, k+"="+v)
	}
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// stderr comes from docker itself; the values are not on the
		// command line, but redact them anyway in case docker echoes one.
		msg := tail(stderr.String())
		for _, v := range spec.Env {
			if len(v) >= 4 {
				msg = strings.ReplaceAll(msg, v, "[REDACTED]")
			}
		}
		return "", fmt.Errorf("docker run %s: %w: %s", spec.Name, err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// InspectContainer returns the state of a container. A missing container
// is not an error (Exists is false).
func (c *CLI) InspectContainer(ctx context.Context, name string) (ContainerState, error) {
	out, err := c.run(ctx, "", "container", "inspect", "--format", "{{json .State}}", "--", name)
	if err != nil {
		if isNoSuchContainer(err) {
			return ContainerState{}, nil
		}
		return ContainerState{}, err
	}
	var st struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return ContainerState{}, fmt.Errorf("parsing container state of %s: %w", name, err)
	}
	return ContainerState{Exists: true, Status: st.Status, ExitCode: st.ExitCode}, nil
}

// RemoveContainer removes a finished container. A missing container is not
// an error.
func (c *CLI) RemoveContainer(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "", "container", "rm", "--", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

// SignalContainer sends a signal (e.g. "SIGTERM") to a running container.
// Unlike "docker stop" it never follows up with SIGKILL.
func (c *CLI) SignalContainer(ctx context.Context, name, signal string) error {
	if !signalPattern.MatchString(signal) {
		return fmt.Errorf("invalid signal %q", signal)
	}
	_, err := c.run(ctx, "", "container", "kill", "--signal", signal, "--", name)
	return err
}

func isNoSuchContainer(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such container")
}

// InspectSelf returns image and mounts of the container with the given id
// (the manager's own container).
func (c *CLI) InspectSelf(ctx context.Context, id string) (SelfInfo, error) {
	out, err := c.run(ctx, "", "container", "inspect", "--format", "{{json .}}", "--", id)
	if err != nil {
		return SelfInfo{}, err
	}
	return parseSelf(out)
}

func parseSelf(out []byte) (SelfInfo, error) {
	var raw struct {
		Image  string `json:"Image"`
		Mounts []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return SelfInfo{}, fmt.Errorf("parsing container inspect: %w", err)
	}
	info := SelfInfo{ImageID: raw.Image}
	for _, m := range raw.Mounts {
		mt := Mount{Type: m.Type, Target: m.Destination, ReadOnly: !m.RW}
		switch m.Type {
		case "bind":
			mt.Source = m.Source
		case "volume":
			mt.Source = m.Name
		default:
			continue // tmpfs, npipe, ...
		}
		info.Mounts = append(info.Mounts, mt)
	}
	return info, nil
}

// containerIDPattern finds a container id in /proc/self/mountinfo, where
// Docker mounts /etc/hostname, /etc/hosts and /etc/resolv.conf from
// /var/lib/docker/containers/<id>/.
var containerIDPattern = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// OwnContainerID returns the id of the container this process runs in, or
// "" if it does not run in a Docker container. mountinfo is the content of
// /proc/self/mountinfo.
func OwnContainerID(mountinfo []byte) string {
	if m := containerIDPattern.FindSubmatch(mountinfo); m != nil {
		return string(m[1])
	}
	return ""
}
