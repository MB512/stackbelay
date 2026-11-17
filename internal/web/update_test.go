package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
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

	// Only the update right shows "Update…" on the Status tab and opens the
	// update page; there is no Update tab.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, `action="/stacks/web/update"`) {
		t.Fatal("Update… without the update right")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/update", nil, nil), http.StatusForbidden, "")
	r := e.do(carol, http.MethodGet, "/stacks/web", nil, nil)
	if !strings.Contains(r.body, `<form method="get" action="/stacks/web/update"`) || strings.Contains(r.body, `>Update</a>`) {
		t.Fatalf("Update… missing, or an Update tab:\n%s", r.body)
	}
	r = e.do(carol, http.MethodGet, "/stacks/web/update", nil, nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{"<h1>Update web</h1>", "rolled back", `name="only_if_new" checked`, `name="pre_stop" checked`,
		`name="rollback" value="in-place"`, "No update check since the manager started"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("update page lacks %s", want)
		}
	}
	if strings.Contains(r.body, "confirm_no_backup") || strings.Contains(r.body, "data-confirm") {
		t.Fatal("no-backup confirmation with repository")
	}

	// Carol queues an update with backup; she may cancel it, alice may not.
	token := csrfOf(e, carol, "/stacks/web/update")
	r = e.do(carol, http.MethodPost, "/stacks/web/update",
		url.Values{"csrf_token": {token}, "only_if_new": {"on"}, "rollback": {"in-place"}}, nil) // pre_stop unchecked
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
		t.Fatalf("update: %d %q %s", r.status, r.location, r.body)
	}
	id := strings.TrimPrefix(r.location, jobLocation)
	j, err := e.st.JobByID(ctx, id)
	if err != nil || j.Type != jobs.TypeUpdate || j.Options.NoBackup || j.Requester != "carol" ||
		!j.Options.OnlyIfNew || !j.Options.RollbackInPlace || !j.Options.NoPreStop {
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
	if !strings.Contains(r.body, "not rolled back") || !strings.Contains(r.body, `name="confirm_no_backup"`) ||
		!strings.Contains(r.body, `data-confirm="Update db without backup?`) {
		t.Fatalf("update page without repository:\n%s", r.body)
	}
	adminToken := e.csrf("/stacks/db/update")
	r = e.post("/stacks/db/update", url.Values{"csrf_token": {adminToken}})
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "Confirm that") {
		t.Fatalf("unconfirmed: %d", r.status)
	}
	r = e.post("/stacks/db/update", url.Values{"csrf_token": {adminToken}, "confirm_no_backup": {"on"}, "rollback": {"in-place"}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("confirmed: %d %s", r.status, r.body)
	}
	j, err = e.st.JobByID(ctx, strings.TrimPrefix(r.location, jobLocation))
	if err != nil || j.Type != jobs.TypeUpdate || !j.Options.NoBackup || j.Options.RollbackInPlace || j.Options.OnlyIfNew {
		t.Fatalf("job without backup: %+v %v", j, err)
	}

	if n := auditActions(t, e.st)["job.create"]; n != 2 {
		t.Fatalf("job.create audited %d times", n)
	}
}

type fakeUpdates struct {
	dirs   []string
	checks []docker.ImageCheck
	err    error
}

func (f *fakeUpdates) CheckUpdates(_ context.Context, dir string) ([]docker.ImageCheck, error) {
	f.dirs = append(f.dirs, dir)
	return f.checks, f.err
}

