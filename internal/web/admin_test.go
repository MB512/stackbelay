package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

func TestAdminJobs(t *testing.T) {
	e := newEnv(t, true, true)
	web, db := e.setupStacks()
	ctx := context.Background()
	add := func(id string, stack int64, typ jobs.Type) {
		t.Helper()
		if err := e.st.CreateJob(ctx, store.Job{ID: id, StackID: stack, Type: typ, Trigger: store.TriggerSchedule,
			QueuedAt: e.clock()}); err != nil {
			t.Fatal(err)
		}
	}
	add("job-1", web, jobs.TypeBackup)
	add("job-2", db, jobs.TypeBackup)
	add("job-3", web, jobs.TypeUpdate)
	// job-1 runs; job-2 and job-3 wait.
	if _, ok, err := e.st.ClaimNextJob(ctx, 1, e.clock().Add(time.Second)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	e.loginAdmin()
	r := e.get("/")
	if !strings.Contains(r.body, `<a href="/admin/users">Admin</a>`) {
		t.Errorf("no Admin link in the header:\n%s", r.body)
	}
	r = e.get("/admin/users")
	if !strings.Contains(r.body, `<a href="/admin/users" aria-current="page">Users</a>`) ||
		!strings.Contains(r.body, `<a href="/admin/jobs">Jobs</a>`) {
		t.Errorf("admin tabs missing on the users page:\n%s", r.body)
	}

	for _, path := range []string{"/admin/jobs", "/admin/jobs/live"} {
		r = e.get(path)
		expect(t, r, http.StatusOK, "")
		queue, recent, ok := strings.Cut(r.body, "<h2>Recent jobs</h2>")
		if !ok {
			t.Fatalf("%s: no recent jobs:\n%s", path, r.body)
		}
		// Running first, then the queued jobs, oldest first.
		i1, i2, i3 := strings.Index(queue, "/jobs/job-1"), strings.Index(queue, "/jobs/job-2"), strings.Index(queue, "/jobs/job-3")
		if i1 < 0 || i2 < i1 || i3 < i2 {
			t.Errorf("%s: queue order wrong (%d, %d, %d):\n%s", path, i1, i2, i3, queue)
		}
		if !strings.Contains(queue, `<a href="/stacks/db">db</a>`) || strings.Count(queue, "waiting") != 2 {
			t.Errorf("%s: queue rows wrong:\n%s", path, queue)
		}
		if strings.Count(recent, "/jobs/job-") != 3 {
			t.Errorf("%s: recent jobs wrong:\n%s", path, recent)
		}
	}
	if r := e.get("/admin/jobs/live"); strings.Contains(r.body, "<html") || !strings.Contains(r.body, `id="admin-jobs-live"`) {
		t.Errorf("live answer is not the fragment:\n%s", r.body)
	}

	// Not for other users, and not in their header.
	alice, _ := e.createUser("alice")
	for _, path := range []string{"/admin/jobs", "/admin/jobs/live"} {
		expect(t, e.do(alice, http.MethodGet, path, nil, nil), http.StatusForbidden, "")
	}
	if r := e.do(alice, http.MethodGet, "/", nil, nil); strings.Contains(r.body, "/admin/") {
		t.Errorf("admin link shown to a user:\n%s", r.body)
	}
}

func TestAdminAudit(t *testing.T) {
	e := newEnv(t, true, true)
	web, _ := e.setupStacks()
	ctx := context.Background()
	e.loginAdmin()
	admin, err := e.st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddAudit(ctx, store.AuditEntry{UserID: admin.ID, Action: "test.stack", StackID: web,
		Detail: "<b>detail</b>"}); err != nil {
		t.Fatal(err)
	}
	r := e.get("/admin/audit")
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{`aria-current="page">Audit log</a>`, "<td>admin</td>", "<code>test.stack</code>",
		`<a href="/stacks/web">web</a>`, "&lt;b&gt;detail&lt;/b&gt;", "<code>login.success</code>"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("audit page lacks %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "Older entries") || strings.Contains(r.body, "Newest entries") {
		t.Errorf("paging links on a single page:\n%s", r.body)
	}

	// Pages of 100: fill up, then follow "Older entries" to the end.
	for i := range auditPageSize + 10 {
		if err := e.st.AddAudit(ctx, store.AuditEntry{Action: "test.fill", Detail: "fill " + itoa(int64(i))}); err != nil {
			t.Fatal(err)
		}
	}
	total, _ := e.st.ListAudit(ctx, 10000)
	seen, path := 0, "/admin/audit"
	for pages := 0; path != ""; pages++ {
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		r := e.get(path)
		expect(t, r, http.StatusOK, "")
		seen += strings.Count(r.body, "<tr>") - 1 // header row
		if pages > 0 && !strings.Contains(r.body, `<a href="/admin/audit">Newest entries</a>`) {
			t.Errorf("%s: no link to the newest entries", path)
		}
		path = ""
		if m := olderLink.FindStringSubmatch(r.body); m != nil {
			path = m[1]
		}
	}
	if seen != len(total) {
		t.Errorf("paged through %d entries, want %d", seen, len(total))
	}
	expect(t, e.get("/admin/audit?before=x"), http.StatusBadRequest, "")

	alice, _ := e.createUser("alice")
	expect(t, e.do(alice, http.MethodGet, "/admin/audit", nil, nil), http.StatusForbidden, "")
}

var olderLink = regexp.MustCompile(`<a href="(/admin/audit\?before=\d+)">Older entries</a>`)
