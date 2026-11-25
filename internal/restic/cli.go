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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CLI implements Runner by running the restic binary.
type CLI struct {
	// Binary is the restic executable; defaults to "restic".
	Binary string
	// Logger receives restic warnings/errors; defaults to slog.Default().
	Logger *slog.Logger
	// Progress, if set, receives the status lines of backups and restores
	// (about once per second, see progressFPS). The worker turns them into
	// the job's progress.
	Progress func(Progress)
	// SilenceInterval: when restic prints nothing on stdout for this long,
	// "restic is still running" is logged; default one minute.
	SilenceInterval time.Duration
}

// progressFPS limits restic's status lines when Progress is set: with
// --json restic would print up to 60 per second.
const progressFPS = "1"

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
	args, err = withLimits(repo, args)
	if err != nil {
		return nil, err
	}
	if c.Progress != nil {
		env = append(env, "RESTIC_PROGRESS_FPS="+progressFPS)
	}
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = env
	// When ctx ends (cancel, job timeout), ask restic to stop like Ctrl+C
	// does, so it removes its lock from the repository: in an append-only
	// repository a stale lock cannot be removed by a client. Only if restic
	// has not exited after stopGrace is it killed (the default without
	// Cancel is an immediate kill). Like Process.CloseMainWindow before
	// Process.Kill in .NET.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = stopGrace
	return cmd, nil
}

// stopGrace is how long restic may take to clean up after an interrupt.
const stopGrace = 30 * time.Second

// withLimits puts the repository's global options (download limit, lock
// wait) in front of args.
func withLimits(repo Repo, args []string) ([]string, error) {
	var global []string
	switch {
	case repo.LimitDownloadKiB < 0 || repo.LimitDownloadKiB > MaxLimitKiB:
		return nil, fmt.Errorf("invalid download limit %d KiB/s", repo.LimitDownloadKiB)
	case repo.LimitDownloadKiB > 0:
		global = append(global, "--limit-download", strconv.Itoa(repo.LimitDownloadKiB))
	}
	switch {
	case repo.LockWait < 0:
		return nil, fmt.Errorf("invalid lock wait %s", repo.LockWait)
	case repo.LockWait > 0:
		// restic retries for this long while another process (e.g. a prune
		// on the server side) holds an exclusive lock, and says so on stderr.
		global = append(global, "--retry-lock", repo.LockWait.String())
	}
	return append(global, args...), nil
}

// passThroughEnv are the only variables restic inherits from our own
// process. Everything else (in the manager e.g. STACKBELAY_SECRET_KEY) must
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
	// restic's stderr (retries, lock waits, warnings) is logged line by line
	// while restic runs (issue #42) and kept for the error below.
	log := c.logger().With("command", "restic "+args[0])
	stderr := &stderrLog{
		emit: func(line string) { logStderrLine(log, redact(line, repo)) },
		max:  maxStderrLines,
	}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("restic %s: %w", args[0], err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting restic %s: %w", args[0], err)
	}
	// A goroutine notes when restic has been silent on stdout for a while,
	// so a hang (e.g. a slow repository server) is visible in the job log.
	// lastOutput is shared with it, hence atomic (like Interlocked in .NET).
	var lastOutput atomic.Int64
	lastOutput.Store(time.Now().UnixNano())
	// On return, close done and wait until the watcher has exited, so it
	// never logs after runJSON returned (like cancelling a Task and
	// awaiting it in .NET).
	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Go(func() { c.watchSilence(log, &lastOutput, done) })
	defer watcher.Wait()
	defer close(done)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		lastOutput.Store(time.Now().UnixNano())
		onLine(sc.Bytes())
	}
	scanErr := sc.Err()
	// Wait must be called after reading stdout completely (similar to
	// awaiting Process.WaitForExit after draining the output stream in .NET).
	waitErr := cmd.Wait()
	// Wait has also finished copying stderr, so a last line without a
	// newline can be flushed now.
	stderr.flush()
	if waitErr != nil {
		return &Error{Op: args[0], Err: waitErr, Stderr: redact(tail(stderr.String()), repo)}
	}
	if scanErr != nil {
		return fmt.Errorf("reading restic %s output: %w", args[0], scanErr)
	}
	return nil
}

