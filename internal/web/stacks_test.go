package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
)

const adminNewPW = "admin long password"

// loginAdmin logs the default client in as admin and replaces the
// generated password.
func (e *testEnv) loginAdmin() {
	e.t.Helper()
	expect(e.t, e.login(e.client, "admin", e.adminPW), http.StatusSeeOther, "/")
	token := e.csrf("/account/password")
	expect(e.t, e.post("/account/password", url.Values{"csrf_token": {token}, "current": {e.adminPW},
		"new": {adminNewPW}, "confirm": {adminNewPW}}), http.StatusSeeOther, "/")
}

var generatedPW = regexp.MustCompile(`<code class="secret">([^<]+)</code>`)

// createUser creates a non-admin user through the admin UI, logs it in with
// a new client and sets its own password. It returns the client and id.
func (e *testEnv) createUser(name string) (*http.Client, int64) {
	e.t.Helper()
	r := e.post("/admin/users", url.Values{"csrf_token": {e.csrf("/admin/users")}, "username": {name}})
	expect(e.t, r, http.StatusOK, "")
	m := generatedPW.FindStringSubmatch(r.body)
	if m == nil {
		e.t.Fatalf("no generated password shown:\n%s", r.body)
	}
	c := e.newClient()
	expect(e.t, e.login(c, name, m[1]), http.StatusSeeOther, "/")
	expect(e.t, e.do(c, http.MethodGet, "/", nil, nil), http.StatusSeeOther, "/account/password")
	token := csrfOf(e, c, "/account/password")
	pw := name + " long password"
	expect(e.t, e.do(c, http.MethodPost, "/account/password", url.Values{"csrf_token": {token}, "current": {m[1]},
		"new": {pw}, "confirm": {pw}}, nil), http.StatusSeeOther, "/")
	u, err := e.st.UserByUsername(context.Background(), name)
	if err != nil || u.IsAdmin || u.MustChangePassword {
		e.t.Fatalf("created user: %+v, %v", u, err)
	}
	return c, u.ID
}

func csrfOf(e *testEnv, c *http.Client, path string) string {
	e.t.Helper()
	r := e.do(c, http.MethodGet, path, nil, nil)
	m := csrfRe.FindStringSubmatch(r.body)
	if m == nil {
		e.t.Fatalf("no CSRF token on %s (status %d)", path, r.status)
	}
	return m[1]
}

func (e *testEnv) setupStacks() (web, db int64) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.st.SyncStacks(ctx, []string{"web", "db"}); err != nil {
		e.t.Fatal(err)
	}
	w, _ := e.st.StackByName(ctx, "web")
	d, _ := e.st.StackByName(ctx, "db")
	return w.ID, d.ID
}

