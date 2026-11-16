package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCompose(t *testing.T) {
	dir := t.TempDir()
	stack := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	// The fake compose rejects files containing "bad" and names the file
	// it was given, like compose does.
	script := `#!/bin/sh
echo "$*" > ` + argsFile + `
for last; do :; done
file="$5"
if grep -q bad "$file"; then echo "validating $file: services.app additional property bad is not allowed" >&2; exit 15; fi
exit 0
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	c := &CLI{Binary: bin}
	ctx := context.Background()

	if err := c.ValidateCompose(ctx, stack, "compose.yml", []byte("services: {}\n")); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	fields := strings.Fields(string(args))
	if len(fields) != 7 || fields[0] != "compose" || fields[2] != stack || fields[3] != "--file" ||
		!strings.HasSuffix(fields[4], ".yml") || filepath.Dir(fields[4]) == stack || fields[5] != "config" || fields[6] != "--quiet" {
		t.Fatalf("args %q", args)
	}
	if _, err := os.Stat(fields[4]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary file not removed")
	}

	err := c.ValidateCompose(ctx, stack, "compose.yml", []byte("bad: 1\n"))
	if !errors.Is(err, ErrInvalidCompose) || !strings.Contains(err.Error(), "validating compose.yml: services.app") ||
		strings.Contains(err.Error(), "stackvault-compose-") {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(stack)
	if len(entries) != 0 {
		t.Fatal("validation wrote into the stack directory")
	}
}
