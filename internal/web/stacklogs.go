package web

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// StackLogs reads the container logs of a stack (*docker.CLI). Both
// methods only run read-only compose commands.
type StackLogs interface {
	Containers(ctx context.Context, dir string) ([]string, error)
	FollowLogs(ctx context.Context, dir string, tailLines int, since time.Time, fn func(docker.LogLine) error) error
}

const (
	// logTailLines is how many lines per container a new log view starts with.
	logTailLines = 500
	// Limits for open log streams: each one is a "docker compose logs"
	// process in the manager.
	maxLogStreamsPerUser = 4
	maxLogStreams        = 20
	// containerListTimeout bounds the "compose ps" for the page.
	containerListTimeout = 15 * time.Second
)

// streamLimiter counts open streams (log streams, shell sessions) per user
// and in total. The zero value is ready to use; it is safe for concurrent
// use.
type streamLimiter struct {
	mu     sync.Mutex
	total  int
	byUser map[int64]int
}

// acquire reserves a stream for the user; false if perUser streams of the
// user or total streams are open. Every successful acquire must be
// followed by release.
func (l *streamLimiter) acquire(userID int64, perUser, total int) bool {
	return l.acquireN(userID, 1, perUser, total)
}

func (l *streamLimiter) release(userID int64) { l.releaseN(userID, 1) }

// acquireN reserves n streams at once, or none (false) if that would
// exceed perUser streams of the user or total streams. Every successful
// acquireN must be followed by releaseN with the same n.
func (l *streamLimiter) acquireN(userID int64, n, perUser, total int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = map[int64]int{}
	}
	if l.total+n > total || l.byUser[userID]+n > perUser {
		return false
	}
	l.total += n
	l.byUser[userID] += n
	return true
}

func (l *streamLimiter) releaseN(userID int64, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total -= n
	if l.byUser[userID] -= n; l.byUser[userID] <= 0 {
		delete(l.byUser, userID)
	}
}

// logPanel is the live log view (template "log-panel", filled by
// static/stacklog.js), used by the Logs tab and the global log view.
type logPanel struct {
	Events      string   // URL of the event stream
	Containers  []string // filter keys, see containerLogLine.Key
	Tail        int      // history lines per container
	Unavailable string   // why logs cannot be shown, or ""
	// Selected is the only container shown at first (?container=, the
	// "Logs" link of a container on the Status tab); "" shows all.
	Selected string
}

// logsTab is the data of the "Logs" tab.
type logsTab struct {
	stackHeader
	Panel logPanel
}

// containerLogLine is one line of a log view; also sent as a "line" event
// of the live stream.
type containerLogLine struct {
	Stack     string // only in the global log view
	Container string
	Time      string // in the manager's time zone, "2006-01-02 15:04:05"; "" if the line has none
	ID        string // see logLineID; data-t of the line, for sorting in the browser
	// Text is the line as HTML: escaped, with ANSI colors as spans
	// (ansiHTML, issue #29).
	Text template.HTML
	Cut  bool
}

// Key identifies the container in the browser's filter (data-c): the
// container name, in the global view "<stack>/<container>"; "" for lines
// without container (compose's own messages).
func (l containerLogLine) Key() string {
	if l.Container == "" || l.Stack == "" {
		return l.Container
	}
	return l.Stack + "/" + l.Container
}

// logLineIDFormat is RFC 3339 in UTC with always nine fraction digits, so
// ids of the same width compare correctly as strings (stacklog.js sorts
// lines and picks the newest id with plain string comparison).
// time.RFC3339Nano parses it.
const logLineIDFormat = "2006-01-02T15:04:05.000000000Z07:00"

// logLineID is the time of a log line as event id and data-t; "" without
// time.
func logLineID(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(logLineIDFormat)
}

func newContainerLogLine(stack string, l docker.LogLine, loc *time.Location) containerLogLine {
	out := containerLogLine{Stack: stack, Container: l.Container, ID: logLineID(l.Time), Text: ansiHTML(l.Text), Cut: l.Cut}
	if !l.Time.IsZero() {
		out.Time = l.Time.In(loc).Format("2006-01-02 15:04:05")
	}
	return out
}

// stackDir returns the directory of a stack from discovery (never from the
// database or the URL). ok is false if the stack directory is missing.
func (s *Server) stackDir(st store.Stack) (string, bool) {
	if s.StacksRoot == "" {
		return "", false
	}
	found, err := stacks.Lookup(s.StacksRoot, st.Name)
	if err != nil {
		return "", false
	}
	return found.Dir, true
}

// stackLogsPage shows the Logs tab: one checkbox per container and the
// log panel that static/stacklog.js fills from stackLogEvents.
func (s *Server) stackLogsPage(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionLogs)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	page := logsTab{stackHeader: s.newStackHeader(a.User, perm, st, tabLogs),
		Panel: logPanel{Events: "/stacks/" + st.Name + "/logs/events", Tail: logTailLines}}
	dir, found := s.stackDir(st)
	switch {
	case s.Logs == nil:
		page.Panel.Unavailable = logsUnavailable
	case !found:
		page.Panel.Unavailable = "The stack directory is missing."
	default:
		names, problem := s.listLogContainers(r.Context(), st.Name, dir)
		page.Panel.Unavailable = problem
		page.Panel.Containers = names
		if c := r.URL.Query().Get("container"); slices.Contains(names, c) {
			page.Panel.Selected = c
		}
	}
	s.render(w, r, http.StatusOK, "stack_logs.html", pageData{Title: st.Name + " logs", Data: page})
}

