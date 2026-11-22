//go:build integration

// Integration test against a real restic binary and a local repository.
// Run with: go test -tags integration ./internal/restic/
package restic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIIntegration(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not in PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	pwFile := filepath.Join(tmp, "pw")
	if err := os.WriteFile(pwFile, []byte("integration-test-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := Repo{Location: repoDir, Env: []string{"RESTIC_PASSWORD_FILE=" + pwFile}}
	c := &CLI{}

	// Repo does not exist yet.
	var rerr *Error
	if err := c.CheckRepo(ctx, repo); !errors.As(err, &rerr) || !IsNoRepository(err) {
		t.Fatalf("expected a no-repository *Error for missing repo, got %v", err)
	}
	if _, err := c.Snapshots(ctx, repo); !IsNoRepository(err) {
		t.Fatalf("snapshots on missing repo: want IsNoRepository, got %v", err)
	}
	if err := c.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// A second init is refused and is not "no repository".
	if err := c.Init(ctx, repo); err == nil || IsNoRepository(err) {
		t.Fatalf("second init: want a refusal, got %v", err)
	}
	if err := c.CheckRepo(ctx, Repo{Location: repoDir, Env: []string{"RESTIC_PASSWORD=wrong-pw"}}); err == nil || IsNoRepository(err) {
		t.Fatalf("wrong password: want an error that is not IsNoRepository, got %v", err)
	}

	stack := filepath.Join(tmp, "stacks", "web")
	if err := os.MkdirAll(filepath.Join(stack, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stack, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := c.Backup(ctx, repo, stack, BackupOptions{Host: "test-host", Tags: []string{"final"}})
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := c.Snapshots(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Hostname != "test-host" || snaps[0].Paths[0] != stack {
		t.Fatalf("unexpected snapshots %+v", snaps)
	}

	if err := os.Chmod(stack, 0o750); err != nil {
		t.Fatal(err)
	}
	id, err = c.Backup(ctx, repo, stack, BackupOptions{Host: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := c.Node(ctx, repo, id, stack)
	if err != nil {
		t.Fatal(err)
	}
	if node.Type != "dir" || node.Path != stack || node.Mode.Perm() != 0o750 || int(node.UID) != os.Getuid() {
		t.Fatalf("unexpected node %+v", node)
	}

	target := filepath.Join(tmp, "restore")
	if err := c.Restore(ctx, repo, id, snaps[0].Paths[0], target, nil); err != nil {
		t.Fatal(err)
	}
	// Contents land directly in target, not under target/<abs path>.
	got, err := os.ReadFile(filepath.Join(target, "compose.yml"))
	if err != nil || !strings.Contains(string(got), "services") {
		t.Fatalf("restored compose.yml: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(target, "data")); err != nil {
		t.Fatal(err)
	}
}

// Restore over an existing directory rewrites changed files, deletes files
// that are not in the snapshot and leaves excluded paths alone.
func TestCLIRestoreInPlace(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not in PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	pwFile := filepath.Join(tmp, "pw")
	if err := os.WriteFile(pwFile, []byte("integration-test-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := Repo{Location: filepath.Join(tmp, "repo"), Env: []string{"RESTIC_PASSWORD_FILE=" + pwFile}}
	c := &CLI{}
	if err := c.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	stack := filepath.Join(tmp, "stacks", "web")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(stack, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yml", "services: {}\n")
	write("data/db", "v1")
	write("data/jobs/J1/log.txt", "old log")
	id, err := c.Backup(ctx, repo, stack, BackupOptions{Host: "test-host"})
	if err != nil {
		t.Fatal(err)
	}

	write("data/db", "v2 longer")
	write("data/new-file", "created after the snapshot")
	write("data/jobs/J1/log.txt", "current log")
	write("data/jobs/J2/status.json", "{}")
	write(".stackbelay-restore-incomplete", "")
	if err := c.Restore(ctx, repo, id, stack, stack, []string{"data/jobs", ".stackbelay-restore-incomplete"}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"data/db":                        "v1",
		"data/jobs/J1/log.txt":           "current log", // excluded: untouched
		"data/jobs/J2/status.json":       "{}",          // excluded: not deleted
		".stackbelay-restore-incomplete": "",            // excluded file: not deleted
	} {
		if got, err := os.ReadFile(filepath.Join(stack, rel)); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v; want %q", rel, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(stack, "data", "new-file")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file not in the snapshot was not deleted: %v", err)
	}

	for _, bad := range []string{"", ".", "/data", "../x", "data/*", "a/./b", "x[1]"} {
		if err := c.Restore(ctx, repo, id, stack, stack, []string{bad}); err == nil {
			t.Errorf("exclude %q accepted", bad)
		}
	}
}
