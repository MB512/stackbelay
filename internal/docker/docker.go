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
	Name string // compose service name
	// Defined: the service is in the compose file. False for an orphan: a
	// container of the project whose service was removed from the file.
	// Jobs only start, stop-and-start, pull or record defined services.
	Defined     bool
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
	// UpAll creates and starts the whole project like "docker compose up
	// --detach": missing containers are created, those whose configuration
	// or image changed are recreated, the others are left alone.
	UpAll(ctx context.Context, projectDir string) error
	// Down stops and removes the project's containers and networks
	// ("docker compose down"). Volumes are kept (never --volumes).
	Down(ctx context.Context, projectDir string) error
	// Start starts existing containers of the given services (compose start).
	Start(ctx context.Context, projectDir string, services []string) error
	// Up recreates and starts the given services (compose up -d
	// --force-recreate). Used after a restore: recreating guarantees that
	// containers use the restored directory and configuration. Docker
	// Desktop (WSL) caches bind mounts per container, so a plain restart
	// would keep showing the renamed previous directory. Named and anonymous
	// volumes are kept. An empty list means all services. composeFiles
	// (absolute paths) replace compose's own file lookup when not empty;
	// used to add an override file that pins image versions.
	Up(ctx context.Context, projectDir string, composeFiles, services []string) error
	// EnsureImage makes an image available locally: nothing happens if it
	// is present, otherwise it is pulled. ref is a validated
	// "name@sha256:..." reference. pulled reports whether it was pulled.
	EnsureImage(ctx context.Context, ref string) (pulled bool, err error)
	// ImageExists reports whether an image is present locally (no pull).
	// ref is a validated "name@sha256:..." reference.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// Pull pulls the current images of the given services (compose pull).
	// Services that are built locally (build:) are skipped. Running
	// containers are not affected.
	Pull(ctx context.Context, projectDir string, services []string) error
	// ConfiguredImages returns for each of the services the image its
	// compose file names and the id of that local image (empty if it names
	// none, or the image is not present). After Pull these are the images
	// "up" would use.
	ConfiguredImages(ctx context.Context, projectDir string, services []string) (map[string]ConfiguredImage, error)
	// WaitHealthy blocks until all given services are running and, if they
	// have a healthcheck, healthy. It gives up when ctx is done.
	WaitHealthy(ctx context.Context, projectDir string, services []string) error
}