// stackLogEvents streams the container logs of a stack (Server-Sent
// Events, text/event-stream):
//
//	event "line"  one log line as HTML; id = the line's time (RFC 3339)
//	event "end"   compose stopped following on its own (older Compose
//	              versions do when no container runs); the client
//	              reconnects later with ?since=<last id>
//	event "failed" reading the logs failed; data is the message (not
//	              named "error": that would also fire EventSource.onerror)
//
// Without ?since= the stream starts with the last lines of every container
// and the access is audited; with it (a reconnect) it resumes after that
// time.
func (s *Server) stackLogEvents(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionLogs)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	dir, found := s.stackDir(st)
	if s.Logs == nil || !found {
		http.Error(w, "logs unavailable", http.StatusServiceUnavailable)
		return
	}
	since, ok := parseSince(w, r)
	if !ok {
		return
	}
	if !s.logStreams.acquire(a.User.ID, maxLogStreamsPerUser, maxLogStreams) {
		http.Error(w, "too many open log views", http.StatusTooManyRequests)
		return
	}
	defer s.logStreams.release(a.User.ID)
	if since.IsZero() {
		s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "stack.logs", StackID: st.ID})
	}
	s.streamLogs(w, r, []logSource{{Dir: dir}}, logTailLines, since)
}

// logsUnavailable is shown when the manager has no log reader.
const logsUnavailable = "Container logs are not available in this installation."

// listLogContainers lists the containers of a stack for a log view. If
// that fails, problem is a message for the page.
func (s *Server) listLogContainers(ctx context.Context, stack, dir string) (names []string, problem string) {
	ctx, cancel := context.WithTimeout(ctx, containerListTimeout)
	defer cancel()
	names, err := s.Logs.Containers(ctx, dir)
	if err != nil {
		s.Logger.Warn("listing containers", "stack", stack, "error", err)
		return names, "Listing the containers failed: " + truncate(err.Error(), 300)
	}
	return names, ""
}

// parseSince reads ?since= of a log stream: the id of the newest line the
// browser has, so a reconnect resumes after it. Zero without it; on an
// invalid value it answers 400 and returns false.
func parseSince(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		http.Error(w, "invalid since", http.StatusBadRequest)
		return time.Time{}, false
	}
	return t.Add(time.Nanosecond), true // the line at "since" was already shown
}

// logSource is one stack of a log stream.
type logSource struct {
	Stack string // name, shown in the lines; "" on the Logs tab of the stack itself
	Dir   string // from discovery
}

// sourceLine is a log line and the stack it came from.
type sourceLine struct {
	stack string
	line  docker.LogLine
}

// sourceResult is how the logs of one stack ended.
type sourceResult struct {
	stack string
	err   error
}

// streamLogs streams the container logs of one or more stacks as
// Server-Sent Events (see stackLogEvents), merged in the order they
// arrive; the browser sorts them by time. The caller has checked the
// rights and reserved one stream per source. When the logs of one stack
// end or fail, the whole stream ends; the browser reconnects with ?since=.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, sources []logSource, tail int, since time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sse := sseWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := io.WriteString(w, "retry: 5000\n\n"); err != nil {
		return
	}

	// The compose processes end with ctx: when the client goes away, the
	// manager shuts down or this function returns.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// FollowLogs blocks, so each stack's runs in its own goroutine (a
	// lightweight thread, like Task.Run in C#) and hands the lines over
	// through one shared channel (like a BlockingCollection). This
	// goroutine alone writes to the response, which also sends the
	// heartbeats.
	lines := make(chan sourceLine, 256)
	results := make(chan sourceResult, len(sources))
	for _, src := range sources {
		go func() {
			err := s.Logs.FollowLogs(ctx, src.Dir, tail, since, func(l docker.LogLine) error {
				select {
				case lines <- sourceLine{stack: src.Stack, line: l}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			results <- sourceResult{stack: src.Stack, err: err}
		}()
	}

	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case l := <-lines:
			if s.sendLogLine(sse, l.stack, l.line) != nil {
				return
			}
		case res := <-results:
			// All lines of that stack were handed over before FollowLogs
			// returned; send what is still buffered.
			for len(lines) > 0 {
				l := <-lines
				if s.sendLogLine(sse, l.stack, l.line) != nil {
					return
				}
			}
			switch err := res.err; {
			case err == nil:
				_ = sse.event("end", "", "")
			case errors.Is(err, context.Canceled) || ctx.Err() != nil:
			default:
				s.Logger.Warn("log stream", "stack", res.stack, "error", err)
				msg := truncate(err.Error(), 300)
				if res.stack != "" {
					msg = res.stack + ": " + msg
				}
				_ = sse.event("failed", "", msg)
			}
			return
		case <-heartbeat.C:
			if sse.comment("ping") != nil {
				return
			}
		case <-s.Stopping: // nil channel: blocks forever
			return
		}
	}
}

// sendLogLine sends one "line" event. Its id is the line's time, which a
// reconnect passes back as ?since=; lines without a time have no id.
func (s *Server) sendLogLine(sse sseWriter, stack string, l docker.LogLine) error {
	html, err := s.partialHTML("stack_logs.html", "container-log-line", newContainerLogLine(stack, l, s.loc()))
	if err != nil {
		return err
	}
	return sse.event("line", logLineID(l.Time), html)
}
