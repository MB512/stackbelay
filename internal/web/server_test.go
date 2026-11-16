package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/secrets"
	"github.com/MB512/stackvault/internal/store"
)

type testEnv struct {
	t       *testing.T
	st      *store.Store
	srv     *httptest.Server
	client  *http.Client
	adminPW string
	snaps   *fakeRefresher
	queue   *fakeQueue
	server  *Server

	mu  sync.Mutex
	now time.Time
}

func (e *testEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *testEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

var bannerPW = regexp.MustCompile(`INITIAL ADMIN PASSWORD \(user "admin"\): (\S+)`)

// newEnv starts a test server. tls=true uses HTTPS with secure cookies
// (production setup); tls=false uses plain HTTP, with secure cookies only
// if secureCookies is set.
func newEnv(t *testing.T, tls, secureCookies bool) *testEnv {
	t.Helper()
	ctx := context.Background()
	box, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	var banner bytes.Buffer
	if err := auth.EnsureInitialAdmin(ctx, st, &banner, discard); err != nil {
		t.Fatal(err)
	}
	m := bannerPW.FindStringSubmatch(banner.String())
	if m == nil {
		t.Fatal("no admin password")
	}

	e := &testEnv{t: t, st: st, adminPW: m[1], now: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		snaps: &fakeRefresher{st: st}, queue: &fakeQueue{st: st}}
	s := &Server{
		Store: st, Logger: discard, SessionTTL: time.Hour, SecureCookies: secureCookies,
		Limiter: &auth.LoginLimiter{Now: e.clock}, Now: e.clock, Snapshots: e.snaps,
		Jobs: e.queue, JobsDir: t.TempDir(),
	}
	e.server = s
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if tls {
		e.srv = httptest.NewTLSServer(h)
	} else {
		e.srv = httptest.NewServer(h)
	}
	t.Cleanup(e.srv.Close)
	e.client = e.newClient()
	return e
}

// newClient returns a client with its own cookie jar (a separate browser)
// that does not follow redirects, so tests can assert them.
func (e *testEnv) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	c := e.srv.Client()
	return &http.Client{
		Transport: c.Transport,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type response struct {
	status   int
	location string
	body     string
	header   http.Header
}

func (e *testEnv) do(c *http.Client, method, path string, form url.Values, header map[string]string) response {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(b), header: resp.Header}
}

func (e *testEnv) get(path string) response { return e.do(e.client, http.MethodGet, path, nil, nil) }
func (e *testEnv) post(path string, form url.Values) response {
	return e.do(e.client, http.MethodPost, path, form, nil)
}

func (e *testEnv) login(c *http.Client, user, pw string) response {
	return e.do(c, http.MethodPost, "/login", url.Values{"username": {user}, "password": {pw}}, nil)
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (e *testEnv) csrf(path string) string {
	e.t.Helper()
	r := e.get(path)
	m := csrfRe.FindStringSubmatch(r.body)
	if m == nil {
		e.t.Fatalf("no CSRF token on %s (status %d)", path, r.status)
	}
	return m[1]
}

func expect(t *testing.T, r response, status int, location string) {
	t.Helper()
	if r.status != status || r.location != location {
		t.Fatalf("got %d %q, want %d %q\n%s", r.status, r.location, status, location, r.body)
	}
}

func TestLoginPasswordChangeLogout(t *testing.T) {
	e := newEnv(t, true, true)

	expect(t, e.get("/"), http.StatusSeeOther, "/login")
	expect(t, e.get("/login"), http.StatusOK, "")

	r := e.login(e.client, "admin", "wrong password")
	expect(t, r, http.StatusUnauthorized, "")
	if !strings.Contains(r.body, "Invalid username or password.") {
		t.Fatal("no error message")
	}
	if r := e.login(e.client, "nobody", "x"); r.status != http.StatusUnauthorized || !strings.Contains(r.body, "Invalid username or password.") {
		t.Fatal("unknown user must look like a wrong password")
	}

	r = e.login(e.client, "admin", e.adminPW)
	expect(t, r, http.StatusSeeOther, "/")
	cookie := r.header.Get("Set-Cookie")
	for _, attr := range []string{"__Host-stackvault_session=", "Path=/", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("session cookie lacks %q: %s", attr, cookie)
		}
	}

	// Generated password: everything leads to the change form.
	expect(t, e.get("/"), http.StatusSeeOther, "/account/password")
	token := e.csrf("/account/password")

	form := func(cur, newPw, confirm string, withToken bool) url.Values {
		v := url.Values{"current": {cur}, "new": {newPw}, "confirm": {confirm}}
		if withToken {
			v.Set("csrf_token", token)
		}
		return v
	}
	const newPw = "my new long password"
	expect(t, e.post("/account/password", form(e.adminPW, newPw, newPw, false)), http.StatusForbidden, "")
	expect(t, e.post("/account/password", url.Values{"csrf_token": {"forged"}, "current": {e.adminPW}}), http.StatusForbidden, "")
	expect(t, e.post("/account/password", form("wrong", newPw, newPw, true)), http.StatusUnauthorized, "")
	expect(t, e.post("/account/password", form(e.adminPW, newPw, "different", true)), http.StatusBadRequest, "")
	expect(t, e.post("/account/password", form(e.adminPW, "short", "short", true)), http.StatusBadRequest, "")
	expect(t, e.post("/account/password", form(e.adminPW, e.adminPW, e.adminPW, true)), http.StatusBadRequest, "")
	expect(t, e.post("/account/password", form(e.adminPW, newPw, newPw, true)), http.StatusSeeOther, "/")

	r = e.get("/")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, "<h1>Stacks</h1>") || !strings.Contains(r.body, "admin (admin)") {
		t.Fatalf("home page:\n%s", r.body)
	}

	// Logout needs the CSRF token, then the session is gone.
	expect(t, e.post("/logout", url.Values{}), http.StatusForbidden, "")
	expect(t, e.post("/logout", url.Values{"csrf_token": {e.csrf("/")}}), http.StatusSeeOther, "/login")
	expect(t, e.get("/"), http.StatusSeeOther, "/login")

	// Old generated password no longer works, the new one does.
	expect(t, e.login(e.client, "admin", e.adminPW), http.StatusUnauthorized, "")
	expect(t, e.login(e.client, "admin", newPw), http.StatusSeeOther, "/")
	expect(t, e.get("/"), http.StatusOK, "")

	entries, _ := e.st.ListAudit(context.Background(), 100)
	actions := map[string]int{}
	for _, a := range entries {
		actions[a.Action]++
	}
	for action, n := range map[string]int{"login.failure": 3, "login.success": 2, "password.change": 1, "logout": 1} {
		if actions[action] < n {
			t.Errorf("audit: %s logged %d times, want >= %d (%v)", action, actions[action], n, actions)
		}
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	e := newEnv(t, true, true)
	other := e.newClient()
	expect(t, e.login(e.client, "admin", e.adminPW), http.StatusSeeOther, "/")
	expect(t, e.login(other, "admin", e.adminPW), http.StatusSeeOther, "/")

	token := e.csrf("/account/password")
	const pw = "another long password"
	expect(t, e.post("/account/password", url.Values{"csrf_token": {token}, "current": {e.adminPW}, "new": {pw}, "confirm": {pw}}),
		http.StatusSeeOther, "/")
	expect(t, e.get("/"), http.StatusOK, "")
	expect(t, e.do(other, http.MethodGet, "/", nil, nil), http.StatusSeeOther, "/login")
}

func TestLoginRequiresHTTPS(t *testing.T) {
	e := newEnv(t, false, true) // plain HTTP, but secure cookies required
	r := e.login(e.client, "admin", e.adminPW)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "requires HTTPS") {
		t.Fatalf("login over plain HTTP: %d\n%s", r.status, r.body)
	}
	// Behind a TLS-terminating proxy.
	r = e.do(e.client, http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {e.adminPW}},
		map[string]string{"X-Forwarded-Proto": "https"})
	expect(t, r, http.StatusSeeOther, "/")
}

