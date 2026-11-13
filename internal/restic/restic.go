// Package restic drives the restic CLI.
//
// Only create and read operations are exposed (backup, restore, snapshots,
// ls, cat config). Repositories are expected to be append-only; never add
// forget/prune/tag here.
package restic

import (
	"context"
	"errors"
	"os"
	"regexp"
	"time"
)

// Repo identifies a repository and carries its credentials.
type Repo struct {
	// Location is the restic repository string (path, rest:..., s3:...).
	// It is passed via RESTIC_REPOSITORY, not as an argument.
	Location string
	// Env holds secret environment variables ("KEY=value") for restic, e.g.
	// RESTIC_PASSWORD_FILE or backend credentials. Values are redacted from
	// any output and never appear on the command line.
	Env []string
}

// Snapshot is one entry of "restic snapshots --json".
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
	Parent   string    `json:"parent"`
}

// Node is one file or directory entry inside a snapshot ("restic ls --json").
type Node struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"` // "dir", "file", "symlink", ...
	Path    string      `json:"path"`
	UID     uint32      `json:"uid"`
	GID     uint32      `json:"gid"`
	Mode    os.FileMode `json:"mode"` // Go FileMode bits, as stored by restic
	ModTime time.Time   `json:"mtime"`
}

// BackupOptions control a single backup run.
type BackupOptions struct {
	Host   string   // value for --host; required (the worker's container hostname is random)
	Tags   []string // e.g. "pre-stop", "final"
	Parent string   // optional parent snapshot id
}

// ErrIncomplete is returned by Backup when restic created a snapshot but
// could not read all source files (exit code 3).
var ErrIncomplete = errors.New("snapshot created, but some files could not be read")

// SnapshotIDPattern is the accepted form of snapshot ids from user input.
var SnapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// Runner is the subset of restic operations the job pipelines need.
type Runner interface {
	// CheckRepo verifies that the repository is reachable and the
	// credentials are valid (restic cat config).
	CheckRepo(ctx context.Context, repo Repo) error
	// Backup backs up the absolute path and returns the new snapshot id.
	// On ErrIncomplete the returned id is still set.
	Backup(ctx context.Context, repo Repo, path string, opts BackupOptions) (string, error)
	// Snapshots lists snapshots; with ids, only those snapshots.
	Snapshots(ctx context.Context, repo Repo, ids ...string) ([]Snapshot, error)
	// Restore restores the contents of subpath within the snapshot
	// (restic restore <id>:<subpath>) into target.
	Restore(ctx context.Context, repo Repo, snapshotID, subpath, target string) error
	// Node returns the metadata of path inside the snapshot (restic ls).
	// Restore uses it to apply mode and owner of the stack directory itself,
	// which "restic restore <id>:<subpath>" does not restore.
	Node(ctx context.Context, repo Repo, snapshotID, path string) (Node, error)
}
