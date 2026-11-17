package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

func TestControlJobs(t *testing.T) {
	e, alice, _ := setupJobsEnv(t) // alice: backup right on web; db has no repository
	ctx := context.Background()
	carol, carolID := e.createUser("carol")
	web, _ := e.st.StackByName(ctx, "web")
	db, _ := e.st.StackByName(ctx, "db")
	for _, st := range []int64{web.ID, db.ID} {
		if err := e.st.SetPermission(ctx, store.Permission{UserID: carolID, StackID: st, CanOperate: true}); err != nil {
			t.Fatal(err)
		}
	}

	// Only the operate right shows the buttons and may use them.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, "/restart") || strings.Contains(r.body, "/stop") {
		t.Fatal("control buttons without the operate right")
	}
	aliceToken := csrfOf(e, alice, "/stacks/web")
	for _, a := range []string{"stop", "start", "restart"} {
		expect(t, e.do(alice, http.MethodPost, "/stacks/web/"+a, url.Values{"csrf_token": {aliceToken}}, nil), http.StatusForbidden, "")
	}
	r := e.do(carol, http.MethodGet, "/stacks/web", nil, nil)
	for _, want := range []string{`action="/stacks/web/restart"`, `action="/stacks/web/stop"`, `action="/stacks/web/start"`,
		`data-confirm="Stop web?`, "/static/confirm.js"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Status tab of carol lacks %q", want)
		}
	}
	if strings.Contains(r.body, "Back up now") {
		t.Error("backup button for carol")
	}
	if r := e.do(carol, http.MethodGet, "/", nil, nil); !strings.Contains(r.body, "operate") {
		t.Error("stack list does not show the operate right")
	}

	// Each action queues a job of its type, without repository need (db),
	// with CSRF token only.
	carolToken := csrfOf(e, carol, "/stacks/web")
	expect(t, e.do(carol, http.MethodPost, "/stacks/web/stop", nil, nil), http.StatusForbidden, "")
	for _, tc := range []struct {
		stack string
		typ   jobs.Type
	}{{"web", jobs.TypeStop}, {"db", jobs.TypeStart}, {"db", jobs.TypeRestart}} {
		r := e.do(carol, http.MethodPost, "/stacks/"+tc.stack+"/"+string(tc.typ), url.Values{"csrf_token": {carolToken}}, nil)
		if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
			t.Fatalf("%s %s: %d %q %s", tc.typ, tc.stack, r.status, r.location, r.body)
		}
		id := strings.TrimPrefix(r.location, jobLocation)
		j, err := e.st.JobByID(ctx, id)
		if err != nil || j.Type != tc.typ || j.StackName != tc.stack || j.Requester != "carol" {
			t.Fatalf("job: %+v %v", j, err)
		}
		// Carol (operate right) may cancel it, alice (backup right) not.
		r = e.do(carol, http.MethodGet, "/jobs/"+id, nil, nil)
		title := map[jobs.Type]string{jobs.TypeStop: "Stop", jobs.TypeStart: "Start", jobs.TypeRestart: "Restart"}[tc.typ]
		if !strings.Contains(r.body, "<h1>"+title+" of") || !strings.Contains(r.body, "/cancel") {
			t.Fatalf("job page for carol:\n%s", r.body)
		}
		if tc.stack == "web" {
			if r := e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil); strings.Contains(r.body, "/cancel") {
				t.Fatal("alice may cancel a stop")
			}
		}
	}
	if n := auditActions(t, e.st)["job.create"]; n != 3 {
		t.Fatalf("job.create audited %d times", n)
	}
}

// The manager's own stack cannot be stopped from the UI; restart works.
func TestControlOwnStack(t *testing.T) {
	e, _, _ := setupJobsEnv(t)
	e.server.OwnStack = "web"
	r := e.get("/stacks/web")
	if !strings.Contains(r.body, `disabled title="This is StackVault&#39;s own stack`) ||
		!strings.Contains(r.body, `action="/stacks/web/restart"`) {
		t.Fatalf("Status tab of the own stack:\n%s", r.body)
	}
	token := e.csrf("/stacks/web")
	r = e.post("/stacks/web/stop", url.Values{"csrf_token": {token}})
	if r.status != http.StatusConflict || !strings.Contains(r.body, "own stack") {
		t.Fatalf("stop of the own stack: %d", r.status)
	}
	if r := e.post("/stacks/web/restart", url.Values{"csrf_token": {token}}); r.status != http.StatusSeeOther {
		t.Fatalf("restart of the own stack: %d %s", r.status, r.body)
	}
	// Other stacks can be stopped.
	if r := e.get("/stacks/db"); strings.Contains(r.body, "own stack") {
		t.Fatal("db marked as own stack")
	}
	if r := e.post("/stacks/db/stop", url.Values{"csrf_token": {token}}); r.status != http.StatusSeeOther {
		t.Fatalf("stop of db: %d", r.status)
	}
}

// The operate right is set in the permission table.
func TestOperatePermission(t *testing.T) {
	e, _, _ := setupJobsEnv(t)
	_, carolID := e.createUser("carol")
	r := e.get("/stacks/web/settings")
	if !strings.Contains(r.body, `name="can_operate" form="perm-`+itoa(carolID)+`"`) {
		t.Fatalf("no operate checkbox:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/settings")
	r = e.do(e.client, http.MethodPost, "/stacks/web/permissions",
		url.Values{"csrf_token": {token}, "user_id": {itoa(carolID)}, "can_operate": {"on"}}, map[string]string{"HX-Request": "true"})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `aria-label="Operate" checked`) {
		t.Fatalf("row after saving:\n%s", r.body)
	}
	web, _ := e.st.StackByName(context.Background(), "web")
	if p, _ := e.st.Permission(context.Background(), carolID, web.ID); !p.CanOperate || p.CanBackup {
		t.Fatalf("stored: %+v", p)
	}
}
