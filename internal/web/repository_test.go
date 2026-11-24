package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// fakeRefresher stands in for snapshots.Cache: it "finds" the configured
// snapshots and stores them in the cache, or fails with err.
type fakeRefresher struct {
	st *store.Store

	mu    sync.Mutex
	snaps []restic.Snapshot
	err   error
	calls int
}

func (f *fakeRefresher) Refresh(ctx context.Context, stackID int64) ([]store.CachedSnapshot, error) {
	repo, err := f.st.RepositoryInfoForStack(ctx, stackID)
	if err != nil {
		return nil, err
	}
	return f.RefreshRepository(ctx, repo.ID)
}

func (f *fakeRefresher) RefreshRepository(ctx context.Context, repoID int64) ([]store.CachedSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if err := f.st.ReplaceSnapshots(ctx, repoID, f.snaps, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)); err != nil {
		return nil, err
	}
	return f.st.ListSnapshots(ctx, repoID)
}

// setRepo gives a stack the repository named after location, creating it
// if no repository has that name yet (stacks with the same location share
// it). It returns the repository id.
func setRepo(ctx context.Context, st *store.Store, stackID int64, location, password string, env map[string]string) (int64, error) {
	id, err := repoIDByName(ctx, st, location)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		if id, err = st.CreateRepository(ctx, location, location, password, env); err != nil {
			return 0, err
		}
	}
	return id, st.SetStackRepository(ctx, stackID, id)
}

