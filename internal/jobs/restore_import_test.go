package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/stacks"
)

// importEnv is restoreEnv with a snapshot from another host and an empty
// stacks root on this one.
func importEnv(t *testing.T) (*env, stacks.Stack) {
	t.Helper()
	e := restoreEnv(t)
	e.restic.snapshots[0].Hostname = "old-host"
	digest := "nginx@sha256:" + strings.Repeat("ab", 32)
	e.restic.restoreFiles[MetadataFile] = "# stackvault-metadata v1\ncreated_at\t2026-11-01T00:00:00Z\nstack\tweb\n" +
		"app\trunning\tnginx:1\t" + digest + "\t-\t-\ndb\texited\t-\t-\t-\t-\n"
	e.docker.missingImages = []string{digest}
	target, err := stacks.ImportTarget(t.TempDir(), "web")
	if err != nil {
		t.Fatal(err)
	}
	return e, target
}

func TestImport(t *testing.T) {
	e, target := importEnv(t)
	res, err := e.p.Import(context.Background(), target, RestoreOptions{SnapshotID: snapID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to stop, no safety snapshot, no swap: restore, check the
	// images, rename, start what ran at snapshot time.
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo",
		"restic.Snapshots " + snapID[:8],
		"restic.Restore " + snapID + ":/home/docker/web",
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.ImageExists nginx@sha256:" + strings.Repeat("ab", 32),
		"docker.Up app",
		"docker.WaitHealthy",
	})
	if res.Snapshot != snapID || res.OldDir != "" || res.SafetySnapshot != "" {
		t.Errorf("result %+v", res)
	}
	if got := readData(t, target.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
	if _, err := os.Stat(stacks.StagingDir(target)); !os.IsNotExist(err) {
		t.Errorf("staging directory left: %v", err)
	}
}

func TestImportWithoutStart(t *testing.T) {
	e, target := importEnv(t)
	// Pinning only applies to a start.
	if _, err := e.p.Import(context.Background(), target, RestoreOptions{SnapshotID: snapID[:8], NoStart: true, PinImages: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.Up") || strings.HasPrefix(c, "docker.EnsureImage") || strings.HasPrefix(c, "docker.Stop") {
			t.Errorf("unexpected call %s", c)
		}
	}
	if got := readData(t, target.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
}

func TestImportFailureLeavesNothing(t *testing.T) {
	e, target := importEnv(t)
	e.restic.failOn["Restore"] = errors.New("connection reset")
	if _, err := e.p.Import(context.Background(), target, RestoreOptions{SnapshotID: snapID[:8]}); err == nil {
		t.Fatal("expected error")
	}
	for _, dir := range []string{target.Dir, stacks.StagingDir(target)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", dir, err)
		}
	}
}

func TestImportRefusesExistingDirectory(t *testing.T) {
	e, target := importEnv(t)
	// The directory appears while the job runs (e.g. created by hand):
	// the rename must not replace it.
	if err := os.MkdirAll(filepath.Join(target.Dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Import(context.Background(), target, RestoreOptions{SnapshotID: snapID[:8]}); err == nil {
		t.Fatal("import over an existing directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(target.Dir, "keep")); err != nil {
		t.Errorf("existing directory touched: %v", err)
	}
	if _, err := os.Stat(stacks.StagingDir(target)); !os.IsNotExist(err) {
		t.Errorf("staging directory left: %v", err)
	}
	if _, err := stacks.ImportTarget(filepath.Dir(target.Dir), "web"); !errors.Is(err, stacks.ErrExists) {
		t.Errorf("ImportTarget on an existing directory: %v", err)
	}
}

func TestImportRejectsStackOptions(t *testing.T) {
	e, target := importEnv(t)
	for _, opts := range []RestoreOptions{
		{SnapshotID: snapID[:8], InPlace: true},
		{SnapshotID: snapID[:8], SkipSafetySnapshot: true},
		{SnapshotID: snapID[:8], CarryOver: "data/jobs"},
	} {
		if _, err := e.p.Import(context.Background(), target, opts); err == nil {
			t.Errorf("options %+v accepted", opts)
		}
	}
}
