package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// fakeLister answers the cleanup preview; it has no removal methods, like
// the manager's real one.
type fakeLister struct {
	images  []docker.DanglingImage
	orphans map[string][]docker.OrphanContainer // by stack name
}

func (f *fakeLister) DanglingImages(context.Context) ([]docker.DanglingImage, error) {
	return f.images, nil
}

func (f *fakeLister) StackOrphans(_ context.Context, dir string) ([]docker.OrphanContainer, error) {
	return f.orphans[filepath.Base(dir)], nil
}

func TestHostTab(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	keep := "sha256:" + strings.Repeat("k", 64)
	old := "sha256:" + strings.Repeat("a", 64)
	lister := &fakeLister{
		images: []docker.DanglingImage{
			{ID: old, LastTagTime: now.Add(-30 * 24 * time.Hour), Size: 3 << 20, RepoDigests: []string{"app@sha256:1234"}},
			{ID: keep, LastTagTime: now.Add(-30 * 24 * time.Hour), Size: 1 << 20},
		},
		orphans: map[string][]docker.OrphanContainer{"web": {
			{ID: strings.Repeat("1", 64), Name: "web-old-1", Service: "old", State: "exited"},
			{ID: strings.Repeat("2", 64), Name: "web-cron-1", Service: "cron", State: "running"},
		}},
	}
	e := newEnvWith(t, true, true, func(s *Server) {
		s.Cleanup, s.CleanupImageMinAge = lister, 7*24*time.Hour
	})
	webID, _ := e.setupStacks()
	root := t.TempDir()
	for _, name := range []string{"web", "db"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.server.StacksRoot = root
	ctx := context.Background()
	// An update replaced "keep" yesterday: the preview keeps it.
	if err := e.st.CreateJob(ctx, store.Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", StackID: webID, Type: jobs.TypeUpdate,
		Trigger: store.TriggerManual, QueuedAt: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1", jobs.StateSucceeded, "", "",
		store.JobResult{ReplacedImages: []string{keep}}, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()

	r := e.get("/admin/host")
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{`aria-current="page">Host</a>`, "more than 7 days ago", ">Clean up now</button>",
		"No cleanups yet."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("host tab lacks %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "cleanup-preview") {
		t.Error("preview shown without asking")
	}

	r = e.get("/admin/host?preview=1")
	if !strings.Contains(r.body, `id="cleanup-preview"`) {
		t.Fatalf("no preview:\n%s", r.body)
	}
	preview, _, _ := strings.Cut(r.body[strings.Index(r.body, `id="cleanup-preview"`):], "Recent cleanups")
	for _, want := range []string{"web-old-1", "web-cron-1", "keep: running", "app@sha256:1234",
		"keep: replaced by an update", "Removable: 3.0 MiB", `<code title="` + old + `">aaaaaaaaaaaa</code>`} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, preview)
		}
	}
	if n := strings.Count(preview, `badge warn">remove`); n != 2 {
		t.Errorf("%d rows marked remove, want 2 (stopped orphan, old image)", n)
	}

	// Clean up now: a host job, audited, admins only.
	token := e.csrf("/admin/host")
	r = e.post("/admin/host/cleanup", url.Values{"csrf_token": {token}})
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, "/jobs/") {
		t.Fatalf("clean up now: %d %q", r.status, r.location)
	}
	id := strings.TrimPrefix(r.location, "/jobs/")
	j, err := e.st.JobByID(ctx, id)
	if err != nil || j.Type != jobs.TypeCleanup || j.StackID != 0 || j.Trigger != store.TriggerManual {
		t.Fatalf("job %+v, %v", j, err)
	}
	r = e.get(r.location)
	if !strings.Contains(r.body, "Host cleanup") || strings.Contains(r.body, `href="/stacks/"`) {
		t.Errorf("job page:\n%s", r.body)
	}
	// A second one while the first waits is refused.
	r = e.post("/admin/host/cleanup", url.Values{"csrf_token": {token}})
	if r.status != http.StatusConflict || !strings.Contains(r.body, "already waiting") {
		t.Errorf("second cleanup: %d", r.status)
	}
	if r := e.get("/admin/jobs"); !strings.Contains(r.body, `<span class="muted">host</span>`) {
		t.Errorf("host job not on the Jobs tab:\n%s", r.body)
	}
	if err := e.st.FinishJob(ctx, id, jobs.StateSucceeded, "", "", store.JobResult{RemovedContainers: 1, RemovedImages: 2, FreedBytes: 3 << 20}, now); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/admin/host"); !strings.Contains(r.body, "1 container, 2 images (3.0 MiB)") {
		t.Errorf("recent cleanup result missing:\n%s", r.body)
	}

	// Host schedules.
	expect(t, e.post("/admin/host/schedules", url.Values{"csrf_token": {token}, "cron_expr": {"0 4 * * 0"}}),
		http.StatusSeeOther, "/admin/host")
	scheds, _ := e.st.ListHostSchedules(ctx)
	if len(scheds) != 1 || scheds[0].Type != jobs.TypeCleanup || scheds[0].CronExpr != "0 4 * * 0" {
		t.Fatalf("host schedules %+v", scheds)
	}
	r = e.post("/admin/host/schedules", url.Values{"csrf_token": {token}, "cron_expr": {"nope"}})
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, `value="nope"`) {
		t.Errorf("bad cron: %d", r.status)
	}
	sid := itoa(scheds[0].ID)
	expect(t, e.post("/admin/host/schedules/"+sid+"/enabled", url.Values{"csrf_token": {token}, "enabled": {"false"}}),
		http.StatusSeeOther, "/admin/host")
	// A stack's schedule cannot be changed through the host routes.
	stackSched, err := e.st.CreateSchedule(ctx, store.Schedule{StackID: webID, CronExpr: "@daily", Type: jobs.TypeBackup})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, e.post("/admin/host/schedules/"+itoa(stackSched)+"/delete", url.Values{"csrf_token": {token}}), http.StatusNotFound, "")
	expect(t, e.post("/admin/host/schedules/"+sid+"/delete", url.Values{"csrf_token": {token}}), http.StatusSeeOther, "/admin/host")
	if scheds, _ := e.st.ListHostSchedules(ctx); len(scheds) != 0 {
		t.Fatalf("host schedule not deleted: %+v", scheds)
	}
	actions := auditActions(t, e.st)
	for _, a := range []string{"schedule.create", "schedule.disable", "schedule.delete", "job.create"} {
		if actions[a] == 0 {
			t.Errorf("no %s audit entry", a)
		}
	}

	// Not for other users: the tab, the actions and the host job's page.
	alice, _ := e.createUser("alice")
	aliceToken := csrfOf(e, alice, "/")
	expect(t, e.do(alice, http.MethodGet, "/admin/host", nil, nil), http.StatusForbidden, "")
	for _, p := range []string{"/admin/host/cleanup", "/admin/host/schedules"} {
		expect(t, e.do(alice, http.MethodPost, p, url.Values{"csrf_token": {aliceToken}, "cron_expr": {"@daily"}}, nil),
			http.StatusForbidden, "")
	}
	expect(t, e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil), http.StatusNotFound, "")
}
