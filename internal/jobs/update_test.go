package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/docker"
)

func TestUpdate(t *testing.T) {
	const (
		check   = "restic.CheckRepo"
		svcs    = "docker.Services"
		pull    = "docker.Pull app,db"
		prestop = "restic.Backup tag=pre-stop parent= host=host-a"
		final   = "restic.Backup tag=final parent=snap-pre-stop host=host-a"
		stop    = "docker.Stop"
		up      = "docker.Up app,db"
		healthy = "docker.WaitHealthy"
		start   = "docker.Start app,db" // old containers again
	)
	tests := []struct {
		name      string
		opts      UpdateOptions
		setup     func(e *env)
		wantCalls []string
		wantRes   ChangeResult
		wantErr   string // substring; "" = success
	}{
		{
			name:      "with backup",
			opts:      UpdateOptions{Backup: true},
			wantCalls: []string{check, svcs, pull, prestop, stop, final, up, healthy},
			wantRes:   ChangeResult{PreStopSnapshot: "snap-pre-stop", Snapshot: "snap-final"},
		},
		{
			name:      "without repository: no snapshots",
			wantCalls: []string{svcs, pull, stop, up, healthy},
		},
		{
			name:      "pull fails: nothing is stopped",
			opts:      UpdateOptions{Backup: true},
			setup:     func(e *env) { e.docker.failOn["Pull"] = errBoom },
			wantCalls: []string{check, svcs, pull},
			wantErr:   "pulling images (the stack was not changed)",
		},
		{
			name:      "repository unreachable: nothing happens",
			opts:      UpdateOptions{Backup: true},
			setup:     func(e *env) { e.restic.failOn["CheckRepo"] = errBoom },
			wantCalls: []string{check},
			wantErr:   "repository not reachable",
		},
		{
			name:      "pre-stop snapshot fails: continue without parent",
			opts:      UpdateOptions{Backup: true},
			setup:     func(e *env) { e.restic.failOn["Backup:pre-stop"] = errBoom },
			wantCalls: []string{check, svcs, pull, prestop, stop, "restic.Backup tag=final parent= host=host-a", up, healthy},
			wantRes:   ChangeResult{Snapshot: "snap-final"},
		},
		{
			name:      "final snapshot fails after stop: old containers restart, no up",
			opts:      UpdateOptions{Backup: true},
			setup:     func(e *env) { e.restic.failOn["Backup:final"] = errBoom },
			wantCalls: []string{check, svcs, pull, prestop, stop, final, start, healthy},
			wantRes:   ChangeResult{PreStopSnapshot: "snap-pre-stop"},
			wantErr:   "final snapshot (the stack was not changed)",
		},
		{
			name:      "stop fails: old containers restart",
			setup:     func(e *env) { e.docker.failOn["Stop"] = errBoom },
			wantCalls: []string{svcs, pull, stop, start, healthy},
			wantErr:   "stopping stack",
		},
		{
			name:      "up fails without repository: no rollback, old version not started",
			setup:     func(e *env) { e.docker.upFailures = 1 },
			wantCalls: []string{svcs, pull, stop, up},
			wantErr:   "not rolled back (no repository)",
		},
		{
			name:      "unhealthy without repository: no rollback",
			setup:     func(e *env) { e.docker.failOn["WaitHealthy"] = errBoom },
			wantCalls: []string{svcs, pull, stop, up, healthy},
			wantErr:   "not rolled back (no repository)",
		},
		{
			name: "nothing running: refused before pulling",
			opts: UpdateOptions{Backup: true},
			setup: func(e *env) {
				e.docker.services = []docker.ServiceState{{Name: "app", State: "exited"}}
			},
			wantCalls: []string{check, svcs},
			wantErr:   "no service of the stack is running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			res, err := e.p.Update(context.Background(), e.stack, tt.opts)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if res != tt.wantRes {
				t.Errorf("result = %+v, want %+v", res, tt.wantRes)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
		})
	}
}

// rollbackEnv is an update environment whose final snapshot can be
// restored (its id is a valid snapshot id known to the fake repository).
func rollbackEnv(t *testing.T) *env {
	e := restoreEnv(t)
	e.restic.ids = map[string]string{TagFinal: snapID}
	return e
}

func TestUpdateRollback(t *testing.T) {
	head := []string{
		"restic.CheckRepo", "docker.Services", "docker.Pull app,db",
		"restic.Backup tag=pre-stop parent= host=host-a", "docker.Stop",
		"restic.Backup tag=final parent=snap-pre-stop host=host-a",
		"docker.Up app,db",
	}
	restore := []string{
		"restic.CheckRepo", "restic.Snapshots " + snapID, "docker.Services", "docker.Stop",
		// no safety snapshot: the final snapshot is the state before the update
		"restic.Restore " + snapID + ":/home/docker/web",
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.Up app", // services running at snapshot time, per its metadata.txt
		"docker.WaitHealthy",
	}
	tests := []struct {
		name      string
		setup     func(e *env)
		wantCalls []string
	}{
		{"up fails", func(e *env) { e.docker.upFailures = 1 }, append(append([]string{}, head...), restore...)},
		{"unhealthy", func(e *env) { e.docker.failOn["WaitHealthy"] = errBoom },
			append(append(append([]string{}, head...), "docker.WaitHealthy"), restore...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := rollbackEnv(t)
			tt.setup(e)
			res, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true})
			if err == nil || !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "rolled back to snapshot "+snapID[:8]) {
				t.Fatalf("err = %v", err)
			}
			wantOld := e.stack.Dir + ".old-20261113T100000Z"
			if !res.RolledBack || res.OldDir != wantOld || res.Snapshot != snapID {
				t.Fatalf("result %+v", res)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
			if got := readData(t, e.stack.Dir); got != "restored" {
				t.Errorf("stack dir has %q after rollback", got)
			}
			assertNoStaging(t, e)
		})
	}
}

func TestUpdateRollbackFails(t *testing.T) {
	e := rollbackEnv(t)
	e.docker.upFailures = 1
	e.restic.failOn["Restore"] = errBoom
	res, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true})
	if err == nil || !strings.Contains(err.Error(), "rollback to snapshot "+snapID[:8]+" failed") {
		t.Fatalf("err = %v", err)
	}
	if res.RolledBack {
		t.Fatal("reported as rolled back")
	}
	if got := readData(t, e.stack.Dir); got != "original" {
		t.Errorf("stack dir has %q", got)
	}
	assertNoStaging(t, e)
}

func TestUpdateCancelledBeforeUpRestartsOldContainers(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.restic.cancelOnFinal = cancel
	_, err := e.p.Update(ctx, e.stack, UpdateOptions{Backup: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	calls := e.calls.list()
	if calls[len(calls)-2] != "docker.Start app,db" || e.docker.startCtxErr != nil {
		t.Fatalf("old containers not restarted with a live context: %v", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "docker.Up") {
			t.Fatal("cancelled update still ran up")
		}
	}
}

func TestUpdateCancelledDuringUpStillRollsBack(t *testing.T) {
	e := rollbackEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.docker.upFailures = 1
	e.docker.onUp = cancel // the job is cancelled while "up" runs
	res, err := e.p.Update(ctx, e.stack, UpdateOptions{Backup: true})
	if err == nil || !res.RolledBack {
		t.Fatalf("rollback did not run after cancellation: %+v, %v", res, err)
	}
}
