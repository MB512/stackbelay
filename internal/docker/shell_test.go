package docker

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartShell(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	// The fake container has only /bin/sh. The session prints its terminal
	// size, then echoes lines with a prefix.
	script := `#!/bin/sh
case "$*" in
  "exec 0123456789ab test -x /bin/bash") exit 1 ;;
  "exec 0123456789ab test -x /bin/sh") exit 0 ;;
  "exec --interactive --tty "*)
    echo "$*" > ` + argsFile + `
    echo "size $(stty size)"
    while read -r line; do echo "got:$line"; done ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	c := &CLI{Binary: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.StartShell(ctx, "--privileged", 80, 24); err == nil {
		t.Fatal("invalid container id accepted")
	}
	tty, err := c.StartShell(ctx, "0123456789ab", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	out := bufio.NewReader(tty)
	waitFor := func(want string) {
		t.Helper()
		for {
			line, err := out.ReadString('\n')
			if err != nil {
				t.Fatalf("waiting for %q: %v", want, err)
			}
			if strings.Contains(line, want) {
				return
			}
		}
	}
	waitFor("size 30 100")
	if _, err := tty.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	waitFor("got:hello")
	if err := tty.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(args)) != "exec --interactive --tty --env TERM=xterm-256color 0123456789ab /bin/sh" {
		t.Fatalf("args %q", args)
	}
	if err := tty.Close(); err != nil {
		t.Fatal(err)
	}
	_ = tty.Close() // a second Close is harmless
}