func TestStackAccessControl(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	get := func(path string) response { return e.do(alice, http.MethodGet, path, nil, nil) }

	// Admin sees everything.
	r := e.get("/")
	expect(t, r, http.StatusOK, "")
	for _, s := range []string{`href="/stacks/web"`, `href="/stacks/db"`, `href="/admin/users"`} {
		if !strings.Contains(r.body, s) {
			t.Errorf("admin stack list lacks %s", s)
		}
	}
	expect(t, e.get("/stacks/db"), http.StatusOK, "")
	expect(t, e.get("/stacks/nope/settings"), http.StatusNotFound, "")

	// Without grants alice sees nothing; hidden stacks look like unknown ones.
	r = get("/")
	expect(t, r, http.StatusOK, "")
	if strings.Contains(r.body, "/stacks/") || strings.Contains(r.body, "/admin/users") {
		t.Fatalf("alice sees stacks or admin links:\n%s", r.body)
	}
	for _, p := range []string{"/stacks/web", "/stacks/nope", "/stacks/..", "/stacks/.hidden"} {
		expect(t, get(p), http.StatusNotFound, "")
	}

	// Admin grants backup on "web" via HTMX: the updated row comes back.
	token := e.csrf("/stacks/web/settings")
	r = e.do(e.client, http.MethodPost, "/stacks/web/permissions",
		url.Values{"csrf_token": {token}, "user_id": {itoa(aliceID)}, "can_backup": {"on"}},
		map[string]string{"HX-Request": "true"})
	expect(t, r, http.StatusOK, "")
	if !strings.HasPrefix(strings.TrimSpace(r.body), "<tr>") || strings.Contains(r.body, "<html") ||
		!regexp.MustCompile(`name="can_backup"[^>]*checked`).MatchString(r.body) ||
		regexp.MustCompile(`name="can_restore"[^>]*checked`).MatchString(r.body) {
		t.Fatalf("HTMX answer is not the updated row:\n%s", r.body)
	}

	r = get("/")
	if !strings.Contains(r.body, `href="/stacks/web"`) || strings.Contains(r.body, `href="/stacks/db"`) {
		t.Fatalf("alice should see web only:\n%s", r.body)
	}

	// The logs right alone also makes a stack visible.
	r = e.post("/stacks/db/permissions", url.Values{"csrf_token": {e.csrf("/stacks/db/settings")},
		"user_id": {itoa(aliceID)}, "can_logs": {"on"}})
	expect(t, r, http.StatusSeeOther, "/stacks/db/settings")
	r = get("/")
	if !strings.Contains(r.body, `href="/stacks/db"`) {
		t.Fatalf("alice should see db with the logs right:\n%s", r.body)
	}
	db, err := e.st.StackByName(context.Background(), "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: db.ID}); err != nil {
		t.Fatal(err)
	}
	// The Backups tab looks the same for alice and the admin, except the
	// admin's extra Settings tab.
	r = get("/stacks/web")
	expect(t, r, http.StatusOK, "")
	adminView := e.get("/stacks/web")
	for _, view := range []response{r, adminView} {
		if strings.Contains(view.body, "/permissions") || strings.Contains(view.body, "/enabled") {
			t.Fatal("settings forms on the Backups tab")
		}
	}
	if strings.Contains(r.body, "/stacks/web/settings") || !strings.Contains(adminView.body, `href="/stacks/web/settings"`) {
		t.Fatal("Settings tab must be shown to admins only")
	}
	expect(t, get("/stacks/web/settings"), http.StatusForbidden, "")
	expect(t, get("/stacks/db"), http.StatusNotFound, "")

	// Admin-only endpoints are refused for alice, even with a valid token.
	aliceToken := csrfOf(e, alice, "/stacks/web")
	for _, p := range []string{"/stacks/web/permissions", "/stacks/web/enabled", "/admin/users", "/admin/users/1"} {
		expect(t, e.do(alice, http.MethodPost, p, url.Values{"csrf_token": {aliceToken}, "user_id": {itoa(aliceID)},
			"can_backup": {"on"}, "can_restore": {"on"}, "username": {"mallory"}, "is_admin": {"on"}}, nil), http.StatusForbidden, "")
	}
	expect(t, get("/admin/users"), http.StatusForbidden, "")
	if p, _ := e.st.Permission(context.Background(), aliceID, webID); p.CanRestore {
		t.Fatal("alice granted herself restore")
	}

	// Revoking (plain form post) hides the stack again.
	expect(t, e.post("/stacks/web/permissions", url.Values{"csrf_token": {token}, "user_id": {itoa(aliceID)}}),
		http.StatusSeeOther, "/stacks/web/settings")
	expect(t, get("/stacks/web"), http.StatusNotFound, "")

	// Permissions for admins or unknown users are refused.
	expect(t, e.post("/stacks/web/permissions", url.Values{"csrf_token": {token}, "user_id": {"1"}, "can_backup": {"on"}}),
		http.StatusBadRequest, "")
	expect(t, e.post("/stacks/web/permissions", url.Values{"csrf_token": {token}, "user_id": {"999"}}), http.StatusBadRequest, "")
	expect(t, e.post("/stacks/web/permissions", url.Values{"csrf_token": {token}, "user_id": {"x"}}), http.StatusBadRequest, "")
	// Without CSRF token nothing changes.
	expect(t, e.post("/stacks/web/permissions", url.Values{"user_id": {itoa(aliceID)}, "can_backup": {"on"}}), http.StatusForbidden, "")

	actions := auditActions(t, e.st)
	if actions["permission.update"] != 3 || actions["user.create"] != 1 {
		t.Fatalf("audit: %v", actions)
	}
}

