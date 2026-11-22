package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
)

const testJobID = "01JCZ8G3T4S5V6W7X8Y9Z0ABCD"

func testEnv(t *testing.T) func(string) string {
	t.Helper()
	env := map[string]string{
		"STACKS_ROOT":          t.TempDir(),
		"STACKBELAY_HOSTNAME":  "h",
		"RESTIC_REPOSITORY":    "/nonexistent",
		"RESTIC_PASSWORD":      "super-secret-repo-pw",
		"RESTIC_REST_PASSWORD": "rest-server-secret",
	}
	return func(k string) string { return env[k] }
}

func TestRunUsageAndConfigErrors(t *testing.T) {
	getenv := testEnv(t)
	tests := []struct {
		name     string
		args     []string
		getenv   func(string) string
		wantCode int
		wantOut  string
	}{
		{"no config", []string{"backup", "x"}, func(string) string { return "" }, 2, "STACKS_ROOT is required"},
		{"no command", nil, getenv, 2, "missing command"},
		{"unknown command", []string{"prune"}, getenv, 2, "unknown command"},
		{"backup without stack", []string{"backup"}, getenv, 2, "backup expects"},
		{"restore without snapshot", []string{"restore", "web"}, getenv, 2, "restore expects"},
		{"run without dir", []string{"run"}, getenv, 2, "run expects"},
		{"update without stack", []string{"update", "--no-backup"}, getenv, 2, "update expects"},
		{"stop without stack", []string{"stop"}, getenv, 2, "stop expects"},
		{"restart with flag", []string{"restart", "--force"}, getenv, 2, "restart expects"},
		{"start with two stacks", []string{"start", "a", "b"}, getenv, 2, "start expects"},
		{"unknown stack", []string{"backup", "../etc"}, getenv, 1, "stack not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code := run(tt.args, tt.getenv, &out)
			if code != tt.wantCode {
				t.Fatalf("exit code %d, want %d; output: %s", code, tt.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestRunJobDirInvalidSpec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, jobs.SpecFile), []byte(`{"version":1,"type":"prune"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"run", dir}, testEnv(t), &out); code != 2 {
		t.Fatalf("exit code %d; output %s", code, out.String())
	}
	st, err := jobs.ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != jobs.StateFailed || !strings.Contains(st.Error, "invalid job spec") || st.FinishedAt == nil {
		t.Fatalf("unexpected status %+v", st)
	}
}

// A valid spec for a stack that does not exist fails after "running" was
// written; status and log must record that, without leaking secrets.
func TestRunJobDirFailedJob(t *testing.T) {
	dir := t.TempDir()
	if err := jobs.WriteSpec(dir, jobs.Spec{ID: testJobID, Type: jobs.TypeBackup, Stack: "missing"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"run", dir}, testEnv(t), &out); code != 1 {
		t.Fatalf("exit code %d; output %s", code, out.String())
	}

	st, err := jobs.ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != testJobID || st.State != jobs.StateFailed || st.StartedAt == nil || st.FinishedAt == nil ||
		!strings.Contains(st.Error, "stack not found") {
		t.Fatalf("unexpected status %+v", st)
	}

	logData, err := os.ReadFile(filepath.Join(dir, jobs.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("log is not JSON lines: %v\n%s", err, logData)
	}
	if last["msg"] != "job failed" || last["job_id"] != testJobID {
		t.Fatalf("unexpected last log line %v", last)
	}
	for _, secret := range []string{"super-secret-repo-pw", "rest-server-secret"} {
		if bytes.Contains(logData, []byte(secret)) || strings.Contains(out.String(), secret) {
			t.Fatalf("secret %q leaked into the log", secret)
		}
	}
}

func TestRedact(t *testing.T) {
	got := redact("pw=super-secret-repo-pw", restic.SecretValues([]string{"RESTIC_PASSWORD=super-secret-repo-pw", "X=ab"}))
	if got != "pw=[REDACTED]" {
		t.Fatalf("got %q", got)
	}
}

// Without repository settings only an update marked --no-backup may run;
// everything else fails before touching the stack.
func TestRunWithoutRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"STACKS_ROOT": root, "STACKBELAY_HOSTNAME": "h"}
	getenv := func(k string) string { return env[k] }
	for _, args := range [][]string{{"backup", "web"}, {"update", "web"}, {"restore", "web", "0123abcd"}} {
		var out bytes.Buffer
		if code := run(args, getenv, &out); code != 1 || !strings.Contains(out.String(), "no repository configured") {
			t.Errorf("%v: code %d, output %s", args, code, out.String())
		}
	}
}

func TestReadComposeNew(t *testing.T) {
	dir := t.TempDir()
	content := []byte("services: {}\n")
	if err := os.WriteFile(filepath.Join(dir, jobs.ComposeNewFile), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readComposeNew(dir, jobs.SHA256Hex(content)); err != nil || string(got) != string(content) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := readComposeNew(dir, jobs.SHA256Hex([]byte("other"))); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := readComposeNew(t.TempDir(), jobs.SHA256Hex(content)); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestParseRestoreArgs(t *testing.T) {
	spec, _, err := parseArgs([]string{"restore", "--pin-images", "web", "0123abcd"})
	if err != nil {
		t.Fatal(err)
	}
	want := jobs.Spec{Type: jobs.TypeRestore, Stack: "web", SnapshotID: "0123abcd", PinImages: true}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("got %+v, want %+v", spec, want)
	}
}

func TestParseUpdateArgs(t *testing.T) {
	spec, _, err := parseArgs([]string{"update", "--no-backup", "web"})
	if err != nil {
		t.Fatal(err)
	}
	want := jobs.Spec{Type: jobs.TypeUpdate, Stack: "web", NoBackup: true}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("got %+v, want %+v", spec, want)
	}
	for _, removed := range [][]string{{"update", "--rollback-in-place", "web"}, {"update", "--no-pre-stop", "web"},
		{"backup", "--pre-stop", "web"}, {"update", "--only-if-new", "web"}} {
		if _, _, err := parseArgs(removed); !errors.Is(err, errUsage) {
			t.Fatalf("removed flag accepted: %v: %v", removed, err)
		}
	}
}

func TestParseControlArgs(t *testing.T) {
	for _, cmd := range []string{"stop", "start", "restart"} {
		spec, _, err := parseArgs([]string{cmd, "web"})
		if err != nil {
			t.Fatal(err)
		}
		if want := (jobs.Spec{Type: jobs.Type(cmd), Stack: "web"}); !reflect.DeepEqual(spec, want) {
			t.Fatalf("got %+v, want %+v", spec, want)
		}
	}
}

func TestNeedsRepository(t *testing.T) {
	for _, tt := range []struct {
		spec jobs.Spec
		want bool
	}{
		{jobs.Spec{Type: jobs.TypeBackup}, true},
		{jobs.Spec{Type: jobs.TypeRestore}, true},
		{jobs.Spec{Type: jobs.TypeUpdate}, true},
		{jobs.Spec{Type: jobs.TypeUpdate, NoBackup: true}, false},
		{jobs.Spec{Type: jobs.TypeComposeApply, NoBackup: true}, false},
		{jobs.Spec{Type: jobs.TypeStop}, false},
		{jobs.Spec{Type: jobs.TypeStart}, false},
		{jobs.Spec{Type: jobs.TypeRestart}, false},
	} {
		if got := needsRepository(tt.spec); got != tt.want {
			t.Errorf("%+v: %v, want %v", tt.spec, got, tt.want)
		}
	}
}

func TestParseCleanupArgs(t *testing.T) {
	spec, _, err := parseArgs([]string{"cleanup", "--image-min-age", "48h"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (jobs.Spec{Type: jobs.TypeCleanup, ImageMinAgeHours: 48}); !reflect.DeepEqual(spec, want) {
		t.Fatalf("got %+v, want %+v", spec, want)
	}
	if spec, _, err := parseArgs([]string{"cleanup"}); err != nil || spec.ImageMinAgeHours != 7*24 {
		t.Fatalf("default: %+v, %v", spec, err)
	}
	for _, args := range [][]string{{"cleanup", "web"}, {"cleanup", "--image-min-age", "-1h"}} {
		if _, _, err := parseArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if needsRepository(jobs.Spec{Type: jobs.TypeCleanup}) {
		t.Error("cleanup needs no repository")
	}
}

func TestParseImportArgs(t *testing.T) {
	spec, _, err := parseArgs([]string{"restore", "--import", "--no-start", "--limit-download", "4096", "web", "0123abcd"})
	if err != nil {
		t.Fatal(err)
	}
	want := jobs.Spec{Type: jobs.TypeRestore, Stack: "web", SnapshotID: "0123abcd", Import: true, NoStart: true, LimitDownloadKiB: 4096}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("got %+v, want %+v", spec, want)
	}
}

// An import must not touch an existing directory: the worker refuses
// before anything else happens.
func TestImportRefusesExistingStack(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"STACKS_ROOT": root, "STACKBELAY_HOSTNAME": "h", "RESTIC_REPOSITORY": "/r", "RESTIC_PASSWORD": "pw"}
	var out bytes.Buffer
	if code := run([]string{"restore", "--import", "web", "0123abcd"}, func(k string) string { return env[k] }, &out); code != 1 ||
		!strings.Contains(out.String(), "already exists") {
		t.Fatalf("code %d, output %s", code, out.String())
	}
}

func TestOutcome(t *testing.T) {
	// What a killed restic returns: no context error wrapped (issue #30).
	killed := errors.New("restoring snapshot: restic restore: signal: killed")
	tests := []struct {
		name    string
		ctxErr  error
		err     error
		want    jobs.State
		wantErr string // prefix of the error; "" for none
	}{
		{"success", nil, nil, jobs.StateSucceeded, ""},
		{"success after context ended", context.Canceled, nil, jobs.StateSucceeded, ""},
		{"failure", nil, killed, jobs.StateFailed, "restoring snapshot:"},
		{"cancelled, killed command", context.Canceled, killed, jobs.StateCancelled, "job cancelled: restoring snapshot:"},
		{"cancelled, wrapped", context.Canceled, fmt.Errorf("x: %w", context.Canceled), jobs.StateCancelled, "job cancelled: x:"},
		{"timeout, killed command", context.DeadlineExceeded, killed, jobs.StateFailed, "job timeout (6h0m0s) exceeded: restoring snapshot:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outcome(tt.ctxErr, tt.err, 6*time.Hour)
			if got != tt.want {
				t.Errorf("state = %q, want %q", got, tt.want)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.wantErr)):
				t.Errorf("err = %v, want prefix %q", err, tt.wantErr)
			}
		})
	}
}