func TestUpdateCheck(t *testing.T) {
	e, alice, _ := setupJobsEnv(t) // alice: backup right on web
	e.setupStatus()                // stack directories and containers
	e.server.Containers.(*fakeContainers).list = []docker.Container{
		{ID: "0000000000a1", Name: "app-1", Service: "app", State: "running"},
		{ID: "0000000000b1", Name: "db-1", Service: "db", State: "running"},
		{ID: "0000000000c1", Name: "tool-1", Service: "tool", State: "exited"},
	}
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	u := &fakeUpdates{checks: []docker.ImageCheck{
		{Service: "app", Image: "nextcloud:29", Running: true, Status: docker.UpdateAvailable,
			Current:   docker.ImageVersion{Version: "29.0.1", Digest: "sha256:aaaaaaaaaaaaaaaa"},
			Available: docker.ImageVersion{Version: "29.0.2", Created: created, Digest: "sha256:bbbbbbbbbbbbbbbb"}},
		{Service: "db", Image: "<b>postgres:16</b>", Running: true, Status: docker.UpToDate, Pinned: true,
			Note: "runs a version pinned by digest by a restore or rollback"},
		{Service: "tool", Status: docker.NotChecked, Note: "built locally (build:), not pulled by an update"},
	}}
	e.server.Updates = u

	// The check needs the update right and a CSRF token.
	aliceToken := csrfOf(e, alice, "/stacks/web")
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, "/update/check") {
		t.Fatal("check offered without the update right")
	}
	expect(t, e.do(alice, http.MethodPost, "/stacks/web/update/check", url.Values{"csrf_token": {aliceToken}}, nil),
		http.StatusForbidden, "")
	expect(t, e.post("/stacks/web/update/check", nil), http.StatusForbidden, "")
	if len(u.dirs) != 0 {
		t.Fatal("checked without permission or token")
	}

	// HTMX: the answer is the Status tab's fragment with the result.
	token := e.csrf("/stacks/web")
	htmx := map[string]string{"HX-Request": "true"}
	r := e.do(e.client, http.MethodPost, "/stacks/web/update/check", url.Values{"csrf_token": {token}}, htmx)
	expect(t, r, http.StatusOK, "")
	if !strings.HasPrefix(strings.TrimSpace(r.body), `<div id="status-live"`) {
		t.Fatalf("check answer is not the fragment:\n%s", r.body)
	}
	for _, want := range []string{"1 running service can be updated", "1 running service runs a version pinned",
		"running: 29.0.1 (sha256:aaaaaaaaaaaa)", "available: 29.0.2 (2026-01-02, sha256:bbbbbbbbbbbb)",
		"update available", "up to date", ">pinned</span>", "not checked", "built locally", "<th>Update</th>"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("check result lacks %q", want)
		}
	}
	if len(u.dirs) != 1 || u.dirs[0] != filepath.Join(e.server.StacksRoot, "web") {
		t.Fatalf("checked dirs %v", u.dirs)
	}

	// The result stays: alice (view right) sees it on the Status tab, the
	// update page shows the summary and the unpin box.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); !strings.Contains(r.body, "1 running service can be updated") {
		t.Fatalf("result not on alice's Status tab:\n%s", r.body)
	}
	r = e.get("/stacks/web/update")
	for _, want := range []string{"Last update check", "1 running service can be updated",
		"<code>db</code>\n      runs the image by digest", ">Unpin now</button>",
		`<input type="hidden" name="only_if_new" value="on">`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("update page lacks %q", want)
		}
	}

	// Without HTMX: redirect to the Status tab. Nothing pinned: no unpin box.
	u.checks[1].Pinned = false
	expect(t, e.post("/stacks/web/update/check", url.Values{"csrf_token": {token}}), http.StatusSeeOther, "/stacks/web")
	if r := e.get("/stacks/web/update"); strings.Contains(r.body, "Unpin now") {
		t.Fatal("unpin offered without pinned services")
	}

	u.err = errors.New("compose project is broken")
	r = e.do(e.client, http.MethodPost, "/stacks/web/update/check", url.Values{"csrf_token": {token}}, htmx)
	if !strings.Contains(r.body, "The check failed: compose project is broken") || strings.Contains(r.body, "<th>Update</th>") {
		t.Fatalf("check error not shown:\n%s", r.body)
	}

	// A check that cannot run keeps the last result and says why.
	e.server.Updates = nil
	r = e.do(e.client, http.MethodPost, "/stacks/web/update/check", url.Values{"csrf_token": {token}}, htmx)
	if !strings.Contains(r.body, "not available in this installation") || !strings.Contains(r.body, "compose project is broken") {
		t.Fatalf("check without checker:\n%s", r.body)
	}
	expect(t, e.post("/stacks/web/update/check", url.Values{"csrf_token": {token}}), http.StatusServiceUnavailable, "")
	if r := e.get("/stacks/web"); !strings.Contains(r.body, "disabled>Check for updates</button>") {
		t.Fatal("check button not disabled without checker")
	}
}

func TestImageVersion(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		v    docker.ImageVersion
		want string
	}{
		{docker.ImageVersion{}, ""},
		{docker.ImageVersion{Version: "1.2"}, "1.2"},
		{docker.ImageVersion{Created: created, Digest: "sha256:0123456789abcdef"}, "2026-01-02, sha256:0123456789ab"},
		{docker.ImageVersion{Version: "1.2", Digest: "sha256:01"}, "1.2 (sha256:01)"},
	} {
		if got := imageVersion(tt.v); got != tt.want {
			t.Errorf("imageVersion(%+v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}
