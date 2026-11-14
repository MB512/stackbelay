package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// CLI implements Runner by running the restic binary.
type CLI struct {
	// Binary is the restic executable; defaults to "restic".
	Binary string
	// Logger receives restic warnings/errors; defaults to slog.Default().
	Logger *slog.Logger
}

var _ Runner = (*CLI)(nil)

func (c *CLI) binary() string {
	if c.Binary == "" {
		return "restic"
	}
	return c.Binary
}

func (c *CLI) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// command prepares a restic command. Credentials go into the environment,
// never into args.
func (c *CLI) command(ctx context.Context, repo Repo, args ...string) (*exec.Cmd, error) {
	env, err := environ(repo, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = env
	return cmd, nil
}

// passThroughEnv are the only variables restic inherits from our own
// process. Everything else (in the manager e.g. STACKVAULT_SECRET_KEY) must
// not reach the child process.
var passThroughEnv = []string{
	"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL",
	"RESTIC_CACHE_DIR", "XDG_CACHE_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

// environ builds restic's environment: an allow-listed subset of our own
// environment, the repository location and the repository's variables.
func environ(repo Repo, lookup func(string) (string, bool)) ([]string, error) {
	var env []string
	for _, k := range passThroughEnv {
		if v, ok := lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "RESTIC_REPOSITORY="+repo.Location)
	for _, kv := range repo.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !ValidEnvKey(k) {
			return nil, fmt.Errorf("repository environment variable %q is not allowed", k)
		}
		env = append(env, kv)
	}
	return env, nil
}

// runJSON runs restic with --json and calls onLine for every stdout line.
// On failure it returns an *Error containing the redacted tail of stderr.
func (c *CLI) runJSON(ctx context.Context, repo Repo, onLine func([]byte), args ...string) error {
	cmd, err := c.command(ctx, repo, append([]string{"--json"}, args...)...)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("restic %s: %w", args[0], err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting restic %s: %w", args[0], err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		onLine(sc.Bytes())
	}
	scanErr := sc.Err()
	// Wait must be called after reading stdout completely (similar to
	// awaiting Process.WaitForExit after draining the output stream in .NET).
	if err := cmd.Wait(); err != nil {
		return &Error{Op: args[0], Err: err, Stderr: redact(tail(stderr.String()), repo)}
	}
	if scanErr != nil {
		return fmt.Errorf("reading restic %s output: %w", args[0], scanErr)
	}
	return nil
}

// Error is returned when restic exits unsuccessfully.
type Error struct {
	Op     string // restic subcommand
	Err    error  // usually *exec.ExitError
	Stderr string // redacted tail of stderr
}

func (e *Error) Error() string {
	return fmt.Sprintf("restic %s: %v: %s", e.Op, e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Message returns restic's own error message for display, e.g. "Fatal:
// wrong password or no key found". With --json restic reports fatal errors
// as {"message_type":"exit_error","message":...}; without such a line the
// redacted stderr tail is returned.
func (e *Error) Message() string {
	lines := strings.Split(strings.TrimSpace(e.Stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m.Type == "exit_error" && m.Message != "" {
			return m.Message
		}
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return s
	}
	return e.Err.Error()
}

// ExitCode returns restic's exit code, or -1 if it did not exit normally.
func (e *Error) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// CheckRepo implements Runner.
func (c *CLI) CheckRepo(ctx context.Context, repo Repo) error {
	return c.runJSON(ctx, repo, func([]byte) {}, "cat", "config")
}

// Backup implements Runner.
func (c *CLI) Backup(ctx context.Context, repo Repo, path string, opts BackupOptions) (string, error) {
	if opts.Host == "" {
		return "", errors.New("restic backup: host is required")
	}
	args := []string{"backup", "--host", opts.Host}
	for _, t := range opts.Tags {
		args = append(args, "--tag", t)
	}
	if opts.Parent != "" {
		args = append(args, "--parent", opts.Parent)
	}
	args = append(args, "--", path)

	var snapshotID string
	err := c.runJSON(ctx, repo, func(line []byte) {
		msg, ok := parseBackupLine(line)
		if !ok {
			return
		}
		switch msg.MessageType {
		case "summary":
			snapshotID = msg.SnapshotID
		case "error":
			c.logger().Warn("restic backup error", "item", msg.Item, "during", msg.During,
				"error", redact(msg.Error.Message, repo))
		}
	}, args...)

	var rerr *Error
	if errors.As(err, &rerr) && rerr.ExitCode() == 3 && snapshotID != "" {
		return snapshotID, fmt.Errorf("restic backup: %w", ErrIncomplete)
	}
	if err != nil {
		return "", err
	}
	if snapshotID == "" {
		return "", errors.New("restic backup: no snapshot id in output")
	}
	return snapshotID, nil
}

type backupMessage struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
	Item        string `json:"item"`
	During      string `json:"during"`
	Error       struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseBackupLine(line []byte) (backupMessage, bool) {
	var m backupMessage
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &m) != nil {
		return m, false
	}
	return m, true
}

// Snapshots implements Runner.
func (c *CLI) Snapshots(ctx context.Context, repo Repo, ids ...string) ([]Snapshot, error) {
	for _, id := range ids {
		if !SnapshotIDPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid snapshot id %q", id)
		}
	}
	var out bytes.Buffer
	args := append([]string{"snapshots", "--"}, ids...)
	if err := c.runJSON(ctx, repo, func(line []byte) {
		out.Write(line)
		out.WriteByte('\n')
	}, args...); err != nil {
		return nil, err
	}
	return parseSnapshots(out.Bytes())
}

func parseSnapshots(data []byte) ([]Snapshot, error) {
	var snaps []Snapshot
	if err := json.Unmarshal(data, &snaps); err != nil {
		return nil, fmt.Errorf("parsing restic snapshots: %w", err)
	}
	return snaps, nil
}

// Restore implements Runner.
func (c *CLI) Restore(ctx context.Context, repo Repo, snapshotID, subpath, target string) error {
	if !SnapshotIDPattern.MatchString(snapshotID) {
		return fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	return c.runJSON(ctx, repo, func([]byte) {},
		"restore", "--target", target, "--", snapshotID+":"+subpath)
}

// Node implements Runner. It runs "restic ls --json <id> <path>", which
// prints the snapshot, then the node for path itself, then its direct
// children; only the node for path is returned.
func (c *CLI) Node(ctx context.Context, repo Repo, snapshotID, path string) (Node, error) {
	if !SnapshotIDPattern.MatchString(snapshotID) {
		return Node{}, fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	var node Node
	found := false
	err := c.runJSON(ctx, repo, func(line []byte) {
		if n, ok := parseNodeLine(line); ok && !found && n.Path == path {
			node, found = n, true
		}
	}, "ls", "--", snapshotID, path)
	if err != nil {
		return Node{}, err
	}
	if !found {
		return Node{}, fmt.Errorf("path %q not found in snapshot %s", path, snapshotID)
	}
	return node, nil
}

func parseNodeLine(line []byte) (Node, bool) {
	var n struct {
		Node
		StructType string `json:"struct_type"`
	}
	if json.Unmarshal(line, &n) != nil || n.StructType != "node" {
		return Node{}, false
	}
	return n.Node, true
}

// redact replaces secret values from repo.Env in s.
func redact(s string, repo Repo) string {
	for _, kv := range repo.Env {
		_, val, ok := strings.Cut(kv, "=")
		if ok && len(val) >= 4 {
			s = strings.ReplaceAll(s, val, "[REDACTED]")
		}
	}
	return s
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 2000
	if len(s) > max {
		return "..." + s[len(s)-max:]
	}
	return s
}
