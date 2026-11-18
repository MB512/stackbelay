package web

import (
	"context"
	"net/http"
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
