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

	"github.com/MB512/stackvault/internal/docker"
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
	// ComposeNewFile is the new compose file of a compose-apply job
	// (written by the manager); ComposePreviousFile the replaced one
	// (written by the worker, for a manual revert).
	ComposeNewFile      = "compose.new"
	ComposePreviousFile = "compose.previous"
)

// Limits of the cleanup options in a spec (it is at most maxSpecBytes).
const (
	MaxImageMinAgeHours = 24 * 3650
	MaxKeepImages       = 500
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
	TypeUpdate  Type = "update"
	// TypeComposeApply applies an edited compose file (compose editor).
	TypeComposeApply Type = "compose-apply"
	// TypeStop, TypeStart and TypeRestart stop, start or restart a stack
	// without backup (see Pipeline.Stop, Start, Restart).
	TypeStop    Type = "stop"
	TypeStart   Type = "start"
	TypeRestart Type = "restart"
)

// TypeCleanup removes stopped orphan containers of the discovered stacks
// and old dangling images (issue #19). It is a host job: it belongs to no
// stack and runs alone.
const TypeCleanup Type = "cleanup"

// Host reports whether t is a host job, not tied to a stack.
func (t Type) Host() bool { return t == TypeCleanup }

// Control reports whether t only stops and/or starts the stack, without
// backup and without repository.
func (t Type) Control() bool { return t == TypeStop || t == TypeStart || t == TypeRestart }

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