// maxStderrLines limits how many stderr lines of one restic command are
// logged, so e.g. a restore with many unreadable files cannot flood the
// job log. The error tail still has the last lines.
const maxStderrLines = 200

// defaultSilenceInterval is how long restic may print nothing on stdout
// before "restic is still running" is logged (and again after each further
// interval). Backups and restores print a status line every second, so
// this only shows up when restic waits for something.
const defaultSilenceInterval = time.Minute

func (c *CLI) silenceInterval() time.Duration {
	if c.SilenceInterval <= 0 {
		return defaultSilenceInterval
	}
	return c.SilenceInterval
}

// watchSilence logs "restic is still running" whenever restic has printed
// nothing on stdout for silenceInterval, until done is closed.
func (c *CLI) watchSilence(log *slog.Logger, lastOutput *atomic.Int64, done <-chan struct{}) {
	interval := c.silenceInterval()
	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			silent := now.Sub(time.Unix(0, lastOutput.Load()))
			if silent >= interval {
				log.Info("restic is still running, no output", "elapsed", now.Sub(start).Round(time.Second).String(),
					"silent_for", silent.Round(time.Second).String())
			}
		}
	}
}

// stderrLog is restic's stderr: it keeps everything for the error tail and
// passes each complete line to emit (at most max lines). Write is called
// from the goroutine that exec uses to copy stderr, String and flush only
// after Wait, so the mutex is only a safeguard.
type stderrLog struct {
	mu      sync.Mutex
	all     bytes.Buffer
	partial []byte
	emitted int
	max     int
	emit    func(string)
}

func (s *stderrLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		s.line(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
	}
	return len(p), nil
}

// flush emits a last line that did not end with a newline.
func (s *stderrLog) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) > 0 {
		s.line(string(s.partial))
		s.partial = nil
	}
}

func (s *stderrLog) line(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	switch {
	case s.emitted < s.max:
		s.emit(line)
	case s.emitted == s.max:
		s.emit("(further restic messages are not logged)")
	default:
		return
	}
	s.emitted++
}

