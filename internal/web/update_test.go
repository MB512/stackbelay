package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

func TestUpdateJob(t *testing.T) {
	e, alice, _ := setupJobsEnv(t) // alice: backup right on web
	ctx := context.Background()
	carol, carolID := e.createUser("carol")
	web, err := e.st.StackByName(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetPermission(ctx, store.Permission{UserID: carolID, StackID: web.ID, CanUpdate: true}); err != nil {
		t.Fatal(err)
	}

	// Only the update right shows the section and opens the page.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, `action="/stacks/web/update"`) {
		t.Fatal("update section without the update right")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/update", nil, nil), http.StatusForbidden, "")
	r := e.do(carol, http.MethodGet, "/stacks/web", nil, nil)
	if !strings.Contains(r.body, `action="/stacks/web/update"`) || !strings.Contains(r.body, "automatic rollback") {
		t.Fatalf("update section:\n%s", r.body)
	}
	r = e.do(carol, http.MethodGet, "/stacks/web/update", nil, nil)
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "rolled back") || strings.Contains(r.body, "confirm_no_backup") {
		t.Fatalf("update page with repository:\n%s", r.body)
	}

	// Carol queues an update with backup; she may cancel it, alice may not.
	token := csrfOf(e, carol, "/stacks/web/update")
	r = e.do(carol, http.MethodPost, "/stacks/web/update", url.Values{"csrf_token": {token}}, nil)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
		t.Fatalf("update: %d %q %s", r.status, r.location, r.body)
	}
	id := strings.TrimPrefix(r.location, jobLocation)
	j, err := e.st.JobByID(ctx, id)
	if err != nil || j.Type != jobs.TypeUpdate || j.Options.NoBackup || j.Requester != "carol" {
		t.Fatalf("job: %+v %v", j, err)
	}
	if r := e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil); strings.Contains(r.body, "/cancel") {
		t.Fatal("alice may cancel an update")
	}
	r = e.do(carol, http.MethodGet, "/jobs/"+id, nil, nil)
	if !strings.Contains(r.body, "<h1>Update of") || !strings.Contains(r.body, "/cancel") {
		t.Fatalf("job page for carol:\n%s", r.body)
	}

	// A rolled-back update says so on its page.
	if err := e.st.FinishJob(ctx, id, jobs.StateFailed, "aaaaaaaa11111111", "update failed and was rolled back",
		store.JobResult{RolledBack: true, PreviousDir: "/s/web.old-x"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r = e.do(carol, http.MethodGet, "/jobs/"+id, nil, nil)
	if !strings.Contains(r.body, "restored to snapshot <code>aaaaaaaa</code>") || !strings.Contains(r.body, "Failed version") {
		t.Fatalf("rolled-back job page:\n%s", r.body)
	}

	// Without repository (db, admin): warning, explicit confirmation required.
	r = e.get("/stacks/db/update")
	if !strings.Contains(r.body, "not rolled back") || !strings.Contains(r.body, `name="confirm_no_backup"`) {
		t.Fatalf("update page without repository:\n%s", r.body)
	}
	adminToken := e.csrf("/stacks/db/update")
	r = e.post("/stacks/db/update", url.Values{"csrf_token": {adminToken}})
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "Confirm that") {
		t.Fatalf("unconfirmed: %d", r.status)
	}
	r = e.post("/stacks/db/update", url.Values{"csrf_token": {adminToken}, "confirm_no_backup": {"on"}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("confirmed: %d %s", r.status, r.body)
	}
	j, err = e.st.JobByID(ctx, strings.TrimPrefix(r.location, jobLocation))
	if err != nil || j.Type != jobs.TypeUpdate || !j.Options.NoBackup {
		t.Fatalf("job without backup: %+v %v", j, err)
	}

	if n := auditActions(t, e.st)["job.create"]; n != 2 {
		t.Fatalf("job.create audited %d times", n)
	}
}
