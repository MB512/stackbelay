package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MB512/stackvault/internal/store"
)

func TestGlobalLogs(t *testing.T) {
	e := newEnv(t, true, true)
	webID, dbID := e.setupStacks()
	fake := e.setupLogs()
	ctx := context.Background()
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	bob, bobID := e.createUser("bob")
	for _, p := range []store.Permission{
		{UserID: aliceID, StackID: webID, CanLogs: true},
		{UserID: aliceID, StackID: dbID, CanBackup: true},
		{UserID: bobID, StackID: webID, CanBackup: true},
	} {
		if err := e.st.SetPermission(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	get := func(c *http.Client, path string) response { return e.do(c, http.MethodGet, path, nil, nil) }

	// The header link only for users with the logs right somewhere.
	if r := get(alice, "/"); !strings.Contains(r.body, `<a href="/logs">Logs</a>`) {
		t.Errorf("no Logs link for alice:\n%s", r.body)
	}
	if r := get(bob, "/"); strings.Contains(r.body, `href="/logs"`) {
		t.Errorf("Logs link for bob:\n%s", r.body)
	}
	if r := get(bob, "/logs"); !strings.Contains(r.body, "You may not read the logs of any stack.") {
		t.Errorf("bob's page:\n%s", r.body)
	}

	// alice chooses from the stacks with her logs right only.
	r := get(alice, "/logs")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `name="stack" value="web">`) || strings.Contains(r.body, `value="db"`) ||
		strings.Contains(r.body, `id="stack-logs"`) {
		t.Errorf("alice's selection:\n%s", r.body)
	}
	r = get(alice, "/logs?stack=web")
	for _, want := range []string{`name="stack" value="web" checked>`, `data-events="/logs/events?stack=web"`,
		`value="web/web-app-1" checked`, `value="web/web-db-1" checked`, "/static/stacklog.js", "the last 100 lines"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("alice's view lacks %s:\n%s", want, r.body)
		}
	}
	for _, path := range []string{"/logs?stack=db", "/logs?stack=nope", "/logs?stack=../web",
		"/logs/events?stack=db", "/logs/events?stack=web&stack=db"} {
		expect(t, get(alice, path), http.StatusNotFound, "")
	}
	expect(t, get(alice, "/logs/events"), http.StatusBadRequest, "")
	expect(t, get(alice, "/logs?stack=web&stack=web&stack=web&stack=web&stack=web"), http.StatusBadRequest, "")
	expect(t, get(e.newClient(), "/logs/events?stack=web"), http.StatusSeeOther, "/login")

	// The admin follows both stacks in one stream: lines of both, keyed
	// by stack and container, then "end".
	var together sync.WaitGroup
	together.Add(2)
	fake.mu.Lock()
	fake.together = &together
	fake.mu.Unlock()
	r = e.get("/logs/events?stack=web&stack=db")
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{
		`data-c="web/web-app-1" data-t="2026-11-16T16:00:01.000000500Z"><span class="ts">`,
		`<span class="ctr">db/web-db-1</span> ready`,
		`data-c=""><span class="ctr">web</span> web-app-1 exited with code 0</div>`,
		"event: end\n",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("stream lacks %q:\n%s", want, r.body)
		}
	}
	fake.mu.Lock()
	fake.together = nil
	fake.mu.Unlock()
	// A reconnect resumes both and is not audited again.
	expect(t, e.get("/logs/events?stack=web&stack=db&since=2026-11-16T16:00:02.000000500Z"), http.StatusOK, "")
	fake.mu.Lock()
	if len(fake.dirs) != 4 || len(fake.sinces) != 4 || fake.sinces[2].IsZero() || fake.sinces[3].IsZero() {
		t.Errorf("FollowLogs calls: %v %v", fake.dirs, fake.sinces)
	}
	fake.mu.Unlock()
	entries, _ := e.st.ListAudit(ctx, 100)
	global := 0
	for _, a := range entries {
		if a.Action == "stack.logs" && a.Detail == "global log view" {
			global++
		}
	}
	if global != 2 {
		t.Errorf("global view audited %d times, want 2 (one per stack)", global)
	}

	// A stack whose directory is gone: named on the page, left out of the
	// stream; the stream itself refuses it.
	if err := os.RemoveAll(filepath.Join(e.server.StacksRoot, "db")); err != nil {
		t.Fatal(err)
	}
	r = e.get("/logs?stack=web&stack=db")
	for _, want := range []string{"db: the stack directory is missing.", `data-events="/logs/events?stack=web"`,
		`value="db" checked disabled>`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("view with a missing stack lacks %s:\n%s", want, r.body)
		}
	}
	expect(t, e.get("/logs/events?stack=web&stack=db"), http.StatusServiceUnavailable, "")
	r = e.get("/logs?stack=db")
	if !strings.Contains(r.body, "None of the chosen stacks has a stack directory.") || !strings.Contains(r.body, "data-unavailable") {
		t.Errorf("view of a missing stack only:\n%s", r.body)
	}

	// Every stack is one stream: with all 4 in use, the view is refused.
	admin, err := e.st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !e.server.logStreams.acquireN(admin.ID, maxLogStreamsPerUser, maxLogStreamsPerUser, maxLogStreams) {
		t.Fatal("reserving streams failed")
	}
	expect(t, e.get("/logs/events?stack=web"), http.StatusTooManyRequests, "")
	e.server.logStreams.releaseN(admin.ID, maxLogStreamsPerUser)
	if e.server.logStreams.total != 0 {
		t.Errorf("streams still reserved: %d", e.server.logStreams.total)
	}
}

func TestStreamLimiterN(t *testing.T) {
	var l streamLimiter
	if !l.acquireN(1, 3, 4, 5) || l.acquireN(1, 2, 4, 5) || !l.acquireN(1, 1, 4, 5) {
		t.Fatal("per-user limit with n wrong")
	}
	if l.acquireN(2, 2, 4, 5) || !l.acquireN(2, 1, 4, 5) {
		t.Fatal("total limit with n wrong")
	}
	l.releaseN(1, 4)
	l.releaseN(2, 1)
	if l.total != 0 || len(l.byUser) != 0 {
		t.Fatalf("not released: %d %v", l.total, l.byUser)
	}
}
