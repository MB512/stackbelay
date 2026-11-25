package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/notify"
	"github.com/MB512/stackbelay/internal/store"
)

type fakeNotifySender struct {
	targets []notify.Target
	sent    []notify.Message
	err     error
}

func (f *fakeNotifySender) Send(_ context.Context, t notify.Target, m notify.Message) error {
	if f.err != nil {
		return f.err
	}
	f.targets, f.sent = append(f.targets, t), append(f.sent, m)
	return nil
}

func TestNotificationSettings(t *testing.T) {
	sender := &fakeNotifySender{}
	e := newEnvWith(t, true, true, func(s *Server) { s.Notify, s.Hostname = sender, "nas01.home.example" })
	ctx := context.Background()
	e.loginAdmin()

	r := e.get("/admin/notifications")
	if r.status != http.StatusOK || !strings.Contains(r.body, "<strong>nas01</strong>") ||
		!strings.Contains(r.body, `value="backup.failed" checked`) || strings.Contains(r.body, `value="backup.succeeded" checked`) {
		t.Fatalf("page:\n%s", r.body)
	}
	token := e.csrf("/admin/notifications")
	// form builds the settings form: kv replaces the defaults, except
	// "event", which may repeat.
	form := func(kv ...string) url.Values {
		v := url.Values{"csrf_token": {token}, "summary_schedule": {"daily"}, "stale_days": {"2"}}
		for i := 0; i < len(kv); i += 2 {
			if kv[i] == "event" {
				v.Add(kv[i], kv[i+1])
			} else {
				v.Set(kv[i], kv[i+1])
			}
		}
		return v
	}

	// Invalid input is refused and shown again.
	for _, bad := range []url.Values{
		form("server_url", "ftp://x", "topic", "t"),
		form("topic", "has space"),
		form("topic", "t", "auth", "token"),
		form("topic", "t", "auth", "basic", "user", "u"),
		form("topic", "t", "stale_days", "0"),
		form("topic", "t", "summary_schedule", "custom", "summary_cron", "not cron"),
	} {
		if r := e.post("/admin/notifications", bad); r.status != http.StatusBadRequest {
			t.Errorf("%v: status %d", bad, r.status)
		}
	}

	// Save with a token; only the checked events are on.
	r = e.post("/admin/notifications", form("server_url", "https://ntfy.example.com", "topic", "stackbelay-x1",
		"auth", "token", "token", "tk_secret", "event", "backup.failed", "event", "left_down", "event", "bogus"))
	if r.status != http.StatusOK || !strings.Contains(r.body, "Saved.") || strings.Contains(r.body, "tk_secret") ||
		!strings.Contains(r.body, "stored – type to change") {
		t.Fatalf("save: %d\n%s", r.status, r.body)
	}
	ns, err := e.st.NotifySettings(ctx)
	if err != nil || ns.ServerURL != "https://ntfy.example.com" || ns.Topic != "stackbelay-x1" || ns.Auth.Token != "tk_secret" ||
		!ns.Events["backup.failed"] || !ns.Events["left_down"] || ns.Events["rollback"] || ns.Events["bogus"] {
		t.Fatalf("stored: %+v, %v", ns, err)
	}

	// An empty token field keeps the stored token.
	expect(t, e.post("/admin/notifications", form("server_url", "https://ntfy.example.com", "topic", "stackbelay-x1",
		"auth", "token", "event", "backup.failed")), http.StatusOK, "")
	if ns, _ := e.st.NotifySettings(ctx); ns.Auth.Token != "tk_secret" {
		t.Errorf("token not kept: %+v", ns.Auth)
	}

	// Test message: sent to the stored target, naming the short host.
	r = e.post("/admin/notifications/test", form())
	if r.status != http.StatusOK || !strings.Contains(r.body, "Test notification sent.") || len(sender.sent) != 1 ||
		sender.targets[0].Auth.Token != "tk_secret" || sender.sent[0].Title != "🔔 StackBelay test · nas01" {
		t.Fatalf("test: %d %+v\n%s", r.status, sender, r.body)
	}
	sender.err = errors.New("ntfy answered 401 Unauthorized")
	if r := e.post("/admin/notifications/test", form()); r.status != http.StatusBadGateway || !strings.Contains(r.body, "401") {
		t.Errorf("failed test: %d\n%s", r.status, r.body)
	}

	// Summary: a custom schedule and threshold; the summary key is off
	// unless its box is checked.
	expect(t, e.post("/admin/notifications", form("topic", "stackbelay-x1", "auth", "none", "event", "summary",
		"stale_days", "8", "summary_schedule", "custom", "summary_cron", "30 6 * * 1-5")), http.StatusOK, "")
	if ns, _ := e.st.NotifySettings(ctx); ns.SummaryCron != "30 6 * * 1-5" || ns.StaleDays != 8 || !ns.Events["summary"] {
		t.Errorf("summary settings: %+v", ns)
	}
	if r := e.get("/admin/notifications"); !strings.Contains(r.body, `<option value="custom" selected>`) {
		t.Error("custom schedule not shown as custom")
	}
	expect(t, e.post("/admin/notifications", form("topic", "stackbelay-x1", "summary_schedule", "weekly")), http.StatusOK, "")
	if ns, _ := e.st.NotifySettings(ctx); ns.SummaryCron != "0 7 * * 1" || ns.Events["summary"] {
		t.Errorf("weekly preset, summary off: %+v", ns)
	}

	// Switching to no auth removes the credentials.
	expect(t, e.post("/admin/notifications", form("topic", "stackbelay-x1", "auth", "none")), http.StatusOK, "")
	if ns, _ := e.st.NotifySettings(ctx); ns.Auth != (store.NotifyAuth{}) || ns.ServerURL != "https://ntfy.sh" {
		t.Errorf("after none: %+v", ns)
	}

	actions := auditActions(t, e.st)
	if actions["notify.update"] == 0 || actions["notify.test"] == 0 {
		t.Errorf("audit: %v", actions)
	}
	entries, _ := e.st.ListAudit(ctx, 100)
	for _, a := range entries {
		if strings.Contains(a.Detail, "tk_secret") || strings.Contains(a.Detail, "stackbelay-x1") {
			t.Errorf("audit leaks a secret: %+v", a)
		}
	}
}

