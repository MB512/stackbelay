package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// fakeRepoLister answers "restic snapshots" for any repository and records
// the repositories it was asked about.
type fakeRepoLister struct {
	mu    sync.Mutex
	snaps []restic.Snapshot
	repos []restic.Repo
}

func (f *fakeRepoLister) Snapshots(_ context.Context, repo restic.Repo, _ ...string) ([]restic.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	return f.snaps, nil
}

var importToken = regexp.MustCompile(`name="token" value="([0-9a-f]{32})"`)

func TestImportStack(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	lister := &fakeRepoLister{snaps: []restic.Snapshot{
		{ID: "a1", ShortID: "a1", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/cloud"}},
		{ID: "a2", ShortID: "a2", Time: t0.Add(time.Hour), Hostname: "old-host", Paths: []string{"/home/docker/cloud"}},
		{ID: "b1", ShortID: "b1", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/web"}},     // web exists here
		{ID: "c1", ShortID: "c1", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/.hidden"}}, // invalid name
		{ID: "d1", ShortID: "d1", Time: t0, Hostname: "old-host", Paths: []string{"/a", "/b"}},             // not a stack
		{ID: "e1", ShortID: "e1", Time: t0, Hostname: "new-host", Paths: []string{"/srv/stacks/wiki"}},
	}}
	e := newEnvWith(t, true, true, func(s *Server) { s.RepoSnapshots = lister; s.Hostname = "new-host" })
	e.setupStacks()
	e.setupLogs() // web and db on disk
	// A directory without a stack record also blocks the name.
	if err := os.MkdirAll(filepath.Join(e.server.StacksRoot, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.loginAdmin()

	r := e.get("/admin/import")
	if !strings.Contains(r.body, `aria-current="page">Import stack</a>`) || !strings.Contains(r.body, `action="/admin/import"`) {
		t.Fatalf("import tab:\n%s", r.body)
	}
	token := e.csrf("/admin/import")
	r = e.post("/admin/import", url.Values{"csrf_token": {token},
		"name": {"old-nas"}, "type": {"other"}, "location": {"rest:https://demo:rest-secret@backup.example/cloud"}, "password": {"repo-secret"}})
	expect(t, r, http.StatusOK, "")
	for _, secret := range []string{"rest-secret", "repo-secret"} {
		if strings.Contains(r.body, secret) {
			t.Fatalf("page contains %q", secret)
		}
	}
	m := importToken.FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no token:\n%s", r.body)
	}
	// cloud (2 snapshots) can be imported; web and wiki are taken; the
	// invalid and multi-path snapshots are not offered.
	rows := strings.Split(r.body[strings.Index(r.body, "<tbody>"):strings.Index(r.body, "</tbody>")], "<tr>")
	var offered, taken []string
	for _, row := range rows[1:] {
		name := regexp.MustCompile(`<td>([a-z.0-9-]+)`).FindStringSubmatch(row)[1]
		if strings.Contains(row, " disabled") {
			taken = append(taken, name)
		} else {
			offered = append(offered, name)
		}
	}
	if strings.Join(offered, ",") != "cloud" || strings.Join(taken, ",") != "web,wiki" {
		t.Fatalf("offered %v, taken %v\n%s", offered, taken, r.body)
	}
	if !strings.Contains(r.body, "<td>2</td>") || !strings.Contains(r.body, "(this host)") {
		t.Errorf("candidate details missing:\n%s", r.body)
	}
	lister.mu.Lock()
	got := lister.repos[0]
	lister.mu.Unlock()
	if got.Location != "rest:https://backup.example/cloud" || !strings.Contains(strings.Join(got.Env, " "), "RESTIC_REST_PASSWORD=rest-secret") {
		t.Fatalf("restic asked about %+v", got.Location)
	}

	// Another user cannot use the token; a taken name cannot be chosen.
	other, _ := e.createUser("mallory")
	expect(t, e.do(other, http.MethodPost, "/admin/import/confirm", url.Values{"csrf_token": {csrfOf(e, other, "/")},
		"token": {m[1]}, "choice": {"0"}}, nil), http.StatusForbidden, "")
	webIndex := "1" // sorted: cloud, web, wiki
	r = e.post("/admin/import/confirm", url.Values{"csrf_token": {token}, "token": {m[1]}, "choice": {webIndex}})
	if r.status != http.StatusConflict {
		t.Fatalf("importing a taken name: %d", r.status)
	}
	// The token is used up (also by a refused attempt): list again.
	expect(t, e.post("/admin/import/confirm", url.Values{"csrf_token": {token}, "token": {m[1]}, "choice": {"0"}}),
		http.StatusBadRequest, "")
	r = e.post("/admin/import", url.Values{"csrf_token": {token},
		"name": {"old-nas"}, "type": {"other"}, "location": {"rest:https://demo:rest-secret@backup.example/cloud"}, "password": {"repo-secret"}})
	m = importToken.FindStringSubmatch(r.body)
	expect(t, e.post("/admin/import/confirm", url.Values{"csrf_token": {token}, "token": {m[1]}, "choice": {"0"}}),
		http.StatusSeeOther, "/stacks/cloud/backups")

	st, err := e.st.StackByName(ctx, "cloud")
	if err != nil || st.Present || st.SourceHost != "old-host" {
		t.Fatalf("imported stack %+v, %v", st, err)
	}
	repo, err := e.st.RepositoryForStack(ctx, st.ID)
	if err != nil || repo.Name != "old-nas" || repo.Location != "rest:https://backup.example/cloud" || repo.Password != "repo-secret" ||
		repo.Env["RESTIC_REST_PASSWORD"] != "rest-secret" {
		t.Fatalf("repository %+v, %v", repo.Location, err)
	}
	if repos, _ := e.st.ListRepositories(ctx); len(repos) != 1 {
		t.Fatalf("repositories: %+v", repos) // the refused attempts did not save it
	}
	e.snaps.mu.Lock()
	refreshed := e.snaps.calls
	e.snaps.mu.Unlock()
	if refreshed == 0 {
		t.Error("snapshot cache not refreshed after the import")
	}
	if n := auditActions(t, e.st); n["stack.import"] != 1 || n["repository.create"] != 1 {
		t.Errorf("audit: %v", n)
	}
	for _, a := range auditDetails(t, e.st) {
		if strings.Contains(a, "secret") {
			t.Fatalf("audit entry contains a secret: %s", a)
		}
	}

	// Not for other users.
	expect(t, e.do(other, http.MethodGet, "/admin/import", nil, nil), http.StatusForbidden, "")
}

// auditDetails returns the details of all audit entries.
func auditDetails(t *testing.T, st *store.Store) []string {
	t.Helper()
	entries, err := st.ListAudit(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, a := range entries {
		out = append(out, a.Detail)
	}
	return out
}

// An import from a configured repository uses its stored credentials and
// adds no repository.
func TestImportFromConfiguredRepository(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	lister := &fakeRepoLister{snaps: []restic.Snapshot{
		{ID: "a1", ShortID: "a1", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/cloud"}},
	}}
	e := newEnvWith(t, true, true, func(s *Server) { s.RepoSnapshots = lister; s.Hostname = "new-host" })
	e.setupStacks()
	e.setupLogs() // stacks root with web and db
	ctx := context.Background()
	nas, err := e.st.CreateRepository(ctx, "nas", "/srv/restic/nas", "stored-pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	token := e.csrf("/admin/import")
	r := e.get("/admin/import")
	if !strings.Contains(r.body, `<option value="`+strconv.FormatInt(nas, 10)+`">nas</option>`) {
		t.Fatalf("configured repositories not offered:\n%s", r.body)
	}

	// A new repository may not take the name of a configured one.
	r = e.post("/admin/import", url.Values{"csrf_token": {token}, "name": {"nas"}, "type": {"other"}, "location": {"/x"}, "password": {"pw"}})
	expect(t, r, http.StatusConflict, "")
	expect(t, e.post("/admin/import", url.Values{"csrf_token": {token}, "repository": {"999"}}), http.StatusBadRequest, "")

	r = e.post("/admin/import", url.Values{"csrf_token": {token}, "repository": {strconv.FormatInt(nas, 10)}})
	expect(t, r, http.StatusOK, "")
	m := importToken.FindStringSubmatch(r.body)
	if m == nil || !strings.Contains(r.body, "in nas (<code>/srv/restic/nas</code>)") {
		t.Fatalf("no candidates:\n%s", r.body)
	}
	lister.mu.Lock()
	got := lister.repos[len(lister.repos)-1]
	lister.mu.Unlock()
	if got.Location != "/srv/restic/nas" || !slices.Contains(got.Env, "RESTIC_PASSWORD=stored-pw") {
		t.Fatalf("restic asked about %+v", got.Location)
	}
	expect(t, e.post("/admin/import/confirm", url.Values{"csrf_token": {token}, "token": {m[1]}, "choice": {"0"}}),
		http.StatusSeeOther, "/stacks/cloud/backups")
	st, err := e.st.StackByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	if ri, err := e.st.RepositoryInfoForStack(ctx, st.ID); err != nil || ri.ID != nas {
		t.Fatalf("repository of the imported stack: %+v, %v", ri, err)
	}
	if repos, _ := e.st.ListRepositories(ctx); len(repos) != 1 {
		t.Fatalf("repositories: %+v", repos)
	}
}

// A stack whose directory is gone can be forgotten on its Settings tab;
// Import stack then reuses its record (issue #38).
func TestForgetStackThenImport(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	lister := &fakeRepoLister{snaps: []restic.Snapshot{
		{ID: "a1", ShortID: "a1", Time: t0, Hostname: "host-a", Paths: []string{"/home/docker/old"}},
	}}
	e := newEnvWith(t, true, true, func(s *Server) { s.RepoSnapshots = lister; s.Hostname = "new-host" })
	e.setupLogs() // stacks root with web and db
	ctx := context.Background()
	if err := e.st.SyncStacks(ctx, []string{"web", "db", "old"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SyncStacks(ctx, []string{"web", "db"}); err != nil { // old's directory is gone
		t.Fatal(err)
	}
	old, _ := e.st.StackByName(ctx, "old")
	repo, err := setRepo(ctx, e.st, old.ID, "/srv/restic/old", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateSchedule(ctx, store.Schedule{StackID: old.ID, CronExpr: "@daily", Type: jobs.TypeBackup, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	token := e.csrf("/admin/import")

	// The import refuses the name and links to the stack.
	r := e.post("/admin/import", url.Values{"csrf_token": {token}, "repository": {strconv.FormatInt(repo, 10)}})
	if !strings.Contains(r.body, "forget it on its Settings tab") || !strings.Contains(r.body, `<a href="/stacks/old/settings">Settings</a>`) ||
		!strings.Contains(r.body, `aria-label="Import old from host-a" disabled required`) {
		t.Fatalf("import of a stack with old setup:\n%s", r.body)
	}

	// Only a stack without directory can be forgotten.
	if r := e.get("/stacks/web/settings"); strings.Contains(r.body, "Forget stack") {
		t.Fatal("forget offered for a present stack")
	}
	expect(t, e.post("/stacks/web/forget", url.Values{"csrf_token": {token}}), http.StatusConflict, "")
	if r := e.get("/stacks/old/settings"); !strings.Contains(r.body, `action="/stacks/old/forget"`) {
		t.Fatalf("forget not offered:\n%s", r.body)
	}
	r = e.post("/stacks/old/forget", url.Values{"csrf_token": {token}})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "Import stack can import this name again.") {
		t.Fatalf("after forget:\n%s", r.body)
	}
	if auditActions(t, e.st)["stack.forget"] != 1 {
		t.Error("forget not audited")
	}

	// Now the import offers it and reuses the record.
	r = e.post("/admin/import", url.Values{"csrf_token": {token}, "repository": {strconv.FormatInt(repo, 10)}})
	m := importToken.FindStringSubmatch(r.body)
	if m == nil || !strings.Contains(r.body, "reuses the forgotten record") {
		t.Fatalf("import after forget:\n%s", r.body)
	}
	expect(t, e.post("/admin/import/confirm", url.Values{"csrf_token": {token}, "token": {m[1]}, "choice": {"0"}}),
		http.StatusSeeOther, "/stacks/old/backups")
	st, err := e.st.StackByName(ctx, "old")
	if err != nil || st.ID != old.ID || st.SourceHost != "host-a" || st.SeenPresent {
		t.Fatalf("reused record %+v, %v", st, err)
	}
	if scheds, _ := e.st.ListSchedules(ctx, old.ID); len(scheds) != 0 {
		t.Fatalf("old schedules came back: %v", scheds)
	}
}
