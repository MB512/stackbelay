package jobs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/docker"
)

// Issue #26: compose refuses to start, recreate or pull an orphan (a
// container whose service is no longer in the compose file): "no such
// service". No job may hand one to compose.

// logTo makes e's pipeline log into a buffer, for checking warnings.
func logTo(e *env) *bytes.Buffer {
	var buf bytes.Buffer
	e.p.Logger = slog.New(slog.NewTextHandler(io.MultiWriter(&buf, io.Discard), nil))
	return &buf
}

func TestComposeApplyRemovesRunningService(t *testing.T) {
	e := newEnv(t)
	// "shell" runs; the new file removes it: afterwards its (stopped)
	// container is an orphan.
	e.docker.services = append(e.docker.services, docker.ServiceState{Name: "shell", State: "running", ContainerID: "5e115e11"})
	e.docker.servicesAfter = []docker.ServiceState{
		{Name: "app", State: "exited", ContainerID: "9f8e9f8e9f8e"},
		{Name: "db", State: "exited", ContainerID: "db0db0db0db0"},
		{Name: "shell", State: "exited", ContainerID: "5e115e11"},
	}
	e.docker.orphansAfter = []string{"shell"}
	logs := logTo(e)
	res, err := e.p.ComposeApply(context.Background(), e.stack, composeOpts(t, e, true))
	if err != nil || res.RolledBack {
		t.Fatalf("result %+v, err %v", res, err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.Up") && c != "docker.Up app,db" {
			t.Errorf("started %s, want app and db only", c)
		}
	}
	if !strings.Contains(logs.String(), "service removed from the compose file") || !strings.Contains(logs.String(), "service=shell") {
		t.Errorf("removed service not reported:\n%s", logs)
	}
}

func TestServicesAfterEditIgnoresOrphans(t *testing.T) {
	before := []docker.ServiceState{
		{Name: "app", Defined: true, State: "running", ContainerID: "a"},
		{Name: "shell", Defined: true, State: "running", ContainerID: "s"},
	}
	// After the change "shell" is gone from the file; p.services drops its
	// container, so servicesAfterEdit never sees it.
	after := []docker.ServiceState{{Name: "app", Defined: true, State: "exited", ContainerID: "a"}}
	start, verify := servicesAfterEdit(before, after, []string{"app", "shell"})
	if strings.Join(start, ",") != "app" || strings.Join(verify, ",") != "app" {
		t.Fatalf("start %v, verify %v", start, verify)
	}
}

func TestOrphansAreLeftAlone(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		// A running and a stopped container of services removed from the
		// file by hand.
		e.docker.services = append(e.docker.services,
			docker.ServiceState{Name: "cron", State: "running", ContainerID: "c0c0c0c0"},
			docker.ServiceState{Name: "old", State: "exited", ContainerID: "01d01d01d"})
		e.docker.orphans = []string{"cron", "old"}
		return e
	}
	assertNoOrphan := func(t *testing.T, e *env) {
		t.Helper()
		for _, c := range e.calls.list() {
			if strings.Contains(c, "cron") || strings.Contains(c, "old") {
				t.Errorf("orphan handed to compose: %s", c)
			}
		}
	}

	t.Run("backup restarts and records defined services only", func(t *testing.T) {
		e := setup(t)
		logs := logTo(e)
		if _, err := e.p.Backup(context.Background(), e.stack); err != nil {
			t.Fatal(err)
		}
		assertNoOrphan(t, e)
		meta, err := os.ReadFile(filepath.Join(e.stack.Dir, MetadataFile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(meta), "cron") || strings.Contains(string(meta), "\nold\t") {
			t.Errorf("orphans recorded in %s:\n%s", MetadataFile, meta)
		}
		if !strings.Contains(logs.String(), "orphans") {
			t.Errorf("orphans not logged:\n%s", logs)
		}
	})
	t.Run("start", func(t *testing.T) {
		e := setup(t)
		if err := e.p.Start(context.Background(), e.stack); err != nil {
			t.Fatal(err)
		}
		assertNoOrphan(t, e)
	})
	t.Run("restart", func(t *testing.T) {
		e := setup(t)
		if err := e.p.Restart(context.Background(), e.stack); err != nil {
			t.Fatal(err)
		}
		assertNoOrphan(t, e)
	})
	t.Run("update", func(t *testing.T) {
		e := setup(t)
		if _, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true}); err != nil {
			t.Fatal(err)
		}
		assertNoOrphan(t, e)
	})
}

// A metadata.txt written before the fix may list an orphan as running: the
// restore does not try to start it.
func TestRestoreSkipsRecordedOrphan(t *testing.T) {
	e := restoreEnv(t)
	e.restic.restoreFiles[MetadataFile] = "# stackvault-metadata v1\ncreated_at\t2026-11-01T00:00:00Z\nstack\tweb\n" +
		"app\trunning\t-\t-\t-\t-\nshell\trunning\t-\t-\t-\t-\n"
	if _, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8]}); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.Up") && c != "docker.Up app" {
			t.Errorf("started %s, want app only", c)
		}
	}
}
