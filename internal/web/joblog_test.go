package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

type sseEvent struct {
	name, id, data string
}

// stream connects to a job's event stream and delivers its events on a
// channel, which is closed when the stream ends.
func stream(t *testing.T, e *testEnv, c *http.Client, jobID string, offset int64) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		e.srv.URL+"/jobs/"+jobID+"/events?offset="+strconv.FormatInt(offset, 10), nil)
	resp, err := c.Do(req) //nolint:bodyclose // closed by the reader goroutine below (or on the error path)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan sseEvent, 100)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					ev.data = strings.Join(data, "\n")
					events <- ev
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return events, cancel
}

func next(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

// nextNamed skips events until one with the given name arrives.
func nextNamed(t *testing.T, events <-chan sseEvent, name string) sseEvent {
	t.Helper()
	for {
		if ev := next(t, events); ev.name == name {
			return ev
		}
	}
}

func appendLog(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, text); err != nil {
		t.Fatal(err)
	}
}

func TestJobEvents(t *testing.T) {
	e, alice, _ := setupJobsEnv(t)
	e.server.StreamInterval = 20 * time.Millisecond
	ctx := context.Background()
	r := e.do(alice, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {csrfOf(e, alice, "/stacks/web")}}, nil)
	id := strings.TrimPrefix(r.location, jobLocation)
	dir := filepath.Join(e.server.JobsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, jobs.LogFile)
	l1 := `{"time":"2026-11-14T10:00:01Z","level":"INFO","msg":"stopping stack"}` + "\n"
	l2 := `{"time":"2026-11-14T10:00:02Z","level":"WARN","msg":"<b>bold</b>"}` + "\n"
	appendLog(t, logPath, l1+l2+`{"time":"2026-11-14T10:00:03Z","level":"INFO","msg":"final sn`)

	events, cancel := stream(t, e, alice, id, 0)
	defer cancel()
	ev := next(t, events)
	if ev.name != "line" || ev.id != strconv.Itoa(len(l1)) || !strings.Contains(ev.data, "stopping stack") {
		t.Fatalf("first event: %+v", ev)
	}
	ev = next(t, events)
	if ev.name != "line" || ev.id != strconv.Itoa(len(l1+l2)) || !strings.Contains(ev.data, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Fatalf("second event (escaped): %+v", ev)
	}
	ev = next(t, events)
	if ev.name != "details" || !strings.Contains(ev.data, `id="job-details"`) || !strings.Contains(ev.data, "queued") {
		t.Fatalf("details: %+v", ev)
	}

	// The incomplete line is sent once the worker finishes it.
	appendLog(t, logPath, "apshot done\"}\n")
	if ev = next(t, events); ev.name != "line" || !strings.Contains(ev.data, "final snapshot done") {
		t.Fatalf("completed line: %+v", ev)
	}

	// State changes arrive as details.
	if _, ok, err := e.st.ClaimNextJob(ctx, 1, time.Now()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ev = nextNamed(t, events, "details"); !strings.Contains(ev.data, "running") {
		t.Fatalf("running details: %+v", ev)
	}
	if err := e.st.FinishJob(ctx, id, jobs.StateSucceeded, "bbbbbbbb22222222", "", store.JobResult{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ev = nextNamed(t, events, "details"); !strings.Contains(ev.data, "succeeded") || !strings.Contains(ev.data, "bbbbbbbb") ||
		strings.Contains(ev.data, "/cancel") {
		t.Fatalf("final details: %+v", ev)
	}
	if ev = next(t, events); ev.name != "done" || ev.data != "succeeded" {
		t.Fatalf("done: %+v", ev)
	}
	if _, ok := <-events; ok {
		t.Fatal("stream did not end after done")
	}

	// Resuming from an offset skips what the browser already has.
	events2, cancel2 := stream(t, e, alice, id, int64(len(l1+l2)))
	defer cancel2()
	if ev = next(t, events2); ev.name != "line" || !strings.Contains(ev.data, "final snapshot done") {
		t.Fatalf("resumed stream: %+v", ev)
	}
	if ev = nextNamed(t, events2, "done"); ev.data != "succeeded" {
		t.Fatalf("resumed done: %+v", ev)
	}
}

func TestJobEventsAccessAndShutdown(t *testing.T) {
	e, alice, _ := setupJobsEnv(t)
	e.server.StreamInterval = 20 * time.Millisecond
	stopping := make(chan struct{})
	e.server.Stopping = stopping
	r := e.do(alice, http.MethodPost, "/stacks/web/backup", url.Values{"csrf_token": {csrfOf(e, alice, "/stacks/web")}}, nil)
	id := strings.TrimPrefix(r.location, jobLocation)

	carol, _ := e.createUser("carol") // no permission on web
	expect(t, e.do(carol, http.MethodGet, "/jobs/"+id+"/events", nil, nil), http.StatusNotFound, "")
	expect(t, e.do(e.newClient(), http.MethodGet, "/jobs/"+id+"/events", nil, nil), http.StatusSeeOther, "/login")

	// No log yet: only details; on shutdown the stream ends without "done".
	events, cancel := stream(t, e, alice, id, 0)
	defer cancel()
	if ev := next(t, events); ev.name != "details" {
		t.Fatalf("first event without log: %+v", ev)
	}
	close(stopping)
	for ev := range events {
		if ev.name == "done" {
			t.Fatal("done sent on shutdown")
		}
	}
}
