package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
)

// tickingClock returns a clock that moves one minute per call, so the
// recorded stop and start times differ.
func tickingClock() func() time.Time {
	now := fixedNow
	return func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
}

func TestStackDowntimeBackup(t *testing.T) {
	tests := []struct {
		name                     string
		setup                    func(e *env)
		wantStopped, wantStarted bool
	}{
		{"stopped and started again", nil, true, true},
		{"final snapshot fails: still started again", func(e *env) { e.restic.failOn["Backup:final"] = errBoom }, true, true},
		{"start fails: no start time", func(e *env) { e.docker.failOn["Start"] = errBoom }, true, false},
		{"stop fails: nothing recorded", func(e *env) { e.docker.failOn["Stop"] = errBoom }, false, false},
		{"stack was not running: no downtime", func(e *env) {
			e.docker.services = []docker.ServiceState{{Name: "app", State: "exited"}}
		}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.p.Now = tickingClock()
			if tt.setup != nil {
				tt.setup(e)
			}
			_, _ = e.p.Backup(context.Background(), e.stack)
			stopped, started := e.p.StackDowntime()
			if stopped.IsZero() == tt.wantStopped || started.IsZero() == tt.wantStarted {
				t.Fatalf("stopped %v, started %v", stopped, started)
			}
			if tt.wantStarted && !started.After(stopped) {
				t.Errorf("start %v not after stop %v", started, stopped)
			}
		})
	}
}

func TestStackDowntimeUpdateRollback(t *testing.T) {
	// The downtime runs from the update's stop to the rollback's start.
	e := rollbackEnv(t)
	e.p.Now = tickingClock()
	e.docker.upFailures = 1
	if _, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true}); err == nil {
		t.Fatal("update must fail")
	}
	stopped, started := e.p.StackDowntime()
	if stopped.IsZero() || !started.After(stopped) {
		t.Fatalf("stopped %v, started %v", stopped, started)
	}

	// A failed rollback leaves the stack down: no start time.
	e = rollbackEnv(t)
	e.docker.upFailures = 1
	e.restic.failOn["Restore"] = errBoom
	_, _ = e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true})
	if stopped, started := e.p.StackDowntime(); stopped.IsZero() || !started.IsZero() {
		t.Fatalf("failed rollback: stopped %v, started %v", stopped, started)
	}
}