// repoIDByName returns the id of the repository with that name, 0 if
// there is none.
func repoIDByName(ctx context.Context, st *store.Store, name string) (int64, error) {
	repos, err := st.ListRepositories(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range repos {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, nil
}

func TestRepositoryAdmin(t *testing.T) {
	e := newEnv(t, true, true)
	webID, dbID := e.setupStacks()
	e.loginAdmin()
	ctx := context.Background()
	e.snaps.snaps = []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "h", Paths: []string{"/s/web"}},
	}
	token := e.csrf("/admin/repositories")

	// New repository from a REST URL with credentials.
	r := e.post("/admin/repositories", url.Values{"csrf_token": {token}, "name": {"nas"},
		"location": {"rest:https://alice:s3cr%2Ft@backup.example:8000/stacks"}, "password": {"repo-pw-1"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "restic found 1 snapshot.") || !strings.Contains(r.body, "RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD") {
		t.Fatalf("no success message:\n%s", r.body)
	}
	id, _ := repoIDByName(ctx, e.st, "nas")
	if id == 0 {
		t.Fatal("repository not created")
	}
	repo := func() store.Repository {
		t.Helper()
		r, err := e.st.Repository(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	got := repo()
	wantEnv := map[string]string{"RESTIC_REST_USERNAME": "alice", "RESTIC_REST_PASSWORD": "s3cr/t"}
	if got.Location != "rest:https://backup.example:8000/stacks" || got.Password != "repo-pw-1" || !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("stored repository: %q %q %v", got.Location, got.Password, got.Env)
	}
	for _, secret := range []string{"s3cr", "repo-pw-1"} {
		if strings.Contains(r.body, secret) {
			t.Fatalf("page contains secret %q", secret)
		}
	}
	if !strings.Contains(r.body, "<code>RESTIC_REST_PASSWORD</code>, <code>RESTIC_REST_USERNAME</code>") {
		t.Fatal("stored variable names not shown")
	}

	// Two stacks use it; the list shows them.
	for _, st := range []int64{webID, dbID} {
		if err := e.st.SetStackRepository(ctx, st, id); err != nil {
			t.Fatal(err)
		}
	}
	r = e.get("/admin/repositories")
	if !strings.Contains(r.body, `<a href="/admin/repositories/`+strconv.FormatInt(id, 10)+`">nas</a>`) ||
		!strings.Contains(r.body, `<a href="/stacks/db/settings">db</a>, <a href="/stacks/web/settings">web</a>`) {
		t.Fatalf("repository list:\n%s", r.body)
	}

	edit := func(v url.Values) response {
		v.Set("csrf_token", token)
		return e.post("/admin/repositories/"+strconv.FormatInt(id, 10), v)
	}
	// Empty password and variables keep the stored ones; the location
	// changes for both stacks.
	expect(t, edit(url.Values{"name": {"nas-2027"}, "location": {"rest:https://backup2.example/stacks"}}), http.StatusOK, "")
	if got := repo(); got.Name != "nas-2027" || got.Location != "rest:https://backup2.example/stacks" ||
		got.Password != "repo-pw-1" || !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("after keep: %+v", got)
	}
	for _, st := range []int64{webID, dbID} {
		if r, err := e.st.RepositoryForStack(ctx, st); err != nil || r.Location != "rest:https://backup2.example/stacks" {
			t.Fatalf("stack %d: %+v, %v", st, r, err)
		}
	}

	// Variables replace all stored ones.
	expect(t, edit(url.Values{"name": {"nas-2027"}, "location": {"s3:s3.example/bucket"}, "password": {"repo-pw-2"},
		"env": {"# S3\r\nAWS_ACCESS_KEY_ID=AK\r\n\r\nAWS_SECRET_ACCESS_KEY=a=b c\r\n"}}), http.StatusOK, "")
	if got := repo(); got.Password != "repo-pw-2" ||
		!reflect.DeepEqual(got.Env, map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "a=b c"}) {
		t.Fatalf("after replace: %q %v", got.Password, got.Env)
	}
	// clear_env removes them.
	expect(t, edit(url.Values{"name": {"nas-2027"}, "location": {"/srv/restic/stacks"}, "clear_env": {"on"}}), http.StatusOK, "")
	if got := repo(); len(got.Env) != 0 {
		t.Fatalf("after clear: %v", got.Env)
	}

	// Refresh failure: saved anyway, error shown.
	e.snaps.err = fmt.Errorf("listing snapshots: %w", &restic.Error{Op: "snapshots", Err: errors.New("exit status 1"),
		Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open config file"}`})
	r = edit(url.Values{"name": {"nas-2027"}, "location": {"/srv/restic/stacks2"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "restic could not open the repository: Fatal: unable to open config file") {
		t.Fatalf("no refresh error:\n%s", r.body)
	}
	if repo().Location != "/srv/restic/stacks2" {
		t.Fatal("repository not saved")
	}

	// Names are unique.
	if _, err := e.st.CreateRepository(ctx, "other", "/srv/restic/other", "pw", nil); err != nil {
		t.Fatal(err)
	}
	r = edit(url.Values{"name": {"other"}, "location": {"/srv/restic/stacks2"}})
	expect(t, r, http.StatusConflict, "")
	r = e.post("/admin/repositories", url.Values{"csrf_token": {token}, "name": {"other"}, "location": {"/x"}, "password": {"pw"}})
	expect(t, r, http.StatusConflict, "")
	if !strings.Contains(r.body, `value="other"`) || !strings.Contains(r.body, `value="/x"`) {
		t.Fatalf("input not kept:\n%s", r.body)
	}
	expect(t, e.get("/admin/repositories/999"), http.StatusNotFound, "")
	expect(t, e.get("/admin/repositories/x"), http.StatusNotFound, "")

	entries, _ := e.st.ListAudit(ctx, 100)
	counts := map[string]int{}
	for _, a := range entries {
		counts[a.Action]++
		for _, secret := range []string{"s3cr", "repo-pw", "AK"} {
			if strings.HasPrefix(a.Action, "repository.") && strings.Contains(a.Detail, secret) {
				t.Fatalf("audit detail contains a secret: %s", a.Detail)
			}
		}
	}
	if counts["repository.create"] != 1 || counts["repository.update"] != 4 {
		t.Fatalf("audit: %v", counts)
	}
}

func TestRepositoryFormValidation(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.loginAdmin()
	token := e.csrf("/admin/repositories")
	tests := []struct {
		name    string
		form    url.Values
		wantMsg string
		secret  string // must not be echoed
	}{
		{"no name", url.Values{"location": {"/r"}, "password": {"x"}}, "name must not be empty", ""},
		{"no password for a new repository", url.Values{"name": {"n"}, "location": {"/r"}}, "password must not be empty", ""},
		{"unparseable URL", url.Values{"name": {"n"}, "location": {"rest:https://alice:pa/ss@host/web"}, "password": {"x"}},
			"cannot be parsed", "pa/ss"},
		{"credentials in s3 URL", url.Values{"name": {"n"}, "location": {"s3:https://ak:sk-secret@s3.example/b"}, "password": {"x"}},
			"Only rest: repository URLs", "sk-secret"},
		{"unsupported variable", url.Values{"name": {"n"}, "location": {"/r"}, "password": {"x"}, "env": {"LD_PRELOAD=/evil.so"}},
			"not supported", "/evil.so"},
		{"password variable", url.Values{"name": {"n"}, "location": {"/r"}, "password": {"x"}, "env": {"RESTIC_PASSWORD=env-pw-secret"}},
			"not supported", "env-pw-secret"},
		{"malformed line", url.Values{"name": {"n"}, "location": {"/r"}, "password": {"x"}, "env": {"AWS_ACCESS_KEY_ID"}}, "KEY=value", ""},
		{"flag as location", url.Values{"name": {"n"}, "location": {"--insecure-tls"}, "password": {"x"}}, "must not start with", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.form.Set("csrf_token", token)
			r := e.post("/admin/repositories", tc.form)
			expect(t, r, http.StatusBadRequest, "")
			if !strings.Contains(r.body, tc.wantMsg) {
				t.Fatalf("message %q missing:\n%s", tc.wantMsg, r.body)
			}
			if tc.secret != "" && strings.Contains(r.body, tc.secret) {
				t.Fatalf("response echoes %q", tc.secret)
			}
		})
	}
	if repos, _ := e.st.ListRepositories(context.Background()); len(repos) != 0 {
		t.Fatal("invalid input saved a repository")
	}
	if e.snaps.calls != 0 {
		t.Fatal("refresh ran for invalid input")
	}
}

func TestRepositoryAdminOnly(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	init := &fakeInit{}
	e.server.Repos = init
	id, err := setRepo(context.Background(), e.st, webID, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: webID, CanBackup: true, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	token := csrfOf(e, alice, "/stacks/web")
	repoPath := "/admin/repositories/" + strconv.FormatInt(id, 10)
	expect(t, e.do(alice, http.MethodGet, "/admin/repositories", nil, nil), http.StatusForbidden, "")
	expect(t, e.do(alice, http.MethodGet, repoPath, nil, nil), http.StatusForbidden, "")
	for _, path := range []string{"/admin/repositories", repoPath, repoPath + "/init", repoPath + "/delete", "/stacks/web/repository"} {
		r := e.do(alice, http.MethodPost, path, url.Values{"csrf_token": {token}, "name": {"x"}, "location": {"/r"}, "password": {"x"}}, nil)
		expect(t, r, http.StatusForbidden, "")
	}
	if len(init.repos) != 0 {
		t.Fatal("init ran for a non-admin")
	}
	if r, err := e.st.RepositoryForStack(context.Background(), webID); err != nil || r.Location != "/srv/restic/web" {
		t.Fatalf("repository changed: %+v, %v", r, err)
	}
}

// The Settings tab chooses one of the repositories for the stack.
func TestSetStackRepository(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	e.loginAdmin()
	e.snaps.snaps = []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "test-host", Paths: []string{"/s/web"}},
		{ID: "bbbbbbbb22222222", ShortID: "bbbbbbbb", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "test-host", Paths: []string{"/s/db"}},
	}
	nas, err := e.st.CreateRepository(ctx, "nas", "/srv/restic/nas", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateRepository(ctx, "cloud", "s3:s3.example/b", "pw", nil); err != nil {
		t.Fatal(err)
	}
	r := e.get("/stacks/web/settings")
	if !strings.Contains(r.body, `<option value="" selected>none (no backups)</option>`) ||
		!strings.Contains(r.body, `<option value="`+strconv.FormatInt(nas, 10)+`">nas</option>`) {
		t.Fatalf("settings tab:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/settings")
	set := func(v string) response {
		return e.post("/stacks/web/repository", url.Values{"csrf_token": {token}, "repository": {v}})
	}

	r = set(strconv.FormatInt(nas, 10))
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "The stack uses the repository nas now. restic found 1 snapshot of this stack.") ||
		!strings.Contains(r.body, `<option value="`+strconv.FormatInt(nas, 10)+`" selected>nas</option>`) {
		t.Fatalf("after choosing:\n%s", r.body)
	}
	if ri, err := e.st.RepositoryInfoForStack(ctx, webID); err != nil || ri.ID != nas {
		t.Fatalf("stack repository: %+v, %v", ri, err)
	}
	if r := e.get("/"); !strings.Contains(r.body, "<td>nas</td>") {
		t.Fatalf("stack list does not show the repository name:\n%s", r.body)
	}
	if r := e.get("/stacks/web/backups"); !strings.Contains(r.body, "nas: <code>/srv/restic/nas</code>") {
		t.Fatalf("backups tab:\n%s", r.body)
	}

	expect(t, set("999"), http.StatusBadRequest, "")
	expect(t, set("x"), http.StatusBadRequest, "")
	r = set("")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "The stack has no repository now.") {
		t.Fatalf("after none:\n%s", r.body)
	}
	if _, err := e.st.RepositoryInfoForStack(ctx, webID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stack still has a repository: %v", err)
	}
	if n := auditActions(t, e.st)["stack.repository"]; n != 2 {
		t.Fatalf("stack.repository logged %d times", n)
	}
}

// A repository can be deleted only when no stack uses it.
func TestDeleteRepository(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	id, err := setRepo(ctx, e.st, webID, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	path := "/admin/repositories/" + strconv.FormatInt(id, 10)
	token := e.csrf(path)
	if r := e.get(path); !strings.Contains(r.body, `class="secondary" disabled>Delete repository`) {
		t.Fatalf("delete not disabled:\n%s", r.body)
	}
	r := e.post(path+"/delete", url.Values{"csrf_token": {token}})
	expect(t, r, http.StatusConflict, "")
	if err := e.st.SetStackRepository(ctx, webID, 0); err != nil {
		t.Fatal(err)
	}
	expect(t, e.post(path+"/delete", url.Values{"csrf_token": {token}}), http.StatusSeeOther, "/admin/repositories")
	if _, err := e.st.Repository(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repository still exists: %v", err)
	}
	if n := auditActions(t, e.st)["repository.delete"]; n != 1 {
		t.Fatalf("repository.delete logged %d times", n)
	}
}

func TestParseEnvLines(t *testing.T) {
	got, err := parseEnvLines("  # comment\nB2_ACCOUNT_ID=id\n\nB2_ACCOUNT_KEY= spaced value \n")
	want := map[string]string{"B2_ACCOUNT_ID": "id", "B2_ACCOUNT_KEY": " spaced value "}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"B2_ACCOUNT_ID=a\nB2_ACCOUNT_ID=b", "RESTIC_PASSWORD_FILE=/x", "PATH=/tmp", "novalue"} {
		if _, err := parseEnvLines(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestRefreshSnapshots(t *testing.T) {
	e := newEnv(t, true, true)
	webID, dbID := e.setupStacks()
	ctx := context.Background()
	if _, err := setRepo(ctx, e.st, webID, "/srv/restic/web", "pw", nil); err != nil {
		t.Fatal(err)
	}
	e.snaps.snaps = []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "h", Paths: []string{"/s/web"}},
	}
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: webID, CanBackup: true}); err != nil {
		t.Fatal(err)
	}
	token := csrfOf(e, alice, "/stacks/web")
	refresh := func(path string, htmx bool) response {
		h := map[string]string{}
		if htmx {
			h["HX-Request"] = "true"
		}
		return e.do(alice, http.MethodPost, path, url.Values{"csrf_token": {token}}, h)
	}

	// Before: nothing cached.
	if r := e.do(alice, http.MethodGet, "/stacks/web/backups", nil, nil); !strings.Contains(r.body, "Use Refresh to load the list") {
		t.Fatalf("backups tab:\n%s", r.body)
	}

	// A viewer refreshes via HTMX and gets the snapshot section back.
	r := refresh("/stacks/web/snapshots/refresh", true)
	expect(t, r, http.StatusOK, "")
	if !strings.HasPrefix(strings.TrimSpace(r.body), `<section id="snapshots">`) || strings.Contains(r.body, "<html") ||
		!strings.Contains(r.body, "aaaaaaaa") || !strings.Contains(r.body, "Refreshed: 1 snapshot in the repository.") {
		t.Fatalf("HTMX answer:\n%s", r.body)
	}
	// Right away again: cooldown, restic is not called.
	r = refresh("/stacks/web/snapshots/refresh", true)
	if !strings.Contains(r.body, "refreshed just now") || e.snaps.calls != 1 {
		t.Fatalf("cooldown: calls=%d\n%s", e.snaps.calls, r.body)
	}
	// Later, as a plain form post: redirect back to the Backups tab.
	e.advance(11 * time.Second)
	expect(t, refresh("/stacks/web/snapshots/refresh", false), http.StatusSeeOther, "/stacks/web/backups")
	if e.snaps.calls != 2 {
		t.Fatalf("calls = %d", e.snaps.calls)
	}

	// Failure is shown in the section.
	e.advance(11 * time.Second)
	e.snaps.err = errors.New("connection refused")
	r = refresh("/stacks/web/snapshots/refresh", true)
	if !strings.Contains(r.body, "Refresh failed: connection refused") {
		t.Fatalf("failure:\n%s", r.body)
	}

	// Hidden stack, no repository, missing token.
	expect(t, refresh("/stacks/db/snapshots/refresh", true), http.StatusNotFound, "")
	if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: dbID, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	expect(t, refresh("/stacks/db/snapshots/refresh", true), http.StatusBadRequest, "")
	expect(t, e.do(alice, http.MethodPost, "/stacks/web/snapshots/refresh", url.Values{}, nil), http.StatusForbidden, "")

	if n := auditActions(t, e.st)["snapshots.refresh"]; n != 3 {
		t.Fatalf("snapshots.refresh logged %d times", n)
	}
}

// fakeInit records "restic init" calls.
type fakeInit struct {
	mu    sync.Mutex
	repos []restic.Repo
	err   error
}

func (f *fakeInit) Init(_ context.Context, repo restic.Repo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	return f.err
}

// resticExitError returns a *restic.Error with the given exit code, as the
// CLI produces it. The exit code needs a real *exec.ExitError; the command
// is a fixed string (no input).
func resticExitError(t *testing.T, code int, message string) error {
	t.Helper()
	err := exec.CommandContext(t.Context(), "sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatal("expected an exit error")
	}
	return &restic.Error{Op: "snapshots", Err: err,
		Stderr: fmt.Sprintf(`{"message_type":"exit_error","code":%d,"message":%q}`, code, message)}
}

func TestInitRepository(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	init := &fakeInit{}
	e.server.Repos = init
	e.loginAdmin()
	token := e.csrf("/admin/repositories")
	post := func(path string, v url.Values) response {
		v.Set("csrf_token", token)
		return e.post(path, v)
	}

	// No repository at the location: the init button is offered.
	e.snaps.err = fmt.Errorf("listing snapshots: %w", resticExitError(t, 10, "Fatal: repository does not exist: unable to open config file"))
	r := post("/admin/repositories", url.Values{"name": {"nas-2027"},
		"location": {"rest:https://u:rest-pw@backup.example/stacks-2027"}, "password": {"repo-pw"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "There is no restic repository at this location yet") || !initEnabled(r.body) {
		t.Fatalf("init not offered:\n%s", r.body)
	}
	id, _ := repoIDByName(context.Background(), e.st, "nas-2027")
	path := "/admin/repositories/" + strconv.FormatInt(id, 10)

	// Initialize: restic init gets the stored credentials, then the new
	// repository is checked.
	e.snaps.err = nil
	r = post(path+"/init", url.Values{})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "Repository initialized. restic found 0 snapshots.") || initEnabled(r.body) ||
		!strings.Contains(r.body, "disabled>Initialize repository") {
		t.Fatalf("after init:\n%s", r.body)
	}
	want := []restic.Repo{{Location: "rest:https://backup.example/stacks-2027", Env: []string{
		"RESTIC_PASSWORD=repo-pw", "RESTIC_REST_PASSWORD=rest-pw", "RESTIC_REST_USERNAME=u"}}}
	if !reflect.DeepEqual(init.repos, want) {
		t.Fatalf("init calls: %+v", init.repos)
	}

	// Other errors (wrong password, network) do not offer init.
	e.snaps.err = resticExitError(t, 12, "Fatal: wrong password or no key found")
	r = post(path, url.Values{"name": {"nas-2027"}, "location": {"/srv/restic/web"}})
	if initEnabled(r.body) || !strings.Contains(r.body, "wrong password") {
		t.Fatalf("init offered for a wrong password:\n%s", r.body)
	}

	// restic refuses (e.g. a repository appeared meanwhile).
	init.err = &restic.Error{Op: "init", Err: errors.New("exit status 1"),
		Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: config file already exists"}`}
	r = post(path+"/init", url.Values{})
	if !strings.Contains(r.body, "restic could not initialize the repository: Fatal: config file already exists") {
		t.Fatalf("init failure:\n%s", r.body)
	}

	if n := auditActions(t, e.st)["repository.init"]; n != 2 {
		t.Fatalf("repository.init logged %d times", n)
	}
	expect(t, post("/admin/repositories/999/init", url.Values{}), http.StatusNotFound, "")
	if len(init.repos) != 2 {
		t.Fatalf("init calls: %d", len(init.repos))
	}
}

// initEnabled reports whether the page has an enabled "Initialize
// repository" button (it is always shown, but disabled unless restic
// reported that no repository exists at the location).
func initEnabled(body string) bool {
	return strings.Contains(body, `<button type="submit">Initialize repository</button>`)
}
