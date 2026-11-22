package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/secrets"
)

// Manager is the configuration of the manager binary.
type Manager struct {
	StacksRoot string // STACKS_ROOT, absolute
	DataDir    string // STACKBELAY_DATA_DIR, absolute, default /data
	// Hostname is passed to workers as STACKBELAY_HOSTNAME (restic --host).
	// Required: inside a container os.Hostname() is a random container id.
	Hostname   string
	ListenAddr string        // STACKBELAY_LISTEN, default ":8080"
	SessionTTL time.Duration // STACKBELAY_SESSION_TTL, default 12h
	// AllowInsecureHTTP permits login over plain HTTP (no reverse proxy
	// with TLS in front). STACKBELAY_ALLOW_INSECURE_HTTP, default false.
	AllowInsecureHTTP bool
	// WorkerImage is the image for worker containers
	// (STACKBELAY_WORKER_IMAGE). Empty means: the manager's own image.
	WorkerImage string
	// MaxJobs is the number of jobs that may run at the same time across
	// all stacks (STACKBELAY_MAX_JOBS, default 3): it limits the disk and
	// network load of parallel jobs.
	MaxJobs int
	// MaxDowntimes is the number of jobs that may keep a stack stopped at
	// the same time (STACKBELAY_MAX_DOWNTIMES, default 1, issue #36): the
	// downtime slots of backups, updates and compose applies.
	MaxDowntimes int
	// JobLogRetention: job directories (log, status, spec) of jobs that
	// finished longer ago are deleted; the job rows stay
	// (STACKBELAY_JOB_LOG_RETENTION, default 2160h = 90 days, 0 = keep
	// forever; issue #16).
	JobLogRetention time.Duration
	// DowntimeTimeout is passed to every worker (STACKBELAY_DOWNTIME_TIMEOUT,
	// default DefaultDowntimeTimeout).
	DowntimeTimeout time.Duration
	// JobTimeout is the hard limit for one job, passed to every worker
	// (STACKBELAY_JOB_TIMEOUT, default DefaultJobTimeout).
	JobTimeout time.Duration
	LogLevel   slog.Level // STACKBELAY_LOG_LEVEL, default info
	// EnableShell allows admins to open shells in stack containers
	// (STACKBELAY_ENABLE_SHELL, default false). Off by default: a shell is
	// free-form command execution.
	EnableShell bool
	// CleanupImageMinAge: the host cleanup keeps dangling images pulled,
	// tagged or replaced by an update less than this long ago
	// (STACKBELAY_CLEANUP_IMAGE_MIN_AGE, default 168h, whole hours up to
	// ten years; 0 keeps none by age).
	CleanupImageMinAge time.Duration
	// TimeZone is the zone all times are shown in and schedules run in:
	// TZ (e.g. "Europe/Berlin"), or the system's local zone if TZ is
	// unset. Go reads TZ for time.Local too; loading it here gives the
	// zone its name and turns a typo into an error instead of silent UTC.
	TimeZone *time.Location

	// SecretKey encrypts repository credentials. From STACKBELAY_SECRET_KEY
	// (base64) or STACKBELAY_SECRET_KEY_FILE (file containing the base64 key).
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
// os.ReadFile (used for STACKBELAY_SECRET_KEY_FILE).
func LoadManager(getenv func(string) string, readFile func(string) ([]byte, error)) (Manager, error) {
	var errs []error
	c := Manager{
		Hostname:     getenv("STACKBELAY_HOSTNAME"),
		ListenAddr:   getenv("STACKBELAY_LISTEN"),
		WorkerImage:  getenv("STACKBELAY_WORKER_IMAGE"),
		SessionTTL:   12 * time.Hour,
		MaxJobs:      3,
		MaxDowntimes: 1,

		JobLogRetention: 90 * 24 * time.Hour,

		CleanupImageMinAge: 7 * 24 * time.Hour,
	}
	c.StacksRoot, errs = absPath(getenv, "STACKS_ROOT", "", errs)
	c.DataDir, errs = absPath(getenv, "STACKBELAY_DATA_DIR", "/data", errs)
	c.LogLevel, errs = logLevel(getenv, errs)
	c.JobTimeout, errs = jobTimeout(getenv, errs)
	c.DowntimeTimeout, errs = downtimeTimeout(getenv, errs)

	if c.Hostname == "" {
		errs = append(errs, errors.New("STACKBELAY_HOSTNAME is required (name of the Docker host, used as restic --host)"))
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if v := getenv("STACKBELAY_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			errs = append(errs, fmt.Errorf("STACKBELAY_SESSION_TTL: invalid duration %q (minimum 1m)", v))
		} else {
			c.SessionTTL = d
		}
	}
	c.MaxJobs, errs = smallCount(getenv, "STACKBELAY_MAX_JOBS", c.MaxJobs, errs)
	c.MaxDowntimes, errs = smallCount(getenv, "STACKBELAY_MAX_DOWNTIMES", c.MaxDowntimes, errs)
	if v := getenv("STACKBELAY_CLEANUP_IMAGE_MIN_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > 3650*24*time.Hour || d%time.Hour != 0 {
			errs = append(errs, fmt.Errorf("STACKBELAY_CLEANUP_IMAGE_MIN_AGE: invalid duration %q (whole hours, e.g. 168h; 0 to 87600h)", v))
		} else {
			c.CleanupImageMinAge = d
		}
	}
	if v := getenv("STACKBELAY_JOB_LOG_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || (d > 0 && d < 24*time.Hour) {
			errs = append(errs, fmt.Errorf("STACKBELAY_JOB_LOG_RETENTION: invalid duration %q (at least 24h, e.g. 2160h for 90 days; 0 keeps the logs forever)", v))
		} else {
			c.JobLogRetention = d
		}
	}
	if v := getenv("STACKBELAY_ENABLE_SHELL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKBELAY_ENABLE_SHELL: %w", err))
		}
		c.EnableShell = b
	}
	if v := getenv("STACKBELAY_ALLOW_INSECURE_HTTP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("STACKBELAY_ALLOW_INSECURE_HTTP: %w", err))
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

	key, keyFile := getenv("STACKBELAY_SECRET_KEY"), getenv("STACKBELAY_SECRET_KEY_FILE")
	switch {
	case key != "" && keyFile != "":
		errs = append(errs, errors.New("set only one of STACKBELAY_SECRET_KEY and STACKBELAY_SECRET_KEY_FILE"))
	case key == "" && keyFile == "":
		// No explicit source: the key file in the data directory is used
		// (loaded or created at startup, not here).
	default:
		if keyFile != "" {
			b, err := readFile(keyFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("STACKBELAY_SECRET_KEY_FILE: %w", err))
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

// smallCount reads a whole number from 1 to 32; def if unset.
func smallCount(getenv func(string) string, name string, def int, errs []error) (int, []error) {
	v := getenv(name)
	if v == "" {
		return def, errs
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 32 {
		return def, append(errs, fmt.Errorf("%s: invalid value %q (1 to 32)", name, v))
	}
	return n, errs
}
