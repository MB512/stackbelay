package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/dispatch"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
)

// fakeQueue stores jobs like the dispatcher but never runs them.
type fakeQueue struct {
	st *store.Store

	mu        sync.Mutex
	n         int
	cancelled []string
}

func (q *fakeQueue) Submit(ctx context.Context, j store.Job) (store.Job, error) {
	q.mu.Lock()
	q.n++
	n := q.n
	q.mu.Unlock()
	j.QueuedAt = time.Date(2026, 11, 14, 10, 0, n, 0, time.UTC)
	id, err := jobs.NewID(j.QueuedAt)
	if err != nil {
		return store.Job{}, err
	}
	j.ID, j.State = id, jobs.StateQueued
	return j, q.st.CreateJob(ctx, j)
}

func (q *fakeQueue) Cancel(ctx context.Context, id string) error {
	j, err := q.st.JobByID(ctx, id)
	if err != nil {
		return err
	}
	if j.State.Final() {
		return dispatch.ErrNotCancellable
	}
	q.mu.Lock()
	q.cancelled = append(q.cancelled, id)
	q.mu.Unlock()
	return q.st.FinishJob(ctx, id, jobs.StateCancelled, "", "cancelled", store.JobResult{}, j.QueuedAt)
}

// setupJobsEnv: stacks with a repository and one cached snapshot on "web",
// admin logged in, alice with backup-only and bob with restore-only rights.
func setupJobsEnv(t *testing.T) (e *testEnv, alice, bob *http.Client) {
	t.Helper()
	e = newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := e.st.SetRepository(ctx, webID, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), Hostname: "h", Paths: []string{"/s/web"}},
	}, time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	bob, bobID := e.createUser("bob")
	for _, p := range []store.Permission{
		{UserID: aliceID, StackID: webID, CanBackup: true},
		{UserID: bobID, StackID: webID, CanRestore: true},
	} {
		if err := e.st.SetPermission(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	return e, alice, bob
}

const jobLocation = "/jobs/"

func TestBackupJob(t *testing.T) {
	e, alice, bob := setupJobsEnv(t)
	aliceToken := csrfOf(e, alice, "/stacks/web")

	// The Backups tab shows the button to alice (backup right) only.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); !strings.Contains(r.body, `action="/stacks/web/backup"`) {
		t.Fatal("no backup button for alice")
	}
	if r := e.do(bob, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, `action="/stacks/web/backup"`) {
		t.Fatal("backup button for bob")
	}

	r := e.do(alice, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {aliceToken}, "pre_stop": {"on"}}, nil)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
		t.Fatalf("backup: %d %q", r.status, r.location)
	}
	id := strings.TrimPrefix(r.location, jobLocation)
	j, err := e.st.JobByID(context.Background(), id)
	if err != nil || j.Type != jobs.TypeBackup || !j.Options.PreStop || j.Requester != "alice" || j.Trigger != store.TriggerManual {
		t.Fatalf("job: %+v %v", j, err)
	}

	// A second backup while one is queued is refused.
	r = e.do(alice, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {aliceToken}}, nil)
	if r.status != http.StatusConflict || !strings.Contains(r.body, "already waiting") {
		t.Fatalf("second backup: %d", r.status)
	}

	// Job page: visible for viewers of the stack, followed live while not final.
	r = e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil)
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `data-events="/jobs/`+id+`/events"`) || strings.Contains(r.body, "data-final") ||
		!strings.Contains(r.body, "/static/joblog.js") || !strings.Contains(r.body, "/cancel") {
		t.Fatalf("job page:\n%s", r.body)
	}
	if r := e.do(bob, http.MethodGet, "/jobs/"+id, nil, nil); r.status != http.StatusOK || strings.Contains(r.body, "/cancel") {
		t.Fatalf("bob may view but not cancel a backup: %d", r.status)
	}

	// Bob (restore only) may not start or cancel backups.
	bobToken := csrfOf(e, bob, "/stacks/web")
	expect(t, e.do(bob, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {bobToken}}, nil), http.StatusForbidden, "")
	expect(t, e.do(bob, http.MethodPost, "/jobs/"+id+"/cancel", url.Values{"csrf_token": {bobToken}}, nil), http.StatusForbidden, "")

	// Alice cancels; afterwards the page no longer polls.
	expect(t, e.do(alice, http.MethodPost, "/jobs/"+id+"/cancel", url.Values{"csrf_token": {aliceToken}}, nil), http.StatusSeeOther, "/jobs/"+id)
	if len(e.queue.cancelled) != 1 {
		t.Fatal("not cancelled")
	}
	r = e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil)
	if !strings.Contains(r.body, "data-final") || !strings.Contains(r.body, "cancelled") {
		t.Fatalf("cancelled job page:\n%s", r.body)
	}
	// Cancelling again just shows the job.
	expect(t, e.do(alice, http.MethodPost, "/jobs/"+id+"/cancel", url.Values{"csrf_token": {aliceToken}}, nil), http.StatusSeeOther, "/jobs/"+id)

	// Backups tab lists the job.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); !strings.Contains(r.body, `href="/jobs/`+id+`"`) {
		t.Fatal("job not listed on the Backups tab")
	}

	// Stacks without repository, hidden stacks and unknown jobs.
	adminToken := e.csrf("/stacks/db")
	if r := e.post("/stacks/db/backup", url.Values{"csrf_token": {adminToken}}); r.status != http.StatusConflict {
		t.Fatalf("backup without repository: %d", r.status)
	}
	expect(t, e.do(alice, http.MethodPost, "/stacks/db/backup", url.Values{"csrf_token": {aliceToken}}, nil), http.StatusNotFound, "")
	expect(t, e.do(alice, http.MethodGet, "/jobs/01AAAAAAAAAAAAAAAAAAAAAAAA", nil, nil), http.StatusNotFound, "")
	expect(t, e.do(alice, http.MethodGet, "/jobs/../../etc", nil, nil), http.StatusNotFound, "")

	actions := auditActions(t, e.st)
	if actions["job.create"] != 1 || actions["job.cancel"] != 1 {
		t.Fatalf("audit: %v", actions)
	}
}

