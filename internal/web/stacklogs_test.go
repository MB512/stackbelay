package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/store"
)

// fakeLogs implements StackLogs with fixed lines.
type fakeLogs struct {
	mu        sync.Mutex
	dirs      []string    // dirs passed to FollowLogs
	sinces    []time.Time // since values passed to FollowLogs
	lines     []docker.LogLine
	followErr error
	// together, if set (under mu), holds every FollowLogs call after its lines until
	// all calls counted in it have sent theirs, so a merged stream has the
	// lines of all stacks before the first one ends.
	together *sync.WaitGroup
}

func (f *fakeLogs) Containers(_ context.Context, dir string) ([]string, error) {
	return []string{"web-app-1", "web-db-1"}, nil
}

func (f *fakeLogs) FollowLogs(_ context.Context, dir string, _ int, since time.Time, fn func(docker.LogLine) error) error {
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.sinces = append(f.sinces, since)
	together := f.together
	f.mu.Unlock()
	for _, l := range f.lines {
		if err := fn(l); err != nil {
			return err
		}
	}
	if together != nil {
		together.Done()
		together.Wait()
	}
	return f.followErr
}

// setupLogs creates stack directories for web and db, and a fake log reader.
func (e *testEnv) setupLogs() *fakeLogs {
	e.t.Helper()
	root := e.t.TempDir()
	for _, name := range []string{"web", "db"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 11, 16, 16, 0, 1, 500, time.UTC)
	f := &fakeLogs{lines: []docker.LogLine{
		{Container: "web-app-1", Time: t0, Text: "started <b>bold</b>"},
		{Container: "web-db-1", Time: t0.Add(time.Second), Text: "ready", Cut: true},
		{Text: "web-app-1 exited with code 0"},
	}}
	e.server.StacksRoot = root
	e.server.Logs = f
	return f
}

