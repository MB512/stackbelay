package restic

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseBackupLine(t *testing.T) {
	tests := []struct {
		line   string
		ok     bool
		typ    string
		snapID string
	}{
		{`{"message_type":"status","percent_done":0.5}`, true, "status", ""},
		{`{"message_type":"summary","files_new":3,"snapshot_id":"1f2e3d4c"}`, true, "summary", "1f2e3d4c"},
		{`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/x"}`, true, "error", ""},
		{`not json`, false, "", ""},
		{``, false, "", ""},
	}
	for _, tt := range tests {
		m, ok := parseBackupLine([]byte(tt.line))
		if ok != tt.ok || m.MessageType != tt.typ || m.SnapshotID != tt.snapID {
			t.Errorf("%q: got ok=%v %+v", tt.line, ok, m)
		}
	}
}

func TestParseSnapshots(t *testing.T) {
	in := `[{"time":"2026-11-12T20:15:00.123456789Z","parent":"aaaa","tree":"t","paths":["/home/docker/nextcloud"],"hostname":"host-a","username":"root","tags":["final"],"id":"0123456789abcdef","short_id":"01234567"}]`
	snaps, err := parseSnapshots([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.ID != "0123456789abcdef" || s.ShortID != "01234567" || s.Hostname != "host-a" ||
		s.Paths[0] != "/home/docker/nextcloud" || s.Tags[0] != "final" || s.Parent != "aaaa" {
		t.Fatalf("unexpected snapshot %+v", s)
	}
	if !s.Time.Equal(time.Date(2026, 11, 12, 20, 15, 0, 123456789, time.UTC)) {
		t.Fatalf("unexpected time %v", s.Time)
	}
}

// restic >= 0.17 stores a summary in every snapshot (shape from 0.18).
func TestParseSnapshotSummary(t *testing.T) {
	in := `[{"time":"2026-11-12T20:15:00Z","paths":["/s/web"],"hostname":"h","id":"0123456789abcdef","short_id":"01234567",
	  "program_version":"restic 0.18.1","summary":{"backup_start":"2026-11-12T20:15:00.5Z","backup_end":"2026-11-12T20:16:30.5Z",
	  "files_new":3,"files_changed":0,"files_unmodified":10,"dirs_new":1,"dirs_changed":0,"dirs_unmodified":2,
	  "data_blobs":3,"tree_blobs":2,"data_added":4096,"data_added_packed":2048,"total_files_processed":13,
	  "total_bytes_processed":330301440}},
	 {"time":"2024-01-01T00:00:00Z","paths":["/s/web"],"hostname":"h","id":"1111111111111111","short_id":"11111111"}]`
	snaps, err := parseSnapshots([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	sum := snaps[0].Summary
	if sum == nil || sum.DataAddedPacked != 2048 || sum.TotalBytesProcessed != 330301440 || sum.TotalFilesProcessed != 13 ||
		sum.Duration() != 90*time.Second {
		t.Fatalf("summary %+v (duration %s)", sum, sum.Duration())
	}
	if snaps[1].Summary != nil || snaps[1].Summary.Duration() != 0 {
		t.Fatal("snapshot without summary")
	}
}

func TestRedact(t *testing.T) {
	repo := Repo{Env: []string{"RESTIC_PASSWORD=s3cret-pw", "RESTIC_REST_PASSWORD=ab", "EMPTY=",
		"RESTIC_REST_USERNAME=demo", "AWS_SECRET_ACCESS_KEY=aws-secret-key", "AWS_ACCESS_KEY_ID=AKIDEXAMPLE"}}
	got := redact("wrong password s3cret-pw for ab: user demo, stack demo-web, key AKIDEXAMPLE/aws-secret-key", repo)
	want := "wrong password [REDACTED] for ab: user demo, stack demo-web, key AKIDEXAMPLE/[REDACTED]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSnapshotIDPattern(t *testing.T) {
	for id, valid := range map[string]bool{
		"01234567":       true,
		"0123456789abcd": true,
		"0123456":        false,
		"latest":         false,
		"ABCDEF01":       false,
		"0123abcd:/etc":  false,
		"--help1234":     false,
	} {
		if SnapshotIDPattern.MatchString(id) != valid {
			t.Errorf("%q: want valid=%v", id, valid)
		}
	}
}

func TestParseNodeLine(t *testing.T) {
	snapshotLine := `{"time":"2026-11-13T21:23:47+01:00","paths":["/s/web"],"id":"e918","message_type":"snapshot","struct_type":"snapshot"}`
	if _, ok := parseNodeLine([]byte(snapshotLine)); ok {
		t.Fatal("snapshot line parsed as node")
	}
	line := `{"name":"web","type":"dir","path":"/s/web","uid":1000,"gid":1001,"mode":2147484141,"permissions":"drwxr-xr-x","mtime":"2026-11-13T21:23:45.51053436+01:00","message_type":"node","struct_type":"node"}`
	n, ok := parseNodeLine([]byte(line))
	if !ok {
		t.Fatal("node line not parsed")
	}
	if n.Path != "/s/web" || n.Type != "dir" || n.UID != 1000 || n.GID != 1001 ||
		!n.Mode.IsDir() || n.Mode.Perm() != 0o755 || n.ModTime.IsZero() {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestEnviron(t *testing.T) {
	own := map[string]string{
		"PATH":                  "/usr/bin",
		"HOME":                  "/root",
		"STACKBELAY_SECRET_KEY": "master-key",
		"RESTIC_PASSWORD":       "from-process",
		"AWS_SECRET_ACCESS_KEY": "from-process",
	}
	lookup := func(k string) (string, bool) { v, ok := own[k]; return v, ok }

	env, err := environ(Repo{Location: "rest:https://x/r", Env: []string{"RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=id"}}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/root", "RESTIC_REPOSITORY=rest:https://x/r", "RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=id"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("env = %v, want %v", env, want)
	}

	for _, bad := range []string{"RESTIC_PASSWORD_COMMAND=sh -c id", "LD_PRELOAD=/x.so", "PATH=/tmp", "NOEQUALS", "RESTIC_REPOSITORY=/other"} {
		if _, err := environ(Repo{Env: []string{bad}}, lookup); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	exitErr := errors.New("exit status 12")
	tests := []struct {
		stderr, want string
	}{
		{`{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}` + "\n",
			"Fatal: wrong password or no key found"},
		{"some warning\n" + `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repo"}`, "Fatal: unable to open repo"},
		{"Fatal: plain text error\n", "Fatal: plain text error"},
		{"", "exit status 12"},
	}
	for _, tc := range tests {
		e := &Error{Op: "snapshots", Err: exitErr, Stderr: tc.stderr}
		if got := e.Message(); got != tc.want {
			t.Errorf("Message() = %q, want %q", got, tc.want)
		}
	}
}

func TestWithLimits(t *testing.T) {
	args, err := withLimits(Repo{}, []string{"--json", "restore"})
	if err != nil || strings.Join(args, " ") != "--json restore" {
		t.Fatalf("no limit: %v %v", args, err)
	}
	args, err = withLimits(Repo{LimitDownloadKiB: 2048}, []string{"--json", "restore"})
	if err != nil || strings.Join(args, " ") != "--limit-download 2048 --json restore" {
		t.Fatalf("limit: %v %v", args, err)
	}
	for _, bad := range []int{-1, MaxLimitKiB + 1} {
		if _, err := withLimits(Repo{LimitDownloadKiB: bad}, nil); err == nil {
			t.Errorf("limit %d accepted", bad)
		}
	}
	// Lock wait (issue #46).
	args, err = withLimits(Repo{LimitDownloadKiB: 2048, LockWait: 30 * time.Minute}, []string{"--json", "backup"})
	if err != nil || strings.Join(args, " ") != "--limit-download 2048 --retry-lock 30m0s --json backup" {
		t.Fatalf("lock wait: %v %v", args, err)
	}
	if _, err := withLimits(Repo{LockWait: -time.Second}, nil); err == nil {
		t.Error("negative lock wait accepted")
	}
}

// A lock held by another process (exit code 11, also after --retry-lock
// gave up) is recognised and explained (issue #46).
func TestLockedRepository(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\n" +
		`echo "$@" > "$0.args"` + "\n" +
		"echo 'repo already locked, waiting up to 1s for the lock' >&2\n" +
		`printf '{"message_type":"exit_error","code":11,"message":"Fatal: repository is already locked exclusively by PID 1 on nas by root"}' >&2` + "\n" +
		"exit 11\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	c := &CLI{Binary: bin, Logger: slog.New(slog.DiscardHandler)}
	_, err := c.Backup(context.Background(), Repo{Location: "/r", LockWait: time.Second}, "/stacks/web", BackupOptions{Host: "h"})
	if !IsLocked(err) || IsNoRepository(err) || !strings.Contains(err.Error(), "locked by another process (maintenance") {
		t.Fatalf("err = %v", err)
	}
	args, _ := os.ReadFile(bin + ".args")
	if !strings.HasPrefix(string(args), "--retry-lock 1s ") {
		t.Errorf("args %q", args)
	}
	if IsLocked(errors.New("other")) {
		t.Error("a plain error is not a lock error")
	}
}

// TestCancelInterruptsRestic checks that an ended context interrupts
// restic (SIGINT, so it can remove its repository lock) instead of
// killing it (issue #30).
func TestCancelInterruptsRestic(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\n" +
		"trap 'echo interrupted; exit 130' INT\n" +
		"echo started\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines []string
	c := &CLI{Binary: bin}
	err := c.runJSON(ctx, Repo{Location: "/repo"}, func(b []byte) {
		lines = append(lines, string(b))
		if string(b) == "started" {
			cancel()
		}
	}, "restore")
	if err == nil {
		t.Fatal("runJSON: want an error after cancel")
	}
	if want := []string{"started", "interrupted"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("output = %q, want %q (restic was not interrupted)", lines, want)
	}
}

// restic's stderr is logged while restic runs (redacted, JSON errors with
// their fields, exit_error skipped), and a silence on stdout is noted
// (issue #42).
func TestStderrAndSilenceAreLogged(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\n" +
		"echo 'Load(<config/0000000000>, 0, 0) returned error, retrying after 1s: 500 for pw-s3cret' >&2\n" +
		`echo '{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/s/web/x"}' >&2` + "\n" +
		"sleep 0.3\n" +
		"echo '{}'\n" +
		`printf '{"message_type":"exit_error","code":1,"message":"Fatal: boom"}' >&2` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, nil))
	c := &CLI{Binary: bin, Logger: log, SilenceInterval: 100 * time.Millisecond}
	repo := Repo{Location: "/r", Env: []string{"RESTIC_PASSWORD=pw-s3cret"}}

	err := c.runJSON(context.Background(), repo, func([]byte) {}, "cat", "config")
	var rerr *Error
	if !errors.As(err, &rerr) || rerr.Message() != "Fatal: boom" {
		t.Fatalf("err = %v, want restic's exit_error", err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="restic: Load(<config/0000000000>, 0, 0) returned error, retrying after 1s: 500 for [REDACTED]"`,
		`msg="restic error" command="restic cat" item=/s/web/x during=archival error="permission denied"`,
		`msg="restic is still running, no output"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pw-s3cret") || strings.Contains(out, "boom") {
		t.Errorf("log contains the secret or the exit_error:\n%s", out)
	}
}

func TestStderrLogLimit(t *testing.T) {
	var got []string
	s := &stderrLog{max: 2, emit: func(l string) { got = append(got, l) }}
	_, _ = s.Write([]byte("a\n\nb\nc\nd"))
	s.flush()
	want := []string{"a", "b", "(further restic messages are not logged)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("emitted %q, want %q", got, want)
	}
	if s.String() != "a\n\nb\nc\nd" {
		t.Errorf("String() = %q", s.String())
	}
}

// lockedWriter serializes writes: the silence watcher and the stderr copy
// log from their own goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
