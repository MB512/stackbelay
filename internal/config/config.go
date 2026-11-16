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

// HasRepo reports whether a restic repository is configured.
func (c Worker) HasRepo() bool { return c.RepoLocation != "" }

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

	c.StacksRoot, errs = absPath(getenv, "STACKS_ROOT", "", errs)

	if c.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_HOSTNAME not set and hostname unavailable: %w", err))
		}
		c.Hostname = h
	}

	// The repository is optional here: an update of a stack without
	// repository runs without one. Jobs that need it check HasRepo.
	hasPassword := getenv("RESTIC_PASSWORD") != "" || getenv("RESTIC_PASSWORD_FILE") != ""
	switch {
	case c.RepoLocation != "" && !hasPassword:
		errs = append(errs, errors.New("RESTIC_PASSWORD_FILE or RESTIC_PASSWORD is required with RESTIC_REPOSITORY"))
	case c.RepoLocation == "" && hasPassword:
		errs = append(errs, errors.New("RESTIC_REPOSITORY is required with RESTIC_PASSWORD(_FILE)"))
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

	c.LogLevel, errs = logLevel(getenv, errs)

	return c, errors.Join(errs...)
}

// absPath reads an absolute path variable. def is used if the variable is
// unset; an empty def makes the variable required.
func absPath(getenv func(string) string, name, def string, errs []error) (string, []error) {
	v := getenv(name)
	if v == "" {
		v = def
	}
	switch {
	case v == "":
		return "", append(errs, fmt.Errorf("%s is required", name))
	case !filepath.IsAbs(v):
		return "", append(errs, fmt.Errorf("%s must be an absolute path", name))
	}
	return filepath.Clean(v), errs
}

func logLevel(getenv func(string) string, errs []error) (slog.Level, []error) {
	var l slog.Level
	if v := getenv("STACKVAULT_LOG_LEVEL"); v != "" {
		if err := l.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_LOG_LEVEL: %w", err))
		}
	}
	return l, errs
}
