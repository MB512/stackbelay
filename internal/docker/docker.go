// Package docker drives Docker Compose through the docker CLI.
//
// The job pipelines depend only on the small Client interface so they can
// be tested with a fake. CLI is the real implementation.
package docker

import (
	"context"
)

// ServiceState describes one compose service at a point in time.
type ServiceState struct {
	Name        string   // compose service name
	State       string   // container state ("running", "exited", ...); "" if no container exists
	Health      string   // "healthy", "unhealthy", "starting" or "" (no healthcheck)
	ContainerID string   // full container id; "" if no container exists
	ImageRef    string   // image as configured, e.g. "nextcloud:29"
	ImageID     string   // local image id, "sha256:..."
	RepoDigests []string // pullable references, e.g. "nextcloud@sha256:..."
}

// Running reports whether the service's container is running.
func (s ServiceState) Running() bool { return s.State == "running" }

// Client is the subset of Docker Compose operations the job pipelines need.
// projectDir is the absolute stack directory; it is used as working
// directory and project directory, so compose.yml and compose.override.yml
// are picked up exactly as when a user runs "docker compose" there.
type Client interface {
	// Services lists all services defined in the project, including those
	// without a container.
	Services(ctx context.Context, projectDir string) ([]ServiceState, error)
	// Stop stops all containers of the project (compose stop, not down).
	Stop(ctx context.Context, projectDir string) error
	// Start starts existing containers of the given services (compose start).
	Start(ctx context.Context, projectDir string, services []string) error
	// Up creates/recreates and starts the given services (compose up -d).
	// An empty list means all services.
	Up(ctx context.Context, projectDir string, services []string) error
	// WaitHealthy blocks until all given services are running and, if they
	// have a healthcheck, healthy. It gives up when ctx is done.
	WaitHealthy(ctx context.Context, projectDir string, services []string) error
}
