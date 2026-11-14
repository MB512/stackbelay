package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

// Files in a job directory (/data/jobs/<id> in the manager, /job in the
// worker). The manager writes the spec, the worker writes status and log.
// Files rather than the database are the handoff channel because the
// manager may be stopped while the job runs (self-backup).
const (
	SpecFile   = "spec.json"
	StatusFile = "status.json"
	LogFile    = "log.txt"
)

const (
	handoffVersion = 1
	maxSpecBytes   = 64 << 10
	maxStatusBytes = 1 << 20
)

// Type is the kind of job.
type Type string

// Job types.
const (
	TypeBackup  Type = "backup"
	TypeRestore Type = "restore"
)

// State is the state of a job.
type State string

// Job states: queued -> running -> succeeded | failed | cancelled.
const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Final reports whether no further state change will happen.
func (s State) Final() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// jobIDPattern matches a ULID (Crockford base32, 26 characters).
var jobIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidJobID reports whether id is a well-formed job id.
func ValidJobID(id string) bool { return jobIDPattern.MatchString(id) }

// Spec describes one job. It contains no credentials: those reach the
// worker as environment variables of its container.
type Spec struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Type    Type   `json:"type"`
	Stack   string `json:"stack"`

	// Backup options.
	PreStop bool `json:"pre_stop,omitempty"`

	// Restore options.
	SnapshotID         string `json:"snapshot_id,omitempty"`
	SkipSafetySnapshot bool   `json:"skip_safety_snapshot,omitempty"`
	NoStart            bool   `json:"no_start,omitempty"`
	// CarryOverJobs is set when the manager restores its own stack: the
	// manager's jobs directory, relative to the stack directory (e.g.
	// "data/jobs"). See RestoreOptions.CarryOver.
	CarryOverJobs string `json:"carry_over_jobs,omitempty"`

	// Informational, for a manager that has to rebuild its job list from
	// the job directories (after restoring its own stack). The worker
	// ignores them.
	Trigger   string `json:"trigger,omitempty"`   // "manual" or "schedule"
	Requester string `json:"requester,omitempty"` // username, "" for scheduled jobs
}

// Validate checks all fields. The worker calls it on every spec it reads.
func (s Spec) Validate() error {
	switch {
	case s.Version != handoffVersion:
		return fmt.Errorf("unsupported spec version %d", s.Version)
	case !ValidJobID(s.ID):
		return fmt.Errorf("invalid job id %q", truncate(s.ID))
	case !stacks.ValidName(s.Stack):
		return fmt.Errorf("invalid stack name %q", truncate(s.Stack))
	}
	switch {
	case s.Trigger != "" && s.Trigger != "manual" && s.Trigger != "schedule":
		return fmt.Errorf("invalid trigger %q", truncate(s.Trigger))
	case len(s.Requester) > 64 || strings.ContainsFunc(s.Requester, func(r rune) bool { return r < ' ' }):
		return errors.New("invalid requester")
	}
	switch s.Type {
	case TypeBackup:
		if s.SnapshotID != "" || s.SkipSafetySnapshot || s.NoStart || s.CarryOverJobs != "" {
			return errors.New("restore options set on a backup job")
		}
	case TypeRestore:
		if !restic.SnapshotIDPattern.MatchString(s.SnapshotID) {
			return fmt.Errorf("invalid snapshot id %q", truncate(s.SnapshotID))
		}
		if s.CarryOverJobs != "" && !ValidCarryOver(s.CarryOverJobs) {
			return fmt.Errorf("invalid carry-over directory %q", truncate(s.CarryOverJobs))
		}
		if s.PreStop {
			return errors.New("backup options set on a restore job")
		}
	default:
		return fmt.Errorf("unknown job type %q", truncate(string(s.Type)))
	}
	return nil
}

// Status is written by the worker on every state change.
type Status struct {
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	State      State      `json:"state"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`

	Snapshot        string `json:"snapshot,omitempty"`          // backup: final snapshot; restore: restored snapshot
	PreStopSnapshot string `json:"pre_stop_snapshot,omitempty"` // backup
	SafetySnapshot  string `json:"safety_snapshot,omitempty"`   // restore
	PreviousDir     string `json:"previous_dir,omitempty"`      // restore: kept for manual rollback
}

// WriteSpec writes spec.json into dir (manager side).
func WriteSpec(dir string, s Spec) error {
	s.Version = handoffVersion
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid job spec: %w", err)
	}
	return writeJSONAtomic(filepath.Join(dir, SpecFile), s)
}

// ReadSpec reads and validates spec.json from dir. Unknown fields are
// rejected so that a newer manager cannot silently pass options an older
// worker would ignore.
func ReadSpec(dir string) (Spec, error) {
	var s Spec
	if err := readJSON(filepath.Join(dir, SpecFile), maxSpecBytes, true, &s); err != nil {
		return Spec{}, fmt.Errorf("reading job spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, fmt.Errorf("invalid job spec: %w", err)
	}
	return s, nil
}

// WriteStatus atomically replaces status.json in dir (worker side).
func WriteStatus(dir string, s Status) error {
	s.Version = handoffVersion
	return writeJSONAtomic(filepath.Join(dir, StatusFile), s)
}

// ReadStatus reads status.json from dir (manager side). It returns an error
// wrapping fs.ErrNotExist if the worker has not written it yet.
func ReadStatus(dir string) (Status, error) {
	var s Status
	if err := readJSON(filepath.Join(dir, StatusFile), maxStatusBytes, false, &s); err != nil {
		return Status{}, fmt.Errorf("reading job status: %w", err)
	}
	if s.Version != handoffVersion {
		return Status{}, fmt.Errorf("unsupported status version %d", s.Version)
	}
	return s, nil
}

// writeJSONAtomic writes v to a temporary file and renames it over path, so
// readers never see a partially written file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := MatchDirOwner(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// MatchDirOwner gives path the owner and group of its directory. The
// worker runs as root, the manager possibly not; the manager created the
// job directory, so files the worker writes there must belong to the
// manager's user to be readable (they are mode 0600). Without permission
// to change the owner (worker not root) it does nothing.
func MatchDirOwner(path string) error {
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	if err := os.Lchown(path, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("changing owner of %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSON(path string, limit int64, strict bool, v any) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is inside the job directory
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s is too large", filepath.Base(path))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// RedactingWriter replaces secret values in everything written through it.
// It is used for job logs; slog writes one record per Write call, so a
// secret cannot be split across calls.
type RedactingWriter struct {
	w       io.Writer
	secrets []string
}

// NewRedactingWriter returns a writer that replaces each of secrets (values
// shorter than 4 characters are ignored) with "[REDACTED]".
func NewRedactingWriter(w io.Writer, secrets []string) *RedactingWriter {
	var keep []string
	for _, s := range secrets {
		if len(s) >= 4 {
			keep = append(keep, s)
		}
	}
	return &RedactingWriter{w: w, secrets: keep}
}

// Write implements io.Writer. It reports len(p) on success even if the
// redacted output has a different length.
func (r *RedactingWriter) Write(p []byte) (int, error) {
	s := string(p)
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	if _, err := io.WriteString(r.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}
