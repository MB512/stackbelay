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

	"github.com/MB512/stackbelay/internal/restic"
)

// Worker is the configuration of the worker binary.
type Worker struct {
	StacksRoot string        // STACKS_ROOT, absolute
	Hostname   string        // STACKBELAY_HOSTNAME, passed to restic --host
	JobTimeout time.Duration // STACKBELAY_JOB_TIMEOUT, default DefaultJobTimeout
	// DowntimeTimeout bounds stop -> final snapshot of a backup, update or
	// compose apply (STACKBELAY_DOWNTIME_TIMEOUT, default
	// DefaultDowntimeTimeout).
	DowntimeTimeout time.Duration
	LogLevel        slog.Level // STACKBELAY_LOG_LEVEL, default info

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
		Hostname:     getenv("STACKBELAY_HOSTNAME"),
		RepoLocation: getenv("RESTIC_REPOSITORY"),
	}

	c.StacksRoot, errs = absPath(getenv, "STACKS_ROOT", "", errs)

	if c.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKBELAY_HOSTNAME not set and hostname unavailable: %w", err))
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

	c.JobTimeout, errs = jobTimeout(getenv, errs)
	c.DowntimeTimeout, errs = downtimeTimeout(getenv, errs)

	c.LogLevel, errs = logLevel(getenv, errs)

	return c, errors.Join(errs...)
}

// DefaultJobTimeout is the hard limit for one job (pre-stop snapshot,
// downtime, restore, everything). High, because a first restore of a large
// stack over the internet can take a day.
const DefaultJobTimeout = 48 * time.Hour

// DefaultDowntimeTimeout is the limit for the time a backup, update or
// compose apply keeps a stack stopped until its final snapshot is done
// (issue #36).
const DefaultDowntimeTimeout = time.Hour

// jobTimeout reads STACKBELAY_JOB_TIMEOUT (Go duration, at least 1m). The
// manager reads it to pass it on to its workers, the worker to apply it.
func jobTimeout(getenv func(string) string, errs []error) (time.Duration, []error) {
	return minuteDuration(getenv, "STACKBELAY_JOB_TIMEOUT", DefaultJobTimeout, "48h", errs)
}

// downtimeTimeout reads STACKBELAY_DOWNTIME_TIMEOUT like jobTimeout.
func downtimeTimeout(getenv func(string) string, errs []error) (time.Duration, []error) {
	return minuteDuration(getenv, "STACKBELAY_DOWNTIME_TIMEOUT", DefaultDowntimeTimeout, "1h", errs)
}

// minuteDuration reads a Go duration of at least one minute; def if unset.
func minuteDuration(getenv func(string) string, name string, def time.Duration, example string, errs []error) (time.Duration, []error) {
	v := getenv(name)
	if v == "" {
		return def, errs
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return def, append(errs, fmt.Errorf("%s: invalid duration %q (minimum 1m, e.g. %s)", name, v, example))
	}
	return d, errs
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
	if v := getenv("STACKBELAY_LOG_LEVEL"); v != "" {
		if err := l.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			errs = append(errs, fmt.Errorf("STACKBELAY_LOG_LEVEL: %w", err))
		}
	}
	return l, errs
}
