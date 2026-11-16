package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// fakeValidator rejects compose files that contain "invalid".
type fakeValidator struct {
	mu    sync.Mutex
	calls []string // "dir name"
}

func (f *fakeValidator) ValidateCompose(_ context.Context, dir, name string, content []byte) error {
	f.mu.Lock()
	f.calls = append(f.calls, dir+" "+name)
	f.mu.Unlock()
	if strings.Contains(string(content), "invalid") {
		return fmt.Errorf("%w: validating %s: services.app.imagee false schema", docker.ErrInvalidCompose, name)
	}
	return nil
}

const oldCompose = "\nservices:\n  app:\n    image: nginx:1.26\n" // leading empty line on purpose

func TestComposeEditor(t *testing.T) {
	e, alice, _ := setupJobsEnv(t) // web has a repository; alice may back up web
	e.setupLogs()                  // stack directories for web and db
	v := &fakeValidator{}
	e.server.Compose = v
	webFile := filepath.Join(e.server.StacksRoot, "web", "compose.yaml")
	if err := os.WriteFile(webFile, []byte(oldCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Admins only: no tab and no access for other users.
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, `/stacks/web/compose"`) {
		t.Fatal("Compose tab shown to a non-admin")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/compose", nil, nil), http.StatusForbidden, "")
	aliceToken := csrfOf(e, alice, "/stacks/web")
	expect(t, e.do(alice, http.MethodPost, "/stacks/web/compose",
		url.Values{"csrf_token": {aliceToken}, "content": {"services: {}\n"}}, nil), http.StatusForbidden, "")

	// The editor shows the file (the leading empty line survives).
	r := e.get("/stacks/web/compose")
	expect(t, r, http.StatusOK, "")
	base := jobs.SHA256Hex([]byte(oldCompose))
	if !strings.Contains(r.body, "rows=\"30\">\n\nservices:") || !strings.Contains(r.body, `value="`+base+`"`) ||
		!strings.Contains(r.body, "<code>compose.yaml</code>") || !strings.Contains(r.body, `aria-current="page">Compose`) {
		t.Fatalf("editor page:\n%s", r.body)
	}
	token := e.csrf("/stacks/web/compose")
	post := func(stack, content, baseSum string, extra ...string) response {
		form := url.Values{"csrf_token": {token}, "content": {content}, "base_sha256": {baseSum}}
		for i := 0; i < len(extra); i += 2 {
			form.Set(extra[i], extra[i+1])
		}
		return e.post("/stacks/"+stack+"/compose", form)
	}

	// Rejected by compose: message shown, text kept, no job.
	r = post("web", "services:\n  app:\n    imagee: invalid\n", base)
	if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "docker compose rejected the file: validating compose.yaml") ||
		!strings.Contains(r.body, "imagee: invalid") {
		t.Fatalf("invalid: %d\n%s", r.status, r.body)
	}
	// Unchanged content (sent with CRLF like a browser) is not applied.
	if r = post("web", strings.ReplaceAll(oldCompose, "\n", "\r\n"), base); !strings.Contains(r.body, "There are no changes.") {
		t.Fatalf("no changes: %d", r.status)
	}

	// Valid change: CRLF normalized, job with content and checksums.
	newContent := "services:\n  app:\n    image: nginx:1.27\n"
	r = post("web", strings.ReplaceAll(newContent, "\n", "\r\n"), base)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, jobLocation) {
		t.Fatalf("apply: %d %s", r.status, r.body)
	}
	id := strings.TrimPrefix(r.location, jobLocation)
	j, err := e.st.JobByID(ctx, id)
	if err != nil || j.Type != jobs.TypeComposeApply || j.Options.ComposeFile != "compose.yaml" || j.Options.NoBackup ||
		j.Options.ComposeBaseSHA256 != base || j.Options.ComposeSHA256 != jobs.SHA256Hex([]byte(newContent)) {
		t.Fatalf("job %+v, %v", j, err)
	}
	if content, err := e.st.JobComposeContent(ctx, id); err != nil || string(content) != newContent {
		t.Fatalf("job content %q, %v", content, err)
	}
	if got, _ := os.ReadFile(webFile); string(got) != oldCompose {
		t.Fatal("the manager changed the compose file itself")
	}
	if len(v.calls) != 2 || v.calls[1] != filepath.Join(e.server.StacksRoot, "web")+" compose.yaml" {
		t.Fatalf("validator calls %v", v.calls)
	}
	// Job page: admin may cancel, alice may view only.
	r = e.get("/jobs/" + id)
	if !strings.Contains(r.body, "<h1>Compose change of") || !strings.Contains(r.body, "/cancel") ||
		!strings.Contains(r.body, "compose.previous") {
		t.Fatalf("job page:\n%s", r.body)
	}
	if r := e.do(alice, http.MethodGet, "/jobs/"+id, nil, nil); r.status != http.StatusOK || strings.Contains(r.body, "/cancel") {
		t.Fatalf("alice on the job page: %d", r.status)
	}
	expect(t, e.do(alice, http.MethodPost, "/jobs/"+id+"/cancel", url.Values{"csrf_token": {aliceToken}}, nil), http.StatusForbidden, "")

	// The file changed on disk since the editor was opened: refused, text kept.
	if err := os.WriteFile(webFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = post("web", "services:\n  mine: {}\n", base)
	if r.status != http.StatusConflict || !strings.Contains(r.body, "was changed after you opened it") ||
		!strings.Contains(r.body, "mine: {}") || !strings.Contains(r.body, `value="`+base+`"`) {
		t.Fatalf("conflict: %d\n%s", r.status, r.body)
	}

	// Without repository (db): confirmation required, job without backup.
	dbFile := filepath.Join(e.server.StacksRoot, "db", "compose.yaml")
	dbBase := jobs.SHA256Hex([]byte("services: {}\n"))
	r = e.get("/stacks/db/compose")
	if !strings.Contains(r.body, "no backup and no rollback") || !strings.Contains(r.body, `name="confirm_no_backup"`) {
		t.Fatalf("db editor:\n%s", r.body)
	}
	if r = post("db", "services:\n  x: {}\n", dbBase); r.status != http.StatusBadRequest {
		t.Fatalf("unconfirmed: %d", r.status)
	}
	r = post("db", "services:\n  x: {}\n", dbBase, "confirm_no_backup", "on")
	if r.status != http.StatusSeeOther {
		t.Fatalf("confirmed: %d %s", r.status, r.body)
	}
	if j, _ := e.st.JobByID(ctx, strings.TrimPrefix(r.location, jobLocation)); !j.Options.NoBackup {
		t.Fatalf("db job %+v", j)
	}
	_ = dbFile

	// Size and text checks.
	if r = post("web", "services: {}\n"+strings.Repeat("#", jobs.MaxComposeBytes), jobs.SHA256Hex([]byte("services: {}\n"))); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("too large: %d", r.status)
	}
	if r = post("web", "  \n", jobs.SHA256Hex([]byte("services: {}\n"))); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("empty: %d", r.status)
	}
	// Beyond the editor's form limit: a clear answer, not a CSRF error.
	if r = post("web", strings.Repeat("x", 1<<20), "x"); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge post: %d", r.status)
	}

	actions := auditActions(t, e.st)
	if actions["job.create"] != 2 {
		t.Fatalf("audit %v", actions)
	}
	entries, _ := e.st.ListAudit(ctx, 100)
	for _, en := range entries {
		if strings.Contains(en.Detail, "nginx") {
			t.Fatal("audit contains the compose content")
		}
	}
}

func TestComposeEditorUnavailable(t *testing.T) {
	e := newEnv(t, true, true)
	e.setupStacks()
	e.setupLogs()
	e.loginAdmin()
	big := filepath.Join(e.server.StacksRoot, "web", "compose.yaml")
	if err := os.WriteFile(big, []byte(strings.Repeat("#", jobs.MaxComposeBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.get("/stacks/web/compose")
	if !strings.Contains(r.body, "larger than 256 KiB") || strings.Contains(r.body, "<textarea") {
		t.Fatalf("large file:\n%s", r.body)
	}
	if err := os.RemoveAll(filepath.Join(e.server.StacksRoot, "db")); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/stacks/db/compose"); !strings.Contains(r.body, "The stack directory is missing.") {
		t.Fatalf("missing dir:\n%s", r.body)
	}
	if !regexp.MustCompile(`href="/stacks/web/compose"`).MatchString(e.get("/stacks/web").body) {
		t.Fatal("Compose tab missing for admins")
	}
	_ = store.Job{}
}
