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
)

// secretEnvVars are restic/backend variables whose values must never be
// logged. Their values are passed to restic and redacted from its output.
var secretEnvVars = []string{
	"RESTIC_PASSWORD",
	"RESTIC_REST_PASSWORD",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"B2_ACCOUNT_KEY",
	"AZURE_ACCOUNT_KEY",
	"AZURE_ACCOUNT_SAS",
	"GOOGLE_ACCESS_TOKEN",
}

// Worker is the configuration of the worker binary.
type Worker struct {
	StacksRoot string        // STACKS_ROOT, absolute
	Hostname   string        // STACKVAULT_HOSTNAME, passed to restic --host
	JobTimeout time.Duration // STACKVAULT_JOB_TIMEOUT, default 6h
	LogLevel   slog.Level    // STACKVAULT_LOG_LEVEL, default info

	// RepoLocation is RESTIC_REPOSITORY. Credentials (RESTIC_PASSWORD_FILE,
	// backend keys, ...) are read by restic from the environment.
	RepoLocation string
	// RepoSecretEnv holds "KEY=value" for all secretEnvVars that are set,
	// so their values can be redacted.
	RepoSecretEnv []string
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
	if getenv("RESTIC_PASSWORD") == "" && getenv("RESTIC_PASSWORD_FILE") == "" && getenv("RESTIC_PASSWORD_COMMAND") == "" {
		errs = append(errs, errors.New("one of RESTIC_PASSWORD_FILE, RESTIC_PASSWORD or RESTIC_PASSWORD_COMMAND is required"))
	}
	for _, k := range secretEnvVars {
		if v := getenv(k); v != "" {
			c.RepoSecretEnv = append(c.RepoSecretEnv, k+"="+v)
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