func TestStackLogsAccess(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	fake := e.setupLogs()
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	ctx := context.Background()

	// Backup right only: the stack is visible, the Logs tab is not.
	if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: webID, CanBackup: true}); err != nil {
		t.Fatal(err)
	}
	r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil)
	if strings.Contains(r.body, `href="/stacks/web/logs"`) {
		t.Fatal("Logs tab shown without the logs right")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/logs", nil, nil), http.StatusForbidden, "")
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/logs/events", nil, nil), http.StatusForbidden, "")
	expect(t, e.do(alice, http.MethodGet, "/stacks/db/logs/events", nil, nil), http.StatusNotFound, "")
	expect(t, e.do(e.newClient(), http.MethodGet, "/stacks/web/logs/events", nil, nil), http.StatusSeeOther, "/login")

	// With the logs right: tab, page with one checkbox per container.
	if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: webID, CanLogs: true}); err != nil {
		t.Fatal(err)
	}
	r = e.do(alice, http.MethodGet, "/stacks/web", nil, nil)
	if !strings.Contains(r.body, `href="/stacks/web/logs"`) {
		t.Fatal("Logs tab missing")
	}
	r = e.do(alice, http.MethodGet, "/stacks/web/logs", nil, nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{`value="web-app-1" checked`, `value="web-db-1" checked`,
		`data-events="/stacks/web/logs/events"`, `/static/stacklog.js`, `aria-current="page">Logs`,
		`id="log-wrap" checked`, `id="log-fullscreen"`, `id="log-bar" hidden`, `id="log-exit-fullscreen"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("logs page lacks %s", want)
		}
	}

	// The stream: escaped lines with container and time id, then "end".
	r = e.do(alice, http.MethodGet, "/stacks/web/logs/events", nil, nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{
		"id: 2026-11-16T16:00:01.000000500Z\nevent: line\ndata: <div class=\"line\" data-c=\"web-app-1\" data-t=\"2026-11-16T16:00:01.000000500Z\">",
		"started &lt;b&gt;bold&lt;/b&gt;",
		"[line cut]",
		"event: line\ndata: <div class=\"line\" data-c=\"\">web-app-1 exited with code 0</div>",
		"event: end\n",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("stream lacks %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "<b>bold") {
		t.Fatal("log text not escaped")
	}

	// A reconnect resumes after the last line and is not audited again.
	r = e.do(alice, http.MethodGet, "/stacks/web/logs/events?since=2026-11-16T16:00:02.000000500Z", nil, nil)
	expect(t, r, http.StatusOK, "")
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/logs/events?since=yesterday", nil, nil), http.StatusBadRequest, "")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sinces) != 2 || !fake.sinces[0].IsZero() ||
		!fake.sinces[1].Equal(time.Date(2026, 11, 16, 16, 0, 2, 501, time.UTC)) {
		t.Fatalf("since values: %v", fake.sinces)
	}
	if fake.dirs[0] != filepath.Join(e.server.StacksRoot, "web") {
		t.Fatalf("dir %s", fake.dirs[0])
	}
	if n := auditActions(t, e.st)["stack.logs"]; n != 1 {
		t.Fatalf("stack.logs audited %d times, want 1", n)
	}
}

func TestStackLogsErrorsAndUnavailable(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	fake := e.setupLogs()
	e.loginAdmin()

	fake.followErr = errors.New("compose project \"web\" is also used by containers from another directory")
	r := e.get("/stacks/web/logs/events")
	if !strings.Contains(r.body, "event: failed\ndata: compose project") || strings.Contains(r.body, "event: end") {
		t.Fatalf("stream:\n%s", r.body)
	}

	// Stack directory gone: the page says so, the stream refuses.
	if err := os.RemoveAll(filepath.Join(e.server.StacksRoot, "db")); err != nil {
		t.Fatal(err)
	}
	r = e.get("/stacks/db/logs")
	if !strings.Contains(r.body, "The stack directory is missing.") || !strings.Contains(r.body, "data-unavailable") {
		t.Fatalf("missing stack:\n%s", r.body)
	}
	expect(t, e.get("/stacks/db/logs/events"), http.StatusServiceUnavailable, "")

	e.server.Logs = nil
	r = e.get("/stacks/web/logs")
	if !strings.Contains(r.body, "Container logs are not available") {
		t.Fatalf("no log reader:\n%s", r.body)
	}
}

func TestStreamLimiter(t *testing.T) {
	var l streamLimiter
	for i := 0; i < maxLogStreamsPerUser; i++ {
		if !l.acquire(1, maxLogStreamsPerUser, maxLogStreams) {
			t.Fatalf("stream %d refused", i)
		}
	}
	if l.acquire(1, maxLogStreamsPerUser, maxLogStreams) {
		t.Fatal("per-user limit not enforced")
	}
	l.release(1)
	if !l.acquire(1, maxLogStreamsPerUser, maxLogStreams) {
		t.Fatal("released stream not reusable")
	}
	for u := int64(2); l.total < maxLogStreams; u++ {
		if !l.acquire(u, maxLogStreamsPerUser, maxLogStreams) {
			t.Fatalf("user %d refused below the total limit", u)
		}
	}
	if l.acquire(999, maxLogStreamsPerUser, maxLogStreams) {
		t.Fatal("total limit not enforced")
	}
}

// Line ids have a fixed width, so the browser can order them as strings,
// and they parse back as ?since=.
func TestLogLineID(t *testing.T) {
	times := []time.Time{
		time.Date(2026, 11, 17, 8, 40, 21, 0, time.UTC),
		time.Date(2026, 11, 17, 8, 40, 21, 5, time.UTC),
		time.Date(2026, 11, 17, 8, 40, 21, 500_000_000, time.UTC),
		time.Date(2026, 11, 17, 9, 40, 22, 0, time.FixedZone("CET", 3600)), // 08:40:22 UTC
	}
	prev := ""
	for _, tm := range times {
		id := logLineID(tm)
		if len(id) != len("2026-11-17T08:40:21.000000000Z") || id <= prev {
			t.Fatalf("id %q (after %q)", id, prev)
		}
		back, err := time.Parse(time.RFC3339Nano, id)
		if err != nil || !back.Equal(tm) {
			t.Fatalf("parse %q: %v, %v", id, back, err)
		}
		prev = id
	}
	if logLineID(time.Time{}) != "" {
		t.Fatal("id for a line without time")
	}
}
