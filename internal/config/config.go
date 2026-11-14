// Package config parses configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

// Worker is the configuration of the worker binary.
type Worker struct {
	StacksRoot string        // STACKS_ROOT, absolute
	Hostname   string        // STACKVAULT_HOSTNAME, passed to restic --host
	JobTimeout time.Duration // STACKVAULT_JOB_TIMEOUT, default 6h
	LogLevel   slog.Level    // STACKVAULT_LOG_LEVEL, default info

	// RepoLocation is RESTIC_REPOSITORY.
	RepoLocation string
	// RepoEnv holds "KEY=value" for every restic.BackendEnvVars variable
	// that is set (password, backend credentials). restic does not inherit
	// anything else from the worker's environment.
	RepoEnv []string
}

// LoadWorker reads the worker configuration. getenv is usually os.Getenv;
// tests pass a map lookup instead.
func LoadWorker(getenv func(string) string) (Worker, error) {
	var errs []error
	c := Worker{
		StacksRoot:   getenv("STACKS_ROOT"),
		Hostname:     getenv("STACKVAULT_HOSTNAME"),
		RepoLocation: getenv("RESTIC_REPOSITORY"),
		JobTimeout:   6 * time.Hour,
	}

	switch {
	case c.StacksRoot == "":
		errs = append(errs, errors.New("STACKS_ROOT is required"))
	case !filepath.IsAbs(c.StacksRoot):
		errs = append(errs, errors.New("STACKS_ROOT must be an absolute path"))
	default:
		c.StacksRoot = filepath.Clean(c.StacksRoot)
	}

	if c.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_HOSTNAME not set and hostname unavailable: %w", err))
		}
		c.Hostname = h
	}

	if c.RepoLocation == "" {
		errs = append(errs, errors.New("RESTIC_REPOSITORY is required"))
	}
	if getenv("RESTIC_PASSWORD") == "" && getenv("RESTIC_PASSWORD_FILE") == "" {
		errs = append(errs, errors.New("RESTIC_PASSWORD_FILE or RESTIC_PASSWORD is required"))
	}
	for _, k := range restic.BackendEnvVars {
		if v := getenv(k); v != "" {
			c.RepoEnv = append(c.RepoEnv, k+"="+v)
		}
	}

	if v := getenv("STACKVAULT_JOB_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("STACKVAULT_JOB_TIMEOUT: invalid duration %q", v))
		} else {
			c.JobTimeout = d
		}
	}

	if v := getenv("STACKVAULT_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_LOG_LEVEL: %w", err))
		}
	}

	return c, errors.Join(errs...)
}