func (s *stderrLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

// logStderrLine logs one (redacted) stderr line of restic. With --json,
// restic writes errors as JSON to stderr: per-file errors of backup and
// restore are logged with their fields; the fatal exit_error is skipped
// because the returned *Error reports it. Plain text lines (retries,
// warnings) are logged as they are.
func logStderrLine(log *slog.Logger, line string) {
	var m struct {
		Type    string `json:"message_type"`
		Message string `json:"message"`
		Item    string `json:"item"`
		During  string `json:"during"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &m) != nil {
		log.Warn("restic: " + line)
		return
	}
	switch {
	case m.Type == "exit_error":
		return
	case m.Type == "error":
		log.Warn("restic error", "item", m.Item, "during", m.During, "error", m.Error.Message)
	case m.Message != "":
		log.Warn("restic: " + m.Message)
	default:
		log.Warn("restic: " + line)
	}
}

// Error is returned when restic exits unsuccessfully.
type Error struct {
	Op     string // restic subcommand
	Err    error  // usually *exec.ExitError
	Stderr string // redacted tail of stderr
}

func (e *Error) Error() string {
	if e.ExitCode() == exitLocked {
		return fmt.Sprintf("restic %s: the repository is locked by another process (maintenance such as prune running?): %s",
			e.Op, e.Stderr)
	}
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

// exitNoRepository is restic's exit code for "repository does not exist"
// (restic >= 0.17).
const exitNoRepository = 10

// exitLocked is restic's exit code for "repository is already locked"
// (restic >= 0.17), also after --retry-lock gave up.
const exitLocked = 11

// IsLocked reports whether err means that restic could not lock the
// repository because another process holds a lock (issue #46).
func IsLocked(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.ExitCode() == exitLocked
}

// IsNoRepository reports whether err means that there is no repository at
// the location (as opposed to e.g. a wrong password or a network error).
func IsNoRepository(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.ExitCode() == exitNoRepository
}

// ExitCode returns restic's exit code, or -1 if it did not exit normally.
func (e *Error) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Init creates a new repository at repo.Location (restic init) with the
// password from repo.Env. restic refuses if a repository already exists
// there, so this never overwrites anything; it is allowed with an
// append-only rest-server. Only the manager calls it, on an explicit admin
// action, so it is not part of Runner.
func (c *CLI) Init(ctx context.Context, repo Repo) error {
	return c.runJSON(ctx, repo, func([]byte) {}, "init")
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
		case "status":
			c.report(Progress{Percent: msg.PercentDone, BytesDone: msg.BytesDone, TotalBytes: msg.TotalBytes,
				FilesDone: msg.FilesDone, TotalFiles: msg.TotalFiles,
				SecondsElapsed: msg.SecondsElapsed, SecondsRemaining: msg.SecondsRemaining})
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
	// status lines
	PercentDone      float64 `json:"percent_done"`
	BytesDone        uint64  `json:"bytes_done"`
	TotalBytes       uint64  `json:"total_bytes"`
	FilesDone        uint64  `json:"files_done"`
	TotalFiles       uint64  `json:"total_files"`
	SecondsElapsed   int64   `json:"seconds_elapsed"`
	SecondsRemaining int64   `json:"seconds_remaining"`
	Item             string  `json:"item"`
	During           string  `json:"during"`
	Error            struct {
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

// Restore implements Runner: "restic restore --delete --overwrite
// if-changed". if-changed treats a file with the same size and modification
// time as unchanged and only reads (and, on a mismatch, rewrites) the
// others, so a large stack is restored without downloading or copying the
// data that did not change, and a restore that was interrupted continues
// where it stopped. Each excluded path becomes an anchored pattern
// ("/data/jobs"), which also protects it from --delete.
func (c *CLI) Restore(ctx context.Context, repo Repo, snapshotID, subpath, target string, exclude []string) error {
	if !SnapshotIDPattern.MatchString(snapshotID) {
		return fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	args := []string{"restore", "--target", target, "--delete", "--overwrite", "if-changed"}
	for _, dir := range exclude {
		if !validExclude(dir) {
			return fmt.Errorf("invalid exclude path %q", dir)
		}
		args = append(args, "--exclude", "/"+dir)
	}
	args = append(args, "--", snapshotID+":"+subpath)
	return c.runJSON(ctx, repo, c.onRestoreLine, args...)
}

// validExclude accepts clean relative paths without restic pattern syntax
// (*, ?, [, \), so the pattern matches exactly that file or directory.
func validExclude(dir string) bool {
	return dir != "" && dir != "." && filepath.IsLocal(dir) && filepath.Clean(dir) == dir &&
		!strings.ContainsAny(dir, "*?[\\\x00\n")
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
	for _, val := range SecretValues(repo.Env) {
		s = strings.ReplaceAll(s, val, "[REDACTED]")
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

// report passes a status to Progress, if set.
func (c *CLI) report(p Progress) {
	if c.Progress != nil {
		c.Progress(p)
	}
}

// restoreMessage is a status line of "restic restore --json" (restic >=
// 0.17). Skipped files and bytes (unchanged, in-place restore) count as
// done, as in restic's own percent_done.
type restoreMessage struct {
	MessageType    string  `json:"message_type"`
	PercentDone    float64 `json:"percent_done"`
	TotalFiles     uint64  `json:"total_files"`
	FilesRestored  uint64  `json:"files_restored"`
	FilesSkipped   uint64  `json:"files_skipped"`
	TotalBytes     uint64  `json:"total_bytes"`
	BytesRestored  uint64  `json:"bytes_restored"`
	BytesSkipped   uint64  `json:"bytes_skipped"`
	SecondsElapsed int64   `json:"seconds_elapsed"`
}

func (c *CLI) onRestoreLine(line []byte) {
	if c.Progress == nil {
		return
	}
	var m restoreMessage
	if json.Unmarshal(line, &m) != nil || m.MessageType != "status" {
		return
	}
	c.report(Progress{Percent: m.PercentDone, BytesDone: m.BytesRestored + m.BytesSkipped, TotalBytes: m.TotalBytes,
		FilesDone: m.FilesRestored + m.FilesSkipped, TotalFiles: m.TotalFiles, SecondsElapsed: m.SecondsElapsed})
}