func TestRestoreJob(t *testing.T) {
	e, alice, bob := setupJobsEnv(t)

	// Restore link and form for bob (restore right), not for alice.
	r := e.do(bob, http.MethodGet, "/stacks/web/backups", nil, nil)
	if !strings.Contains(r.body, "/stacks/web/restore?snapshot=aaaaaaaa11111111") {
		t.Fatalf("no restore link for bob:\n%s", r.body)
	}
	if r := e.do(alice, http.MethodGet, "/stacks/web/backups", nil, nil); strings.Contains(r.body, "/restore?") {
		t.Fatal("restore link for alice")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/restore?snapshot=aaaaaaaa11111111", nil, nil), http.StatusForbidden, "")

	r = e.do(bob, http.MethodGet, "/stacks/web/restore?snapshot=aaaaaaaa11111111", nil, nil)
	expect(t, r, http.StatusOK, "")
	if strings.Contains(r.body, `name="safety_snapshot"`) {
		t.Fatal("non-admin may not skip the safety snapshot")
	}
	if !strings.Contains(r.body, `value="in-place"`) {
		t.Fatal("no in-place option for a user with restore right")
	}
	for _, bad := range []string{"bbbbbbbb22222222", "../x", ""} {
		expect(t, e.do(bob, http.MethodGet, "/stacks/web/restore?snapshot="+url.QueryEscape(bad), nil, nil), http.StatusNotFound, "")
	}

	// Bob tries to skip the safety snapshot: ignored.
	token := csrfOf(e, bob, "/stacks/web")
	r = e.do(bob, http.MethodPost, "/stacks/web/restore",
		url.Values{"csrf_token": {token}, "snapshot_id": {"aaaaaaaa11111111"}, "start": {"on"}, "images": {"pinned"},
			"method": {"in-place"}}, nil)
	if r.status != http.StatusSeeOther {
		t.Fatalf("restore: %d\n%s", r.status, r.body)
	}
	j, _ := e.st.JobByID(context.Background(), strings.TrimPrefix(r.location, jobLocation))
	if j.Type != jobs.TypeRestore || j.Options.SnapshotID != "aaaaaaaa11111111" || j.Options.SkipSafetySnapshot || j.Options.NoStart ||
		!j.Options.PinImages || !j.Options.InPlace {
		t.Fatalf("restore job: %+v", j.Options)
	}
	expect(t, e.do(bob, http.MethodPost, "/stacks/web/restore",
		url.Values{"csrf_token": {token}, "snapshot_id": {"bbbbbbbb22222222"}}, nil), http.StatusBadRequest, "")

	// The admin may skip it and not start the stack.
	if err := e.st.FinishJob(context.Background(), j.ID, jobs.StateFailed, "", "x", store.JobResult{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r = e.post("/stacks/web/restore", url.Values{"csrf_token": {e.csrf("/stacks/web")}, "snapshot_id": {"aaaaaaaa11111111"}})
	j, _ = e.st.JobByID(context.Background(), strings.TrimPrefix(r.location, jobLocation))
	if !j.Options.SkipSafetySnapshot || !j.Options.NoStart || j.Options.PinImages || j.Options.InPlace {
		t.Fatalf("admin restore options: %+v", j.Options)
	}
}

func TestJobLog(t *testing.T) {
	e, alice, _ := setupJobsEnv(t)
	token := csrfOf(e, alice, "/stacks/web")
	r := e.do(alice, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {token}}, nil)
	id := strings.TrimPrefix(r.location, jobLocation)
	dir := filepath.Join(e.server.JobsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := `{"time":"2026-11-14T10:00:01.5Z","level":"INFO","msg":"stopping stack","job_id":"` + id + `","services":["app"]}
{"time":"2026-11-14T10:00:02Z","level":"ERROR","msg":"<script>alert(1)</script>"}
not json
`
	if err := os.WriteFile(filepath.Join(dir, jobs.LogFile), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil)
	for _, want := range []string{"10:00:01", "stopping stack", "services=[app]", "not json", "&lt;script&gt;"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if strings.Contains(r.body, "<script>alert") {
		t.Fatal("log is not escaped")
	}
}

func TestReadLogTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.txt")
	line := `{"time":"2026-11-14T10:00:00Z","level":"INFO","msg":"` + strings.Repeat("x", 100) + `"}` + "\n"
	content := strings.Repeat(line, maxLogBytes/len(line)+10) + `{"msg":"half writ` // the worker is still writing
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, cut, next, err := readLogTail(p, time.UTC)
	if err != nil || !cut || len(lines) == 0 || lines[0].Level != "INFO" {
		t.Fatalf("cut=%v lines=%d err=%v", cut, len(lines), err)
	}
	// The incomplete last line is left for the live stream.
	if want := int64(len(content) - len(`{"msg":"half writ`)); next != want || lines[len(lines)-1].End != want {
		t.Fatalf("next=%d, last end=%d, want %d", next, lines[len(lines)-1].End, want)
	}
}

// A repository shared by several stacks and hosts: only the stack's own
// snapshots from this host are listed and restorable.
func TestSharedRepositoryShowsOwnSnapshotsOnly(t *testing.T) {
	e := newEnv(t, true, true)
	e.server.Hostname = "host-a"
	webID, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := e.st.SetRepository(ctx, webID, "/srv/restic/shared", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "host-a", Paths: []string{"/s/web"}},
		{ID: "bbbbbbbb22222222", ShortID: "bbbbbbbb", Time: t0, Hostname: "host-a", Paths: []string{"/s/db"}},
		{ID: "cccccccc33333333", ShortID: "cccccccc", Time: t0, Hostname: "host-b", Paths: []string{"/s/web"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()

	r := e.get("/stacks/web/backups")
	if !strings.Contains(r.body, "aaaaaaaa") || strings.Contains(r.body, "bbbbbbbb") || strings.Contains(r.body, "cccccccc") {
		t.Fatalf("snapshot list:\n%s", r.body)
	}
	if !strings.Contains(r.body, "2 other snapshots in this repository belong to other stacks or hosts") {
		t.Fatal("no hint about hidden snapshots")
	}
	if r := e.get("/"); !strings.Contains(r.body, "<td>1</td>") {
		t.Fatalf("stack list must count 1 snapshot:\n%s", r.body)
	}
	expect(t, e.get("/stacks/web/restore?snapshot=bbbbbbbb22222222"), http.StatusNotFound, "")
	expect(t, e.post("/stacks/web/restore", url.Values{"csrf_token": {e.csrf("/stacks/web")}, "snapshot_id": {"cccccccc33333333"}}),
		http.StatusBadRequest, "")
	expect(t, e.get("/stacks/web/restore?snapshot=aaaaaaaa11111111"), http.StatusOK, "")
}