func TestInsecureModeWorksOverHTTP(t *testing.T) {
	e := newEnv(t, false, false)
	r := e.login(e.client, "admin", e.adminPW)
	expect(t, r, http.StatusSeeOther, "/")
	if c := r.header.Get("Set-Cookie"); !strings.HasPrefix(c, "stackvault_session=") || strings.Contains(c, "Secure") {
		t.Fatalf("insecure cookie: %s", c)
	}
	expect(t, e.get("/"), http.StatusSeeOther, "/account/password")
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, true, true)
	for i := 0; i < 6; i++ {
		expect(t, e.login(e.client, "admin", "wrong"), http.StatusUnauthorized, "")
	}
	// Locked now, even with the right password; the name is case-insensitive.
	if r := e.login(e.client, "ADMIN", e.adminPW); r.status != http.StatusTooManyRequests || !strings.Contains(r.body, "Try again in") {
		t.Fatalf("expected lockout, got %d", r.status)
	}
	e.advance(2 * time.Second)
	expect(t, e.login(e.client, "admin", e.adminPW), http.StatusSeeOther, "/")
}

func TestCrossOriginPostRejected(t *testing.T) {
	e := newEnv(t, true, true)
	r := e.do(e.client, http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {e.adminPW}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.status != http.StatusForbidden {
		t.Fatalf("cross-site login post: %d", r.status)
	}
}

func TestSessionExpiryAndSliding(t *testing.T) {
	e := newEnv(t, true, true)
	expect(t, e.login(e.client, "admin", e.adminPW), http.StatusSeeOther, "/")

	// Active use extends the session (TTL 1h): 3 x 40 min.
	for i := 0; i < 3; i++ {
		e.advance(40 * time.Minute)
		expect(t, e.get("/account/password"), http.StatusOK, "")
	}
	// Idle longer than the TTL: logged out.
	e.advance(61 * time.Minute)
	expect(t, e.get("/account/password"), http.StatusSeeOther, "/login")
}

func TestHealthzStaticAndHeaders(t *testing.T) {
	e := newEnv(t, true, true)
	r := e.get("/healthz")
	expect(t, r, http.StatusOK, "")
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if r.header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	r = e.get("/static/style.css")
	if r.status != http.StatusOK || !strings.Contains(r.header.Get("Content-Type"), "text/css") {
		t.Fatalf("style.css: %d %s", r.status, r.header.Get("Content-Type"))
	}
	r = e.get("/static/htmx-2.0.11.min.js")
	if r.status != http.StatusOK || !strings.Contains(r.header.Get("Content-Type"), "javascript") {
		t.Fatalf("htmx: %d %s", r.status, r.header.Get("Content-Type"))
	}
	// Icons are served without login (the login page shows them too).
	for path, ct := range map[string]string{
		"/static/favicon.svg":          "image/svg+xml",
		"/static/favicon.ico":          "image/",
		"/static/apple-touch-icon.png": "image/png",
	} {
		if r := e.get(path); r.status != http.StatusOK || !strings.Contains(r.header.Get("Content-Type"), ct) {
			t.Errorf("%s: %d %s", path, r.status, r.header.Get("Content-Type"))
		}
	}
	if !strings.Contains(e.get("/login").body, `rel="icon"`) {
		t.Error("pages must link the favicon")
	}
	if r := e.get("/login"); r.header.Get("Cache-Control") != "no-store" {
		t.Error("pages must not be cached")
	}
	expect(t, e.get("/nope"), http.StatusNotFound, "")

	_ = e.st.Close()
	expect(t, e.get("/healthz"), http.StatusServiceUnavailable, "")
}

// Dark mode follows the browser setting: the page announces both schemes
// and the stylesheet has a dark color set.
func TestDarkModeFollowsBrowser(t *testing.T) {
	e := newEnv(t, true, true)
	if r := e.get("/login"); !strings.Contains(r.body, `<meta name="color-scheme" content="light dark">`) {
		t.Errorf("login page lacks the color-scheme meta tag:\n%s", r.body)
	}
	css := e.get("/static/style.css").body
	for _, want := range []string{"color-scheme: light dark", "@media (prefers-color-scheme: dark)"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
}
