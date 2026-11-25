package jobs

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/MB512/stackbelay/internal/restic"
)

// lockedErr is restic's error for a repository locked by another process
// (exit code 11), as restic.CLI returns it.
func lockedErr(t *testing.T) error {
	t.Helper()
	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 11").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return &restic.Error{Op: "backup", Err: ee, Stderr: "repository is already locked exclusively"}
}

// A repository that stays locked (e.g. a prune on the server, issue #46)
// ends a backup or update before the stack is stopped: the final snapshot
// would wait for the same lock with the stack down.
func TestLockedRepositoryStopsNothing(t *testing.T) {
	const (
		check    = "restic.CheckRepo"
		services = "docker.Services"
		prestop  = "restic.Backup tag=pre-stop parent= host=host-a"
	)
	t.Run("backup", func(t *testing.T) {
		e := newEnv(t)
		e.restic.failOn["Backup:pre-stop"] = lockedErr(t)
		_, err := e.p.Backup(context.Background(), e.stack)
		if !restic.IsLocked(err) {
			t.Fatalf("err = %v", err)
		}
		assertCalls(t, e.calls.list(), []string{check, services, prestop})
	})
	t.Run("update", func(t *testing.T) {
		e := newEnv(t)
		e.restic.failOn["Backup:pre-stop"] = lockedErr(t)
		res, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true})
		if !restic.IsLocked(err) || res.RolledBack {
			t.Fatalf("err = %v, res %+v", err, res)
		}
		for _, c := range e.calls.list() {
			if c == "docker.Stop" {
				t.Fatal("the stack was stopped")
			}
		}
	})
	t.Run("other pre-stop failures still continue", func(t *testing.T) {
		e := newEnv(t)
		e.restic.failOn["Backup:pre-stop"] = errBoom
		if _, err := e.p.Backup(context.Background(), e.stack); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}
