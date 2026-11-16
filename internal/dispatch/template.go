// Package dispatch runs queued jobs: it starts one worker container per job
// (from a fixed template), watches it, and records the result. The queue
// itself, including the per-stack lock, lives in the database
// (store.ClaimNextJob).
package dispatch

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
)

// Fixed parts of the worker container template.
const (
	// WorkerJobDir is where the job directory is mounted in the worker.
	WorkerJobDir = "/job"
	// workerCacheDir holds the restic cache (a named volume shared by all
	// workers, so repeated backups do not download the index each time).
	workerCacheDir = "/cache"
	// CacheVolume is the name of the restic cache volume.
	CacheVolume = "stackvault-restic-cache"
	// dockerSocket is the Docker socket path, on the host and in containers.
	dockerSocket = "/var/run/docker.sock"
	// WorkerBinary is the worker executable in the image.
	WorkerBinary = "/app/worker"
	// LabelWorker marks worker containers; LabelJob holds the job id.
	LabelWorker = "stackvault.worker"
	LabelJob    = "stackvault.job"
)

// ContainerName returns the name of a job's worker container.
func ContainerName(jobID string) string { return "stackvault-job-" + strings.ToLower(jobID) }

// Template describes how worker containers are built. Nothing in it comes
// from user input; per job only the job id and the repository change.
type Template struct {
	Image      string
	StacksRoot string // mounted at the same path as on the host
	// Jobs is where the manager's jobs directory lives on the host: a bind
	// mount (Source = host path of the jobs directory) or a volume
	// (Source = volume name, SubPath = path of the jobs directory in it).
	// The job's own subdirectory is appended per job.
	Jobs docker.Mount
	// Socket is the host path of the Docker socket.
	Socket   string
	Hostname string // STACKVAULT_HOSTNAME for restic --host
	LogLevel string // STACKVAULT_LOG_LEVEL, optional
}

// Spec returns the container for one job. Credentials are passed as
// environment values (by name on the docker command line).
func (t Template) Spec(jobID string, repo restic.Repo) (docker.ContainerSpec, error) {
	jobMount := docker.Mount{Type: t.Jobs.Type, Source: t.Jobs.Source, Target: WorkerJobDir}
	switch t.Jobs.Type {
	case "bind":
		jobMount.Source = path.Join(t.Jobs.Source, jobID)
	case "volume":
		jobMount.SubPath = path.Join(t.Jobs.SubPath, jobID)
	default:
		return docker.ContainerSpec{}, fmt.Errorf("unsupported jobs mount type %q", t.Jobs.Type)
	}
	env := map[string]string{
		"STACKS_ROOT":         t.StacksRoot,
		"STACKVAULT_HOSTNAME": t.Hostname,
		"RESTIC_CACHE_DIR":    workerCacheDir,
	}
	if repo.Location != "" { // empty: an update without backup (no repository)
		env["RESTIC_REPOSITORY"] = repo.Location
	}
	if t.LogLevel != "" {
		env["STACKVAULT_LOG_LEVEL"] = t.LogLevel
	}
	for _, kv := range repo.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !restic.ValidEnvKey(k) {
			return docker.ContainerSpec{}, fmt.Errorf("repository variable %q is not allowed", k)
		}
		env[k] = v
	}
	return docker.ContainerSpec{
		Name:   ContainerName(jobID),
		Image:  t.Image,
		Labels: map[string]string{LabelWorker: "true", LabelJob: jobID},
		Mounts: []docker.Mount{
			{Type: "bind", Source: t.StacksRoot, Target: t.StacksRoot},
			{Type: "bind", Source: t.Socket, Target: dockerSocket},
			jobMount,
			{Type: "volume", Source: CacheVolume, Target: workerCacheDir},
		},
		Env: env,
		Cmd: []string{WorkerBinary, "run", WorkerJobDir},
		// The image's HEALTHCHECK is the manager's (GET /healthz).
		NoHealthcheck: true,
	}, nil
}

// JobsDirInStack returns the jobs directory relative to the given stack's
// directory, if it lies inside it (the manager's own stack, whose data
// directory holds the job files), else "". A restore of that stack must
// carry the job files over (jobs.Spec.CarryOverJobs).
func (t Template) JobsDirInStack(stack string) string {
	if t.Jobs.Type != "bind" {
		return "" // a volume is not part of any stack directory
	}
	rel, err := filepath.Rel(path.Join(t.StacksRoot, stack), t.Jobs.Source)
	if err != nil || !jobs.ValidCarryOver(rel) {
		return ""
	}
	return rel
}

// TemplateConfig is what NewTemplate needs from the manager configuration.
type TemplateConfig struct {
	WorkerImage string // empty: the manager's own image
	StacksRoot  string
	JobsDir     string // the jobs directory as the manager sees it
	Hostname    string
	LogLevel    string
}

// NewTemplate builds the worker template. self describes the manager's own
// container (nil if the manager does not run in a container, e.g. during
// development; then paths are used as they are).
func NewTemplate(cfg TemplateConfig, self *docker.SelfInfo) (Template, error) {
	t := Template{
		Image: cfg.WorkerImage, StacksRoot: cfg.StacksRoot, Socket: dockerSocket,
		Hostname: cfg.Hostname, LogLevel: cfg.LogLevel,
		Jobs: docker.Mount{Type: "bind", Source: cfg.JobsDir},
	}
	if self == nil {
		if t.Image == "" {
			return Template{}, errors.New("STACKVAULT_WORKER_IMAGE is required when the manager does not run in a container")
		}
		return t, nil
	}
	if t.Image == "" {
		t.Image = self.ImageID
	}

	// STACKS_ROOT must be mounted at the same path as on the host, so
	// relative paths in compose files resolve identically.
	m, rel, ok := mountFor(self.Mounts, cfg.StacksRoot)
	if !ok || m.Type != "bind" || path.Join(m.Source, rel) != cfg.StacksRoot {
		return Template{}, fmt.Errorf("STACKS_ROOT %s must be bind-mounted from the same path on the host "+
			"(e.g. %s:%s)", cfg.StacksRoot, cfg.StacksRoot, cfg.StacksRoot)
	}
	if m, _, ok := mountFor(self.Mounts, dockerSocket); ok && m.Type == "bind" {
		t.Socket = m.Source
	}
	m, rel, ok = mountFor(self.Mounts, cfg.JobsDir)
	if !ok {
		return Template{}, fmt.Errorf("the data directory (%s) must be a mounted volume or host directory, "+
			"so that worker containers can reach the job files", cfg.JobsDir)
	}
	switch m.Type {
	case "bind":
		t.Jobs = docker.Mount{Type: "bind", Source: path.Join(m.Source, rel)}
	case "volume":
		t.Jobs = docker.Mount{Type: "volume", Source: m.Source, SubPath: path.Join(m.SubPath, rel)}
	}
	return t, nil
}

// mountFor finds the mount whose target contains p (the longest match) and
// returns p relative to the mount target.
func mountFor(mounts []docker.Mount, p string) (docker.Mount, string, bool) {
	var best docker.Mount
	var bestRel string
	found := false
	for _, m := range mounts {
		rel, err := filepath.Rel(m.Target, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if !found || len(m.Target) > len(best.Target) {
			best, bestRel, found = m, rel, true
		}
	}
	if bestRel == "." {
		bestRel = ""
	}
	return best, bestRel, found
}
