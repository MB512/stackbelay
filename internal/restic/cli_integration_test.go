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
	if err := c.CheckRepo(ctx, repo); !errors.As(err, &rerr) {
		t.Fatalf("expected *Error for missing repo, got %v", err)
	}
	init := exec.Command("restic", "init")
	init.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repoDir, "RESTIC_PASSWORD_FILE="+pwFile)
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("restic init: %v: %s", err, out)
	}
	if err := c.CheckRepo(ctx, repo); err != nil {
		t.Fatal(err)
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

	target := filepath.Join(tmp, "restore")
	if err := c.Restore(ctx, repo, id, snaps[0].Paths[0], target); err != nil {
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
