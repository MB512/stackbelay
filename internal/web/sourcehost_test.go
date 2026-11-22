package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// setupMigration: web and db on disk (setupStatus), a stack "cloud" imported
// from "old-host" (no directory yet), snapshots of both hosts. This host is
// "new-host".
func (e *testEnv) setupMigration(webID int64) (cloud store.Stack, fc *fakeContainers) {
	e.t.Helper()
	ctx := context.Background()
	fc = e.setupStatus()
	e.server.Hostname = "new-host"
	cloud, err := e.st.CreateImportedStack(ctx, "cloud", "old-host")
	if err != nil {
		e.t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id    int64
		snaps []restic.Snapshot
	}{
		{cloud.ID, []restic.Snapshot{{ID: "cccccccc11111111", ShortID: "cccccccc", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/cloud"}}}},
		{webID, []restic.Snapshot{
			{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "new-host", Paths: []string{"/s/web"}},
			{ID: "bbbbbbbb11111111", ShortID: "bbbbbbbb", Time: t0, Hostname: "old-host", Paths: []string{"/home/docker/web"}},
		}},
	} {
		repo, err := e.st.SetRepository(ctx, tc.id, "/srv/restic/"+itoa(tc.id), "pw", nil)
		if err != nil {
			e.t.Fatal(err)
		}
		if err := e.st.ReplaceSnapshots(ctx, repo, tc.snaps, t0); err != nil {
			e.t.Fatal(err)
		}
	}
	return cloud, fc
}

func TestSourceHostAndImportRestore(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	cloud, _ := e.setupMigration(webID)
	ctx := context.Background()
	e.loginAdmin()
	alice, aliceID := e.createUser("alice")
	for _, id := range []int64{webID, cloud.ID} {
		if err := e.st.SetPermission(ctx, store.Permission{UserID: aliceID, StackID: id, CanRestore: true}); err != nil {
			t.Fatal(err)
		}
	}

	// web lists this host's snapshots; the Settings tab offers old-host.
	r := e.get("/stacks/web/backups")
	if !strings.Contains(r.body, "aaaaaaaa") || strings.Contains(r.body, "bbbbbbbb") {
		t.Errorf("web lists the wrong snapshots:\n%s", r.body)
	}
	r = e.get("/stacks/web/settings")
	for _, want := range []string{`<option value="new-host" selected>new-host (this host)</option>`, `<option value="old-host">old-host</option>`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("settings lack %s:\n%s", want, r.body)
		}
	}
	token := e.csrf("/stacks/web/settings")
	expect(t, e.post("/stacks/web/source-host", url.Values{"csrf_token": {token}, "source_host": {"elsewhere"}}), http.StatusBadRequest, "")
	expect(t, e.post("/stacks/web/source-host", url.Values{"csrf_token": {token}, "source_host": {"old-host"}}),
		http.StatusSeeOther, "/stacks/web/settings")
	r = e.get("/stacks/web/backups")
	if !strings.Contains(r.body, "bbbbbbbb") || strings.Contains(r.body, "aaaaaaaa") || !strings.Contains(r.body, "lists the snapshots of host") {
		t.Errorf("web after switching to old-host:\n%s", r.body)
	}
	// Another host's snapshots: admins only.
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/restore?snapshot=bbbbbbbb11111111", nil, nil), http.StatusForbidden, "")
	if r := e.do(alice, http.MethodGet, "/stacks/web/backups", nil, nil); strings.Contains(r.body, "/restore?snapshot=") {
		t.Error("restore links for another host's snapshots shown to a non-admin")
	}
	r = e.post("/stacks/web/restore", url.Values{"csrf_token": {token}, "snapshot_id": {"bbbbbbbb11111111"},
		"images": {"pinned"}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("sync restore: %d\n%s", r.status, r.body)
	}
	j, _ := e.st.JobByID(ctx, strings.TrimPrefix(r.location, "/jobs/"))
	if j.Options.Import || !j.Options.NoStart || !j.Options.PinImages {
		t.Errorf("sync options %+v", j.Options)
	}
	if err := e.st.FinishJob(ctx, j.ID, jobs.StateSucceeded, "", "", store.JobResult{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Back to this host.
	expect(t, e.post("/stacks/web/source-host", url.Values{"csrf_token": {token}, "source_host": {"new-host"}}),
		http.StatusSeeOther, "/stacks/web/settings")
	if got, _ := e.st.StackByName(ctx, "web"); got.SourceHost != "" {
		t.Errorf("source host stored as %q, want this host (empty)", got.SourceHost)
	}

	// The imported stack: no directory, its first restore creates it.
	r = e.get("/stacks/cloud/restore?snapshot=cccccccc11111111")
	for _, want := range []string{"does not exist on this host yet", `name="start">`, `value="pinned" checked`,
		"numeric user and group ids", `name="limit_download"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("import restore page lacks %s:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, `name="method"`) || strings.Contains(r.body, `name="safety_snapshot"`) {
		t.Error("swap or safety snapshot offered for an import")
	}
	token = e.csrf("/stacks/cloud/backups")
	expect(t, e.post("/stacks/cloud/restore", url.Values{"csrf_token": {token}, "snapshot_id": {"cccccccc11111111"},
		"limit_download": {"lots"}}), http.StatusBadRequest, "")
	r = e.post("/stacks/cloud/restore", url.Values{"csrf_token": {token}, "snapshot_id": {"cccccccc11111111"},
		"images": {"pinned"}, "limit_download": {"5"}, "method": {"in-place"}, "safety_snapshot": {""}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("import restore: %d\n%s", r.status, r.body)
	}
	j, _ = e.st.JobByID(ctx, strings.TrimPrefix(r.location, "/jobs/"))
	want := store.JobOptions{SnapshotID: "cccccccc11111111", Import: true, NoStart: true, PinImages: true,
		LimitDownloadKiB: 5 * 1024}
	if j.Type != jobs.TypeRestore || j.Options != want {
		t.Errorf("import job %+v, options want %+v", j, want)
	}

	if n := auditActions(t, e.st)["stack.source_host"]; n != 2 {
		t.Errorf("stack.source_host audited %d times, want 2", n)
	}
}
