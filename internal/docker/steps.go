package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ComposeStep is one step that "docker compose --progress json" reports
// while it creates and starts a project (issue #28), e.g. object
// "Container web-app-1", status "Done", text "Started"; or object
// "Network web_default", text "Created".
type ComposeStep struct {
	Object string // e.g. "Container web-app-1"
	Status string // "Working", "Done", "Warning" or "Error"
	Text   string // e.g. "Creating", "Started", "Waiting", "Healthy"
}

// Done reports whether the step is finished (not in progress).
func (s ComposeStep) Done() bool { return s.Status != "Working" }

func (s ComposeStep) String() string { return s.Object + ": " + s.Text }

// composeSteps runs a compose command that creates or starts containers.
// With c.Steps set it adds "--progress json" and reports each step while
// the command runs; otherwise it runs silently like compose.
func (c *CLI) composeSteps(ctx context.Context, dir string, args ...string) error {
	if c.Steps == nil {
		_, err := c.compose(ctx, dir, args...)
		return err
	}
	full := append([]string{"compose", "--project-directory", dir, "--progress", "json"}, args...)
	name := "docker compose " + strings.Join(args[:min(len(args), 3)], " ")
	cmd := exec.CommandContext(ctx, c.binary(), full...)
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var other []string // non-step output and errors, for the error message
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l pullLine // same line format as pull: id, status, text
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.ID == "" {
			other = append(other, sc.Text())
			continue
		}
		step := ComposeStep{Object: l.ID, Status: l.Status, Text: l.Text}
		if step.Status == "Error" {
			other = append(other, step.String())
		}
		c.Steps(step)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, tail(strings.Join(other, "\n")))
	}
	return nil
}
