package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// fakeReloader counts scheduler reloads.
type fakeReloader struct {
	mu sync.Mutex
	n  int
}

func (f *fakeReloader) Reload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return nil
}

func (f *fakeReloader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func TestSchedules(t *testing.T) {
	e := newEnv(t, true, true)
	ctx := context.Background()
	reloader := &fakeReloader{}
	e.server.Schedules = reloader
	if err := e.st.SyncStacks(ctx, []string{"web", "db"}); err != nil {
		t.Fatal(err)
	}
	web, _ := e.st.StackByName(ctx, "web")
	if _, err := e.st.SetRepository(ctx, web.ID, "/srv/restic/web", "repo-pw", nil); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: web.ID, CanBackup: true, CanRestore: true}); err != nil {
		t.Fatal(err)
	}

	r := e.get("/stacks/web/settings")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "No schedules") || !strings.Contains(r.body, `action="/stacks/web/schedules"`) {
		t.Fatalf("settings tab without schedules:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/settings")
	post := func(path string, v url.Values) response {
		v.Set("csrf_token", token)
		return e.post(path, v)
	}

	// Invalid expressions are refused and the input is kept.
	for _, expr := range []string{"", "every night", "0 3 * *", "@every 10s", "CRON_TZ=UTC 0 3 * * *"} {
		r := post("/stacks/web/schedules", url.Values{"cron_expr": {expr}, "pre_stop": {"on"}})
		expect(t, r, http.StatusBadRequest, "")
		if expr != "" && !strings.Contains(r.body, `value="`+expr+`"`) {
			t.Errorf("input %q not kept:\n%s", expr, r.body)
		}
	}
	if list, _ := e.st.ListSchedules(ctx, 0); len(list) != 0 {
		t.Fatalf("invalid schedules saved: %+v", list)
	}

	// A valid schedule is saved normalized, audited and loaded by the scheduler.
	expect(t, post("/stacks/web/schedules", url.Values{"cron_expr": {"  0  3 * * * "}, "pre_stop": {"on"}}),
		http.StatusSeeOther, "/stacks/web/settings")
	list, err := e.st.ListSchedules(ctx, web.ID)
	if err != nil || len(list) != 1 || list[0].CronExpr != "0 3 * * *" || !list[0].PreStop || !list[0].Enabled {
		t.Fatalf("schedules = %+v, %v", list, err)
	}
	id := itoa(list[0].ID)
	if reloader.count() != 1 {
		t.Errorf("reloads = %d, want 1", reloader.count())
	}
	// The clock is 2026-11-14 10:00 UTC; the test runs in the local zone,
	// so only check that a next run is shown.
	r = e.get("/stacks/web/settings")
	if !strings.Contains(r.body, "<code>0 3 * * *</code>") || !strings.Contains(r.body, "2026-11-1") {
		t.Fatalf("schedule not listed with next run:\n%s", r.body)
	}
	if r := e.do(alice, http.MethodGet, "/stacks/web/backups", nil, nil); !strings.Contains(r.body, "Next scheduled backup") {
		t.Errorf("Backups tab lacks next scheduled backup:\n%s", r.body)
	}

	// Disable, enable, and changes through another stack's URL.
	expect(t, post("/stacks/web/schedules/"+id+"/enabled", url.Values{"enabled": {"false"}}), http.StatusSeeOther, "/stacks/web/settings")
	if sc, _ := e.st.ScheduleByID(ctx, list[0].ID); sc.Enabled {
		t.Fatal("schedule still enabled")
	}
	if r := e.get("/stacks/web/backups"); strings.Contains(r.body, "Next scheduled backup") {
		t.Error("disabled schedule shown as next scheduled backup")
	}
	expect(t, post("/stacks/web/schedules/"+id+"/enabled", url.Values{"enabled": {"true"}}), http.StatusSeeOther, "/stacks/web/settings")
	expect(t, post("/stacks/db/schedules/"+id+"/enabled", url.Values{"enabled": {"false"}}), http.StatusNotFound, "")
	expect(t, post("/stacks/db/schedules/"+id+"/delete", url.Values{}), http.StatusNotFound, "")
	expect(t, post("/stacks/web/schedules/x/delete", url.Values{}), http.StatusNotFound, "")
	expect(t, post("/stacks/web/schedules/999/delete", url.Values{}), http.StatusNotFound, "")

	// Non-admins may not change schedules, even with backup rights.
	aliceToken := csrfOf(e, alice, "/stacks/web")
	for _, p := range []string{"/stacks/web/schedules", "/stacks/web/schedules/" + id + "/enabled", "/stacks/web/schedules/" + id + "/delete"} {
		expect(t, e.do(alice, http.MethodPost, p, url.Values{"csrf_token": {aliceToken}, "cron_expr": {"@hourly"},
			"enabled": {"false"}}, nil), http.StatusForbidden, "")
	}
	// No CSRF token: refused.
	expect(t, e.post("/stacks/web/schedules/"+id+"/delete", url.Values{}), http.StatusForbidden, "")

	expect(t, post("/stacks/web/schedules/"+id+"/delete", url.Values{}), http.StatusSeeOther, "/stacks/web/settings")
	if list, _ := e.st.ListSchedules(ctx, 0); len(list) != 0 {
		t.Fatalf("schedule not deleted: %+v", list)
	}
	if reloader.count() != 4 {
		t.Errorf("reloads = %d, want 4 (create, disable, enable, delete)", reloader.count())
	}

	// An update schedule with its options; rollback in place is ignored for
	// backups. An update schedule is not a "next scheduled backup".
	expect(t, post("/stacks/web/schedules", url.Values{"cron_expr": {"@weekly"}, "job_type": {"update"},
		"rollback_in_place": {"on"}}), http.StatusSeeOther, "/stacks/web/settings")
	expect(t, post("/stacks/web/schedules", url.Values{"cron_expr": {"@daily"}, "job_type": {"backup"},
		"rollback_in_place": {"on"}}), http.StatusSeeOther, "/stacks/web/settings")
	list, _ = e.st.ListSchedules(ctx, web.ID)
	if len(list) != 2 || list[0].Type != jobs.TypeUpdate || !list[0].RollbackInPlace || list[0].PreStop ||
		list[1].Type != jobs.TypeBackup || list[1].RollbackInPlace {
		t.Fatalf("schedules = %+v", list)
	}
	r = e.get("/stacks/web/settings")
	if !strings.Contains(r.body, "<td>Update</td>") || !strings.Contains(r.body, "only if new, rollback in place") {
		t.Errorf("update schedule not listed:\n%s", r.body)
	}
	expect(t, post("/stacks/web/schedules/"+itoa(list[1].ID)+"/delete", url.Values{}), http.StatusSeeOther, "/stacks/web/settings")
	if r := e.get("/stacks/web/backups"); strings.Contains(r.body, "Next scheduled backup") {
		t.Error("update schedule shown as next scheduled backup")
	}
	r = post("/stacks/web/schedules", url.Values{"cron_expr": {"@daily"}, "job_type": {"restore"}})
	expect(t, r, http.StatusBadRequest, "")
	r = post("/stacks/web/schedules", url.Values{"cron_expr": {"nope"}, "job_type": {"update"}, "rollback_in_place": {"on"}})
	if !strings.Contains(r.body, `name="job_type" value="update" checked`) || !strings.Contains(r.body, `name="rollback_in_place" checked`) {
		t.Errorf("update input not kept:\n%s", r.body)
	}

	audit, err := e.st.ListAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range audit {
		if strings.HasPrefix(a.Action, "schedule.") {
			actions = append(actions, a.Action)
		}
	}
	if got := strings.Join(actions, ","); got != "schedule.delete,schedule.create,schedule.create,schedule.delete,schedule.enable,schedule.disable,schedule.create" {
		t.Errorf("schedule audit entries = %s", got)
	}
}

func TestScheduleLimit(t *testing.T) {
	e := newEnv(t, true, true)
	ctx := context.Background()
	if err := e.st.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	web, _ := e.st.StackByName(ctx, "web")
	for range maxSchedulesPerStack {
		if _, err := e.st.CreateSchedule(ctx, store.Schedule{StackID: web.ID, CronExpr: "@daily", Type: jobs.TypeBackup}); err != nil {
			t.Fatal(err)
		}
	}
	e.loginAdmin()
	token := e.csrf("/stacks/web/settings")
	r := e.post("/stacks/web/schedules", url.Values{"csrf_token": {token}, "cron_expr": {"@hourly"}})
	expect(t, r, http.StatusBadRequest, "")
	if !strings.Contains(r.body, "at most") {
		t.Errorf("limit error not shown:\n%s", r.body)
	}
}
