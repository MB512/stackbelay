package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/secrets"
)

// Manager is the configuration of the manager binary.
type Manager struct {
	StacksRoot string // STACKS_ROOT, absolute
	DataDir    string // STACKVAULT_DATA_DIR, absolute, default /data
	// Hostname is passed to workers as STACKVAULT_HOSTNAME (restic --host).
	// Required: inside a container os.Hostname() is a random container id.
	Hostname   string
	ListenAddr string        // STACKVAULT_LISTEN, default ":8080"
	SessionTTL time.Duration // STACKVAULT_SESSION_TTL, default 12h
	// AllowInsecureHTTP permits login over plain HTTP (no reverse proxy
	// with TLS in front). STACKVAULT_ALLOW_INSECURE_HTTP, default false.
	AllowInsecureHTTP bool
	// WorkerImage is the image for worker containers
	// (STACKVAULT_WORKER_IMAGE). Empty means: the manager's own image.
	WorkerImage string
	// MaxJobs is the number of jobs that may run at the same time across
	// all stacks (STACKVAULT_MAX_JOBS, default 1). Each job stops a stack,
	// so parallel jobs are opt-in.
	MaxJobs  int
	LogLevel slog.Level // STACKVAULT_LOG_LEVEL, default info
	// EnableShell allows admins to open shells in stack containers
	// (STACKVAULT_ENABLE_SHELL, default false). Off by default: a shell is
	// free-form command execution.
	EnableShell bool
	// CleanupImageMinAge: the host cleanup keeps dangling images pulled,
	// tagged or replaced by an update less than this long ago
	// (STACKVAULT_CLEANUP_IMAGE_MIN_AGE, default 168h, whole hours up to
	// ten years; 0 keeps none by age).
	CleanupImageMinAge time.Duration
	// TimeZone is the zone all times are shown in and schedules run in:
	// TZ (e.g. "Europe/Berlin"), or the system's local zone if TZ is
	// unset. Go reads TZ for time.Local too; loading it here gives the
	// zone its name and turns a typo into an error instead of silent UTC.
	TimeZone *time.Location

	// SecretKey encrypts repository credentials. From STACKVAULT_SECRET_KEY
	// (base64) or STACKVAULT_SECRET_KEY_FILE (file containing the base64 key).
	// Nil if neither is set: the manager then uses SecretKeyPath, see
	// secrets.LoadOrCreateKeyFile.
	SecretKey []byte
}

// SecretKeyPath is the key file used when no key source is configured.
func (m Manager) SecretKeyPath() string { return filepath.Join(m.DataDir, secrets.KeyFileName) }

// DBPath is the SQLite database file.
func (m Manager) DBPath() string { return filepath.Join(m.DataDir, "stackvault.db") }

// JobsDir holds one directory per job (spec, status, log).
func (m Manager) JobsDir() string { return filepath.Join(m.DataDir, "jobs") }

// LoadManager reads the manager configuration. readFile is usually
// os.ReadFile (used for STACKVAULT_SECRET_KEY_FILE).
func LoadManager(getenv func(string) string, readFile func(string) ([]byte, error)) (Manager, error) {
	var errs []error
	c := Manager{
		Hostname:    getenv("STACKVAULT_HOSTNAME"),
		ListenAddr:  getenv("STACKVAULT_LISTEN"),
		WorkerImage: getenv("STACKVAULT_WORKER_IMAGE"),
		SessionTTL:  12 * time.Hour,
		MaxJobs:     1,

		CleanupImageMinAge: 7 * 24 * time.Hour,
	}
	c.StacksRoot, errs = absPath(getenv, "STACKS_ROOT", "", errs)
	c.DataDir, errs = absPath(getenv, "STACKVAULT_DATA_DIR", "/data", errs)
	c.LogLevel, errs = logLevel(getenv, errs)

	if c.Hostname == "" {
		errs = append(errs, errors.New("STACKVAULT_HOSTNAME is required (name of the Docker host, used as restic --host)"))
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if v := getenv("STACKVAULT_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			errs = append(errs, fmt.Errorf("STACKVAULT_SESSION_TTL: invalid duration %q (minimum 1m)", v))
		} else {
			c.SessionTTL = d
		}
	}
	if v := getenv("STACKVAULT_MAX_JOBS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			errs = append(errs, fmt.Errorf("STACKVAULT_MAX_JOBS: invalid value %q (1 to 32)", v))
		} else {
			c.MaxJobs = n
		}
	}
	if v := getenv("STACKVAULT_CLEANUP_IMAGE_MIN_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > 3650*24*time.Hour || d%time.Hour != 0 {
			errs = append(errs, fmt.Errorf("STACKVAULT_CLEANUP_IMAGE_MIN_AGE: invalid duration %q (whole hours, e.g. 168h; 0 to 87600h)", v))
		} else {
			c.CleanupImageMinAge = d
		}
	}
	if v := getenv("STACKVAULT_ENABLE_SHELL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_ENABLE_SHELL: %w", err))
		}
		c.EnableShell = b
	}
	if v := getenv("STACKVAULT_ALLOW_INSECURE_HTTP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKVAULT_ALLOW_INSECURE_HTTP: %w", err))
		}
		c.AllowInsecureHTTP = b
	}

	c.TimeZone = time.Local
	if tz := strings.TrimPrefix(getenv("TZ"), ":"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			errs = append(errs, fmt.Errorf("TZ: %w", err))
		} else {
			c.TimeZone = loc
		}
	}

	key, keyFile := getenv("STACKVAULT_SECRET_KEY"), getenv("STACKVAULT_SECRET_KEY_FILE")
	switch {
	case key != "" && keyFile != "":
		errs = append(errs, errors.New("set only one of STACKVAULT_SECRET_KEY and STACKVAULT_SECRET_KEY_FILE"))
	case key == "" && keyFile == "":
		// No explicit source: the key file in the data directory is used
		// (loaded or created at startup, not here).
	default:
		if keyFile != "" {
			b, err := readFile(keyFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("STACKVAULT_SECRET_KEY_FILE: %w", err))
				break
			}
			key = string(b)
		}
		k, err := secrets.ParseKey(key)
		if err != nil {
			// The error never contains the key itself.
			errs = append(errs, fmt.Errorf("secret key: %w", err))
		}
		c.SecretKey = k
	}

	return c, errors.Join(errs...)
}
