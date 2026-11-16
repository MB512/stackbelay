package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Container is one container of a stack, for choosing a shell target.
type Container struct {
	ID      string // short container id from compose ps
	Name    string // as compose prefixes its log lines (see Containers)
	Service string
	State   string // running, exited, ...
}

// Running reports whether a shell can be opened in the container.
func (c Container) Running() bool { return c.State == "running" }

// shellContainerIDPattern matches the container ids compose and docker print.
var shellContainerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// ValidContainerID reports whether id looks like a docker container id.
func ValidContainerID(id string) bool { return shellContainerIDPattern.MatchString(id) }

// StackContainers returns all containers of the stack in dir, sorted by
// name. Like Services it first checks that the compose project belongs to
// dir, so containers of another project with the same name never appear.
func (c *CLI) StackContainers(ctx context.Context, dir string) ([]Container, error) {
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
	var list []Container
	for _, e := range entries {
		if e.Name == "" || !ValidContainerID(e.ID) {
			continue
		}
		name := e.Name
		if e.Project != "" {
			name = strings.TrimPrefix(name, e.Project+"-")
		}
		list = append(list, Container{ID: e.ID, Name: name, Service: e.Service, State: e.State})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// TTY is an interactive session: reading returns the terminal output,
// writing sends keystrokes. Close ends the session.
type TTY interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Close() error
}

// shellPaths are tried in this order; the first executable one is used.
var shellPaths = []string{"/bin/bash", "/bin/sh"}

// StartShell opens an interactive shell (bash if present, otherwise sh)
// as the container's default user: "docker exec -it" runs in a pseudo-
// terminal, so the shell gets a real terminal (prompt, line editing,
// full-screen programs). containerID must be a validated id of a stack
// container (see StackContainers). The session ends with ctx or Close.
func (c *CLI) StartShell(ctx context.Context, containerID string, cols, rows uint16) (TTY, error) {
	if !ValidContainerID(containerID) {
		return nil, fmt.Errorf("invalid container id %q", containerID)
	}
	shell := ""
	for _, p := range shellPaths {
		// A probe without shell syntax: "test -x" as its own command.
		if _, err := c.run(ctx, "", "exec", containerID, "test", "-x", p); err == nil {
			shell = p
			break
		}
	}
	if shell == "" {
		return nil, errors.New("the container has no shell (/bin/bash or /bin/sh)")
	}

	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, c.binary(), "exec", "--interactive", "--tty",
		"--env", "TERM=xterm-256color", containerID, shell)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("starting shell: %w", err)
	}
	return &ptySession{pty: f, cmd: cmd, cancel: cancel}, nil
}

// ptySession is a TTY backed by a docker CLI process in a pseudo-terminal.
type ptySession struct {
	pty    *os.File
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
}

func (s *ptySession) Read(p []byte) (int, error)  { return s.pty.Read(p) }
func (s *ptySession) Write(p []byte) (int, error) { return s.pty.Write(p) }

// Resize sets the terminal size; the docker CLI passes it on to the
// container (the kernel signals SIGWINCH to it).
func (s *ptySession) Resize(cols, rows uint16) error {
	return pty.Setsize(s.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close stops the docker CLI (the container's shell gets a hangup) and
// releases the terminal. It is safe to call more than once.
func (s *ptySession) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.cmd.Wait()
		_ = s.pty.Close()
	})
	return nil
}
