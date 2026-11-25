package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/notify"
)

func TestAdminMute(t *testing.T) {
	mutes := &notify.Mutes{}
	e := newEnvWith(t, true, true, func(s *Server) { s.Mutes = mutes })
	ctx := context.Background()
	webID, _ := e.setupStacks()
	e.loginAdmin()
	admin, _ := e.st.UserByUsername(ctx, "admin")

	r := e.get("/stacks/web")
	if !strings.Contains(r.body, "🔕 Mute</button>") {
		t.Fatalf("no mute button:\n%s", r.body)
	}
	token := e.csrf("/stacks/web")

	// Muting returns to the page it was used on; a foreign target is refused.
	expect(t, e.post("/account/notify-mute", url.Values{"csrf_token": {token}, "on": {"true"}, "back": {"/stacks/web"}}),
		http.StatusSeeOther, "/stacks/web")
	r = e.get("/stacks/web")
	if !strings.Contains(r.body, `data-countdown="300"`) || !strings.Contains(r.body, "until 10:05") ||
		!strings.Contains(r.body, "Unmute") {
		t.Fatalf("badge:\n%s", r.body)
	}

	// A job started now is muted, and its stack is muted for the monitor.
	if r := e.post("/stacks/web/stop", url.Values{"csrf_token": {token}}); r.status != http.StatusSeeOther {
		t.Fatalf("stop: %d", r.status)
	}
	list, err := e.st.ListJobs(ctx, webID, 1)
	if err != nil || len(list) != 1 || !list[0].Options.NotifyMuted {
		t.Fatalf("job not muted: %+v, %v", list, err)
	}
	if !mutes.StackMuted(webID, e.clock()) {
		t.Fatal("stack not muted for the monitor")
	}

	// Polls do not keep the mute alive, page requests do.
	e.advance(4 * time.Minute)
	e.do(e.client, http.MethodGet, "/stacks/web/status/live", nil, map[string]string{pollHeader: "1", "HX-Request": "true"})
	e.advance(2 * time.Minute)
	if !mutes.Until(admin.ID, e.clock()).IsZero() {
		t.Fatal("a poll must not extend the mute")
	}
	expect(t, e.post("/account/notify-mute", url.Values{"csrf_token": {token}, "on": {"true"}, "back": {"https://evil.example/"}}),
		http.StatusSeeOther, "/")
	e.advance(4 * time.Minute)
	e.get("/")
	e.advance(4 * time.Minute)
	if mutes.Until(admin.ID, e.clock()).IsZero() {
		t.Fatal("a page request must extend the mute")
	}

	// Unmute; jobs are no longer marked.
	expect(t, e.post("/account/notify-mute", url.Values{"csrf_token": {token}, "on": {"false"}, "back": {"/"}}),
		http.StatusSeeOther, "/")
	if r := e.post("/stacks/web/start", url.Values{"csrf_token": {token}}); r.status != http.StatusSeeOther {
		t.Fatalf("start: %d", r.status)
	}
	if list, _ := e.st.ListJobs(ctx, webID, 1); list[0].Options.NotifyMuted {
		t.Fatal("job after unmute must not be muted")
	}
	if a := auditActions(t, e.st); a["notify.mute"] != 2 || a["notify.unmute"] != 1 {
		t.Errorf("audit: %v", a)
	}
}

func TestAdminMuteNotForUsers(t *testing.T) {
	e := newEnvWith(t, true, true, func(s *Server) { s.Mutes = &notify.Mutes{} })
	e.setupStacks()
	e.loginAdmin()
	alice, _ := e.createUser("alice")
	if r := e.do(alice, http.MethodGet, "/", nil, nil); strings.Contains(r.body, "notify-mute") {
		t.Error("a non-admin sees the mute")
	}
	token := csrfOf(e, alice, "/")
	if r := e.do(alice, http.MethodPost, "/account/notify-mute", url.Values{"csrf_token": {token}, "on": {"true"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("non-admin mute: %d", r.status)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/stacks/web": "/stacks/web", "/": "/", "": "/", "//evil.example": "/", "/\\evil.example": "/",
		"https://evil.example": "/", "stacks": "/", "/a\r\nb": "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}
