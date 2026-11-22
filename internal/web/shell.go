package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/store"
)

// ShellStarter lists a stack's containers and opens shells in them
// (*docker.CLI).
type ShellStarter interface {
	StackContainers(ctx context.Context, dir string) ([]docker.Container, error)
	StartShell(ctx context.Context, containerID string, cols, rows uint16) (docker.TTY, error)
}

const (
	// Limits for open shell sessions (each is a "docker exec" process).
	maxShellsPerUser = 2
	maxShells        = 10
	// shellSessionCheck is how often an open shell re-checks the user's
	// session: logging out, expiry or losing the admin flag ends it.
	shellSessionCheck = 30 * time.Second
	// shellReadLimit bounds one message from the browser (keystrokes or a
	// resize, normally a few bytes; pasting sends more).
	shellReadLimit = 64 << 10
)

// shellCSP is the Content-Security-Policy of the shell page. xterm.js adds
// <style> elements at runtime, so inline styles are allowed here (only
// here); scripts stay same-origin only, and WebSockets to the own origin
// are covered by 'self'.
const shellCSP = "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// shellPage is the data of the admin-only shell page. It has no tab: the
// Status tab links to it per running container.
type shellPage struct {
	Stack       store.Stack
	Selected    *docker.Container // the container whose terminal is shown
	SocketURL   string            // WebSocket path of the selected container's session
	Unavailable string
}

// shellContainers lists the stack's containers for the shell, with the
// stack directory from discovery. On failure the reason is returned as
// text for the page.
func (s *Server) shellContainers(ctx context.Context, st store.Stack) ([]docker.Container, string) {
	dir, found := s.stackDir(st)
	switch {
	case s.Shells == nil:
		return nil, "Shells are not available in this installation."
	case !found:
		return nil, "The stack directory is missing."
	}
	ctx, cancel := context.WithTimeout(ctx, containerListTimeout)
	defer cancel()
	list, err := s.Shells.StackContainers(ctx, dir)
	if err != nil {
		s.Logger.Warn("listing containers", "stack", st.Name, "error", err)
		return nil, "Listing the containers failed: " + truncate(err.Error(), 300)
	}
	return list, ""
}

func findContainer(list []docker.Container, id string) (docker.Container, bool) {
	for _, c := range list {
		if c.ID == id {
			return c, true
		}
	}
	return docker.Container{}, false
}

// stackShell shows the terminal of the container ?container=<id>
// (static/shell.js connects it to shellSocket). Without a container it
// redirects to the Status tab, which lists the containers. Admin only, and
// only registered with STACKBELAY_ENABLE_SHELL.
func (s *Server) stackShell(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView) // the route requires an admin
	if !ok {
		return
	}
	id := r.URL.Query().Get("container")
	if id == "" {
		http.Redirect(w, r, "/stacks/"+url.PathEscape(st.Name), http.StatusSeeOther)
		return
	}
	a := authFrom(r.Context())
	page := shellPage{Stack: st}
	var list []docker.Container
	list, page.Unavailable = s.shellContainers(r.Context(), st)
	if page.Unavailable == "" {
		c, found := findContainer(list, id)
		switch {
		case !found:
			page.Unavailable = "That container does not belong to this stack (any more)."
		case !c.Running():
			page.Unavailable = "Container " + c.Name + " is not running."
		default:
			page.Selected = &c
			q := url.Values{"container": {c.ID}, "csrf": {a.Session.CSRFToken}}
			page.SocketURL = "/stacks/" + url.PathEscape(st.Name) + "/shell/ws?" + q.Encode()
		}
	}
	w.Header().Set("Content-Security-Policy", shellCSP)
	title := st.Name + " shell"
	if page.Selected != nil {
		title = page.Selected.Name + " shell"
	}
	s.render(w, r, http.StatusOK, "stack_shell.html", pageData{Title: title, Data: page})
}

// shellResize is the only text message the browser sends.
type shellResize struct {
	Type string `json:"type"` // "resize"
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// terminalSize parses a terminal dimension (1..1000); def if absent.
func terminalSize(v string, def uint16) (uint16, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return 0, false
	}
	return uint16(n), true //nolint:gosec // G115: checked range
}