func TestStackDetailShowsRepositoryAndSnapshots(t *testing.T) {
	e := newEnv(t, true, true)
	web, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := e.st.SetRepository(ctx, web, "rest:https://backup.example/web", "repo-secret-pw",
		map[string]string{"RESTIC_REST_PASSWORD": "rest-secret-pw"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "host-a", Paths: []string{"/s/web"}, Tags: []string{"final"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()

	r := e.get("/stacks/web/backups")
	expect(t, r, http.StatusOK, "")
	for _, s := range []string{"rest:https://backup.example/web", "aaaaaaaa", "final", "2026-11-13 03:00 UTC"} {
		if !strings.Contains(r.body, s) {
			t.Errorf("stack page lacks %q", s)
		}
	}
	for _, secret := range []string{"repo-secret-pw", "rest-secret-pw"} {
		if strings.Contains(r.body, secret) {
			t.Fatalf("stack page contains secret %q", secret)
		}
	}
	r = e.get("/")
	if !strings.Contains(r.body, "<th>Scheduling</th>") || !strings.Contains(r.body, "configured") ||
		!strings.Contains(r.body, `<span class="muted">never</span>`) {
		t.Errorf("stack list:\n%s", r.body)
	}
	// Last backup: the newest backup job with its state, the last success
	// as tooltip.
	for _, j := range []store.Job{
		{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", State: jobs.StateSucceeded, FinishedAt: t0},
		{ID: "01AAAAAAAAAAAAAAAAAAAAAAA2", State: jobs.StateFailed, FinishedAt: t0.Add(24 * time.Hour)},
	} {
		j.StackID, j.Type, j.Trigger, j.QueuedAt, j.StartedAt = web, jobs.TypeBackup, store.TriggerSchedule, t0, t0
		if err := e.st.ImportJob(ctx, j, ""); err != nil {
			t.Fatal(err)
		}
	}
	r = e.get("/")
	if want := `<a href="/jobs/01AAAAAAAAAAAAAAAAAAAAAAA2" title="Last successful backup: 2026-11-13 03:00 UTC">2026-11-14 03:00 UTC</a> <span class="badge err">failed</span>`; !strings.Contains(r.body, want) {
		t.Errorf("stack list lacks %s:\n%s", want, r.body)
	}

	// Disable and enable the stack on the Settings tab.
	r = e.get("/stacks/web/settings")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `action="/stacks/web/enabled"`) || !strings.Contains(r.body, `aria-current="page">Settings`) {
		t.Fatalf("settings tab:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/settings")
	expect(t, e.post("/stacks/web/enabled", url.Values{"csrf_token": {token}, "enabled": {"false"}}), http.StatusSeeOther, "/stacks/web/settings")
	if st, _ := e.st.StackByName(ctx, "web"); st.Enabled {
		t.Fatal("stack still enabled")
	}
	expect(t, e.post("/stacks/web/enabled", url.Values{"csrf_token": {token}, "enabled": {"true"}}), http.StatusSeeOther, "/stacks/web/settings")
	if st, _ := e.st.StackByName(ctx, "web"); !st.Enabled {
		t.Fatal("stack still disabled")
	}
}

func TestUserManagement(t *testing.T) {
	e := newEnv(t, true, true)
	e.loginAdmin()
	bob, bobID := e.createUser("bob")
	token := e.csrf("/admin/users")
	post := func(path string, v url.Values) response {
		v.Set("csrf_token", token)
		return e.post(path, v)
	}

	// Invalid and duplicate usernames.
	for _, name := range []string{"", "Bob", "../x", "-x", strings.Repeat("a", 65)} {
		expect(t, post("/admin/users", url.Values{"username": {name}}), http.StatusBadRequest, "")
	}
	expect(t, post("/admin/users", url.Values{"username": {"bob"}}), http.StatusConflict, "")

	// The admin cannot change their own account here.
	admin, _ := e.st.UserByUsername(context.Background(), "admin")
	expect(t, post("/admin/users/"+itoa(admin.ID), url.Values{}), http.StatusBadRequest, "")
	expect(t, post("/admin/users/"+itoa(admin.ID)+"/password", url.Values{}), http.StatusBadRequest, "")
	expect(t, post("/admin/users/999", url.Values{}), http.StatusNotFound, "")
	expect(t, post("/admin/users/abc", url.Values{}), http.StatusNotFound, "")

	// Promote bob, then bob may demote the admin (two admins exist) ...
	expect(t, post("/admin/users/"+itoa(bobID), url.Values{"is_admin": {"on"}}), http.StatusSeeOther, "/admin/users")
	bobToken := csrfOf(e, bob, "/admin/users")
	expect(t, e.do(bob, http.MethodPost, "/admin/users/"+itoa(admin.ID), url.Values{"csrf_token": {bobToken}}, nil),
		http.StatusSeeOther, "/admin/users")
	// ... and the former admin lost access to the user page.
	expect(t, e.get("/admin/users"), http.StatusForbidden, "")

	// bob disables the former admin: its session ends immediately.
	expect(t, e.do(bob, http.MethodPost, "/admin/users/"+itoa(admin.ID), url.Values{"csrf_token": {bobToken}, "disabled": {"on"}}, nil),
		http.StatusSeeOther, "/admin/users")
	expect(t, e.get("/"), http.StatusSeeOther, "/login")
	expect(t, e.login(e.client, "admin", adminNewPW), http.StatusUnauthorized, "")

	// bob resets the admin's password: shown once, must be changed.
	r := e.do(bob, http.MethodPost, "/admin/users/"+itoa(admin.ID), url.Values{"csrf_token": {bobToken}, "is_admin": {"on"}}, nil)
	expect(t, r, http.StatusSeeOther, "/admin/users")
	r = e.do(bob, http.MethodPost, "/admin/users/"+itoa(admin.ID)+"/password", url.Values{"csrf_token": {bobToken}}, nil)
	expect(t, r, http.StatusOK, "")
	m := generatedPW.FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no new password shown:\n%s", r.body)
	}
	expect(t, e.login(e.client, "admin", m[1]), http.StatusSeeOther, "/")
	expect(t, e.get("/"), http.StatusSeeOther, "/account/password")

	actions := auditActions(t, e.st)
	if actions["user.update"] != 4 || actions["user.password_reset"] != 1 {
		t.Fatalf("audit: %v", actions)
	}
}

func auditActions(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	entries, err := st.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, a := range entries {
		out[a.Action]++
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Unavailable actions are shown disabled, not hidden (only missing
// permissions hide them).
func TestBackupsTabWithoutRepositoryShowsDisabledActions(t *testing.T) {
	e := newEnv(t, true, true)
	if err := e.st.SyncStacks(context.Background(), []string{"web"}); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	r := e.get("/stacks/web")
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{`disabled>Back up now</button>`, "the stack has no repository"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Status tab lacks %q:\n%s", want, r.body)
		}
	}
	r = e.get("/stacks/web/backups")
	if !strings.Contains(r.body, `disabled>Refresh</button>`) {
		t.Errorf("Backups tab lacks a disabled Refresh button:\n%s", r.body)
	}
	// Without the backup right, the Backup section stays hidden.
	alice, aliceID := e.createUser("alice")
	web, _ := e.st.StackByName(context.Background(), "web")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: web.ID, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	r = e.do(alice, http.MethodGet, "/stacks/web", nil, nil)
	expect(t, r, http.StatusOK, "")
	if strings.Contains(r.body, "Back up now") {
		t.Errorf("Status tab of a user without backup right:\n%s", r.body)
	}
	r = e.do(alice, http.MethodGet, "/stacks/web/backups", nil, nil)
	if !strings.Contains(r.body, `disabled>Refresh</button>`) {
		t.Errorf("Backups tab of a user without backup right:\n%s", r.body)
	}
}
