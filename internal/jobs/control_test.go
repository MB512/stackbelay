package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStop(t *testing.T) {
	e := newEnv(t)
	if err := e.p.Stop(context.Background(), e.stack); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Stop"})

	e = newEnv(t)
	e.docker.failOn["Stop"] = errBoom
	if err := e.p.Stop(context.Background(), e.stack); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
}

func TestStart(t *testing.T) {
	// Every service with a container is started (app and db are running,
	// tool has exited), then "never", which is defined but has no
	// container (added while the stack was stopped), is created; each
	// with the health wait.
	e := newEnv(t)
	if err := e.p.Start(context.Background(), e.stack); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Services", "docker.Start app,db,tool", "docker.WaitHealthy",
		"docker.Up never", "docker.WaitHealthy"})

	// Creating the new service fails: the job fails (the others run).
	e2 := newEnv(t)
	e2.docker.failOn["Up"] = errBoom
	if err := e2.p.Start(context.Background(), e2.stack); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "creating new services") {
		t.Fatalf("failed creation: %v", err)
	}

	// Nothing new: only compose start.
	e.calls.log = nil
	e.docker.services = e.docker.services[:3]
	if err := e.p.Start(context.Background(), e.stack); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Services", "docker.Start app,db,tool", "docker.WaitHealthy"})

	// Unhealthy after the start: only a warning, like after a backup.
	e.calls.log = nil
	e.docker.failOn["WaitHealthy"] = errBoom
	if err := e.p.Start(context.Background(), e.stack); err != nil {
		t.Fatalf("unhealthy start failed the job: %v", err)
	}

	e.docker.failOn["Start"] = errBoom
	if err := e.p.Start(context.Background(), e.stack); !errors.Is(err, errBoom) {
		t.Fatalf("failed start: %v", err)
	}

	// No containers at all: nothing to start.
	e = newEnv(t)
	for i := range e.docker.services {
		e.docker.services[i].ContainerID = ""
	}
	err := e.p.Start(context.Background(), e.stack)
	if err == nil || !strings.Contains(err.Error(), "no containers") {
		t.Fatalf("start without containers: %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Services"})
}

func TestRestart(t *testing.T) {
	e := newEnv(t)
	if err := e.p.Restart(context.Background(), e.stack); err != nil {
		t.Fatal(err)
	}
	// Only the services that were running are started again.
	assertCalls(t, e.calls.list(), []string{"docker.Services", "docker.Stop", "docker.Start app,db", "docker.WaitHealthy"})
}

// A failing stop must still start the stack again.
func TestRestartStartsAfterFailedStop(t *testing.T) {
	e := newEnv(t)
	e.docker.failOn["Stop"] = errBoom
	err := e.p.Restart(context.Background(), e.stack)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Services", "docker.Stop", "docker.Start app,db", "docker.WaitHealthy"})
}

// A cancelled restart still starts with a context that is not cancelled.
func TestRestartCancelledStillStarts(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.docker.failOn["Stop"] = context.Canceled
	defer cancel()
	cancelOnStop := &cancelingDocker{fakeDocker: e.docker, cancel: cancel}
	e.p.Docker = cancelOnStop
	err := e.p.Restart(ctx, e.stack)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if calls := e.calls.list(); calls[len(calls)-2] != "docker.Start app,db" {
		t.Fatalf("stack was not started: %v", calls)
	}
	if e.docker.startCtxErr != nil {
		t.Fatalf("start ran with a cancelled context: %v", e.docker.startCtxErr)
	}
}

func TestRestartWithFailures(t *testing.T) {
	// Nothing running: refused before anything is stopped.
	e := newEnv(t)
	for i := range e.docker.services {
		e.docker.services[i].State = "exited"
	}
	err := e.p.Restart(context.Background(), e.stack)
	if err == nil || !strings.Contains(err.Error(), "use Start") {
		t.Fatalf("restart of a stopped stack: %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"docker.Services"})

	// The start fails: reported, the stop succeeded.
	e = newEnv(t)
	e.docker.failOn["Start"] = errBoom
	if err := e.p.Restart(context.Background(), e.stack); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "starting stack") {
		t.Fatalf("failed start: %v", err)
	}
}

// cancelingDocker cancels the job's context while the stack is stopped.
type cancelingDocker struct {
	*fakeDocker
	cancel context.CancelFunc
}

func (c *cancelingDocker) Stop(ctx context.Context, dir string) error {
	err := c.fakeDocker.Stop(ctx, dir)
	c.cancel()
	return err
}
