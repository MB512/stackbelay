package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/stacks"
)

func restoreOpts() RestoreOptions { return RestoreOptions{SnapshotID: snapID[:8]} }

// restoreCall is the expected restic call for the test stack; exclude
// always starts with the incomplete-restore marker.
func restoreCall(exclude ...string) string {
	return "restic.Restore " + snapID + ":/home/docker/web exclude=" +
		strings.Join(append([]string{stacks.IncompleteMarker}, exclude...), ",")
}

// assertNoSideDirs checks that a restore created no sibling directories
// (staging or previous version, as before issue #33).
func assertNoSideDirs(t *testing.T, e *env) {
	t.Helper()
	side, _ := filepath.Glob(e.stack.Dir + ".*")
	if len(side) > 0 {
		t.Fatalf("sibling directories created: %v", side)
	}
}

func assertMarker(t *testing.T, dir string, want bool) {
	t.Helper()
	if got := stacks.RestoreIncomplete(dir); got != want {
		t.Fatalf("incomplete-restore marker present = %v, want %v", got, want)
	}
}

func TestRestoreSuccess(t *testing.T) {
	e := restoreEnv(t)
	stale := filepath.Join(e.stack.Dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("created after the snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	var markerDuringRestore bool
	e.restic.restoreVisit = func(dir string) { markerDuringRestore = stacks.RestoreIncomplete(dir) }
	res, err := e.p.Restore(context.Background(), e.stack, restoreOpts())
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo",
		"restic.Snapshots " + snapID[:8],
		"docker.Services",
		"docker.Stop",
		"restic.Backup tag=pre-restore parent= host=host-a",
		restoreCall(),
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.Services", // the restored file's services (orphans are not started)
		"docker.Up app",   // only services running at backup time
		"docker.WaitHealthy",
	})
	if res.SafetySnapshot != "snap-pre-restore" || res.Snapshot != snapID {
		t.Errorf("unexpected result %+v", res)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("file not in the snapshot kept: %v", err)
	}
	fi, err := os.Stat(e.stack.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 || !fi.ModTime().Equal(e.restic.node.ModTime) {
		t.Errorf("stack dir metadata not restored: mode %v, mtime %v", fi.Mode(), fi.ModTime())
	}
	if !markerDuringRestore {
		t.Error("no incomplete-restore marker while restic writes")
	}
	assertMarker(t, e.stack.Dir, false)
	assertNoSideDirs(t, e)
}

// Before restic writes into the stack directory, a failure starts the
// services that were running again; the directory is untouched.
func TestRestoreFailureBeforeWriting(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(e *env)
		wantCalls []string
	}{
		{
			name:  "safety snapshot fails",
			setup: func(e *env) { e.restic.failOn["Backup:pre-restore"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"restic.Backup tag=pre-restore parent= host=host-a", "docker.Start app,db", "docker.WaitHealthy"},
		},
		{
			name:  "stop fails",
			setup: func(e *env) { e.docker.failOn["Stop"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"docker.Start app,db", "docker.WaitHealthy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			tt.setup(e)
			_, err := e.p.Restore(context.Background(), e.stack, restoreOpts())
			if !errors.Is(err, errBoom) {
				t.Fatalf("expected errBoom, got %v", err)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
			if got := readData(t, e.stack.Dir); got != "original" {
				t.Errorf("stack directory was modified: %q", got)
			}
			if strings.Contains(err.Error(), "partly restored") {
				t.Errorf("untouched stack reported as partly restored: %v", err)
			}
			assertMarker(t, e.stack.Dir, false)
		})
	}
}

// Once restic has written into the stack directory, the previous state is
// gone: the stack stays stopped (no start of a half-restored stack), the
// marker stays until a restore completes, and the error says how to get
// back.
func TestRestorePartialFailure(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(e *env)
		opts       RestoreOptions
		wantInErr  []string
		lastCall   string
		wantMarker bool
	}{
		{
			name:       "restic restore fails",
			setup:      func(e *env) { e.restic.failOn["Restore"] = errBoom },
			opts:       restoreOpts(),
			wantInErr:  []string{"partly restored", "left stopped", "continue it", "safety snapshot snap-pre"},
			lastCall:   restoreCall(),
			wantMarker: true,
		},
		{
			name:       "reading directory metadata fails",
			setup:      func(e *env) { e.restic.failOn["Node"] = errBoom },
			opts:       restoreOpts(),
			wantInErr:  []string{"partly restored", "safety snapshot snap-pre"},
			lastCall:   "restic.Node " + snapID + ":/home/docker/web",
			wantMarker: true,
		},
		{
			name:       "without safety snapshot (rollback)",
			setup:      func(e *env) { e.restic.failOn["Restore"] = errBoom },
			opts:       RestoreOptions{SnapshotID: snapID[:8], skipSafetySnapshot: true},
			wantInErr:  []string{"partly restored", "run the restore again to continue it"},
			lastCall:   restoreCall(),
			wantMarker: true,
		},
		{
			name: "pinned image missing",
			setup: func(e *env) {
				e.restic.restoreFiles[MetadataFile] = pinMetadata
				e.docker.failOn["EnsureImage"] = errBoom
			},
			opts:      RestoreOptions{SnapshotID: snapID[:8], PinImages: true},
			wantInErr: []string{"is restored, but the stack was not started"},
			lastCall:  "docker.EnsureImage " + testDigest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			tt.setup(e)
			_, err := e.p.Restore(context.Background(), e.stack, tt.opts)
			if !errors.Is(err, errBoom) {
				t.Fatalf("expected errBoom, got %v", err)
			}
			for _, want := range tt.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			calls := e.calls.list()
			if calls[len(calls)-1] != tt.lastCall {
				t.Fatalf("stack touched after the failure (want last call %q):\n  %s", tt.lastCall, strings.Join(calls, "\n  "))
			}
			assertMarker(t, e.stack.Dir, tt.wantMarker)
			assertNoSideDirs(t, e)
		})
	}
}

func TestRestoreSnapshotWithoutComposeFile(t *testing.T) {
	e := restoreEnv(t)
	delete(e.restic.restoreFiles, "compose.yml")
	if err := os.Remove(e.stack.ComposeFile); err != nil { // --delete removes the old one
		t.Fatal(err)
	}
	_, err := e.p.Restore(context.Background(), e.stack, restoreOpts())
	if err == nil || !strings.Contains(err.Error(), "does not contain a compose file") {
		t.Fatalf("unexpected error %v", err)
	}
	assertMarker(t, e.stack.Dir, true)
}

// A restore into a directory whose last restore did not complete continues
// it: no safety snapshot of the partly restored directory, and the marker
// is removed once the restore completes.
func TestRestoreContinuesIncompleteRestore(t *testing.T) {
	e := restoreEnv(t)
	if err := os.WriteFile(filepath.Join(e.stack.Dir, stacks.IncompleteMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.p.Restore(context.Background(), e.stack, restoreOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.Contains(c, TagPreRestore) {
			t.Fatalf("safety snapshot of a partly restored directory: %v", e.calls.list())
		}
	}
	if res.SafetySnapshot != "" {
		t.Errorf("result %+v", res)
	}
	assertMarker(t, e.stack.Dir, false)
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
}

// A failing start after a complete restore is reported as such, without
// the advice for a partial restore.
func TestRestoreStartFails(t *testing.T) {
	e := restoreEnv(t)
	e.docker.failOn["Up"] = errBoom
	_, err := e.p.Restore(context.Background(), e.stack, restoreOpts())
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "starting restored stack") {
		t.Fatalf("unexpected error %v", err)
	}
	if strings.Contains(err.Error(), "partly restored") || strings.Contains(err.Error(), "not started") {
		t.Errorf("misleading error: %v", err)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
	assertMarker(t, e.stack.Dir, false)
}

// The manager restoring its own stack: its job directory is excluded from
// the restore, so the current job files stay as they are.
func TestRestoreExcludesCarryOver(t *testing.T) {
	e := restoreEnv(t)
	jobFile := filepath.Join(e.stack.Dir, "data", "jobs", "J1", "status.json")
	if err := os.MkdirAll(filepath.Dir(jobFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobFile, []byte("newest"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := restoreOpts()
	opts.CarryOver = "data/jobs"
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range e.calls.list() {
		found = found || c == restoreCall("data/jobs")
	}
	if !found {
		t.Fatalf("job directory not excluded: %v", e.calls.list())
	}
	if b, err := os.ReadFile(jobFile); err != nil || string(b) != "newest" {
		t.Fatalf("job file: %q %v", b, err)
	}
	assertNoSideDirs(t, e)
}

func TestRestoreNoStart(t *testing.T) {
	e := restoreEnv(t)
	opts := restoreOpts()
	opts.NoStart = true
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.Up") || strings.HasPrefix(c, "docker.Start") {
			t.Fatalf("stack started: %v", e.calls.list())
		}
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
}

func TestRestoreRejectsInvalidCarryOver(t *testing.T) {
	e := restoreEnv(t)
	opts := restoreOpts()
	opts.CarryOver = "../other"
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err == nil {
		t.Fatal("expected error")
	}
	if len(e.calls.list()) != 0 {
		t.Fatalf("stack touched: %v", e.calls.list())
	}
}

// While a restore is incomplete, nothing but a restore, stop and down may
// touch the stack: no start of partly restored data, and no backup that
// would record it as a valid snapshot.
func TestIncompleteRestoreBlocksJobs(t *testing.T) {
	ctx := context.Background()
	for name, run := range map[string]func(e *env) error{
		"backup":  func(e *env) error { _, err := e.p.Backup(ctx, e.stack); return err },
		"update":  func(e *env) error { _, err := e.p.Update(ctx, e.stack, UpdateOptions{Backup: true}); return err },
		"start":   func(e *env) error { return e.p.Start(ctx, e.stack) },
		"restart": func(e *env) error { return e.p.Restart(ctx, e.stack) },
		"up":      func(e *env) error { return e.p.Up(ctx, e.stack) },
		"compose-apply": func(e *env) error {
			_, err := e.p.ComposeApply(ctx, e.stack, ComposeApplyOptions{Content: []byte("services: {}\n"), Backup: true})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := restoreEnv(t)
			if err := os.WriteFile(filepath.Join(e.stack.Dir, stacks.IncompleteMarker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run(e); err == nil || !strings.Contains(err.Error(), "did not complete") {
				t.Fatalf("err = %v", err)
			}
			if calls := e.calls.list(); len(calls) != 0 {
				t.Fatalf("stack touched: %v", calls)
			}
		})
	}
	// Stop and down are allowed.
	e := restoreEnv(t)
	if err := os.WriteFile(filepath.Join(e.stack.Dir, stacks.IncompleteMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Stop(ctx, e.stack); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := e.p.Down(ctx, e.stack); err != nil {
		t.Fatalf("down: %v", err)
	}
}
