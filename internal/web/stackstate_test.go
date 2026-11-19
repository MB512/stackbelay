package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

func TestClassifyStack(t *testing.T) {
	run := func(svc string) docker.Container {
		return docker.Container{Name: svc + "-1", Service: svc, State: "running"}
	}
	exited := func(svc string, code int) docker.Container {
		return docker.Container{Name: svc + "-1", Service: svc, State: "exited", ExitCode: code}
	}
	tests := []struct {
		name       string
		services   []string
		containers []docker.Container
		want       string // label
		wantDetail string // substring of the tooltip
	}{
		{"no containers", []string{"app"}, nil, "not created", ""},
		{"all running", []string{"app", "db"}, []docker.Container{run("app"), run("db")}, "started", ""},
		{"healthy and starting count as running", []string{"app", "db"},
			[]docker.Container{{Service: "app", State: "running", Health: "healthy"}, {Service: "db", State: "running", Health: "starting"}},
			"started", ""},
		{"one-shot service done", []string{"app", "init"}, []docker.Container{run("app"), exited("init", 0)}, "started", ""},
		{"stopped (exit codes of a stop)", []string{"app", "db"}, []docker.Container{exited("app", 0), exited("db", 137)}, "stopped", ""},
		{"created, never started", []string{"app"}, []docker.Container{{Name: "app-1", Service: "app", State: "created"}}, "stopped", ""},
		{"service crashed", []string{"app", "db"}, []docker.Container{run("app"), exited("db", 1)}, "degraded", "db is not running"},
		{"service without container", []string{"app", "db"}, []docker.Container{run("app")}, "degraded", "db is not running"},
		{"unhealthy", []string{"app"}, []docker.Container{{Name: "app-1", Service: "app", State: "running", Health: "unhealthy"}},
			"degraded", "app-1 is unhealthy"},
		{"restarting", []string{"app"}, []docker.Container{{Name: "app-1", Service: "app", State: "restarting"}},
			"degraded", "app-1 is restarting"},
		{"scaled service with one running replica", []string{"app"},
			[]docker.Container{run("app"), {Name: "app-2", Service: "app", State: "exited", ExitCode: 2}}, "started", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStack(docker.Overview{Services: tt.services, Containers: tt.containers})
			if got.Label != tt.want || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("got %+v, want %q with detail %q", got, tt.want, tt.wantDetail)
			}
		})
	}

	legacy := fmt.Errorf("%w: containers web_app_1", docker.ErrLegacyCompose)
	if got := overviewState(docker.Overview{}, legacy); got.Label != "legacy (compose v1)" || !strings.Contains(got.Detail, "web_app_1") {
		t.Errorf("legacy: %+v", got)
	}
	if got := overviewState(docker.Overview{}, errors.New("daemon down")); got.Label != "unknown" || !strings.Contains(got.Detail, "daemon down") {
		t.Errorf("error: %+v", got)
	}
}

func TestStateCache(t *testing.T) {
	var c stateCache
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var loads atomic.Int32
	release := make(chan struct{})
	load := func() stackState {
		loads.Add(1)
		<-release
		return stateStarted
	}

	// Concurrent requests for the same stack share one load.
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if got := c.get(ctx, "/s/web", clock, load); got != stateStarted {
				t.Errorf("got %+v", got)
			}
		})
	}
	for loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Fatalf("%d loads, want 1", n)
	}

	// Cached within the TTL, loaded again after it.
	c.get(ctx, "/s/web", clock, load)
	if n := loads.Load(); n != 1 {
		t.Fatalf("%d loads within the TTL", n)
	}
	now = now.Add(stateCacheTTL)
	c.get(ctx, "/s/web", clock, load)
	if n := loads.Load(); n != 2 {
		t.Fatalf("%d loads after the TTL", n)
	}

	// A cancelled request returns at once; the load goes on for others.
	blocked := make(chan struct{})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	got := c.get(cctx, "/s/db", clock, func() stackState { <-blocked; return stateStopped })
	if got.Label != "unknown" {
		t.Fatalf("cancelled: %+v", got)
	}
	close(blocked)
	if got := c.get(ctx, "/s/db", clock, load); got != stateStopped {
		t.Fatalf("after the cancelled request: %+v", got)
	}
}

func TestStackStateCell(t *testing.T) {
	e := newEnv(t, true, true)
	web, _ := e.setupStacks()
	f := e.setupStatus() // app running and healthy, db exited with code 0
	e.loginAdmin()
	ctx := context.Background()

	// The list loads the cell lazily and has the refresh button.
	r := e.get("/")
	for _, want := range []string{`hx-get="/stacks/web/state" hx-trigger="load"`, `hx-select="#stack-rows" hx-target="#stack-rows"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("stack list lacks %s:\n%s", want, r.body)
		}
	}

	r = e.get("/stacks/web/state")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `<span class="badge ok">started</span>`) {
		t.Fatalf("state cell: %s", r.body)
	}

	// Cached: a second request within the TTL does not ask docker again.
	e.get("/stacks/web/state")
	if f.overviews != 1 {
		t.Fatalf("%d docker calls, want 1", f.overviews)
	}

	// A running job makes the stack busy, without asking docker.
	if err := e.st.ImportJob(ctx, store.Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", StackID: web, Type: jobs.TypeBackup,
		Trigger: store.TriggerSchedule, State: jobs.StateRunning, QueuedAt: e.clock(), StartedAt: e.clock()}, ""); err != nil {
		t.Fatal(err)
	}
	r = e.get("/stacks/web/state")
	if !strings.Contains(r.body, `<a href="/jobs/01AAAAAAAAAAAAAAAAAAAAAAA1" title="A backup job is running."><span class="badge run">busy: backup</span></a>`) {
		t.Fatalf("busy: %s", r.body)
	}

	// Legacy and errors (db is not cached yet).
	f.err = fmt.Errorf("%w: containers db_app_1", docker.ErrLegacyCompose)
	if r = e.get("/stacks/db/state"); !strings.Contains(r.body, `>legacy (compose v1)</span>`) {
		t.Fatalf("legacy: %s", r.body)
	}

	// A missing directory needs no request: the list shows it directly.
	if err := e.st.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	r = e.get("/")
	if !strings.Contains(r.body, `>missing</span>`) || strings.Contains(r.body, `hx-get="/stacks/db/state"`) {
		t.Fatalf("missing stack in list:\n%s", r.body)
	}
	if r = e.get("/stacks/db/state"); !strings.Contains(r.body, `>missing</span>`) {
		t.Fatalf("missing: %s", r.body)
	}

	// Users without view right get nothing.
	alice, _ := e.createUser("alice")
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/state", nil, nil), http.StatusNotFound, "")
}
