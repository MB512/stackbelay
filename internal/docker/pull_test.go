package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines as "docker compose --progress json pull" (Compose v5) prints them.
var pullOutput = []string{
	`{"id":"Image alpine:3.20","status":"Working","text":"Pulling"}`,
	`{"id":"Image nginx:1.27","status":"Working","text":"Pulling"}`,
	`{"id":"l1","parent_id":"Image alpine:3.20","status":"Working","text":"Pulling fs layer","details":"0B"}`,
	`{"id":"l2","parent_id":"Image nginx:1.27","status":"Working","text":"Pulling fs layer","details":"0B"}`,
	`{"id":"l3","parent_id":"Image nginx:1.27","status":"Working","text":"Pulling fs layer","details":"0B"}`,
	`{"id":"l1","parent_id":"Image alpine:3.20","status":"Working","text":"Downloading","current":1048576,"total":4194304,"percent":25}`,
	`{"id":"l2","parent_id":"Image nginx:1.27","status":"Working","text":"Downloading","current":2097152,"total":10485760,"percent":20}`,
	`{"id":"l3","parent_id":"Image nginx:1.27","status":"Done","text":"Already exists","percent":100}`,
	`{"id":"l1","parent_id":"Image alpine:3.20","status":"Done","text":"Download complete","percent":100}`,
	`{"id":"l1","parent_id":"Image alpine:3.20","status":"Working","text":"Extracting","current":1}`,
	`{"id":"l1","parent_id":"Image alpine:3.20","status":"Done","text":"Pull complete","percent":100}`,
	`{"id":"Image alpine:3.20","status":"Done","text":"Pulled"}`,
}

func TestPullTracker(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)
	tr := newPullTracker(t0)
	var doneAt []int
	for i, l := range pullOutput {
		if tr.add([]byte(l)) {
			doneAt = append(doneAt, i)
		}
	}
	if len(doneAt) != 1 || doneAt[0] != len(pullOutput)-1 {
		t.Fatalf("image done at %v", doneAt)
	}
	s := tr.status(t0.Add(4 * time.Second))
	if s.BytesDone != 4194304+2097152 || s.BytesTotal != 4194304+10485760 || s.SecondsElapsed != 4 || len(s.Images) != 2 {
		t.Fatalf("status %+v", s)
	}
	if got, want := s.Summary(), "alpine:3.20 done, nginx:1.27 2.0 MiB of 10.0 MiB"; got != want {
		t.Fatalf("summary %q, want %q", got, want)
	}
	if tr.add([]byte("not json")) {
		t.Fatal("garbage counted")
	}
	if got := (PullStatus{Images: []ImagePull{{Image: "x"}}}).Summary(); got != "x waiting" {
		t.Fatalf("summary %q", got)
	}
}

func TestPullWithProgress(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	out := filepath.Join(dir, "pull.out")
	if err := os.WriteFile(out, []byte(strings.Join(pullOutput, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Prints the progress to stderr; fails if the arguments are not the
	// expected json pull.
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"--progress json pull --ignore-buildable -- a b\") cat " + out + " >&2 ;;\n" +
		"  *\"--progress json pull --ignore-buildable -- broken\") echo '{\"id\":\"Image x:1\",\"status\":\"Error\",\"text\":\"manifest unknown\"}' >&2; exit 18 ;;\n" +
		"  *) echo \"unexpected: $*\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	var reports []PullStatus
	c := &CLI{Binary: bin, Pulling: func(s PullStatus) { reports = append(reports, s) }}
	if err := c.Pull(context.Background(), t.TempDir(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 2 {
		t.Fatalf("reports %+v", reports)
	}
	if last := reports[len(reports)-1]; last.Summary() != "alpine:3.20 done, nginx:1.27 2.0 MiB of 10.0 MiB" {
		t.Fatalf("last report %+v", last)
	}

	err := c.Pull(context.Background(), t.TempDir(), []string{"broken"})
	if err == nil || !strings.Contains(err.Error(), "Image x:1: manifest unknown") {
		t.Fatalf("error %v", err)
	}
}
