package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
)

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		300 * time.Millisecond:      "<1 s",
		45 * time.Second:            "45 s",
		90 * time.Second:            "1 min 30 s",
		3 * time.Minute:             "3 min",
		2*time.Hour + 5*time.Minute: "2 h 5 min",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// The Backups tab shows what each snapshot added and how long it took,
// and the stack size at the last backup.
func TestSnapshotSizes(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := e.st.SetRepository(ctx, webID, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "h", Paths: []string{"/s/web"},
			Summary: &restic.SnapshotSummary{BackupStart: t0, BackupEnd: t0.Add(90 * time.Second),
				DataAddedPacked: 2 << 20, TotalBytesProcessed: 315 << 20, TotalFilesProcessed: 1200}},
		{ID: "bbbbbbbb11111111", ShortID: "bbbbbbbb", Time: t0.Add(-24 * time.Hour), Hostname: "h", Paths: []string{"/s/web"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	r := e.get("/stacks/web/backups")
	for _, want := range []string{
		"Stack size at the last backup: <strong>315.0 MiB</strong>", "1200 files", "the backup took 1 min 30 s",
		`<td title="315.0 MiB in 1200 files backed up">2.0 MiB</td>`, "<td>1 min 30 s</td>",
		`title="Made before restic 0.17: no size recorded">–</span>`, "not the size of\n    the stack",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Backups tab lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(r.body)
	}
}

func TestJobDuration(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		j    store.Job
		want string
	}{
		{store.Job{QueuedAt: t0}, ""},
		{store.Job{QueuedAt: t0, StartedAt: t0, FinishedAt: t0.Add(75 * time.Second)}, "1 min 15 s"},
		{store.Job{QueuedAt: t0, StartedAt: t0.Add(-5 * time.Minute)}, "running for 5 min"},
	} {
		if got := jobDuration(tc.j, t0); got != tc.want {
			t.Errorf("jobDuration(%+v) = %q, want %q", tc.j, got, tc.want)
		}
	}
}

// Job lists and the job page show how long a job ran.
func TestJobDurationsShown(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	t0 := e.clock()
	if err := e.st.CreateJob(ctx, store.Job{ID: "01AAAAAAAAAAAAAAAAAAAAAAA1", StackID: webID, Type: jobs.TypeStop,
		Trigger: store.TriggerManual, QueuedAt: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.st.ClaimNextJob(ctx, 1, t0.Add(-time.Hour)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := e.st.FinishJob(ctx, "01AAAAAAAAAAAAAAAAAAAAAAA1", jobs.StateSucceeded, "", "", store.JobResult{}, t0.Add(-time.Hour+42*time.Second)); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	for _, path := range []string{"/stacks/web", "/admin/jobs", "/jobs/01AAAAAAAAAAAAAAAAAAAAAAA1"} {
		if r := e.get(path); !strings.Contains(r.body, "42 s") {
			t.Errorf("%s does not show the duration:\n%s", path, r.body)
		}
	}
}

func TestMeasureDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", 100)
	write("data/a.bin", 1000)
	write("data/sub/b.bin", 24)
	// A link to something big outside is not followed.
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(big, filepath.Join(dir, "data", "link")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	r, err := measureDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Bytes != 1124 || r.Files != 3 {
		t.Fatalf("measured %d bytes in %d files, want 1124 in 3", r.Bytes, r.Files)
	}
	if os.Geteuid() != 0 && (r.Unreadable != 1 || r.FirstError == "") {
		t.Fatalf("unreadable %d (%q), want the locked directory", r.Unreadable, r.FirstError)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := measureDir(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk: %v", err)
	}
}

func TestCalculateStackSize(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.setupLogs() // creates web and db directories with a compose file
	if err := os.WriteFile(filepath.Join(e.server.StacksRoot, "web", "data.bin"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	r := e.get("/stacks/web/backups")
	if !strings.Contains(r.body, "not calculated") || !strings.Contains(r.body, `action="/stacks/web/size"`) {
		t.Fatalf("Backups tab without a size:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/backups")
	htmx := map[string]string{"HX-Request": "true"}
	r = e.do(e.client, http.MethodPost, "/stacks/web/size", url.Values{"csrf_token": {token}}, htmx)
	expect(t, r, http.StatusOK, "")
	if !strings.HasPrefix(strings.TrimSpace(r.body), `<div id="stack-size">`) || !strings.Contains(r.body, "<strong>3.0 MiB</strong> (2 files") {
		t.Fatalf("size fragment:\n%s", r.body)
	}
	// Right after: not again.
	r = e.do(e.client, http.MethodPost, "/stacks/web/size", url.Values{"csrf_token": {token}}, htmx)
	if !strings.Contains(r.body, "calculated just now") {
		t.Errorf("second calculation not refused:\n%s", r.body)
	}
	// The result stays on the page; the action is audited.
	if r := e.get("/stacks/web/backups"); !strings.Contains(r.body, "<strong>3.0 MiB</strong>") {
		t.Error("result not kept")
	}
	if auditActions(t, e.st)["stack.size"] != 1 {
		t.Error("calculation not audited once")
	}
	// Without directory: the button is disabled, the action says why.
	if err := os.RemoveAll(filepath.Join(e.server.StacksRoot, "db")); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/stacks/db/backups"); !strings.Contains(r.body, `disabled title="The stack directory is missing">Calculate`) {
		t.Errorf("Calculate not disabled for a missing directory:\n%s", r.body)
	}
	r = e.do(e.client, http.MethodPost, "/stacks/db/size", url.Values{"csrf_token": {token}}, htmx)
	if !strings.Contains(r.body, "The stack directory is missing.") {
		t.Errorf("missing directory:\n%s", r.body)
	}
}

// Pre-stop snapshots are hidden and added to the snapshot after them;
// listed on request, and only admins may restore them (issue #35).
func TestPreStopSnapshotsFolded(t *testing.T) {
	e, _, bob := setupJobsEnv(t) // bob has the restore right on web
	ctx := context.Background()
	repo, err := e.st.RepositoryInfoForStack(ctx, func() int64 { st, _ := e.st.StackByName(ctx, "web"); return st.ID }())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC) // after the cached snapshot of setupJobsEnv
	sum := func(start time.Time, d time.Duration, added uint64) *restic.SnapshotSummary {
		return &restic.SnapshotSummary{BackupStart: start, BackupEnd: start.Add(d), DataAddedPacked: added,
			TotalBytesProcessed: 100 << 20, TotalFilesProcessed: 10}
	}
	if err := e.st.ReplaceSnapshots(ctx, repo.ID, []restic.Snapshot{
		{ID: "cccccccc11111111", ShortID: "cccccccc", Time: t0, Hostname: "h", Paths: []string{"/s/web"},
			Tags: []string{jobs.TagPreStop}, Summary: sum(t0, 2*time.Minute, 3<<20)},
		{ID: "dddddddd11111111", ShortID: "dddddddd", Time: t0.Add(3 * time.Minute), Hostname: "h", Paths: []string{"/s/web"},
			Tags: []string{jobs.TagFinal}, Summary: sum(t0.Add(3*time.Minute), 10*time.Second, 1<<20)},
	}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	r := e.do(bob, http.MethodGet, "/stacks/web/backups", nil, nil)
	for _, want := range []string{"4.0 MiB", "2 min 10 s", "Stack stopped for 10 s", "Show pre-stop snapshots"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Backups tab lacks %q", want)
		}
	}
	if strings.Contains(r.body, "cccccccc") {
		t.Error("pre-stop snapshot listed by default")
	}

	r = e.do(bob, http.MethodGet, "/stacks/web/backups?prestop=1", nil, nil)
	if !strings.Contains(r.body, "<code>cccccccc</code>") || !strings.Contains(r.body, "not consistent") ||
		strings.Contains(r.body, "restore?snapshot=cccccccc11111111") {
		t.Errorf("pre-stop list for a non-admin:\n%s", r.body)
	}
	expect(t, e.do(bob, http.MethodGet, "/stacks/web/restore?snapshot=cccccccc11111111", nil, nil), http.StatusForbidden, "")
	token := csrfOf(e, bob, "/stacks/web")
	expect(t, e.do(bob, http.MethodPost, "/stacks/web/restore",
		url.Values{"csrf_token": {token}, "snapshot_id": {"cccccccc11111111"}}, nil), http.StatusForbidden, "")

	// Admins may, with a warning.
	if r := e.get("/stacks/web/backups?prestop=1"); !strings.Contains(r.body, "restore?snapshot=cccccccc11111111") {
		t.Error("no restore link for the admin")
	}
	if r := e.get("/stacks/web/restore?snapshot=cccccccc11111111"); r.status != http.StatusOK ||
		!strings.Contains(r.body, "This is a pre-stop snapshot") {
		t.Errorf("admin restore page: %d", r.status)
	}
}
