package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
)

// fakeGate records Acquire in the call list and returns err.
type fakeGate struct {
	calls *calls
	err   error
}

func (g *fakeGate) Acquire(ctx context.Context) error {
	g.calls.add("gate.Acquire")
	return g.err
}

func TestFileDowntimeGate(t *testing.T) {
	dir := t.TempDir()
	g := &FileDowntimeGate{Dir: dir, Poll: 5 * time.Millisecond}
	done := make(chan error, 1)
	// A goroutine is like Task.Run in C#; the channel carries its result.
	go func() { done <- g.Acquire(context.Background()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if requested, granted := DowntimeState(dir); requested && !granted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no downtime request written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Acquire returned before the grant: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := GrantDowntime(dir); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Cancelled while waiting.
	ctx, cancel := context.WithCancel(context.Background())
	other := &FileDowntimeGate{Dir: t.TempDir(), Poll: 5 * time.Millisecond}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := other.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Acquire: %v", err)
	}
}

// A backup asks for its downtime slot after the pre-stop snapshot and
// before it stops anything; waiting is not downtime.
func TestBackupDowntimeSlot(t *testing.T) {
	const (
		prestop = "restic.Backup tag=pre-stop parent= host=host-a"
		final   = "restic.Backup tag=final parent=snap-pre-stop host=host-a"
	)
	t.Run("granted", func(t *testing.T) {
		e := newEnv(t)
		e.p.Downtime = &fakeGate{calls: e.calls}
		if _, err := e.p.Backup(context.Background(), e.stack); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "docker.Services", prestop, "gate.Acquire",
			"docker.Stop", final, "docker.Start app,db", "docker.WaitHealthy"})
	})
	t.Run("cancelled while waiting: nothing stopped, nothing to restart", func(t *testing.T) {
		e := newEnv(t)
		e.p.Downtime = &fakeGate{calls: e.calls, err: context.Canceled}
		_, err := e.p.Backup(context.Background(), e.stack)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "was not stopped") {
			t.Fatalf("err = %v", err)
		}
		assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "docker.Services", prestop, "gate.Acquire"})
	})
	t.Run("nothing running: no slot needed", func(t *testing.T) {
		e := newEnv(t)
		e.p.Downtime = &fakeGate{calls: e.calls, err: errBoom}
		e.docker.services = []docker.ServiceState{{Name: "app", State: "exited", Defined: true}}
		if _, err := e.p.Backup(context.Background(), e.stack); err != nil {
			t.Fatal(err)
		}
		for _, c := range e.calls.list() {
			if c == "gate.Acquire" {
				t.Fatal("slot requested for a stopped stack")
			}
		}
	})
}

// The downtime timeout aborts the final snapshot; the stack is started
// again and the error says why.
func TestBackupDowntimeTimeout(t *testing.T) {
	e := newEnv(t)
	e.p.DowntimeTimeout = 20 * time.Millisecond
	e.restic.blockFinal = true
	_, err := e.p.Backup(context.Background(), e.stack)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "downtime timeout (20ms") ||
		!strings.Contains(err.Error(), "started again") {
		t.Fatalf("err = %v", err)
	}
	calls := e.calls.list()
	if calls[len(calls)-2] != "docker.Start app,db" || e.docker.startCtxErr != nil {
		t.Fatalf("stack not restarted with a live context: %v", calls)
	}
}

// An update waits for its slot like a backup; the downtime timeout ends it
// before anything was changed, so the old containers start again.
func TestUpdateDowntime(t *testing.T) {
	e := newEnv(t)
	e.p.Downtime = &fakeGate{calls: e.calls}
	e.p.DowntimeTimeout = 20 * time.Millisecond
	e.restic.blockFinal = true
	_, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true})
	if err == nil || !strings.Contains(err.Error(), "downtime timeout") || !strings.Contains(err.Error(), "the stack was not changed") {
		t.Fatalf("err = %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "docker.Services", "docker.Pull app,db", "docker.ConfiguredImages app,db",
		"restic.Backup tag=pre-stop parent= host=host-a", "gate.Acquire", "docker.Stop",
		"restic.Backup tag=final parent=snap-pre-stop host=host-a", "docker.Start app,db", "docker.WaitHealthy"})
}
