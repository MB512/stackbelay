package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
)

const testJobID = "01JCZ8G3T4S5V6W7X8Y9Z0ABCD"

func testEnv(t *testing.T) func(string) string {
	t.Helper()
	env := map[string]string{
		"STACKS_ROOT":          t.TempDir(),
		"STACKVAULT_HOSTNAME":  "h",
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
	env := map[string]string{"STACKS_ROOT": root, "STACKVAULT_HOSTNAME": "h"}
	getenv := func(k string) string { return env[k] }
	for _, args := range [][]string{{"backup", "web"}, {"update", "web"}, {"restore", "web", "0123abcd"}} {
		var out bytes.Buffer
		if code := run(args, getenv, &out); code != 1 || !strings.Contains(out.String(), "no repository configured") {
			t.Errorf("%v: code %d, output %s", args, code, out.String())
		}
	}
}
