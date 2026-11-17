package jobs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/docker"
)

const newCompose = "services:\n  app:\n    image: nginx:1.27\n"

// composeOpts returns options that apply newCompose to e's stack.
func composeOpts(t *testing.T, e *env, backup bool) ComposeApplyOptions {
	t.Helper()
	current, err := os.ReadFile(e.stack.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	return ComposeApplyOptions{Content: []byte(newCompose), BaseSHA256: SHA256Hex(current), Backup: backup,
		PreviousCopy: filepath.Join(t.TempDir(), "compose.previous")}
}

func readCompose(t *testing.T, e *env) string {
	t.Helper()
	b, err := os.ReadFile(e.stack.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestComposeApply(t *testing.T) {
	e := newEnv(t)
	if err := os.Chmod(e.stack.ComposeFile, 0o640); err != nil {
		t.Fatal(err)
	}
	original := readCompose(t, e)
	// The new file keeps app and db, drops "tool" and adds "worker".
	e.docker.servicesAfter = []docker.ServiceState{{Name: "app"}, {Name: "db"}, {Name: "never"}, {Name: "worker"}}
	opts := composeOpts(t, e, true)
	res, err := e.p.ComposeApply(context.Background(), e.stack, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo", "docker.Services",
		"restic.Backup tag=pre-stop parent= host=host-a", "docker.Stop",
		"restic.Backup tag=final parent=snap-pre-stop host=host-a",
		"docker.Services", // services of the new file
		"docker.Up app,db,worker",
		"docker.WaitHealthy",
		"docker.Services", // images after the change
	})
	if res.Snapshot != "snap-final" || res.RolledBack {
		t.Fatalf("result %+v", res)
	}
	if got := readCompose(t, e); got != newCompose {
		t.Fatalf("compose file %q", got)
	}
	if info, _ := os.Stat(e.stack.ComposeFile); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", info.Mode().Perm())
	}
	if prev, _ := os.ReadFile(opts.PreviousCopy); string(prev) != original {
		t.Errorf("previous copy %q", prev)
	}
	entries, _ := os.ReadDir(e.stack.Dir)
	for _, en := range entries {
		if strings.Contains(en.Name(), ".stackvault-") {
			t.Errorf("temporary file left: %s", en.Name())
		}
	}
}

func TestComposeApplyRefusesChangedFile(t *testing.T) {
	e := newEnv(t)
	opts := composeOpts(t, e, true)
	if err := os.WriteFile(e.stack.ComposeFile, []byte("services: {changed: {}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := e.p.ComposeApply(context.Background(), e.stack, opts)
	if err == nil || !strings.Contains(err.Error(), "was changed after it was opened") {
		t.Fatalf("err = %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "docker.Services"})
	if got := readCompose(t, e); got != "services: {changed: {}}\n" {
		t.Fatalf("compose file %q", got)
	}
}

func TestComposeApplyStoppedStack(t *testing.T) {
	e := newEnv(t)
	e.docker.services = []docker.ServiceState{{Name: "app", State: "exited", ContainerID: "c1"}}
	_, err := e.p.ComposeApply(context.Background(), e.stack, composeOpts(t, e, true))
	if err != nil {
		t.Fatal(err)
	}
	// One snapshot of the stopped stack, the new file, nothing started.
	assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "docker.Services",
		"restic.Backup tag=final parent= host=host-a"})
	if got := readCompose(t, e); got != newCompose {
		t.Fatalf("compose file %q", got)
	}
}

func TestComposeApplyRollback(t *testing.T) {
	e := rollbackEnv(t)
	e.docker.upFailures = 1
	res, err := e.p.ComposeApply(context.Background(), e.stack, composeOpts(t, e, true))
	if err == nil || !res.RolledBack || !strings.Contains(err.Error(), "compose-apply failed and was rolled back") {
		t.Fatalf("result %+v, err %v", res, err)
	}
	// The restored snapshot brings back the compose file of the backup.
	if got := readCompose(t, e); got != "services:\n  app: {}\n" {
		t.Fatalf("compose file after rollback %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(res.OldDir, filepath.Base(e.stack.ComposeFile))); string(b) != newCompose {
		t.Fatalf("failed version should keep the new file, has %q", b)
	}
}

func TestComposeApplyRollbackInPlace(t *testing.T) {
	e := rollbackEnv(t)
	e.docker.upFailures = 1
	opts := composeOpts(t, e, true)
	opts.RollbackInPlace = true
	res, err := e.p.ComposeApply(context.Background(), e.stack, opts)
	if err == nil || !res.RolledBack || res.OldDir != "" || !strings.Contains(err.Error(), "rolled back in place") {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := readCompose(t, e); got != "services:\n  app: {}\n" {
		t.Fatalf("compose file after rollback %q", got)
	}
	assertNoSideDirs(t, e)
}

func TestComposeApplyWithoutBackup(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(e *env)
		wantErr   string
		wantCalls []string
		wantFile  string
	}{
		{"success", nil, "", []string{"docker.Services", "docker.Stop", "docker.Services", "docker.Up app,db", "docker.WaitHealthy",
			"docker.Services"}, newCompose},
		{"unhealthy: not rolled back", func(e *env) { e.docker.failOn["WaitHealthy"] = errBoom }, "not rolled back (no repository)",
			[]string{"docker.Services", "docker.Stop", "docker.Services", "docker.Up app,db", "docker.WaitHealthy"}, newCompose},
		{"stop fails: old containers restart, file unchanged", func(e *env) { e.docker.failOn["Stop"] = errBoom }, "stopping stack",
			[]string{"docker.Services", "docker.Stop", "docker.Start app,db", "docker.WaitHealthy"}, "services:\n  app: {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			_, err := e.p.ComposeApply(context.Background(), e.stack, composeOpts(t, e, false))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatal(err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
			if got := readCompose(t, e); got != tt.wantFile {
				t.Fatalf("compose file %q", got)
			}
		})
	}
}

func TestServicesAfterEdit(t *testing.T) {
	names := func(ns ...string) []docker.ServiceState {
		var out []docker.ServiceState
		for _, n := range ns {
			out = append(out, docker.ServiceState{Name: n})
		}
		return out
	}
	start, verify := servicesAfterEdit(names("app", "db", "job"), names("app", "job", "cache"), []string{"app", "db"})
	if !reflect.DeepEqual(start, []string{"app", "cache"}) || !reflect.DeepEqual(verify, []string{"app"}) {
		t.Fatalf("start %v, verify %v", start, verify)
	}
}

func TestComposeApplyRejectsBadSize(t *testing.T) {
	e := newEnv(t)
	for _, content := range [][]byte{nil, make([]byte, MaxComposeBytes+1)} {
		if _, err := e.p.ComposeApply(context.Background(), e.stack, ComposeApplyOptions{Content: content}); err == nil {
			t.Errorf("%d bytes accepted", len(content))
		}
	}
	if len(e.calls.list()) != 0 {
		t.Fatal("stack touched")
	}
}