func TestNotificationsAdminOnly(t *testing.T) {
	e := newEnv(t, true, true)
	e.loginAdmin()
	alice, _ := e.createUser("alice")
	if r := e.do(alice, http.MethodGet, "/admin/notifications", nil, nil); r.status != http.StatusForbidden {
		t.Errorf("non-admin: %d", r.status)
	}
}

func TestStackNotifyMute(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	e.loginAdmin()
	token := e.csrf("/stacks/web/settings")

	expect(t, e.post("/stacks/web/notify-muted", url.Values{"csrf_token": {token}, "muted": {"true"}}),
		http.StatusSeeOther, "/stacks/web/settings")
	if m, _ := e.st.MutedStacks(context.Background()); !m[webID] {
		t.Fatal("not muted")
	}
	if r := e.get("/stacks/web/settings"); !strings.Contains(r.body, "Unmute") {
		t.Errorf("settings tab:\n%s", r.body)
	}
	if r := e.get("/admin/notifications"); !strings.Contains(r.body, `<a href="/stacks/web/settings">web</a>`) {
		t.Errorf("muted list:\n%s", r.body)
	}
	expect(t, e.post("/stacks/web/notify-muted", url.Values{"csrf_token": {token}, "muted": {"false"}}),
		http.StatusSeeOther, "/stacks/web/settings")
	if m, _ := e.st.MutedStacks(context.Background()); m[webID] {
		t.Fatal("still muted")
	}
	if auditActions(t, e.st)["stack.notify_muted"] != 2 {
		t.Error("no audit entry")
	}
}
