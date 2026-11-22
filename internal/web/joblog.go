package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

// logLine is one parsed line of a job log (JSON lines written by slog).
type logLine struct {
	Time  string
	Level string
	Msg   string
	Attrs string // remaining fields as "key=value ..."
	End   int64  // byte offset in log.txt after this line
}

const (
	// maxLogBytes is how much of the end of a job log the page shows.
	maxLogBytes = 512 << 10
	// maxChunkBytes is how much log one stream round reads at most.
	maxChunkBytes = 256 << 10
	// defaultStreamInterval is how often a live stream looks for news.
	defaultStreamInterval = 500 * time.Millisecond
	// streamHeartbeat keeps idle connections open through proxies.
	streamHeartbeat = 15 * time.Second
)

func (s *Server) logPath(jobID string) string {
	return filepath.Join(s.JobsDir, jobID, jobs.LogFile) // jobID is validated (ValidJobID)
}

// readLogTail reads the end of a job log for the page. next is the byte
// offset after the last complete line; the live stream continues there.
// Times are shown in loc.
func readLogTail(path string, loc *time.Location) (lines []logLine, cut bool, next int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, 0, err
	}
	start := int64(0)
	if info.Size() > maxLogBytes {
		start, cut = info.Size()-maxLogBytes, true
	}
	lines, next, err = readLogFrom(path, start, maxLogBytes, cut, loc)
	return lines, cut, next, err
}

// readLogFrom reads complete lines starting at byte offset from, at most
// limit bytes. skipPartial drops the first line (from is in the middle of
// a line). next is the offset after the last complete line read; a line
// the worker is still writing is left for the next call. The worker
// already redacted secrets; lines that are not JSON are shown as they are.
func readLogFrom(path string, from, limit int64, skipPartial bool, loc *time.Location) (lines []logLine, next int64, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is built from a validated job id
	if err != nil {
		return nil, from, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.NewSectionReader(f, from, limit))
	if err != nil {
		return nil, from, err
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		if int64(len(data)) == limit { // one line longer than limit: skip it
			return nil, from + limit, nil
		}
		return nil, from, nil
	}
	data = data[:end+1]
	next = from + int64(len(data))
	pos := from
	for i, line := range strings.SplitAfter(string(data), "\n") {
		pos += int64(len(line))
		if (i == 0 && skipPartial) || strings.TrimSpace(line) == "" {
			continue
		}
		l := parseLogLine(strings.TrimSpace(line), loc)
		l.End = pos
		lines = append(lines, l)
	}
	return lines, next, nil
}

// parseLogLine parses one JSON log line of the worker; its time is shown
// in loc.
func parseLogLine(line string, loc *time.Location) logLine {
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return logLine{Msg: line}
	}
	l := logLine{}
	if v, ok := rec["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			l.Time = t.In(loc).Format("15:04:05")
		}
	}
	l.Level, _ = rec["level"].(string)
	l.Msg, _ = rec["msg"].(string)
	var attrs []string
	for _, k := range slices.Sorted(maps.Keys(rec)) {
		switch k {
		case "time", "level", "msg", "job_id":
			continue
		}
		attrs = append(attrs, fmt.Sprintf("%s=%v", k, rec[k]))
	}
	l.Attrs = strings.Join(attrs, " ")
	return l
}

// sseWriter writes Server-Sent Events. Each event is flushed at once.
type sseWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

// event writes one event. data may contain newlines; every line becomes a
// "data:" field, and the browser joins them again.
func (e sseWriter) event(name, id, data string) error {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + name + "\n")
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	if _, err := io.WriteString(e.w, b.String()); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (e sseWriter) comment(text string) error {
	if _, err := io.WriteString(e.w, ": "+text+"\n\n"); err != nil {
		return err
	}
	return e.rc.Flush()
}

// jobEvents streams a job live (Server-Sent Events, text/event-stream):
//
//	event "line"    one log line as HTML; id = byte offset after the line
//	event "details" the job details as HTML (state, times, snapshots),
//	                sent when they change
//	event "done"    the job is final and its log was sent completely
//
// The stream starts at ?offset= (the page's LogOffset, or the last event
// id after a reconnect), so no line is sent twice. It ends with "done",
// when the client goes away, or when the manager shuts down (the client
// then reconnects; during a self-backup that may take a while).
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	j, perm, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		offset = 0
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	sse := sseWriter{w: w, rc: http.NewResponseController(w)}
	// Ask the browser to wait 2 s before its own reconnect attempts.
	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}

	interval := s.StreamInterval
	if interval <= 0 {
		interval = defaultStreamInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastBeat := time.Now()
	var lastDetails string
	ctx := r.Context()

	for {
		// Load the job before reading the log: if it is final now, the
		// worker has finished writing, so the log read below is complete.
		cur, err := s.Store.JobByID(ctx, j.ID)
		if err != nil {
			if ctx.Err() == nil {
				s.Logger.Warn("job stream: loading job", "job_id", j.ID, "error", err)
			}
			return
		}
		if offset, err = s.streamLog(sse, j.ID, offset); err != nil {
			return
		}
		details, err := s.jobDetailsHTML(ctx, cur, perm)
		if err != nil {
			s.Logger.Warn("job stream: rendering details", "job_id", j.ID, "error", err)
			return
		}
		if details != lastDetails {
			if sse.event("details", "", details) != nil {
				return
			}
			lastDetails = details
		}
		if cur.State.Final() {
			_ = sse.event("done", "", string(cur.State))
			return
		}
		if time.Since(lastBeat) > streamHeartbeat {
			if sse.comment("ping") != nil {
				return
			}
			lastBeat = time.Now()
		}
		select {
		case <-ctx.Done(): // client gone
			return
		case <-s.Stopping: // manager shuts down; nil channel blocks forever
			return
		case <-ticker.C:
		}
	}
}

// streamLog sends the new complete log lines after offset and returns the
// new offset.
func (s *Server) streamLog(sse sseWriter, jobID string, offset int64) (int64, error) {
	if s.JobsDir == "" {
		return offset, nil
	}
	for {
		lines, next, err := readLogFrom(s.logPath(jobID), offset, maxChunkBytes, false, s.loc())
		if errors.Is(err, os.ErrNotExist) {
			return offset, nil // the worker has not started yet
		}
		if err != nil {
			s.Logger.Warn("job stream: reading log", "job_id", jobID, "error", err)
			return offset, nil
		}
		if next == offset {
			return offset, nil
		}
		// One event per line, its id is the offset after it: a reconnect
		// resumes exactly behind the last line the browser received.
		for _, l := range lines {
			html, err := s.partialHTML("job.html", "log-line", l)
			if err != nil {
				return offset, err
			}
			if err := sse.event("line", strconv.FormatInt(l.End, 10), html); err != nil {
				return offset, err
			}
		}
		if len(lines) == 0 || lines[len(lines)-1].End != next { // skipped empty lines or an overlong line
			if err := sse.event("offset", strconv.FormatInt(next, 10), ""); err != nil {
				return offset, err
			}
		}
		offset = next
	}
}

// jobDetailsHTML renders the job details section for the live stream.
func (s *Server) jobDetailsHTML(ctx context.Context, j store.Job, perm store.Permission) (string, error) {
	return s.partialHTML("job.html", "job-details", s.newJobPage(ctx, j, perm))
}

// partialHTML executes a named template of a page into a string.
func (s *Server) partialHTML(page, name string, data any) (string, error) {
	t, ok := s.templates[page]
	if !ok {
		return "", fmt.Errorf("unknown template %s", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
