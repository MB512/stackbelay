package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MB512/stackvault/internal/docker"
)

// fakeTTY answers every write with "got:<data>".
type fakeTTY struct {
	out     chan []byte
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	resizes []string
}

func newFakeTTY() *fakeTTY { return &fakeTTY{out: make(chan []byte, 16), done: make(chan struct{})} }

func (f *fakeTTY) Read(p []byte) (int, error) {
	select {
	case b := <-f.out:
		return copy(p, b), nil
	case <-f.done:
		return 0, io.EOF
	}
}

func (f *fakeTTY) Write(p []byte) (int, error) {
	if string(p) == "exit\n" {
		_ = f.Close() // the shell ends itself
		return len(p), nil
	}
	f.out <- append([]byte("got:"), p...)
	return len(p), nil
}

func (f *fakeTTY) Resize(cols, rows uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, strings.Join([]string{itoa(int64(cols)), itoa(int64(rows))}, "x"))
	return nil
}

func (f *fakeTTY) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

// fakeShells has a running app and a stopped db container.
type fakeShells struct {
	mu      sync.Mutex
	started []string // "id cols x rows"
	tty     *fakeTTY
}

func (f *fakeShells) StackContainers(context.Context, string) ([]docker.Container, error) {
	return []docker.Container{
		{ID: "0000000000a1", Name: "app-1", Service: "app", State: "running"},
		{ID: "0000000000d1", Name: "db-1", Service: "db", State: "exited"},
	}, nil
}

func (f *fakeShells) StartShell(_ context.Context, id string, cols, rows uint16) (docker.TTY, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id+" "+itoa(int64(cols))+"x"+itoa(int64(rows)))
	f.tty = newFakeTTY()
	return f.tty, nil
}

func shellEnv(t *testing.T, enabled bool) (*testEnv, *fakeShells) {
	t.Helper()
	shells := &fakeShells{}
	e := newEnvWith(t, true, true, func(s *Server) {
		s.EnableShell = enabled
		s.Shells = shells
	})
	e.setupStacks()
	e.setupLogs() // stack directories
	e.loginAdmin()
	return e, shells
}

// dial opens the shell WebSocket with the client's cookies.
func dial(t *testing.T, e *testEnv, c *http.Client, query url.Values, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	u := strings.Replace(e.srv.URL, "https://", "wss://", 1) + "/stacks/web/shell/ws?" + query.Encode()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c, HTTPHeader: h})
}

func TestShellDisabledByDefault(t *testing.T) {
	e, _ := shellEnv(t, false)
	if r := e.get("/stacks/web"); strings.Contains(r.body, "/shell") {
		t.Fatal("Shell tab shown although disabled")
	}
	expect(t, e.get("/stacks/web/shell"), http.StatusNotFound, "")
	expect(t, e.get("/stacks/web/shell/ws"), http.StatusNotFound, "")
}

func TestShellAccess(t *testing.T) {
	e, _ := shellEnv(t, true)
	alice, _ := e.createUser("alice")
	if r := e.get("/stacks/web"); !strings.Contains(r.body, `href="/stacks/web/shell"`) {
		t.Fatal("Shell tab missing for admins")
	}
	if r := e.do(alice, http.MethodGet, "/stacks/web", nil, nil); strings.Contains(r.body, "/shell") {
		t.Fatal("Shell tab shown to a non-admin")
	}
	expect(t, e.do(alice, http.MethodGet, "/stacks/web/shell", nil, nil), http.StatusForbidden, "")

	// The container list: open links only for running containers.
	r := e.get("/stacks/web/shell")
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(r.body, `href="?container=0000000000a1"`) || strings.Contains(r.body, `?container=0000000000d1`) ||
		strings.Contains(r.body, "xterm") {
		t.Fatalf("container list:\n%s", r.body)
	}
	// A stopped or foreign container is not opened.
	for _, id := range []string{"0000000000d1", "0000000000ff"} {
		if r := e.get("/stacks/web/shell?container=" + id); strings.Contains(r.body, `id="terminal"`) {
			t.Fatalf("terminal for %s", id)
		}
	}
	// The terminal page: own CSP with inline styles for xterm.js only here.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+"/stacks/web/shell?container=0000000000a1", nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") ||
		!strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}
	if !strings.Contains(string(body), `data-socket="/stacks/web/shell/ws?container=0000000000a1&amp;csrf=`) ||
		!strings.Contains(string(body), "/static/xterm-6.0.0.js") {
		t.Fatalf("terminal page:\n%s", body)
	}
	if csp := e.get("/stacks/web").header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") {
		t.Fatal("inline styles allowed outside the shell page")
	}
}

func TestShellSession(t *testing.T) {
	e, shells := shellEnv(t, true)
	token := e.csrf("/stacks/web")
	ok := url.Values{"container": {"0000000000a1"}, "csrf": {token}, "cols": {"120"}, "rows": {"40"}}

	// Refused: no or wrong CSRF token, stopped container, foreign origin.
	for name, q := range map[string]url.Values{
		"no token":          {"container": {"0000000000a1"}},
		"stopped container": {"container": {"0000000000d1"}, "csrf": {token}},
		"bad size":          {"container": {"0000000000a1"}, "csrf": {token}, "cols": {"0"}},
	} {
		if _, resp, err := dial(t, e, e.client, q, ""); err == nil {
			t.Errorf("%s: connected", name)
		} else if resp != nil {
			_ = resp.Body.Close()
		}
	}
	if _, resp, err := dial(t, e, e.client, ok, "https://evil.example"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	conn, _, err := dial(t, e, e.client, ok, e.srv.URL) //nolint:bodyclose // websocket.Dial: the body is the connection
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "got:ls\n" {
		t.Fatalf("read %v %q %v", typ, data, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":99999,"rows":30}`)) // ignored
	// "exit" ends the shell: the server closes the connection normally.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close: %v", err)
	}

	shells.mu.Lock()
	started := append([]string(nil), shells.started...)
	tty := shells.tty
	shells.mu.Unlock()
	if len(started) != 1 || started[0] != "0000000000a1 120x40" {
		t.Fatalf("started %v", started)
	}
	tty.mu.Lock()
	resizes := append([]string(nil), tty.resizes...)
	tty.mu.Unlock()
	if len(resizes) != 1 || resizes[0] != "100x30" {
		t.Fatalf("resizes %v", resizes)
	}

	// Start and end are audited; the end entry is written after the close.
	deadline := time.Now().Add(5 * time.Second)
	for {
		actions := auditActions(t, e.st)
		if actions["shell.start"] == 1 && actions["shell.end"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit %v", actions)
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, _ := e.st.ListAudit(context.Background(), 100)
	for _, en := range entries {
		if strings.Contains(en.Detail, "ls") && strings.HasPrefix(en.Action, "shell.") {
			t.Fatal("keystrokes in the audit log")
		}
	}
}

// Logging out ends an open shell at the next session check.
func TestShellEndsWithSession(t *testing.T) {
	e, _ := shellEnv(t, true)
	e.server.shellCheck = 100 * time.Millisecond
	token := e.csrf("/stacks/web")
	conn, _, err := dial(t, e, e.client, url.Values{"container": {"0000000000a1"}, "csrf": {token}}, "") //nolint:bodyclose // see above
	if err != nil {
		t.Fatal(err)
	}
	expect(t, e.post("/logout", url.Values{"csrf_token": {token}}), http.StatusSeeOther, "/login")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if errors.Is(err, context.DeadlineExceeded) || websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("shell not closed after logout: %v", err)
	}
}
