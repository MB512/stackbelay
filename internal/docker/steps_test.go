package docker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// upOutput is what compose v5 prints with "--progress json up --detach"
// for a project where b waits for a healthy a.
var upOutput = []string{
	`{"id":"Network p_default","status":"Working","text":"Creating"}`,
	`{"id":"Network p_default","status":"Done","text":"Created"}`,
	`{"id":"Container p-a-1","status":"Working","text":"Creating"}`,
	`{"id":"Container p-a-1","status":"Done","text":"Created"}`,
	`{"id":"Container p-a-1","status":"Working","text":"Waiting"}`,
	`{"id":"Container p-a-1","status":"Done","text":"Healthy"}`,
	`{"id":"Container p-b-1","status":"Done","text":"Started"}`,
}

func TestComposeSteps(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	out := filepath.Join(dir, "up.out")
	if err := os.WriteFile(out, []byte(strings.Join(upOutput, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"--progress json up --detach\") cat " + out + " >&2 ;;\n" +
		"  *\"--progress json start -- a b\") cat " + out + " >&2 ;;\n" +
		"  *\"--progress json --file /x/pin.yml up --detach --force-recreate -- a\") " +
		"echo 'no json here' >&2; echo '{\"id\":\"Container p-a-1\",\"status\":\"Error\",\"text\":\"port is already allocated\"}' >&2; exit 1 ;;\n" +
		"  *\" up --detach\") exit 0 ;;\n" + // without progress
		"  *) echo \"unexpected: $*\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	ctx := context.Background()

	// Without Steps: compose runs without --progress json.
	if err := (&CLI{Binary: bin}).UpAll(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	var steps []string
	c := &CLI{Binary: bin, Steps: func(s ComposeStep) {
		if s.Done() {
			steps = append(steps, s.String())
		}
	}}
	if err := c.UpAll(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Network p_default: Created", "Container p-a-1: Created", "Container p-a-1: Healthy", "Container p-b-1: Started"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps %q", steps)
	}
	steps = nil
	if err := c.Start(ctx, t.TempDir(), []string{"a", "b"}); err != nil || len(steps) != 4 {
		t.Fatalf("start: %v, steps %q", err, steps)
	}

	err := c.Up(ctx, t.TempDir(), []string{"/x/pin.yml"}, []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "docker compose --file /x/pin.yml up") ||
		!strings.Contains(err.Error(), "Container p-a-1: port is already allocated") || !strings.Contains(err.Error(), "no json here") {
		t.Fatalf("error %v", err)
	}
}
