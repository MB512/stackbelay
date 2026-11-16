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

	// Only the update right shows the tab and opens it.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, `href="/stacks/web/update"`) {
		t.Fatal("Update tab without the update right")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/update", nil, nil), http.StatusForbidden, "")
	r := e.do(carol, http.MethodGet, "/stacks/web", nil, nil)
	if !strings.Contains(r.body, `href="/stacks/web/update"`) || strings.Contains(r.body, `action="/stacks/web/update"`) {
		t.Fatalf("Update tab missing, or update form still on the Backups tab:\n%s", r.body)
	}
	r = e.do(carol, http.MethodGet, "/stacks/web/update", nil, nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{`aria-current="page">Update`, "rolled back", `name="only_if_new" checked`,
		`name="rollback" value="in-place"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("update tab lacks %s", want)
		}
	}
	if strings.Contains(r.body, "confirm_no_backup") {
		t.Fatal("no-backup confirmation with repository")
	}

	// Carol queues an update with backup; she may cancel it, alice may not.
	token := csrfOf(e, carol, "/stacks/web/update")
	r = e.do(carol, http.MethodPost, "/stacks/web/update",
		url.Values{"csrf_token": {token}, "only_if_new": {"on"}, "rollback": {"in-place"}}, nil)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
		t.Fatalf("update: %d %q %s", r.status, r.location, r.body)
	}
	id := strings.TrimPrefix(r.location, jobLocation)
	j, err := e.st.JobByID(ctx, id)
	if err != nil || j.Type != jobs.TypeUpdate || j.Options.NoBackup || j.Requester != "carol" ||
		!j.Options.OnlyIfNew || !j.Options.RollbackInPlace {
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
	e, alice, _ := setupJobsEnv(t)
	e.setupLogs() // stack directories
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	u := &fakeUpdates{checks: []docker.ImageCheck{
		{Service: "app", Image: "nextcloud:29", Running: true, Status: docker.UpdateAvailable,
			Current:   docker.ImageVersion{Version: "29.0.1", Digest: "sha256:aaaaaaaaaaaaaaaa"},
			Available: docker.ImageVersion{Version: "29.0.2", Created: created, Digest: "sha256:bbbbbbbbbbbbbbbb"}},
		{Service: "db", Image: "<b>postgres:16</b>", Running: true, Status: docker.UpToDate},
		{Service: "tool", Status: docker.NotChecked, Note: "built locally (build:), not pulled by an update"},
	}}
	e.server.Updates = u

	// The check needs the update right and a CSRF token.
	aliceToken := csrfOf(e, alice, "/stacks/web")
	expect(t, e.do(alice, http.MethodPost, "/stacks/web/update/check", url.Values{"csrf_token": {aliceToken}}, nil),
		http.StatusForbidden, "")
	expect(t, e.post("/stacks/web/update/check", nil), http.StatusForbidden, "")
	if len(u.dirs) != 0 {
		t.Fatal("checked without permission or token")
	}

	r := e.post("/stacks/web/update/check", url.Values{"csrf_token": {e.csrf("/stacks/web/update")}})
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{"1 running service can be updated", "29.0.1 (sha256:aaaaaaaaaaaa)",
		"29.0.2 (2026-01-02, sha256:bbbbbbbbbbbb)", "update available", "up to date", "not checked",
		"built locally", "&lt;b&gt;postgres:16&lt;/b&gt;", `name="only_if_new"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("check result lacks %q", want)
		}
	}
	if len(u.dirs) != 1 || u.dirs[0] != filepath.Join(e.server.StacksRoot, "web") {
		t.Fatalf("checked dirs %v", u.dirs)
	}

	u.err = errors.New("compose project is broken")
	r = e.post("/stacks/web/update/check", url.Values{"csrf_token": {e.csrf("/stacks/web/update")}})
	if !strings.Contains(r.body, "The check failed: compose project is broken") {
		t.Fatalf("check error not shown:\n%s", r.body)
	}

	e.server.Updates = nil
	r = e.get("/stacks/web/update")
	if strings.Contains(r.body, "/update/check") || !strings.Contains(r.body, "not available in this installation") {
		t.Fatal("check offered without checker")
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
