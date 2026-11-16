package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxLogLineBytes is the length at which a log line is cut; the rest of
// the line is dropped.
const MaxLogLineBytes = 16 << 10

// LogLine is one line of "docker compose logs".
type LogLine struct {
	Container string    // as compose prints it, e.g. "app-1" (see Containers); "" for compose's own messages
	Time      time.Time // from --timestamps; zero if the line has none
	Text      string
	Cut       bool // the line was longer than MaxLogLineBytes
}

// Containers returns the names of all containers of the stack in dir
// (running or not), sorted, as "docker compose logs" prefixes their lines:
// without the "<project>-" part ("app-1" for container "web-app-1"); a
// custom container_name stays as it is. Like Services it first checks that
// the compose project belongs to dir, so the logs of another project with
// the same name are never shown.
func (c *CLI) Containers(ctx context.Context, dir string) ([]string, error) {
	if err := c.checkProjectOwner(ctx, dir); err != nil {
		return nil, err
	}
	out, err := c.compose(ctx, dir, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	entries, err := parsePS(out)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		name := e.Name
		if e.Project != "" {
			name = strings.TrimPrefix(name, e.Project+"-")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// FollowLogs runs "docker compose logs --follow" for the stack in dir and
// calls fn for every line, in the order compose prints them (all containers
// mixed). Without since it starts with the last tail lines of each
// container (tailLines); with since it starts at that time (used to resume a stream).
//
// It returns when ctx is done (the process is killed), when fn returns an
// error (that error is returned), or when compose ends on its own. Current
// Compose versions keep following a stopped stack and pick up its
// containers again when they start; older ones end once no container
// runs. It checks the project owner first (see Containers).
func (c *CLI) FollowLogs(ctx context.Context, dir string, tailLines int, since time.Time, fn func(LogLine) error) error {
	if err := c.checkProjectOwner(ctx, dir); err != nil {
		return err
	}
	args := []string{"compose", "--project-directory", dir, "logs", "--follow", "--timestamps", "--no-color"}
	if since.IsZero() {
		args = append(args, "--tail", strconv.Itoa(tailLines))
	} else {
		args = append(args, "--since", since.UTC().Format(time.RFC3339Nano))
	}

	// A context of our own: returning early (fn failed) must also stop the
	// process. cancel is deferred, so it runs on every return path, like a
	// "finally" block.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("docker compose logs: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker compose logs: %w", err)
	}

	readErr := readLines(stdout, MaxLogLineBytes, func(line string, cut bool) error {
		l := ParseLogLine(line)
		l.Cut = cut
		return fn(l)
	})
	if readErr != nil {
		cancel()
		_, _ = io.Copy(io.Discard, stdout) // let Wait finish
	}
	waitErr := cmd.Wait()
	switch {
	case readErr != nil:
		return readErr
	case ctx.Err() != nil:
		return ctx.Err()
	case waitErr != nil:
		return fmt.Errorf("docker compose logs: %w: %s", waitErr, tail(stderr.String()))
	}
	return nil
}

// ParseLogLine splits a line of "docker compose logs --timestamps
// --no-color", e.g. "app-1  | 2026-11-16T16:00:00.123456789Z text".
// Lines without the "name | " prefix (compose's own messages such as
// "web-app-1 exited with code 0") have no container.
func ParseLogLine(line string) LogLine {
	name, rest, ok := strings.Cut(line, " | ")
	if !ok || strings.ContainsAny(strings.TrimSpace(name), " \t") || strings.TrimSpace(name) == "" {
		return LogLine{Text: line}
	}
	l := LogLine{Container: strings.TrimSpace(name), Text: rest}
	ts, text, _ := strings.Cut(rest, " ")
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		l.Time, l.Text = t, text
	}
	return l
}

// readLines calls fn for every line of r (without the line break). Lines
// longer than maxLen are cut to maxLen and the rest is skipped (cut is
// true), so one huge line cannot exhaust memory. A last line without a
// line break is passed on as well.
func readLines(r io.Reader, maxLen int, fn func(line string, cut bool) error) error {
	// bufio enforces a minimum size (16 bytes); the cut below is exact.
	br := bufio.NewReaderSize(r, maxLen)
	for {
		chunk, err := br.ReadSlice('\n')
		cut := false
		var line []byte
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// Keep the first maxLen bytes, drop everything up to the next '\n'.
			line, cut = append([]byte(nil), chunk...), true
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
		default:
			line = chunk
		}
		if len(line) > 0 {
			s := strings.TrimRight(string(line), "\r\n")
			if len(s) > maxLen {
				s, cut = s[:maxLen], true
			}
			if cut {
				// The cut may split a multi-byte character.
				s = strings.ToValidUTF8(s, "")
			}
			if ferr := fn(s, cut); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
