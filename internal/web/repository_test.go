package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	repo, err := f.st.RepositoryInfoForStack(ctx, stackID)
	if err != nil {
		return nil, err
	}
	if err := f.st.ReplaceSnapshots(ctx, repo.ID, f.snaps, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)); err != nil {
		return nil, err
	}
	return f.st.ListSnapshots(ctx, repo.ID)
}

func TestRepositoryForm(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	e.loginAdmin()
	ctx := context.Background()
	e.snaps.snaps = []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "h", Paths: []string{"/s/web"}},
	}
	token := e.csrf("/stacks/web/settings")
	save := func(v url.Values) response {
		v.Set("csrf_token", token)
		return e.post("/stacks/web/repository", v)
	}
	repo := func() store.Repository {
		t.Helper()
		r, err := e.st.RepositoryForStack(ctx, webID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// New repository from a REST URL with credentials.
	r := save(url.Values{"location": {"rest:https://alice:s3cr%2Ft@backup.example:8000/web"}, "password": {"repo-pw-1"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "restic found 1 snapshot.") || !strings.Contains(r.body, "RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD") {
		t.Fatalf("no success message:\n%s", r.body)
	}
	got := repo()
	wantEnv := map[string]string{"RESTIC_REST_USERNAME": "alice", "RESTIC_REST_PASSWORD": "s3cr/t"}
	if got.Location != "rest:https://backup.example:8000/web" || got.Password != "repo-pw-1" || !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("stored repository: %q %q %v", got.Location, got.Password, got.Env)
	}
	for _, secret := range []string{"s3cr", "repo-pw-1"} {
		if strings.Contains(r.body, secret) {
			t.Fatalf("settings page contains secret %q", secret)
		}
	}
	if !strings.Contains(r.body, "<code>RESTIC_REST_PASSWORD</code>, <code>RESTIC_REST_USERNAME</code>") {
		t.Fatal("stored variable names not shown")
	}

	// Empty password and variables keep the stored ones; the location
	// changes (and so does the snapshot cache).
	expect(t, save(url.Values{"location": {"rest:https://backup2.example/web"}}), http.StatusOK, "")
	if got := repo(); got.Location != "rest:https://backup2.example/web" || got.Password != "repo-pw-1" || !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("after keep: %q %q %v", got.Location, got.Password, got.Env)
	}

	// Variables replace all stored ones.
	expect(t, save(url.Values{"location": {"s3:s3.example/bucket"}, "password": {"repo-pw-2"},
		"env": {"# S3\r\nAWS_ACCESS_KEY_ID=AK\r\n\r\nAWS_SECRET_ACCESS_KEY=a=b c\r\n"}}), http.StatusOK, "")
	if got := repo(); got.Password != "repo-pw-2" ||
		!reflect.DeepEqual(got.Env, map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "a=b c"}) {
		t.Fatalf("after replace: %q %v", got.Password, got.Env)
	}
	// clear_env removes them.
	expect(t, save(url.Values{"location": {"/srv/restic/web"}, "clear_env": {"on"}}), http.StatusOK, "")
	if got := repo(); len(got.Env) != 0 {
		t.Fatalf("after clear: %v", got.Env)
	}

	// Refresh failure: saved anyway, error shown.
	e.snaps.err = fmt.Errorf("listing snapshots for stack 1: %w", &restic.Error{Op: "snapshots", Err: errors.New("exit status 1"),
		Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open config file"}`})
	r = save(url.Values{"location": {"/srv/restic/web2"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "restic could not open the repository: Fatal: unable to open config file") {
		t.Fatalf("no refresh error:\n%s", r.body)
	}
	if repo().Location != "/srv/restic/web2" {
		t.Fatal("repository not saved")
	}

	entries, _ := e.st.ListAudit(ctx, 100)
	n := 0
	for _, a := range entries {
		if a.Action == "repository.update" {
			n++
			for _, secret := range []string{"s3cr", "repo-pw", "AK"} {
				if strings.Contains(a.Detail, secret) {
					t.Fatalf("audit detail contains a secret: %s", a.Detail)
				}
			}
		}
	}
	if n != 5 {
		t.Fatalf("repository.update logged %d times", n)
	}
}

func TestRepositoryFormValidation(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.loginAdmin()
	token := e.csrf("/stacks/web/settings")
	tests := []struct {
		name    string
		form    url.Values
		wantMsg string
		secret  string // must not be echoed
	}{
		{"no password for a new repository", url.Values{"location": {"/r"}}, "password must not be empty", ""},
		{"unparseable URL", url.Values{"location": {"rest:https://alice:pa/ss@host/web"}, "password": {"x"}},
			"cannot be parsed", "pa/ss"},
		{"credentials in s3 URL", url.Values{"location": {"s3:https://ak:sk-secret@s3.example/b"}, "password": {"x"}},
			"Only rest: repository URLs", "sk-secret"},
		{"unsupported variable", url.Values{"location": {"/r"}, "password": {"x"}, "env": {"LD_PRELOAD=/evil.so"}},
			"not supported", "/evil.so"},
		{"password variable", url.Values{"location": {"/r"}, "password": {"x"}, "env": {"RESTIC_PASSWORD=other"}},
			"not supported", "other"},
		{"malformed line", url.Values{"location": {"/r"}, "password": {"x"}, "env": {"AWS_ACCESS_KEY_ID"}}, "KEY=value", ""},
		{"flag as location", url.Values{"location": {"--insecure-tls"}, "password": {"x"}}, "must not start with", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.form.Set("csrf_token", token)
			r := e.post("/stacks/web/repository", tc.form)
			expect(t, r, http.StatusBadRequest, "")
			if !strings.Contains(r.body, tc.wantMsg) {
				t.Fatalf("message %q missing:\n%s", tc.wantMsg, r.body)
			}
			if tc.secret != "" && strings.Contains(r.body, tc.secret) {
				t.Fatalf("response echoes %q", tc.secret)
			}
		})
	}
	if _, err := e.st.RepositoryInfoForStack(context.Background(), 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("invalid input saved a repository")
	}
	if e.snaps.calls != 0 {
		t.Fatal("refresh ran for invalid input")
	}
}

func TestRepositoryFormAdminOnly(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: webID, CanBackup: true, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	token := csrfOf(e, alice, "/stacks/web")
	r := e.do(alice, http.MethodPost, "/stacks/web/repository",
		url.Values{"csrf_token": {token}, "location": {"/r"}, "password": {"x"}}, nil)
	expect(t, r, http.StatusForbidden, "")
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
	if _, err := e.st.SetRepository(ctx, webID, "/srv/restic/web", "pw", nil); err != nil {
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
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); !strings.Contains(r.body, "Use Refresh to load the list") {
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
	expect(t, refresh("/stacks/web/snapshots/refresh", false), http.StatusSeeOther, "/stacks/web")
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
	token := e.csrf("/stacks/web/settings")
	post := func(path string, v url.Values) response {
		v.Set("csrf_token", token)
		return e.post(path, v)
	}

	// No repository at the location: the init button is offered.
	e.snaps.err = fmt.Errorf("listing snapshots: %w", resticExitError(t, 10, "Fatal: repository does not exist: unable to open config file"))
	r := post("/stacks/web/repository", url.Values{"location": {"rest:https://u:rest-pw@backup.example/web-2027"}, "password": {"repo-pw"}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "There is no restic repository at this location yet") ||
		!strings.Contains(r.body, `action="/stacks/web/repository/init"`) {
		t.Fatalf("init not offered:\n%s", r.body)
	}

	// Initialize: restic init gets the stored credentials, then the new
	// repository is checked.
	e.snaps.err = nil
	r = post("/stacks/web/repository/init", url.Values{})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "Repository initialized. restic found 0 snapshots.") || strings.Contains(r.body, "repository/init") {
		t.Fatalf("after init:\n%s", r.body)
	}
	want := []restic.Repo{{Location: "rest:https://backup.example/web-2027", Env: []string{
		"RESTIC_PASSWORD=repo-pw", "RESTIC_REST_PASSWORD=rest-pw", "RESTIC_REST_USERNAME=u"}}}
	if !reflect.DeepEqual(init.repos, want) {
		t.Fatalf("init calls: %+v", init.repos)
	}

	// Other errors (wrong password, network) do not offer init.
	e.snaps.err = resticExitError(t, 12, "Fatal: wrong password or no key found")
	r = post("/stacks/web/repository", url.Values{"location": {"/srv/restic/web"}})
	if strings.Contains(r.body, "repository/init") || !strings.Contains(r.body, "wrong password") {
		t.Fatalf("init offered for a wrong password:\n%s", r.body)
	}

	// restic refuses (e.g. a repository appeared meanwhile).
	init.err = &restic.Error{Op: "init", Err: errors.New("exit status 1"),
		Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: config file already exists"}`}
	r = post("/stacks/web/repository/init", url.Values{})
	if !strings.Contains(r.body, "restic could not initialize the repository: Fatal: config file already exists") {
		t.Fatalf("init failure:\n%s", r.body)
	}

	if n := auditActions(t, e.st)["repository.init"]; n != 2 {
		t.Fatalf("repository.init logged %d times", n)
	}

	// A stack without repository is refused.
	expect(t, post("/stacks/db/repository/init", url.Values{}), http.StatusBadRequest, "")
	if len(init.repos) != 2 {
		t.Fatalf("init calls: %d", len(init.repos))
	}
}

func TestInitRepositoryAdminOnly(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	init := &fakeInit{}
	e.server.Repos = init
	if _, err := e.st.SetRepository(context.Background(), webID, "/srv/restic/web", "pw", nil); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	if err := e.st.SetPermission(context.Background(), store.Permission{UserID: aliceID, StackID: webID, CanBackup: true, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	r := e.do(alice, http.MethodPost, "/stacks/web/repository/init", url.Values{"csrf_token": {csrfOf(e, alice, "/stacks/web")}}, nil)
	expect(t, r, http.StatusForbidden, "")
	if len(init.repos) != 0 {
		t.Fatal("init ran for a non-admin")
	}
}
