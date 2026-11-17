package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// fakeContainers implements ContainerLister.
type fakeContainers struct {
	list []docker.Container
	err  error
	dirs []string
}

func (f *fakeContainers) StackContainers(_ context.Context, dir string) ([]docker.Container, error) {
	f.dirs = append(f.dirs, dir)
	return f.list, f.err
}

// setupStatus creates the stack directories (see setupLogs) and a fake
// container list for the Status tab.
func (e *testEnv) setupStatus() *fakeContainers {
	e.t.Helper()
	e.setupLogs()
	f := &fakeContainers{list: []docker.Container{
		{ID: "0000000000a1", Name: "app-1", Service: "app", State: "running", Health: "healthy",
			Status: "Up 2 hours (healthy)", Image: "nginx:1.27"},
		{ID: "0000000000b1", Name: "db-1", Service: "db", State: "exited", Status: "Exited (0) 3 minutes ago",
			Image: "postgres:17"},
	}}
	e.server.Containers = f
	return f
}

func TestStatusTab(t *testing.T) {
	e := newEnv(t, true, true)
	web, _ := e.setupStacks()
	f := e.setupStatus()
	if err := e.st.CreateJob(context.Background(), store.Job{ID: "job-1", StackID: web, Type: jobs.TypeBackup,
		Trigger: store.TriggerSchedule, QueuedAt: e.clock()}); err != nil {
		t.Fatal(err)
	}
	// A user without backup right (restore only: any grant includes
	// viewing) sees services and jobs, but not "Back up now".
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: web, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{
		`aria-current="page">Status`, `href="/stacks/web/backups"`,
		"app-1", "Up 2 hours (healthy)", `<span class="badge ok">healthy</span>`, "nginx:1.27",
		"db-1", `<span class="badge">exited</span>`,
		"schedule", // the job's trigger
		`hx-get="/stacks/web/status/live"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Status tab lacks %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "Back up now") {
		t.Error("backup action without backup right")
	}
	if len(f.dirs) != 1 || !strings.HasSuffix(f.dirs[0], "/web") {
		t.Errorf("containers listed for %v", f.dirs)
	}

	// The poll returns only the fragment.
	r = e.do(alice, http.MethodGet, "/stacks/web/status/live", nil, map[string]string{"HX-Request": "true", pollHeader: "1"})
	expect(t, r, http.StatusOK, "")
	if !strings.HasPrefix(strings.TrimSpace(r.body), `<div id="status-live"`) || strings.Contains(r.body, "<html") {
		t.Errorf("poll answer:\n%s", r.body)
	}

	// Stacks the user may not view: 404 for page and poll.
	expect(t, e.do(alice, http.MethodGet, "/stacks/db", nil, nil), http.StatusNotFound, "")
	expect(t, e.do(alice, http.MethodGet, "/stacks/db/status/live", nil, nil), http.StatusNotFound, "")

	// A failing container list is shown, the page still works.
	f.err = errors.New("compose ps failed")
	r = e.do(alice, http.MethodGet, "/stacks/web", nil, nil)
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "Listing the containers failed: compose ps failed") {
		t.Errorf("no error shown:\n%s", r.body)
	}
}

func TestStatusTabWithoutContainerList(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.loginAdmin()
	r := e.get("/stacks/web")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "The container list is not available in this installation.") {
		t.Errorf("Status tab:\n%s", r.body)
	}
}

// The periodic refresh does not extend the session; an expired session
// answers HTMX requests with HX-Redirect instead of the login page.
func TestStatusPollDoesNotExtendSession(t *testing.T) {
	e := newEnv(t, true, true) // SessionTTL: 1 hour
	e.setupStacks()
	e.setupStatus()
	e.loginAdmin()
	poll := map[string]string{"HX-Request": "true", pollHeader: "1"}

	e.advance(40 * time.Minute) // past half the TTL: a normal request would extend it
	expect(t, e.do(e.client, http.MethodGet, "/stacks/web/status/live", nil, poll), http.StatusOK, "")
	e.advance(25 * time.Minute) // 65 minutes after login
	r := e.do(e.client, http.MethodGet, "/stacks/web/status/live", nil, poll)
	if r.status != http.StatusUnauthorized || r.header.Get("HX-Redirect") != "/login" {
		t.Fatalf("expired poll: status %d, HX-Redirect %q", r.status, r.header.Get("HX-Redirect"))
	}

	// A normal request does extend the session.
	e.loginAdmin2()
	e.advance(40 * time.Minute)
	expect(t, e.get("/stacks/web"), http.StatusOK, "")
	e.advance(25 * time.Minute)
	expect(t, e.do(e.client, http.MethodGet, "/stacks/web/status/live", nil, poll), http.StatusOK, "")
}

// loginAdmin2 logs the default client in again with the changed password.
func (e *testEnv) loginAdmin2() {
	e.t.Helper()
	expect(e.t, e.login(e.client, "admin", adminNewPW), http.StatusSeeOther, "/")
}

// The Logs link of a container opens the Logs tab with only that
// container selected; it needs the logs right.
func TestStatusLogsLink(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.setupStatus() // also a fake log reader: web-app-1, web-db-1
	e.loginAdmin()
	r := e.get("/stacks/web")
	if !strings.Contains(r.body, `href="/stacks/web/logs?container=app-1"`) {
		t.Fatalf("no Logs link:\n%s", r.body)
	}
	r = e.get("/stacks/web/logs?container=web-db-1")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `value="web-db-1" checked>`) || !strings.Contains(r.body, `value="web-app-1">`) {
		t.Fatalf("preselection:\n%s", r.body)
	}
	// An unknown container selects all.
	r = e.get("/stacks/web/logs?container=nope")
	if !strings.Contains(r.body, `value="web-db-1" checked>`) || !strings.Contains(r.body, `value="web-app-1" checked>`) {
		t.Fatalf("unknown container:\n%s", r.body)
	}

	alice, aliceID := e.createUser("alice")
	web, _ := e.st.StackByName(context.Background(), "web")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: web.ID, CanBackup: true}); err != nil {
		t.Fatal(err)
	}
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, "/logs?") {
		t.Fatal("Logs link without the logs right")
	}
}

// Every page loads static/forms.js (confirmations, busy buttons); slow
// forms name what they are doing while the button is busy.
func TestBusyForms(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.setupLogs() // stack directories with a compose file (compose editor)
	e.loginAdmin()
	for path, wants := range map[string][]string{
		"/stacks/web":          {`<script src="/static/forms.js" defer>`, `<span class="hint busy-text">asking the registries…</span>`},
		"/stacks/web/backups":  {`<span class="hint busy-text">reading the repository…</span>`},
		"/stacks/web/settings": {`data-busy="Saving and checking access…"`, `data-busy="Initializing…"`},
		"/stacks/web/compose":  {`data-busy="Validating…"`},
		"/":                    {`<script src="/static/forms.js" defer>`},
	} {
		r := e.get(path)
		for _, want := range wants {
			if !strings.Contains(r.body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
	if r := e.get("/static/forms.js"); r.status != http.StatusOK || !strings.Contains(r.body, "data-confirm") {
		t.Fatalf("forms.js: %d", r.status)
	}
}