// shellSocket connects a browser terminal to a shell in a stack container
// over a WebSocket: binary messages carry terminal data in both
// directions, text messages from the browser are resizes. Admin only;
// the CSRF token is required as ?csrf= (a WebSocket upgrade is a GET), the
// Origin must match the host (websocket.Accept), the container must be a
// running container of the stack. Start and end are audited, keystrokes
// are not recorded.
func (s *Server) shellSocket(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView) // the route requires an admin
	if !ok {
		return
	}
	a := authFrom(r.Context())
	q := r.URL.Query()
	if !auth.TokensEqual(q.Get("csrf"), a.Session.CSRFToken) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	cols, okCols := terminalSize(q.Get("cols"), 80)
	rows, okRows := terminalSize(q.Get("rows"), 24)
	if !docker.ValidContainerID(q.Get("container")) || !okCols || !okRows {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	list, reason := s.shellContainers(r.Context(), st)
	if reason != "" {
		http.Error(w, reason, http.StatusServiceUnavailable)
		return
	}
	c, found := findContainer(list, q.Get("container"))
	if !found || !c.Running() {
		http.Error(w, "container not found or not running", http.StatusNotFound)
		return
	}
	if !s.shells.acquire(a.User.ID, maxShellsPerUser, maxShells) {
		http.Error(w, "too many open shells", http.StatusTooManyRequests)
		return
	}
	defer s.shells.release(a.User.ID)

	conn, err := websocket.Accept(w, r, nil) // rejects other origins
	if err != nil {
		return // Accept has answered the request
	}
	defer func() { _ = conn.CloseNow() }() // no-op after the regular Close below
	conn.SetReadLimit(shellReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	tty, err := s.Shells.StartShell(ctx, c.ID, cols, rows)
	if err != nil {
		s.Logger.Warn("starting shell", "stack", st.Name, "container", c.Name, "error", err)
		_ = conn.Close(websocket.StatusInternalError, truncate(err.Error(), 100))
		return
	}
	started := s.now()
	target := fmt.Sprintf("container %s (%s)", c.Name, c.ID)
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "shell.start", StackID: st.ID, Detail: target})

	// Two goroutines copy data, one per direction; the first to stop
	// cancels ctx (the end of the session). They use the request's context
	// for the WebSocket, not ctx: cancelling the context of a pending
	// websocket Read drops the connection, and the browser should get a
	// proper close with the reason. A WaitGroup (like Task.WhenAll in C#)
	// makes sure both are done before the handler returns.
	connCtx := r.Context()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // container -> browser
		defer wg.Done()
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 && conn.Write(connCtx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return // the shell ended (exit) or the session was closed
			}
		}
	}()
	go func() { // browser -> container
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := conn.Read(connCtx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := tty.Write(data); err != nil {
					return
				}
				continue
			}
			var msg shellResize
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" &&
				msg.Cols >= 1 && msg.Cols <= 1000 && msg.Rows >= 1 && msg.Rows <= 1000 {
				_ = tty.Resize(uint16(msg.Cols), uint16(msg.Rows)) //nolint:gosec // G115: checked range
			}
		}
	}()

	endReason := s.watchShell(ctx, a)
	cancel()
	_ = tty.Close() // ends the output goroutine
	// Sends the close frame and waits for the browser's answer, which the
	// input goroutine's Read receives; then both goroutines are done.
	_ = conn.Close(websocket.StatusNormalClosure, endReason)
	wg.Wait()
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "shell.end", StackID: st.ID,
		Detail: fmt.Sprintf("%s after %s: %s", target, s.now().Sub(started).Round(time.Second), endReason)})
}

// watchShell blocks until the session ends and returns why: the shell or
// the browser ended it (ctx done), the manager shuts down, or the user's
// login is no longer valid (logged out, expired, disabled, not an admin).
func (s *Server) watchShell(ctx context.Context, a *authInfo) string {
	every := s.shellCheck
	if every <= 0 {
		every = shellSessionCheck
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "shell closed"
		case <-s.Stopping: // nil channel: blocks forever
			return "the manager is shutting down"
		case <-ticker.C:
			_, user, err := s.Store.SessionWithUser(ctx, a.Session.IDHash, s.now())
			switch {
			case errors.Is(err, store.ErrNotFound) || (err == nil && (user.Disabled || !user.IsAdmin)):
				return "login no longer valid"
			case err != nil && ctx.Err() == nil:
				s.Logger.Warn("shell: checking session", "error", err)
			}
		}
	}
}