// sha256Pattern matches a SHA-256 in lowercase hex.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	PinImages          bool   `json:"pin_images,omitempty"` // see RestoreOptions.PinImages
	InPlace            bool   `json:"in_place,omitempty"`   // see RestoreOptions.InPlace
	// Import: the first restore of a stack from another host; its
	// directory does not exist yet (Pipeline.Import).
	Import bool `json:"import,omitempty"`
	// LimitDownloadKiB (restore): restic --limit-download, KiB/s; 0 = none.
	LimitDownloadKiB int `json:"limit_download_kib,omitempty"`
	// CarryOverJobs is set when the manager restores (or updates, for the
	// rollback) its own stack: the manager's jobs directory, relative to
	// the stack directory (e.g. "data/jobs"). See RestoreOptions.CarryOver.
	CarryOverJobs string `json:"carry_over_jobs,omitempty"`

	// Update options. NoBackup is set when the stack has no repository:
	// no snapshots and no rollback (see UpdateOptions.Backup). It is an
	// explicit choice of the manager, so missing repository settings in
	// the worker can never silently turn off the backup of an update.
	NoBackup bool `json:"no_backup,omitempty"`
	// OnlyIfNew and NoPreStop (update): see UpdateOptions.
	OnlyIfNew bool `json:"only_if_new,omitempty"`
	NoPreStop bool `json:"no_pre_stop,omitempty"`
	// RollbackInPlace (update, compose-apply): see UpdateOptions.RollbackInPlace.
	RollbackInPlace bool `json:"rollback_in_place,omitempty"`

	// Cleanup options (host job, Stack is empty): see CleanupOptions.
	ImageMinAgeHours int      `json:"image_min_age_hours,omitempty"`
	KeepImages       []string `json:"keep_images,omitempty"`

	// Compose-apply options: SHA-256 (hex) of the new content (in
	// ComposeNewFile) and of the file the editor started from.
	ComposeSHA256     string `json:"compose_sha256,omitempty"`
	ComposeBaseSHA256 string `json:"compose_base_sha256,omitempty"`

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
	case s.Type.Host() && s.Stack != "":
		return errors.New("a host job has no stack")
	case !s.Type.Host() && !stacks.ValidName(s.Stack):
		return fmt.Errorf("invalid stack name %q", truncate(s.Stack))
	}
	if !s.Type.Host() && (s.ImageMinAgeHours != 0 || len(s.KeepImages) > 0) {
		return fmt.Errorf("cleanup options set on a job of type %s", s.Type)
	}
	if s.Type != TypeRestore && (s.Import || s.LimitDownloadKiB != 0) {
		return fmt.Errorf("restore options set on a job of type %s", s.Type)
	}
	if s.LimitDownloadKiB < 0 || s.LimitDownloadKiB > restic.MaxLimitKiB {
		return fmt.Errorf("invalid download limit %d KiB/s", s.LimitDownloadKiB)
	}
	if s.Import && (s.InPlace || s.SkipSafetySnapshot || s.CarryOverJobs != "") {
		return errors.New("an import restores into a new directory: no in-place, safety snapshot or carry-over options")
	}
	switch {
	case s.Trigger != "" && s.Trigger != "manual" && s.Trigger != "schedule":
		return fmt.Errorf("invalid trigger %q", truncate(s.Trigger))
	case len(s.Requester) > 64 || strings.ContainsFunc(s.Requester, func(r rune) bool { return r < ' ' }):
		return errors.New("invalid requester")
	}
	if s.Type != TypeComposeApply && (s.ComposeSHA256 != "" || s.ComposeBaseSHA256 != "") {
		return errors.New("compose options set on a job that does not apply a compose file")
	}
	if (s.OnlyIfNew || s.NoPreStop) && s.Type != TypeUpdate {
		return fmt.Errorf("update options set on a job of type %s", s.Type)
	}
	if s.RollbackInPlace && (s.Type != TypeUpdate && s.Type != TypeComposeApply || s.NoBackup) {
		return errors.New("rollback_in_place set on a job without rollback")
	}
	switch s.Type {
	case TypeBackup:
		if s.SnapshotID != "" || s.SkipSafetySnapshot || s.NoStart || s.PinImages || s.InPlace || s.CarryOverJobs != "" {
			return errors.New("restore options set on a backup job")
		}
		if s.NoBackup {
			return errors.New("update options set on a backup job")
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
		if s.NoBackup {
			return errors.New("update options set on a restore job")
		}
	case TypeUpdate, TypeComposeApply:
		// The pre-stop snapshot is always taken (with a repository).
		if s.PreStop || s.SnapshotID != "" || s.SkipSafetySnapshot || s.NoStart || s.PinImages || s.InPlace {
			return fmt.Errorf("backup or restore options set on a job of type %s", s.Type)
		}
		if s.CarryOverJobs != "" && !ValidCarryOver(s.CarryOverJobs) {
			return fmt.Errorf("invalid carry-over directory %q", truncate(s.CarryOverJobs))
		}
		if s.Type == TypeComposeApply && (!sha256Pattern.MatchString(s.ComposeSHA256) || !sha256Pattern.MatchString(s.ComposeBaseSHA256)) {
			return errors.New("invalid compose checksums")
		}
	case TypeCleanup:
		if s.PreStop || s.SnapshotID != "" || s.SkipSafetySnapshot || s.NoStart || s.PinImages || s.InPlace ||
			s.CarryOverJobs != "" || s.NoBackup {
			return errors.New("stack options set on a cleanup job")
		}
		if s.ImageMinAgeHours < 0 || s.ImageMinAgeHours > MaxImageMinAgeHours {
			return fmt.Errorf("invalid image minimum age %d h", s.ImageMinAgeHours)
		}
		if len(s.KeepImages) > MaxKeepImages {
			return fmt.Errorf("too many images to keep (%d, at most %d)", len(s.KeepImages), MaxKeepImages)
		}
		for _, id := range s.KeepImages {
			if !docker.ValidImageID(id) {
				return fmt.Errorf("invalid image id %q", truncate(id))
			}
		}
	case TypeStop, TypeStart, TypeRestart:
		if s.PreStop || s.SnapshotID != "" || s.SkipSafetySnapshot || s.NoStart || s.PinImages || s.InPlace ||
			s.CarryOverJobs != "" || s.NoBackup {
			return fmt.Errorf("options set on a job of type %s", s.Type)
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

	Snapshot        string `json:"snapshot,omitempty"`          // backup, update: final snapshot; restore: restored snapshot
	PreStopSnapshot string `json:"pre_stop_snapshot,omitempty"` // backup, update
	SafetySnapshot  string `json:"safety_snapshot,omitempty"`   // restore
	PreviousDir     string `json:"previous_dir,omitempty"`      // restore, rolled-back update: kept for manual rollback
	RolledBack      bool   `json:"rolled_back,omitempty"`       // update: failed and restored to Snapshot
	Unchanged       bool   `json:"unchanged,omitempty"`         // update "only if new": no new image, nothing done
	// ImagesChanged: update, compose apply: services whose running image
	// (image id) differs after the change. ReplacedImages: the image ids
	// they ran before.
	ImagesChanged  []string `json:"images_changed,omitempty"`
	ReplacedImages []string `json:"replaced_images,omitempty"`
	// Cleanup: what was removed (FreedBytes: sizes of the removed images).
	RemovedContainers int   `json:"removed_containers,omitempty"`
	RemovedImages     int   `json:"removed_images,omitempty"`
	FreedBytes        int64 `json:"freed_bytes,omitempty"`
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
