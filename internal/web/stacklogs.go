package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/stacks"
	"github.com/MB512/stackvault/internal/store"
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
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = map[int64]int{}
	}
	if l.total >= total || l.byUser[userID] >= perUser {
		return false
	}
	l.total++
	l.byUser[userID]++
	return true
}

func (l *streamLimiter) release(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.byUser[userID]--; l.byUser[userID] <= 0 {
		delete(l.byUser, userID)
	}
}

// logsTab is the data of the "Logs" tab.
type logsTab struct {
	stackHeader
	Containers  []string
	Unavailable string // why logs cannot be shown, or ""
	// Selected is the only container shown at first (?container=, the
	// "Logs" link of a container on the Status tab); "" shows all.
	Selected string
}

// containerLogLine is one line of the Logs tab; also sent as a "line"
// event of the live stream.
type containerLogLine struct {
	Container string
	Time      string // UTC, "2006-01-02 15:04:05"; "" if the line has none
	Text      string
	Cut       bool
}

func newContainerLogLine(l docker.LogLine) containerLogLine {
	out := containerLogLine{Container: l.Container, Text: l.Text, Cut: l.Cut}
	if !l.Time.IsZero() {
		out.Time = l.Time.UTC().Format("2006-01-02 15:04:05")
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
	page := logsTab{stackHeader: s.newStackHeader(a.User, perm, st, tabLogs)}
	dir, found := s.stackDir(st)
	switch {
	case s.Logs == nil:
		page.Unavailable = "Container logs are not available in this installation."
	case !found:
		page.Unavailable = "The stack directory is missing."
	default:
		ctx, cancel := context.WithTimeout(r.Context(), containerListTimeout)
		names, err := s.Logs.Containers(ctx, dir)
		cancel()
		if err != nil {
			s.Logger.Warn("listing containers", "stack", st.Name, "error", err)
			page.Unavailable = "Listing the containers failed: " + truncate(err.Error(), 300)
		}
		page.Containers = names
		if c := r.URL.Query().Get("container"); slices.Contains(names, c) {
			page.Selected = c
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
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "invalid since", http.StatusBadRequest)
			return
		}
		since = t.Add(time.Nanosecond) // the line at "since" was already shown
	}
	if !s.logStreams.acquire(a.User.ID, maxLogStreamsPerUser, maxLogStreams) {
		http.Error(w, "too many open log views", http.StatusTooManyRequests)
		return
	}
	defer s.logStreams.release(a.User.ID)
	if since.IsZero() {
		s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "stack.logs", StackID: st.ID})
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sse := sseWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := io.WriteString(w, "retry: 5000\n\n"); err != nil {
		return
	}

	// The compose process ends with ctx: when the client goes away or the
	// manager shuts down.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// FollowLogs blocks, so it runs in its own goroutine (a lightweight
	// thread, like Task.Run in C#) and hands the lines over through a
	// channel (like a BlockingCollection). This goroutine alone writes to
	// the response, which also sends the heartbeats.
	lines := make(chan docker.LogLine, 256)
	result := make(chan error, 1)
	go func() {
		result <- s.Logs.FollowLogs(ctx, dir, logTailLines, since, func(l docker.LogLine) error {
			select {
			case lines <- l:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case l := <-lines:
			if s.sendLogLine(sse, l) != nil {
				return
			}
		case err := <-result:
			// All lines were handed over before FollowLogs returned; send
			// what is still buffered.
			for len(lines) > 0 {
				if s.sendLogLine(sse, <-lines) != nil {
					return
				}
			}
			switch {
			case err == nil:
				_ = sse.event("end", "", "")
			case errors.Is(err, context.Canceled) || ctx.Err() != nil:
			default:
				s.Logger.Warn("log stream", "stack", st.Name, "error", err)
				_ = sse.event("failed", "", truncate(err.Error(), 300))
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
func (s *Server) sendLogLine(sse sseWriter, l docker.LogLine) error {
	html, err := s.partialHTML("stack_logs.html", "container-log-line", newContainerLogLine(l))
	if err != nil {
		return err
	}
	id := ""
	if !l.Time.IsZero() {
		id = l.Time.UTC().Format(time.RFC3339Nano)
	}
	return sse.event("line", id, html)
}
